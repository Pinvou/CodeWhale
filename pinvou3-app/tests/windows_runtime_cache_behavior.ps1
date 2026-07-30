$ErrorActionPreference = "Stop"

$appRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$resolverPath = Join-Path $appRoot "src-tauri\packaging\windows\runtime\scripts\resolve-runtime.ps1"
. $resolverPath -ImportFunctionsOnly

function Assert-True {
  param([bool]$Condition, [string]$Message)
  if (-not $Condition) {
    throw $Message
  }
}

function Write-TestFile {
  param([string]$Path, [string]$Content)
  New-Item -ItemType Directory -Path (Split-Path -Parent $Path) -Force | Out-Null
  [System.IO.File]::WriteAllText($Path, $Content, [System.Text.Encoding]::UTF8)
}

function Write-StageCacheMetadata {
  param([string]$Root)
  $writtenManifestPath = Write-StageManifest -Root $Root
  $manifestSha256 = Get-Sha256 -Path $writtenManifestPath
  Write-Utf8WithoutBom -Path (Join-Path $Root ".verified-lock") -Content (Get-VerificationMarkerContent -StageManifestSha256 $manifestSha256)
}

$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("pinvou3-runtime-cache-test-" + [System.Guid]::NewGuid().ToString("N"))
try {
  $stagingParent = Join-Path $testRoot "windows-runtime"
  $stagingRoot = Join-Path $stagingParent $stageId
  $stageManifestPath = Join-Path $stagingRoot ".stage-manifest.json"
  $generatedConfigPath = Join-Path $stagingParent "tauri.generated.conf.json"
  $runtimeDescriptorPath = Join-Path $stagingParent "runtime-descriptor.json"
  $resolverLockPath = Join-Path $stagingParent ".resolver.lock"

  $firstLock = Enter-ResolverLock -TimeoutSeconds 0
  try {
    $secondLockWasRejected = $false
    try {
      $unexpectedLock = Enter-ResolverLock -TimeoutSeconds 0
      $unexpectedLock.Dispose()
    } catch {
      $secondLockWasRejected = $true
    }
    Assert-True -Condition $secondLockWasRejected -Message "concurrent resolver access must be rejected while the cache lock is held"
  } finally {
    $firstLock.Dispose()
  }
  $releasedLock = Enter-ResolverLock -TimeoutSeconds 0
  $releasedLock.Dispose()

  $bootstrapperRelativePath = "payload/vc_redist/VC_redist.x64.exe"
  $bootstrapperSourcePath = Join-Path $stagingRoot $bootstrapperRelativePath.Replace('/', '\')
  $runtimeExecutablePath = Join-Path $stagingRoot "expanded\poppler\pdftotext.exe"
  $runtimeDependencyPath = Join-Path $stagingRoot "expanded\poppler\support.dll"
  Write-TestFile -Path $bootstrapperSourcePath -Content "bootstrapper"
  Write-TestFile -Path $runtimeExecutablePath -Content "executable"
  Write-TestFile -Path $runtimeDependencyPath -Content "abc"

  $bootstrapperPath = Join-Path $stagingParent "nsis\vc_redist\VC_redist.x64.exe"
  Write-TestFile -Path $bootstrapperPath -Content "bootstrapper"
  Write-TauriOverlay -StageId $stageId

  $trustedStagedFiles = @(Get-StagedRelativeFiles -Root $stagingRoot)
  Write-StageCacheMetadata -Root $stagingRoot

  $bootstrapperItem = Get-Item -LiteralPath $bootstrapperSourcePath
  $manifest = [pscustomobject]@{
    stagedFiles = $trustedStagedFiles
    files = @(
      [pscustomobject]@{
        path = $bootstrapperRelativePath
        bytes = [long]$bootstrapperItem.Length
        sha256 = Get-Sha256 -Path $bootstrapperSourcePath
      }
    )
  }
  Write-RuntimeDescriptor -Manifest $manifest -StageId $stageId

  Assert-True -Condition (Test-VerifiedStageReusable -Manifest $manifest) -Message "fresh verified staging should be reusable"

  Write-TestFile -Path $runtimeDescriptorPath -Content "tampered descriptor"
  Assert-True -Condition (-not (Test-VerifiedStageReusable -Manifest $manifest)) -Message "tampered runtime descriptor must invalidate the cache"
  Write-RuntimeDescriptor -Manifest $manifest -StageId $stageId
  Assert-True -Condition (Test-VerifiedStageReusable -Manifest $manifest) -Message "restoring the runtime descriptor should restore reuse"

  Write-TestFile -Path $runtimeDependencyPath -Content "xyz"
  Assert-True -Condition (-not (Test-VerifiedStageReusable -Manifest $manifest)) -Message "same-size staged content tampering must invalidate the cache"

  Write-StageCacheMetadata -Root $stagingRoot
  Assert-True -Condition (-not (Test-VerifiedStageReusable -Manifest $manifest)) -Message "rewriting cached content, stage manifest and verified lock together must not forge trust"

  Write-TestFile -Path $runtimeDependencyPath -Content "abc"
  Write-StageCacheMetadata -Root $stagingRoot
  Assert-True -Condition (Test-VerifiedStageReusable -Manifest $manifest) -Message "restoring the verified staged content should restore reuse"

  Remove-Item -LiteralPath $runtimeDependencyPath -Force
  Assert-True -Condition (-not (Test-VerifiedStageReusable -Manifest $manifest)) -Message "missing staged dependencies must invalidate the cache"

  Write-TestFile -Path $runtimeDependencyPath -Content "abc"
  Write-TestFile -Path (Join-Path $stagingRoot "expanded\poppler\unexpected.dll") -Content "unexpected"
  Assert-True -Condition (-not (Test-VerifiedStageReusable -Manifest $manifest)) -Message "unexpected staged files must invalidate the cache"
  Remove-Item -LiteralPath (Join-Path $stagingRoot "expanded\poppler\unexpected.dll") -Force

  $nestedMarkerPath = Join-Path $stagingRoot "expanded\poppler\nested\.verified-lock"
  $nestedManifestPath = Join-Path $stagingRoot "expanded\poppler\nested\.stage-manifest.json"
  Write-TestFile -Path $nestedMarkerPath -Content "nested marker payload"
  Write-TestFile -Path $nestedManifestPath -Content "nested manifest payload"
  $stagedPaths = @(Get-StagedRelativePaths -Root $stagingRoot)
  Assert-True -Condition ($stagedPaths -contains "expanded/poppler/nested/.verified-lock") -Message "nested .verified-lock files must remain in the complete staged-file set"
  Assert-True -Condition ($stagedPaths -contains "expanded/poppler/nested/.stage-manifest.json") -Message "nested .stage-manifest.json files must remain in the complete staged-file set"
  Assert-True -Condition (-not (Test-VerifiedStageReusable -Manifest $manifest)) -Message "unexpected nested metadata-named files must invalidate the cache"
  Remove-Item -LiteralPath $nestedMarkerPath -Force
  Remove-Item -LiteralPath $nestedManifestPath -Force

  $duplicateTrustedEntries = @($trustedStagedFiles) + @($trustedStagedFiles[0])
  Assert-True -Condition (-not (Test-StagedFilesAgainstTrustedManifest -Root $stagingRoot -TrustedEntries $duplicateTrustedEntries)) -Message "duplicate Windows-insensitive trusted paths must be rejected"
  $escapingEntry = [pscustomobject]@{ path = "../escape.dll"; bytes = 0; sha256 = ("0" * 64) }
  Assert-True -Condition (-not (Test-StagedFilesAgainstTrustedManifest -Root $stagingRoot -TrustedEntries @($escapingEntry))) -Message "trusted paths escaping the staging root must be rejected"

  Write-Host "Windows runtime cache behavior: ok"
} finally {
  Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}
