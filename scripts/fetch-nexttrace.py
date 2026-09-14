"""Fetch pinned, unmodified NextTrace binaries and matching license/source.

Run from any directory: python scripts/fetch-nexttrace.py [linux-amd64 ...]
Without arguments, fetch all platforms listed in the project manifest.
"""
import hashlib
import json
import pathlib
import shutil
import sys
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1] / "tools" / "nexttrace"


def download(url, path, digest=None):
    if path.exists() and digest and hashlib.sha256(path.read_bytes()).hexdigest() == digest:
        print(f"Already verified: {path.name}")
        return
    temp = path.with_name(path.name + ".download")
    req = urllib.request.Request(url, headers={"User-Agent": "DnsLatencyRouter"})
    with urllib.request.urlopen(req, timeout=120) as source, temp.open("wb") as out:
        shutil.copyfileobj(source, out)
    if digest and hashlib.sha256(temp.read_bytes()).hexdigest() != digest:
        temp.unlink()
        raise RuntimeError(f"SHA-256 mismatch: {path.name}")
    temp.replace(path)
    if not path.name.endswith(".exe"):
        path.chmod(0o755)
    print(f"Downloaded: {path.name}")


def main():
    manifest = json.loads((ROOT / "manifest.json").read_text(encoding="utf-8"))
    requested = set(sys.argv[1:])
    assets = manifest["assets"]
    platforms = {a["name"].removeprefix("nexttrace_").removesuffix(".exe").replace("_", "-") for a in assets}
    if requested - platforms:
        raise SystemExit("Unsupported platform: " + ", ".join(sorted(requested - platforms)))
    for asset in assets:
        platform = asset["name"].removeprefix("nexttrace_").removesuffix(".exe").replace("_", "-")
        if not requested or platform in requested:
            download(asset["browser_download_url"], ROOT / asset["name"], asset["digest"].removeprefix("sha256:"))
    version = manifest["version"]
    download(f"https://raw.githubusercontent.com/nxtrace/NTrace-core/{version}/LICENSE", ROOT / "LICENSE")
    download(f"https://api.github.com/repos/nxtrace/NTrace-core/tarball/{version}", ROOT / f"source-{version}.tar.gz")


if __name__ == "__main__":
    main()
