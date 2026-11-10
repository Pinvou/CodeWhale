#requires -Version 7.4
[CmdletBinding()]
param(
  [ValidateSet('account','billing','token')][string]$Domain,
  [Parameter(Mandatory=$true)][string]$OutputDirectory,
  [string]$PgToolsDirectory,
  [ValidateRange(1,86400)][int]$TimeoutSeconds=1200,
  [switch]$Verify
)
$ErrorActionPreference='Stop'
$script:historyClock=[Diagnostics.Stopwatch]::StartNew()
$script:historyRoot=$null

function Assert-PrivateDirectory([string]$Path) {
  $item=Get-Item -LiteralPath $Path -Force
  if(-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'HISTORY_DIRECTORY_REJECTED'}
  if($IsWindows){
    $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $acl=Get-Acl -LiteralPath $Path
    if(-not $acl.AreAccessRulesProtected){throw 'HISTORY_DIRECTORY_NOT_PRIVATE'}
    if($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -notin @($sid,'S-1-5-18')){throw 'HISTORY_DIRECTORY_OWNER_REJECTED'}
    foreach($rule in $acl.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier])){
      if($rule.AccessControlType -eq 'Allow' -and $rule.IdentityReference.Value -notin @($sid,'S-1-5-18')){throw 'HISTORY_DIRECTORY_NOT_PRIVATE'}
    }
  } elseif($IsLinux){
    if(-not $item.User -or $item.User -cne [Environment]::UserName){throw 'HISTORY_DIRECTORY_OWNER_REJECTED'}
    if(([int][IO.File]::GetUnixFileMode($Path) -band 63) -ne 0){throw 'HISTORY_DIRECTORY_NOT_PRIVATE'}
  } else {throw 'HISTORY_PLATFORM_UNSUPPORTED'}
}

