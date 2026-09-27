#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
repo_root="$(pwd)"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
cp go/go.mod go/go.sum "$out/"
mkdir -p "$out/bridge" app/libs
cp go/bridge/*.go "$out/bridge/"
(
  cd "$out"
  go get golang.org/x/mobile/bind@latest
  gomobile bind -target=android/arm64 -javapkg=net.shadownet.shadowvpn.core \
    -o "$repo_root/app/libs/shadowvpn-core.aar" ./bridge
)
