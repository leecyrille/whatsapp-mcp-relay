"""Attacks a public MCP hostname from outside, with no login. Prints statuses, sends no credentials.

    external_check.py a.example.com
"""
import base64
import json
import sys

import httpx

host = sys.argv[1]
base = f"https://{host}"
INIT = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "external-check", "version": "1"},
    },
}
HEADERS = {"Accept": "application/json, text/event-stream"}


def _b64(obj) -> str:
    return base64.urlsafe_b64encode(json.dumps(obj, separators=(",", ":")).encode()).decode().rstrip("=")


# Well-formed but unsigned: the gateway must refuse it.
FAKE_JWT = f'{_b64({"alg": "RS256", "kid": "x"})}.{_b64({"aud": ["x"], "email": "owner@example.com", "iss": "x"})}.c2ln'


def report(label, response):
    location = response.headers.get("location", "")
    challenge = response.headers.get("www-authenticate", "")
    print(f"{label}: {response.status_code}"
          f"{' -> ' + location.split('?')[0] if location else ''}"
          f"{' | www-authenticate: ' + challenge[:150] if challenge else ''}")


with httpx.Client(timeout=30, follow_redirects=False) as client:
    report("POST /mcp, no login", client.post(f"{base}/mcp", json=INIT, headers=HEADERS))
    report("GET /mcp, no login", client.get(f"{base}/mcp", headers=HEADERS))
    report("POST /mcp, forged JWT header", client.post(
        f"{base}/mcp", json=INIT, headers={**HEADERS, "Cf-Access-Jwt-Assertion": FAKE_JWT}))
    report("POST /mcp, forged bearer", client.post(
        f"{base}/mcp", json=INIT, headers={**HEADERS, "Authorization": f"Bearer {FAKE_JWT}"}))
    report("GET /, no login", client.get(f"{base}/"))
    report("GET /api/health, no login", client.get(f"{base}/api/health"))

    for path in ("/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource"):
        response = client.get(base + path)
        report(f"discovery {path}", response)
        if response.status_code == 200 and "json" in response.headers.get("content-type", ""):
            meta = response.json()
            print("  ", json.dumps({k: meta.get(k) for k in (
                "issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint",
                "code_challenge_methods_supported", "resource", "authorization_servers") if k in meta}))
            registration = meta.get("registration_endpoint")
            if registration:
                # Only a refused redirect is tested, so no client is created for the real callbacks.
                bad = client.post(registration, json={
                    "client_name": "external-check",
                    "redirect_uris": ["https://evil.example/callback"],
                    "grant_types": ["authorization_code"],
                    "response_types": ["code"],
                    "token_endpoint_auth_method": "none",
                })
                print(f"  registration with a non-Claude callback: {bad.status_code} {bad.text[:160]}")
