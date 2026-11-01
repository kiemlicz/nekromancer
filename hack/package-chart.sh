#!/usr/bin/env bash
# Usage: package-chart.sh <tag|branch> <ref name> <sha> <registry owner> <out dir>
# Prints the path of the packaged chart. Works on a copy, the repo is left untouched.
set -euo pipefail

ref_type=$1 ref_name=$2 sha=$3 owner=${4,,} out=$5
short=${sha:0:7}

case $ref_type in
  tag)
    [[ $ref_name =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "tag $ref_name is not vX.Y.Z" >&2; exit 1; }
    version=${ref_name#v}
    app_version=$version
    ;;
  branch)
    base=$(sed -n 's/^version: //p' charts/nekromancer/Chart.yaml)
    version=$base-$ref_name-$short
    # Matches the image tag from metadata-action type=sha.
    app_version=sha-$short
    ;;
  *)
    echo "unknown ref type $ref_type" >&2
    exit 1
    ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -r charts/nekromancer "$work/"
sed -i "s|^  repository: \"nekromancer\"$|  repository: \"ghcr.io/$owner/nekromancer\"|" "$work/nekromancer/values.yaml"
grep -q "repository: \"ghcr.io/$owner/nekromancer\"" "$work/nekromancer/values.yaml" || { echo "image repository not set" >&2; exit 1; }

mkdir -p "$out"
helm package "$work/nekromancer" --version "$version" --app-version "$app_version" --destination "$out" >/dev/null
echo "$out/nekromancer-$version.tgz"
