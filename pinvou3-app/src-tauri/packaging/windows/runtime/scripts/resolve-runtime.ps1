param(
  [ValidateSet("Validate", "Stage", "StageOnnx")]
  [string]$Mode = "Stage",
  [string]$RuntimeRoot = "",
  [string]$LockFile = "",
  [switch]$Force,
  [switch]$ImportFunctionsOnly
)

$ErrorActionPreference = "Stop"

# A Windows PowerShell 5.1 process spawned under a pwsh 7 parent (a CI step
# shell or a pwsh terminal) inherits pwsh's PSModulePath and then cannot load
# its own built-in modules: Get-FileHash raises CommandNotFound and Get-Acl
# resolves the unloadable PowerShell 7 module. Rebuild the host default
# module path before the first module-dependent cmdlet (Add-Type included);
# plain 5.1 consoles already match and are left untouched.
if ($PSVersionTable.PSVersion.Major -lt 6) {
  $systemModulePath = Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\Modules"
  $modulePathEntries = @(
    [System.Environment]::GetEnvironmentVariable("PSModulePath", "Process") -split ";"
  ) | Where-Object { $_ }
  $pwshModuleEntries = $modulePathEntries | Where-Object {
    $_ -like "*\PowerShell\7\Modules" -or $_ -like "*\PowerShell\Modules"
  }
  if (($modulePathEntries -notcontains $systemModulePath) -or $pwshModuleEntries) {
    $env:PSModulePath = @(
      (Join-Path $HOME "Documents\WindowsPowerShell\Modules"),
      (Join-Path $env:ProgramFiles "WindowsPowerShell\Modules"),
      $systemModulePath
    ) -join ";"
  }
}

Add-Type -AssemblyName System.IO.Compression.FileSystem

$tauriRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..\..\..")).Path
$appRoot = (Resolve-Path (Join-Path $tauriRoot "..")).Path
$repoRoot = (Resolve-Path (Join-Path $appRoot "..")).Path
$defaultLockFile = Join-Path $tauriRoot "config\platforms\windows\runtime\x86_64.lock.json"
$generatedConfigPath = Join-Path $tauriRoot "target\windows-runtime\tauri.generated.conf.json"
$runtimeDescriptorPath = Join-Path $tauriRoot "target\windows-runtime\runtime-descriptor.json"
$onnxDevDescriptorPath = Join-Path $tauriRoot "target\windows-runtime\onnx-dev-descriptor.json"
$stagingParent = Join-Path $tauriRoot "target\windows-runtime"
$sourceVerificationMarkerPath = Join-Path $stagingParent ".verified-lock"
$resolverLockPath = Join-Path $stagingParent ".resolver.lock"
$verificationSchemaVersion = 5
$stageManifestSchemaVersion = 2

if ([string]::IsNullOrWhiteSpace($LockFile)) {
  $LockFile = $defaultLockFile
}
if (-not (Test-Path -LiteralPath $LockFile -PathType Leaf)) {
  throw "Windows runtime lock manifest not found: $LockFile"
}
$lock = Get-Content -LiteralPath $LockFile -Raw -Encoding UTF8 | ConvertFrom-Json
if ([int]$lock.schemaVersion -ne 2 -or [string]$lock.source.type -ne "git-submodule") {
  throw "Unsupported Windows runtime lock schema or source type."
}
$vcMinimumVersionText = [string]$lock.vcRedist.minimumVersion
if ($vcMinimumVersionText -notmatch '^\d+\.\d+\.\d+\.\d+$') {
  throw "Windows runtime lock must declare vcRedist.minimumVersion as a four-part version."
}
try {
  $vcMinimumVersion = [System.Version]$vcMinimumVersionText
} catch {
  throw "Windows runtime lock contains an invalid VC++ minimum version: $vcMinimumVersionText"
}
if ([string]::IsNullOrWhiteSpace($RuntimeRoot)) {
  $runtimeRootFromEnvironment = [Environment]::GetEnvironmentVariable("PINVOU3_WINDOWS_RUNTIME_ROOT")
  if (-not [string]::IsNullOrWhiteSpace($runtimeRootFromEnvironment)) {
    $RuntimeRoot = $runtimeRootFromEnvironment
  } else {
    $RuntimeRoot = Join-Path $repoRoot ([string]$lock.source.path).Replace('/', '\')
  }
}
$RuntimeRoot = [System.IO.Path]::GetFullPath($RuntimeRoot)
$stageId = ([string]$lock.source.commit).Substring(0, 12) + "-" + ([string]$lock.manifest.sha256).Substring(0, 12)
$stagingRoot = Join-Path $stagingParent $stageId
$stageManifestPath = Join-Path $stagingRoot ".stage-manifest.json"

function Write-Utf8WithoutBom {
  param([string]$Path, [string]$Content)
  $parent = Split-Path -Parent $Path
  if ($parent) {
    New-Item -ItemType Directory -Path $parent -Force | Out-Null
  }
  $encoding = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Content, $encoding)
}

function Write-Utf8Atomically {
  param([string]$Path, [string]$Content)

  $parent = Split-Path -Parent $Path
  if ($parent) {
    New-Item -ItemType Directory -Path $parent -Force | Out-Null
  }
  $temporaryPath = Join-Path $parent ("." + [System.IO.Path]::GetFileName($Path) + ".tmp-" + [System.Guid]::NewGuid().ToString("N"))
  try {
    Write-Utf8WithoutBom -Path $temporaryPath -Content $Content
    if (Test-Path -LiteralPath $Path -PathType Leaf) {
      # ReplaceFile() swaps the destination in one atomic operation, so the
      # unlocked descriptor readers (describeWindowsRuntime and the ONNX dev
      # reader in scripts/tauri/windows-runtime.js) never observe the file
      # missing. Move-Item -Force deletes the destination before renaming the
      # replacement into place, which re-opens exactly that window on every
      # re-stage. PowerShell turns a plain $null argument into an empty
      # string for a .NET string parameter, and Replace would then resolve
      # "" as the backup path and fail; NullString passes a real null.
      [System.IO.File]::Replace($temporaryPath, $Path, [NullString]::Value)
    } else {
      [System.IO.File]::Move($temporaryPath, $Path)
    }
  } finally {
    Remove-Item -LiteralPath $temporaryPath -Force -ErrorAction SilentlyContinue
  }
}

