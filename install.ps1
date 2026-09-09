#Requires -Version 5.0
<#
.SYNOPSIS
    Install nemuz on Windows.

.DESCRIPTION
    Downloads a release archive from GitHub, verifies its SHA-256 against the
    published checksums.txt, and installs the binary — to a user-level
    directory by default, with no administrator prompt. nemuz's whole argument
    is a small, sandboxed binary; an installer that demanded elevation to put
    one file on disk would undercut that before the first command runs.

        irm https://raw.githubusercontent.com/kansaok/nemuz/master/install.ps1 | iex

.PARAMETER Version
    A specific release to install, e.g. "v0.13.0". Defaults to the latest.

.PARAMETER InstallDir
    Where to put nemuz.exe. Defaults to $env:LOCALAPPDATA\nemuz\bin, which
    this script adds to the user's PATH if it is not already there.

.NOTES
    Written and checked for the same PowerShell semantics install.sh already
    proves on Linux and macOS, but — unlike that script — not run end to end
    on a real Windows machine as part of building it, because none was
    available. Report anything that doesn't work: https://github.com/kansaok/nemuz/issues
#>
[CmdletBinding()]
param(
    [string]$Version,
    [string]$InstallDir = "$env:LOCALAPPDATA\nemuz\bin"
)

$ErrorActionPreference = "Stop"
$Repo = "kansaok/nemuz"

function Write-Info($msg) { Write-Host $msg }
function Fail($msg) { Write-Error "nemuz: $msg"; exit 1 }

# --- detect architecture, matching goreleaser's {{.Os}}_{{.Arch}} naming ---

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { Fail "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

# --- resolve the version ---

if (-not $Version) {
    Write-Info "finding the latest release..."
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
    $Version = $release.tag_name
    if (-not $Version) { Fail "could not determine the latest version" }
}
$versionNumber = $Version -replace '^v', ''

$archive = "nemuz_${versionNumber}_windows_${arch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$Version"

Write-Info "installing nemuz $Version for windows/$arch"

# --- download and verify ---

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $archivePath = Join-Path $tmp $archive
    $checksumsPath = Join-Path $tmp "checksums.txt"

    Write-Info "downloading $archive..."
    try {
        Invoke-WebRequest -Uri "$baseUrl/$archive" -OutFile $archivePath
    } catch {
        Fail "download failed - does $Version exist for windows/$arch? see https://github.com/$Repo/releases"
    }
    try {
        Invoke-WebRequest -Uri "$baseUrl/checksums.txt" -OutFile $checksumsPath
    } catch {
        Fail "could not download checksums.txt to verify the archive"
    }

    Write-Info "verifying checksum..."
    $expectedLine = Select-String -Path $checksumsPath -Pattern " $archive$" | Select-Object -First 1
    if (-not $expectedLine) { Fail "no checksum listed for $archive" }
    $expected = ($expectedLine.Line -split '\s+')[0]

    $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
    if ($expected -ne $actual) {
        Fail "checksum mismatch for $archive - expected $expected, got $actual"
    }

    # --- install ---

    Expand-Archive -Path $archivePath -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item -Path (Join-Path $tmp "nemuz.exe") -Destination (Join-Path $InstallDir "nemuz.exe") -Force
} finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Info "installed to $InstallDir\nemuz.exe"

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
    $env:Path = "$env:Path;$InstallDir"
    Write-Info ""
    Write-Info "Added $InstallDir to your user PATH."
    Write-Info "Open a new terminal for it to take effect there."
}

Write-Info ""
Write-Info "Run 'nemuz doctor' to check what this machine can confine."
Write-Info "(Landlock and seccomp are Linux-only; on Windows, doctor reports the sandbox as unavailable rather than implying otherwise.)"
