#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
repo_root="$(pwd)"
out="$(mktemp -d)"
mobile_version="v0.0.0-20260908204917-8b95e45f8d3e"
ndk_version="28.0.13004108"
if [[ -z "${ANDROID_HOME:-}" || ! -d "$ANDROID_HOME/ndk/$ndk_version" ]]; then
  echo "Android NDK $ndk_version was not found under ANDROID_HOME" >&2
  exit 1
fi
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$ndk_version"
trap 'rm -rf "$out"' EXIT
cp go/go.mod go/go.sum "$out/"
mkdir -p "$out/bridge" "$out/third_party" app/libs
cp go/bridge/*.go "$out/bridge/"
cp -R go/third_party/utls "$out/third_party/"
(
  cd "$out"
  go get -tool "golang.org/x/mobile/cmd/gobind@$mobile_version"
  gomobile bind -androidapi=29 -ldflags="-checklinkname=0 -s -w" \
    -target=android/arm64,android/amd64 -javapkg=net.shadownet.shadowvpn.core \
    -o "$repo_root/app/libs/shadowvpn-core.aar" ./bridge
)
