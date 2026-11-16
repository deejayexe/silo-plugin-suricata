"""Package the three plugins into one Silo catalog using actual binary hashes."""
import argparse
import copy
import hashlib
import json
import re
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PLUGINS = ("surimatrix", "surify", "duplicata")


def build(directory, repo, tag, build_number, stable=False):
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", repo):
        raise ValueError("Expected owner/repository")
    if not re.fullmatch(r"[\w.-]+", tag):
        raise ValueError("Invalid release tag")
    if not re.fullmatch(r"[1-9]\d*\.[1-9]\d*", build_number):
        raise ValueError("Expected positive run ID and attempt")
    directory = Path(directory)
    release = f"https://github.com/{repo}/releases/download/{tag}"
    entries, sums = [], []
    for name in PLUGINS:
        manifest = json.loads((ROOT / "plugins" / name / "manifest.json").read_text())
        if not re.fullmatch(r"\d+\.\d+\.\d+", manifest["version"]):
            raise ValueError("Plugin source versions must be stable semantic versions")
        if not stable:
            # Duplicata's initial candidate follows its original stable 0.1.0.
            base = "0.1.1" if name == "duplicata" and manifest["version"] == "0.1.0" else manifest["version"]
            manifest["version"] = f"{base}-dev.{build_number}"
        manifest.pop("checksum", None)
        binaries = {}
        for platform in manifest["supported_platforms"]:
            osname, arch = platform["os"], platform["arch"]
            filename = f"{name}-{osname}-{arch}"
            binary = (directory / filename).read_bytes()
            checksum = hashlib.sha256(binary).hexdigest()
            binaries[f"{osname}/{arch}"] = dict(url=f"{release}/{filename}", checksum=checksum)
            sums.append(f"{checksum}  {filename}")
            packaged = copy.deepcopy(manifest)
            packaged["supported_platforms"] = [platform]
            packaged["checksum"] = checksum
            archive = directory / f"{name}_{manifest['version']}_{osname}_{arch}.zip"
            with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as z:
                z.writestr("manifest.json", json.dumps(packaged, ensure_ascii=False, indent=2))
                info = zipfile.ZipInfo("plugin")
                info.external_attr = 0o100755 << 16
                z.writestr(info, binary, compress_type=zipfile.ZIP_DEFLATED)
            sums.append(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}")
        entries.append(dict(manifest=manifest, repo_url=f"https://github.com/{repo}",
                            checksums_url=f"{release}/checksums.txt", binaries=binaries))
    payload = json.dumps(dict(plugins=entries), ensure_ascii=False, indent=2) + "\n"
    for alias in ("manifest.json", "repository.json"):
        (directory / alias).write_text(payload)
    (directory / "checksums.txt").write_text("\n".join(sorted(sums)) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", default="dist")
    parser.add_argument("--repo", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--build-number", required=True)
    parser.add_argument("--stable", action="store_true", help="Use each plugin's source version without a development suffix")
    args = parser.parse_args()
    build(args.directory, args.repo, args.tag, args.build_number, stable=args.stable)
