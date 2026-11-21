$ErrorActionPreference = "Stop"

# A Windows PowerShell 5.1 process spawned under a pwsh 7 parent (a CI step
# shell or a pwsh terminal) inherits pwsh's PSModulePath and then cannot load
# its own built-in modules (Get-FileHash). Rebuild the host default module
# path before the fixture needs built-in commands; plain 5.1 consoles match and are
# left untouched.
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

$appRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$resolverPath = Join-Path $appRoot "src-tauri\packaging\windows\runtime\scripts\resolve-runtime.ps1"
$runtimeInitPath = Join-Path $appRoot "src-tauri\packaging\windows\runtime\scripts\init-submodule.ps1"
. $resolverPath -ImportFunctionsOnly
. $runtimeInitPath -ImportFunctionsOnly

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

function Test-FixtureStageReusable {
  param($Manifest)
  return Test-VerifiedStageReusable -Manifest $Manifest -PythonDependencyProbe { param($PythonRoot) $true }
}

function New-PythonDependencyTarget {
  param([string]$Target, [string]$Filename)
  $wheelSha = "a" * 64
  if ($Target -cnotmatch '^cp(?<major>\d)(?<minor>\d+)-win_amd64$') {
    throw "invalid test target: $Target"
  }
  return [pscustomobject]@{
    platform = "windows-x64"
    python = "$($Matches['major']).$($Matches['minor'])"
    imports = [object[]]@("demo")
    wheels = [object[]]@(
      [pscustomobject]@{
        name = "demo"
        version = "1"
        filename = $Filename
        url = "https://files.pythonhosted.org/packages/$Filename"
        sha256 = $wheelSha
      }
    )
  }
}

