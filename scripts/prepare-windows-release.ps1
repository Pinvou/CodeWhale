[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$InstallerPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+(?:\.\d+)?$')]
    [string]$Version,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{7,40}$')]
    [string]$GitCommit,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$ExpectedSha256,

    [Parameter(Mandatory = $true)]
    [string]$Notes,

    [Parameter(Mandatory = $true)]
    [string]$CurrentManifest,

    [string]$BaseUrl = 'https://pinvou.com/pinvou3',
    [string]$OutputManifest,
    [string]$OutputReleaseInfo
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Utf8NoBom {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,
        [Parameter(Mandatory = $true)]
        [string]$Text
    )

    $encoding = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, $Text, $encoding)
}

function Read-StrictUtf8 {
    param([Parameter(Mandatory = $true)][string]$Path)

    # Windows PowerShell 5.1 treats no-BOM text as the system ANSI code page.
    # A throwing UTF-8 decoder accepts no-BOM/BOM UTF-8 and rejects malformed bytes.
    return [System.IO.File]::ReadAllText($Path, [System.Text.UTF8Encoding]::new($false, $true))
}

function Parse-ReleaseVersion {
    param([Parameter(Mandatory = $true)][string]$Value)

    try {
        return [System.Version]::Parse($Value)
    } catch {
        throw "Invalid release version: $Value"
    }
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$versionPath = Join-Path $repoRoot 'VERSION'
if (-not (Test-Path -LiteralPath $versionPath -PathType Leaf)) {
    throw "Repository VERSION file not found: $versionPath"
}
$repositoryVersion = (Get-Content -LiteralPath $versionPath -Raw).Trim()
if ($repositoryVersion -ne $Version) {
    throw "Release version $Version does not match repository VERSION $repositoryVersion"
}

$resolvedInstaller = (Resolve-Path -LiteralPath $InstallerPath).Path
$installer = Get-Item -LiteralPath $resolvedInstaller
if ($installer.PSIsContainer) {
    throw "Installer path is a directory: $resolvedInstaller"
}
$expectedFileName = "pinvou3_${Version}_x64-setup.exe"
if ($installer.Name -cne $expectedFileName) {
    throw "Signed installer must be named $expectedFileName; got $($installer.Name)"
}

$resolvedManifest = (Resolve-Path -LiteralPath $CurrentManifest).Path
$manifestText = Read-StrictUtf8 -Path $resolvedManifest
if ([string]::IsNullOrWhiteSpace($manifestText)) {
    throw "Current manifest UTF-8 content is empty: $resolvedManifest"
}
try {
    $manifest = $manifestText | ConvertFrom-Json
} catch {
    throw "Current manifest is not valid JSON: $resolvedManifest"
}
if ($null -eq $manifest) {
    throw "Current manifest is empty: $resolvedManifest"
}

if (-not $manifest.PSObject.Properties['platforms']) {
    $manifest | Add-Member -MemberType NoteProperty -Name platforms -Value ([pscustomobject]@{})
}
if ($null -eq $manifest.platforms) {
    $manifest.platforms = [pscustomobject]@{}
}

$newVersion = Parse-ReleaseVersion -Value $Version
$currentWindows = $manifest.platforms.PSObject.Properties['windows-x86_64']
if ($null -ne $currentWindows -and $null -ne $currentWindows.Value.version) {
    $publishedVersion = Parse-ReleaseVersion -Value ([string]$currentWindows.Value.version)
    if ($newVersion -le $publishedVersion) {
        throw "Windows release version must be greater than published version $publishedVersion"
    }
}

$hash = (Get-FileHash -LiteralPath $resolvedInstaller -Algorithm SHA256).Hash.ToLowerInvariant()
$normalizedExpectedHash = $ExpectedSha256.ToLowerInvariant()
if ($hash -cne $normalizedExpectedHash) {
    throw "Signed installer SHA-256 mismatch: expected $normalizedExpectedHash, got $hash"
}

try {
    $baseUri = [System.Uri]::new($BaseUrl, [System.UriKind]::Absolute)
} catch {
    throw "Base URL is not an absolute URL: $BaseUrl"
}
if ($baseUri.Scheme -cne 'https') {
    throw "Base URL must use HTTPS: $BaseUrl"
}

$publishedAt = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
$downloadUrl = "$($BaseUrl.TrimEnd('/'))/$expectedFileName"
$entry = [pscustomobject][ordered]@{
    version = $Version
    url = $downloadUrl
    format = 'nsis'
    sha256 = $hash
    size = [int64]$installer.Length
    restart_after_install = $false
    notes = $Notes
    pub_date = $publishedAt
}
$manifest.platforms | Add-Member -MemberType NoteProperty -Name 'windows-x86_64' -Value $entry -Force

if ([string]::IsNullOrWhiteSpace($OutputManifest)) {
    $OutputManifest = Join-Path (Get-Location) 'latest.json.new'
}
if ([string]::IsNullOrWhiteSpace($OutputReleaseInfo)) {
    $OutputReleaseInfo = Join-Path (Get-Location) "pinvou3_${Version}_windows-release-info.json"
}
$manifestOutput = [System.IO.Path]::GetFullPath($OutputManifest)
$releaseInfoOutput = [System.IO.Path]::GetFullPath($OutputReleaseInfo)

$releaseInfo = [pscustomobject][ordered]@{
    schema_version = 1
    platform = 'windows-x86_64'
    version = $Version
    git_commit = $GitCommit.ToLowerInvariant()
    file_name = $expectedFileName
    size = [int64]$installer.Length
    sha256 = $hash
    url = $downloadUrl
    prepared_at = $publishedAt
}

Write-Utf8NoBom -Path $manifestOutput -Text (($manifest | ConvertTo-Json -Depth 20) + "`n")
Write-Utf8NoBom -Path $releaseInfoOutput -Text (($releaseInfo | ConvertTo-Json -Depth 5) + "`n")

$verifyManifest = Read-StrictUtf8 -Path $manifestOutput | ConvertFrom-Json
$verifyEntry = $verifyManifest.platforms.PSObject.Properties['windows-x86_64'].Value
if ($verifyEntry.version -ne $Version -or $verifyEntry.sha256 -ne $hash -or $verifyEntry.size -ne $installer.Length) {
    throw 'Generated manifest verification failed'
}

Write-Host "Prepared Windows release v$Version"
Write-Host "Installer: $resolvedInstaller"
Write-Host "SHA-256: $hash"
Write-Host "Size: $($installer.Length) bytes"
Write-Host "Manifest: $manifestOutput"
Write-Host "Release info: $releaseInfoOutput"
