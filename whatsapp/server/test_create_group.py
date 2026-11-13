"""Creating a group from the connector. All data here is made up, and no request leaves this test."""
import asyncio
import sqlite3

import pytest
import requests

import new_group
import server as s

DANA = "15550000001@s.whatsapp.net"
DANA_LID = "777777777@lid"
ROBIN = "15550000002@s.whatsapp.net"
DANA_TWO = "15550000003@s.whatsapp.net"
SAM = "15550000004@s.whatsapp.net"
NEW = "120363000000000001@g.us"

CONTACTS = [
    {"jid": DANA, "full_name": "Dana Smith", "first_name": "Dana"},
    {"jid": DANA_LID, "push_name": "Dana on holiday"},
    {"jid": ROBIN, "full_name": "Robin Park", "first_name": "Robin"},
    {"jid": DANA_TWO, "full_name": "Dana Jones", "first_name": "Dana"},
    {"jid": SAM, "push_name": "Sam Profile"},
]
ALIASES = [[DANA_LID, DANA]]


def call(*args, **kwargs):
    return getattr(s.create_group, "fn", s.create_group)(*args, **kwargs)


class Reply:
    def __init__(self, status, body, ok_json=True):
        self.status_code, self.body, self.ok_json = status, body, ok_json

    def json(self):
        if not self.ok_json:
            raise ValueError("not json")
        return self.body


@pytest.fixture
def bridge(monkeypatch, tmp_path):
    calls, logged = [], []
    answer = {"reply": Reply(200, {"ok": True, "jid": NEW, "name": "Made up", "added": [DANA, ROBIN], "not_added": []})}

    def get(url, timeout=None):
        assert url == f"{s.BRIDGE}/app/contacts"
        return Reply(200, {"contacts": CONTACTS, "aliases": ALIASES})

    def post(url, json=None, timeout=None):
        calls.append({"url": url, "json": json})
        if isinstance(answer["reply"], Exception):
            raise answer["reply"]
        return answer["reply"]

    db = tmp_path / "messages.db"
    conn = sqlite3.connect(db)
    conn.execute("CREATE TABLE chats (jid TEXT, name TEXT)")
    conn.execute("INSERT INTO chats VALUES ('120363000000000009@g.us', 'Already Here')")
    conn.commit()
    conn.close()
    monkeypatch.setattr(s.guards, "STORE", tmp_path)
    monkeypatch.setattr(s.requests, "get", get)
    monkeypatch.setattr(s.requests, "post", post)
    monkeypatch.setattr(s, "_bridge_health", lambda: {"running": True, "connected": True, "logged_in": True})
    monkeypatch.setattr(s.guards, "log_send", lambda *a, **k: logged.append(a))
    return calls, answer, logged


def test_it_creates_through_the_bridges_one_path_and_gives_the_address(bridge):
    calls, _, logged = bridge
    out = call("Made up", ["Dana Smith", "Robin Park"])
    assert calls == [{"url": f"{s.BRIDGE}/app/group/create", "json": {"name": "Made up", "members": [DANA, ROBIN], "via": "connector"}}]
    assert NEW in out and "Dana Smith" in out and "Robin Park" in out
    assert "Not added" not in out
    assert logged and logged[0][0] == "create_group" and logged[0][1] == NEW and logged[0][3] is True


def test_a_number_or_address_works_and_both_addresses_count_once(bridge):
    calls, _, _ = bridge
    call("Made up", ["+1 555 000 0001", DANA_LID, DANA, "robin park"])
    assert calls[0]["json"]["members"] == [DANA, ROBIN]


def test_a_name_matching_several_gives_the_choices_and_creates_nothing(bridge):
    calls, _, _ = bridge
    out = call("Made up", ["Dana", "Robin Park"])
    assert calls == []
    assert out.startswith("Not created")
    assert "Dana Smith" in out and "Dana Jones" in out and DANA in out and DANA_TWO in out


def test_a_part_of_a_name_isnt_exact_even_with_one_match(bridge):
    calls, _, _ = bridge
    out = call("Made up", ["Robin"])
    assert calls == [] and out.startswith("Not created") and ROBIN in out


def test_a_profile_name_isnt_trusted_even_when_it_matches_whole(bridge):
    calls, _, _ = bridge
    out = call("Made up", ["Sam Profile"])
    assert calls == [] and out.startswith("Not created") and SAM in out


def test_someone_unknown_stops_the_whole_call(bridge):
    calls, _, _ = bridge
    out = call("Made up", ["Robin Park", "Nobody Known"])
    assert calls == [] and out.startswith("Not created") and "Nobody Known" in out


def test_a_number_not_in_contacts_is_unknown(bridge):
    calls, _, _ = bridge
    assert call("Made up", ["15559998888"]).startswith("Not created")
    assert calls == []


