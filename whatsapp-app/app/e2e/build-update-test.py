"""Builds a pair of real installers for update-test.mjs, under a throwaway name, ID and signing key.

They're release builds of the real code, with the test-overrides feature so they can point at a fake
relay. The different name means they can't touch a real install on this PC, and the throwaway key means
nothing they sign would be accepted by a real copy of the app.

    C:\\mcp\\venv\\Scripts\\python.exe e2e\\build-update-test.py <work folder>
"""
import json
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tools"))
from release import APP, BUNDLE, CLI, check_signature, sign  # noqa: E402

PRODUCT = "WA Relay UpdTest"
OLD, NEW = "1.0.90", "1.0.91"


def tauri(*args, **kwargs):
    return subprocess.run(["node", str(CLI), *args], cwd=APP, check=True, **kwargs)


def main():
    work = Path(sys.argv[1])
    work.mkdir(parents=True, exist_ok=True)
    key = work / "test.key"
    other = work / "other.key"
    for path in (key, other):
        if not path.exists():
            tauri("signer", "generate", "--ci", "-p", "", "-w", str(path), capture_output=True)
    pubkey = Path(str(key) + ".pub").read_text().strip()
    for version in (OLD, NEW):
        config = {
            "productName": PRODUCT,
            "version": version,
            "mainBinaryName": "wa-relay-updtest",
            "identifier": "app.warelay.updtest",
            "plugins": {
                "deep-link": {"desktop": {"schemes": ["warelaytest"]}},
                "updater": {"pubkey": pubkey, "dangerousInsecureTransportProtocol": True},
            },
            "bundle": {"windows": {"nsis": {"startMenuFolder": PRODUCT}}},
        }
        conf = work / f"config-{version}.json"
        conf.write_text(json.dumps(config), encoding="utf-8")
        tauri("build", "--features", "test-overrides", "--config", str(conf))
        built = BUNDLE / f"{PRODUCT}_{version}_x64-setup.exe"
        target = work / built.name
        shutil.copyfile(built, target)
        built.unlink()
        sig = sign(target, version, key)
        check_signature(target.read_bytes(), sig.read_text().strip(), pubkey, version)
    # The same file signed by someone else, and signed without its version.
    new = work / f"{PRODUCT}_{NEW}_x64-setup.exe"
    good = new.with_name(new.name + ".sig")
    keep = good.read_bytes()
    sign(new, NEW, other)
    shutil.move(good, work / "new-wrong-key.sig")
    subprocess.run(["node", str(CLI), "signer", "sign", "-f", str(key), "-p", "", str(new)], cwd=APP, check=True, capture_output=True)
    shutil.move(good, work / "new-no-version.sig")
    good.write_bytes(keep)
    print("built", OLD, NEW)


if __name__ == "__main__":
    main()
