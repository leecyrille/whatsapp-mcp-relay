"""Email addresses the user keeps for their WhatsApp contacts. Connector only: nothing here reaches WhatsApp, their phone
or the app, and nothing in the connector sends email to them. Kept per person, under their phone number when it's
known, so either of their WhatsApp addresses finds them."""
from __future__ import annotations

import json
import os
import re
import sqlite3
import tempfile
import threading
from pathlib import Path

import guards
from guards import Refused

MAX_PER_PERSON = 5
MAX_LENGTH = 254
FILE_NAME = "app-contact-emails.json"

_EMAIL = re.compile(r"^[^\s@<>\"',;:()\[\]\\]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+$")
_lock = threading.Lock()


def clean_email(raw: str) -> str:
    """A tidy address, or Refused. Only checks its shape: it doesn't mean the address exists."""
    email = (raw or "").strip()
    if len(email) > MAX_LENGTH or not email.isascii() or not _EMAIL.match(email):
        raise Refused("That doesn't look like an email address.")
    user, _, domain = email.rpartition("@")
    return f"{user}@{domain.lower()}"


def _path(store: Path) -> Path:
    return store / FILE_NAME


def _load(store: Path) -> dict[str, list[str]]:
    try:
        data = json.loads(_path(store).read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}
    except (OSError, ValueError) as exc:
        # Stop rather than treat it as empty, or the next save would overwrite everything.
        raise Refused("The contact email file can't be read, so nothing was changed.") from exc
    if not isinstance(data, dict):
        raise Refused("The contact email file can't be read, so nothing was changed.")
    return {k: [e for e in v if isinstance(e, str)] for k, v in data.items() if isinstance(v, list)}


def _save(data: dict[str, list[str]], store: Path) -> None:
    fd, tmp = tempfile.mkstemp(dir=store, prefix="app-contact-emails-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=1, ensure_ascii=False)
        os.replace(tmp, _path(store))
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise


def canonical(jid: str, store: Path) -> str:
    """One address per person: the phone number when a hidden ID has a known one."""
    if not jid.endswith("@lid"):
        return jid
    db = store / "whatsapp.db"
    if not db.exists():
        return jid
    conn = sqlite3.connect(f"{db.resolve().as_uri()}?mode=ro", uri=True, timeout=5)
    try:
        row = conn.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? LIMIT 1", (jid.split("@")[0],)).fetchone()
    except sqlite3.Error:
        row = None
    finally:
        conn.close()
    return f"{row[0]}@s.whatsapp.net" if row else jid


def add(jid: str, email: str, store: Path = guards.STORE) -> bool:
    """Adds an address. False means they already had it. Case doesn't make a different address."""
    email = clean_email(email)
    key = canonical(jid, store)
    with _lock:
        data = _load(store)
        current = data.get(key, [])
        if any(e.lower() == email.lower() for e in current):
            return False
        if len(current) >= MAX_PER_PERSON:
            raise Refused(f"That person already has {MAX_PER_PERSON} email addresses. Remove one first.")
        data[key] = [*current, email]
        _save(data, store)
    return True


def remove(jid: str, email: str, store: Path = guards.STORE) -> bool:
    """Removes an address. False means they didn't have it."""
    target = (email or "").strip().lower()
    key = canonical(jid, store)
    with _lock:
        data = _load(store)
        current = data.get(key, [])
        kept = [e for e in current if e.lower() != target]
        if len(kept) == len(current):
            return False
        if kept:
            data[key] = kept
        else:
            del data[key]
        _save(data, store)
    return True


def emails_for(jid: str, store: Path = guards.STORE) -> list[str]:
    return list(_load(store).get(canonical(jid, store), []))


def search(query: str, store: Path = guards.STORE) -> dict[str, list[str]]:
    """People with an address containing the query."""
    needle = (query or "").strip().lower()
    if not needle:
        return {}
    return {jid: found for jid, found in _load(store).items() if any(needle in e.lower() for e in found)}
