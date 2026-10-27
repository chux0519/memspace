# Install the MemSpace client from GitHub releases (Windows).
#
#   irm https://raw.githubusercontent.com/chux0519/memspace/main/install.ps1 | iex
#
# Choose a version or directory with environment variables before running:
#   $env:MEMSPACE_VERSION = "v0.1.0"        # default: the latest release
#   $env:MEMSPACE_INSTALL_DIR = "C:\tools"  # default: $HOME\bin
#
# The download is verified against the release's SHA256SUMS.

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # Invoke-WebRequest is slow with a progress bar

$repo = "chux0519/memspace"
$version = if ($env:MEMSPACE_VERSION) { $env:MEMSPACE_VERSION } else { "latest" }
$dir = if ($env:MEMSPACE_INSTALL_DIR) { $env:MEMSPACE_INSTALL_DIR } else { Join-Path $HOME "bin" }

$arch = $env:PROCESSOR_ARCHITECTURE
if ($arch -ne "AMD64" -and $arch -ne "ARM64") {
    throw "install.ps1: unsupported architecture $arch"
}
# Windows on ARM runs the amd64 build under emulation.
$asset = "memspace-windows-amd64.exe"

$base = if ($version -eq "latest") {
    "https://github.com/$repo/releases/latest/download"
} else {
    "https://github.com/$repo/releases/download/$version"
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("memspace-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force $tmp | Out-Null
try {
    Write-Host "Downloading $asset ($version)..."
    Invoke-WebRequest "$base/$asset" -OutFile (Join-Path $tmp $asset) -UseBasicParsing
    Invoke-WebRequest "$base/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS") -UseBasicParsing

    $want = $null
    foreach ($line in Get-Content (Join-Path $tmp "SHA256SUMS")) {
        $fields = $line -split "\s+"
        if ($fields.Count -ge 2 -and $fields[1].TrimStart("*") -eq $asset) { $want = $fields[0].ToLower() }
    }
    if (-not $want) { throw "install.ps1: $asset is not listed in SHA256SUMS" }
    $got = (Get-FileHash (Join-Path $tmp $asset) -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $want) { throw "install.ps1: checksum mismatch for $asset (got $got, want $want)" }

    New-Item -ItemType Directory -Force $dir | Out-Null
    $target = Join-Path $dir "memspace.exe"
    Move-Item -Force (Join-Path $tmp $asset) $target
    Write-Host "Installed $(& $target --version) to $target"
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not (($userPath -split ";") -contains $dir)) {
    Write-Host "Note: $dir is not on PATH. To add it for your user:"
    Write-Host "  [Environment]::SetEnvironmentVariable('Path', `"$dir;`$([Environment]::GetEnvironmentVariable('Path','User'))`", 'User')"
}
