"""Email addresses for contacts, in the connector. All data here is made up."""
import json
import sqlite3

import pytest

import contact_emails as ce
import guards
from guards import Refused

PN = "15551110000@s.whatsapp.net"
LID = "777777777@lid"


@pytest.fixture
def store(tmp_path):
    conn = sqlite3.connect(tmp_path / "whatsapp.db")
    conn.executescript(
        """
        CREATE TABLE whatsmeow_lid_map (lid TEXT, pn TEXT);
        INSERT INTO whatsmeow_lid_map VALUES ('777777777', '15551110000');
        """
    )
    conn.commit()
    conn.close()
    return tmp_path


def test_add_and_read_back_under_either_address(store):
    assert ce.add(LID, "Sam@Example.COM", store)
    assert ce.emails_for(PN, store) == ["Sam@example.com"]
    assert ce.emails_for(LID, store) == ["Sam@example.com"]


def test_several_addresses_and_no_duplicates(store):
    assert ce.add(PN, "a@example.com", store)
    assert ce.add(PN, "b@example.org", store)
    assert not ce.add(PN, "A@EXAMPLE.com", store)
    assert ce.emails_for(PN, store) == ["a@example.com", "b@example.org"]


def test_limit_per_person(store):
    for i in range(ce.MAX_PER_PERSON):
        assert ce.add(PN, f"u{i}@example.com", store)
    with pytest.raises(Refused, match="already has"):
        ce.add(PN, "more@example.com", store)


def test_remove(store):
    ce.add(PN, "a@example.com", store)
    assert not ce.remove(PN, "other@example.com", store)
    assert ce.remove(LID, "A@example.com", store)
    assert ce.emails_for(PN, store) == []
    assert json.loads((store / ce.FILE_NAME).read_text(encoding="utf-8")) == {}


@pytest.mark.parametrize("bad", [
    "", "   ", "no-at-sign", "a@b", "@example.com", "a b@example.com", "a@exa mple.com", "a@@example.com",
    "<a@example.com>", "a@example.com, b@example.com", "a@example.com;b@example.com", "a@-example.com",
    "a@example.com\nBcc: x@example.com", "а@example.com", "x" * 250 + "@example.com",
])
def test_bad_addresses_refused(store, bad):
    with pytest.raises(Refused):
        ce.add(PN, bad, store)
    assert ce.emails_for(PN, store) == []


def test_damaged_file_is_never_overwritten(store):
    path = store / ce.FILE_NAME
    path.write_text("{damaged", encoding="utf-8")
    with pytest.raises(Refused, match="can't be read"):
        ce.add(PN, "a@example.com", store)
    assert path.read_text(encoding="utf-8") == "{damaged"


def test_search(store):
    ce.add(PN, "sam@example.com", store)
    assert ce.search("EXAMPLE", store) == {PN: ["sam@example.com"]}
    assert ce.search("nothing", store) == {}
    assert ce.search("", store) == {}


def test_an_email_does_not_make_someone_a_recipient(store):
    ce.add("15559998888@s.whatsapp.net", "x@example.com", store)
    with pytest.raises(Refused):
        guards.require_known_recipient("15559998888", store)


@pytest.mark.parametrize("written", ["+1 (555) 111-0000", "1 555 111 0000", "1.555.111.0000", "+15551110000", "15551110000"])
def test_numbers_written_any_usual_way(written):
    assert guards.person_address(written) == PN


def test_tidying_does_not_loosen_send_checks():
    with pytest.raises(Refused):
        guards.normalise_recipient("+1 555 111 0000")


@pytest.mark.parametrize("bad", ["120363000000000001@g.us", "status@broadcast", "abc", "+", "12345"])
def test_still_people_only(bad):
    with pytest.raises(Refused):
        guards.person_address(bad)


def test_photo_limit_fits_a_team_in_a_day():
    assert guards.DEFAULT_LIMITS["picture_per_day"] >= 40 and guards.DEFAULT_LIMITS["picture_per_hour"] >= 40
