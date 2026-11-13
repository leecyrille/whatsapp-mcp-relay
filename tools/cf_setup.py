"""Creates the Cloudflare side of one MCP connector. Safe to re-run; prints no secrets.

Order matters: login and policy first, public route last, so the hostname is never exposed
without Access in front of it.

    cf_setup.py whatsapp
"""
import base64
import json
import secrets
import sys
from pathlib import Path

sys.path.insert(0, r"C:\mcp\tools")
from cf_api import call, errors
from site_config import DOMAIN, HOSTS, TEAM, TUNNEL

CF_DIR = Path(r"C:\mcp\cloudflared")
GATEWAY = Path(r"C:\mcp\gateway\gateway.json")
CALLBACKS = ["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"]
EXTRA_ROUTES = {HOSTS["app"]: 8704}  # WhatsApp app relay; it verifies the Access JWT itself.
CONNECTORS = {
    key: {"host": HOSTS[key], "port": port, "name": HOSTS[key]}
    for key, port in (("whatsapp", 8701), ("pc", 8702), ("claude", 8703))
    if key in HOSTS
}


def need(data, what):
    if not data.get("success"):
        raise SystemExit(f"FAILED {what} ({data['_status']}): {errors(data)}")
    return data.get("result")


def zone_info():
    zones = need(call("GET", "/zones", params={"name": DOMAIN}), "find zone")
    return zones[0]["id"], zones[0]["account"]["id"]


def ensure_org(account):
    found = call("GET", f"/accounts/{account}/access/organizations")
    if found.get("success") and found.get("result"):
        print("organization: exists", found["result"].get("auth_domain"))
        return found["result"]
    made = need(
        call(
            "POST",
            f"/accounts/{account}/access/organizations",
            {"name": TEAM, "auth_domain": f"{TEAM}.cloudflareaccess.com"},
        ),
        "create Access organization",
    )
    print("organization: created", made.get("auth_domain"))
    return made


def ensure_otp(account):
    found = need(call("GET", f"/accounts/{account}/access/identity_providers"), "list login methods")
    for provider in found:
        if provider.get("type") == "onetimepin":
            print("login method: one-time PIN exists")
            return provider["id"]
    made = need(
        call(
            "POST",
            f"/accounts/{account}/access/identity_providers",
            {"name": "Email PIN", "type": "onetimepin", "config": {}},
        ),
        "create one-time PIN login",
    )
    print("login method: one-time PIN created")
    return made["id"]


def ensure_app(account, conn, emails, idp):
    host = f"{conn['host']}.{DOMAIN}"
    apps = need(call("GET", f"/accounts/{account}/access/apps"), "list Access apps")
    app = next((a for a in apps if a.get("domain") == host), None)
    if app is None:
        app = need(
            call(
                "POST",
                f"/accounts/{account}/access/apps",
                {
                    "name": conn["name"],
                    "domain": host,
                    "type": "self_hosted",
                    "session_duration": "720h",
                    "allowed_idps": [idp],
                    "auto_redirect_to_identity": True,
                    "app_launcher_visible": False,
                    "policies": [
                        {
                            "name": f"{conn['name']}-allow",
                            "decision": "allow",
                            "include": [{"email": {"email": e}} for e in emails],
                        }
                    ],
                },
            ),
            "create Access app",
        )
        print("access app: created", host)
    else:
        print("access app: exists", host)
    # Managed OAuth: GET the whole app, then PUT it back with OAuth enabled.
    full = need(call("GET", f"/accounts/{account}/access/apps/{app['id']}"), "read Access app")
    # Keep callbacks added later (ChatGPT on whatsapp, 2026-11-09, cf_allow_chatgpt.py).
    existing = ((full.get("oauth_configuration") or {}).get("dynamic_client_registration") or {}).get("allowed_uris") or []
    full["oauth_configuration"] = {
        "enabled": True,
        "dynamic_client_registration": {
            "enabled": True,
            "allowed_uris": CALLBACKS + [u for u in existing if u not in CALLBACKS],
            "allow_any_on_localhost": False,
            "allow_any_on_loopback": False,
        },
        "grant": {"session_duration": "720h", "access_token_lifetime": "1h"},
    }
    updated = need(
        call("PUT", f"/accounts/{account}/access/apps/{app['id']}", full), "enable Managed OAuth"
    )
    print("managed oauth:", json.dumps(updated.get("oauth_configuration")))
    print("policies:", [(p.get("name"), p.get("decision")) for p in updated.get("policies", [])])
    return updated


def ensure_tunnel(account):
    found = need(
        call("GET", f"/accounts/{account}/cfd_tunnel", params={"name": TUNNEL, "is_deleted": "false"}),
        "list tunnels",
    )
    CF_DIR.mkdir(exist_ok=True)
    if found:
        print("tunnel: exists")
        return found[0]["id"]
    secret = base64.b64encode(secrets.token_bytes(32)).decode()
    made = need(
        call(
            "POST",
            f"/accounts/{account}/cfd_tunnel",
            {"name": TUNNEL, "tunnel_secret": secret, "config_src": "local"},
        ),
        "create tunnel",
    )
    (CF_DIR / f"{made['id']}.json").write_text(
        json.dumps({"AccountTag": account, "TunnelSecret": secret, "TunnelID": made["id"]}),
        encoding="utf-8",
    )
    print("tunnel: created")
    return made["id"]


def write_tunnel_config(tunnel_id, live):
    lines = [
        f"tunnel: {tunnel_id}",
        f"credentials-file: {CF_DIR / (tunnel_id + '.json')}",
        "no-autoupdate: true",
        "ingress:",
    ]
    for key in live:
        conn = CONNECTORS[key]
        lines += [
            f"  - hostname: {conn['host']}.{DOMAIN}",
            f"    service: http://127.0.0.1:{conn['port']}",
        ]
    # Routes that aren't MCP connectors, so they aren't in gateway.json.
    for host, port in EXTRA_ROUTES.items():
        lines += [f"  - hostname: {host}.{DOMAIN}", f"    service: http://127.0.0.1:{port}"]
    lines.append("  - service: http_status:404")
    (CF_DIR / "config.yml").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print("tunnel config: routes", live)


def ensure_dns(zone, conn, tunnel_id):
    host = f"{conn['host']}.{DOMAIN}"
    found = need(call("GET", f"/zones/{zone}/dns_records", params={"name": host}), "list DNS")
    target = f"{tunnel_id}.cfargotunnel.com"
    if found:
        print("dns: exists", host)
        return
    need(
        call(
            "POST",
            f"/zones/{zone}/dns_records",
            {"type": "CNAME", "name": host, "content": target, "proxied": True},
        ),
        "create DNS record",
    )
    print("dns: created", host)


def main():
    key = sys.argv[1]
    conn = CONNECTORS[key]
    gateway = json.loads(GATEWAY.read_text(encoding="utf-8"))
    zone, account = zone_info()
    org = ensure_org(account)
    team = org["auth_domain"].split(".")[0]
    idp = ensure_otp(account)
    app = ensure_app(account, conn, gateway["servers"][key]["allowed_emails"], idp)
    gateway["team_domain"] = team
    gateway["servers"][key]["aud"] = app["aud"]
    GATEWAY.write_text(json.dumps(gateway, indent=2) + "\n", encoding="utf-8")
    print("gateway config: filled for", key)
    tunnel_id = ensure_tunnel(account)
    live = [k for k, s in gateway["servers"].items() if s["aud"] != "TODO"]
    write_tunnel_config(tunnel_id, live)
    ensure_dns(zone, conn, tunnel_id)


if __name__ == "__main__":
    main()
