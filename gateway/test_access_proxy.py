"""Attack tests for the origin gateway. Uses a throwaway key and a fake upstream."""
import base64
import json
import logging
import time

import httpx
import jwt
import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from starlette.testclient import TestClient

from access_proxy import ConfigError, make_app, validate_config

KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
OTHER_KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
PUB = KEY.public_key()
PUB_PEM = PUB.public_bytes(serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo)

RAW = {
    "name": "t", "team_domain": "myteam", "aud": "aud-1", "upstream": "http://127.0.0.1:18701",
    "listen_port": 8701, "allowed_emails": ["Owner@Example.com"], "calls_log": "unused.log",
}
CFG = validate_config(RAW)


def claims(**over):
    now = int(time.time())
    base = {"iss": CFG["issuer"], "aud": ["aud-1"], "email": "owner@example.com", "iat": now, "exp": now + 300}
    base.update(over)
    return {k: v for k, v in base.items() if v is not None}


def token(key=KEY, alg="RS256", **over):
    return jwt.encode(claims(**over), key, algorithm=alg)


class Chunks(httpx.AsyncByteStream):
    """An unread upstream body, delivered in pieces like a real streamed response."""

    def __init__(self, *chunks):
        self.chunks = chunks

    async def __aiter__(self):
        for chunk in self.chunks:
            yield chunk


@pytest.fixture
def harness():
    seen = []

    def upstream(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, stream=Chunks(b'{"ok":', b' true}'), headers={"mcp-session-id": "abc"})

    log = logging.getLogger("test.calls")
    log.handlers.clear()
    log.setLevel(logging.INFO)
    records = []

    class Capture(logging.Handler):
        def emit(self, record):
            records.append(json.loads(record.getMessage()))

    log.addHandler(Capture())
    app = make_app(CFG, key_resolver=lambda _t: PUB, transport=httpx.MockTransport(upstream), logger=log)
    return TestClient(app, raise_server_exceptions=True), seen, records


def post(client, jwt_value=None, path="/mcp", headers=None, body=None):
    h = dict(headers or {})
    if jwt_value is not None:
        h["Cf-Access-Jwt-Assertion"] = jwt_value
    return client.post(path, headers=h, json=body or {"jsonrpc": "2.0", "method": "ping", "id": 1})


def test_valid_token_is_forwarded(harness):
    client, seen, _ = harness
    r = post(client, token())
    assert r.status_code == 200 and r.json() == {"ok": True}
    assert r.headers["mcp-session-id"] == "abc"
    assert len(seen) == 1 and str(seen[0].url) == "http://127.0.0.1:18701/mcp"


def test_no_token_rejected(harness):
    client, seen, _ = harness
    assert post(client).status_code == 401
    assert not seen


@pytest.mark.parametrize("bad", ["", "garbage", "a.b.c"])
def test_malformed_token_rejected(harness, bad):
    client, seen, _ = harness
    assert post(client, bad).status_code == 401
    assert not seen


def test_wrong_signing_key_rejected(harness):
    client, seen, _ = harness
    assert post(client, token(key=OTHER_KEY)).status_code == 401
    assert not seen


def test_wrong_audience_rejected(harness):
    client, seen, _ = harness
    assert post(client, token(aud=["another-access-app"])).status_code == 401
    assert not seen


def test_wrong_issuer_rejected(harness):
    client, seen, _ = harness
    assert post(client, token(iss="https://evil.cloudflareaccess.com")).status_code == 401
    assert not seen


def test_expired_rejected(harness):
    client, seen, _ = harness
    assert post(client, token(iat=int(time.time()) - 900, exp=int(time.time()) - 600)).status_code == 401
    assert not seen


@pytest.mark.parametrize("missing", ["exp", "iat", "iss", "aud"])
def test_missing_required_claim_rejected(harness, missing):
    client, seen, _ = harness
    assert post(client, token(**{missing: None})).status_code == 401
    assert not seen


def test_alg_none_rejected(harness):
    client, seen, _ = harness
    enc = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
    forged = f"{enc({'alg': 'none', 'typ': 'JWT'})}.{enc(claims())}."
    assert post(client, forged).status_code == 401
    assert not seen


