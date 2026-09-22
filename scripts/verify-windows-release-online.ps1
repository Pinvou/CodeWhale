[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^https://')]
    [string]$Url,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$ExpectedSha256,

    [Parameter(Mandatory = $true)]
    [ValidateRange(1, 4294967296)]
    [Int64]$ExpectedSize
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-Sha256Hex {
    param([Parameter(Mandatory = $true)][string]$Path)

    # Some minimal Windows PowerShell 5.1 images omit the hashing cmdlet.
    $stream = [System.IO.File]::Open(
        $Path,
        [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Read,
        [System.IO.FileShare]::Read
    )
    try {
        $sha256 = [System.Security.Cryptography.SHA256]::Create()
        try {
            $digest = $sha256.ComputeHash($stream)
            return ([System.BitConverter]::ToString($digest)).Replace('-', '').ToLowerInvariant()
        } finally {
            $sha256.Dispose()
        }
    } finally {
        $stream.Dispose()
    }
}

try {
    $uri = [System.Uri]::new($Url, [System.UriKind]::Absolute)
} catch {
    throw "Release URL is not an absolute URL: $Url"
}
if ($uri.Scheme -cne 'https' -or [string]::IsNullOrWhiteSpace($uri.Host)) {
    throw "Release URL must be an HTTPS URL with a host: $Url"
}

$tempDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ("pinvou3-release-verify-" + [Guid]::NewGuid().ToString('N'))
$tempFile = Join-Path $tempDirectory 'download.bin'
[System.IO.Directory]::CreateDirectory($tempDirectory) | Out-Null

try {
    Invoke-WebRequest -Uri $uri.AbsoluteUri -OutFile $tempFile -UseBasicParsing
    $download = Get-Item -LiteralPath $tempFile
    if ($download.Length -ne $ExpectedSize) {
        throw "Online installer size mismatch: expected $ExpectedSize bytes, got $($download.Length) bytes"
    }

    $actualSha256 = Get-Sha256Hex -Path $tempFile
    $expected = $ExpectedSha256.ToLowerInvariant()
    if ($actualSha256 -cne $expected) {
        throw "Online installer SHA-256 mismatch: expected $expected, got $actualSha256"
    }

    Write-Host "Online Windows installer verified"
    Write-Host "URL: $($uri.AbsoluteUri)"
    Write-Host "SHA-256: $actualSha256"
    Write-Host "Size: $($download.Length) bytes"
} finally {
    if (Test-Path -LiteralPath $tempDirectory) {
        Remove-Item -LiteralPath $tempDirectory -Recurse -Force
    }
}
