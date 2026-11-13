"""Lets ChatGPT's connector sign in to one MCP connector (default: the whatsapp host from site.json).

Adds ChatGPT's OAuth callbacks to that Access app's dynamic-client-registration allowlist and keeps
the existing entries. Signing in still needs an allowed email and its emailed PIN. Prints no secrets.

    cf_allow_chatgpt.py [host]
"""
import json
import sys

sys.path.insert(0, r"C:\mcp\tools")
from cf_api import call, errors
from site_config import DOMAIN, HOSTS

# Cloudflare doesn't send RFC 9207 `iss`, so ChatGPT uses its callback-ID-specific redirect.
# The stable one is kept in case that changes. 2026-11-09.
CHATGPT = ["https://chatgpt.com/connector/oauth/*", "https://chatgpt.com/connector_platform_oauth_redirect"]


def need(data, what):
    if not data.get("success"):
        raise SystemExit(f"FAILED {what} ({data['_status']}): {errors(data)}")
    return data.get("result")


def main():
    host = f"{sys.argv[1] if len(sys.argv) > 1 else HOSTS['whatsapp']}.{DOMAIN}"
    zones = need(call("GET", "/zones", params={"name": DOMAIN}), "find zone")
    account = zones[0]["account"]["id"]
    apps = need(call("GET", f"/accounts/{account}/access/apps"), "list Access apps")
    app = next((a for a in apps if a.get("domain") == host), None)
    if app is None:
        raise SystemExit(f"No Access app for {host}")
    full = need(call("GET", f"/accounts/{account}/access/apps/{app['id']}"), "read Access app")
    dcr = full["oauth_configuration"]["dynamic_client_registration"]
    before = list(dcr.get("allowed_uris") or [])
    dcr["allowed_uris"] = before + [u for u in CHATGPT if u not in before]
    if dcr["allowed_uris"] == before:
        print("already allowed:", before)
        return
    updated = need(call("PUT", f"/accounts/{account}/access/apps/{app['id']}", full), "update Access app")
    print("before:", before)
    print("after:", updated["oauth_configuration"]["dynamic_client_registration"]["allowed_uris"])
    print("policies:", [(p.get("name"), p.get("decision")) for p in updated.get("policies", [])])


if __name__ == "__main__":
    main()
