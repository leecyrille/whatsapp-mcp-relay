"""Send guardrails: rate limits, known-recipient check, outbox-only files, send log."""
from __future__ import annotations

import base64
import binascii
import json
import re
import sqlite3
import time
from datetime import datetime
from pathlib import Path

ROOT = Path(r"C:\mcp\whatsapp")
LIMITS_FILE = ROOT / "limits.json"
RATE_DB = ROOT / "rate.db"
SEND_LOG = ROOT / "sends.log"
OUTBOX = ROOT / "outbox"
STORE = ROOT / "src" / "whatsapp-bridge" / "store"

DEFAULT_LIMITS = {"per_minute": 5, "per_hour": 20, "per_day": 60, "min_gap_seconds": 8,
                  "picture_per_hour": 60, "picture_per_day": 200,
                  "name_per_hour": 120, "name_per_day": 400}
PICTURE_MAX_BYTES = 8 * 1024 * 1024
WINDOWS = (("per_minute", 60, "minute"), ("per_hour", 3600, "hour"), ("per_day", 86400, "day"))

_JID = re.compile(r"^[0-9a-zA-Z.:_-]+@(s\.whatsapp\.net|g\.us|lid)$")
_PHONE = re.compile(r"^[0-9]{6,15}$")


class Refused(Exception):
    """A guardrail said no. The message is short enough to read aloud."""


def load_limits(path: Path = LIMITS_FILE) -> dict:
    """Read on every send so edits apply without a restart. Bad file: use defaults."""
    limits = dict(DEFAULT_LIMITS)
    try:
        limits.update(json.loads(path.read_text(encoding="utf-8")))
    except (OSError, ValueError):
        pass
    return limits


def normalise_recipient(recipient: str) -> str:
    """Turn a phone number or JID into a JID. Broadcast and channel JIDs are refused."""
    value = (recipient or "").strip()
    if _JID.match(value):
        return value
    if _PHONE.match(value):
        return f"{value}@s.whatsapp.net"
    raise Refused("Recipient must be a phone number with country code or a chat JID. Broadcasts aren't allowed.")


def is_known_recipient(jid: str, store: Path = STORE) -> bool:
    """True only if the JID is an existing chat or a saved contact."""
    checks = (
        (store / "messages.db", "SELECT 1 FROM chats WHERE jid = ? LIMIT 1"),
        (store / "whatsapp.db", "SELECT 1 FROM whatsmeow_contacts WHERE their_jid = ? LIMIT 1"),
    )
    for db, sql in checks:
        if not db.exists():
            continue
        conn = sqlite3.connect(f"{db.resolve().as_uri()}?mode=ro", uri=True, timeout=5)
        try:
            if conn.execute(sql, (jid,)).fetchone():
                return True
        except sqlite3.Error:
            pass
        finally:
            conn.close()
    return False


def require_known_recipient(recipient: str, store: Path = STORE) -> str:
    jid = normalise_recipient(recipient)
    if not is_known_recipient(jid, store):
        raise Refused("That number isn't an existing chat or contact, so I won't send to it. Use search_contacts to find the right one.")
    return jid


def outbox_file(media_path: str, outbox: Path = OUTBOX) -> str:
    """Files may only be sent from the outbox, so a caller can't send arbitrary PC files."""
    root = outbox.resolve()
    candidate = Path(media_path or "")
    if not candidate.is_absolute():
        candidate = root / candidate
    resolved = candidate.resolve()
    if not resolved.is_relative_to(root) or resolved == root:
        raise Refused(f"Only files in {root} can be sent.")
    if not resolved.is_file():
        raise Refused(f"No such file in the outbox: {resolved.name}")
    return str(resolved)


def looks_like_picture(data: bytes) -> bool:
    """A first check by content, never by file name. The bridge decodes and re-encodes it properly."""
    return (
        data.startswith(b"\xff\xd8\xff")
        or data.startswith(b"\x89PNG\r\n\x1a\n")
        or (len(data) > 12 and data[:4] == b"RIFF" and data[8:12] == b"WEBP")
    )


