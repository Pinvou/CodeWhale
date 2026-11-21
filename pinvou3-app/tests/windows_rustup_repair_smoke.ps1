param(
  [string]$Toolchain
)

$ErrorActionPreference = "Stop"

# BEGIN rustup-smoke-diagnostic:helpers
# Test-only observations: no raw errors, environment values or paths are emitted.
$script:rustupSmokeDiagnostic = $null
try {
  $script:rustupSmokeDiagnostic = @{
    Clock = [Diagnostics.Stopwatch]::StartNew()
    Phase = "channel"
    PhaseStarted = 0L
    Cleanup = "not_started"
  }
} catch { }

function Set-RustupSmokePhase {
  param([string]$Phase)
  try {
    if ($null -ne $script:rustupSmokeDiagnostic) {
      $script:rustupSmokeDiagnostic.Phase = $Phase
      $script:rustupSmokeDiagnostic.PhaseStarted = $script:rustupSmokeDiagnostic.Clock.ElapsedMilliseconds
    }
  } catch { }
}

function Set-RustupSmokeCleanup {
  param([string]$Stage)
  try {
    if ($null -ne $script:rustupSmokeDiagnostic) {
      $script:rustupSmokeDiagnostic.Cleanup = $Stage
    }
  } catch { }
}

function Format-RustupSmokeDiagnostic {
  param([string]$Kind, [string]$Outcome = "unknown")
  try {
    $phase = "unknown"
    $cleanup = "unknown"
    $elapsed = 0L
    $phaseElapsed = 0L
    if ($null -ne $script:rustupSmokeDiagnostic) {
      $allowedPhases = @(
        "channel", "rustup_home", "toolchain_lookup", "account_check",
        "refusal_guards", "copy_toolchain", "copy_settings", "corrupt_binaries",
        "incomplete_probe", "repair", "repair_evidence", "binary_verify",
        "stdlib_location", "stdlib_remove", "stdlib_probe", "complete"
      )
      if ($script:rustupSmokeDiagnostic.Phase -cin $allowedPhases) {
        $phase = $script:rustupSmokeDiagnostic.Phase
      }
      if ($script:rustupSmokeDiagnostic.Cleanup -cin @(
        "not_started", "restore_environment", "check_root", "remove_requested", "finished"
      )) {
        $cleanup = $script:rustupSmokeDiagnostic.Cleanup
      }
      $now = $script:rustupSmokeDiagnostic.Clock.ElapsedMilliseconds
      $elapsed = [Math]::Min(86400000L, [Math]::Max(0L, $now))
      $phaseElapsed = [Math]::Min(86400000L, [Math]::Max(
        0L, $now - $script:rustupSmokeDiagnostic.PhaseStarted
      ))
    }
    if ($Kind -cnotin @("failure", "cleanup")) { $Kind = "unknown" }
    if ($Outcome -cnotin @("absent", "present", "not_created", "unknown")) {
      $Outcome = "unknown"
    }
    return (
      "[rustup-smoke-diagnostic] kind={0} phase={1} elapsed_ms={2} phase_elapsed_ms={3} cleanup={4} root={5}" -f
      $Kind, $phase, $elapsed, $phaseElapsed, $cleanup, $Outcome
    )
  } catch {
    return "[rustup-smoke-diagnostic] kind=unknown phase=unknown elapsed_ms=0 phase_elapsed_ms=0 cleanup=unknown root=unknown"
  }
}

function Get-RustupSmokeRootObservation {
  param([string]$Root)
  try {
    if ([string]::IsNullOrWhiteSpace($Root)) { return "not_created" }
    if (Test-Path -LiteralPath $Root -ErrorAction Stop) { return "present" }
    return "absent"
  } catch {
    return "unknown"
  }
}

function Write-RustupSmokeDiagnostic {
  param([string]$Kind, [string]$Outcome = "unknown")
  try {
    [Console]::Out.WriteLine((Format-RustupSmokeDiagnostic -Kind $Kind -Outcome $Outcome))
  } catch { }
}
# END rustup-smoke-diagnostic:helpers

# BEGIN rustup-smoke-diagnostic:outer_try
try {
# END rustup-smoke-diagnostic:outer_try

if ([string]::IsNullOrWhiteSpace($Toolchain)) {
  $toolchainFile = Join-Path $PSScriptRoot "..\src-tauri\rust-toolchain.toml"
  $toolchainConfig = Get-Content -LiteralPath $toolchainFile -Raw
  $channelMatch = [regex]::Match(
    $toolchainConfig,
    '(?m)^\s*channel\s*=\s*"([^"]+)"'
  )
  if (-not $channelMatch.Success) {
    throw "Unable to read the pinned Rust channel from: $toolchainFile"
  }
  $Toolchain = $channelMatch.Groups[1].Value
}

Set-RustupSmokePhase "rustup_home" # rustup-smoke-diagnostic
$rustupHome = (& rustup show home).Trim()
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($rustupHome)) {
  throw "Unable to resolve the current rustup home."
}

