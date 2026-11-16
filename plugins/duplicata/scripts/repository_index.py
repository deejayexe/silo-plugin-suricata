"""Build the Silo community index from actual release binaries, never placeholder hashes."""
import argparse
import hashlib
import json
import re
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("--version", required=True)
parser.add_argument("--repo", required=True)
parser.add_argument("--tag", help="Release tag; defaults to vVERSION")
args = parser.parse_args()
tag = args.tag or f"v{args.version}"
if not re.fullmatch(r"[A-Za-z0-9_.-]+", tag):
    parser.error("Invalid release tag")
if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
    parser.error("Version must be MAJOR.MINOR.PATCH")
if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo):
    parser.error("Repository must be owner/name")
manifest = json.loads(Path("manifest.json").read_text())
manifest["version"] = args.version
base = f"https://github.com/{args.repo}"
manifest["presentation"].update(source_url=base, homepage_url=base, support_url=base + "/issues")
binaries = {}
checksums = []
for arch in ("amd64", "arm64"):
    name = f"plugin-linux-{arch}"
    digest = hashlib.sha256((Path("release") / name).read_bytes()).hexdigest()
    binaries[f"linux/{arch}"] = dict(url=f"{base}/releases/download/{tag}/{name}", checksum=digest)
    checksums.append(f"{digest}  {name}")
Path("release/checksums.txt").write_text("\n".join(checksums) + "\n")
Path("repository.json").write_text(json.dumps(dict(plugins=[dict(
    manifest=manifest, repo_url=base,
    checksums_url=f"{base}/releases/download/{tag}/checksums.txt", binaries=binaries
)]), ensure_ascii=False, indent=2) + "\n")
