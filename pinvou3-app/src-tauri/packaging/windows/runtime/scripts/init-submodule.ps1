param(
  [Alias("ForceLfsPull")]
  [switch]$ForceDownload,
  [switch]$CacheKeyOnly,
  [switch]$OnnxOnly,
  [switch]$ImportFunctionsOnly
)

$ErrorActionPreference = "Stop"

# A Windows PowerShell 5.1 process spawned under a pwsh 7 parent (a CI step
# shell or a pwsh terminal) inherits pwsh's PSModulePath and then cannot load
# its own built-in modules (Get-FileHash). Rebuild the host default module
# path whenever the process path is missing the system module directory or
# carries pwsh entries; plain 5.1 consoles already match and are left
# untouched.
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

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..\..\..\..\..")).Path
$runtimePath = "private-runtimes/windows"
$runtimeRoot = Join-Path $repoRoot $runtimePath
$runtimeLockPath = Join-Path $repoRoot "pinvou3-app\src-tauri\config\platforms\windows\runtime\x86_64.lock.json"
$resolverCacheRoot = Join-Path $repoRoot "pinvou3-app\src-tauri\target\windows-runtime"
$resolverLockPath = Join-Path $resolverCacheRoot ".resolver.lock"
$releaseCacheRoot = Join-Path $resolverCacheRoot "release-assets"
$cacheSchema = "v6"
. (Join-Path $PSScriptRoot "release-assets.ps1")

function Invoke-Git {
  param(
    [string]$WorkingDirectory,
    [string[]]$Arguments,
    [string]$FailureMessage,
    [string]$GitCommand = "git"
  )

  try {
    $gitExecutable = [string](
      Get-Command -Name $GitCommand -CommandType Application -ErrorAction Stop |
        Select-Object -First 1 -ExpandProperty Source
    )
  } catch {
    # A missing/unresolvable executable must keep the contextual failure
    # message instead of surfacing the raw Get-Command error.
    throw "$FailureMessage Unable to resolve the Git executable '$GitCommand': $($_.Exception.Message)"
  }
  $previousErrorActionPreference = $ErrorActionPreference
  $exitCode = $null
  try {
    # Windows PowerShell 5.1 converts native stderr into ErrorRecord objects.
    # Git writes normal fetch progress to stderr, so only its exit code may
    # decide success; keep the captured stderr for diagnostics on real errors.
    $ErrorActionPreference = "Continue"
    $output = & $gitExecutable -C $WorkingDirectory @Arguments 2>&1
    $exitCode = $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $previousErrorActionPreference
  }
  if ($exitCode -ne 0) {
    throw "$FailureMessage $($output -join ' ')"
  }
  return @($output)
}

function Get-GitlinkCommit {
  $lines = Invoke-Git -WorkingDirectory $repoRoot -Arguments @("ls-files", "--stage", "--", $runtimePath) -FailureMessage "Unable to inspect the Windows runtime gitlink."
  $line = ($lines -join "`n").Trim()
  if ($line -notmatch "^160000 ([0-9a-fA-F]{40}) 0`t") {
    throw "Windows runtime gitlink is missing or unresolved: $runtimePath"
  }
  return $Matches[1].ToLowerInvariant()
}

function Get-RuntimeLockSha256 {
  if (-not (Test-Path -LiteralPath $runtimeLockPath -PathType Leaf)) {
    throw "Windows runtime lock file is missing: $runtimeLockPath"
  }
  return (Get-FileHash -LiteralPath $runtimeLockPath -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-CurrentRuntimeCommit {
  if (-not (Test-Path -LiteralPath (Join-Path $runtimeRoot ".git"))) {
    return ""
  }
  $output = & git -C $runtimeRoot rev-parse HEAD 2>$null
  if ($LASTEXITCODE -ne 0) {
    return ""
  }
  return ($output -join "`n").Trim().ToLowerInvariant()
}

function Update-RuntimeSubmoduleWithoutLfsSmudge {
  $previousSkipSmudge = [Environment]::GetEnvironmentVariable("GIT_LFS_SKIP_SMUDGE", "Process")
  try {
    [Environment]::SetEnvironmentVariable("GIT_LFS_SKIP_SMUDGE", "1", "Process")
    Invoke-Git -WorkingDirectory $repoRoot -Arguments @(
      "-c", "submodule.$runtimePath.update=checkout",
      "submodule", "update", "--init", "--checkout", "--", $runtimePath
    ) -FailureMessage "Unable to initialize the private Windows runtime submodule. Confirm repository access and retry." | Out-Null
  } finally {
    [Environment]::SetEnvironmentVariable("GIT_LFS_SKIP_SMUDGE", $previousSkipSmudge, "Process")
  }
}

function Enter-RuntimeResolverLock {
  param([int]$TimeoutSeconds = 120)

  New-Item -ItemType Directory -Path $resolverCacheRoot -Force | Out-Null
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

if ($ImportFunctionsOnly) {
  return
}

$expectedCommit = Get-GitlinkCommit
$lockSha256 = Get-RuntimeLockSha256
$cacheKey = "pinvou3-windows-runtime-$cacheSchema-$expectedCommit-$($lockSha256.Substring(0, 16))"
if ($CacheKeyOnly) {
  Write-Output $cacheKey
  exit 0
}

$resolverLock = Enter-RuntimeResolverLock
try {
  $currentCommit = Get-CurrentRuntimeCommit
  if ($currentCommit -ne $expectedCommit) {
    Update-RuntimeSubmoduleWithoutLfsSmudge

    $currentCommit = Get-CurrentRuntimeCommit
    if ($currentCommit -ne $expectedCommit) {
      throw "Windows runtime submodule did not reach the expected gitlink commit. Expected $expectedCommit, found $currentCommit."
    }
    Write-Host "Updated private Windows runtime submodule: $expectedCommit"
  } else {
    Write-Host "Reused private Windows runtime submodule checkout: $expectedCommit"
  }

  $runtimeLock = Get-Content -LiteralPath $runtimeLockPath -Raw -Encoding UTF8 | ConvertFrom-Json
  Sync-RuntimeReleaseAssets -RuntimeRoot $runtimeRoot -CacheRoot $releaseCacheRoot `
    -Lock $runtimeLock -Commit $expectedCommit -OnnxOnly:$OnnxOnly -ForceDownload:$ForceDownload

  Write-Host "Windows runtime Jenkins cache key: $cacheKey"
  Write-Host "Windows runtime verified Release cache: $releaseCacheRoot"
  Write-Output "PINVOU3_WINDOWS_RUNTIME_CACHE_KEY=$cacheKey"
} finally {
  if ($null -ne $resolverLock) {
    $resolverLock.Dispose()
  }
}