def test_going_ahead_without_someone_unknown_needs_asking_for(bridge):
    calls, answer, _ = bridge
    answer["reply"] = Reply(200, {"ok": True, "jid": NEW, "name": "Made up", "added": [ROBIN], "not_added": []})
    out = call("Made up", ["Robin Park", "Nobody Known"], continue_without_unknown=True)
    assert calls[0]["json"]["members"] == [ROBIN]
    assert "Nobody Known" in out and "Left out" in out


def test_that_option_never_skips_someone_unclear(bridge):
    calls, _, _ = bridge
    out = call("Made up", ["Robin Park", "Dana"], continue_without_unknown=True)
    assert calls == [] and out.startswith("Not created")


def test_nobody_left_creates_nothing(bridge):
    calls, _, _ = bridge
    assert call("Made up", ["Nobody Known"], continue_without_unknown=True).startswith("Not created")
    assert call("Made up", []).startswith("Not created")
    assert calls == []


def test_a_group_with_that_name_needs_saying_so(bridge):
    calls, _, _ = bridge
    out = call("  already   here ", ["Robin Park"])
    assert calls == [] and out.startswith("Not created") and "120363000000000009@g.us" in out
    call("Already Here", ["Robin Park"], allow_same_name=True)
    assert len(calls) == 1


def test_it_says_exactly_who_wasnt_added_and_why(bridge):
    _, answer, _ = bridge
    answer["reply"] = Reply(200, {"ok": True, "jid": NEW, "name": "Made up", "added": [DANA], "not_added": [
        {"jid": ROBIN, "code": 403}, {"jid": DANA_TWO, "code": 403, "invited": True}, {"jid": SAM, "code": 0},
    ]})
    out = call("Made up", ["Dana Smith", "Robin Park", "Dana Jones", SAM])
    assert "Added (1): Dana Smith" in out
    assert "Robin Park" in out and "privacy" in out
    assert "invitation" in out
    assert "weren't in WhatsApp's reply" in out


@pytest.mark.parametrize("reply", [
    Reply(429, {"error": "That's a lot of new groups for now. No group was created. Try again later."}),
    Reply(422, {"error": "WhatsApp refused that, and no group was created."}),
])
def test_a_refusal_is_passed_on(bridge, reply):
    calls, answer, logged = bridge
    answer["reply"] = reply
    out = call("Made up", ["Robin Park"])
    assert out.startswith("Not created") and reply.body["error"] in out
    assert len(calls) == 1, "never retried"
    assert logged[0][3] is False


@pytest.mark.parametrize("reply", [
    Reply(503, {"error": "WhatsApp didn't answer, so the group may or may not exist. Check your chats before trying again."}),
    requests.Timeout("made up"),
    requests.ConnectionError("made up"),
    Reply(200, {}, ok_json=False),
    Reply(200, {"ok": True}),
])
def test_an_unclear_result_never_retries_and_never_claims_either_way(bridge, reply):
    calls, answer, _ = bridge
    answer["reply"] = reply
    out = call("Made up", ["Robin Park"])
    assert len(calls) == 1, "a retry could make a second group and notify people twice"
    assert not out.startswith("Created") and not out.startswith("Not created")
    assert "may or may not exist" in out and "list_chats" in out


def test_with_whatsapp_down_nothing_is_attempted(bridge, monkeypatch):
    calls, _, _ = bridge
    monkeypatch.setattr(s, "_bridge_health", lambda: {"running": True, "connected": False, "logged_in": True})
    assert call("Made up", ["Robin Park"]).startswith("Not created")
    assert calls == []


def test_contacts_unavailable_creates_nothing(bridge, monkeypatch):
    calls, _, _ = bridge
    def broken(url, timeout=None):
        raise requests.ConnectionError("made up")
    monkeypatch.setattr(s.requests, "get", broken)
    assert call("Made up", ["Robin Park"]).startswith("Not created")
    assert calls == []


def test_the_tool_says_what_it_does_and_is_marked_outward_facing():
    async def tools():
        return {t.name: t for t in await s.mcp.list_tools()}

    tool = asyncio.run(tools())["create_group"]
    text = tool.description.lower()
    for must in ("notifies", "can't be undone", "confirm", "never", "untrusted"):
        assert must in text, must
    hints = tool.annotations
    assert hints.readOnlyHint is False and hints.destructiveHint is True
    assert hints.idempotentHint is False and hints.openWorldHint is True


def test_the_connect_time_instructions_cover_it():
    text = s.SAFETY.lower()
    assert "create_group" in text and "can't be undone" in text and "never" in text


def test_photos_rename_rules_stay_as_they_were():
    async def tools():
        return {t.name for t in await s.mcp.list_tools()}

    names = asyncio.run(tools())
    assert not [t for t in names if "leave" in t or "exit" in t or "delete_group" in t or "group_photo" in t]


def test_names_are_compared_whole_and_tidily():
    book = new_group.Book(CONTACTS, ALIASES)
    found = new_group.resolve(["  dana   SMITH "], book)
    assert list(found.found) == [DANA] and not found.unclear and not found.unknown
