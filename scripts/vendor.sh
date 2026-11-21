#!/bin/sh
# Fetch the pinned frontend libraries into web/static/vendor so they can be
# embedded in the binary. Nothing is loaded from a CDN at runtime.
set -eu

CESIUM_VERSION=1.147.0
DATASTAR_VERSION=1.0.5
MP4_MUXER_VERSION=5.2.2

root=$(cd "$(dirname "$0")/.." && pwd)
dest="$root/web/static/vendor"
stamp="$dest/.versions"
want="cesium=$CESIUM_VERSION datastar=$DATASTAR_VERSION mp4-muxer=$MP4_MUXER_VERSION"

if [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$want" ]; then
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
rm -rf "$dest"
mkdir -p "$dest/cesium"

echo "fetching CesiumJS $CESIUM_VERSION"
curl -fsSL "https://registry.npmjs.org/cesium/-/cesium-$CESIUM_VERSION.tgz" -o "$tmp/cesium.tgz"
mkdir "$tmp/cesium"
tar -xzf "$tmp/cesium.tgz" -C "$tmp/cesium" package/Build/Cesium package/LICENSE.md
cp -R "$tmp/cesium/package/Build/Cesium/." "$dest/cesium/"
cp "$tmp/cesium/package/LICENSE.md" "$dest/cesium/LICENSE.md"
# The ESM build and source maps are not used.
rm -f "$dest/cesium/index.js" "$dest/cesium/index.cjs" "$dest/cesium/index.js.map"
find "$dest/cesium" -name '*.map' -delete

echo "fetching Datastar $DATASTAR_VERSION"
curl -fsSL "https://cdn.jsdelivr.net/gh/starfederation/datastar@v$DATASTAR_VERSION/bundles/datastar.js" -o "$dest/datastar.js"

echo "fetching mp4-muxer $MP4_MUXER_VERSION"
curl -fsSL "https://registry.npmjs.org/mp4-muxer/-/mp4-muxer-$MP4_MUXER_VERSION.tgz" -o "$tmp/mp4.tgz"
mkdir "$tmp/mp4"
tar -xzf "$tmp/mp4.tgz" -C "$tmp/mp4"
cp "$tmp/mp4/package/build/mp4-muxer.mjs" "$dest/mp4-muxer.mjs"
cp "$tmp/mp4/package/LICENSE" "$dest/mp4-muxer.LICENSE"

echo "$want" > "$stamp"
