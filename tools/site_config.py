r"""Site settings shared by the setup scripts, from C:\mcp\site.json. That file is private: copy
site.example.json to site.json and fill it in. Prints nothing."""
from __future__ import annotations

import json
from pathlib import Path

SITE_FILE = Path(r"C:\mcp\site.json")


def load(path: Path = SITE_FILE) -> dict:
    try:
        cfg = json.loads(path.read_text(encoding="utf-8-sig"))
    except FileNotFoundError:
        raise SystemExit(f"{path} is missing. Copy site.example.json to site.json and fill it in.")
    for key in ("domain", "access_team", "tunnel", "hosts"):
        if not cfg.get(key):
            raise SystemExit(f"{path} has no {key!r}.")
    cfg["hosts"] = {"whatsapp": "a", "app": "d", **cfg["hosts"]}
    return cfg


SITE = load()
DOMAIN: str = SITE["domain"]
TEAM: str = SITE["access_team"]
TUNNEL: str = SITE["tunnel"]
HOSTS: dict[str, str] = SITE["hosts"]
