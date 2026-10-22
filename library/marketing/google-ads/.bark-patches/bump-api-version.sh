#!/usr/bin/env bash
# Bark interim patch (PENG-5568): retarget the generated google-ads engine at a
# newer Google Ads API version by rewriting every whole-word API version token.
#
# Upstream generates this module from Google's discovery doc (spec.json) with
# CLI Printing Press; the real fix is regenerating from the new version's doc.
# Until upstream does that, this script is the reviewable source of truth for
# the rewrite: re-run it on the parent commit and the result must match the
# commit that follows it byte-for-byte.
#
# Usage (from repo root): library/marketing/google-ads/.bark-patches/bump-api-version.sh 22 26
set -euo pipefail
from="${1:?from version, e.g. 22}"; to="${2:?to version, e.g. 26}"
dir="library/marketing/google-ads"
# Every v<from>/V<from> token in this module is the API version: URL paths
# (/v22/..., /v22:method), schema names (GoogleAdsGoogleadsV22...), spec and
# printing-press metadata ("version"/"api_version"), and the User-Agent.
# CHANGELOG.md and this patch directory are history, not code — left alone.
grep -rlE "\b[vV]${from}\b|GoogleadsV${from}" "$dir" \
  --exclude=CHANGELOG.md --exclude-dir=.bark-patches \
  | xargs sed -i -E \
      -e "s#\bv${from}\b#v${to}#g" \
      -e "s#GoogleadsV${from}#GoogleadsV${to}#g"
left=$(grep -rnE "\b[vV]${from}\b|GoogleadsV${from}" "$dir" --exclude=CHANGELOG.md --exclude-dir=.bark-patches || true)
if [ -n "$left" ]; then echo "unrewritten v${from} refs remain:" >&2; echo "$left" >&2; exit 1; fi
echo "rewrote v${from} -> v${to} in $dir"
