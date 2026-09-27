# ShadowVPN Android

Independent Android project based on the ShadowVPN Desktop client. The Go
files in `go/bridge/` are a snapshot of the desktop subscription parser, DNS,
bootstrap and configuration logic, with a small Android bridge added. The
Android app uses `VpnService` and the same pinned Xray-core version. It does
not need the Windows repository to build.

The Android UI and `VpnService` are written in Java 17. Gradle uses Groovy
build scripts; there is no Kotlin compiler or Kotlin runtime dependency.

This is an **unverified starting point**, not a working Android release. The
APK has not been built or tested on a device yet.

## Build without Android Studio

Create a separate GitHub repository for this project and upload the files from
this directory to its root. In GitHub, open **Actions → Android build → Run
workflow**. After a successful run, open the run and download the
`ShadowVPN-Android-debug` artifact. Unzip it to get `app-debug.apk`. No Android
Studio, emulator or local Android SDK is needed for this route. A failed run
provides build logs instead of an APK; the code has not yet passed CI.

## Build

Install JDK 17, Android SDK platform 35, Go 1.27 or newer, Gradle 8.9, and
`gomobile`/`gobind` compatible with that Go version. Set `ANDROID_HOME` to the
SDK directory. From this repository root:

```sh
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
gomobile init
bash build-bridge.sh
gradle :app:assembleDebug
```

The generated debug APK will be in `app/build/outputs/apk/debug/`. The AAR is
generated during the build and ignored by Git. A GitHub Actions build workflow
is included so compilation can be checked after this project is uploaded.

## What the draft contains

- Import of one HTTPS subscription using the desktop parser, including VLESS,
  Trojan, Hysteria2 and Xray JSON profiles.
- Selection of a server, Android VPN permission prompt, foreground VPN service
  and Android TUN descriptor passed into the pinned Xray core.
- Bootstrap DNS resolution before establishing the VPN, IPv4/IPv6 default
  routes, and an explicit disconnect path.

The full desktop interface, multiple subscriptions, Auto balancing, server
groups, speed tests, split routing, Android app rules, logs and robust recovery
still need to be implemented and verified. The Windows WFP Kill Switch cannot
be reused on Android; the platform VPN lockdown setting needs separate UX and
testing.

The app excludes its own UID from the VPN so its Xray outbound sockets do not
loop into the tunnel. Android owns the IP addresses, routes and system DNS;
those fields are removed from the desktop Xray configuration before starting.

The shared source snapshot comes from
[`HikaruApps/ShadowVPN-Proxy-Client`](https://github.com/HikaruApps/ShadowVPN-Proxy-Client)
at commit `b84715fdb363ef75a602cb3baa4b8475331048eb`. License: AGPL-3.0-only;
see `LICENSE`.