function New-PrivateDirectory([string]$Path) {
  $full=[IO.Path]::GetFullPath($Path)
  if(Test-Path -LiteralPath $full){throw 'HISTORY_DESTINATION_EXISTS'}
  $parent=Get-Item -LiteralPath ([IO.Path]::GetDirectoryName($full)) -Force
  if(-not $parent.PSIsContainer -or ($parent.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'HISTORY_PARENT_REJECTED'}
  Assert-PrivateDirectory $parent.FullName
  if($IsWindows){
    $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $acl=[Security.AccessControl.DirectorySecurity]::new()
    $acl.SetSecurityDescriptorSddlForm(('O:{0}D:P(A;OICI;FA;;;{0})(A;OICI;FA;;;SY)' -f $sid))
    $null=[IO.FileSystemAclExtensions]::CreateDirectory($acl,$full)
  } elseif($IsLinux){
    $null=[IO.Directory]::CreateDirectory($full,[IO.UnixFileMode]448)
  } else {throw 'HISTORY_PLATFORM_UNSUPPORTED'}
  Assert-PrivateDirectory $full
  return $full
}

function Resolve-PgTool([string]$Name) {
  $binary=if($IsWindows){$Name+'.exe'}else{$Name}
  if($PgToolsDirectory){$path=Join-Path $PgToolsDirectory $binary}else{$path=(Get-Command $binary -CommandType Application -ErrorAction Stop).Source}
  $item=Get-Item -LiteralPath $path -Force
  if($item.PSIsContainer){throw 'HISTORY_TOOL_REJECTED'}
  return $item.FullName
}

function Invoke-OwnedPg([string]$Name,[string[]]$Arguments,[string]$Label) {
  $remaining=[int64]$TimeoutSeconds*1000-$script:historyClock.ElapsedMilliseconds
  if($remaining -le 0){throw 'HISTORY_BUDGET_EXHAUSTED'}
  $stdout=Join-Path $script:historyRoot ('.'+$Label+'-'+[guid]::NewGuid().ToString('N')+'.stdout')
  $stderr=$stdout+'.stderr'
  $start=[Diagnostics.ProcessStartInfo]::new()
  $start.FileName=Resolve-PgTool $Name
  $start.UseShellExecute=$false
  $start.CreateNoWindow=$true
  $start.RedirectStandardOutput=$true
  $start.RedirectStandardError=$true
  foreach($argument in $Arguments){$start.ArgumentList.Add($argument)}
  $start.ArgumentList.Add('--no-password')
  $start.Environment['PGCONNECT_TIMEOUT']='5'
  $start.Environment['PGOPTIONS']='-c default_transaction_read_only=on -c statement_timeout=30000 -c lock_timeout=5000'
  $start.Environment['PGAPPNAME']='pinvou-history-export'
  $process=[Diagnostics.Process]::new()
  $process.StartInfo=$start
  $outStream=$null;$errStream=$null;$started=$false;$outCopy=$null;$errCopy=$null
  try {
    $outStream=[IO.File]::Open($stdout,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
    $errStream=[IO.File]::Open($stderr,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
    $started=$process.Start()
    if(-not $started){throw 'HISTORY_PROCESS_START_FAILED'}
    $outCopy=$process.StandardOutput.BaseStream.CopyToAsync($outStream)
    $errCopy=$process.StandardError.BaseStream.CopyToAsync($errStream)
    if(-not $process.WaitForExit([int][Math]::Min($remaining,[int]::MaxValue))){
      $process.Kill($true)
      if(-not $process.WaitForExit(10000)){throw 'HISTORY_PROCESS_RETIREMENT_UNKNOWN'}
      throw 'HISTORY_BUDGET_EXHAUSTED'
    }
    $null=[Threading.Tasks.Task]::WaitAll([Threading.Tasks.Task[]]@($outCopy,$errCopy),10000)
    if(-not $outCopy.IsCompletedSuccessfully -or -not $errCopy.IsCompletedSuccessfully){throw 'HISTORY_PROCESS_STREAM_FAILED'}
    if($process.ExitCode -ne 0){throw 'HISTORY_PG_COMMAND_FAILED'}
  } finally {
    if($started -and -not $process.HasExited){
      $process.Kill($true)
      if(-not $process.WaitForExit(10000)){throw 'HISTORY_PROCESS_RETIREMENT_UNKNOWN'}
    }
    if($outCopy -and $errCopy){$null=[Threading.Tasks.Task]::WaitAll([Threading.Tasks.Task[]]@($outCopy,$errCopy),10000)}
    if($outStream){$outStream.Dispose()};if($errStream){$errStream.Dispose()};$process.Dispose()
  }
  if((Get-Item -LiteralPath $stdout).Length -gt 524288){throw 'HISTORY_METADATA_TOO_LARGE'}
  return [IO.File]::ReadAllText($stdout)
}

function Get-HistoryMetadata {
  $sql=Join-Path $PSScriptRoot '../account-platform/deploy/runbooks/sql/history-export-metadata.sql'
  $text=Invoke-OwnedPg 'psql' @('-X','-A','-t','-v','ON_ERROR_STOP=1','-v',('audit_domain='+$Domain),'-f',[IO.Path]::GetFullPath($sql)) 'metadata'
  $json=@($text -split '\r?\n' | Where-Object { $_.StartsWith('{') })
  if($json.Count -ne 1){throw 'HISTORY_METADATA_REJECTED'}
  return ($json[0] | ConvertFrom-Json -Depth 30)
}

function Assert-Archive([string]$Path,[string[]]$Tables) {
  $item=Get-Item -LiteralPath $Path -Force
  if($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $item.Length -lt 1){throw 'HISTORY_ARCHIVE_REJECTED'}
  $catalog=Invoke-OwnedPg 'pg_restore' @('--list',$item.FullName) 'archive-catalog'
  $actual=@([regex]::Matches($catalog,'TABLE DATA public ([a-z_]+)\b') | ForEach-Object {$_.Groups[1].Value} | Sort-Object)
  if(($actual -join ',') -ne (($Tables | Sort-Object) -join ',')){throw 'HISTORY_ARCHIVE_TABLES_REJECTED'}
  return @{bytes=$item.Length;sha256=(Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash.ToLowerInvariant()}
}

try {
  if($Verify){
    $script:historyRoot=[IO.Path]::GetFullPath($OutputDirectory)
    Assert-PrivateDirectory $script:historyRoot
    $manifestPath=Join-Path $script:historyRoot 'manifest.json'
    $manifestItem=Get-Item -LiteralPath $manifestPath -Force
    if($manifestItem.PSIsContainer -or ($manifestItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $manifestItem.Length -gt 524288){throw 'HISTORY_MANIFEST_REJECTED'}
    $manifest=[IO.File]::ReadAllText($manifestPath) | ConvertFrom-Json -Depth 30
    if($manifest.protocol -ne 'account-history-export/v1' -or $manifest.domain -notin @('account','billing','token') -or ($Domain -and $Domain -ne $manifest.domain)){throw 'HISTORY_MANIFEST_REJECTED'}
    $tables=@('schema_migrations','outbox_events')
    if($manifest.domain -eq 'billing'){$tables+='payment_provider_health_observations'}
    if(($manifest.tables -join ',') -cne ($tables -join ',')){throw 'HISTORY_MANIFEST_TABLES_REJECTED'}
    $verified=Assert-Archive (Join-Path $script:historyRoot 'history.dump') $tables
    if($verified.bytes -ne $manifest.archive_bytes -or $verified.sha256 -cne $manifest.archive_sha256){throw 'HISTORY_CHECKSUM_MISMATCH'}
    if($script:historyClock.Elapsed.TotalSeconds -ge $TimeoutSeconds){throw 'HISTORY_BUDGET_EXHAUSTED'}
    [Console]::Out.WriteLine('ACCOUNT_HISTORY_EXPORT_VERIFIED')
  } else {
    if(-not $Domain -or -not $env:PGDATABASE -or $env:PGDATABASE.Contains('://') -or $env:PGDATABASE.Contains('=')){throw 'HISTORY_PG_ENVIRONMENT_REQUIRED'}
    $script:historyRoot=New-PrivateDirectory $OutputDirectory
    $before=Get-HistoryMetadata
    $tables=@('schema_migrations','outbox_events')
    if($Domain -eq 'billing'){$tables+='payment_provider_health_observations'}
    $archive=Join-Path $script:historyRoot 'history.dump'
    $arguments=@('--format=custom','--data-only','--strict-names','--no-owner','--no-privileges','--lock-wait-timeout=5000','--file',$archive)
    foreach($table in $tables){$arguments+=('--table=public.'+$table)}
    $null=Invoke-OwnedPg 'pg_dump' $arguments 'dump'
    $after=Get-HistoryMetadata
    foreach($key in @('domain','database','database_oid','database_role','server_version','postmaster_started_at')){
      if($before.$key -cne $after.$key){throw 'HISTORY_SOURCE_CHANGED'}
    }
    if(($before.migrations | ConvertTo-Json -Depth 20 -Compress) -cne ($after.migrations | ConvertTo-Json -Depth 20 -Compress)){throw 'HISTORY_LEDGER_CHANGED'}
    $verified=Assert-Archive $archive $tables
    if($script:historyClock.Elapsed.TotalSeconds -ge $TimeoutSeconds){throw 'HISTORY_BUDGET_EXHAUSTED'}
    $manifest=[ordered]@{protocol='account-history-export/v1';domain=$Domain;source=$before;observed_until=$after.observed_at;archive_bytes=$verified.bytes;archive_sha256=$verified.sha256;tables=$tables;scope='selected table data only; no restore or delivery authority';snapshot='pg_dump internal snapshot; source metadata captured before/after, not snapshot row counts'}
    $manifestPath=Join-Path $script:historyRoot 'manifest.json'
    $bytes=[Text.Encoding]::UTF8.GetBytes(($manifest | ConvertTo-Json -Depth 30)+"`n")
    $stream=[IO.File]::Open($manifestPath,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try {$stream.Write($bytes);$stream.Flush($true)} finally {$stream.Dispose()}
    [Console]::Out.WriteLine('ACCOUNT_HISTORY_EXPORT_COMPLETE')
  }
} catch {
  $failure=$_
  try {
    if($script:historyRoot){
      Assert-PrivateDirectory $script:historyRoot
      $diagnostic=Join-Path $script:historyRoot ('.failure-'+[guid]::NewGuid().ToString('N')+'.txt')
      [IO.File]::WriteAllText($diagnostic,$failure.ToString()+"`n"+$failure.ScriptStackTrace)
    }
  } catch { }
  [Console]::Error.WriteLine('ACCOUNT_HISTORY_EXPORT_FAILED')
  exit 1
}
