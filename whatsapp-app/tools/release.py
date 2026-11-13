"""Builds, signs and publishes a WA Relay installer: to dist, which the relay serves to its own users, and as
a GitHub release, which every copy checks first.

    python tools/release.py                  # dist, plus GitHub once the repository is public
    python tools/release.py --github dry-run # dist, and the GitHub files and command, without publishing
    python tools/release.py --bake-relay --step
        # a build with this site's relay address built in, for dist only, that every older copy passes
        # through first, so each saves the address before a build without one arrives

Bump the version in tauri.conf.json, Cargo.toml and package.json first. The signing key's path comes from
WA_RELAY_SIGNING_KEY, or secrets/wa-relay-update.key next to this folder. It's never printed and never leaves
this PC, so there are no CI secrets. Nothing is published unless the signature checks out against the public
key the app has built in. Site details (relay address, GitHub repository) come from site.json next to this
folder, which stays private; site.example.json shows its shape.
"""
from __future__ import annotations

import argparse
import base64
import datetime
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

ROOT = Path(__file__).resolve().parents[1]
APP = ROOT / "app"
TAURI = APP / "src-tauri"
DIST = ROOT / "dist"
SITE = ROOT.parent / "site.json"
KEY = Path(os.environ.get("WA_RELAY_SIGNING_KEY") or ROOT.parent / "secrets" / "wa-relay-update.key")
UPDATE_RS = TAURI / "src" / "update.rs"
GITHUB_NOTES = "WA Relay {version}. Copies that are already installed update themselves. For a first install, see the README."
CLI = APP / "node_modules" / "@tauri-apps" / "cli" / "tauri.js"
BUNDLE = TAURI / "target" / "release" / "bundle" / "nsis"


def version_key(name: str) -> tuple[int, ...]:
    found = re.search(r"\d+(?:\.\d+)+", name)
    return tuple(int(part) for part in found.group(0).split(".")) if found else ()


def sign(installer: Path, version: str, key: Path = KEY) -> Path:
    """Signs with the version bound in, so an old genuine installer can't be announced as a newer one."""
    cmd = ["node", str(CLI), "signer", "sign", "--app-version", version, "-f", str(key), "-p", "", str(installer)]
    result = subprocess.run(cmd, cwd=APP, capture_output=True, text=True)
    if result.returncode != 0:
        # Only the exit code: nothing from this command is worth risking in a log.
        raise SystemExit(f"Signing failed with exit code {result.returncode}.")
    sig = installer.with_name(installer.name + ".sig")
    if not sig.is_file():
        raise SystemExit("Signing produced no .sig file.")
    return sig


def check_signature(data: bytes, sig_b64: str, pubkey_b64: str, version: str) -> None:
    """The same checks the app makes before installing. Raises ValueError if any of them fail."""
    pub_lines = base64.b64decode(pubkey_b64).decode().strip().splitlines()
    pub = base64.b64decode(pub_lines[1])
    if len(pub) != 42 or pub[:2] != b"Ed":
        raise ValueError("unexpected public key format")
    lines = base64.b64decode(sig_b64).decode().strip().splitlines()
    if len(lines) != 4 or not lines[2].startswith("trusted comment: "):
        raise ValueError("unexpected signature format")
    block = base64.b64decode(lines[1])
    if len(block) != 74 or block[2:10] != pub[2:10]:
        raise ValueError("signed by a different key")
    message = hashlib.blake2b(data).digest() if block[:2] == b"ED" else data
    key = Ed25519PublicKey.from_public_bytes(pub[10:])
    comment = lines[2][len("trusted comment: "):]
    try:
        key.verify(block[10:], message)
        key.verify(base64.b64decode(lines[3]), block[10:] + comment.encode())
    except InvalidSignature as e:
        raise ValueError("signature doesn't match") from e
    fields = dict(field.split(":", 1) for field in comment.split("\t") if ":" in field)
    if fields.get("version") != version:
        raise ValueError(f"signed for version {fields.get('version')}, not {version}")


