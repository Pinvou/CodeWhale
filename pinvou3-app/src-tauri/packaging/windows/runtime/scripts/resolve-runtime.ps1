param(
  [ValidateSet("Validate", "Stage", "StageOnnx")]
  [string]$Mode = "Stage",
  [string]$RuntimeRoot = "",
  [string]$LockFile = "",
  [switch]$Force,
  [switch]$ImportFunctionsOnly
)

$ErrorActionPreference = "Stop"

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
    Move-Item -LiteralPath $temporaryPath -Destination $Path -Force
  } finally {
    Remove-Item -LiteralPath $temporaryPath -Force -ErrorAction SilentlyContinue
  }
}

function Assert-ChildPath {
  param([string]$Root, [string]$Path)
  if ([string]::IsNullOrWhiteSpace($Root) -or [string]::IsNullOrWhiteSpace($Path)) {
    throw "Expected non-empty root and child paths. Root='$Root' Path='$Path'"
  }
  $rootFull = [System.IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
  $pathFull = [System.IO.Path]::GetFullPath($Path)
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
  return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
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

  if (-not (Test-VerificationMarker -Path $sourceVerificationMarkerPath)) {
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
  return @(
    Get-ChildItem -LiteralPath $Root -Recurse -File -Force |
      ForEach-Object {
        $fullPath = [System.IO.Path]::GetFullPath($_.FullName)
        $relativePath = $fullPath.Substring($rootPrefix.Length).Replace('\', '/')
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
      $item = Get-Item -LiteralPath $fullPath
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
      if (
        -not (Test-Path -LiteralPath $fullPath -PathType Leaf) -or
        [long](Get-Item -LiteralPath $fullPath).Length -ne [long]$entry.bytes -or
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
    [System.IO.Compression.ZipFile]::ExtractToDirectory($ZipPath, $temporary)
    $required = Get-ChildItem -LiteralPath $temporary -File -Recurse -Filter $RequiredFile | Select-Object -First 1
    if ($null -eq $required) {
      throw "Runtime archive does not contain ${RequiredFile}: $ZipPath"
    }
    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    if ($OnnxOnly) {
      Copy-Item -LiteralPath $required.FullName -Destination $Destination -Force
      $shared = Join-Path $required.DirectoryName "onnxruntime_providers_shared.dll"
      if (Test-Path -LiteralPath $shared -PathType Leaf) {
        Copy-Item -LiteralPath $shared -Destination $Destination -Force
      }
    } else {
      Get-ChildItem -LiteralPath $required.DirectoryName -Force | ForEach-Object {
        Copy-Item -LiteralPath $_.FullName -Destination $Destination -Recurse -Force
      }
    }
  } finally {
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
  }
}

function Expand-ComponentRuntime {
  param(
    [string]$ZipPath,
    [string]$Destination,
    [string]$RequiredFile
  )
  New-Item -ItemType Directory -Path $Destination -Force | Out-Null
  [System.IO.Compression.ZipFile]::ExtractToDirectory($ZipPath, $Destination)
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
  $actualFiles = @(Get-ChildItem -LiteralPath $Destination -File -Recurse -Force)
  if ($actualFiles.Count -ne [int]$archiveLock.files) {
    throw "Managed component archive file count mismatch after extraction: $ArchiveManifestPath"
  }
  foreach ($entry in $archiveLock.entries) {
    $entryPath = Join-Path $Destination ([string]$entry.path).Replace('/', '\')
    Assert-ChildPath -Root $Destination -Path $entryPath
    if (-not (Test-Path -LiteralPath $entryPath -PathType Leaf)) {
      throw "Managed component file is missing after extraction: $ArchiveManifestPath -> $($entry.path)"
    }
    $item = Get-Item -LiteralPath $entryPath
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

function Test-PythonWheelTarget {
  param([string]$Filename, [string]$Target)

  if ($Target -notmatch '^(cp(?<targetDigits>\d+))-(?<targetPlatform>.+)$') {
    return $false
  }
  $targetPython = $Matches[1]
  $targetDigits = [int]$Matches['targetDigits']
  $targetPlatform = $Matches['targetPlatform']
  if ($Filename -notmatch '-(?<python>[^-]+)-(?<abi>[^-]+)-(?<platform>[^-]+)\.whl$') {
    return $false
  }
  $pythonTags = @($Matches['python'].Split('.'))
  $abiTags = @($Matches['abi'].Split('.'))
  $platformTags = @($Matches['platform'].Split('.'))
  if ($pythonTags -contains 'py3' -and $abiTags -contains 'none' -and $platformTags -contains 'any') {
    return $true
  }
  $abiCompatible = ($abiTags -contains $targetPython) -or ($abiTags -contains 'abi3') -or ($abiTags -contains 'none')
  $platformCompatible = ($platformTags -contains $targetPlatform) -or ($platformTags -contains 'any')
  $pythonCompatible = $false
  foreach ($tag in $pythonTags) {
    if ($tag -eq $targetPython -or ($tag -eq 'py3' -and $abiTags -contains 'none')) {
      $pythonCompatible = $true
      break
    }
    if ($abiTags -contains 'abi3' -and $tag -match '^cp(?<digits>\d+)$' -and [int]$Matches['digits'] -le $targetDigits) {
      $pythonCompatible = $true
      break
    }
  }
  return $pythonCompatible -and $abiCompatible -and $platformCompatible
}

function Test-PythonDependencyTargets {
  param([string]$PythonRoot)

  $pythonExe = Join-Path $PythonRoot "python.exe"
  if (-not (Test-Path -LiteralPath $pythonExe -PathType Leaf)) {
    Write-Warning "Python dependency ABI probe could not find python.exe: $pythonExe"
    return $false
  }
  $manifestRoot = Join-Path $appRoot "resources\mcp-servers"
  $manifestPaths = @(Get-ChildItem -LiteralPath $manifestRoot -Filter manifest.json -File -Recurse)
  foreach ($manifestItem in $manifestPaths) {
    $manifestPath = $manifestItem.FullName
    $tool = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($null -eq $tool.windows_python_dependencies) {
      continue
    }
    $target = [string]$tool.windows_python_dependencies.target
    if ([string]::IsNullOrWhiteSpace($target)) {
      Write-Warning "Python MCP manifest has no Windows dependency target: $manifestPath"
      return $false
    }
    $canonicalLines = @("target:$target")
    foreach ($wheel in @($tool.windows_python_dependencies.wheels)) {
      $filename = [string]$wheel.filename
      if (-not (Test-PythonWheelTarget -Filename $filename -Target $target)) {
        Write-Warning "Python wheel '$filename' is incompatible with dependency target '$target' in $manifestPath"
        return $false
      }
      $canonicalLines += "${filename}:$([string]$wheel.sha256)"
    }
    $computedLock = Get-StringSha256 -Value ($canonicalLines -join "`n")
    if ($computedLock -ne ([string]$tool.windows_python_dependencies.lock_id).ToLowerInvariant()) {
      Write-Warning "Python dependency lock does not bind the declared target and wheels in $manifestPath"
      return $false
    }
    $probe = "import platform,sys;m=platform.machine().lower();p='win_amd64' if m in ('amd64','x86_64') else 'win_'+m;a=f'cp{sys.version_info.major}{sys.version_info.minor}-{p}';raise SystemExit(0 if a==sys.argv[1] else 17)"
    & $pythonExe -I -c $probe $target
    if ($LASTEXITCODE -ne 0) {
      Write-Warning "Bundled Python ABI does not match MCP dependency target '$target' in $manifestPath"
      return $false
    }
  }
  return $true
}

function Write-TauriOverlay {
  param([string]$StageId)
  Write-Utf8Atomically -Path $generatedConfigPath -Content (Get-TauriOverlayContent -StageId $StageId)
}

function Test-VerifiedStageReusable {
  param($Manifest)

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

  if (-not (Test-PythonDependencyTargets -PythonRoot (Join-Path $stagingRoot "expanded\python"))) {
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
      if (-not (Test-PythonDependencyTargets -PythonRoot (Join-Path $expandedRoot "python"))) {
        throw "Bundled Python ABI is incompatible with the locked Windows MCP dependency wheels."
      }
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
        Remove-Item -LiteralPath $stagingRoot -Recurse -Force
      }
      Move-Item -LiteralPath $stageContext.TemporaryRoot -Destination $stagingRoot
    } finally {
      Remove-Item -LiteralPath $stageContext.TemporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
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
      Remove-Item -LiteralPath $onnxStageRoot -Recurse -Force
    }
    Move-Item -LiteralPath $temporaryRoot -Destination $onnxStageRoot
    $finalDylib = Join-Path $onnxStageRoot "onnxruntime\onnxruntime.dll"
    Write-Utf8Atomically -Path $onnxDevDescriptorPath -Content (Get-OnnxDevDescriptorContent -StageId $stageId -DylibPath $finalDylib)
  } finally {
    Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
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
