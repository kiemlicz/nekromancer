#!/usr/bin/env bash
# Checks the published chart's version, appVersion and default image per git ref.
set -euo pipefail

OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
chart_sum() { find charts -type f -exec sha256sum {} + | sort; }
BEFORE=$(chart_sum)
BASE=$(sed -n 's/^version: //p' charts/nekromancer/Chart.yaml)

# args: ref_type ref_name sha owner want_version want_app want_image
check() {
  local tgz
  tgz=$(hack/package-chart.sh "$1" "$2" "$3" "$4" "$OUT/$2")
  [ "$(basename "$tgz")" = "nekromancer-$5.tgz" ] || fail "$2: package is $tgz"
  helm show chart "$tgz" | grep -qx "version: $5" || fail "$2: version"
  helm show chart "$tgz" | grep -qx "appVersion: $6" || fail "$2: appVersion"
  helm template t "$tgz" | grep -q "image: $7\$" || fail "$2: image, want $7"
}

check tag v1.2.3 abcdef1234567890 kiemlicz 1.2.3 1.2.3 ghcr.io/kiemlicz/nekromancer:1.2.3
check branch main abcdef1234567890 kiemlicz "$BASE-main-abcdef1" sha-abcdef1 ghcr.io/kiemlicz/nekromancer:sha-abcdef1
# All digits with a leading zero is an invalid semver identifier unless joined to "main".
check branch main 0123456789abcdef kiemlicz "$BASE-main-0123456" sha-0123456 ghcr.io/kiemlicz/nekromancer:sha-0123456
# GHCR rejects uppercase paths.
check tag v1.2.3 abcdef1234567890 KieMlicz 1.2.3 1.2.3 ghcr.io/kiemlicz/nekromancer:1.2.3

if hack/package-chart.sh tag 1.2.3 abcdef1234567890 kiemlicz "$OUT/bad" >/dev/null 2>&1; then
  fail "tag without v prefix accepted"
fi
[ "$(chart_sum)" = "$BEFORE" ] || fail "repo chart modified"
echo PASS