def picture_bytes(image_base64: str | None, outbox_file_name: str | None, outbox: Path = OUTBOX) -> bytes:
    """Reads a picture from base64 or the outbox, the only two places allowed. No other path and no URL."""
    if bool(image_base64) == bool(outbox_file_name):
        raise Refused("Give either image_base64 or outbox_file, not both.")
    if image_base64:
        text = image_base64.strip()
        if text.startswith("data:"):
            _, _, text = text.partition(",")
        if len(text) > PICTURE_MAX_BYTES * 4 // 3 + 8:
            raise Refused(f"The image must be under {PICTURE_MAX_BYTES // (1024 * 1024)} MB.")
        try:
            data = base64.b64decode(text, validate=True)
        except (binascii.Error, ValueError) as exc:
            raise Refused("image_base64 isn't valid base64.") from exc
    else:
        path = Path(outbox_file(outbox_file_name, outbox))
        if path.stat().st_size > PICTURE_MAX_BYTES:
            raise Refused(f"The image must be under {PICTURE_MAX_BYTES // (1024 * 1024)} MB.")
        data = path.read_bytes()
    if len(data) > PICTURE_MAX_BYTES:
        raise Refused(f"The image must be under {PICTURE_MAX_BYTES // (1024 * 1024)} MB.")
    if not looks_like_picture(data):
        raise Refused("Only JPEG, PNG or WebP images can be used. SVG isn't allowed.")
    return data


