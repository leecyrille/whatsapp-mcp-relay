"""Origin auth gateway for the home-PC MCP servers.

Every request arriving from cloudflared must carry a Cloudflare Access JWT that
is RS256-signed by our team's keys, with the right issuer and per-app audience,
unexpired, for an email on this server's allowlist. Anything else is rejected
before it reaches the MCP server. Fails closed: bad config means it won't start.

Usage: access_proxy.py --server whatsapp   (reads C:\\mcp\\gateway\\gateway.json)
"""
from __future__ import annotations

import argparse
import json
import logging
import logging.handlers
import sys
from pathlib import Path

import anyio
import httpx
import jwt
import uvicorn
from starlette.applications import Starlette
from starlette.background import BackgroundTask
from starlette.requests import Request
from starlette.responses import JSONResponse, Response, StreamingResponse
from starlette.routing import Route

LISTEN_HOST = "127.0.0.1"  # Deliberately not configurable.
MCP_PATH = "/mcp"
MAX_BODY_BYTES = 32 * 1024 * 1024
LOG_MAX_BYTES = 10 * 1024 * 1024
LOG_ARG_CHARS = 300

# Never forwarded: edge credentials plus hop-by-hop headers.
STRIP_REQUEST_HEADERS = {
    "authorization", "cookie", "cf-access-jwt-assertion", "cf-access-token",
    "cf-access-client-id", "cf-access-client-secret", "cf-authorization",
    "origin", "host", "content-length", "connection", "keep-alive",
    "proxy-authenticate", "proxy-authorization", "proxy-connection", "te",
    "trailer", "transfer-encoding", "upgrade",
}
STRIP_RESPONSE_HEADERS = {
    "content-length", "connection", "keep-alive", "proxy-authenticate",
    "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
}


class ConfigError(Exception):
    pass


def validate_config(raw: dict) -> dict:
    """Return a normalised config or raise. Never falls back to 'allow'."""
    for key in ("team_domain", "aud", "upstream", "listen_port", "allowed_emails", "calls_log"):
        if not raw.get(key):
            raise ConfigError(f"missing {key}")
    team = str(raw["team_domain"]).strip().lower()
    if "." in team or "/" in team or "TODO" in str(raw["team_domain"]):
        raise ConfigError("team_domain must be the bare team name, e.g. 'myteam'")
    if "TODO" in str(raw["aud"]):
        raise ConfigError("aud is still a placeholder")
    emails = [str(e).strip().lower() for e in raw["allowed_emails"]]
    if not emails or any("*" in e or "@" not in e for e in emails):
        raise ConfigError("allowed_emails must be explicit addresses")
    upstream = str(raw["upstream"]).rstrip("/")
    if not upstream.startswith(("http://127.0.0.1:", "http://localhost:")):
        raise ConfigError("upstream must be on localhost")
    port = int(raw["listen_port"])
    if not 1024 <= port <= 65535:
        raise ConfigError("listen_port out of range")
    # Tools only some logins may call, as {"tool": ["address", ...]}. A tool not listed is open to every login above.
    tool_access = {}
    for tool, allowed in (raw.get("tool_access") or {}).items():
        if not isinstance(allowed, list):
            raise ConfigError(f"tool_access for {tool} must be a list of addresses")
        listed = {str(e).strip().lower() for e in allowed}
        if not listed <= set(emails):
            raise ConfigError(f"tool_access for {tool} names an address not in allowed_emails")
        tool_access[str(tool)] = listed
    return {
        "name": raw.get("name", "server"),
        "issuer": f"https://{team}.cloudflareaccess.com",
        "certs_url": f"https://{team}.cloudflareaccess.com/cdn-cgi/access/certs",
        "aud": str(raw["aud"]),
        "allowed_emails": set(emails),
        "upstream": upstream,
        "listen_port": port,
        "calls_log": str(raw["calls_log"]),
        "tool_access": tool_access,
    }


def make_logger(path: str, name: str) -> logging.Logger:
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    logger = logging.getLogger(f"calls.{name}")
    logger.setLevel(logging.INFO)
    logger.propagate = False
    if not logger.handlers:
        handler = logging.handlers.RotatingFileHandler(
            path, maxBytes=LOG_MAX_BYTES, backupCount=3, encoding="utf-8"
        )
        handler.setFormatter(logging.Formatter("%(asctime)s %(message)s"))
        logger.addHandler(handler)
    return logger


