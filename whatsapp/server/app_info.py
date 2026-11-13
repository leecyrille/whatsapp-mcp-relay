"""What the WhatsApp connector tells Claude when it connects, and where the app installer is.

Both are read fresh, so publishing an installer or editing the notes needs no restart.
"""
from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote

DIST = Path(r"C:\mcp\whatsapp-app\dist")
NOTES = Path(r"C:\mcp\whatsapp\instructions.md")
SETUP = Path(r"C:\mcp\whatsapp\app-setup.md")
SITE = Path(r"C:\mcp\site.json")
MAX_NOTES_CHARS = 4000
DEFAULT_SETUP = (
    "1. Open the download link on the new PC and sign in with a login the app's Access policy allows.\n"
    "2. Run the installer.\n"
    "3. Open the app and sign in when it asks."
)

_hashes: dict[tuple[str, int, int], str] = {}


def download_base(site: Path = SITE) -> str:
    """The app hostname's download path, from site.json (domain and hosts.app). Read fresh each time."""
    try:
        cfg = json.loads(site.read_text(encoding="utf-8-sig"))
        domain, host = cfg["domain"].strip(), cfg.get("hosts", {}).get("app", "d").strip()
    except (OSError, ValueError, KeyError, AttributeError):
        return "https://<app host>/download"
    return f"https://{host}.{domain}/download"


@dataclass(frozen=True)
class Installer:
    name: str
    version: str
    size: int
    sha256: str
    url: str


def _version_key(name: str) -> tuple[int, ...]:
    found = re.search(r"(\d+(?:\.\d+)+)", name)
    return tuple(int(part) for part in found.group(1).split(".")) if found else ()


def _sha256(path: Path, size: int, mtime: int) -> str:
    key = (str(path), size, mtime)
    if key not in _hashes:
        digest = hashlib.sha256()
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1 << 20), b""):
                digest.update(chunk)
        _hashes[key] = digest.hexdigest()
    return _hashes[key]


def latest_installer(dist: Path = DIST, site: Path = SITE) -> Installer | None:
    """The highest-versioned .exe, the same file type the relay is willing to serve."""
    try:
        files = [p for p in dist.iterdir() if p.is_file() and p.suffix.lower() == ".exe"]
    except OSError:
        return None
    if not files:
        return None
    best = max(files, key=lambda p: (_version_key(p.name), p.stat().st_mtime_ns))
    stat = best.stat()
    version = ".".join(str(part) for part in _version_key(best.name)) or "unknown"
    return Installer(
        name=best.name,
        version=version,
        size=stat.st_size,
        sha256=_sha256(best, stat.st_size, stat.st_mtime_ns),
        url=f"{download_base(site)}/{quote(best.name)}",
    )


def _read_text(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8-sig").strip()
    except (OSError, UnicodeError):
        return ""


def installer_message(dist: Path = DIST, setup: Path = SETUP) -> str:
    found = latest_installer(dist)
    if found is None:
        return "The WhatsApp app installer isn't published yet, so there's nothing to install."
    steps = _read_text(setup) or DEFAULT_SETUP
    return (
        f"Latest WhatsApp app installer: {found.name} (version {found.version}, "
        f"{found.size / (1024 * 1024):.1f} MB).\n"
        f"Download: {found.url}\n"
        f"SHA-256: {found.sha256}\n"
        "The link needs a browser sign-in with a login the app's Access policy allows.\n\n"
        f"Setup:\n{steps}"
    )


def build_instructions(safety: str, dist: Path = DIST, notes: Path = NOTES) -> str:
    """The safety text always comes first and can't be removed by the notes file."""
    parts = [safety]
    try:
        found = latest_installer(dist)
    except OSError:
        found = None
    if found is not None:
        parts.append(
            f"The current WhatsApp desktop app is version {found.version}. "
            "If the user wants it on a PC, call get_app_installer and give them the link."
        )
    extra = _read_text(notes)
    if extra:
        parts.append(extra[:MAX_NOTES_CHARS])
    return "\n\n".join(parts)
