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
run, download the `ShadowVPN-Android-debug` artifact and unzip it to get
`app-debug.apk`. No local Android SDK is needed for this route.

## Build

Install JDK 17, Android SDK platform 35, Android NDK 27.2.12479018, Go 1.27 or
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

## Features

- Separate Wi-Fi and LTE HTTPS subscription sources with subscription traffic
  metadata.
- Desktop-compatible subscription parsing, including VLESS, Trojan, Hysteria2
  and Xray JSON profiles.
- Selection of a server or Auto/Auto without Russia, Android VPN permission
  prompt, foreground VPN service and Android TUN descriptor passed into the
  pinned Xray core.
- Bootstrap DNS resolution before establishing the VPN, IPv4/IPv6 default
  routes, and an explicit disconnect path.
- Mobile server drawer with filtering, sorting, latency checks and country
  flags.
- Connection traffic, session time, public IP and subscription quota display.
- Per-app split tunneling, connection settings, logs and Android battery/VPN
  settings shortcuts.
- Patched local uTLS source with an updated ShadowVPN ClientHello profile.

Auto uses the desktop TCP pre-check and Xray leastPing/Observatory. The Windows
WFP Kill Switch cannot be reused on Android; the app opens Android's native VPN
lockdown settings instead.

The app excludes its own UID from the VPN so its Xray outbound sockets do not
loop into the tunnel. Android owns the IP addresses, routes and system DNS;
those fields are removed from the desktop Xray configuration before starting.

The shared source snapshot comes from
[`HikaruApps/ShadowVPN-Proxy-Client`](https://github.com/HikaruApps/ShadowVPN-Proxy-Client)
at commit `b84715fdb363ef75a602cb3baa4b8475331048eb`. License: AGPL-3.0-only;
see `LICENSE`.
