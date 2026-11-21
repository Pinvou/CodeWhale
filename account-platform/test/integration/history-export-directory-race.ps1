#requires -Version 7.4
# Test-only deterministic interleaving. Both actual CLIs must observe an absent
# destination before either may allocate it; no production test hook is added.
param([string]$SourceScript,[string]$Domain,[string]$OutputDirectory,[string]$BarrierRoot,[ValidateSet('1','2')][string]$Worker)
$ErrorActionPreference='Stop'
function global:Test-Path {
  param([string]$LiteralPath)
  $exists=Microsoft.PowerShell.Management\Test-Path -LiteralPath $LiteralPath
  if($LiteralPath -ceq $OutputDirectory -and -not $exists){
    [IO.File]::WriteAllText((Join-Path $BarrierRoot ('ready-'+$Worker)),'ready')
    $clock=[Diagnostics.Stopwatch]::StartNew()
    while(-not ([IO.File]::Exists((Join-Path $BarrierRoot 'ready-1')) -and [IO.File]::Exists((Join-Path $BarrierRoot 'ready-2')))){
      if($clock.Elapsed.TotalSeconds -gt 10){throw 'HISTORY_TEST_BARRIER_TIMEOUT'}
      Start-Sleep -Milliseconds 5
    }
  }
  return $exists
}
& $SourceScript -Domain $Domain -OutputDirectory $OutputDirectory -TimeoutSeconds 30 -PgToolsDirectory $env:PINVOU_HISTORY_EXPORT_TEST_PGTOOLS
exit $LASTEXITCODE
