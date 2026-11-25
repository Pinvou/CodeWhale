$ErrorActionPreference = 'Stop'
$source = (Resolve-Path "$PSScriptRoot/../../pinvou3-app/src-tauri/src/features/updater/platform/windows_handoff.cs").Path
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')
$dir = [IO.Path]::GetFullPath((Join-Path $tempRoot ('pinvou-handoff-test-' + [guid]::NewGuid().ToString('N'))))
if ([IO.Path]::GetDirectoryName($dir) -ne $tempRoot -or
    ![IO.Path]::GetFileName($dir).StartsWith('pinvou-handoff-test-', [StringComparison]::Ordinal)) { throw 'invalid test directory' }
New-Item -ItemType Directory $dir | Out-Null
$helper = Join-Path $dir 'helper.ps1'
@'
param($source,$pidToWait,$created,$started,$approved,$result,$waitMs)
$ErrorActionPreference='Stop'
Add-Type -Path $source
$exe = (Get-Command cmd.exe).Source
[PinvouInstallHandoff]::Run([uint32]$pidToWait,[long]$created,$exe,('/c echo ran > "' + $result + '"'),$started,$approved,'test',[uint32]$waitMs)
'@ | Set-Content -LiteralPath $helper -Encoding UTF8
# Test-only observed copy: deleting these fixed probes restores every source byte.
function Add-HandoffProbes([string]$actual) {
  $original = $actual
  $pairs = @()
  $pairs += ,@('    public static void Run(', '    static void Observe(string started, string phase) { try { System.IO.File.AppendAllText(started + ".phase", phase + "\n"); } catch { /* Only diagnostic I/O. */ } }
    public static void Run(')
  $pairs += ,@('        try {
            old = OpenProcess', '        try {
            Observe(started, "run_enter");
            old = OpenProcess')
  $pairs += ,@('            uint wait = WaitForSingleObject(old, waitMilliseconds);', '            Observe(started, "ready_written");
            Observe(started, "wait_enter");
            uint wait = WaitForSingleObject(old, waitMilliseconds);
            Observe(started, wait == 0 ? "wait_signaled" : wait == 0x102 ? "wait_timeout" : wait == 0xffffffff ? "wait_failed" : "wait_other");')
  $pairs += ,@('        } finally {
            if (initialized)', '        } finally {
            Observe(started, "cleanup_enter");
            if (initialized)')
  $pairs += ,@('            if (old != IntPtr.Zero) CloseHandle(old);', '            if (old != IntPtr.Zero) CloseHandle(old);
            Observe(started, "cleanup_done");')
  foreach ($pair in $pairs) {
    if ($actual.Split([string[]]@($pair[0]),[StringSplitOptions]::None).Length -ne 2) { throw 'handoff diagnostic anchor mismatch' }
    $actual = $actual.Replace($pair[0], $pair[1])
  }
  $inverse = $actual
  for ($index=$pairs.Count-1; $index -ge 0; $index--) { $inverse=$inverse.Replace($pairs[$index][1],$pairs[$index][0]) }
  if ($inverse -cne $original) { throw 'handoff diagnostic inverse mismatch' }
  return $actual
}
function Get-ClosedHandoffPhase([string]$text) {
  $allowed=@('run_enter','ready_written','wait_enter','wait_signaled','wait_timeout','wait_failed','wait_other','cleanup_enter','cleanup_done','wrapper_return')
  $phase='unknown'
  if ($text.Length -gt 8192) { $text=$text.Substring($text.Length-8192); $lines=$text -split "`r?`n"; $lines=@($lines | Select-Object -Skip 1) } else { $lines=$text -split "`r?`n" }
  foreach($line in $lines) { if($line -cin $allowed) { $phase=$line } }
  return $phase
}
function Get-HandoffPhaseFile([string]$started) {
  # Only this owned fixed-literal phase file is read; unexpected shape/size is unavailable.
  try {
    $file=[IO.FileInfo]::new($started+'.phase')
    if (!$file.Exists -or $file.Length -gt 8192) { return 'unknown' }
    return (Get-ClosedHandoffPhase ([IO.File]::ReadAllText($file.FullName)))
  } catch { return 'unknown' }
}
function Write-ClosedHandoffFailure([string]$scenario,[string]$started,[Diagnostics.Stopwatch]$clock) {
  # This catch protects only diagnostic I/O; the caller still throws its original failure.
  try {
    $phase=Get-HandoffPhaseFile $started
    $name=if($scenario -cin @('approved','unapproved','wrong_identity','helper_crash','timeout')){$scenario}else{'unknown'}
    $elapsed=if($null -ne $clock -and $clock.ElapsedMilliseconds -ge 0 -and $clock.ElapsedMilliseconds -le 180000){[string]$clock.ElapsedMilliseconds}else{'unknown'}
    [Console]::Error.WriteLine('handoff_diagnostic case='+$name+' phase='+$phase+' elapsed_ms='+$elapsed)
  } catch { }
}
# Pure parser counterexamples contain only synthetic data and launch no process.
if ((Get-ClosedHandoffPhase 'TEST_ONLY_UNRECOGNIZED_PRIVATE_VALUE') -cne 'unknown' -or
    (Get-ClosedHandoffPhase "wait_enter`ncleanup_done`nTEST_ONLY_UNRECOGNIZED_PRIVATE_VALUE") -cne 'cleanup_done' -or
    (Get-ClosedHandoffPhase 'cleanup_done extra') -cne 'unknown' -or
    (Get-ClosedHandoffPhase ('wait_enter'+"`n"+('x'*8192))) -cne 'unknown') { throw 'handoff diagnostic parser failed' }
$diagnosticProbe=Join-Path $dir 'diagnostic-only'
if ((Get-HandoffPhaseFile $diagnosticProbe) -cne 'unknown') { throw 'missing diagnostic file accepted' }
[IO.File]::WriteAllText($diagnosticProbe+'.phase',('x'*8193))
if ((Get-HandoffPhaseFile $diagnosticProbe) -cne 'unknown') { throw 'oversized diagnostic file accepted' }
$diagnosticDirectory=Join-Path $dir 'diagnostic-directory'
[IO.Directory]::CreateDirectory($diagnosticDirectory+'.phase') | Out-Null
if ((Get-HandoffPhaseFile $diagnosticDirectory) -cne 'unknown') { throw 'diagnostic directory accepted' }
$originalError=[Console]::Error
$capturedError=[IO.StringWriter]::new()
try {
  [Console]::SetError($capturedError)
  $failure=''
  try { Write-ClosedHandoffFailure 'timeout' $diagnosticDirectory ([Diagnostics.Stopwatch]::StartNew()); throw 'TEST_ONLY_ORIGINAL_FAILURE' } catch { $failure=$_.Exception.Message }
  if ($failure -cne 'TEST_ONLY_ORIGINAL_FAILURE') { throw 'diagnostic replaced original failure' }
  if ($capturedError.ToString() -cnotmatch '^handoff_diagnostic case=timeout phase=unknown elapsed_ms=[0-9]+\r?\n$') { throw 'diagnostic output not closed' }
} finally { [Console]::SetError($originalError); $capturedError.Dispose() }
$productionBytes=[IO.File]::ReadAllBytes($source)
$productionText=[Text.Encoding]::UTF8.GetString($productionBytes)
$observedSource=Join-Path $dir 'observed_handoff.cs'
[IO.File]::WriteAllText($observedSource,(Add-HandoffProbes $productionText),[Text.UTF8Encoding]::new($false))
$source=$observedSource

$old = $null
$worker = $null
try {
  foreach ($scenario in @('approved','unapproved','wrong_identity','helper_crash','timeout')) {
    $started=Join-Path $dir "$scenario.started"; $approved=Join-Path $dir "$scenario.approved"; $result=Join-Path $dir "$scenario.result"
    $scenarioClock=[Diagnostics.Stopwatch]::StartNew()
    $old=Start-Process powershell.exe -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 120' -PassThru -WindowStyle Hidden
    $ticks=$old.StartTime.ToUniversalTime().ToFileTimeUtc(); if ($scenario -eq 'wrong_identity') { $ticks++ }
    $waitMs = if ($scenario -eq 'timeout') { 10000 } else { 300000 }
    $worker=Start-Process powershell.exe -ArgumentList @('-NoProfile','-File',"`"$helper`"", "`"$source`"",$old.Id,$ticks,"`"$started`"","`"$approved`"","`"$result`"",$waitMs) -PassThru -WindowStyle Hidden -RedirectStandardError (Join-Path $dir "worker-error.txt")
    if ($scenario -eq 'wrong_identity') { if (!$worker.WaitForExit(90000)) { Write-ClosedHandoffFailure $scenario $started $scenarioClock; throw 'identity probe timed out' }; if ($worker.ExitCode -eq 0 -or (Test-Path $started)) { throw 'wrong identity accepted' }; Stop-Process -Id $old.Id -Force; $old.WaitForExit(); Write-Host 'wrong_identity passed'; continue }
    $deadline=[DateTime]::UtcNow.AddSeconds(90)
    $record = ''
    while (!$record.EndsWith(':ready', [StringComparison]::Ordinal) -and !$worker.HasExited -and [DateTime]::UtcNow -lt $deadline) {
      if (Test-Path $started) { try { $record = [IO.File]::ReadAllText($started) } catch [IO.IOException] {} }
      Start-Sleep -Milliseconds 50; $worker.Refresh()
    }
    if (!$record.EndsWith(':ready', [StringComparison]::Ordinal)) { Write-ClosedHandoffFailure $scenario $started $scenarioClock; throw "$scenario did not reach suspended state" }
    $fields = $record.Split(':')
    if ($fields.Count -ne 5 -or $fields[0] -ne 'test' -or $fields[4] -cne 'ready' -or [uint32]$fields[2] -ne $worker.Id -or [long]$fields[3] -ne $worker.StartTime.ToUniversalTime().ToFileTimeUtc()) { throw 'helper identity did not match' }
    if (Test-Path $result) { throw "$scenario ran before old process exited" }
    if ($scenario -eq 'approved') { [IO.File]::WriteAllText($approved,'test') }
    if ($scenario -eq 'helper_crash') { Stop-Process -Id $worker.Id -Force }
    if ($scenario -eq 'timeout') { if (!$worker.WaitForExit(30000)) { Write-ClosedHandoffFailure $scenario $started $scenarioClock; throw 'helper did not time out' } }
    Stop-Process -Id $old.Id -Force
    $old.WaitForExit()
    if (!$worker.WaitForExit(30000)) { Write-ClosedHandoffFailure $scenario $started $scenarioClock; throw 'helper did not exit' }; Start-Sleep -Seconds 1
    if ($scenario -eq 'approved' -and !(Test-Path $result)) { throw 'approved installer did not run' }
    if ($scenario -ne 'approved' -and (Test-Path $result)) { throw "$scenario unexpectedly ran" }
    if ($scenario -in @('unapproved','timeout') -and $worker.ExitCode -eq 0) { throw 'failed handoff succeeded' }
    Write-Host "$scenario passed"
  }
} finally {
  foreach ($process in @($worker, $old)) {
    if ($null -ne $process) {
      $process.Refresh()
      if (!$process.HasExited) { $process.Kill(); $process.WaitForExit() }
    }
  }
  $resolved = [IO.Path]::GetFullPath($dir)
  if ($resolved -ne $dir -or [IO.Path]::GetDirectoryName($resolved) -ne $tempRoot -or
      ((Get-Item -LiteralPath $resolved -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'unsafe test cleanup path' }
  Remove-Item -LiteralPath $resolved -Recurse -Force
}