$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("pinvou3-runtime-cache-test-" + [System.Guid]::NewGuid().ToString("N"))
try {
  New-Item -ItemType Directory -Path $testRoot -Force | Out-Null
  foreach ($filename in @(
    "demo-1-cp313-cp313-win_amd64.whl",
    "demo-1-cp313-abi3-win_amd64.whl",
    "demo-1-cp312-abi3-win_amd64.whl",
    "demo-1-cp32-abi3-win_amd64.whl",
    "demo-1-cp313-none-any.whl",
    "demo-1-py313-none-any.whl",
    "demo-1-py312-none-any.whl",
    "demo-1-py313-none-win_amd64.whl",
    "demo-1-py3-none-any.whl"
  )) {
    Assert-True -Condition (Test-PythonWheelTarget -Filename $filename -Target "cp313-win_amd64") -Message "compatible PEP 425 tag should pass: $filename"
    Assert-True -Condition (Test-PythonDependencyTarget -Target (New-PythonDependencyTarget -Target "cp313-win_amd64" -Filename $filename)) -Message "valid target-bound dependency lock should pass: $filename"
  }
  foreach ($filename in @(
    "demo-1-cp27-abi3-win_amd64.whl",
    "demo-1-cp31-abi3-win_amd64.whl",
    "demo-1-cp314-abi3-win_amd64.whl",
    "demo-1-py314-none-any.whl",
    "demo-1-cp313-cp313-win_arm64.whl",
    "demo-1-CP313-cp313-win_amd64.whl",
    "demo-1-cp313-cp313-any.whl",
    "demo-1-cp313-abi3-any.whl"
  )) {
    Assert-True -Condition (-not (Test-PythonWheelTarget -Filename $filename -Target "cp313-win_amd64")) -Message "incompatible PEP 425 tag should fail: $filename"
  }
  $targetDrift = New-PythonDependencyTarget -Target "cp313-win_amd64" -Filename "demo-1-cp313-cp313-win_amd64.whl"
  $targetDrift.python = "3.14"
  Assert-True -Condition (-not (Test-PythonDependencyTarget -Target $targetDrift)) -Message "dependency lock must bind target canonically"
  $uppercaseSha = New-PythonDependencyTarget -Target "cp313-win_amd64" -Filename "demo-1-py3-none-any.whl"
  $uppercaseSha.wheels[0].sha256 = "A" * 64
  Assert-True -Condition (-not (Test-PythonDependencyTarget -Target $uppercaseSha)) -Message "wheel SHA must be lowercase hexadecimal"
  $nonStringTarget = New-PythonDependencyTarget -Target "cp313-win_amd64" -Filename "demo-1-py3-none-any.whl"
  $nonStringTarget.python = 313
  Assert-True -Condition (-not (Test-PythonDependencyTarget -Target $nonStringTarget)) -Message "dependency target must remain a JSON string"
  $emptyImports = New-PythonDependencyTarget -Target "cp313-win_amd64" -Filename "demo-1-py3-none-any.whl"
  $emptyImports.imports = [object[]]@()
  Assert-True -Condition (-not (Test-PythonDependencyTarget -Target $emptyImports)) -Message "dependency imports must be a non-empty array"
  Assert-True -Condition (Test-PythonDependencyTargets -PythonRoot $testRoot -AbiProbe { param($PythonExe, $Target) $Target.python -ceq "3.13" -and $Target.platform -ceq "windows-x64" }) -Message "manifest ABI/canonical gate should accept matching injected runtime ABI"
  Assert-True -Condition (-not (Test-PythonDependencyTargets -PythonRoot $testRoot -AbiProbe { param($PythonExe, $Target) $false })) -Message "manifest ABI gate should reject a mismatched injected runtime ABI"

  $successfulGitCommand = Join-Path $testRoot "git-success.cmd"
  $failingGitCommand = Join-Path $testRoot "git-failure.cmd"
  [System.IO.File]::WriteAllText(
    $successfulGitCommand,
    "@echo off`r`necho normal git progress 1>&2`r`nexit /b 0`r`n",
    [System.Text.Encoding]::ASCII
  )
  [System.IO.File]::WriteAllText(
    $failingGitCommand,
    "@echo off`r`necho real git failure 1>&2`r`nexit /b 7`r`n",
    [System.Text.Encoding]::ASCII
  )
  $successfulGitOutput = @(Invoke-Git -WorkingDirectory $testRoot -Arguments @("status") -FailureMessage "unexpected" -GitCommand $successfulGitCommand)
  Assert-True -Condition (($successfulGitOutput -join " ") -match "normal git progress") -Message "successful Git stderr must be retained without failing Windows PowerShell"
  $failedGitWasRejected = $false
  try {
    Invoke-Git -WorkingDirectory $testRoot -Arguments @("status") -FailureMessage "expected failure:" -GitCommand $failingGitCommand | Out-Null
  } catch {
    $failedGitWasRejected = $_.Exception.Message -match "expected failure:.*real git failure"
  }
  Assert-True -Condition $failedGitWasRejected -Message "non-zero Git exit codes must fail and retain stderr diagnostics"

  $firstGitDirectory = Join-Path $testRoot "git-first"
  $secondGitDirectory = Join-Path $testRoot "git-second"
  $duplicateGitName = "git-duplicate.cmd"
  New-Item -ItemType Directory -Path $firstGitDirectory, $secondGitDirectory -Force | Out-Null
  [System.IO.File]::WriteAllText(
    (Join-Path $firstGitDirectory $duplicateGitName),
    "@echo off`r`necho first git executable`r`nexit /b 0`r`n",
    [System.Text.Encoding]::ASCII
  )
  [System.IO.File]::WriteAllText(
    (Join-Path $secondGitDirectory $duplicateGitName),
    "@echo off`r`necho second git executable`r`nexit /b 0`r`n",
    [System.Text.Encoding]::ASCII
  )
  $previousPath = $env:PATH
  try {
    $env:PATH = "$firstGitDirectory;$secondGitDirectory;$previousPath"
    $duplicateCommands = @(Get-Command -Name "git-duplicate" -CommandType Application -ErrorAction Stop)
    Assert-True -Condition ($duplicateCommands.Count -ge 2) -Message "duplicate Git command test requires at least two PATH matches"
    $duplicateGitOutput = @(Invoke-Git -WorkingDirectory $testRoot -Arguments @("status") -FailureMessage "unexpected" -GitCommand "git-duplicate")
    Assert-True -Condition (($duplicateGitOutput -join " ") -match "first git executable") -Message "Invoke-Git must select one executable instead of concatenating duplicate PATH matches"
    Assert-True -Condition (($duplicateGitOutput -join " ") -notmatch "second git executable") -Message "Invoke-Git must use only the highest-precedence executable"
  } finally {
    $env:PATH = $previousPath
  }

  $gitFixture = Join-Path $testRoot "tracked-identity"
  New-Item -ItemType Directory -Path $gitFixture -Force | Out-Null
  foreach ($arguments in @(
    @("init", "--quiet"),
    @("config", "user.name", "Runtime Fixture"),
    @("config", "user.email", "runtime-fixture@example.invalid")
  )) { Invoke-Git -WorkingDirectory $gitFixture -Arguments $arguments -FailureMessage "Unable to initialize identity fixture" | Out-Null }
  $identityFile = Join-Path $gitFixture "tracked.txt"
  Write-TestFile -Path $identityFile -Content "original content"
  Invoke-Git -WorkingDirectory $gitFixture -Arguments @("add", "--", "tracked.txt") -FailureMessage "Unable to stage identity fixture" | Out-Null
  Invoke-Git -WorkingDirectory $gitFixture -Arguments @("-c", "commit.gpgsign=false", "-c", "core.hooksPath=.no-hooks", "commit", "--quiet", "-m", "identity fixture") -FailureMessage "Unable to commit identity fixture" | Out-Null
  Assert-RuntimeTrackedFilesClean -Root $gitFixture
  Write-TestFile -Path $identityFile -Content "modified content"
  $dirtyRejected = $false
  try { Assert-RuntimeTrackedFilesClean -Root $gitFixture } catch { $dirtyRejected = $true }
  Assert-True $dirtyRejected "runtime identity must reject unstaged tracked edits"
  Invoke-Git -WorkingDirectory $gitFixture -Arguments @("add", "--", "tracked.txt") -FailureMessage "Unable to stage modified identity fixture" | Out-Null
  $stagedRejected = $false
  try { Assert-RuntimeTrackedFilesClean -Root $gitFixture } catch { $stagedRejected = $true }
  Assert-True $stagedRejected "runtime identity must reject staged edits"
  Write-TestFile -Path $identityFile -Content "original content"
  $concealedIndexRejected = $false
  try { Assert-RuntimeTrackedFilesClean -Root $gitFixture } catch { $concealedIndexRejected = $true }
  Assert-True $concealedIndexRejected "a HEAD-equivalent worktree must not conceal staged edits"

  . (Join-Path $PSScriptRoot 'windows_runtime_lfs_behavior.ps1')

  # Behavioral coverage for the LFS relocation contract: the submodule update
  # must run with GIT_LFS_SKIP_SMUDGE=1 in effect (pointers only), so the
  # explicit `git lfs pull -c lfs.storage=...` stays the single
  # materialization step into the relocated cache, and the previous env value
  # must be restored afterwards.
  $script:lfsRecorderCalls = New-Object System.Collections.Generic.List[object]
  function Invoke-Git {
    param(
      [string]$WorkingDirectory,
      [string[]]$Arguments,
      [string]$FailureMessage,
      [string]$GitCommand = "git"
    )
    $script:lfsRecorderCalls.Add([pscustomobject]@{
      WorkingDirectory = $WorkingDirectory
      Arguments = [string[]]$Arguments
      SkipSmudge = $env:GIT_LFS_SKIP_SMUDGE
    })
    return @()
  }
  $previousLfsSkipSmudge = $env:GIT_LFS_SKIP_SMUDGE
  # Sample the variable right after the production call and BEFORE this
  # fixture's own finally restores it: an observation taken after the finally
  # would pass even if the production restore itself regressed (the fixture
  # would mask it), so the production-side restore contract needs its own
  # pre-cleanup observation. Compare with [string] coercion against the value
  # preset for the call: "unset" presents as $null on Windows but as an empty
  # string after a .NET restore on Unix, so raw equality would be
  # platform-fragile; the coercion makes both render as "" while a real
  # leftover ("1") still differs.
  $presetSkipSmudge = $null
  $postCallSkipSmudge = $null
  try {
    $env:GIT_LFS_SKIP_SMUDGE = $null
    $presetSkipSmudge = $env:GIT_LFS_SKIP_SMUDGE
    Update-RuntimeSubmoduleWithoutLfsSmudge
    $postCallSkipSmudge = $env:GIT_LFS_SKIP_SMUDGE
  } finally {
    $env:GIT_LFS_SKIP_SMUDGE = $previousLfsSkipSmudge
    # Restore the production Invoke-Git for the remainder of the test run.
    . $runtimeInitPath -ImportFunctionsOnly
  }
  Assert-True -Condition ($script:lfsRecorderCalls.Count -eq 1) -Message "the submodule update must invoke Git exactly once"
  $lfsRecorderCall = $script:lfsRecorderCalls[0]
  Assert-True -Condition ($lfsRecorderCall.SkipSmudge -ceq "1") -Message "the submodule update must run under GIT_LFS_SKIP_SMUDGE=1 so pointers are materialized only by the explicit lfs pull"
  Assert-True -Condition (@($lfsRecorderCall.Arguments) -ccontains "submodule" -and @($lfsRecorderCall.Arguments) -ccontains "update") -Message "the submodule update arguments must drive the gitlink checkout"
  Assert-True -Condition ([string]$postCallSkipSmudge -ceq [string]$presetSkipSmudge) -Message "the production submodule update must restore GIT_LFS_SKIP_SMUDGE itself (fixture cleanup must not be what masks a regression)"
  Assert-True -Condition ([string]$env:GIT_LFS_SKIP_SMUDGE -ceq [string]$previousLfsSkipSmudge) -Message "GIT_LFS_SKIP_SMUDGE must be restored after the submodule update"

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

  # Mirror the production regression: the node component's bundled npm
  # node_modules tree exceeds MAX_PATH (260) under the self-hosted
  # windows-heavy workspace roots. Fixture files must be created through the
  # \\?\ namespace — the plain provider cmdlets cannot create past-MAX_PATH
  # paths — and the deep file joins the fixture BEFORE the trusted manifest
  # is captured, so every staging/reuse assertion below walks it for real.
  $deepRelativeSegments = @(
    "maxpath-depth-01-longest",
    "maxpath-depth-02-longest",
    "maxpath-depth-03-longest",
    "maxpath-depth-04-longest",
    "maxpath-depth-05-longest",
    "maxpath-depth-06-longest",
    "maxpath-depth-07-longest"
  )
  $deepRelativePath = (@("expanded", "node") + $deepRelativeSegments + @("deep.js")) -join "/"
  $deepFilePath = Join-Path $stagingRoot ($deepRelativePath.Replace('/', '\'))
  [void][System.IO.Directory]::CreateDirectory((Get-ExtendedPath (Split-Path -Parent $deepFilePath)))
  [System.IO.File]::WriteAllText((Get-ExtendedPath $deepFilePath), "max-path deep content", [System.Text.Encoding]::UTF8)
  Assert-True -Condition ($deepFilePath.Length -gt 260) -Message "max-path fixture must exceed MAX_PATH to be meaningful (was $($deepFilePath.Length) chars)"

  Write-TauriOverlay -StageId $stageId

  # The overlay is consumed by build.js and the NSIS staging script; pin its
  # emitted shape instead of leaving the call side-effect-free.
  Assert-True -Condition (Test-Path -LiteralPath $generatedConfigPath -PathType Leaf) -Message "Write-TauriOverlay must emit tauri.generated.conf.json"
  $generatedOverlay = ConvertFrom-Json -InputObject ([System.IO.File]::ReadAllText($generatedConfigPath, [System.Text.Encoding]::UTF8))
  $overlayProperties = @($generatedOverlay.bundle.resources.PSObject.Properties)
  Assert-True -Condition ($overlayProperties.Count -gt 0) -Message "the generated runtime overlay must declare bundle.resources mappings"
  Assert-True -Condition (@($overlayProperties | Where-Object { [string]::IsNullOrEmpty([string]$_.Value) }).Count -eq 0) -Message "runtime overlay mappings must carry non-empty destinations"

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

  Assert-True -Condition (Test-FixtureStageReusable -Manifest $manifest) -Message "fresh verified staging should be reusable"

  Write-TestFile -Path $runtimeDescriptorPath -Content "tampered descriptor"
  Assert-True -Condition (-not (Test-FixtureStageReusable -Manifest $manifest)) -Message "tampered runtime descriptor must invalidate the cache"
  Write-RuntimeDescriptor -Manifest $manifest -StageId $stageId
  Assert-True -Condition (Test-FixtureStageReusable -Manifest $manifest) -Message "restoring the runtime descriptor should restore reuse"

  Write-TestFile -Path $runtimeDependencyPath -Content "xyz"
  Assert-True -Condition (-not (Test-FixtureStageReusable -Manifest $manifest)) -Message "same-size staged content tampering must invalidate the cache"

  Write-StageCacheMetadata -Root $stagingRoot
  Assert-True -Condition (-not (Test-FixtureStageReusable -Manifest $manifest)) -Message "rewriting cached content, stage manifest and verified lock together must not forge trust"

  Write-TestFile -Path $runtimeDependencyPath -Content "abc"
  Write-StageCacheMetadata -Root $stagingRoot
  Assert-True -Condition (Test-FixtureStageReusable -Manifest $manifest) -Message "restoring the verified staged content should restore reuse"

  Remove-Item -LiteralPath $runtimeDependencyPath -Force
  Assert-True -Condition (-not (Test-FixtureStageReusable -Manifest $manifest)) -Message "missing staged dependencies must invalidate the cache"

  Write-TestFile -Path $runtimeDependencyPath -Content "abc"
  Write-TestFile -Path (Join-Path $stagingRoot "expanded\poppler\unexpected.dll") -Content "unexpected"
  Assert-True -Condition (-not (Test-FixtureStageReusable -Manifest $manifest)) -Message "unexpected staged files must invalidate the cache"
  Remove-Item -LiteralPath (Join-Path $stagingRoot "expanded\poppler\unexpected.dll") -Force

  $nestedMarkerPath = Join-Path $stagingRoot "expanded\poppler\nested\.verified-lock"
  $nestedManifestPath = Join-Path $stagingRoot "expanded\poppler\nested\.stage-manifest.json"
  Write-TestFile -Path $nestedMarkerPath -Content "nested marker payload"
  Write-TestFile -Path $nestedManifestPath -Content "nested manifest payload"
  $stagedPaths = @(Get-StagedRelativePaths -Root $stagingRoot)
  Assert-True -Condition ($stagedPaths -contains "expanded/poppler/nested/.verified-lock") -Message "nested .verified-lock files must remain in the complete staged-file set"
  Assert-True -Condition ($stagedPaths -contains "expanded/poppler/nested/.stage-manifest.json") -Message "nested .stage-manifest.json files must remain in the complete staged-file set"
  Assert-True -Condition ($stagedPaths -contains $deepRelativePath) -Message "the past-MAX_PATH staged file must be enumerated by the long-path walk"
  Assert-True -Condition (-not (Test-FixtureStageReusable -Manifest $manifest)) -Message "unexpected nested metadata-named files must invalidate the cache"
  Remove-Item -LiteralPath $nestedMarkerPath -Force
  Remove-Item -LiteralPath $nestedManifestPath -Force

  $duplicateTrustedEntries = @($trustedStagedFiles) + @($trustedStagedFiles[0])
  Assert-True -Condition (-not (Test-StagedFilesAgainstTrustedManifest -Root $stagingRoot -TrustedEntries $duplicateTrustedEntries)) -Message "duplicate Windows-insensitive trusted paths must be rejected"

  # Same bytes and path modulo casing: the rejection must be
  # case-insensitive (OrdinalIgnoreCase), or two trusted entries that only
  # differ by case would stage the same file twice.
  $caseVariantTrustedEntries = @($trustedStagedFiles) + @(
    [pscustomobject]@{
      path = $bootstrapperRelativePath -creplace 'VC_redist', 'vc_REDIST'
      bytes = [long]$bootstrapperItem.Length
      sha256 = (Get-Sha256 -Path $bootstrapperSourcePath)
    }
  )
  Assert-True -Condition ($caseVariantTrustedEntries[-1].path -cne $bootstrapperRelativePath) -Message "case-variant fixture must differ from the trusted path letter-for-letter"
  Assert-True -Condition (-not (Test-StagedFilesAgainstTrustedManifest -Root $stagingRoot -TrustedEntries $caseVariantTrustedEntries)) -Message "case-insensitive duplicate trusted paths must be rejected"
  $escapingEntry = [pscustomobject]@{ path = "../escape.dll"; bytes = 0; sha256 = ("0" * 64) }
  Assert-True -Condition (-not (Test-StagedFilesAgainstTrustedManifest -Root $stagingRoot -TrustedEntries @($escapingEntry))) -Message "trusted paths escaping the staging root must be rejected"

  # Expand-FlattenedRuntime must survive destination trees past MAX_PATH:
  # the flattened copy arm (robocopy) is what unbreaks the node component on
  # the self-hosted windows-heavy runners. The zip carries node.exe plus a
  # deep chain mirroring npm's bundled node_modules: the deep entry itself
  # lands ~290 chars into TEMP, so extraction goes through the \\?\ long-path
  # channel, and the flattened destination crosses MAX_PATH for the robocopy
  # arm — together they exercise the exact production geometry.
  Add-Type -AssemblyName System.IO.Compression
  $flatRoot = Join-Path $testRoot "flatten"
  $flatDestination = Join-Path $testRoot "flatten\expanded\node"
  # The node-prefixed form is the ZIP ENTRY name only; Expand-FlattenedRuntime
  # flattens the archive's top-level directory away (node.exe lands at the
  # destination root), mirroring the production node zip's single top-level
  # directory.
  $flatZipEntryDeepPath = (@("node") + $deepRelativeSegments + @("deep.js")) -join "\"
  $flatZipPath = Join-Path $flatRoot "node-runtime.zip"
  [void][System.IO.Directory]::CreateDirectory((Get-ExtendedPath $flatRoot))
  $zipStream = [System.IO.File]::Create($flatZipPath)
  $zipArchive = [System.IO.Compression.ZipArchive]::new($zipStream, [System.IO.Compression.ZipArchiveMode]::Create)
  try {
    foreach ($zipEntry in @(
      [pscustomobject]@{ path = "node/node.exe"; content = "node exe stub" },
      [pscustomobject]@{ path = ($flatZipEntryDeepPath.Replace('\', '/')); content = "flattened deep payload" }
    )) {
      $entry = $zipArchive.CreateEntry($zipEntry.path)
      $entryWriter = [System.IO.StreamWriter]::new($entry.Open())
      try {
        $entryWriter.Write($zipEntry.content)
      } finally {
        $entryWriter.Dispose()
      }
    }
  } finally {
    $zipArchive.Dispose()
    $zipStream.Dispose()
  }
  Expand-FlattenedRuntime -ZipPath $flatZipPath -Destination $flatDestination -RequiredFile "node.exe"
  # Success-boundary regression (review 5796830161): robocopy reports success
  # with exit codes 0-7, and the resolver used to leave those codes in
  # $LASTEXITCODE on the success path. stage-runtime.ps1 and
  # stage-onnx-runtime.ps1 forward $LASTEXITCODE as their process exit code
  # ("if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }"), so a verified staging
  # still failed the Windows dev/build chain in scripts/tauri/windows-runtime.js.
  # Every real robocopy arm exercised below must finish at zero.
  Assert-True -Condition ($LASTEXITCODE -eq 0) -Message "successful flattened runtime expansion (robocopy /E) must leave the process-visible exit code at zero"
  # Destination-relative path WITHOUT the "node" prefix: the flatten semantics
  # strip the archive's top-level directory (the same geometry as the real
  # node-v*-win-x64.zip whose staged manifest puts node.exe at the destination
  # root).
  $flatDeepPath = Join-Path $flatDestination ((@($deepRelativeSegments) -join '\') + '\deep.js')
  Assert-True -Condition ($flatDeepPath.Length -gt 260) -Message "flattened expansion destination must exceed MAX_PATH to exercise robocopy (was $($flatDeepPath.Length) chars)"
  Assert-True -Condition ([System.IO.File]::ReadAllText((Get-ExtendedPath $flatDeepPath)) -eq "flattened deep payload") -Message "flattened runtime expansion must carry past-MAX_PATH files through robocopy intact"
  Assert-True -Condition (Test-Path -LiteralPath (Join-Path $flatDestination "node.exe") -PathType Leaf) -Message "flattened runtime expansion must stage the required file at the destination root"
  Assert-True -Condition (@(Get-ChildItem -LiteralPath $flatDestination -Force).Count -eq 2) -Message "flattened runtime expansion must produce exactly the archive root entries"

  # Assert-ChildPath refuses '..' traversal segments outright (GetFullPath
  # skips normalization on \\?\ device paths, so an interior '..' would not
  # collapse before the containment comparison): an archive entry carrying
  # traversal segments must fail the extraction instead of escaping the
  # destination.
  $slipRoot = Join-Path $testRoot "slip"
  [void][System.IO.Directory]::CreateDirectory((Get-ExtendedPath $slipRoot))
  $slipZipPath = Join-Path $slipRoot "slip.zip"
  $zipStream = [System.IO.File]::Create($slipZipPath)
  $zipArchive = [System.IO.Compression.ZipArchive]::new($zipStream, [System.IO.Compression.ZipArchiveMode]::Create)
  try {
    $slipEntry = $zipArchive.CreateEntry("node/../../slip-escape.txt")
    $slipWriter = [System.IO.StreamWriter]::new($slipEntry.Open())
    try {
      $slipWriter.Write("slip payload")
    } finally {
      $slipWriter.Dispose()
    }
  } finally {
    $zipArchive.Dispose()
    $zipStream.Dispose()
  }
  $slipThrew = $false
  try {
    Expand-ZipArchiveLongPath -ZipPath $slipZipPath -Destination $slipRoot
  } catch {
    $slipThrew = $true
  }
  Assert-True -Condition $slipThrew -Message "an archive entry with traversal segments must fail extraction instead of escaping the destination"
  Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $testRoot "slip-escape.txt"))) -Message "traversal-entry extraction must not write outside the destination root"

  # Remove-TreeLongPath must delete trees holding past-MAX_PATH files and
  # tolerate a missing target under -MissingOk (the staging finally blocks).
  $removalRoot = Join-Path $testRoot "removal"
  $removalDeepPath = Join-Path $removalRoot ((@($deepRelativeSegments) -join '\') + '\deep.js')
  [void][System.IO.Directory]::CreateDirectory((Get-ExtendedPath (Split-Path -Parent $removalDeepPath)))
  [System.IO.File]::WriteAllText((Get-ExtendedPath $removalDeepPath), "removal payload", [System.Text.Encoding]::UTF8)
  Remove-TreeLongPath -Path $removalRoot
  Assert-True -Condition (-not (Test-Path -LiteralPath $removalRoot)) -Message "long-path cleanup must remove a tree holding past-MAX_PATH files"
  Assert-True -Condition ($LASTEXITCODE -eq 0) -Message "successful long-path cleanup (robocopy /MIR) must leave the process-visible exit code at zero"
  Remove-TreeLongPath -Path $removalRoot -MissingOk

  # Robocopy exit codes >= 8 must stay fatal: a PATH-stubbed robocopy that
  # reports 8 (the same stub technique as the Git failure fixtures above) has
  # to fail both long-path arms through their real error messages, so the
  # success-boundary reset cannot swallow genuine robocopy failures.
  $robocopyStubDirectory = Join-Path $testRoot "robocopy-stub"
  New-Item -ItemType Directory -Path $robocopyStubDirectory -Force | Out-Null
  [System.IO.File]::WriteAllText(
    (Join-Path $robocopyStubDirectory "robocopy.cmd"),
    # The stub stays silent on stderr: Windows PowerShell 5.1 turns piped
    # native stderr into ErrorRecord objects and $ErrorActionPreference Stop
    # would raise NativeCommandError at the robocopy call itself, masking the
    # exit-code classification this section has to exercise.
    "@echo off`r`nexit /b 8`r`n",
    [System.Text.Encoding]::ASCII
  )
  # Claim leftover extraction trees by the resolver's own cleanup-failure
  # record instead of diffing pinvou3-runtime-expand-* names in shared TEMP:
  # a before/after name diff can swallow a concurrent run's active extraction
  # tree on a self-hosted runner, while the record names exactly the trees
  # this run created and failed to clean.
  $script:RuntimeExpandCleanupFailures = @()
  $previousRobocopyPath = $env:PATH
  try {
    $env:PATH = "$robocopyStubDirectory;$previousRobocopyPath"
    Assert-True -Condition ((@(Get-Command -Name "robocopy" -CommandType Application -ErrorAction Stop)[0]).Source -like "$robocopyStubDirectory*") -Message "the robocopy failure stub must shadow the real robocopy on PATH"
    $removalStubRoot = Join-Path $testRoot "removal-stub"
    New-Item -ItemType Directory -Path (Join-Path $removalStubRoot "payload") -Force | Out-Null
    [System.IO.File]::WriteAllText((Join-Path $removalStubRoot "payload\payload.txt"), "stub payload", [System.Text.Encoding]::UTF8)
    $mirCleanupThrew = $false
    try {
      Remove-TreeLongPath -Path $removalStubRoot
    } catch {
      $mirCleanupThrew = $_.Exception.Message -match "Robocopy /MIR cleanup failed with exit code 8"
    }
    Assert-True -Condition $mirCleanupThrew -Message "robocopy exit code 8 must fail the /MIR cleanup arm with its real error message"
    $flattenedCopyThrew = $false
    try {
      Expand-FlattenedRuntime -ZipPath $flatZipPath -Destination (Join-Path $testRoot "flatten-stub\expanded\node") -RequiredFile "node.exe"
    } catch {
      $flattenedCopyThrew = $_.Exception.Message -match "Robocopy failed with exit code 8"
    }
    Assert-True -Condition $flattenedCopyThrew -Message "robocopy exit code 8 must fail the flattened copy arm with its real error message"
  } finally {
    $env:PATH = $previousRobocopyPath
    # Guaranteed claiming: an earlier assertion throwing must not skip the
    # recorded extraction trees (their deep entries need the long-path arm,
    # so plain Remove-Item would leave past-MAX_PATH litter behind). The
    # stub's robocopy would fail every claim, so this runs only after PATH
    # is restored above.
    foreach ($expandFailure in @($script:RuntimeExpandCleanupFailures)) {
      Remove-TreeLongPath -Path $expandFailure.Path -MissingOk
    }
  }
  # The stubbed flattened copy fails inside Expand-FlattenedRuntime, whose
  # cleanup failures degrade to a loud warning: the finally above already
  # claimed this run's recorded trees; what remains here is asserting the
  # recording itself stayed exact and the claiming actually removed them
  # (the second pass is a no-op under -MissingOk).
  $expandCleanupFailures = @($script:RuntimeExpandCleanupFailures)
  Assert-True -Condition ($expandCleanupFailures.Count -ge 1) -Message "the stubbed extraction cleanup failure must be recorded for precise claiming"
  foreach ($expandFailure in $expandCleanupFailures) {
    Assert-True -Condition ((Split-Path -Leaf $expandFailure.Path) -like "pinvou3-runtime-expand-*") -Message "recorded extraction cleanup failures must point at pinvou3-runtime-expand-* trees: $($expandFailure.Path)"
    Remove-TreeLongPath -Path $expandFailure.Path -MissingOk
    Assert-True -Condition (-not (Test-Path -LiteralPath $expandFailure.Path)) -Message "claiming a recorded extraction tree must actually remove it: $($expandFailure.Path)"
  }

  # Explicit \\?\-prefixed roots and children must reach containment checks in
  # the same namespace: GetFullPath keeps the prefix on the root while the
  # child chain strips it, so an explicitly extended RuntimeRoot used to be
  # mis-rejected as "outside the expected root" for its own children.
  $nsRoot = Join-Path $testRoot "ns-unified"
  $nsChild = Join-Path $nsRoot "child\file.txt"
  Write-TestFile -Path $nsChild -Content "ns"
  $nsExtendedRoot = Get-ExtendedPath $nsRoot
  $nsExtendedChild = Get-ExtendedPath $nsChild
  $nsAcceptThrew = $false
  try { Assert-ChildPath -Root $nsExtendedRoot -Path $nsChild } catch { $nsAcceptThrew = $true }
  Assert-True -Condition (-not $nsAcceptThrew) -Message "an extended RuntimeRoot must not be mis-rejected for a contained plain child"
  $nsAcceptThrew = $false
  try { Assert-ChildPath -Root $nsExtendedRoot -Path $nsExtendedChild } catch { $nsAcceptThrew = $true }
  Assert-True -Condition (-not $nsAcceptThrew) -Message "an extended RuntimeRoot must not be mis-rejected for a contained extended child"
  $nsOutsidePath = Join-Path $testRoot "ns-outside.txt"
  $nsRejectThrew = $false
  try { Assert-ChildPath -Root $nsExtendedRoot -Path $nsOutsidePath } catch { $nsRejectThrew = $_.Exception.Message -match "outside the expected root" }
  Assert-True -Condition $nsRejectThrew -Message "an extended RuntimeRoot must still reject a path outside it"
  Assert-True -Condition ((Get-RelativeStagePath -Root $nsExtendedRoot -Path $nsExtendedChild) -eq (Get-RelativeStagePath -Root $nsRoot -Path $nsChild)) -Message "extended and plain root/path pairs must yield the same relative staging path"

  # The staging descriptor key must be the exact slash-joined path: the root
  # normalization appends the trailing separator through expression
  # evaluation, because in command parsing mode a bare command call binds the
  # trailing "+ '\'" as additional arguments (silently absorbed by a simple
  # function) and the returned key used to gain a leading slash that the Node
  # descriptor consumer rejects as an absolute path. Pin the production key
  # shape (Get-RelativeStagePath consumers hand it straight to
  # scripts/tauri/windows-runtime.js), and pin Assert-ChildPath's containment
  # boundary, whose root also carries the appended separator: without it the
  # containment check degrades to a plain prefix match.
  $relativeRoot = Join-Path $testRoot "relative-key\src-tauri"
  $relativeChild = Join-Path $relativeRoot "target\windows-runtime\dcde8f01abcd-0123456789ab-onnx-dev\onnxruntime\onnxruntime.dll"
  Write-TestFile -Path $relativeChild -Content "dylib"
  $relativeKey = Get-RelativeStagePath -Root $relativeRoot -Path $relativeChild
  Assert-True -Condition ($relativeKey -eq "target/windows-runtime/dcde8f01abcd-0123456789ab-onnx-dev/onnxruntime/onnxruntime.dll") -Message "the relative staging key must be the exact slash-joined production shape without a leading separator: $relativeKey"
  $boundaryRoot = Join-Path $testRoot "containment-bound"
  New-Item -ItemType Directory -Path (Join-Path $boundaryRoot "inner") -Force | Out-Null
  $boundarySibling = Join-Path (Join-Path $testRoot "containment-boundary") "sibling.txt"
  Write-TestFile -Path $boundarySibling -Content "sibling"
  $boundaryThrew = $false
  try { Assert-ChildPath -Root $boundaryRoot -Path $boundarySibling } catch { $boundaryThrew = $_.Exception.Message -match "outside the expected root" }
  Assert-True -Condition $boundaryThrew -Message "a sibling directory sharing the root name as a prefix must not pass the containment check"

  # Write-Utf8Atomically keeps shared-visibility descriptors continuously
  # present for the unlocked readers in scripts/tauri/windows-runtime.js:
  # replacement must swap the file in atomically, a failed replacement must
  # preserve the previous generation and leave no temp litter behind.
  $atomicRoot = Join-Path $testRoot "atomic"
  New-Item -ItemType Directory -Path $atomicRoot | Out-Null
  $atomicPath = Join-Path $atomicRoot "descriptor.json"
  Write-Utf8Atomically -Path $atomicPath -Content "first"
  Assert-True -Condition ([System.IO.File]::ReadAllText($atomicPath) -eq "first") -Message "atomic write must create the destination with the requested content"
  foreach ($generation in @("second", "third")) {
    Write-Utf8Atomically -Path $atomicPath -Content $generation
    Assert-True -Condition ([System.IO.File]::ReadAllText($atomicPath) -eq $generation) -Message "atomic replacement must swap in the new content in one operation"
  }
  # A plain .NET reader opens the descriptor without FILE_SHARE_DELETE, which
  # blocks ReplaceFile: the writer must fail loudly instead of deleting the
  # destination first (Move-Item -Force semantics).
  $sharingReader = [System.IO.File]::Open(
    $atomicPath,
    [System.IO.FileMode]::Open,
    [System.IO.FileAccess]::Read,
    [System.IO.FileShare]::Read
  )
  try {
    $replacedUnderReader = $false
    try {
      Write-Utf8Atomically -Path $atomicPath -Content "interrupted"
      $replacedUnderReader = $true
    } catch {
    }
    Assert-True -Condition (-not $replacedUnderReader) -Message "atomic replace under a sharing reader must fail loudly instead of deleting the destination"
  } finally {
    $sharingReader.Dispose()
  }
  Assert-True -Condition ([System.IO.File]::ReadAllText($atomicPath) -eq "third") -Message "a failed atomic replacement must preserve the previous descriptor"
  $tempLitter = @(Get-ChildItem -LiteralPath $atomicRoot -Filter ".descriptor.json.tmp-*" -Force)
  Assert-True -Condition ($tempLitter.Count -eq 0) -Message "atomic replacement left temp files behind: $($tempLitter.Count)"
  Write-Utf8Atomically -Path $atomicPath -Content "fourth"
  Assert-True -Condition ([System.IO.File]::ReadAllText($atomicPath) -eq "fourth") -Message "atomic replacement must succeed again once the reader is gone"

  Write-Host "Windows runtime cache behavior: ok"
} finally {
  # The fixture tree holds past-MAX_PATH files; plain Remove-Item -Recurse
  # would silently leave deep litter behind on the runner (the resolver's own
  # cleanup goes through the long-path channel).
  Remove-TreeLongPath -Path $testRoot -MissingOk
}