def check_rejects_tampering(data: bytes, sig_b64: str, pubkey_b64: str, version: str) -> None:
    """Makes sure the check above can fail, so a pass means something."""
    flipped = bytearray(data)
    flipped[len(flipped) // 2] ^= 1
    for bad_data, bad_version in ((bytes(flipped), version), (data, version + "0")):
        try:
            check_signature(bad_data, sig_b64, pubkey_b64, bad_version)
        except ValueError:
            continue
        raise SystemExit("The signature check accepted a tampered update. Not publishing.")


def configured_version() -> str:
    conf = json.loads((TAURI / "tauri.conf.json").read_text(encoding="utf-8"))
    version = conf["version"]
    cargo = re.search(r'^version = "([^"]+)"', (TAURI / "Cargo.toml").read_text(encoding="utf-8"), re.M).group(1)
    package = json.loads((APP / "package.json").read_text(encoding="utf-8"))["version"]
    if not version == cargo == package:
        raise SystemExit(f"Versions disagree: tauri.conf {version}, Cargo.toml {cargo}, package.json {package}.")
    if len(version.split(".")) != 3:
        raise SystemExit("The version must have three parts.")
    return version


def publish(installer: Path, sig: Path, version: str, step: bool = False) -> Path:
    """The signature goes first: the relay won't offer an installer without one. The old build moves out
    last, so dist always has something to serve. A step release stays, with its marker, since the relay
    keeps offering it to anything older (relay/internal/relay/update.go)."""
    DIST.mkdir(exist_ok=True)
    existing = [p for p in DIST.iterdir() if p.is_file() and p.suffix.lower() == ".exe"]
    for old in existing:
        if version_key(old.name) >= version_key(version):
            raise SystemExit(f"{old.name} is already in dist. Bump the version.")
    shutil.copyfile(sig, DIST / sig.name)
    part = DIST / (installer.name + ".part")
    shutil.copyfile(installer, part)
    target = DIST / installer.name
    if step:
        (DIST / (installer.name + ".step")).write_bytes(b"")
    part.replace(target)
    archive = DIST / "old"
    archive.mkdir(exist_ok=True)
    for old in existing:
        if old.with_name(old.name + ".step").exists():
            continue
        old.replace(archive / old.name)
        old_sig = old.with_name(old.name + ".sig")
        if old_sig.exists():
            old_sig.replace(archive / old_sig.name)
    return target


def site() -> dict:
    try:
        return json.loads(SITE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def site_relay(details: dict) -> str:
    host, domain = (details.get("hosts") or {}).get("app"), details.get("domain")
    if not host or not domain:
        raise SystemExit(f"{SITE} needs domain and hosts.app to build the relay address in.")
    return f"https://{host}.{domain}"


def private_strings(details: dict) -> list[str]:
    """What must never be in a build for everyone: this site's own names. Not the tunnel's name, which is
    often an ordinary word and identifies nothing outside the Cloudflare account."""
    found = [details.get("domain"), details.get("access_team")]
    found += list(details.get("app_emails") or [])
    if details.get("domain"):
        found += [f"{h}.{details['domain']}" for h in (details.get("hosts") or {}).values()]
    return sorted({s for s in found if isinstance(s, str) and len(s) >= 4})


def check_build_is_generic(details: dict) -> None:
    """Scans the built program for this site's names, as UTF-8 and UTF-16, before it goes to everyone."""
    data = (TAURI / "target" / "release" / "wa-relay-app.exe").read_bytes().lower()
    for text in private_strings(details):
        for encoded in (text.lower().encode(), text.lower().encode("utf-16-le")):
            if encoded in data:
                raise SystemExit("The build contains one of this site's private names. Not publishing it to GitHub.")


def feed_repo() -> str | None:
    pattern = re.escape("https://github.com/") + "([^/]+/[^/]+)" + re.escape("/releases/latest/download/latest.json")
    found = re.search(pattern, UPDATE_RS.read_text(encoding="utf-8"))
    return found.group(1) if found else None


def github_files(installer: Path, sig_b64: str, version: str, repo: str) -> tuple[Path, list[Path]]:
    """The release's files: the installer under a name with no spaces (GitHub would change them), its
    signature, and latest.json in the form the updater reads."""
    out = DIST / "github" / version
    out.mkdir(parents=True, exist_ok=True)
    name = f"WA-Relay_{version}_x64-setup.exe"
    asset = out / name
    shutil.copyfile(installer, asset)
    (out / (name + ".sig")).write_text(sig_b64, encoding="utf-8")
    latest = {
        "version": version,
        "notes": GITHUB_NOTES.format(version=version),
        "pub_date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "platforms": {
            "windows-x86_64": {
                "signature": sig_b64,
                "url": f"https://github.com/{repo}/releases/download/v{version}/{name}",
            }
        },
    }
    (out / "latest.json").write_text(json.dumps(latest, indent=2), encoding="utf-8")
    return out, [asset, out / (name + ".sig"), out / "latest.json"]


def gh(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["gh", *args], capture_output=True, text=True)


def publish_github(installer: Path, sig_b64: str, version: str, mode: str, details: dict) -> None:
    repo = details.get("github_repo")
    if not repo:
        print("No github_repo in site.json, so no GitHub release.")
        return
    if feed_repo() != repo:
        raise SystemExit(f"The app checks {feed_repo()} for updates, not {repo}. Not publishing to GitHub.")
    check_build_is_generic(details)
    out, files = github_files(installer, sig_b64, version, repo)
    command = ["release", "create", f"v{version}", *map(str, files), "--repo", repo, "--title", f"WA Relay {version}",
               "--notes", GITHUB_NOTES.format(version=version), "--latest"]
    if mode == "dry-run":
        print(f"GitHub dry run: files in {out}")
        print("Would run: gh " + " ".join(f'"{a}"' if " " in a else a for a in command))
        return
    seen = gh("repo", "view", repo, "--json", "visibility", "-q", ".visibility")
    if seen.returncode != 0 or seen.stdout.strip().upper() != "PUBLIC":
        print(f"{repo} isn't public yet, so no GitHub release. Files are ready in {out}.")
        return
    made = gh(*command)
    if made.returncode != 0:
        raise SystemExit(f"gh release create failed: {made.stderr.strip()[:300]}")
    print(f"Published GitHub release v{version} to {repo}.")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--skip-build", action="store_true", help="use the installer already built")
    parser.add_argument("--github", choices=["publish", "dry-run", "skip"], default="publish",
                        help="publish (once the repository is public), dry-run, or skip")
    parser.add_argument("--bake-relay", action="store_true", help="build this site's relay address in; dist only, never GitHub")
    parser.add_argument("--step", action="store_true", help="older copies pass through this release first")
    args = parser.parse_args()
    version = configured_version()
    details = site()
    env = dict(os.environ)
    env.pop("WA_DEFAULT_RELAY", None)
    if args.bake_relay:
        if args.skip_build:
            raise SystemExit("--bake-relay needs a fresh build.")
        env["WA_DEFAULT_RELAY"] = site_relay(details)
    if not args.skip_build:
        subprocess.run(["node", str(CLI), "build"], cwd=APP, check=True, env=env)
    installer = BUNDLE / f"WA Relay_{version}_x64-setup.exe"
    if not installer.is_file():
        raise SystemExit(f"{installer} wasn't built.")
    sig = sign(installer, version)
    data = installer.read_bytes()
    sig_b64 = sig.read_text(encoding="utf-8").strip()
    pubkey = json.loads((TAURI / "tauri.conf.json").read_text(encoding="utf-8"))["plugins"]["updater"]["pubkey"]
    try:
        check_signature(data, sig_b64, pubkey, version)
    except ValueError as e:
        raise SystemExit(f"Not publishing: {e}.") from e
    check_rejects_tampering(data, sig_b64, pubkey, version)
    target = publish(installer, sig, version, step=args.step)
    print(f"Published {target.name}, {target.stat().st_size} bytes{' as a step release' if args.step else ''}")
    print(f"SHA-256 {hashlib.sha256(target.read_bytes()).hexdigest().upper()}")
    print("Signature verified against the app's public key, and a tampered copy was refused.")
    if args.bake_relay:
        print("Built with this site's relay address, so it stays out of GitHub.")
    elif args.github != "skip":
        publish_github(installer, sig_b64, version, args.github, details)
    return 0


if __name__ == "__main__":
    sys.exit(main())
