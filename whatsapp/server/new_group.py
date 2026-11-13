"""Working out who someone means when creating a WhatsApp group, with no guessing.

Only a whole saved name or business name counts as exact. Profile names are chosen by the people themselves, so
one could be set to look like someone else, and a part of a name could be anyone. Those come back as choices.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from guards import Refused, person_address


@dataclass
class Choice:
    jid: str
    name: str


@dataclass
class Resolved:
    found: dict[str, Choice] = field(default_factory=dict)  # By person, so two addresses count once.
    unclear: dict[str, list[Choice]] = field(default_factory=dict)
    unknown: list[str] = field(default_factory=list)


def _plain(text: str | None) -> str:
    return " ".join((text or "").split()).casefold()


def _label(row: dict) -> str:
    return row.get("full_name") or row.get("business_name") or row.get("first_name") or row.get("push_name") or row["jid"]


class Book:
    """The bridge's contact list. Each person is listed once, under their phone number where it's known."""

    def __init__(self, contacts: list[dict], aliases: list[list[str]]):
        self.alias = {}
        for lid, phone in aliases or []:
            self.alias[lid] = phone
        self.people: dict[str, dict] = {}
        for row in contacts or []:
            jid = row.get("jid") or ""
            if not jid:
                continue
            key = self.canon(jid)
            merged = self.people.setdefault(key, {"jid": key})
            for field_name in ("full_name", "first_name", "push_name", "business_name"):
                # A saved name under either address counts. The phone number's wins, as it does in the app.
                if row.get(field_name) and (jid == key or not merged.get(field_name)):
                    merged[field_name] = row[field_name]

    def canon(self, jid: str) -> str:
        return self.alias.get(jid, jid)

    def by_address(self, jid: str) -> Choice | None:
        row = self.people.get(self.canon(jid))
        return Choice(row["jid"], _label(row)) if row else None

    def exact(self, name: str) -> list[Choice]:
        return [
            Choice(row["jid"], _label(row))
            for row in self.people.values()
            if name in (_plain(row.get("full_name")), _plain(row.get("business_name")))
        ]

    def close(self, name: str) -> list[Choice]:
        out = []
        for row in self.people.values():
            names = [_plain(row.get(f)) for f in ("full_name", "first_name", "push_name", "business_name")]
            if any(n and name in n for n in names):
                out.append(Choice(row["jid"], _label(row)))
        return out


def _address_of(member: str) -> str | None:
    text = member.strip()
    digits = text.replace("+", "").replace(" ", "").replace("-", "").replace("(", "").replace(")", "").replace(".", "")
    if "@" not in text and not digits.isdigit():
        return None
    try:
        return person_address(text)
    except Refused:
        return ""


def resolve(members: list[str], book: Book) -> Resolved:
    out = Resolved()
    for member in members:
        member = (member or "").strip()
        if not member:
            continue
        address = _address_of(member)
        if address is not None:
            person = book.by_address(address) if address else None
            if person:
                out.found[person.jid] = person
            else:
                out.unknown.append(member)
            continue
        name = _plain(member)
        exact = book.exact(name)
        if len(exact) == 1:
            out.found[exact[0].jid] = exact[0]
        elif exact:
            out.unclear[member] = exact
        else:
            close = book.close(name)
            if close:
                out.unclear[member] = close[:8]
            else:
                out.unknown.append(member)
    return out


def why_not_added(person: dict) -> str:
    """The same words the app uses."""
    if person.get("invited"):
        return "WhatsApp offered them an invitation instead."
    if person.get("code") == 403:
        return "Their privacy settings don't allow it."
    if person.get("code"):
        return f"WhatsApp refused them (code {person['code']})."
    return "They weren't in WhatsApp's reply."