def _connect(db: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(db, timeout=10, isolation_level=None)
    conn.execute("CREATE TABLE IF NOT EXISTS sends (ts REAL NOT NULL)")
    conn.execute("CREATE TABLE IF NOT EXISTS picture_changes (ts REAL NOT NULL)")
    conn.execute("CREATE TABLE IF NOT EXISTS name_changes (ts REAL NOT NULL)")
    return conn


def tidy_contact(contact: str) -> str:
    """Lets a number be written the usual ways, like +1 (780) 555-0100. Addresses are left as they are."""
    value = (contact or "").strip()
    return value if "@" in value else re.sub(r"[\s()+.\-]", "", value)


def person_address(contact: str) -> str:
    """A person's address, not a group's. They don't need a chat yet: this also adds someone new."""
    jid = normalise_recipient(tidy_contact(contact))
    if jid.endswith("@g.us"):
        raise Refused("That's a group. This only names people.")
    return jid


def has_custom_name(jid: str, store: Path = STORE) -> bool:
    """True if the user named this person, under this address or their other one."""
    try:
        names = json.loads((store / "app-names.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    if not isinstance(names, dict):
        return False
    if jid in names:
        return True
    db = store / "whatsapp.db"
    if not db.exists():
        return False
    user = jid.split("@")[0]
    conn = sqlite3.connect(f"{db.resolve().as_uri()}?mode=ro", uri=True, timeout=5)
    try:
        for lid, pn in conn.execute("SELECT lid, pn FROM whatsmeow_lid_map WHERE lid = ? OR pn = ?", (user, user)):
            if f"{lid}@lid" in names or f"{pn}@s.whatsapp.net" in names:
                return True
    except sqlite3.Error:
        pass
    finally:
        conn.close()
    return False


def reserve_name_change(db: Path = RATE_DB, limits: dict | None = None, now: float | None = None) -> None:
    """Limits name changes, counted apart from sends and pictures. Enough to name a team in one go."""
    limits = limits or load_limits()
    now = time.time() if now is None else now
    conn = _connect(db)
    try:
        conn.execute("BEGIN IMMEDIATE")
        try:
            conn.execute("DELETE FROM name_changes WHERE ts < ?", (now - 86400,))
            for key, span, label in (("name_per_hour", 3600, "hour"), ("name_per_day", 86400, "day")):
                rows = conn.execute("SELECT ts FROM name_changes WHERE ts > ? ORDER BY ts", (now - span,)).fetchall()
                if len(rows) >= int(limits[key]):
                    wait = rows[0][0] + span - now
                    raise Refused(f"Name limit reached: {limits[key]} per {label}. Try again in {_short_wait(wait)}.")
            conn.execute("INSERT INTO name_changes (ts) VALUES (?)", (now,))
            conn.execute("COMMIT")
        except BaseException:
            conn.execute("ROLLBACK")
            raise
    finally:
        conn.close()


def reserve_picture_change(db: Path = RATE_DB, limits: dict | None = None, now: float | None = None) -> None:
    """Limits changes to pictures, counted apart from sends so one can't use up the other."""
    limits = limits or load_limits()
    now = time.time() if now is None else now
    conn = _connect(db)
    try:
        conn.execute("BEGIN IMMEDIATE")
        try:
            conn.execute("DELETE FROM picture_changes WHERE ts < ?", (now - 86400,))
            for key, span, label in (("picture_per_hour", 3600, "hour"), ("picture_per_day", 86400, "day")):
                rows = conn.execute("SELECT ts FROM picture_changes WHERE ts > ? ORDER BY ts", (now - span,)).fetchall()
                if len(rows) >= int(limits[key]):
                    wait = rows[0][0] + span - now
                    raise Refused(f"Picture limit reached: {limits[key]} per {label}. Try again in {_short_wait(wait)}.")
            conn.execute("INSERT INTO picture_changes (ts) VALUES (?)", (now,))
            conn.execute("COMMIT")
        except BaseException:
            conn.execute("ROLLBACK")
            raise
    finally:
        conn.close()


def _short_wait(seconds: float) -> str:
    seconds = max(1, int(seconds + 0.999))
    if seconds < 90:
        return f"{seconds} seconds"
    if seconds < 5400:
        return f"{round(seconds / 60)} minutes"
    return f"{round(seconds / 3600)} hours"


def reserve_send(db: Path = RATE_DB, limits: dict | None = None, now: float | None = None) -> None:
    """Atomically check every limit and record the attempt, or raise Refused.

    Attempts count even if the send later fails: when WhatsApp is unhappy,
    retrying harder is exactly what we want to avoid.
    """
    limits = limits or load_limits()
    now = time.time() if now is None else now
    conn = _connect(db)
    try:
        conn.execute("BEGIN IMMEDIATE")
        try:
            conn.execute("DELETE FROM sends WHERE ts < ?", (now - 86400,))
            last = conn.execute("SELECT MAX(ts) FROM sends").fetchone()[0]
            gap = float(limits["min_gap_seconds"])
            if last is not None and now - last < gap:
                raise Refused(f"Too soon after the last send. Try again in {_short_wait(gap - (now - last))}.")
            for key, span, label in WINDOWS:
                rows = conn.execute("SELECT ts FROM sends WHERE ts > ? ORDER BY ts", (now - span,)).fetchall()
                if len(rows) >= int(limits[key]):
                    wait = rows[0][0] + span - now
                    raise Refused(f"Send limit reached: {limits[key]} per {label}. Try again in {_short_wait(wait)}.")
            conn.execute("INSERT INTO sends (ts) VALUES (?)", (now,))
            conn.execute("COMMIT")
        except BaseException:
            conn.execute("ROLLBACK")
            raise
    finally:
        conn.close()


def usage(db: Path = RATE_DB, limits: dict | None = None, now: float | None = None) -> dict:
    limits = limits or load_limits()
    now = time.time() if now is None else now
    conn = _connect(db)
    try:
        out = {}
        for key, span, _ in WINDOWS:
            used = conn.execute("SELECT COUNT(*) FROM sends WHERE ts > ?", (now - span,)).fetchone()[0]
            out[key] = {"used": used, "limit": int(limits[key])}
        return out
    finally:
        conn.close()


def log_send(kind: str, jid: str, text: str, ok: bool, log: Path = SEND_LOG) -> None:
    preview = " ".join((text or "").split())[:80]
    stamp = datetime.now().astimezone().isoformat(timespec="seconds")
    with open(log, "a", encoding="utf-8") as fh:
        fh.write(f"{stamp}\t{kind}\t{jid}\t{'ok' if ok else 'FAILED'}\t{preview}\n")