# Adds the Windows extended-length (\\?\) prefix so .NET Framework file APIs
# skip path normalization: normalization caps paths at MAX_PATH (260) and
# throws DirectoryNotFound/PathTooLong past it, which is exactly where the
# staged node tree lands on the self-hosted windows-heavy runners (a 64-char
# workspace root plus npm's bundled node_modules reaches ~263 chars; runs
# 41809813991/41854498430/41977103388 all died on the same field_behavior.js
# path). Idempotent; accepts only absolute drive/UNC paths — every staging
# root in this script is absolute.
function Get-ExtendedPath {
  param([string]$Path)
  if ([string]::IsNullOrWhiteSpace($Path)) {
    throw "Cannot build an extended path from an empty value"
  }
  if ($Path.StartsWith('\\?\', [System.StringComparison]::OrdinalIgnoreCase)) {
    return $Path
  }
  if ($Path.StartsWith('\\', [System.StringComparison]::Ordinal)) {
    return '\\?\UNC\' + $Path.Substring(2)
  }
  if ($Path -match '^[A-Za-z]:[\\/]') {
    return '\\?\' + $Path
  }
  throw "Get-ExtendedPath requires an absolute drive or UNC path: $Path"
}

# Depth-first file walk that enumerates through the \\?\ device namespace:
# plain Get-ChildItem -Recurse materializes FileInfo objects through path
# normalization and throws past MAX_PATH. Returns unprefixed absolute paths;
# callers re-add the prefix via Get-ExtendedPath when handing paths to .NET
# file APIs. Order is not sorted — callers that need deterministic output
# sort the relative paths themselves.
function Get-LongPathFiles {
  param([string]$Root)
  $files = [System.Collections.Generic.List[string]]::new()
  $pendingDirectories = [System.Collections.Generic.Stack[string]]::new()
  $pendingDirectories.Push((Get-ExtendedPath $Root))
  while ($pendingDirectories.Count -gt 0) {
    $directory = $pendingDirectories.Pop()
    foreach ($entry in [System.IO.Directory]::EnumerateFiles($directory, '*')) {
      $files.Add($entry.Substring(4))
    }
    foreach ($entry in [System.IO.Directory]::EnumerateDirectories($directory, '*')) {
      $pendingDirectories.Push($entry)
    }
  }
  return $files
}

# Long-path-safe tree removal for Windows PowerShell 5.1: robocopy is
# long-path native, so mirroring an empty directory over the target deletes
# even past-MAX_PATH content (a successful stage leaves the deep npm tree
# behind, and plain Remove-Item -Recurse would fail on the next re-stage).
# The emptied husk is a short path and goes through the normal cmdlet.
function Remove-TreeLongPath {
  param([string]$Path, [switch]$MissingOk)
  if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
    if ($MissingOk) {
      return
    }
    throw "Windows runtime staging cleanup target is missing: $Path"
  }
  $emptyRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("pinvou3-rm-" + [System.Guid]::NewGuid().ToString("N"))
  try {
    [void][System.IO.Directory]::CreateDirectory($emptyRoot)
    $robocopyOutput = & robocopy $emptyRoot $Path /MIR /NFL /NDL /NJH /NJS /NP
    $robocopyExitCode = $LASTEXITCODE
    if ($robocopyExitCode -ge 8) {
      $robocopyTail = (@($robocopyOutput) | Select-Object -Last 5) -join ' | '
      throw ("Robocopy /MIR cleanup failed with exit code {0}: {1} (tail: {2})" -f $robocopyExitCode, $Path, $robocopyTail)
    }
    Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $Path) {
      throw "Windows runtime staging cleanup could not remove: $Path"
    }
  } finally {
    Remove-Item -LiteralPath $emptyRoot -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# Long-path-safe archive extraction for Windows PowerShell 5.1:
# ZipFile.ExtractToDirectory normalizes every target path and throws
# PathTooLongException past MAX_PATH (extraction targets are TEMP plus the
# archive's relative depth — the node archive's npm tree sits close enough
# to the 260 cliff that deep TEMP roots or fixtures cross it, proven by the
# cache-behavior deep fixture). Walk the entries manually and materialize
# each directory/file through the \\?\ namespace instead. Containment is
# still enforced per entry: an entry escaping the destination fails the
# Assert-ChildPath check.
function Expand-ZipArchiveLongPath {
  param([string]$ZipPath, [string]$Destination)
  $archive = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
  try {
    foreach ($entry in $archive.Entries) {
      $entryTargetPath = Join-Path $Destination ($entry.FullName.Replace('/', '\'))
      Assert-ChildPath -Root $Destination -Path $entryTargetPath
      if ([string]::IsNullOrEmpty($entry.Name)) {
        # Directory entries carry an empty Name; file entries create their
        # parent directories below, so this only materializes explicit ones.
        [void][System.IO.Directory]::CreateDirectory((Get-ExtendedPath $entryTargetPath))
        continue
      }
      $parentDirectory = [System.IO.Path]::GetDirectoryName($entryTargetPath)
      if (-not [string]::IsNullOrEmpty($parentDirectory)) {
        [void][System.IO.Directory]::CreateDirectory((Get-ExtendedPath $parentDirectory))
      }
      [System.IO.Compression.ZipFileExtensions]::ExtractToFile($entry, (Get-ExtendedPath $entryTargetPath), $true)
    }
  } finally {
    $archive.Dispose()
  }
}

function Assert-ChildPath {
  param([string]$Root, [string]$Path)
  if ([string]::IsNullOrWhiteSpace($Root) -or [string]::IsNullOrWhiteSpace($Path)) {
    throw "Expected non-empty root and child paths. Root='$Root' Path='$Path'"
  }
  $rootFull = [System.IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
  # Canonicalize through the \\?\ namespace: GetFullPath on an unprefixed
  # child past MAX_PATH throws instead of comparing, and device paths skip
  # normalization, so strip the pass-through prefix before the containment
  # comparison. Staging roots are short; children can exceed MAX_PATH.
  $pathFull = [System.IO.Path]::GetFullPath((Get-ExtendedPath $Path)).Substring(4)
  if (-not $pathFull.StartsWith($rootFull, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to use a path outside the expected root: $Path"
  }
}

function Get-RelativeStagePath {
  param([string]$Root, [string]$Path)
  $rootFull = [System.IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
  $pathFull = [System.IO.Path]::GetFullPath($Path)
  if (-not $pathFull.StartsWith($rootFull, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Path is outside the Windows runtime staging root: $Path"
  }
  return $pathFull.Substring($rootFull.Length).Replace('\', '/')
}

function Get-Sha256 {
  param([string]$Path)
  # Get-FileHash resolves -LiteralPath through the PowerShell provider, which
  # normalizes the path and fails past MAX_PATH; stream the file directly
  # through the \\?\ device namespace instead (staged node trees exceed it on
  # the self-hosted windows-heavy runners). Same digest, lowercase hex.
  $stream = [System.IO.File]::OpenRead((Get-ExtendedPath $Path))
  $sha256 = [System.Security.Cryptography.SHA256]::Create()
  try {
    return ([System.BitConverter]::ToString($sha256.ComputeHash($stream))).Replace('-', '').ToLowerInvariant()
  } finally {
    $stream.Dispose()
    $sha256.Dispose()
  }
}

function Get-StringSha256 {
  param([string]$Value)
  $sha = [System.Security.Cryptography.SHA256]::Create()
  try {
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($Value)
    return ([System.BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
  } finally {
    $sha.Dispose()
  }
}

function Get-VerificationMarkerState {
  param([string]$StageManifestSha256 = "")

  $state = [ordered]@{
    schemaVersion = $verificationSchemaVersion
    sourceCommit = ([string]$lock.source.commit).ToLowerInvariant()
    manifestSha256 = ([string]$lock.manifest.sha256).ToLowerInvariant()
    lockFileSha256 = Get-Sha256 -Path $LockFile
    target = [string]$lock.target
  }
  if (-not [string]::IsNullOrWhiteSpace($StageManifestSha256)) {
    $state["stageManifestSha256"] = $StageManifestSha256.ToLowerInvariant()
  }
  return $state
}

function Get-VerificationMarkerContent {
  param([string]$StageManifestSha256 = "")

  $state = Get-VerificationMarkerState -StageManifestSha256 $StageManifestSha256
  return (($state | ConvertTo-Json -Depth 4) + "`n")
}

function Test-VerificationMarker {
  param(
    [string]$Path,
    [string]$StageManifestSha256 = ""
  )

  if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
    return $false
  }
  $actual = [System.IO.File]::ReadAllText($Path, [System.Text.Encoding]::UTF8)
  return $actual -eq (Get-VerificationMarkerContent -StageManifestSha256 $StageManifestSha256)
}

function Get-GitOutput {
  param([string[]]$Arguments)
  $output = & git -C $RuntimeRoot @Arguments 2>&1
  if ($LASTEXITCODE -ne 0) {
    throw "Unable to inspect Windows runtime submodule: $($output -join ' ')"
  }
  return ($output -join "`n").Trim()
}

function Get-SuperprojectGitlinkCommit {
  $sourcePath = [string]$lock.source.path
  $output = & git -C $repoRoot ls-files --stage -- $sourcePath 2>&1
  if ($LASTEXITCODE -ne 0) {
    throw "Unable to inspect the main-repository Windows runtime gitlink: $($output -join ' ')"
  }
  $line = ($output -join "`n").Trim()
  if ($line -notmatch "^160000 ([0-9a-fA-F]{40}) 0`t") {
    throw "Windows runtime submodule gitlink is missing or unresolved in the main-repository index: $sourcePath"
  }
  return $Matches[1].ToLowerInvariant()
}

function Assert-RuntimeIdentity {
  $actualCommit = Get-GitOutput -Arguments @("rev-parse", "HEAD")
  $expectedCommit = ([string]$lock.source.commit).ToLowerInvariant()
  $gitlinkCommit = Get-SuperprojectGitlinkCommit
  if ($gitlinkCommit -ne $expectedCommit) {
    throw "Windows runtime gitlink does not match the main-repository lock. Expected $expectedCommit, found $gitlinkCommit."
  }
  if ($actualCommit -ne $expectedCommit) {
    throw "Windows runtime submodule commit mismatch. Expected $expectedCommit, found $actualCommit."
  }
  $actualUrl = (Get-GitOutput -Arguments @("remote", "get-url", "origin")).TrimEnd('/')
  $expectedUrl = ([string]$lock.source.url).TrimEnd('/')
  if ($actualUrl -ne $expectedUrl) {
    throw "Windows runtime submodule origin URL mismatch. Expected $expectedUrl, found $actualUrl."
  }
  $dirty = Get-GitOutput -Arguments @("status", "--porcelain", "--untracked-files=no")
  if (-not [string]::IsNullOrWhiteSpace($dirty)) {
    throw "Windows runtime submodule contains tracked local changes; commit and update the main-repository lock first."
  }
  return $actualCommit
}

function Get-RuntimeManifestPath {
  $manifestPath = Join-Path $RuntimeRoot ([string]$lock.manifest.path).Replace('/', '\')
  Assert-ChildPath -Root $RuntimeRoot -Path $manifestPath
  return $manifestPath
}

function Read-CompatibleRuntimeManifest {
  param([string]$ManifestPath)

  $manifest = Get-Content -LiteralPath $ManifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
  if (
    [int]$manifest.schemaVersion -ne 2 -or
    [string]$manifest.target -ne [string]$lock.target -or
    @($manifest.stagedFiles).Count -eq 0
  ) {
    throw "Windows runtime submodule manifest schema or target is incompatible."
  }
  return $manifest
}

function Test-LfsPointer {
  param([string]$Path)
  $item = Get-Item -LiteralPath $Path
  if ($item.Length -gt 1024) { return $false }
  $firstLine = Get-Content -LiteralPath $Path -TotalCount 1 -ErrorAction SilentlyContinue
  return [string]$firstLine -eq "version https://git-lfs.github.com/spec/v1"
}

function Get-VerifiedManifest {
  if (-not (Test-Path -LiteralPath $RuntimeRoot -PathType Container)) {
    throw "Windows runtime submodule is not initialized: $RuntimeRoot. Run 'npm run runtime:windows:init' from pinvou3-app."
  }

  $actualCommit = Assert-RuntimeIdentity

  $manifestPath = Get-RuntimeManifestPath
  if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    throw "Windows runtime submodule manifest is missing: $manifestPath"
  }
  if ((Get-Sha256 -Path $manifestPath) -ne [string]$lock.manifest.sha256) {
    throw "Windows runtime submodule manifest SHA-256 does not match the main-repository lock."
  }

  $manifest = Read-CompatibleRuntimeManifest -ManifestPath $manifestPath
  foreach ($entry in $manifest.files) {
    $sourcePath = Join-Path $RuntimeRoot ([string]$entry.path).Replace('/', '\')
    Assert-ChildPath -Root $RuntimeRoot -Path $sourcePath
    if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
      throw "Locked Windows runtime file is missing: $($entry.path)"
    }
    if (Test-LfsPointer -Path $sourcePath) {
      throw "Git LFS object is not materialized: $($entry.path). Run 'git -C private-runtimes/windows lfs pull'."
    }
    $item = Get-Item -LiteralPath $sourcePath
    if ([long]$item.Length -ne [long]$entry.bytes) {
      throw "Locked Windows runtime file size mismatch: $($entry.path)"
    }
    if ((Get-Sha256 -Path $sourcePath) -ne [string]$entry.sha256) {
      throw "Locked Windows runtime file SHA-256 mismatch: $($entry.path)"
    }
  }

  $vcEntries = @($manifest.files | Where-Object { [string]$_.component -eq "vc_redist" })
  if ($vcEntries.Count -ne 1) {
    throw "Windows runtime manifest must contain exactly one VC++ redistributable."
  }
  $vcSourcePath = Join-Path $RuntimeRoot ([string]$vcEntries[0].path).Replace('/', '\')
  $vcVersionInfo = [System.Diagnostics.FileVersionInfo]::GetVersionInfo($vcSourcePath)
  $vcActualVersion = [System.Version]::new(
    $vcVersionInfo.FileMajorPart,
    $vcVersionInfo.FileMinorPart,
    $vcVersionInfo.FileBuildPart,
    $vcVersionInfo.FilePrivatePart
  )
  if ($vcActualVersion -lt $vcMinimumVersion) {
    throw "Locked VC++ redistributable version $vcActualVersion is older than required version $vcMinimumVersion."
  }

  Write-Host ("Validated Windows runtime submodule: {0} files at {1}" -f $manifest.files.Count, $actualCommit)
  return $manifest
}

function Get-VerifiedOnnxManifest {
  if (-not (Test-Path -LiteralPath $RuntimeRoot -PathType Container)) {
    throw "Windows runtime submodule is not initialized: $RuntimeRoot. Run 'npm run runtime:windows:init:onnx' from pinvou3-app."
  }
  $actualCommit = Assert-RuntimeIdentity
  $manifestPath = Get-RuntimeManifestPath
  if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    throw "Windows runtime submodule manifest is missing: $manifestPath"
  }
  if ((Get-Sha256 -Path $manifestPath) -ne [string]$lock.manifest.sha256) {
    throw "Windows runtime submodule manifest SHA-256 does not match the main-repository lock."
  }
  $manifest = Read-CompatibleRuntimeManifest -ManifestPath $manifestPath
  $entries = @($manifest.files | Where-Object { [string]$_.path -like "*/onnxruntime-win-x64-*-runtime.zip" })
  if ($entries.Count -ne 1) {
    throw "Windows runtime manifest must contain exactly one ONNX Runtime archive."
  }
  $sourcePath = Join-Path $RuntimeRoot ([string]$entries[0].path).Replace('/', '\')
  Assert-ChildPath -Root $RuntimeRoot -Path $sourcePath
  if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf) -or (Test-LfsPointer -Path $sourcePath)) {
    throw "Locked ONNX Runtime archive is unavailable: $sourcePath. Run 'npm run runtime:windows:init:onnx'."
  }
  $item = Get-Item -LiteralPath $sourcePath
  if ([long]$item.Length -ne [long]$entries[0].bytes -or (Get-Sha256 -Path $sourcePath) -ne [string]$entries[0].sha256) {
    throw "Locked ONNX Runtime archive failed verification: $sourcePath"
  }
  Write-Host ("Validated Windows ONNX Runtime at {0}" -f $actualCommit)
  return $manifest
}

function Get-CachedVerifiedManifest {
  param([switch]$VerifyContent)

  # The source marker is only written after a full Get-VerifiedManifest pass
  # (including the VC++ version check) and binds the lock file SHA-256, so a
  # changed lock invalidates it. Identity failures below stay fatal; per-file
  # mismatches just fall back to full verification.
  if (-not (Test-VerificationMarker -Path $sourceVerificationMarkerPath)) {
    return $null
  }

  # A vanished checkout would surface as a raw git error from the identity
  # check below; decline the cache so the full path raises the actionable
  # "not initialized" message instead.
  if (-not (Test-Path -LiteralPath $RuntimeRoot -PathType Container)) {
    return $null
  }

  $actualCommit = Assert-RuntimeIdentity
  $manifestPath = Get-RuntimeManifestPath
  if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    return $null
  }
  if ((Get-Sha256 -Path $manifestPath) -ne [string]$lock.manifest.sha256) {
    return $null
  }

  $manifest = Read-CompatibleRuntimeManifest -ManifestPath $manifestPath
  foreach ($entry in $manifest.files) {
    $sourcePath = Join-Path $RuntimeRoot ([string]$entry.path).Replace('/', '\')
    Assert-ChildPath -Root $RuntimeRoot -Path $sourcePath
    if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
      return $null
    }
    if (Test-LfsPointer -Path $sourcePath) {
      return $null
    }
    if ([long](Get-Item -LiteralPath $sourcePath).Length -ne [long]$entry.bytes) {
      return $null
    }
    if ($VerifyContent -and (Get-Sha256 -Path $sourcePath) -ne [string]$entry.sha256) {
      return $null
    }
  }

  if ($VerifyContent) {
    Write-Host ("Revalidated cached Windows runtime content: {0} files at {1}" -f $manifest.files.Count, $actualCommit)
  } else {
    Write-Host ("Reused Windows runtime verification metadata: {0} files at {1}" -f $manifest.files.Count, $actualCommit)
  }
  return $manifest
}

function Get-StagedRelativePaths {
  param([string]$Root)

  if (-not (Test-Path -LiteralPath $Root -PathType Container)) {
    return @()
  }
  $rootPrefix = [System.IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
  $metadataPaths = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
  [void]$metadataPaths.Add(".verified-lock")
  [void]$metadataPaths.Add(".stage-manifest.json")
  # Walk through the \\?\ namespace: Get-ChildItem -Recurse plus per-file
  # GetFullPath both normalize and throw past MAX_PATH on the staged node
  # tree (self-hosted windows-heavy runner roots push npm's bundled tree to
  # ~263 chars).
  return @(
    Get-LongPathFiles -Root $Root |
      ForEach-Object {
        $relativePath = $_.Substring($rootPrefix.Length).Replace('\', '/')
        if (-not $metadataPaths.Contains($relativePath)) {
          $relativePath
        }
      } |
      Sort-Object
  )
}

function Get-StagedRelativeFiles {
  param([string]$Root)

  return @(
    Get-StagedRelativePaths -Root $Root | ForEach-Object {
      $relativePath = [string]$_
      $fullPath = Join-Path $Root $relativePath.Replace('/', '\')
      # FileInfo through the \\?\ namespace: Get-Item normalizes and throws
      # past MAX_PATH (deep node tree on self-hosted runners).
      $item = [System.IO.FileInfo]::new((Get-ExtendedPath $fullPath))
      [pscustomobject]@{
        path = $relativePath
        bytes = [long]$item.Length
        sha256 = Get-Sha256 -Path $fullPath
      }
    }
  )
}

function Test-StagedFilesAgainstTrustedManifest {
  param(
    [string]$Root,
    $TrustedEntries
  )

  try {
    $entries = @($TrustedEntries)
    if ($entries.Count -eq 0) {
      return $false
    }

    $expectedByPath = [System.Collections.Generic.Dictionary[string, object]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($entry in $entries) {
      $relativePath = ([string]$entry.path).Replace('\', '/')
      $sha256 = ([string]$entry.sha256).ToLowerInvariant()
      if (
        [string]::IsNullOrWhiteSpace($relativePath) -or
        $relativePath.StartsWith('/') -or
        $relativePath -match '^[A-Za-z]:' -or
        $relativePath -match '(^|/)\.\.(/|$)' -or
        $relativePath -in @(".verified-lock", ".stage-manifest.json") -or
        $sha256 -notmatch '^[0-9a-f]{64}$' -or
        [long]$entry.bytes -lt 0 -or
        $expectedByPath.ContainsKey($relativePath)
      ) {
        return $false
      }

      $fullPath = Join-Path $Root $relativePath.Replace('/', '\')
      Assert-ChildPath -Root $Root -Path $fullPath
      # Existence/size checks through the \\?\ namespace: Test-Path and
      # Get-Item normalize their input and throw past MAX_PATH (deep staged
      # node tree on self-hosted runners); the .NET device-path forms skip
      # normalization instead.
      $extendedPath = Get-ExtendedPath $fullPath
      if (
        -not [System.IO.File]::Exists($extendedPath) -or
        [long]([System.IO.FileInfo]::new($extendedPath)).Length -ne [long]$entry.bytes -or
        (Get-Sha256 -Path $fullPath) -ne $sha256
      ) {
        return $false
      }
      $expectedByPath.Add($relativePath, $entry)
    }

    $actualPaths = @(Get-StagedRelativePaths -Root $Root)
    if ($actualPaths.Count -ne $expectedByPath.Count) {
      return $false
    }
    foreach ($actualPath in $actualPaths) {
      if (-not $expectedByPath.ContainsKey($actualPath)) {
        return $false
      }
    }
    return $true
  } catch {
    return $false
  }
}

function Get-StageManifestContent {
  param([string]$Root)

  $identity = Get-VerificationMarkerState
  $manifest = [ordered]@{
    schemaVersion = $stageManifestSchemaVersion
    sourceCommit = $identity.sourceCommit
    manifestSha256 = $identity.manifestSha256
    lockFileSha256 = $identity.lockFileSha256
    target = $identity.target
    files = @(Get-StagedRelativeFiles -Root $Root)
  }
  return (($manifest | ConvertTo-Json -Depth 6) + "`n")
}

function Write-StageManifest {
  param([string]$Root)

  $path = Join-Path $Root ".stage-manifest.json"
  Write-Utf8WithoutBom -Path $path -Content (Get-StageManifestContent -Root $Root)
  return $path
}

function Test-StageManifest {
  param(
    [string]$ManifestPath,
    $TrustedEntries
  )

  try {
    if (-not (Test-Path -LiteralPath $ManifestPath -PathType Leaf)) {
      return $false
    }
    $cached = Get-Content -LiteralPath $ManifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $identity = Get-VerificationMarkerState
    if (
      [int]$cached.schemaVersion -ne $stageManifestSchemaVersion -or
      [string]$cached.sourceCommit -ne [string]$identity.sourceCommit -or
      [string]$cached.manifestSha256 -ne [string]$identity.manifestSha256 -or
      [string]$cached.lockFileSha256 -ne [string]$identity.lockFileSha256 -or
      [string]$cached.target -ne [string]$identity.target
    ) {
      return $false
    }

    $trustedByPath = [System.Collections.Generic.Dictionary[string, object]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($entry in @($TrustedEntries)) {
      $relativePath = [string]$entry.path
      if ([string]::IsNullOrWhiteSpace($relativePath) -or $trustedByPath.ContainsKey($relativePath)) {
        return $false
      }
      $trustedByPath.Add($relativePath, $entry)
    }

    $cachedByPath = [System.Collections.Generic.Dictionary[string, object]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($entry in @($cached.files)) {
      $relativePath = [string]$entry.path
      if (
        [string]::IsNullOrWhiteSpace($relativePath) -or
        $cachedByPath.ContainsKey($relativePath) -or
        -not $trustedByPath.ContainsKey($relativePath)
      ) {
        return $false
      }
      $trusted = $trustedByPath[$relativePath]
      if (
        [long]$entry.bytes -ne [long]$trusted.bytes -or
        ([string]$entry.sha256).ToLowerInvariant() -ne ([string]$trusted.sha256).ToLowerInvariant()
      ) {
        return $false
      }
      $cachedByPath.Add($relativePath, $entry)
    }
    return $cachedByPath.Count -eq $trustedByPath.Count
  } catch {
    return $false
  }
}

function Find-ComponentArchive {
  param([string]$PayloadRoot, [string]$Pattern, [string]$Label)
  $matches = @(Get-ChildItem -LiteralPath $PayloadRoot -File -Filter $Pattern)
  if ($matches.Count -ne 1) {
    throw "Expected exactly one $Label component archive matching '$Pattern', found $($matches.Count)."
  }
  return $matches[0].FullName
}

function Expand-FlattenedRuntime {
  param(
    [string]$ZipPath,
    [string]$Destination,
    [string]$RequiredFile,
    [switch]$OnnxOnly
  )
  $temporary = Join-Path ([System.IO.Path]::GetTempPath()) ("pinvou3-runtime-expand-" + [System.Guid]::NewGuid().ToString("N"))
  try {
    Expand-ZipArchiveLongPath -ZipPath $ZipPath -Destination $temporary
    $requiredPath = Get-LongPathFiles -Root $temporary |
      Where-Object { [System.IO.Path]::GetFileName($_) -eq $RequiredFile } |
      Select-Object -First 1
    if ($null -eq $requiredPath) {
      throw "Runtime archive does not contain ${RequiredFile}: $ZipPath"
    }
    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    if ($OnnxOnly) {
      Copy-Item -LiteralPath $requiredPath -Destination $Destination -Force
      $shared = Join-Path ([System.IO.Path]::GetDirectoryName($requiredPath)) "onnxruntime_providers_shared.dll"
      if (Test-Path -LiteralPath $shared -PathType Leaf) {
        Copy-Item -LiteralPath $shared -Destination $Destination -Force
      }
    } else {
      # Copy-Item -Recurse (and every PS 5.1 provider cmdlet) enforces the
      # 260-char MAX_PATH limit: the node component ships npm's full bundled
      # node_modules, whose deepest files exceed 260 chars under the
      # self-hosted windows-heavy workspace roots. robocopy is long-path
      # native; /E copies the whole subtree including hidden entries, and
      # without /MIR it only adds (the destination was just created).
      $requiredDirectory = [System.IO.Path]::GetDirectoryName($requiredPath)
      $robocopyOutput = & robocopy $requiredDirectory $Destination /E /NFL /NDL /NJH /NJS /NP
      $robocopyExitCode = $LASTEXITCODE
      if ($robocopyExitCode -ge 8) {
        $robocopyTail = (@($robocopyOutput) | Select-Object -Last 5) -join ' | '
        throw ("Robocopy failed with exit code {0} while copying '{1}' to '{2}' (tail: {3})" -f $robocopyExitCode, $requiredDirectory, $Destination, $robocopyTail)
      }
    }
  } finally {
    Remove-TreeLongPath -Path $temporary -MissingOk
  }
}

function Expand-ComponentRuntime {
  param(
    [string]$ZipPath,
    [string]$Destination,
    [string]$RequiredFile
  )
  New-Item -ItemType Directory -Path $Destination -Force | Out-Null
  Expand-ZipArchiveLongPath -ZipPath $ZipPath -Destination $Destination
  $requiredPath = Join-Path $Destination $RequiredFile
  if (-not (Test-Path -LiteralPath $requiredPath -PathType Leaf)) {
    throw "Component archive is missing required runtime file '${RequiredFile}': $ZipPath"
  }
}

function Test-ManagedArchiveExpansion {
  param(
    $Manifest,
    [string]$ArchiveManifestPath,
    [string]$Destination
  )
  $archiveLocks = @($Manifest.managedArchives | Where-Object { [string]$_.archive -eq $ArchiveManifestPath })
  if ($archiveLocks.Count -ne 1) {
    throw "Managed component archive lock is missing or duplicated: $ArchiveManifestPath"
  }
  $archiveLock = $archiveLocks[0]
  # Same long-path rules as the staged walk: count through the \\?\ namespace
  # walk and verify each entry through the extended .NET forms, so a future
  # archive that deepens its layout cannot reintroduce a MAX_PATH failure.
  $actualFiles = @(Get-LongPathFiles -Root $Destination)
  if ($actualFiles.Count -ne [int]$archiveLock.files) {
    throw "Managed component archive file count mismatch after extraction: $ArchiveManifestPath"
  }
  foreach ($entry in $archiveLock.entries) {
    $entryPath = Join-Path $Destination ([string]$entry.path).Replace('/', '\')
    Assert-ChildPath -Root $Destination -Path $entryPath
    $extendedEntryPath = Get-ExtendedPath $entryPath
    if (-not [System.IO.File]::Exists($extendedEntryPath)) {
      throw "Managed component file is missing after extraction: $ArchiveManifestPath -> $($entry.path)"
    }
    $item = [System.IO.FileInfo]::new($extendedEntryPath)
    if ([long]$item.Length -ne [long]$entry.bytes -or (Get-Sha256 -Path $entryPath) -ne [string]$entry.sha256) {
      throw "Managed component file failed verification after extraction: $ArchiveManifestPath -> $($entry.path)"
    }
  }
}

function Get-RuntimeDescriptorContent {
  param($Manifest, [string]$StageId)
  $relativeRoot = "target/windows-runtime/$StageId"
  $vcRedistRelativePath = "payload/vc_redist/VC_redist.x64.exe"
  $vcRedistPath = Join-Path $stagingRoot $vcRedistRelativePath.Replace('/', '\')
  if (-not (Test-Path -LiteralPath $vcRedistPath -PathType Leaf)) {
    throw "Staged VC++ runtime is missing while generating the runtime descriptor: $vcRedistPath"
  }
  $descriptor = [ordered]@{
    schemaVersion = 1
    target = [string]$lock.target
    stageId = $StageId
    configPath = "target/windows-runtime/tauri.generated.conf.json"
    nodeExecutable = "$relativeRoot/expanded/node/node.exe"
    npmExecPath = "$relativeRoot/expanded/node/node_modules/npm/bin/npm-cli.js"
    onnxRuntimeDylib = "$relativeRoot/expanded/onnxruntime/onnxruntime.dll"
    vcRedist = [ordered]@{
      source = "$relativeRoot/$vcRedistRelativePath"
      bytes = [long](Get-Item -LiteralPath $vcRedistPath).Length
      sha256 = Get-Sha256 -Path $vcRedistPath
    }
    asrModel = [ordered]@{
      delivery = "download-on-first-use"
      bundled = $false
      filename = "sensevoice-small-q8.gguf"
    }
  }
  return (($descriptor | ConvertTo-Json -Depth 8) + "`n")
}

function Write-RuntimeDescriptor {
  param($Manifest, [string]$StageId)
  Write-Utf8Atomically -Path $runtimeDescriptorPath -Content (Get-RuntimeDescriptorContent -Manifest $Manifest -StageId $StageId)
}

function Get-TauriOverlayContent {
  param([string]$StageId)
  $relativeRoot = "target/windows-runtime/$StageId"
  $resources = [ordered]@{
    "$relativeRoot/expanded/poppler/" = "runtime/poppler"
    "$relativeRoot/expanded/pandoc/" = "runtime/pandoc"
    "$relativeRoot/expanded/tesseract/" = "runtime/tesseract"
    "$relativeRoot/expanded/python/" = "runtime/python"
    "$relativeRoot/expanded/node/" = "runtime/node"
    "$relativeRoot/expanded/onnxruntime/" = "runtime/onnxruntime"
    "$relativeRoot/expanded/asr/README.md" = "runtime/asr/README.md"
    "$relativeRoot/expanded/asr/pinvou-asr.exe" = "runtime/asr/pinvou-asr.exe"
    "$relativeRoot/expanded/asr/llama-funasr-sensevoice.exe" = "runtime/asr/llama-funasr-sensevoice.exe"
    "$relativeRoot/expanded/asr/models/fsmn-vad.gguf" = "runtime/asr/models/fsmn-vad.gguf"
    "$relativeRoot/expanded/7zip/" = "runtime/7zip"
  }
  $config = [ordered]@{ bundle = [ordered]@{ resources = $resources } }
  return (($config | ConvertTo-Json -Depth 8) + "`n")
}

function Get-PythonInterpreterTagVersion {
  param([string]$Tag, [string]$Prefix)

  if ($Tag -cnotmatch "^${Prefix}(?<major>\d)(?<minor>\d+)$") {
    return $null
  }
  return [pscustomobject]@{
    Major = [int]$Matches['major']
    Minor = [int]$Matches['minor']
  }
}

function Test-CompatiblePurePythonTag {
  param([string]$Tag, $TargetVersion)

  if ($Tag -ceq "py$($TargetVersion.Major)") {
    return $true
  }
  $version = Get-PythonInterpreterTagVersion -Tag $Tag -Prefix 'py'
  return $null -ne $version -and
    $version.Major -eq $TargetVersion.Major -and
    $version.Minor -le $TargetVersion.Minor
}

function Test-PythonWheelTarget {
  param([string]$Filename, [string]$Target)

  if ($Target -cnotmatch '^(cp(?<targetDigits>\d+))-(?<targetPlatform>[a-z0-9_]+)$') {
    return $false
  }
  $targetPython = $Matches[1]
  $targetPlatform = $Matches['targetPlatform']
  $targetVersion = Get-PythonInterpreterTagVersion -Tag $targetPython -Prefix 'cp'
  if ($null -eq $targetVersion) {
    return $false
  }
  if ($Filename -cnotmatch '-(?<python>[^-]+)-(?<abi>[^-]+)-(?<platform>[^-]+)\.whl$') {
    return $false
  }
  $pythonTags = @($Matches['python'].Split('.'))
  $abiTags = @($Matches['abi'].Split('.'))
  $platformTags = @($Matches['platform'].Split('.'))
  foreach ($tag in @($pythonTags) + @($abiTags) + @($platformTags)) {
    if ($tag -cnotmatch '^[a-z0-9_]+$') {
      return $false
    }
  }
  foreach ($pythonTag in $pythonTags) {
    foreach ($abiTag in $abiTags) {
      foreach ($platformTag in $platformTags) {
        $purePython = $abiTag -ceq 'none' -and (
          $pythonTag -ceq $targetPython -or
          (Test-CompatiblePurePythonTag -Tag $pythonTag -TargetVersion $targetVersion)
        )
        $exactCpython = $pythonTag -ceq $targetPython -and $abiTag -ceq $targetPython
        $pythonVersion = Get-PythonInterpreterTagVersion -Tag $pythonTag -Prefix 'cp'
        $stableAbi = $abiTag -ceq 'abi3' -and
          $null -ne $pythonVersion -and
          $pythonVersion.Major -eq $targetVersion.Major -and
          ($pythonVersion.Major -gt 3 -or ($pythonVersion.Major -eq 3 -and $pythonVersion.Minor -ge 2)) -and
          $pythonVersion.Minor -le $targetVersion.Minor
        $compatible = if ($platformTag -ceq $targetPlatform) {
          $exactCpython -or $stableAbi -or $purePython
        } elseif ($platformTag -ceq 'any') {
          $purePython
        } else {
          $false
        }
        if ($compatible) {
          return $true
        }
      }
    }
  }
  return $false
}

function Test-PythonDependencyTarget {
  param($Target)

  if (
    $null -eq $Target -or
    $Target.platform -isnot [string] -or
    $Target.python -isnot [string] -or
    $Target.imports -isnot [System.Array] -or
    $Target.wheels -isnot [System.Array] -or
    $Target.platform -cnotmatch '^[a-z0-9]+-[a-z0-9]+$' -or
    $Target.python -cnotmatch '^\d+\.\d+$' -or
    @($Target.imports).Count -eq 0 -or
    @($Target.wheels).Count -eq 0
  ) {
    return $false
  }
  foreach ($import in @($Target.imports)) {
    if ($import -isnot [string] -or $import -cnotmatch '^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$') {
      return $false
    }
  }

  $wheelTarget = $null
  if ($Target.platform -ceq 'windows-x64') {
    $versionDigits = ([string]$Target.python).Replace('.', '')
    $wheelTarget = "cp${versionDigits}-win_amd64"
  }
  foreach ($wheel in @($Target.wheels)) {
    if (
      $null -eq $wheel -or
      $wheel.name -isnot [string] -or
      $wheel.version -isnot [string] -or
      [string]::IsNullOrWhiteSpace([string]$wheel.name) -or
      [string]::IsNullOrWhiteSpace([string]$wheel.version) -or
      $wheel.filename -isnot [string] -or
      $wheel.url -isnot [string] -or
      $wheel.sha256 -isnot [string] -or
      $wheel.sha256 -cnotmatch '^[0-9a-f]{64}$' -or
      $wheel.filename -cnotmatch '^[A-Za-z0-9._+-]+\.whl$'
    ) {
      return $false
    }
    try {
      $uri = [System.Uri]::new($wheel.url)
    } catch {
      return $false
    }
    if (
      $uri.Scheme -cne 'https' -or
      $uri.Host -cne 'files.pythonhosted.org' -or
      [System.Uri]::UnescapeDataString($uri.Segments[-1]) -cne [string]$wheel.filename -or
      ($null -ne $wheelTarget -and -not (Test-PythonWheelTarget -Filename $wheel.filename -Target $wheelTarget))
    ) {
      return $false
    }
  }
  return $true
}

function Test-PythonDependencyTargets {
  param(
    [string]$PythonRoot,
    [scriptblock]$AbiProbe,
    [string]$ManifestRoot = ""
  )

  $pythonExe = Join-Path $PythonRoot "python.exe"
  if ($null -eq $AbiProbe -and -not (Test-Path -LiteralPath $pythonExe -PathType Leaf)) {
    Write-Warning "Python dependency ABI probe could not find python.exe: $pythonExe"
    return $false
  }
  if ([string]::IsNullOrWhiteSpace($ManifestRoot)) {
    $ManifestRoot = Join-Path $appRoot "resources\mcp-servers"
  }
  if (-not (Test-Path -LiteralPath $ManifestRoot -PathType Container)) {
    Write-Warning "Python dependency manifest root does not exist: $ManifestRoot"
    return $false
  }
  $manifestPaths = @(Get-ChildItem -LiteralPath $ManifestRoot -Filter manifest.json -File -Recurse)
  foreach ($manifestItem in $manifestPaths) {
    $manifestPath = $manifestItem.FullName
    $tool = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $pipDependencies = @($tool.pip_dependencies | Where-Object { $null -ne $_ })
    if ($null -eq $tool.python_dependencies) {
      if ($pipDependencies.Count -gt 0) {
        Write-Warning "Python MCP declares pip_dependencies without a python_dependencies lock: $manifestPath"
        return $false
      }
      continue
    }
    $dependencyLock = $tool.python_dependencies
    if (
      [int]$dependencyLock.schema_version -ne 1 -or
      $dependencyLock.targets -isnot [System.Array] -or
      @($dependencyLock.targets).Count -eq 0
    ) {
      Write-Warning "Python MCP dependency lock schema is invalid: $manifestPath"
      return $false
    }
    $seenPlatforms = New-Object 'System.Collections.Generic.HashSet[string]' ([System.StringComparer]::Ordinal)
    foreach ($target in @($dependencyLock.targets)) {
      if (-not (Test-PythonDependencyTarget -Target $target) -or -not $seenPlatforms.Add([string]$target.platform)) {
        Write-Warning "Python MCP dependency target is invalid or duplicated: $manifestPath"
        return $false
      }
    }
    $windowsTargets = @($dependencyLock.targets | Where-Object { [string]$_.platform -ceq 'windows-x64' })
    if ($windowsTargets.Count -ne 1) {
      Write-Warning "Python MCP dependency lock must contain exactly one windows-x64 target: $manifestPath"
      return $false
    }
    $target = $windowsTargets[0]
    $abiMatches = if ($null -ne $AbiProbe) {
      & $AbiProbe $pythonExe $target
    } else {
      $probe = "import platform,sys; expected=(int(sys.argv[1]),int(sys.argv[2])); machine=platform.machine().lower(); ok=sys.version_info[:2]==expected and machine in ('amd64','x86_64'); raise SystemExit(0 if ok else 17)"
      $version = ([string]$target.python).Split('.')
      # Discard probe stdout: pipeline output would join the trailing boolean into
      # an array and make `-not $abiMatches` false even on a non-zero exit code.
      $null = & $pythonExe -I -S -B -c $probe $version[0] $version[1]
      $LASTEXITCODE -eq 0
    }
    if (-not $abiMatches) {
      Write-Warning "Bundled Python ABI does not match MCP dependency target '$($target.python)/$($target.platform)' in $manifestPath"
      return $false
    }
  }
  return $true
}

function Assert-FreshStagePythonDependencies {
  param([string]$PythonRoot, [scriptblock]$PythonDependencyProbe)

  $valid = if ($null -eq $PythonDependencyProbe) {
    Test-PythonDependencyTargets -PythonRoot $PythonRoot
  } else {
    & $PythonDependencyProbe $PythonRoot
  }
  if (-not $valid) {
    throw "Bundled Python ABI is incompatible with the locked Windows MCP dependency wheels."
  }
}

function Write-TauriOverlay {
  param([string]$StageId)
  Write-Utf8Atomically -Path $generatedConfigPath -Content (Get-TauriOverlayContent -StageId $StageId)
}

function Test-VerifiedStageReusable {
  param($Manifest, [scriptblock]$PythonDependencyProbe)

  $stageMarkerPath = Join-Path $stagingRoot ".verified-lock"
  if (-not (Test-Path -LiteralPath $stageManifestPath -PathType Leaf)) {
    return $false
  }
  $stageManifestSha256 = Get-Sha256 -Path $stageManifestPath
  if (-not (Test-VerificationMarker -Path $stageMarkerPath -StageManifestSha256 $stageManifestSha256)) {
    return $false
  }
  if (-not (Test-StagedFilesAgainstTrustedManifest -Root $stagingRoot -TrustedEntries $Manifest.stagedFiles)) {
    return $false
  }
  if (-not (Test-StageManifest -ManifestPath $stageManifestPath -TrustedEntries $Manifest.stagedFiles)) {
    return $false
  }

  $pythonRoot = Join-Path $stagingRoot "expanded\python"
  $pythonDependenciesValid = if ($null -eq $PythonDependencyProbe) {
    Test-PythonDependencyTargets -PythonRoot $pythonRoot
  } else {
    & $PythonDependencyProbe $pythonRoot
  }
  if (-not $pythonDependenciesValid) {
    return $false
  }

  if (-not (Test-Path -LiteralPath $generatedConfigPath -PathType Leaf)) {
    return $false
  }
  $actualOverlay = [System.IO.File]::ReadAllText($generatedConfigPath, [System.Text.Encoding]::UTF8)
  if ($actualOverlay -ne (Get-TauriOverlayContent -StageId $stageId)) {
    return $false
  }

  if (-not (Test-Path -LiteralPath $runtimeDescriptorPath -PathType Leaf)) {
    return $false
  }
  $actualDescriptor = [System.IO.File]::ReadAllText($runtimeDescriptorPath, [System.Text.Encoding]::UTF8)
  if ($actualDescriptor -ne (Get-RuntimeDescriptorContent -Manifest $Manifest -StageId $stageId)) {
    return $false
  }

  return $true
}

function Stage-Submodule {
  param(
    $Manifest,
    [switch]$Rebuild
  )
  $markerPath = Join-Path $stagingRoot ".verified-lock"
  Assert-ChildPath -Root (Join-Path $tauriRoot "target") -Path $stagingRoot

  $markerValid = $false
  if (Test-Path -LiteralPath $stageManifestPath -PathType Leaf) {
    $existingStageManifestSha256 = Get-Sha256 -Path $stageManifestPath
    $markerValid = Test-VerificationMarker -Path $markerPath -StageManifestSha256 $existingStageManifestSha256
  }
  if ($Force -or $Rebuild -or -not $markerValid) {
    New-Item -ItemType Directory -Path $stagingParent -Force | Out-Null
    $temporaryRoot = Join-Path $stagingParent (".tmp-" + [System.Guid]::NewGuid().ToString("N"))
    $stageContext = [pscustomobject]@{
      TemporaryRoot = $temporaryRoot
      PayloadRoot = Join-Path $temporaryRoot "payload"
      ExpandedRoot = Join-Path $temporaryRoot "expanded"
    }
    Assert-ChildPath -Root $stagingParent -Path $stageContext.TemporaryRoot
    try {
      foreach ($entry in $Manifest.files) {
        $sourcePath = Join-Path $RuntimeRoot ([string]$entry.path).Replace('/', '\')
        $destinationPath = Join-Path $stageContext.TemporaryRoot ([string]$entry.path).Replace('/', '\')
        Assert-ChildPath -Root $stageContext.TemporaryRoot -Path $destinationPath
        New-Item -ItemType Directory -Path (Split-Path -Parent $destinationPath) -Force | Out-Null
        Copy-Item -LiteralPath $sourcePath -Destination $destinationPath -Force
      }

      $payloadRoot = $stageContext.PayloadRoot
      $expandedRoot = $stageContext.ExpandedRoot
      Expand-ComponentRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "7zip-runtime.zip" -Label "7-Zip") -Destination (Join-Path $expandedRoot "7zip") -RequiredFile "7z.exe"
      Expand-ComponentRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "asr-runtime.zip" -Label "ASR") -Destination (Join-Path $expandedRoot "asr") -RequiredFile "pinvou-asr.exe"
      Expand-ComponentRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "poppler-runtime.zip" -Label "Poppler") -Destination (Join-Path $expandedRoot "poppler") -RequiredFile "pdftotext.exe"
      Expand-ComponentRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "tesseract-runtime.zip" -Label "Tesseract") -Destination (Join-Path $expandedRoot "tesseract") -RequiredFile "tesseract.exe"
      Test-ManagedArchiveExpansion -Manifest $Manifest -ArchiveManifestPath "payload/7zip-runtime.zip" -Destination (Join-Path $expandedRoot "7zip")
      Test-ManagedArchiveExpansion -Manifest $Manifest -ArchiveManifestPath "payload/asr-runtime.zip" -Destination (Join-Path $expandedRoot "asr")
      Test-ManagedArchiveExpansion -Manifest $Manifest -ArchiveManifestPath "payload/poppler-runtime.zip" -Destination (Join-Path $expandedRoot "poppler")
      Test-ManagedArchiveExpansion -Manifest $Manifest -ArchiveManifestPath "payload/tesseract-runtime.zip" -Destination (Join-Path $expandedRoot "tesseract")

      # The main ASR model is downloaded and verified on first use. Keep only the
      # engine and the small VAD model in installer staging.
      $bundledAsrModelPath = Join-Path $expandedRoot "asr\models\sensevoice-small-q8.gguf"
      if (Test-Path -LiteralPath $bundledAsrModelPath -PathType Leaf) {
        Remove-Item -LiteralPath $bundledAsrModelPath -Force
      }
      if (Test-Path -LiteralPath $bundledAsrModelPath) {
        throw "Download-on-first-use ASR model remained in Windows runtime staging: $bundledAsrModelPath"
      }

      Expand-FlattenedRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "python-*-embed-amd64.zip" -Label "Python") -Destination (Join-Path $expandedRoot "python") -RequiredFile "pythonw.exe"
      Assert-FreshStagePythonDependencies -PythonRoot (Join-Path $expandedRoot "python")
      Expand-FlattenedRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "node-*-win-x64.zip" -Label "Node.js") -Destination (Join-Path $expandedRoot "node") -RequiredFile "node.exe"
      Expand-FlattenedRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "pandoc-*-windows-x86_64.zip" -Label "Pandoc") -Destination (Join-Path $expandedRoot "pandoc") -RequiredFile "pandoc.exe"
      Expand-FlattenedRuntime -ZipPath (Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "onnxruntime-win-x64-*-runtime.zip" -Label "ONNX Runtime") -Destination (Join-Path $expandedRoot "onnxruntime") -RequiredFile "onnxruntime.dll" -OnnxOnly
      if (-not (Test-StagedFilesAgainstTrustedManifest -Root $temporaryRoot -TrustedEntries $Manifest.stagedFiles)) {
        throw "Generated Windows runtime staging does not match the locked private-runtime stagedFiles manifest."
      }
      $temporaryStageManifestPath = Write-StageManifest -Root $temporaryRoot
      $temporaryStageManifestSha256 = Get-Sha256 -Path $temporaryStageManifestPath
      Write-Utf8WithoutBom -Path (Join-Path $temporaryRoot ".verified-lock") -Content (Get-VerificationMarkerContent -StageManifestSha256 $temporaryStageManifestSha256)

      if (Test-Path -LiteralPath $stagingRoot) {
        Remove-TreeLongPath -Path $stagingRoot
      }
      Move-Item -LiteralPath $stageContext.TemporaryRoot -Destination $stagingRoot
    } finally {
      Remove-TreeLongPath -Path $stageContext.TemporaryRoot -MissingOk
    }
  }

  Write-TauriOverlay -StageId $stageId
  Write-RuntimeDescriptor -Manifest $Manifest -StageId $stageId
  Write-Host ("Staged Windows runtime submodule: {0}" -f $stagingRoot)
  Write-Host ("Generated Tauri overlay: {0}" -f $generatedConfigPath)
  Write-Host ("Generated runtime descriptor: {0}" -f $runtimeDescriptorPath)
}

function Get-OnnxDevDescriptorContent {
  param([string]$StageId, [string]$DylibPath)
  $descriptor = [ordered]@{
    schemaVersion = 1
    target = [string]$lock.target
    stageId = $StageId
    onnxRuntimeDylib = Get-RelativeStagePath -Root $tauriRoot -Path $DylibPath
    bytes = [long](Get-Item -LiteralPath $DylibPath).Length
    sha256 = Get-Sha256 -Path $DylibPath
  }
  return (($descriptor | ConvertTo-Json -Depth 4) + "`n")
}

function Test-OnnxDevStageReusable {
  param([string]$StageRoot)
  if (-not (Test-Path -LiteralPath $onnxDevDescriptorPath -PathType Leaf)) { return $false }
  try {
    $descriptor = Get-Content -LiteralPath $onnxDevDescriptorPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ([int]$descriptor.schemaVersion -ne 1 -or [string]$descriptor.stageId -ne $stageId) { return $false }
    $dylib = Join-Path $tauriRoot ([string]$descriptor.onnxRuntimeDylib).Replace('/', '\')
    Assert-ChildPath -Root $tauriRoot -Path $dylib
    $expectedDylib = Join-Path $StageRoot "onnxruntime\onnxruntime.dll"
    if (-not ([System.IO.Path]::GetFullPath($dylib)).Equals([System.IO.Path]::GetFullPath($expectedDylib), [System.StringComparison]::OrdinalIgnoreCase)) { return $false }
    if (-not (Test-Path -LiteralPath $dylib -PathType Leaf)) { return $false }
    $item = Get-Item -LiteralPath $dylib
    return [long]$item.Length -eq [long]$descriptor.bytes -and (Get-Sha256 -Path $dylib) -eq [string]$descriptor.sha256
  } catch {
    return $false
  }
}

function Stage-OnnxRuntime {
  param($Manifest)
  $onnxStageRoot = Join-Path $stagingParent ($stageId + "-onnx-dev")
  if (-not $Force -and (Test-OnnxDevStageReusable -StageRoot $onnxStageRoot)) {
    Write-Host ("Reused verified Windows ONNX Runtime staging: {0}" -f $onnxStageRoot)
    return
  }
  $temporaryRoot = Join-Path $stagingParent (".onnx-tmp-" + [System.Guid]::NewGuid().ToString("N"))
  Assert-ChildPath -Root $stagingParent -Path $temporaryRoot
  try {
    $payloadRoot = Join-Path $RuntimeRoot "payload"
    $destination = Join-Path $temporaryRoot "onnxruntime"
    $archive = Find-ComponentArchive -PayloadRoot $payloadRoot -Pattern "onnxruntime-win-x64-*-runtime.zip" -Label "ONNX Runtime"
    Expand-FlattenedRuntime -ZipPath $archive -Destination $destination -RequiredFile "onnxruntime.dll" -OnnxOnly
    $dylib = Join-Path $destination "onnxruntime.dll"
    if (-not (Test-Path -LiteralPath $dylib -PathType Leaf)) {
      throw "ONNX Runtime staging did not produce onnxruntime.dll"
    }
    if (Test-Path -LiteralPath $onnxStageRoot) {
      Remove-TreeLongPath -Path $onnxStageRoot
    }
    Move-Item -LiteralPath $temporaryRoot -Destination $onnxStageRoot
    $finalDylib = Join-Path $onnxStageRoot "onnxruntime\onnxruntime.dll"
    Write-Utf8Atomically -Path $onnxDevDescriptorPath -Content (Get-OnnxDevDescriptorContent -StageId $stageId -DylibPath $finalDylib)
  } finally {
    Remove-TreeLongPath -Path $temporaryRoot -MissingOk
  }
  Write-Host ("Staged Windows ONNX Runtime for development: {0}" -f $onnxStageRoot)
  Write-Host ("Generated ONNX development descriptor: {0}" -f $onnxDevDescriptorPath)
}

function Enter-ResolverLock {
  param([int]$TimeoutSeconds = 120)

  New-Item -ItemType Directory -Path $stagingParent -Force | Out-Null
  $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
  while ($true) {
    try {
      return [System.IO.File]::Open(
        $resolverLockPath,
        [System.IO.FileMode]::OpenOrCreate,
        [System.IO.FileAccess]::ReadWrite,
        [System.IO.FileShare]::None
      )
    } catch [System.IO.IOException] {
      if ([DateTime]::UtcNow -ge $deadline) {
        throw "Timed out waiting for the Windows runtime resolver lock: $resolverLockPath"
      }
      Start-Sleep -Milliseconds 200
    }
  }
}

function Invoke-RuntimeResolution {
  if ($Mode -eq "StageOnnx") {
    Stage-OnnxRuntime -Manifest (Get-VerifiedOnnxManifest)
    return
  }
  $verifiedManifest = $null
  $sourceMetadataWasCached = $false
  if (-not $Force) {
    $verifiedManifest = Get-CachedVerifiedManifest -VerifyContent:($Mode -eq "Validate")
    $sourceMetadataWasCached = $null -ne $verifiedManifest
  }

  if ($null -eq $verifiedManifest) {
    $verifiedManifest = Get-VerifiedManifest
    Write-Utf8Atomically -Path $sourceVerificationMarkerPath -Content (Get-VerificationMarkerContent)
  }

  if ($Mode -ne "Stage") {
    return
  }

  if (-not $Force -and (Test-VerifiedStageReusable -Manifest $verifiedManifest)) {
    Write-Host ("Reused verified Windows runtime staging: {0}" -f $stagingRoot)
    Write-Host ("Reused Tauri overlay: {0}" -f $generatedConfigPath)
    Write-Host ("Reused runtime descriptor: {0}" -f $runtimeDescriptorPath)
    return
  }

  if ($sourceMetadataWasCached) {
    Write-Host "Cached staging failed integrity checks; revalidating source payloads before rebuilding."
    $verifiedManifest = Get-VerifiedManifest
    Write-Utf8Atomically -Path $sourceVerificationMarkerPath -Content (Get-VerificationMarkerContent)
  }

  Stage-Submodule -Manifest $verifiedManifest -Rebuild
}

if ($ImportFunctionsOnly) {
  return
}

$resolverLock = Enter-ResolverLock
try {
  Invoke-RuntimeResolution
} finally {
  if ($null -ne $resolverLock) {
    $resolverLock.Dispose()
  }
}
