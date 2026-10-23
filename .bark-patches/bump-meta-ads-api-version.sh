#!/usr/bin/env bash
# Bark interim patch: retarget the meta-ads engine to a newer Graph/Marketing
# API version. Meta silently auto-upgrades retired versions (a v19.0 call is
# served as v21.0), so the engine runs untested behaviour until it requests a
# supported version explicitly. Reproducible: run from the repo root.
#
#   .bark-patches/bump-meta-ads-api-version.sh v19.0 v26.0
#
# .manuscripts/ is the historical generation record and is left untouched.
set -euo pipefail

from="${1:?usage: $0 <from-version> <to-version>}"
to="${2:?usage: $0 <from-version> <to-version>}"
lib=library/marketing/meta-ads

files=(
  "$lib/internal/config/config.go"
  "$lib/internal/client/client.go"
  "$lib/spec.yaml"
  "$lib/tools-manifest.json"
  "$lib/.printing-press.json"
)

for f in "${files[@]}"; do
  sed -i "s/${from//./\\.}/${to}/g" "$f"
done

# spec_checksum is the sha256 of spec.yaml; keep it in sync.
sum=$(sha256sum "$lib/spec.yaml" | cut -d' ' -f1)
sed -i -E "s/\"spec_checksum\": \"sha256:[0-9a-f]+\"/\"spec_checksum\": \"sha256:${sum}\"/" "$lib/.printing-press.json"

left=$(grep -rn --exclude-dir=.manuscripts "${from//./\\.}" "$lib" || true)
if [[ -n "$left" ]]; then
  echo "unexpected ${from} references left:" >&2
  echo "$left" >&2
  exit 1
fi