def describe_rpc(body: bytes) -> dict:
    """Pull the JSON-RPC method and tool call out of a body for the audit log."""
    try:
        msg = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        return {}
    if not isinstance(msg, dict):
        return {"rpc": "batch"}
    out = {"rpc": msg.get("method")}
    params = msg.get("params")
    if msg.get("method") == "tools/call" and isinstance(params, dict):
        out["tool"] = params.get("name")
        args = params.get("arguments")
        if private_args(out["tool"]):
            # Contact details are personal data about other people, so the log keeps which fields, not what's in them.
            out["args"] = "fields: " + (",".join(sorted(args)) if isinstance(args, dict) else "none")
        else:
            out["args"] = json.dumps(args, ensure_ascii=False)[:LOG_ARG_CHARS]
    return out


def private_args(tool) -> bool:
    """Tools whose arguments are someone's contact details."""
    return isinstance(tool, str) and "contact" in tool


def hidden_tools(email: str, rules: dict) -> set:
    """Tools this login may not call, so they aren't offered to it either."""
    return {tool for tool, allowed in (rules or {}).items() if email not in allowed}


def is_tool_list(body: bytes) -> bool:
    try:
        msg = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        return False
    return isinstance(msg, dict) and msg.get("method") == "tools/list"


def filter_tool_list(raw: bytes, content_type: str, hidden: set) -> bytes:
    """The tool list without the hidden tools, as plain JSON or as server-sent events. Raises ValueError if it can't
    be read, and the caller then refuses the list rather than show tools it couldn't take out."""
    def drop(msg):
        result = msg.get("result") if isinstance(msg, dict) else None
        if isinstance(result, dict) and isinstance(result.get("tools"), list):
            result["tools"] = [t for t in result["tools"] if not (isinstance(t, dict) and t.get("name") in hidden)]
        return msg

    if "text/event-stream" in content_type.lower():
        lines = []
        for line in raw.decode("utf-8").split("\n"):
            if line.startswith("data:"):
                line = "data: " + json.dumps(drop(json.loads(line[5:].strip())), ensure_ascii=False)
            lines.append(line)
        return "\n".join(lines).encode("utf-8")
    return json.dumps(drop(json.loads(raw)), ensure_ascii=False).encode("utf-8")


def refused_tools(body: bytes, email: str, rules: dict) -> list[str] | None:
    """The tools in this body that this login may not call. None if the body can't be read, which is refused too,
    since a body only the server behind this could read would get past the check."""
    if not rules or not body:
        return []
    try:
        msg = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        return None
    refused = []
    for one in msg if isinstance(msg, list) else [msg]:
        if not isinstance(one, dict) or one.get("method") != "tools/call":
            continue
        params = one.get("params")
        tool = params.get("name") if isinstance(params, dict) else None
        if not isinstance(tool, str) or (tool in rules and email not in rules[tool]):
            refused.append(str(tool))
    return refused


