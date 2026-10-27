#!/bin/sh
# Copy the release files into the two folder layouts GitHub serves them from
# (latest, and the tag), so install.sh can be tested against a local web
# server. It prints the folder to serve.
set -eu
if [ "$#" -ne 3 ]; then
    echo 'Usage: scripts/fake-release.sh <dist> <root> <tag>' >&2
    exit 1
fi
dist=$1
root=$2
tag=$3
case "$tag" in v[0-9]*.[0-9]*.[0-9]*) ;;
    *) echo 'The tag must look like v0.1.0.' >&2; exit 1 ;;
esac
case "$tag" in */*) echo 'The tag must not contain a slash.' >&2; exit 1 ;; esac
# Check every file before copying anything.
assets='maildo-linux-amd64 maildo-linux-arm64 maildo-darwin-amd64 maildo-darwin-arm64 checksums.txt'
for asset in $assets; do
    [ -f "$dist/$asset" ] || { echo "$dist has no $asset. Run make build-all first." >&2; exit 1; }
done
for layout in "$root/releases/latest/download" "$root/releases/download/$tag"; do
    mkdir -p "$layout"
    for asset in $assets; do cp "$dist/$asset" "$layout/$asset"; done
done
(cd "$root" && pwd -P)
