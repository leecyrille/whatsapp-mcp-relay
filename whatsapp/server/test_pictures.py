import base64

import pytest

import guards
from guards import Refused

PNG = b"\x89PNG\r\n\x1a\n" + b"\x00" * 32
JPEG = b"\xff\xd8\xff\xe0" + b"\x00" * 32
WEBP = b"RIFF\x24\x00\x00\x00WEBPVP8 " + b"\x00" * 32
LIMITS = {**guards.DEFAULT_LIMITS, "picture_per_hour": 3, "picture_per_day": 5}


def b64(data):
    return base64.b64encode(data).decode()


@pytest.mark.parametrize("data", [PNG, JPEG, WEBP])
def test_raster_images_pass_by_content(data):
    assert guards.picture_bytes(b64(data), None) == data


def test_data_url_prefix_is_accepted():
    assert guards.picture_bytes("data:image/png;base64," + b64(PNG), None) == PNG


@pytest.mark.parametrize("data", [
    b'<svg xmlns="http://www.w3.org/2000/svg"/>',
    b'<?xml version="1.0"?><svg/>',
    b"GIF89a\x01\x00\x01\x00",
    b"MZ\x90\x00",
    b"<html><script>alert(1)</script>",
    b"",
])
def test_anything_else_is_refused(data):
    with pytest.raises(Refused):
        guards.picture_bytes(b64(data) if data else "", None if data else "x")


def test_a_false_extension_makes_no_difference(tmp_path):
    (tmp_path / "photo.png").write_bytes(b"<svg/>")
    with pytest.raises(Refused, match="JPEG, PNG or WebP"):
        guards.picture_bytes(None, "photo.png", tmp_path)
    (tmp_path / "actually.txt").write_bytes(PNG)
    assert guards.picture_bytes(None, "actually.txt", tmp_path) == PNG


def test_needs_exactly_one_source():
    with pytest.raises(Refused, match="either"):
        guards.picture_bytes(None, None)
    with pytest.raises(Refused, match="either"):
        guards.picture_bytes(b64(PNG), "x.png")


def test_bad_base64_is_refused():
    with pytest.raises(Refused, match="base64"):
        guards.picture_bytes("not base64 at all!!", None)


@pytest.mark.parametrize("path", [
    r"..\secret.png", r"C:\Windows\win.ini", r"C:\mcp\.env", "../../.env",
    "https://evil.example/x.png", r"\\server\share\x.png",
])
def test_nothing_outside_the_outbox_can_be_read(tmp_path, path):
    outbox = tmp_path / "outbox"
    outbox.mkdir()
    (tmp_path / "secret.png").write_bytes(PNG)
    with pytest.raises(Refused):
        guards.picture_bytes(None, path, outbox)


def test_size_limits(tmp_path, monkeypatch):
    monkeypatch.setattr(guards, "PICTURE_MAX_BYTES", 100)
    big = PNG + b"\x00" * 200
    with pytest.raises(Refused, match="under"):
        guards.picture_bytes(b64(big), None)
    (tmp_path / "big.png").write_bytes(big)
    with pytest.raises(Refused, match="under"):
        guards.picture_bytes(None, "big.png", tmp_path)


def test_picture_limits_and_they_are_separate_from_sends(tmp_path):
    db = tmp_path / "r.db"
    for i in range(3):
        guards.reserve_picture_change(db, LIMITS, now=1000 + i)
    with pytest.raises(Refused, match="3 per hour"):
        guards.reserve_picture_change(db, LIMITS, now=1003)
    guards.reserve_send(db, guards.DEFAULT_LIMITS, now=1004)
    assert guards.usage(db, guards.DEFAULT_LIMITS, now=1004)["per_minute"]["used"] == 1
    guards.reserve_picture_change(db, LIMITS, now=1000 + 3601)
    guards.reserve_picture_change(db, LIMITS, now=1000 + 3602)
    with pytest.raises(Refused, match="5 per day"):
        guards.reserve_picture_change(db, LIMITS, now=1000 + 7300)


def test_old_limits_files_still_work(tmp_path):
    f = tmp_path / "limits.json"
    f.write_text('{"per_day": 10}')
    loaded = guards.load_limits(f)
    assert loaded["picture_per_hour"] == 60 and loaded["per_day"] == 10


class FakeResponse:
    def __init__(self, status, body):
        self.status_code = status
        self._body = body

    def json(self):
        return self._body


@pytest.fixture
def server(monkeypatch, tmp_path):
    import server as s

    monkeypatch.setattr(s.guards, "require_known_recipient", lambda chat: "15550000001@s.whatsapp.net")
    monkeypatch.setattr(s.guards, "reserve_picture_change", lambda: None)
    return s


def tool(fn):
    return getattr(fn, "fn", fn)


def test_set_goes_only_to_the_local_bridge(server, monkeypatch):
    seen = {}

    def fake(method, url, **kw):
        seen.update(method=method, url=url, **kw)
        return FakeResponse(200, {"ok": True})

    monkeypatch.setattr(server.requests, "request", fake)
    out = tool(server.set_chat_picture)("15550000001", image_base64=b64(PNG))
    assert "Nothing changed on WhatsApp" in out
    assert seen["method"] == "POST" and seen["url"] == "http://127.0.0.1:18700/api/app/picture/own"
    assert seen["data"] == PNG and seen["params"] == {"jid": "15550000001@s.whatsapp.net"}


def test_bad_image_never_reaches_the_bridge(server, monkeypatch):
    monkeypatch.setattr(server.requests, "request", lambda *a, **k: pytest.fail("reached the bridge"))
    out = tool(server.set_chat_picture)("15550000001", image_base64=b64(b"<svg/>"))
    assert out.startswith("Not changed:")


def test_bridge_refusal_is_reported(server, monkeypatch):
    monkeypatch.setattr(server.requests, "request", lambda *a, **k: FakeResponse(400, {"error": "Couldn't use that image."}))
    assert "Couldn't use that image" in tool(server.set_chat_picture)("15550000001", image_base64=b64(PNG))


def test_remove(server, monkeypatch):
    monkeypatch.setattr(server.requests, "request", lambda *a, **k: FakeResponse(200, {"ok": True, "changed": True}))
    assert "Back to the WhatsApp picture" in tool(server.remove_chat_picture)("15550000001")
    monkeypatch.setattr(server.requests, "request", lambda *a, **k: FakeResponse(200, {"ok": True, "changed": False}))
    assert "no replacement" in tool(server.remove_chat_picture)("15550000001")


def test_instructions_and_tools_say_it_never_touches_whatsapp(server):
    assert "never change anything on WhatsApp" in server.SAFETY
    assert "60 changes an hour" in server.SAFETY
    for fn in (server.set_chat_picture, server.remove_chat_picture):
        doc = tool(fn).__doc__
        assert "WhatsApp" in doc and "60 changes an hour" in doc
