"""Creates the Cloudflare side of the app's hostname. Safe to re-run; prints no secrets.

Same order as tools/cf_setup.py beside this folder: Access first and the public route last, so the hostname
is never exposed without a login in front of it.

    cf_setup_app.py access   Create the Access app and write its aud to relay.json.
    cf_setup_app.py dns      Create the DNS record. Run only after the tunnel route exists.

The tunnel route itself lives in cloudflared's config.yml. See README.md.

Only the user's personal login is allowed: this app can act on the PC through Claude, so it gets the same
allowlist as the PC control connector, not the wider WhatsApp one.
"""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT.parent / "tools"))
from cf_api import call, errors  # noqa: E402

# This site's own names come from site.json next to this folder, which stays private. site.example.json
# shows its shape: domain, access_team, tunnel, hosts.app (the app's host name, such as "d"), app_emails.
SITE = ROOT.parent / "site.json"
try:
    _site = json.loads(SITE.read_text(encoding="utf-8"))
    DOMAIN = _site["domain"]
    HOST = f"{_site['hosts']['app']}.{DOMAIN}"
    TUNNEL = _site.get("tunnel") or "home"
    EMAILS = list(_site["app_emails"])
except (OSError, ValueError, KeyError, TypeError) as e:
    raise SystemExit(f"{SITE} is missing or incomplete ({e}). Copy site.example.json and fill it in.")
RELAY_JSON = ROOT / "relay" / "bin" / "relay.json"


def need(data, what):
    if not data.get("success"):
        raise SystemExit(f"FAILED {what} ({data['_status']}): {errors(data)}")
    return data.get("result")


def zone_info():
    zones = need(call("GET", "/zones", params={"name": DOMAIN}), "find zone")
    return zones[0]["id"], zones[0]["account"]["id"]


def login_method(account):
    for provider in need(call("GET", f"/accounts/{account}/access/identity_providers"), "list login methods"):
        if provider.get("type") == "onetimepin":
            return provider["id"]
    raise SystemExit("No one-time PIN login exists. Run tools/cf_setup.py first.")


def ensure_app(account, idp):
    apps = need(call("GET", f"/accounts/{account}/access/apps"), "list Access apps")
    app = next((a for a in apps if a.get("domain") == HOST), None)
    if app:
        print("access app: exists", HOST)
        return app
    app = need(
        call(
            "POST",
            f"/accounts/{account}/access/apps",
            {
                "name": HOST.split(".")[0],
                "domain": HOST,
                "type": "self_hosted",
                "session_duration": "720h",
                "allowed_idps": [idp],
                "auto_redirect_to_identity": True,
                "app_launcher_visible": False,
                "http_only_cookie_attribute": True,
                "same_site_cookie_attribute": "lax",
                "policies": [
                    {
                        "name": "d-allow",
                        "decision": "allow",
                        "include": [{"email": {"email": e}} for e in EMAILS],
                    }
                ],
            },
        ),
        "create Access app",
    )
    print("access app: created", HOST)
    return app


def do_access():
    _, account = zone_info()
    org = need(call("GET", f"/accounts/{account}/access/organizations"), "read Access organization")
    team = org["auth_domain"].split(".")[0]
    app = ensure_app(account, login_method(account))
    cfg = json.loads(RELAY_JSON.read_text(encoding="utf-8"))
    cfg["team_domain"] = team
    cfg["aud"] = app["aud"]
    cfg["allowed_emails"] = EMAILS
    RELAY_JSON.write_text(json.dumps(cfg, indent=2) + "\n", encoding="utf-8")
    print("relay.json: filled; team", team)
    full = need(call("GET", f"/accounts/{account}/access/apps/{app['id']}"), "read Access app")
    policies = need(call("GET", f"/accounts/{account}/access/apps/{app['id']}/policies"), "read policies")
    print("policies:", [(p.get("name"), p.get("decision"), p.get("include")) for p in policies])
    print("session:", full.get("session_duration"), "same_site:", full.get("same_site_cookie_attribute"))


def do_dns():
    zone, account = zone_info()
    tunnels = need(
        call("GET", f"/accounts/{account}/cfd_tunnel", params={"name": TUNNEL, "is_deleted": "false"}),
        "list tunnels",
    )
    if not tunnels:
        raise SystemExit("Tunnel not found.")
    found = need(call("GET", f"/zones/{zone}/dns_records", params={"name": HOST}), "list DNS")
    if found:
        print("dns: exists", HOST)
        return
    need(
        call(
            "POST",
            f"/zones/{zone}/dns_records",
            {"type": "CNAME", "name": HOST, "content": f"{tunnels[0]['id']}.cfargotunnel.com", "proxied": True},
        ),
        "create DNS record",
    )
    print("dns: created", HOST)


if __name__ == "__main__":
    {"access": do_access, "dns": do_dns}[sys.argv[1]]()
