import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class PackagingTests(unittest.TestCase):
    def test_catalog_controls_are_numeric(self):
        manifest = json.loads((ROOT / "manifest.json").read_text())
        for section in manifest["global_config_schema"]:
            for field in section["admin_form"]["fields"]:
                self.assertIs(type(field["control"]), int)

    def test_scan_defaults_and_version_match(self):
        manifest = json.loads((ROOT / "manifest.json").read_text())
        self.assertEqual(manifest["version"], (ROOT / "VERSION").read_text().strip())
        analysis = next(s for s in manifest["global_config_schema"] if s["key"] == "analysis")
        schema = json.loads(analysis["json_schema"])["properties"]
        fields = {f["key"]: f for f in analysis["admin_form"]["fields"]}
        for key, maximum in (("requests_per_second", 100), ("concurrency", 16)):
            self.assertEqual(schema[key]["default"], maximum)
            self.assertEqual(schema[key]["minimum"], 1)
            self.assertEqual(schema[key]["maximum"], maximum)
            self.assertEqual(fields[key]["default_value"], maximum)
            self.assertEqual(fields[key]["validation"]["max"], maximum)

    def test_admin_routes_only(self):
        manifest = json.loads((ROOT / "manifest.json").read_text())
        routes = manifest["http_routes"]
        self.assertTrue(all(r["access"] == "admin" for r in routes))
        self.assertEqual(len(routes), len({(r["method"], r["path"]) for r in routes}))
        self.assertTrue(all(r["method"] == "POST" for r in routes if "delete" in r["path"]))

    def test_secrets_not_required_on_edit(self):
        manifest = json.loads((ROOT / "manifest.json").read_text())
        fields = manifest["global_config_schema"][0]["admin_form"]["fields"]
        key = next(f for f in fields if f["key"] == "api_key")
        self.assertTrue(key["secret"])
        self.assertFalse(key.get("required", False))

    def test_index_uses_actual_binary_hashes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "manifest.json").write_bytes((ROOT / "manifest.json").read_bytes())
            (root / "release").mkdir()
            for arch in ("amd64", "arm64"):
                (root / "release" / f"plugin-linux-{arch}").write_bytes(arch.encode())
            subprocess.run([sys.executable, str(ROOT / "scripts/repository_index.py"),
                            "--version", "0.1.0", "--repo", "example/review"], cwd=root, check=True)
            entry = json.loads((root / "repository.json").read_text())["plugins"][0]
            self.assertEqual(entry["binaries"]["linux/amd64"]["checksum"], hashlib.sha256(b"amd64").hexdigest())
            self.assertEqual(entry["manifest"]["version"], "0.1.0")
            self.assertIn("/example/review/releases/", entry["binaries"]["linux/arm64"]["url"])

    def test_bad_release_version_rejected(self):
        result = subprocess.run([sys.executable, str(ROOT / "scripts/repository_index.py"),
                                 "--version", "bad/version", "--repo", "example/review"], capture_output=True)
        self.assertNotEqual(result.returncode, 0)

    def test_no_fake_repository_index(self):
        if not (ROOT / "repository.json").exists():
            return
        index = json.loads((ROOT / "repository.json").read_text())
        for binary in index["plugins"][0]["binaries"].values():
            self.assertRegex(binary["checksum"], r"^[0-9a-f]{64}$")
            self.assertRegex(binary["url"], r"/releases/download/(?:v|duplicata-test-v)\d+\.\d+\.\d+/plugin-linux-(?:amd64|arm64)$")

if __name__ == "__main__":
    unittest.main()