Set-RustupSmokePhase "toolchain_lookup" # rustup-smoke-diagnostic
$installedToolchain = (& rustup toolchain list) |
  Where-Object { $_ -match "^$([regex]::Escape($Toolchain))-" } |
  Select-Object -First 1
if (-not $installedToolchain) {
  throw "Source toolchain is not installed locally: $Toolchain"
}
$installedToolchain = ($installedToolchain -split "\s+")[0]
$sourceToolchain = Join-Path $rustupHome "toolchains\$installedToolchain"

$temporaryRoot = Join-Path (
  [IO.Path]::GetTempPath()
) ("pinvou-rustup-repair-" + [guid]::NewGuid().ToString("N"))
$temporaryRustup = Join-Path $temporaryRoot "rustup"
$temporaryCargo = Join-Path $temporaryRoot "cargo"
$temporaryToolchains = Join-Path $temporaryRustup "toolchains"
$temporaryToolchain = Join-Path $temporaryToolchains $installedToolchain
$repairScript = Join-Path $PSScriptRoot "..\scripts\ci\ensure-rust-toolchain.ps1"

Set-RustupSmokePhase "account_check" # rustup-smoke-diagnostic
& $repairScript -CheckOnly
if ($LASTEXITCODE -ne 0) {
  throw "Account toolchain check failed before the isolated repair smoke test."
}

