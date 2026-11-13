"""The connector shows hidden IDs and @mentions by name. All data here is made up."""
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(r"C:\mcp\whatsapp\src\whatsapp-mcp-server")))
import whatsapp as wa  # noqa: E402


@pytest.fixture
def names(tmp_path, monkeypatch):
    db = tmp_path / "whatsapp.db"
    conn = sqlite3.connect(db)
    conn.executescript(
        """
        CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT);
        CREATE TABLE whatsmeow_lid_map (lid TEXT, pn TEXT);
        CREATE TABLE whatsmeow_device (jid TEXT, lid TEXT);
        INSERT INTO whatsmeow_contacts VALUES ('me', '15551110000@s.whatsapp.net', '', 'Dana Saved', '', '');
        INSERT INTO whatsmeow_contacts VALUES ('me', '777777777@lid', '', '', 'Dana', '');
        INSERT INTO whatsmeow_contacts VALUES ('me', '888888888@lid', '', '', 'Only Profile', '');
        INSERT INTO whatsmeow_lid_map VALUES ('777777777', '15551110000');
        INSERT INTO whatsmeow_lid_map VALUES ('666666666', '15552220000');
        INSERT INTO whatsmeow_device VALUES ('15559990000:12@s.whatsapp.net', '11111:12@lid');
        """
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(wa, "WHATSAPP_DB_PATH", str(db))
    monkeypatch.setattr(wa, "_names_loaded_at", 0.0)
    yield


def test_best_name_for_each_kind_of_address(names):
    assert wa.person_name("777777777") == "Dana Saved"
    assert wa.person_name("15551110000") == "Dana Saved"
    assert wa.person_name("888888888") == "Only Profile"
    assert wa.person_name("666666666") == "+15552220000"
    assert wa.person_name("555555555") is None
    assert wa.person_name("11111") == "Me"
    assert wa.person_name("15559990000") == "Me"


def test_mentions_are_named_and_anything_else_is_left_alone(names):
    text = "Ask @777777777, @15551110000 or @555555555. mail a@777777777 @77 x@"
    assert wa.resolve_mentions(text) == "Ask @Dana Saved, @Dana Saved or @555555555. mail a@777777777 @77 x@"
    assert wa.resolve_mentions(None) is None
    assert wa.resolve_mentions("") == ""


def test_a_sender_stored_by_hidden_id_gets_the_saved_name(names):
    assert wa.get_sender_name("777777777@lid") == "Dana Saved"
    assert wa.get_sender_name("777777777") == "Dana Saved"


def test_no_database_changes_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr(wa, "WHATSAPP_DB_PATH", str(tmp_path / "missing.db"))
    monkeypatch.setattr(wa, "_names_loaded_at", 0.0)
    monkeypatch.setattr(wa, "_names", {})
    monkeypatch.setattr(wa, "_numbers", {})
    assert wa.resolve_mentions("Hi @777777777") == "Hi @777777777"
