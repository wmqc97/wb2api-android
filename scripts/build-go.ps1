# 把 server/ 里的 Go 网关交叉编译成 Android 原生可执行文件，直接产出到
# app/src/main/jniLibs/<abi>/libwb2api.so。
#
# 两个必须这么做的理由：
#   1. 必须开 cgo。GOOS=android 时 Go 工具链强制要求外部链接器
#      （CGO_ENABLED=0 直接报 "requires external (cgo) linking"）；而且 Android
#      没有 /etc/resolv.conf，纯 Go 解析器拿不到 DNS，只有 cgo 才走系统 netd。
#   2. 必须叫 .so 并放进 jniLibs。Android 10 起禁止应用执行自己数据目录里落盘的
#      文件，唯一允许 exec 的位置是安装时由系统解压出来的 nativeLibraryDir。
#
# 用法：
#   pwsh -File scripts/build-go.ps1                 # 两个 ABI 都编
#   pwsh -File scripts/build-go.ps1 -Abi arm64      # 只编 arm64-v8a

[CmdletBinding()]
param(
  [ValidateSet('all', 'arm64', 'x86_64')]
  [string]$Abi = 'all',

  # 注入 main.appVersion；留空则用源码里的默认值。
  [string]$Version = 'v1.9.41'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$serverDir = Join-Path $root 'server'
$jniRoot = Join-Path $root 'app\src\main\jniLibs'

# ── Go 工具链 ────────────────────────────────────────────────────────────────
$go = Join-Path $env:TEMP 'gotool\go\bin\go.exe'
if (-not (Test-Path $go)) {
  $cmd = Get-Command go -ErrorAction SilentlyContinue
  if ($cmd) { $go = $cmd.Source }
}
if (-not $go) { throw '找不到 go.exe：请安装 Go 1.22+，或把工具链解压到 %TEMP%\gotool\go' }

$env:GOROOT = Split-Path -Parent (Split-Path -Parent $go)
if (-not $env:GOPATH) { $env:GOPATH = Join-Path $env:TEMP 'gopath' }
if (-not $env:GOCACHE) { $env:GOCACHE = Join-Path $env:TEMP 'gocache' }

# ── NDK ─────────────────────────────────────────────────────────────────────
$sdk = $env:ANDROID_HOME
if (-not $sdk) { $sdk = $env:ANDROID_SDK_ROOT }
if (-not $sdk) { $sdk = 'F:\Android\Sdk' }

$ndkParent = Join-Path $sdk 'ndk'
if (-not (Test-Path $ndkParent)) { throw "找不到 NDK 目录：$ndkParent（可用 ANDROID_HOME 指定 SDK）" }
$ndk = Get-ChildItem $ndkParent -Directory | Sort-Object Name -Descending | Select-Object -First 1
$toolchain = Join-Path $ndk.FullName 'toolchains\llvm\prebuilt\windows-x86_64\bin'
if (-not (Test-Path $toolchain)) { throw "NDK 工具链不完整：$toolchain" }

Write-Host "Go    : $go"
Write-Host "NDK   : $($ndk.FullName)"
Write-Host "输出  : $jniRoot"
Write-Host ''

$targets = [ordered]@{
  arm64  = @{ goarch = 'arm64'; abiDir = 'arm64-v8a'; cc = 'aarch64-linux-android21-clang.cmd' }
  x86_64 = @{ goarch = 'amd64'; abiDir = 'x86_64';      cc = 'x86_64-linux-android21-clang.cmd' }
}

foreach ($name in $targets.Keys) {
  if ($Abi -ne 'all' -and $Abi -ne $name) { continue }
  $t = $targets[$name]

  $env:GOOS = 'android'
  $env:GOARCH = $t.goarch
  $env:CGO_ENABLED = '1'
  $env:CC = Join-Path $toolchain $t.cc
  $env:CXX = $env:CC
  $env:AR = Join-Path $toolchain 'llvm-ar.exe'

  $outDir = Join-Path $jniRoot $t.abiDir
  New-Item -ItemType Directory -Force -Path $outDir | Out-Null
  $out = Join-Path $outDir 'libwb2api.so'

  $ldflags = '-s -w'
  if ($Version) { $ldflags = "$ldflags -X main.appVersion=$Version" }

  Write-Host "==> $name ($($t.goarch)) -> $out"
  Push-Location $serverDir
  try {
    & $go build -trimpath -buildvcs=false -ldflags $ldflags -o $out ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build 失败（$name），exit=$LASTEXITCODE" }
  } finally {
    Pop-Location
  }
  $size = [math]::Round((Get-Item $out).Length / 1MB, 2)
  Write-Host "    ok, $size MB`n"
}

Write-Host '完成。'
