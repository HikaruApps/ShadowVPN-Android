#!/usr/bin/env bash
# Local build helper: builds the Go bridge and the debug APK.
#
# The gradle wrapper wants gradle-8.9-bin, which cannot be downloaded on every
# machine, so a cached distribution is used when the wrapper cannot fetch one.
set -euo pipefail
cd "$(dirname "$0")"
repo_root="$(pwd)"

export ANDROID_HOME="${ANDROID_HOME:-$HOME/Android/Sdk}"

# gomobile and gobind must be the same pinned golang.org/x/mobile version
# (see CLAUDE.md); mixing versions fails with "no Go package in .../bind".
tool_bin="${GOBIN:-$HOME/go/bin}"
if ! command -v gomobile >/dev/null 2>&1 || ! command -v gobind >/dev/null 2>&1; then
  echo "gomobile/gobind not found in PATH; install them first:" >&2
  echo "  go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20260908204917-8b95e45f8d3e" >&2
  echo "  go install golang.org/x/mobile/cmd/gobind@v0.0.0-20260908204917-8b95e45f8d3e" >&2
  exit 1
fi

echo "==> Building the Go bridge"
PATH="$tool_bin:$PATH" bash ./build-bridge.sh

echo "==> Building the debug APK"
gradle_cmd=()
if [[ -x ./gradlew ]]; then
  gradle_cmd=(bash ./gradlew)
else
  echo "./gradlew is missing" >&2
  exit 1
fi
"${gradle_cmd[@]}" assembleDebug --console=plain

apk="$repo_root/app/build/outputs/apk/debug/app-debug.apk"
if [[ -f "$apk" ]]; then
  echo "==> APK: $apk"
fi