try {
  # The repair engine's refusal guards are the core safety contract: they must
  # reject the shared account rustup, a filesystem root, a non-empty unmarked
  # directory, a missing RUSTUP_HOME and a run without the managed flag before
  # touching anything. Exercise them for real here; every case below throws
  # before rustup runs, so at most an empty lock file is created inside the
  # throwaway test home.
  Set-RustupSmokePhase "refusal_guards" # rustup-smoke-diagnostic
  $previousRustupHome = $env:RUSTUP_HOME
  $previousCargoHome = $env:CARGO_HOME
  $previousManagedFlag = $env:PINVOU3_MANAGED_RUSTUP
  $previousDistServer = $env:RUSTUP_DIST_SERVER
  $previousUpdateRoot = $env:RUSTUP_UPDATE_ROOT
  $previousDownloadTimeout = $env:RUSTUP_DOWNLOAD_TIMEOUT
  $env:PINVOU3_MANAGED_RUSTUP = "1"
  $unmarkedHome = Join-Path $temporaryRoot "unmarked"
  New-Item -ItemType Directory -Path $unmarkedHome -Force | Out-Null
  Set-Content -LiteralPath (Join-Path $unmarkedHome "placeholder") `
    -Value "not a rustup home" -Encoding Ascii
  $guardRejections = @(
    [pscustomobject]@{
      Name = "the build account's shared rustup home"
      Home = Join-Path $env:USERPROFILE ".rustup"
      Managed = "1"
      Message = "Refusing to modify the build account's shared RUSTUP_HOME"
    },
    [pscustomobject]@{
      Name = "a filesystem root"
      Home = [IO.Path]::GetPathRoot(([IO.Path]::GetFullPath($temporaryRoot)))
      Managed = "1"
      Message = "Refusing to use a filesystem root as RUSTUP_HOME"
    },
    [pscustomobject]@{
      Name = "a non-empty unmarked directory"
      Home = $unmarkedHome
      Managed = "1"
      Message = "Refusing to adopt a non-empty unmarked RUSTUP_HOME"
    },
    [pscustomobject]@{
      Name = "a missing RUSTUP_HOME"
      Home = $null
      Managed = "1"
      Message = "Refusing automatic repair without an isolated RUSTUP_HOME"
    },
    [pscustomobject]@{
      Name = "a run without the managed flag"
      Home = $temporaryRustup
      Managed = $null
      Message = "Refusing automatic repair without PINVOU3_MANAGED_RUSTUP=1"
    }
  )
  foreach ($guard in $guardRejections) {
    if ($null -eq $guard.Home) {
      Remove-Item Env:RUSTUP_HOME -ErrorAction SilentlyContinue
    } else {
      $env:RUSTUP_HOME = $guard.Home
    }
    if ($null -eq $guard.Managed) {
      Remove-Item Env:PINVOU3_MANAGED_RUSTUP -ErrorAction SilentlyContinue
    } else {
      $env:PINVOU3_MANAGED_RUSTUP = $guard.Managed
    }
    $rejectionMessage = $null
    try {
      & $repairScript
    } catch {
      $rejectionMessage = "$($_.Exception.Message)"
    }
    if ($null -eq $rejectionMessage -or -not $rejectionMessage.Contains($guard.Message)) {
      throw (
        "The repair guard did not reject {0} (got: {1}); RUSTUP_HOME={2}" -f
        $guard.Name, $rejectionMessage, $guard.Home
      )
    }
    Write-Host "[test] Guard rejected $($guard.Name): OK"
  }
  $env:PINVOU3_MANAGED_RUSTUP = "1"

  Set-RustupSmokePhase "copy_toolchain" # rustup-smoke-diagnostic
  New-Item -ItemType Directory -Path $temporaryToolchains, $temporaryCargo -Force |
    Out-Null
  Set-Content -LiteralPath (Join-Path $temporaryRustup ".pinvou3-managed-rustup") `
    -Value "pinvou3-managed-rustup-v1" -Encoding Ascii
  Write-Host "[test] Copying the source toolchain into the isolated test root."
  & robocopy $sourceToolchain $temporaryToolchain /E /COPY:DAT /DCOPY:DAT `
    /R:1 /W:1 /MT:8 /NFL /NDL /NJH /NJS /NP
  $robocopyExitCode = $LASTEXITCODE
  if ($robocopyExitCode -ge 8) {
    throw "Failed to copy the source toolchain with robocopy: $robocopyExitCode"
  }

  Set-RustupSmokePhase "copy_settings" # rustup-smoke-diagnostic
  $sourceSettings = Join-Path $rustupHome "settings.toml"
  if (Test-Path -LiteralPath $sourceSettings) {
    Copy-Item -LiteralPath $sourceSettings `
      -Destination (Join-Path $temporaryRustup "settings.toml") -Force
  }

  Set-RustupSmokePhase "corrupt_binaries" # rustup-smoke-diagnostic
  $resolvedTemporaryRoot = (Resolve-Path -LiteralPath $temporaryRoot).Path
  $missingExecutables = @(
    (Join-Path $temporaryToolchain "bin\cargo.exe"),
    (Join-Path $temporaryToolchain "bin\rustc.exe")
  )
  foreach ($target in $missingExecutables) {
    $targetParent = (Resolve-Path -LiteralPath (Split-Path -Parent $target)).Path
    if (-not $targetParent.StartsWith(
      $resolvedTemporaryRoot,
      [StringComparison]::OrdinalIgnoreCase
    )) {
      throw "Refusing to corrupt a path outside the isolated test root: $target"
    }
    Remove-Item -LiteralPath $target -Force
  }

  Write-Host "[test] Prepared isolated inconsistent toolchain: $temporaryRoot"
  $env:RUSTUP_HOME = $temporaryRustup
  $env:CARGO_HOME = $temporaryCargo
  $env:PINVOU3_MANAGED_RUSTUP = "1"
  $env:RUSTUP_DIST_SERVER = "http://127.0.0.1:1"
  $env:RUSTUP_UPDATE_ROOT = "http://127.0.0.1:1/rustup"
  # Keep the production timeout unchanged; this test must reach the next
  # fallback source promptly when a distribution endpoint stalls.
  $env:RUSTUP_DOWNLOAD_TIMEOUT = "120"

  # With RUSTUP_HOME pointed at the corrupted copy, the real -CheckOnly probe
  # must classify it as incomplete (exit 2) before any repair runs; this is
  # the only place the incomplete branch executes against real rustup state.
  Set-RustupSmokePhase "incomplete_probe" # rustup-smoke-diagnostic
  & $repairScript -CheckOnly
  if ($LASTEXITCODE -ne 2) {
    throw (
      "Inconsistent toolchain was not detected by -CheckOnly (exit $LASTEXITCODE, expected 2)."
    )
  }
  Write-Host "[test] -CheckOnly detected the corrupted toolchain: OK"

  Set-RustupSmokePhase "repair" # rustup-smoke-diagnostic
  # Stream diagnostics before a terminating repair error can discard an
  # assignment's buffered output. Keep the same evidence for assertions.
  $repairOutput = [System.Collections.Generic.List[object]]::new()
  & $repairScript `
    -InstallAttemptTimeoutSeconds 300 `
    -RepairAttemptsPerSource 1 *>&1 | ForEach-Object {
      $repairOutput.Add($_)
      Write-Host $_
    }
  $repairLog = $repairOutput | Out-String

  Set-RustupSmokePhase "repair_evidence" # rustup-smoke-diagnostic
  $expectedRepairEvidence = @(
    "Using configured source: http://127.0.0.1:1",
    "Repair attempt 1/1 failed using configured source",
    "Resetting the incomplete isolated toolchain before retry",
    "Using rsproxy mirror: https://rsproxy.cn",
    "Toolchain repair succeeded using"
  )
  foreach ($evidence in $expectedRepairEvidence) {
    if (-not $repairLog.Contains($evidence)) {
      throw "Repair output did not contain expected evidence: $evidence"
    }
  }

  Set-RustupSmokePhase "binary_verify" # rustup-smoke-diagnostic
  foreach ($command in @("cargo", "rustc", "clippy-driver", "rustfmt")) {
    & rustup run $Toolchain $command --version
    if ($LASTEXITCODE -ne 0) {
      throw "Post-repair verification failed: $command"
    }
  }

  # rust-std corruption with intact binaries must be detected too: the four
  # binary probes never link against std, so the target libdir is probed
  # directly. Remove it and require -CheckOnly to classify the toolchain as
  # incomplete through the real probe path.
  Set-RustupSmokePhase "stdlib_location" # rustup-smoke-diagnostic
  $stdTargetLibDir = (& rustup run $Toolchain rustc --print target-libdir |
    Select-Object -Last 1)
  $stdTargetLibDir = "$stdTargetLibDir".Trim()
  if ([string]::IsNullOrWhiteSpace($stdTargetLibDir) -or -not $stdTargetLibDir.StartsWith(
    $resolvedTemporaryRoot,
    [StringComparison]::OrdinalIgnoreCase
  )) {
    throw "Unexpected std target libdir outside the isolated test root: $stdTargetLibDir"
  }
  Set-RustupSmokePhase "stdlib_remove" # rustup-smoke-diagnostic
  Remove-Item -LiteralPath $stdTargetLibDir -Recurse -Force
  Set-RustupSmokePhase "stdlib_probe" # rustup-smoke-diagnostic
  & $repairScript -CheckOnly
  if ($LASTEXITCODE -ne 2) {
    throw (
      "A missing rust-std was not detected by -CheckOnly (exit $LASTEXITCODE, expected 2)."
    )
  }
  Write-Host "[test] -CheckOnly detected the missing rust-std: OK"

  Set-RustupSmokePhase "complete" # rustup-smoke-diagnostic
  Write-Host "[test] Isolated inconsistent-toolchain repair: PASS"
} finally {
  Set-RustupSmokeCleanup "restore_environment" # rustup-smoke-diagnostic
  foreach ($entry in @(
    @("RUSTUP_HOME", $previousRustupHome),
    @("CARGO_HOME", $previousCargoHome),
    @("PINVOU3_MANAGED_RUSTUP", $previousManagedFlag),
    @("RUSTUP_DIST_SERVER", $previousDistServer),
    @("RUSTUP_UPDATE_ROOT", $previousUpdateRoot),
    @("RUSTUP_DOWNLOAD_TIMEOUT", $previousDownloadTimeout)
  )) {
    if ($null -eq $entry[1]) {
      Remove-Item -LiteralPath "Env:\$($entry[0])" -ErrorAction SilentlyContinue
    } else {
      Set-Item -LiteralPath "Env:\$($entry[0])" -Value $entry[1]
    }
  }
  Set-RustupSmokeCleanup "check_root" # rustup-smoke-diagnostic
  if (Test-Path -LiteralPath $temporaryRoot) {
    $resolved = (Resolve-Path -LiteralPath $temporaryRoot).Path
    $temporaryBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    $safeName = (Split-Path -Leaf $resolved).StartsWith(
      "pinvou-rustup-repair-",
      [StringComparison]::OrdinalIgnoreCase
    )
    if (-not $resolved.StartsWith(
      $temporaryBase,
      [StringComparison]::OrdinalIgnoreCase
    ) -or -not $safeName) {
      throw "Refusing to remove an unexpected test path: $resolved"
    }
    Set-RustupSmokeCleanup "remove_requested" # rustup-smoke-diagnostic
    Remove-Item -LiteralPath $resolved -Recurse -Force -ErrorAction SilentlyContinue
  }
  Set-RustupSmokeCleanup "finished" # rustup-smoke-diagnostic
}
# BEGIN rustup-smoke-diagnostic:outer_observation
} catch {
  # Diagnostic failure must not replace the original exception or its exit status.
  Write-RustupSmokeDiagnostic -Kind "failure"
  throw
} finally {
  try {
    $rootObservation = Get-RustupSmokeRootObservation -Root $temporaryRoot
    Write-RustupSmokeDiagnostic -Kind "cleanup" -Outcome $rootObservation
  } catch { }
}
# END rustup-smoke-diagnostic:outer_observation
