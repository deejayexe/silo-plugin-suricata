import hashlib
import importlib.util
import json
import tempfile
import unittest
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("catalog", ROOT / "scripts/catalog.py")
catalog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(catalog)


class CatalogTests(unittest.TestCase):
    def test_installation_contract(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            for name in catalog.PLUGINS:
                source = json.loads((ROOT / "plugins" / name / "manifest.json").read_text())
                for platform in source["supported_platforms"]:
                    filename = f"{name}-{platform['os']}-{platform['arch']}"
                    (directory / filename).write_bytes(filename.encode())
            catalog.build(directory, "example/suricata", "test-1", "123.1")
            self.assertEqual((directory / "manifest.json").read_bytes(),
                             (directory / "repository.json").read_bytes())
            entries = json.loads((directory / "manifest.json").read_text())["plugins"]
            self.assertEqual({e["manifest"]["plugin_id"] for e in entries},
                             {"silo.surimatrix", "silo.gotify", "community.duplicata-versions"})
            for entry in entries:
                manifest = entry["manifest"]
                for platform, binary in entry["binaries"].items():
                    filename = binary["url"].split("/")[-1]
                    self.assertIn("/example/suricata/releases/download/test-1/", binary["url"])
                    self.assertEqual(binary["checksum"], hashlib.sha256((directory / filename).read_bytes()).hexdigest())
                    name = filename.split("-")[0]
                    osname, arch = platform.split("/")
                    with zipfile.ZipFile(directory / f"{name}_{manifest['version']}_{osname}_{arch}.zip") as z:
                        self.assertEqual(set(z.namelist()), {"manifest.json", "plugin"})
                        packaged = json.loads(z.read("manifest.json"))
                        self.assertEqual(packaged["checksum"], hashlib.sha256(z.read("plugin")).hexdigest())
                        self.assertEqual(packaged["version"], manifest["version"])
                        self.assertEqual(packaged["supported_platforms"], [dict(os=osname, arch=arch)])
                        self.assertTrue(z.getinfo("plugin").external_attr >> 16 & 0o111)
                if manifest["plugin_id"] == "community.duplicata-versions":
                    self.assertEqual(manifest["version"], "0.1.1-dev.123.1")

    def test_missing_binary_prevents_catalog_publication(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(FileNotFoundError):
                catalog.build(tmp, "example/suricata", "test-1", "123.1")
            self.assertFalse((Path(tmp) / "manifest.json").exists())

    def test_invalid_release_metadata_rejected(self):
        for repo, tag, number in [("bad", "test", "123.1"),
                                   ("example/repo", "bad/tag", "123.1"),
                                   ("example/repo", "test", "01.1")]:
            with self.assertRaises(ValueError):
                catalog.build("unused", repo, tag, number)