def make_app(cfg: dict, key_resolver=None, transport: httpx.AsyncBaseTransport | None = None,
             logger: logging.Logger | None = None) -> Starlette:
    log = logger or make_logger(cfg["calls_log"], cfg["name"])
    if key_resolver is None:
        jwks = jwt.PyJWKClient(cfg["certs_url"], cache_keys=True, lifespan=3600)
        key_resolver = lambda token: jwks.get_signing_key_from_jwt(token).key
    client = httpx.AsyncClient(
        transport=transport,
        timeout=httpx.Timeout(connect=10, read=None, write=60, pool=10),
        follow_redirects=False,
    )

    def verify(token: str) -> dict:
        return jwt.decode(
            token,
            key_resolver(token),
            algorithms=["RS256"],
            audience=cfg["aud"],
            issuer=cfg["issuer"],
            options={"require": ["exp", "iat", "iss", "aud"]},
        )

    async def handle(request: Request) -> Response:
        path = request.url.path
        token = request.headers.get("cf-access-jwt-assertion")
        if not token:
            log.info(json.dumps({"result": 401, "why": "no jwt", "path": path}))
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        try:
            claims = await anyio.to_thread.run_sync(verify, token)
        except Exception as exc:  # Any verification or key-fetch failure is a rejection.
            log.info(json.dumps({"result": 401, "why": type(exc).__name__, "path": path}))
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        email = str(claims.get("email", "")).strip().lower()
        if email not in cfg["allowed_emails"]:
            log.info(json.dumps({"result": 403, "email": email, "path": path}))
            return JSONResponse({"error": "forbidden"}, status_code=403)
        if path != MCP_PATH and not path.startswith(MCP_PATH + "/"):
            return JSONResponse({"error": "not found"}, status_code=404)

        body = await request.body()
        if len(body) > MAX_BODY_BYTES:
            return JSONResponse({"error": "too large"}, status_code=413)
        headers = [
            (k, v) for k, v in request.headers.items() if k.lower() not in STRIP_REQUEST_HEADERS
        ]
        url = cfg["upstream"] + path
        if request.url.query:
            url += "?" + request.url.query
        record = {"email": email, "method": request.method, **describe_rpc(body)}
        refused = refused_tools(body, email, cfg.get("tool_access") or {})
        if refused is None:
            log.info(json.dumps({**record, "result": 400, "why": "unreadable with tool rules"}, ensure_ascii=False))
            return JSONResponse({"error": "bad request"}, status_code=400)
        if refused:
            log.info(json.dumps({**record, "result": 403, "why": "tool not allowed for this login", "refused": refused}, ensure_ascii=False))
            msg = json.loads(body)
            if isinstance(msg, list):
                return JSONResponse({"error": "forbidden"}, status_code=403)
            # A normal tool result, so the model reads why rather than seeing the connection fail.
            return JSONResponse({"jsonrpc": "2.0", "id": msg.get("id"), "result": {
                "content": [{"type": "text", "text": f"Refused: this login can't use {refused[0]}. Nothing was done."}],
                "isError": True,
            }})
        try:
            upstream = await client.send(
                client.build_request(request.method, url, headers=headers, content=body),
                stream=True,
            )
        except httpx.HTTPError as exc:
            log.info(json.dumps({**record, "result": 502, "why": type(exc).__name__}, ensure_ascii=False))
            return JSONResponse({"error": "upstream unavailable"}, status_code=502)
        log.info(json.dumps({**record, "result": upstream.status_code}, ensure_ascii=False))
        hidden = hidden_tools(email, cfg.get("tool_access") or {})
        if hidden and upstream.status_code == 200 and is_tool_list(body):
            raw = await upstream.aread()
            await upstream.aclose()
            try:
                filtered = filter_tool_list(raw, upstream.headers.get("content-type", ""), hidden)
            except (ValueError, UnicodeDecodeError):
                log.info(json.dumps({**record, "result": 502, "why": "tool list unreadable, not filtered"}))
                return JSONResponse({"error": "upstream unreadable"}, status_code=502)
            return Response(filtered, status_code=200, headers={
                k: v for k, v in upstream.headers.items()
                if k.lower() not in STRIP_RESPONSE_HEADERS and k.lower() not in ("content-length", "content-encoding")
            })
        return StreamingResponse(
            upstream.aiter_raw(),
            status_code=upstream.status_code,
            headers={
                k: v for k, v in upstream.headers.items()
                if k.lower() not in STRIP_RESPONSE_HEADERS
            },
            background=BackgroundTask(upstream.aclose),
        )

    return Starlette(routes=[
        Route("/{path:path}", handle, methods=["GET", "POST", "DELETE"]),
    ])


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--server", required=True)
    parser.add_argument("--config", default=str(Path(__file__).with_name("gateway.json")))
    args = parser.parse_args()
    try:
        all_cfg = json.loads(Path(args.config).read_text(encoding="utf-8"))
        raw = dict(all_cfg["servers"][args.server])
        raw["team_domain"] = all_cfg.get("team_domain")
        raw["name"] = args.server
        cfg = validate_config(raw)
    except (OSError, ValueError, KeyError, ConfigError) as exc:
        print(f"gateway refusing to start: {exc}", file=sys.stderr)
        return 2
    uvicorn.run(make_app(cfg), host=LISTEN_HOST, port=cfg["listen_port"], log_level="warning")
    return 0


if __name__ == "__main__":
    sys.exit(main())
