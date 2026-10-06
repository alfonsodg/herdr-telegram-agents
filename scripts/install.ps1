# Downloads the release binary for this host into bin\herdr-tg.exe and
# checks its SHA-256 against the release checksums file. Run by herdr as
# the plugin's [[build]] step on windows; it gets no HERDR_* variables.
# $env:HERDR_TG_BASE_URL overrides the download location; it must be
# https:// unless $env:HERDR_TG_ALLOW_INSECURE_BASE is "1". The plugin's
# updater sets $env:HERDR_TG_EXPECTED_SHA256 to the checksum the owner
# approved: the binary must match it as well as checksums.txt.
$ErrorActionPreference = "Stop"

# -LiteralPath: a checkout under a folder such as plugin[1] must not be read
# as a wildcard pattern. Every later path is relative to this directory.
Set-Location -LiteralPath (Join-Path $PSScriptRoot "..")
Write-Host "install: working in $((Get-Location).ProviderPath)"

$repo = "permgps/herdr-telegram-agents"

# Get-FileHash is a script function that Windows PowerShell 5.1 autoloads
# from its Utility module. When Herdr was started from PowerShell 7, this
# process inherits pwsh's module paths first and cannot load it, so hash
# through .NET, which needs no module.
function Get-Sha256Hex([string]$Path) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $stream = [System.IO.File]::OpenRead((Resolve-Path -LiteralPath $Path -ErrorAction Stop).ProviderPath)
        try {
            return ([System.BitConverter]::ToString($sha.ComputeHash($stream)) -replace '-', '').ToLower()
        } finally {
            $stream.Dispose()
        }
    } finally {
        $sha.Dispose()
    }
}

$match = Select-String -Path herdr-plugin.toml -Pattern '^version\s*=\s*"(.*)"' | Select-Object -First 1
if (-not $match) { throw "install: no version in herdr-plugin.toml" }
$version = $match.Matches[0].Groups[1].Value

$arch = "amd64"
if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq "Arm64") {
    Write-Host "install: no windows/arm64 build; using the amd64 binary under emulation"
}

$asset = "herdr-tg_windows_$arch.exe"
$base = $env:HERDR_TG_BASE_URL
if (-not $base) { $base = "https://github.com/$repo/releases/download/v$version" }
if (-not $base.StartsWith("https://") -and $env:HERDR_TG_ALLOW_INSECURE_BASE -ne "1") {
    throw "install: HERDR_TG_BASE_URL must be https:// (set HERDR_TG_ALLOW_INSECURE_BASE=1 for a local snapshot)"
}
Write-Host "install: herdr-tg $version for windows/$arch"

New-Item -ItemType Directory -Force -Path bin | Out-Null
# Unique names: a run killed half-way must not break the next one.
$suffix = [System.Guid]::NewGuid().ToString("N")
$tmp = "bin\herdr-tg.$suffix.tmp"
$sums = "bin\checksums.$suffix.txt"

try {
    Write-Host "install: downloading $asset"
    Invoke-WebRequest -Uri "$base/$asset" -OutFile $tmp -UseBasicParsing -TimeoutSec 120 -MaximumRedirection 5
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -UseBasicParsing -TimeoutSec 60 -MaximumRedirection 5

    $lines = @(Select-String -Path $sums -Pattern "\s$([regex]::Escape($asset))$")
    if ($lines.Count -eq 0) { throw "install: $asset is missing from checksums.txt" }
    if ($lines.Count -ne 1) { throw "install: $asset is listed $($lines.Count) times in checksums.txt" }
    $expected = ($lines[0].Line -split '\s+')[0].ToLower()
    $actual = Get-Sha256Hex $tmp
    if ($expected -ne $actual) {
        throw "install: checksum mismatch for $asset (expected $expected, got $actual)"
    }
    $approved = "$env:HERDR_TG_EXPECTED_SHA256".ToLower()
    if ($approved -and $approved -ne $actual) {
        throw "install: $asset differs from the approved checksum (approved $approved, got $actual)"
    }
    Write-Host "install: checksum ok"

    Move-Item -Force $tmp "bin\herdr-tg.exe"
    Write-Host "install: installed bin\herdr-tg.exe"
} finally {
    if (Test-Path $tmp) { Remove-Item -Force $tmp }
    if (Test-Path $sums) { Remove-Item -Force $sums }
}

& ".\bin\herdr-tg.exe" version