def test_hs256_key_confusion_rejected(harness):
    client, seen, _ = harness
    import hashlib
    import hmac
    enc = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
    head = enc(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    pay = enc(json.dumps(claims()).encode())
    sig = enc(hmac.new(PUB_PEM, f"{head}.{pay}".encode(), hashlib.sha256).digest())
    assert post(client, f"{head}.{pay}.{sig}").status_code == 401
    assert not seen


def test_email_not_on_allowlist_forbidden(harness):
    client, seen, records = harness
    assert post(client, token(email="stranger@example.com")).status_code == 403
    assert not seen
    assert records[-1]["result"] == 403


def test_token_without_email_forbidden(harness):
    client, seen, _ = harness
    assert post(client, token(email=None)).status_code == 403
    assert not seen


def test_email_match_is_case_insensitive(harness):
    client, seen, _ = harness
    assert post(client, token(email="OWNER@example.COM")).status_code == 200
    assert len(seen) == 1


def test_auth_checked_before_path(harness):
    client, seen, _ = harness
    assert post(client, path="/anything").status_code == 401
    assert not seen


@pytest.mark.parametrize("path", ["/", "/api/send", "/mcpx", "/.env"])
def test_only_mcp_path_is_forwarded(harness, path):
    client, seen, _ = harness
    assert post(client, token(), path=path).status_code == 404
    assert not seen


def test_edge_credentials_are_stripped(harness):
    client, seen, _ = harness
    post(client, token(), headers={
        "Authorization": "Bearer oauth:abc", "Cookie": "CF_Authorization=x",
        "Cf-Access-Client-Secret": "s", "Origin": "https://claude.ai", "X-Keep": "yes",
    })
    h = seen[0].headers
    for name in ("authorization", "cookie", "cf-access-jwt-assertion", "cf-access-client-secret", "origin"):
        assert name not in h
    assert h["x-keep"] == "yes"
    assert h["host"] == "127.0.0.1:18701"


def test_tool_calls_are_audited_without_tokens(harness):
    client, _, records = harness
    t = token()
    post(client, t, body={"jsonrpc": "2.0", "id": 2, "method": "tools/call",
                          "params": {"name": "send_message", "arguments": {"recipient": "1", "message": "x" * 999}}})
    rec = records[-1]
    assert rec["tool"] == "send_message" and rec["email"] == "owner@example.com" and rec["result"] == 200
    assert len(rec["args"]) <= 300
    assert t not in json.dumps(records)


@pytest.mark.parametrize("change", [
    {"aud": ""}, {"aud": "TODO"}, {"team_domain": ""}, {"team_domain": "TODO"},
    {"team_domain": "evil.com/x"}, {"allowed_emails": []}, {"allowed_emails": ["*"]},
    {"allowed_emails": ["*@gmail.com"]}, {"allowed_emails": ["notanemail"]},
    {"upstream": "http://10.0.0.5:80"}, {"upstream": "https://example.com"},
    {"listen_port": 80}, {"calls_log": ""},
])
def test_bad_config_fails_closed(change):
    with pytest.raises(ConfigError):
        validate_config({**RAW, **change})


# Tools only some logins may call. The email comes only from the verified token.
RULES = {**RAW, "allowed_emails": ["owner@example.com", "work@example.com", "other@example.com"],
         "tool_access": {"write_thing": ["Owner@Example.com"]}}


@pytest.fixture
def ruled():
    seen = []

    def upstream(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, stream=Chunks(b'{"ok": true}'))

    log = logging.getLogger("test.ruled")
    log.handlers.clear()
    log.setLevel(logging.INFO)
    records = []

    class Capture(logging.Handler):
        def emit(self, record):
            records.append(json.loads(record.getMessage()))

    log.addHandler(Capture())
    app = make_app(validate_config(RULES), key_resolver=lambda _t: PUB, transport=httpx.MockTransport(upstream), logger=log)
    return TestClient(app, raise_server_exceptions=True), seen, records


def tool_call(name="write_thing", id_=7):
    return {"jsonrpc": "2.0", "id": id_, "method": "tools/call", "params": {"name": name, "arguments": {"a": 1}}}


def test_the_listed_login_may_call_a_ruled_tool(ruled):
    client, seen, _ = ruled
    r = post(client, token(), body=tool_call())
    assert r.status_code == 200 and r.json() == {"ok": True} and len(seen) == 1


def test_another_login_is_refused_logged_and_never_reaches_the_server(ruled):
    client, seen, records = ruled
    r = post(client, token(email="work@example.com"), body=tool_call())
    assert r.status_code == 200 and r.json()["result"]["isError"] is True and r.json()["id"] == 7
    assert seen == []
    assert records[-1]["result"] == 403 and records[-1]["email"] == "work@example.com" and records[-1]["tool"] == "write_thing"


def test_other_tools_stay_open_to_every_login(ruled):
    client, seen, _ = ruled
    assert post(client, token(email="work@example.com"), body=tool_call("read_thing")).status_code == 200
    assert len(seen) == 1


def test_a_batch_cant_hide_a_ruled_tool(ruled):
    client, seen, records = ruled
    h = {"Cf-Access-Jwt-Assertion": token(email="work@example.com")}
    r = client.post("/mcp", headers=h, json=[tool_call("read_thing", 1), tool_call("write_thing", 2)])
    assert r.status_code == 403 and seen == [] and records[-1]["result"] == 403


def test_an_unreadable_body_is_refused_when_rules_are_on(ruled):
    client, seen, records = ruled
    h = {"Cf-Access-Jwt-Assertion": token(email="work@example.com")}
    assert client.post("/mcp", headers=h, content=b"\xff\xfe not json").status_code == 400
    assert seen == [] and records[-1]["result"] == 400


@pytest.mark.parametrize("weird", [{"params": "x"}, {"params": {"name": 5}}, {"params": {}}])
def test_a_tool_call_with_no_readable_name_is_refused(ruled, weird):
    client, seen, _ = ruled
    body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call", **weird}
    r = post(client, token(email="work@example.com"), body=body)
    assert r.json()["result"]["isError"] is True and seen == []


def test_a_forged_email_header_counts_for_nothing(ruled):
    client, seen, _ = ruled
    r = post(client, token(email="work@example.com"), body=tool_call(),
             headers={"X-Forwarded-Email": "owner@example.com", "Cf-Access-Authenticated-User-Email": "owner@example.com"})
    assert r.json()["result"]["isError"] is True and seen == []


def test_a_forged_header_with_no_valid_token_is_refused(ruled):
    client, seen, _ = ruled
    r = client.post("/mcp", headers={"Cf-Access-Authenticated-User-Email": "owner@example.com"}, json=tool_call())
    assert r.status_code == 401 and seen == []
    assert post(client, token(key=OTHER_KEY), body=tool_call()).status_code == 401 and seen == []


def test_an_unknown_or_missing_email_is_refused(ruled):
    client, seen, _ = ruled
    assert post(client, token(email="stranger@example.com"), body=tool_call()).status_code == 403
    assert post(client, token(email=None), body=tool_call()).status_code == 403
    assert seen == []


@pytest.mark.parametrize("bad", [
    {"write_thing": ["stranger@example.com"]}, {"write_thing": "owner@example.com"},
])
def test_a_bad_tool_rule_stops_it_starting(bad):
    with pytest.raises(ConfigError):
        validate_config({**RULES, "tool_access": bad})


def test_no_rules_changes_nothing(harness):
    client, seen, _ = harness
    assert post(client, token(), body=tool_call()).status_code == 200 and len(seen) == 1
    assert client.post("/mcp", headers={"Cf-Access-Jwt-Assertion": token()}, content=b"not json").status_code == 200


# Contact cards: the Gmail login reads and writes, the two work logins only read. All made up.
CARD_RULES = {**RAW, "allowed_emails": ["gmail@example.com", "work1@example.com", "work2@example.com"],
              "tool_access": {"create_contact_card": ["gmail@example.com"], "update_contact_card": ["gmail@example.com"]}}
TOOLS = {"tools": [{"name": "search_contact_cards"}, {"name": "get_contact_card"}, {"name": "create_contact_card"},
                   {"name": "update_contact_card"}, {"name": "send_message"}]}


def cards_app(kind):
    seen = []

    async def upstream(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        msg = json.loads(await request.aread())
        if msg.get("method") == "tools/list":
            reply = {"jsonrpc": "2.0", "id": msg.get("id"), "result": TOOLS}
            if kind == "sse":
                return httpx.Response(200, stream=Chunks(f"event: message\ndata: {json.dumps(reply)}\n\n".encode()),
                                      headers={"content-type": "text/event-stream"})
            return httpx.Response(200, stream=Chunks(json.dumps(reply).encode()), headers={"content-type": "application/json"})
        done = {"jsonrpc": "2.0", "id": msg.get("id"), "result": {"content": []}}
        return httpx.Response(200, stream=Chunks(json.dumps(done).encode()), headers={"content-type": "application/json"})

    log = logging.getLogger(f"test.cards.{kind}")
    log.handlers.clear()
    log.setLevel(logging.INFO)
    records = []

    class Capture(logging.Handler):
        def emit(self, record):
            records.append(json.loads(record.getMessage()))

    log.addHandler(Capture())
    app = make_app(validate_config(CARD_RULES), key_resolver=lambda _t: PUB, transport=httpx.MockTransport(upstream), logger=log)
    return TestClient(app, raise_server_exceptions=True), seen, records


def listed(r):
    text = r.text
    if text.lstrip().startswith("event:"):
        text = [line[5:] for line in text.split("\n") if line.startswith("data:")][0]
    return [t["name"] for t in json.loads(text)["result"]["tools"]]


@pytest.mark.parametrize("kind", ["json", "sse"])
def test_work_logins_arent_offered_card_writes(kind):
    client, _, _ = cards_app(kind)
    ask = {"jsonrpc": "2.0", "id": 3, "method": "tools/list"}
    everything = listed(post(client, token(email="gmail@example.com"), body=ask))
    assert everything == [t["name"] for t in TOOLS["tools"]]
    for work in ("work1@example.com", "work2@example.com"):
        names = listed(post(client, token(email=work), body=ask))
        assert names == ["search_contact_cards", "get_contact_card", "send_message"]


@pytest.mark.parametrize("who, tool, allowed", [
    ("gmail@example.com", "create_contact_card", True), ("gmail@example.com", "update_contact_card", True),
    ("gmail@example.com", "search_contact_cards", True),
    ("work1@example.com", "create_contact_card", False), ("work2@example.com", "update_contact_card", False),
    ("work1@example.com", "search_contact_cards", True), ("work2@example.com", "get_contact_card", True),
])
def test_card_writes_are_checked_on_every_call(who, tool, allowed):
    client, seen, records = cards_app("json")
    r = post(client, token(email=who), body=tool_call(tool))
    assert (len(seen) == 1) == allowed
    assert ("isError" in r.json().get("result", {})) == (not allowed)
    assert records[-1]["result"] == (200 if allowed else 403)


def test_unknown_logins_and_forged_headers_get_nothing_about_cards():
    client, seen, _ = cards_app("json")
    assert post(client, token(email="stranger@example.com"), body=tool_call("get_contact_card")).status_code == 403
    assert client.post("/mcp", headers={"Cf-Access-Authenticated-User-Email": "gmail@example.com"},
                       json=tool_call("create_contact_card")).status_code == 401
    assert seen == []


def test_contact_details_stay_out_of_the_log():
    client, _, records = cards_app("json")
    call = {"jsonrpc": "2.0", "id": 9, "method": "tools/call",
            "params": {"name": "create_contact_card", "arguments": {"given": "Made", "family": "Up", "phones": ["+1 555 000 0001"]}}}
    post(client, token(email="gmail@example.com"), body=call)
    assert records[-1]["args"] == "fields: family,given,phones"
    assert "555" not in json.dumps(records)
