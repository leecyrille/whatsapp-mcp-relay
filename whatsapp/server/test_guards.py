import sqlite3

import pytest

import guards
from guards import Refused

LIMITS = {"per_minute": 5, "per_hour": 20, "per_day": 60, "min_gap_seconds": 8}


def test_gap_enforced(tmp_path):
    db = tmp_path / "r.db"
    guards.reserve_send(db, LIMITS, now=1000)
    with pytest.raises(Refused, match="Too soon"):
        guards.reserve_send(db, LIMITS, now=1007)
    guards.reserve_send(db, LIMITS, now=1008)


def test_per_minute_limit(tmp_path):
    db = tmp_path / "r.db"
    for i in range(5):
        guards.reserve_send(db, LIMITS, now=1000 + i * 8)
    with pytest.raises(Refused, match="5 per minute"):
        guards.reserve_send(db, LIMITS, now=1000 + 5 * 8)
    guards.reserve_send(db, LIMITS, now=1061)


def test_per_hour_limit(tmp_path):
    db = tmp_path / "r.db"
    t = 1000
    for _ in range(20):
        guards.reserve_send(db, LIMITS, now=t)
        t += 61
    with pytest.raises(Refused, match="20 per hour"):
        guards.reserve_send(db, LIMITS, now=t)
    guards.reserve_send(db, LIMITS, now=1000 + 3601)


def test_per_day_limit_and_survives_restart(tmp_path):
    db = tmp_path / "r.db"
    t = 100000
    for _ in range(60):
        guards.reserve_send(db, LIMITS, now=t)
        t += 181  # stays under the hourly limit
    with pytest.raises(Refused, match="60 per day"):
        guards.reserve_send(db, LIMITS, now=t)
    assert guards.usage(db, LIMITS, now=t)["per_day"]["used"] == 60


def test_refused_send_is_not_counted(tmp_path):
    db = tmp_path / "r.db"
    guards.reserve_send(db, LIMITS, now=1000)
    with pytest.raises(Refused):
        guards.reserve_send(db, LIMITS, now=1001)
    assert guards.usage(db, LIMITS, now=1001)["per_minute"]["used"] == 1


def test_limits_file_edit_applies_and_bad_file_keeps_defaults(tmp_path):
    f = tmp_path / "limits.json"
    f.write_text('{"per_hour": 3}')
    assert guards.load_limits(f)["per_hour"] == 3
    f.write_text("{not json")
    assert guards.load_limits(f) == guards.DEFAULT_LIMITS


@pytest.mark.parametrize("value,expected", [
    ("17805551234", "17805551234@s.whatsapp.net"),
    ("17805551234@s.whatsapp.net", "17805551234@s.whatsapp.net"),
    ("120363000000000000@g.us", "120363000000000000@g.us"),
    ("123456789012345@lid", "123456789012345@lid"),
])
def test_recipient_ok(value, expected):
    assert guards.normalise_recipient(value) == expected


@pytest.mark.parametrize("value", [
    "", "status@broadcast", "123@broadcast", "123@newsletter", "+17805551234",
    "1,2,3", "abc", "a@b.com", "123@s.whatsapp.net;456@s.whatsapp.net",
])
def test_recipient_bad(value):
    with pytest.raises(Refused):
        guards.normalise_recipient(value)


def _store(tmp_path):
    store = tmp_path / "store"
    store.mkdir()
    c = sqlite3.connect(store / "messages.db")
    c.execute("CREATE TABLE chats (jid TEXT)")
    c.execute("INSERT INTO chats VALUES ('111111@s.whatsapp.net')")
    c.commit(); c.close()
    c = sqlite3.connect(store / "whatsapp.db")
    c.execute("CREATE TABLE whatsmeow_contacts (their_jid TEXT)")
    c.execute("INSERT INTO whatsmeow_contacts VALUES ('222222@s.whatsapp.net')")
    c.commit(); c.close()
    return store


def test_known_recipients(tmp_path):
    store = _store(tmp_path)
    assert guards.require_known_recipient("111111", store) == "111111@s.whatsapp.net"
    assert guards.require_known_recipient("222222", store) == "222222@s.whatsapp.net"


def test_unknown_number_refused(tmp_path):
    with pytest.raises(Refused, match="existing chat or contact"):
        guards.require_known_recipient("333333", _store(tmp_path))


def test_missing_store_refuses(tmp_path):
    with pytest.raises(Refused):
        guards.require_known_recipient("111111", tmp_path / "nope")


def test_outbox_allows_only_files_inside(tmp_path):
    outbox = tmp_path / "outbox"
    outbox.mkdir()
    (outbox / "a.pdf").write_text("x")
    secret = tmp_path / "secret.txt"
    secret.write_text("x")
    assert guards.outbox_file("a.pdf", outbox).endswith("a.pdf")
    assert guards.outbox_file(str(outbox / "a.pdf"), outbox).endswith("a.pdf")
    for bad in (str(secret), "..\\secret.txt", "../secret.txt", str(outbox), "", "missing.pdf"):
        with pytest.raises(Refused):
            guards.outbox_file(bad, outbox)


def test_send_log_truncates_and_flattens(tmp_path):
    log = tmp_path / "s.log"
    guards.log_send("text", "1@s.whatsapp.net", "line one\nline two " + "x" * 200, True, log)
    fields = log.read_text(encoding="utf-8").rstrip("\n").split("\t")
    assert fields[1:4] == ["text", "1@s.whatsapp.net", "ok"]
    assert len(fields[4]) == 80 and "\n" not in fields[4]
