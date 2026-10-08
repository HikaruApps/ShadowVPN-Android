# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Android VPN client (Java 17, Groovy Gradle, no Kotlin) wrapping a Go core built
with `gomobile`. The Go code in `go/bridge/` is a snapshot of the desktop client
[`HikaruApps/ShadowVPN-Proxy-Client`](https://github.com/HikaruApps/ShadowVPN-Proxy-Client)
(commit `b84715fd…`) plus a small Android bridge. minSdk 29, compile/target SDK 35.
License AGPL-3.0-only. User-facing strings and many comments are in Russian.

## Commands

Toolchain (pinned; keep in sync across `build-bridge.sh`, `build-bridge.ps1`,
`.github/workflows/android.yml` and README): JDK 17, Android SDK platform 35,
NDK `28.0.13004108`, Go 1.27+, gomobile/gobind `v0.0.0-20260908204917-8b95e45f8d3e`,
Gradle 8.9 (wrapper), AGP 8.7.3. `ANDROID_HOME` must be set.

```sh
bash build-bridge.sh              # gomobile bind -> app/libs/shadowvpn-core.aar (arm64 + x86_64)
./gradlew :app:assembleDebug      # APK in app/build/outputs/apk/debug/
cd go && go test ./...            # Go tests (host, no Android needed)
cd go && go test ./bridge -run TestParseNaiveHTTPSURI   # single test
```

- The AAR must exist before Gradle can compile the app; it is generated and git-ignored.
- `build-bridge.sh` copies `go.mod`, `go.sum`, `bridge/*.go` and `third_party/utls`
  into a temp dir and binds there. `-ldflags=-checklinkname=0` is required (Xray's
  transitive dep `github.com/wlynxg/anet`).
- CI (`.github/workflows/android.yml`) runs bridge build → `go test ./...` →
  `assembleDebug` on every push/PR and uploads `ShadowVPN-Android-debug`.
- Version bumps: `versionCode`/`versionName` in `app/build.gradle`.

## Architecture

Three layers, all in one process:

1. **UI — WebView SPA** (`app/src/main/assets/shadowvpn/`: `index.html`, `app.js`,
   `styles.css`, flag SVGs, `logo.png`). Not native Android views. Opening
   `index.html` in a desktop browser renders mock data (no `window.ShadowVpnAndroid`);
   query params `?disconnected`, `?error`, `?lite`, `?preview=servers|menu|welcome|settings|
   subscriptions|logs|connection|split|application|about|auto` open specific screens.
   Long lists (servers, apps, Auto servers) are paged in via `lazyList`; the
   `html.lite` class (low RAM/CPU devices) disables shadows and animations.
2. **Java host** (`app/src/main/java/net/shadownet/shadowvpn/android/`)
   - `MainActivity` hosts the WebView, exposes the `AndroidBridge` inner class to JS
     as `window.ShadowVpnAndroid` (`@JavascriptInterface` methods: `toggleVpn`,
     `selectServer`, `saveSettings`, `saveSubscriptions`, `pingServers`, …).
     State goes back to JS by `evaluateJavascript("window.ShadowVPN.render(<snapshot>)")`.
     Settings/subscriptions persist in `SharedPreferences("shadowvpn")`. Blocking
     Go calls run on background executors, results posted to the UI thread.
   - `ShadowVpnService` (`VpnService`, foreground) receives profile/MTU/IPv6/per-app
     routing via intent extras, calls `Bridge.prepare`, builds the TUN
     (`172.31.255.2/30`, optional `fd31:ffff::2/126`, default routes), then
     `Bridge.start(tunFd)`. It always excludes its own package from the VPN so
     Xray outbound sockets don't loop into the tunnel.
3. **Go core** (`go/bridge/`, Java package `net.shadownet.shadowvpn.core.bridge.Bridge`)
   - Exported gomobile API in `bridge.go`: `SetDeviceSeed`, `DeviceHWID`,
     `Configure(optionsJSON)`, `ImportSubscription(s)`, `PingProfiles`,
     `Prepare(profileID, doh)`, `Start(tunFD)`, `Stop`. Data crosses the boundary
     as JSON strings; global state lives in the mutex-guarded `mobile` struct.
   - `subscription.go` (parsing VLESS/Trojan/Hysteria2/Naive/Xray JSON), `dns.go`,
     `bootstrap.go`, `auto.go` (Auto = TCP pre-check + Xray leastPing/Observatory),
     `ping.go`, `geodata.go`, `identity.go` — mostly shared with the desktop client;
     prefer keeping them close to upstream.
   - Android owns IPs/routes/system DNS, so the desktop Xray config's TUN/DNS
     fields are stripped before start (see `TestAndroidRuntimeConfigRemovesPlatformOwnedTunSettings`).
   - Build tags: `naive_runtime_android.go` + `naive_library_android_{arm64,amd64}.go`
     embed Cronet as a direct Xray outbound (no local SOCKS); `naive_runtime_stub.go`
     (`!android`) lets host tests compile. `platform_other.go` stubs the desktop's
     Windows-only kill switch / interface pinning.
   - `go/third_party/utls` is a patched local uTLS (via `replace` in `go.mod`) with a
     custom ShadowVPN Chrome ClientHello (`u_shadowvpn_chrome152.go`).
