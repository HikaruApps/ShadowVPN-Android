$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = $PSScriptRoot
$mobileVersion = 'v0.0.0-20260908204917-8b95e45f8d3e'
$ndkVersion = '28.0.13004108'
if (-not $env:ANDROID_HOME) { throw 'ANDROID_HOME is not set' }
$ndkDirectory = Join-Path $env:ANDROID_HOME "ndk\$ndkVersion"
if (-not (Test-Path -LiteralPath $ndkDirectory -PathType Container)) {
    throw "Android NDK $ndkVersion was not found under ANDROID_HOME"
}
$env:ANDROID_NDK_HOME = $ndkDirectory
$temporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$temporaryDirectory = Join-Path $temporaryRoot ('shadowvpn-android-' + [guid]::NewGuid().ToString('N'))

try {
    New-Item -ItemType Directory -Path (Join-Path $temporaryDirectory 'bridge') -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $temporaryDirectory 'third_party') -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $repoRoot 'app\libs') -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repoRoot 'go\go.mod'), (Join-Path $repoRoot 'go\go.sum') -Destination $temporaryDirectory
    Copy-Item -Path (Join-Path $repoRoot 'go\bridge\*.go') -Destination (Join-Path $temporaryDirectory 'bridge')
    Copy-Item -LiteralPath (Join-Path $repoRoot 'go\third_party\utls') `
        -Destination (Join-Path $temporaryDirectory 'third_party') -Recurse

    Push-Location $temporaryDirectory
    try {
        & go get -tool "golang.org/x/mobile/cmd/gobind@$mobileVersion"
        if ($LASTEXITCODE -ne 0) { throw "go get failed with exit code $LASTEXITCODE" }
        & gomobile bind '-androidapi=29' '-ldflags=-checklinkname=0 -s -w' `
            '-target=android/arm64,android/amd64' '-javapkg=net.shadownet.shadowvpn.core' `
            -o (Join-Path $repoRoot 'app\libs\shadowvpn-core.aar') ./bridge
        if ($LASTEXITCODE -ne 0) { throw "gomobile bind failed with exit code $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
} finally {
    if (Test-Path -LiteralPath $temporaryDirectory) {
        $resolved = (Resolve-Path -LiteralPath $temporaryDirectory).Path
        if (-not $resolved.StartsWith($temporaryRoot, [StringComparison]::OrdinalIgnoreCase) `
                -or [IO.Path]::GetFileName($resolved) -notlike 'shadowvpn-android-*') {
            throw "Refusing to remove unexpected temporary directory: $resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
