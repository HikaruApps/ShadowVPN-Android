# ShadowVPN Android

Independent Android project based on the ShadowVPN Desktop client. The Go
files in `go/bridge/` are a snapshot of the desktop subscription parser, DNS,
bootstrap and configuration logic, with a small Android bridge added. The
Android app uses `VpnService` and the same pinned Xray-core version. It does
not need the Windows repository to build.

The Android UI and `VpnService` are written in Java 17. Gradle uses Groovy
build scripts; there is no Kotlin compiler or Kotlin runtime dependency.

The project builds a standalone Android APK and currently targets Android 10+
(API 29). The native bridge is rebuilt from source during CI; generated AAR
and APK files are intentionally not committed.

## Build without Android Studio

In GitHub, open **Actions → Android build → Run workflow**. After a successful
run, download the `ShadowVPN-Android` artifact and unzip it to get
`app-release.apk`. No local Android SDK is needed for this route.

## Build

Install JDK 17, Android SDK platform 35, Android NDK 28.0.13004108, Go 1.27 or
newer, and `gomobile`/`gobind` compatible with that Go version. Gradle 8.9 is
provided by the checked-in wrapper. Set `ANDROID_HOME` to the SDK directory.
From this repository root:

```sh
go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20260908204917-8b95e45f8d3e
go install golang.org/x/mobile/cmd/gobind@v0.0.0-20260908204917-8b95e45f8d3e
gomobile init
bash build-bridge.sh
./gradlew :app:assembleDebug
```

On Windows PowerShell, replace `bash build-bridge.sh` with:

```powershell
.\build-bridge.ps1
.\gradlew.bat :app:assembleDebug
```

The generated debug APK will be in `app/build/outputs/apk/debug/`. The AAR
contains arm64 and x86_64 Android libraries, is generated during the build and
is ignored by Git. A GitHub Actions workflow builds it and runs the Go tests.
The bridge scripts pass `-checklinkname=0`, which is required by Xray's
transitive Android network-interface dependency `github.com/wlynxg/anet`.
Native libraries are compressed in the APK and extracted by Android at install
time; this keeps the download substantially smaller than the installed size.

## Features

- Separate Wi-Fi and LTE HTTPS subscription sources with subscription traffic
  metadata, plus manually pasted configs. With both subscriptions set, Auto
  uses the one that matches the current network and reconnects when the phone
  switches between Wi-Fi and mobile data.
- Share links: VLESS, VMess (v2rayN format), Trojan, Shadowsocks (SIP002 and
  legacy), Hysteria2 and NaiveProxy over HTTP/2 or QUIC/HTTP/3
  (`naive+https://user:pass@host`, `naive+quic://user:pass@host`). A broken or
  unsupported line is skipped instead of failing the whole subscription.
- Xray JSON: arrays of full client configs (the "v2ray-json" subscription
  format), single configs and bare outbounds. VLESS, VMess, Trojan,
  Shadowsocks, Hysteria, SOCKS, HTTP and WireGuard servers in flat or
  `vnext`/`servers` form; every balancer member becomes a separate server; a
  freedom dialer chain (`sockopt.dialerProxy`, the usual fragmentation setup)
  and `mux` are kept. Listeners, DNS and host-level socket options are never
  imported. Routing rules are sanitized to proxy/direct/block and applied when
  "Правила из подписки" is on.
- Servers are cached in app storage: the list appears instantly on launch, the
  app works offline, and an always-on VPN connects after a reboot.
- Routing: full tunnel, "everything except the rules" or "only the rules",
  with domains, `full:`/`keyword:`/`regexp:`, `geosite:` and `geoip:`.
  GeoIP (runetfreedom) and GeoSite (v2fly) download on first use into app
  storage, refresh weekly and fall back to the cached copy offline. Their
  sources can be changed in settings.
- All DNS of the tunnel is answered by Xray through the proxy with the chosen
  provider (subscription DoH, Cloudflare, Google, Quad9 or custom servers).
- Local networks stay outside the tunnel (optional), IPv6 is opt-in and
  blocked by Android when off, per-app split tunneling, TLS fragmentation.
- Nothing listens on localhost: the exit IP is checked through the Xray
  instance itself. Latency checks and subscription updates work while
  connected; changing settings while connected reconnects automatically.
- Connection traffic, session time, public IP and subscription quota display,
  Xray warnings in the log screen.
- Patched local uTLS source with an updated ShadowVPN ClientHello profile.

Auto uses the desktop TCP pre-check and Xray leastPing/Observatory; Naive
profiles are selected manually because Xray Observatory cannot balance an
external Cronet transport. The Windows
WFP Kill Switch cannot be reused on Android; the app opens Android's native VPN
lockdown settings instead, and the service connects with the saved settings
when Android starts it as an always-on VPN.

The app excludes its own UID from the VPN so its Xray outbound sockets do not
loop into the tunnel. Android owns the IP addresses, routes and system DNS;
those fields are removed from the desktop Xray configuration before starting.

The shared source snapshot comes from
[`HikaruApps/ShadowVPN-Proxy-Client`](https://github.com/HikaruApps/ShadowVPN-Proxy-Client)
at commit `b84715fdb363ef75a602cb3baa4b8475331048eb`. License: AGPL-3.0-only;
see `LICENSE`.
