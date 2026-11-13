"""Small Cloudflare API helper. Reads the token from C:\\mcp\\.env and never prints it."""
import json
import sys
from pathlib import Path

import httpx

BASE = "https://api.cloudflare.com/client/v4"


def token() -> str:
    for line in Path(r"C:\mcp\.env").read_text(encoding="utf-8-sig").splitlines():
        line = line.strip()
        if line.startswith("CLOUDFLARE_API_TOKEN="):
            value = line.split("=", 1)[1].strip().strip('"').strip("'")
            if value:
                return value
    raise SystemExit("CLOUDFLARE_API_TOKEN is not set in C:\\mcp\\.env")


def call(method: str, path: str, body=None, params=None) -> dict:
    """Returns the API's JSON, without raising on API errors, so callers can report them."""
    response = httpx.request(
        method,
        BASE + path,
        headers={"Authorization": f"Bearer {token()}"},
        json=body,
        params=params,
        timeout=60,
    )
    try:
        data = response.json()
    except ValueError:
        data = {"success": False, "errors": [{"message": response.text[:300]}]}
    data["_status"] = response.status_code
    return data


def errors(data: dict) -> str:
    return "; ".join(f"{e.get('code')}: {e.get('message')}" for e in data.get("errors", []))


if __name__ == "__main__":
    # cf_api.py GET /path [json-body]
    method, path = sys.argv[1], sys.argv[2]
    payload = json.loads(sys.argv[3]) if len(sys.argv) > 3 else None
    print(json.dumps(call(method, path, payload), indent=1))
