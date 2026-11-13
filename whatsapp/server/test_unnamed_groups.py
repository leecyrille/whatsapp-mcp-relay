"""A group with no name goes by its members' names in list_chats, get_chat and search. All data here is made up."""
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(r"C:\mcp\whatsapp\src\whatsapp-mcp-server")))
import whatsapp as wa  # noqa: E402

UNNAMED = "120363000000000077@g.us"
NO_LABEL = "120363000000000078@g.us"
NAMED = "120363000000000079@g.us"
PERSON = "15550000001@s.whatsapp.net"


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(
        f"""
        CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
        CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
            media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
            PRIMARY KEY (id, chat_jid));
        CREATE TABLE app_chat (jid TEXT PRIMARY KEY, seq INTEGER NOT NULL, last_read_ms INTEGER NOT NULL DEFAULT 0,
            label TEXT NOT NULL DEFAULT '');
        INSERT INTO chats VALUES ('{UNNAMED}', '', '2026-11-10 09:00:00+00:00');
        INSERT INTO chats VALUES ('{NO_LABEL}', NULL, '2026-11-10 08:00:00+00:00');
        INSERT INTO chats VALUES ('{NAMED}', 'Made-up Club', '2026-11-10 07:00:00+00:00');
        INSERT INTO chats VALUES ('{PERSON}', 'Sample Person', '2026-11-10 06:00:00+00:00');
        INSERT INTO app_chat VALUES ('{UNNAMED}', 1, 0, 'Made-up Ann & Made-up Bo');
        INSERT INTO app_chat VALUES ('{NAMED}', 2, 0, 'An old label');
        INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
            VALUES ('M1', '{UNNAMED}', '15550000002', 'A made-up message', '2026-11-10 09:00:00+00:00', 0);
        """
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(wa, "MESSAGES_DB_PATH", str(path))
    yield path


def test_list_chats_names_groups_with_no_name(db):
    names = {c.jid: c.name for c in wa.list_chats(limit=10)}
    assert names == {
        UNNAMED: "Made-up Ann & Made-up Bo",
        NO_LABEL: "Unnamed group",
        NAMED: "Made-up Club",
        PERSON: "Sample Person",
    }


def test_get_chat_and_search_use_it_too(db):
    assert wa.get_chat(UNNAMED).name == "Made-up Ann & Made-up Bo"
    assert [c.jid for c in wa.list_chats(query="made-up bo")] == [UNNAMED]
    assert "Made-up Ann & Made-up Bo" in str(wa.list_messages(chat_jid=UNNAMED))


def test_the_label_is_never_stored_as_the_name(db):
    wa.list_chats(limit=10)
    conn = sqlite3.connect(db)
    assert conn.execute("SELECT name FROM chats WHERE jid = ?", (UNNAMED,)).fetchone()[0] == ""
    conn.close()


def test_a_store_without_labels_still_works(db):
    conn = sqlite3.connect(db)
    conn.execute("DROP TABLE app_chat")
    conn.commit()
    conn.close()
    assert wa.get_chat(UNNAMED).name == "Unnamed group"
    assert wa.get_chat(PERSON).name == "Sample Person"
