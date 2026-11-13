"""Renaming a group from the connector. All data here is made up, and no request leaves this test."""
import asyncio

import pytest
import requests

import server as s

GROUP = "120363000000000001@g.us"


def call(fn, *args):
    return getattr(fn, "fn", fn)(*args)


class Reply:
    def __init__(self, status, body):
        self.status_code, self.body = status, body

    def json(self):
        return self.body


@pytest.fixture
def bridge(monkeypatch):
    calls = []
    answer = {"reply": Reply(200, {"ok": True, "name": "New name", "changed": True})}

    def post(url, params=None, json=None, timeout=None):
        calls.append({"url": url, "params": params, "json": json})
        if isinstance(answer["reply"], Exception):
            raise answer["reply"]
        return answer["reply"]

    monkeypatch.setattr(s.requests, "post", post)
    monkeypatch.setattr(s.wa, "forget_names", lambda: None)
    return calls, answer


def test_it_reaches_the_bridges_rename_for_that_group(bridge):
    calls, _ = bridge
    out = call(s.rename_group, GROUP, "New name")
    assert calls == [{"url": f"{s.BRIDGE}/app/group/rename", "params": {"jid": GROUP}, "json": {"name": "New name"}}]
    assert "Renamed" in out and "Everyone" in out


@pytest.mark.parametrize("target", ["15551110000@s.whatsapp.net", "777777777@lid", "", "not an address"])
def test_only_a_group_can_be_renamed(bridge, target):
    calls, _ = bridge
    assert call(s.rename_group, target, "New name").startswith("Not changed")
    assert calls == []


def test_a_refusal_is_passed_on_as_not_changed(bridge):
    _, answer = bridge
    answer["reply"] = Reply(409, {"error": "Only this group's admins can rename it, so nothing changed."})
    assert call(s.rename_group, GROUP, "New name").startswith("Not changed: Only this group's admins")


def test_the_same_name_says_so(bridge):
    _, answer = bridge
    answer["reply"] = Reply(200, {"ok": True, "name": "New name", "changed": False})
    assert "already called" in call(s.rename_group, GROUP, "New name")


def test_no_answer_doesnt_claim_either_way(bridge):
    _, answer = bridge
    answer["reply"] = requests.ConnectionError("made up")
    assert "Check the group's name" in call(s.rename_group, GROUP, "New name")


def test_leaving_and_deleting_arent_connector_tools():
    async def names():
        return {t.name for t in await s.mcp.list_tools()} if hasattr(s.mcp, "list_tools") else set((await s.mcp.get_tools()).keys())

    tools = asyncio.run(names())
    assert "rename_group" in tools
    assert not [t for t in tools if "leave" in t or "exit" in t or "delete_group" in t or "remove_group" in t]
