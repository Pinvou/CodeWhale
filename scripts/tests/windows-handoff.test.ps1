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
$old = $null
$worker = $null
try {
  foreach ($scenario in @('approved','unapproved','wrong_identity','helper_crash','timeout')) {
    $started=Join-Path $dir "$scenario.started"; $approved=Join-Path $dir "$scenario.approved"; $result=Join-Path $dir "$scenario.result"
    $old=Start-Process powershell.exe -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 120' -PassThru -WindowStyle Hidden
    $ticks=$old.StartTime.ToUniversalTime().ToFileTimeUtc(); if ($scenario -eq 'wrong_identity') { $ticks++ }
    $waitMs = if ($scenario -eq 'timeout') { 10000 } else { 300000 }
    $worker=Start-Process powershell.exe -ArgumentList @('-NoProfile','-File',"`"$helper`"", "`"$source`"",$old.Id,$ticks,"`"$started`"","`"$approved`"","`"$result`"",$waitMs) -PassThru -WindowStyle Hidden -RedirectStandardError (Join-Path $dir "worker-error.txt")
    if ($scenario -eq 'wrong_identity') { if (!$worker.WaitForExit(90000)) { throw 'identity probe timed out' }; if ($worker.ExitCode -eq 0 -or (Test-Path $started)) { throw 'wrong identity accepted' }; Stop-Process -Id $old.Id -Force; $old.WaitForExit(); Write-Host 'wrong_identity passed'; continue }
    $deadline=[DateTime]::UtcNow.AddSeconds(90)
    $record = ''
    while (!$record.EndsWith(':ready', [StringComparison]::Ordinal) -and !$worker.HasExited -and [DateTime]::UtcNow -lt $deadline) {
      if (Test-Path $started) { try { $record = [IO.File]::ReadAllText($started) } catch [IO.IOException] {} }
      Start-Sleep -Milliseconds 50; $worker.Refresh()
    }
    if (!$record.EndsWith(':ready', [StringComparison]::Ordinal)) { throw "$scenario did not reach suspended state: $(Get-Content (Join-Path $dir worker-error.txt) -Raw)" }
    $fields = $record.Split(':')
    if ($fields.Count -ne 5 -or $fields[0] -ne 'test' -or $fields[4] -cne 'ready' -or [uint32]$fields[2] -ne $worker.Id -or [long]$fields[3] -ne $worker.StartTime.ToUniversalTime().ToFileTimeUtc()) { throw 'helper identity did not match' }
    if (Test-Path $result) { throw "$scenario ran before old process exited" }
    if ($scenario -eq 'approved') { [IO.File]::WriteAllText($approved,'test') }
    if ($scenario -eq 'helper_crash') { Stop-Process -Id $worker.Id -Force }
    if ($scenario -eq 'timeout') { if (!$worker.WaitForExit(30000)) { throw 'helper did not time out' } }
    Stop-Process -Id $old.Id -Force
    $old.WaitForExit()
    if (!$worker.WaitForExit(30000)) { throw 'helper did not exit' }; Start-Sleep -Seconds 1
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
