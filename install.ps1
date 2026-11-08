<#
.SYNOPSIS
  Install term-llm from a GitHub release on Windows.

.DESCRIPTION
  Downloads the Windows amd64 release ZIP and checksums.txt, verifies the
  SHA-256 checksum, and installs term-llm.exe. Checksums provide integrity,
  not publisher authentication: both files come from the same release.

.PARAMETER Version
  Release tag to install (for example 0.10.0 or v0.10.0). Defaults to latest.

.PARAMETER InstallDir
  Destination directory. Defaults to %LOCALAPPDATA%\term-llm\bin.

.PARAMETER ArchivePath
  Install from a local ZIP instead of downloading (checksums are still required).

.PARAMETER ChecksumsPath
  Local checksums.txt to use with -ArchivePath.

.EXAMPLE
  irm https://raw.githubusercontent.com/samsaffron/term-llm/main/install.ps1 | iex

.EXAMPLE
  .\install.ps1 -InstallDir C:\tools\term-llm
#>
[CmdletBinding()]
param(
  [string]$Version = "",
  [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "term-llm\bin"),
  [string]$ArchivePath = "",
  [string]$ChecksumsPath = "",
  [string]$BaseUrl = "https://github.com/samsaffron/term-llm/releases"
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$Repo = "samsaffron/term-llm"
$AssetArch = "amd64"

function Fail([string]$Message) {
  throw "Error: $Message"
}

[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

if ($ArchivePath -and -not $ChecksumsPath) {
  Fail "-ArchivePath requires -ChecksumsPath"
}

if (-not $Version) {
  if ($ArchivePath) {
    Fail "-Version is required with -ArchivePath"
  }
  $latest = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases/latest"
  $Version = $latest.tag_name
}
if ($Version -notmatch '^v?[0-9][A-Za-z0-9._+-]*$') {
  Fail "Invalid release version: $Version"
}
if (-not $Version.StartsWith("v")) {
  $Version = "v$Version"
}
$assetVersion = $Version.Substring(1)
$asset = "term-llm_${assetVersion}_windows_${AssetArch}.zip"

$temp = Join-Path ([IO.Path]::GetTempPath()) ("term-llm-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $temp | Out-Null
try {
  $zipPath = Join-Path $temp $asset
  $checksumsFile = Join-Path $temp "checksums.txt"
  if ($ArchivePath) {
    Copy-Item -LiteralPath $ArchivePath -Destination $zipPath
    Copy-Item -LiteralPath $ChecksumsPath -Destination $checksumsFile
  } else {
    $releaseUrl = "$BaseUrl/download/$Version"
    Write-Host "==> Downloading $asset"
    Invoke-WebRequest -UseBasicParsing -Uri "$releaseUrl/$asset" -OutFile $zipPath
    Invoke-WebRequest -UseBasicParsing -Uri "$releaseUrl/checksums.txt" -OutFile $checksumsFile
  }

  $checksumMatches = @()
  foreach ($line in Get-Content -LiteralPath $checksumsFile) {
    $line = $line.TrimEnd()
    if ($line -match '^([0-9a-fA-F]{64}) [ *](.+)$' -and $Matches[2] -eq $asset) {
      $checksumMatches += $Matches[1].ToLowerInvariant()
    }
  }
  if ($checksumMatches.Count -ne 1) {
    Fail "Missing, malformed, or duplicate checksum for $asset"
  }
  $actual = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actual -ne $checksumMatches[0]) {
    Fail "SHA256 checksum mismatch for $asset"
  }

  Write-Host "==> Extracting term-llm.exe"
  $extract = Join-Path $temp "extract"
  Expand-Archive -LiteralPath $zipPath -DestinationPath $extract -Force
  $binary = Join-Path $extract "term-llm.exe"
  if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    Fail "term-llm.exe not found at the archive root"
  }

  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  $target = Join-Path $InstallDir "term-llm.exe"
  Copy-Item -LiteralPath $binary -Destination $target -Force

  Write-Host "==> Installed $target"
  $pathEntries = ($env:PATH -split ';') | ForEach-Object { $_.TrimEnd('\') }
  if (-not ($pathEntries | Where-Object { $_ -ieq $InstallDir.TrimEnd('\') })) {
    $quotedInstallDir = $InstallDir.Replace("'", "''")
    Write-Host "Add it to PATH for this user:"
    Write-Host "  [Environment]::SetEnvironmentVariable('PATH', '$quotedInstallDir;' + [Environment]::GetEnvironmentVariable('PATH','User'), 'User')"
    Write-Host "Open a new terminal after changing PATH."
  }
  Write-Host "Run: term-llm version"
} finally {
  Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue
}
