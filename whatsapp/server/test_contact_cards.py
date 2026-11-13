"""Contact card tools, against a fake bridge that keeps the real one's rules. All data is made up."""
import asyncio

import pytest

import cards
import guards
import server as s

PN = "15550001001@s.whatsapp.net"
OTHER = "15550001002@s.whatsapp.net"


class FakeBridge:
    """The bridge's card routes: versions, one card per person, and no delete."""

    def __init__(self):
        self.cards = {}
        self.calls = []
        self.next = 0

    def __call__(self, method, path="", **kw):
        self.calls.append((method, path))
        if method == "GET":
            params = kw.get("params") or {}
            if "q" in params:
                q = params["q"].lower()
                found = [c for c in self.cards.values() if q in str(c).lower()]
                return 200, {"cards": sorted(found, key=lambda c: c["display"])[: params.get("limit", 10)]}
            if "jid" in params:
                return 200, {"card": next((c for c in self.cards.values() if params["jid"] in c.get("links", [])), None)}
            return 200, {"cards": list(self.cards.values())}
        if path == "/save":
            card, expect = dict(kw["json"]["card"]), kw["json"]["expect_version"]
            for link in card.get("links") or []:
                owner = next((c for c in self.cards.values() if link in c.get("links", []) and c["id"] != card.get("id")), None)
                if owner:
                    return 400, {"error": f"{link} is already linked to {owner['display']}. Unlink it there first"}
            if card.get("id"):
                if self.cards[card["id"]]["version"] != expect:
                    return 409, {"error": "This card was changed somewhere else since you opened it."}
                card["version"] = expect + 1
            else:
                self.next += 1
                card["id"], card["version"] = f"{self.next:024d}", 1
            card["display"] = card.get("display") or " ".join(card.get("name", {}).get(f, "") for f in ("given", "family")).strip()
            self.cards[card["id"]] = card
            return 200, {"card": card}
        return 404, {}


@pytest.fixture
def bridge(monkeypatch):
    fake = FakeBridge()
    monkeypatch.setattr(cards, "_request", fake)
    reserved = []
    monkeypatch.setattr(guards, "reserve_name_change", lambda: reserved.append(1))
    fake.reserved = reserved
    return fake


def test_creating_a_card_links_people_by_number(bridge):
    out = s.create_contact_card(given="Ann", family="Example", company="Made-up Co",
                                phones=[cards.LabeledValue(value="+1 555 000 1001", label="mobile")], link_to=["+1 (555) 000-1001"])
    card = next(iter(bridge.cards.values()))
    assert out.startswith("Created.") and card["display"] == "Ann Example" and card["links"] == [PN]
    assert card["phones"] == [{"label": "mobile", "value": "+1 555 000 1001"}] and bridge.reserved == [1]


def test_update_changes_only_what_it_names(bridge):
    s.create_contact_card(given="Ann", family="Example", title="Engineer", emails=[cards.LabeledValue(value="ann@example.com")])
    card_id = next(iter(bridge.cards))
    out = s.update_contact_card(card_id, company="New Co", link_to=[PN])
    card = bridge.cards[card_id]
    assert out.startswith("Updated.") and card["company"] == "New Co" and card["title"] == "Engineer"
    assert card["emails"] == [{"label": "", "value": "ann@example.com"}] and card["links"] == [PN] and card["version"] == 2
    s.update_contact_card(card_id, title="", unlink=[PN])
    assert bridge.cards[card_id]["title"] == "" and bridge.cards[card_id]["links"] == []


def test_refusals_come_back_as_said(bridge):
    s.create_contact_card(display_name="First", link_to=[PN])
    assert "already linked to First" in s.create_contact_card(display_name="Second", link_to=[PN])
    assert s.update_contact_card("000000000000000000000099", title="x") == "Not changed: there's no card with that ID."
    assert "group" in s.create_contact_card(display_name="Group", link_to=["120363000000000001@g.us"])


def test_reading_by_number_id_or_name(bridge):
    s.create_contact_card(display_name="Ann Example", link_to=[PN])
    s.create_contact_card(display_name="Ann Other")
    card_id = next(iter(bridge.cards))
    for how in ("+1 555 000 1001", card_id):
        out = s.get_contact_card(how)
        assert out.startswith(cards.UNTRUSTED) and "Ann Example" in out
    assert s.get_contact_card("Ann").startswith("More than one card matches")
    assert "Ann Other" in s.get_contact_card("Ann Other")
    assert s.get_contact_card("Nobody") == "No card found."
    assert s.search_contact_cards("ann").startswith(cards.UNTRUSTED)


def test_an_email_goes_on_the_persons_card(bridge, monkeypatch):
    monkeypatch.setattr(s.wa, "custom_name", lambda jid: None)
    monkeypatch.setattr(s.wa, "person_name", lambda user: "Saved Person")
    monkeypatch.setattr(s.contact_emails, "remove", lambda jid, email: False)
    assert s.add_contact_email(PN, "Person@Example.com").startswith("Added Person@example.com to Saved Person's card")
    card = next(iter(bridge.cards.values()))
    assert card["links"] == [PN]
    assert s.add_contact_email(PN, "person@example.com") == "They already have person@example.com."
    assert s.remove_contact_email(PN, "person@example.com") == "Removed."
    assert bridge.cards[card["id"]]["emails"] == []


def test_cards_cant_be_deleted_here_and_writes_say_so():
    tools = {t.name: t for t in asyncio.run(s.mcp.list_tools())}
    assert not [n for n in tools if "delete" in n and "card" in n]
    for name in ("create_contact_card", "update_contact_card"):
        assert tools[name].annotations.readOnlyHint is False and tools[name].annotations.destructiveHint is False
    for name in ("search_contact_cards", "get_contact_card"):
        assert "untrusted" in tools[name].description.lower()
    assert "Only logins given write access" in tools["create_contact_card"].description
