"""Builds three real installers for chain-update-test.mjs, under a throwaway name and signing key:

  A  an older release's own source (passed in), as copies in use today are: its own identifier, and the
     relay feed only. Its built-in relay address is pointed at the test relay.
  B  this source, with the test relay's address built in (as release.py --bake-relay does), and the new
     identifier, so it carries A's data and login across and saves the address.
  C  this source with nothing built in, as published for everyone, fetched from a fake GitHub feed.

    C:\\mcp\\venv\\Scripts\\python.exe e2e\\build-chain-test.py <work folder> <older app source folder>

The older source is an app folder from an earlier release (app/, without node_modules or target). It's
copied into the work folder and never changed where it is.
"""
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tools"))
from release import APP, BUNDLE, CLI, check_signature, sign  # noqa: E402

PRODUCT = "WA Relay UpdTest"
RELAY = "http://127.0.0.1:18793"
VERSIONS = {"A": "1.0.80", "B": "1.0.81", "C": "1.0.82"}
OLD_ID = "test.old.warelay.updtest"
NEW_ID = "app.warelay.updtest"


def build(src: Path, version: str, identifier: str, pubkey: str, work: Path, env_extra: dict) -> Path:
    config = {
        "productName": PRODUCT,
        "version": version,
        "mainBinaryName": "wa-relay-updtest",
        "identifier": identifier,
        "plugins": {
            "deep-link": {"desktop": {"schemes": ["warelaytest"]}},
            "updater": {"pubkey": pubkey, "dangerousInsecureTransportProtocol": True},
        },
        "bundle": {"windows": {"nsis": {"startMenuFolder": PRODUCT}}},
    }
    conf = work / f"config-{version}.json"
    conf.write_text(json.dumps(config), encoding="utf-8")
    env = dict(os.environ)
    env.pop("WA_DEFAULT_RELAY", None)
    env.update(env_extra)
    subprocess.run(["node", str(CLI), "build", "--features", "test-overrides", "--config", str(conf)], cwd=src, check=True, env=env)
    built = src / "src-tauri" / "target" / "release" / "bundle" / "nsis" / f"{PRODUCT}_{version}_x64-setup.exe"
    target = work / built.name
    shutil.copyfile(built, target)
    built.unlink()
    return target


def main():
    work = Path(sys.argv[1]).resolve()
    older = Path(sys.argv[2]).resolve()
    work.mkdir(parents=True, exist_ok=True)
    key = work / "test.key"
    if not key.exists():
        subprocess.run(["node", str(CLI), "signer", "generate", "--ci", "-p", "", "-w", str(key)], cwd=APP, check=True, capture_output=True)
    pubkey = Path(str(key) + ".pub").read_text().strip()

    # A: a copy of the older source, its relay pointed here, sharing this folder's installed packages.
    old = work / "old-src"
    if old.exists():
        modules = old / "node_modules"
        if modules.is_junction() or modules.is_symlink():
            os.rmdir(modules)  # only the link, never what it points at
        shutil.rmtree(old)
    shutil.copytree(older, old, ignore=shutil.ignore_patterns("node_modules", "target", "dist"))
    lib = old / "src-tauri" / "src" / "lib.rs"
    text, n = re.subn(r'pub const DEFAULT_RELAY: &str = "[^"]*";', f'pub const DEFAULT_RELAY: &str = "{RELAY}";', lib.read_text(encoding="utf-8"))
    if n != 1:
        raise SystemExit("The older source has no DEFAULT_RELAY to point at the test relay.")
    lib.write_text(text, encoding="utf-8")
    subprocess.run(["cmd", "/c", "mklink", "/J", str(old / "node_modules"), str(APP / "node_modules")], check=True, capture_output=True)

    built = {
        "A": build(old, VERSIONS["A"], OLD_ID, pubkey, work, {}),
        "B": build(APP, VERSIONS["B"], NEW_ID, pubkey, work, {"WA_DEFAULT_RELAY": RELAY}),
        "C": build(APP, VERSIONS["C"], NEW_ID, pubkey, work, {}),
    }
    os.rmdir(old / "node_modules")
    for name, path in built.items():
        sig = sign(path, VERSIONS[name], key)
        check_signature(path.read_bytes(), sig.read_text().strip(), pubkey, VERSIONS[name])
    # C must not carry the relay address B had built in.
    release = APP / "src-tauri" / "target" / "release"
    exes = [p for p in (release / "wa-relay-updtest.exe", release / "wa-relay-app.exe") if p.exists()]
    newest = max(exes, key=lambda p: p.stat().st_mtime)
    data = newest.read_bytes()
    if RELAY.encode() in data or RELAY.encode("utf-16-le") in data:
        raise SystemExit("C has the relay address built in.")
    print("built", *VERSIONS.values())


if __name__ == "__main__":
    main()
