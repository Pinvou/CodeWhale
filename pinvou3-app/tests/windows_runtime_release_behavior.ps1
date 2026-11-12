$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\src-tauri\packaging\windows\runtime\scripts\release-assets.ps1')
function Assert-True { param([bool]$Condition, [string]$Message) if (-not $Condition) { throw $Message } }
function Assert-Rejected {
  param([scriptblock]$Action, [string]$Message)
  $rejected = $false
  try { & $Action } catch { $rejected = $true }
  Assert-True $rejected $Message
}
function Write-FixtureManifest {
  [IO.File]::WriteAllText($manifestPath, ($fixture | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
  $lock.manifest.sha256 = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
}
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('pinvou-runtime-release-test-' + [Guid]::NewGuid().ToString('N'))
$runtime = Join-Path $testRoot 'runtime'
$cache = Join-Path $testRoot 'cache'
$sources = Join-Path $testRoot 'sources'
$manifestPath = Join-Path $runtime 'windows-runtime.manifest.json'
$commit = 'c' * 40
$script:downloadCount = 0
try {
  New-Item -ItemType Directory -Path $runtime, $sources -Force | Out-Null
  $fixture = [pscustomobject]@{ schemaVersion = 2; target = 'windows-x86_64'; files = @() }
  foreach ($name in @('a.zip', 'onnx-runtime.zip')) {
    $source = Join-Path $sources $name
    [IO.File]::WriteAllText($source, ('runtime bytes for ' + $name), [Text.Encoding]::ASCII)
    $fixture.files += [pscustomobject]@{
      path = 'payload/' + $name
      component = $(if ($name -eq 'a.zip') { 'demo' } else { 'onnxruntime' })
      bytes = (Get-Item -LiteralPath $source).Length
      sha256 = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
    }
  }
  $lock = [pscustomobject]@{
    source = [pscustomobject]@{ url = 'https://github.com/example/runtime.git'; commit = $commit }
    manifest = [pscustomobject]@{ path = 'windows-runtime.manifest.json'; sha256 = '' }
  }
  Write-FixtureManifest
  $download = {
    param($Uri, $Destination)
    Assert-True ($Uri.AbsoluteUri.StartsWith("https://github.com/example/runtime/releases/download/runtime-$commit/")) 'download must use the commit-bound repository Release'
    $script:downloadCount++
    [IO.File]::Copy((Join-Path $sources ([IO.Path]::GetFileName($Uri.AbsolutePath))), $Destination)
  }
  $argsForSync = @{ RuntimeRoot = $runtime; CacheRoot = $cache; Lock = $lock; Commit = $commit; Download = $download }
  Sync-RuntimeReleaseAssets @argsForSync
  Assert-True ($script:downloadCount -eq 2) 'fresh initialization must download exactly the two locked files'
  foreach ($entry in $fixture.files) { Assert-True (Test-RuntimeReleaseFile (Join-Path $runtime $entry.path) $entry) 'downloaded payload must match the lock' }
  Sync-RuntimeReleaseAssets @argsForSync
  Assert-True ($script:downloadCount -eq 2) 'valid workspace must initialize offline'

  $first = $fixture.files[0]; $target = Join-Path $runtime $first.path
  [IO.File]::WriteAllText($target, ('x' * $first.bytes), [Text.Encoding]::ASCII)
  Sync-RuntimeReleaseAssets @argsForSync
  Assert-True ((Test-RuntimeReleaseFile $target $first) -and $script:downloadCount -eq 2) 'same-size workspace corruption must be repaired from verified cache'
  $cached = Join-Path $cache $first.sha256
  [IO.File]::WriteAllText($cached, ('x' * $first.bytes), [Text.Encoding]::ASCII)
  Remove-Item -LiteralPath $target
  Sync-RuntimeReleaseAssets @argsForSync
  Assert-True ((Test-RuntimeReleaseFile $target $first) -and $script:downloadCount -eq 3) 'corrupt cache must trigger a hash-checked download'

  $badDownload = { param($Uri, $Destination) [IO.File]::WriteAllText($Destination, ('x' * $first.bytes), [Text.Encoding]::ASCII) }
  $badArgs = $argsForSync.Clone(); $badArgs.Download = $badDownload
  Assert-Rejected { Sync-RuntimeReleaseAssets @badArgs -ForceDownload } 'wrong downloaded hash must fail closed'
  Assert-True (Test-RuntimeReleaseFile $target $first) 'failed replacement must preserve the verified workspace file'
  Assert-True (@(Get-ChildItem -LiteralPath $cache -Filter '*.download-*').Count -eq 0) 'failed downloads must leave no temporary payloads'
  $unavailableArgs = $argsForSync.Clone(); $unavailableArgs.Download = { throw 'HTTP 404' }
  Assert-Rejected { Sync-RuntimeReleaseAssets @unavailableArgs -ForceDownload } 'missing Release assets must fail without falling back to LFS'

  foreach ($entry in $fixture.files) { Remove-Item -LiteralPath (Join-Path $runtime $entry.path) }
  $onnx = $fixture.files[1]; Remove-Item -LiteralPath (Join-Path $cache $onnx.sha256)
  Sync-RuntimeReleaseAssets @argsForSync -OnnxOnly
  Assert-True ((Test-RuntimeReleaseFile (Join-Path $runtime $onnx.path) $onnx) -and -not (Test-Path -LiteralPath $target) -and $script:downloadCount -eq 4) 'ONNX-only initialization must not materialize unrelated payloads'

  $before = $script:downloadCount
  $fixture.files[0].path = 'payload/../../escape.zip'; Write-FixtureManifest
  Assert-Rejected { Sync-RuntimeReleaseAssets @argsForSync } 'manifest traversal must fail before downloading'
  Assert-True ($script:downloadCount -eq $before) 'invalid manifest must not cause network requests'
  $fixture.files[0].path = 'payload/a.zip'; Write-FixtureManifest
  $fixture.files[1].path = 'payload/subdir/a.zip'; Write-FixtureManifest
  Assert-Rejected { Sync-RuntimeReleaseAssets @argsForSync } 'ambiguous Release basenames must be rejected'
  $fixture.files[1].path = 'payload/onnx-runtime.zip'; Write-FixtureManifest
  $lock.manifest.sha256 = '0' * 64
  Assert-Rejected { Sync-RuntimeReleaseAssets @argsForSync } 'modified manifest must fail against the parent lock'
  Write-FixtureManifest
  $lock.source.commit = 'd' * 40
  Assert-Rejected { Sync-RuntimeReleaseAssets @argsForSync } 'gitlink and lock commit mismatch must fail'
  $lock.source.commit = $commit
  $junctionTarget = Join-Path $testRoot 'junction-target'
  $junctionCache = Join-Path $testRoot 'linked-cache'
  New-Item -ItemType Directory -Path $junctionTarget -Force | Out-Null
  New-Item -ItemType Junction -Path $junctionCache -Target $junctionTarget | Out-Null
  try {
    $junctionArgs = $argsForSync.Clone(); $junctionArgs.CacheRoot = $junctionCache
    Assert-Rejected { Sync-RuntimeReleaseAssets @junctionArgs } 'cache junction must not redirect runtime writes'
  } finally { [IO.Directory]::Delete($junctionCache) }
  foreach ($uri in @('http://github.com/example/a', 'https://example.invalid/a', 'https://user:secret@github.com/a', 'https://github.com:8443/a')) {
    Assert-Rejected { Assert-RuntimeReleaseUri ([Uri]$uri) } 'untrusted or downgraded download endpoint must fail'
  }
  Assert-RuntimeReleaseUri ([Uri]'https://release-assets.githubusercontent.com/example')
  Write-Host 'Windows runtime Release download, integrity, offline reuse, corruption repair and failure boundaries passed.'
} finally {
  $resolved = [IO.Path]::GetFullPath($testRoot)
  if ([IO.Path]::GetDirectoryName($resolved) -ne [IO.Path]::GetTempPath().TrimEnd('\', '/') -or [IO.Path]::GetFileName($resolved) -notlike 'pinvou-runtime-release-test-*') { throw 'Unexpected fixture cleanup path.' }
  if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
