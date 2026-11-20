#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
arch="$(go env GOARCH)"
mkdir -p dist
build_version="${BUILD_VERSION:-0.0.2}"
CGO_ENABLED=1 go build -tags goolm -trimpath -ldflags="-s -w -X main.version=$build_version" -o dist/plugin .
cp dist/plugin "dist/plugin-linux-$arch"
python3 - "$arch" "$build_version" <<'PY'
import hashlib, json, pathlib, sys, zipfile
root = pathlib.Path('dist')
binary = (root/'plugin').read_bytes()
manifest = json.loads(pathlib.Path('manifest.json').read_text())
manifest['version'] = sys.argv[2]
manifest['checksum'] = hashlib.sha256(binary).hexdigest()
name = f"surimatrix_{manifest['version']}_linux_{sys.argv[1]}.zip"
with zipfile.ZipFile(root/name, 'w', zipfile.ZIP_DEFLATED) as z:
    z.writestr('manifest.json', json.dumps(manifest, ensure_ascii=False, indent=2))
    info = zipfile.ZipInfo('plugin'); info.external_attr = 0o100755 << 16
    z.writestr(info, binary, compress_type=zipfile.ZIP_DEFLATED)
(root/(name+'.sha256')).write_text(hashlib.sha256((root/name).read_bytes()).hexdigest()+'  '+name+'\n')
print(root/name)
PY
