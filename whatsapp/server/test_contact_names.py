"""The user's own names for people, in the connector. All data here is made up."""
import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(r"C:\mcp\whatsapp\src\whatsapp-mcp-server")))
import whatsapp as wa  # noqa: E402

import guards  # noqa: E402
from guards import Refused  # noqa: E402

PN = "15551110000@s.whatsapp.net"
LID = "777777777@lid"
STRANGER = "888888888@lid"


@pytest.fixture
def store(tmp_path, monkeypatch):
    conn = sqlite3.connect(tmp_path / "whatsapp.db")
    conn.executescript(
        """
        CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT);
        CREATE TABLE whatsmeow_lid_map (lid TEXT, pn TEXT);
        CREATE TABLE whatsmeow_device (jid TEXT, lid TEXT);
        INSERT INTO whatsmeow_contacts VALUES ('me', '15551110000@s.whatsapp.net', '', 'Saved Name', '', '');
        INSERT INTO whatsmeow_lid_map VALUES ('777777777', '15551110000');
        """
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(wa, "WHATSAPP_DB_PATH", str(tmp_path / "whatsapp.db"))
    wa.forget_names()
    yield tmp_path
    wa.forget_names()


def write_names(store, names):
    (store / "app-names.json").write_text(json.dumps(names), encoding="utf-8")
    wa.forget_names()


def test_no_file_changes_nothing(store):
    assert wa.person_name("15551110000") == "Saved Name"
    assert wa.custom_name(PN) is None
    assert wa.custom_names() == {}


def test_own_name_beats_saved_name_at_both_addresses(store):
    write_names(store, {PN: "Sam Smith (ACME)"})
    assert wa.person_name("15551110000") == "Sam Smith (ACME)"
    assert wa.person_name("777777777") == "Sam Smith (ACME)"
    assert wa.get_sender_name(LID) == "Sam Smith (ACME)"
    assert wa.custom_name(LID) == "Sam Smith (ACME)"
    assert wa.custom_name(PN) == "Sam Smith (ACME)"
    assert wa.resolve_mentions("Ask @777777777") == "Ask @Sam Smith (ACME)"


def test_named_by_hidden_id_reaches_the_number(store):
    write_names(store, {LID: "By Hidden"})
    assert wa.custom_name(PN) == "By Hidden"
    assert wa.person_name("15551110000") == "By Hidden"


def test_phone_number_name_wins(store):
    write_names(store, {LID: "Older", PN: "Newer"})
    assert wa.person_name("777777777") == "Newer"
    assert wa.custom_name(LID) == "Newer"


def test_unknown_member_can_be_named(store):
    write_names(store, {STRANGER: "Group Member"})
    assert wa.person_name("888888888") == "Group Member"


def test_damaged_file_is_ignored(store):
    (store / "app-names.json").write_text("{damaged", encoding="utf-8")
    wa.forget_names()
    assert wa.person_name("15551110000") == "Saved Name"
    (store / "app-names.json").write_text('["a list"]', encoding="utf-8")
    wa.forget_names()
    assert wa.custom_names() == {}


@pytest.mark.parametrize("value", ["120363000000000001@g.us", "status@broadcast", "123@broadcast", "", "not a number"])
def test_only_people_can_be_named(value):
    with pytest.raises(Refused):
        guards.person_address(value)


def test_people_accepted():
    assert guards.person_address("15551110000") == PN
    assert guards.person_address(LID) == LID


def test_a_name_does_not_make_someone_a_recipient(store):
    """The important one: naming a stranger must not let the connector message them."""
    write_names(store, {STRANGER: "Group Member"})
    with pytest.raises(Refused):
        guards.require_known_recipient(STRANGER, store)


def test_a_name_counts_for_pictures(store):
    assert not guards.has_custom_name(STRANGER, store)
    write_names(store, {STRANGER: "Group Member"})
    assert guards.has_custom_name(STRANGER, store)


def test_name_is_found_under_the_other_address(store):
    write_names(store, {PN: "Sam"})
    assert guards.has_custom_name(LID, store)
    assert not guards.has_custom_name(STRANGER, store)


def test_picture_tool_refuses_a_stranger_and_accepts_someone_named(store, monkeypatch):
    import server as s

    monkeypatch.setattr(s.guards, "STORE", store)
    monkeypatch.setattr(s.guards, "has_custom_name", lambda jid: guards_has(jid, store))
    with pytest.raises(Refused, match="set_contact_name"):
        s._picture_target(STRANGER)
    write_names(store, {STRANGER: "Group Member"})
    assert s._picture_target(STRANGER) == STRANGER


def guards_has(jid, store):
    return json.loads((store / "app-names.json").read_text(encoding="utf-8")).get(jid) is not None if (store / "app-names.json").exists() else False


def test_name_limits_are_separate_from_sends(tmp_path):
    db = tmp_path / "rate.db"
    limits = dict(guards.DEFAULT_LIMITS, name_per_hour=2, name_per_day=3)
    guards.reserve_name_change(db, limits, now=1000.0)
    guards.reserve_name_change(db, limits, now=1001.0)
    with pytest.raises(Refused, match="Name limit"):
        guards.reserve_name_change(db, limits, now=1002.0)
    guards.reserve_send(db, dict(guards.DEFAULT_LIMITS), now=1003.0)
    guards.reserve_name_change(db, limits, now=1000.0 + 3601)
    with pytest.raises(Refused, match="per day"):
        guards.reserve_name_change(db, limits, now=1000.0 + 7300)


def test_team_fits_the_default_limit():
    assert guards.DEFAULT_LIMITS["name_per_hour"] >= 40
