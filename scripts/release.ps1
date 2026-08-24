# 크로스 컴파일 릴리스 빌드 (Windows용). release.sh와 같은 산출물을 만든다.
#
# 사용법: .\scripts\release.ps1 [-Version v1.0.0]
[CmdletBinding()]
param([string]$Version = '')

$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

if (-not $Version) {
  try { $Version = (git describe --tags --always --dirty 2>$null) } catch { }
  if (-not $Version) { $Version = 'dev' }
}
$commit = ''
try { $commit = (git rev-parse --short=12 HEAD 2>$null) } catch { }
$date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

$pkg = 'dbstudio/internal/buildinfo'
$ldflags = "-s -w -X $pkg.Version=$Version -X $pkg.Commit=$commit -X $pkg.Date=$date"

$out = 'dist'
if (Test-Path $out) { Remove-Item -Recurse -Force $out }
New-Item -ItemType Directory -Path $out | Out-Null

# release.sh 의 TARGETS 와 같은 목록이어야 한다. 한쪽만 늘리면 손으로 만든
# 빌드와 CI 빌드의 산출물이 달라진다.
$targets = @(
  @{ os = 'linux';   arch = 'amd64' },
  @{ os = 'linux';   arch = 'arm64' },
  @{ os = 'darwin';  arch = 'amd64' },
  @{ os = 'darwin';  arch = 'arm64' },
  @{ os = 'windows'; arch = 'amd64' },
  @{ os = 'windows'; arch = 'arm64' }
)

Write-Host "dbstudio $Version ($commit) 빌드"
foreach ($t in $targets) {
  $name = "dbstudio_${Version}_$($t.os)_$($t.arch)"
  $bin = Join-Path $out $name
  if ($t.os -eq 'windows') { $bin = "$bin.exe" }
  Write-Host "  -> $($t.os)/$($t.arch)"
  $env:CGO_ENABLED = '0'
  $env:GOOS = $t.os
  $env:GOARCH = $t.arch
  go build -trimpath -ldflags $ldflags -o $bin ./cmd/dbstudio
  if ($LASTEXITCODE -ne 0) { throw "build failed: $($t.os)/$($t.arch)" }
}
# 빌드 환경 변수를 남겨두면 이후 go 명령이 크로스 컴파일 모드로 동작한다.
Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH

# 체크섬은 sha256sum과 같은 형식("<hash>  <파일>")으로 적어 어느 쪽에서든 검증된다.
$lines = Get-ChildItem $out -File | ForEach-Object {
  "$((Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower())  ./$($_.Name)"
}
$lines | Set-Content (Join-Path $out 'SHA256SUMS') -Encoding utf8

Get-ChildItem $out -File | Select-Object Name, @{n='MB';e={[math]::Round($_.Length/1MB,1)}} | Format-Table -AutoSize
$lines | ForEach-Object { Write-Host $_ }
