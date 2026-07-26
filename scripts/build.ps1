# Cross-compile the plugin to linux/amd64 from Windows using Zig as the C toolchain.
# Requires: Go >= 1.26, Zig (https://ziglang.org/download/), both on PATH or at the default paths below.
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$GoExe = $(if (Get-Command go -ErrorAction SilentlyContinue) { "go" } else { "C:\DevTools\Go\bin\go.exe" }),
    [string]$ZigExe = $(if (Get-Command zig -ErrorAction SilentlyContinue) { "zig" } else { "C:\DevTools\Zig\zig.exe" }),
    [string]$GlibcTarget = "x86_64-linux-gnu.2.36"
)

$ErrorActionPreference = "Stop"
$root = Split-Path $PSScriptRoot -Parent
$out = Join-Path $root "dist\cpa-apply-patch-v$Version.so"
New-Item -ItemType Directory -Force (Join-Path $root "dist") | Out-Null

$env:CGO_ENABLED = "1"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CC = "$ZigExe cc -target $GlibcTarget"

Push-Location $root
try {
    & $GoExe build -buildmode=c-shared -trimpath -o $out .
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    Remove-Item ($out -replace '\.so$', '.h') -ErrorAction SilentlyContinue
    "built: $out ($([math]::Round((Get-Item $out).Length / 1MB, 1)) MB)"
} finally {
    Pop-Location
}
