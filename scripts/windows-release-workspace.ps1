#Requires -Version 7.0
[CmdletBinding()]
param(
  [ValidateSet('Prepare','Cleanup')][string]$Mode = 'Prepare',
  [Parameter(Mandatory=$true)][string]$SourceRoot,
  [string]$ExpectedCommit = $env:GITHUB_SHA,
  [string]$TemporaryRoot = $env:RUNNER_TEMP,
  [string]$StatePath,
  [string]$Owner = "$env:GITHUB_RUN_ID/$env:GITHUB_RUN_ATTEMPT/$env:GITHUB_JOB"
)
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'Windows release workspace requires Windows.' }
$source = [IO.Path]::GetFullPath($SourceRoot).TrimEnd('\')
if (-not $TemporaryRoot) { $TemporaryRoot = [IO.Path]::GetTempPath() }
$temporary = [IO.Path]::GetFullPath($TemporaryRoot).TrimEnd('\')
if (-not (Test-Path -LiteralPath $temporary -PathType Container)) { throw 'Runner temporary directory is absent.' }
if ($Owner -eq '//') { throw 'Workspace owner identity is required.' }
$parents = @($temporary, (Split-Path -Parent (Split-Path -Parent $source))) |
  ForEach-Object { [IO.Path]::GetFullPath($_).TrimEnd('\') } | Select-Object -Unique

function Invoke-WorkspaceGit {
  param([string[]]$GitArguments)
  $saved = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
  try { $output = & git @GitArguments 2>&1; $code = $LASTEXITCODE }
  finally { $ErrorActionPreference = $saved }
  if ($code -ne 0) { throw "Release workspace git operation failed ($code)." }
  return (($output | Out-String).Trim())
}
function Write-WorkspaceJson {
  param([string]$Path, $Value)
  [IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
}
function Assert-OwnedWorkspace {
  param($State)
  if ($State.owner -cne $Owner -or $State.source -ine $source) { throw 'Workspace owner or source differs.' }
  $target = [IO.Path]::GetFullPath([string]$State.root).TrimEnd('\')
  $parent = Split-Path -Parent $target
  if ($parent -notin $parents -or (Split-Path -Leaf $target) -cnotmatch '^w[a-f0-9]{6}$' -or
      $target -ieq $source -or $source.StartsWith($target+'\',[StringComparison]::OrdinalIgnoreCase) -or
      $target.StartsWith($source+'\',[StringComparison]::OrdinalIgnoreCase)) {
    throw 'Workspace cleanup target is outside its approved temporary roots.'
  }
  if ((Test-Path -LiteralPath $target) -and ((Get-Item -LiteralPath $target -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Workspace root is a reparse point.' }
  $marker = Get-Content -LiteralPath ($target+'.owner.json') -Raw | ConvertFrom-Json
  if ($marker.owner -cne $Owner -or $marker.source -ine $source -or $marker.root -ine $target -or
      $marker.commit -cne $State.commit) {
    throw 'Workspace ownership marker or checkout identity differs.'
  }
  if ($State.prepared -and (Invoke-WorkspaceGit @('-C',$target,'rev-parse','HEAD')) -cne $State.commit) { throw 'Prepared checkout identity differs.' }
  return $target
}

if ($Mode -eq 'Cleanup') {
  if (-not $StatePath) { throw 'Workspace cleanup state is required.' }
  $state = Get-Content -LiteralPath $StatePath -Raw | ConvertFrom-Json
  if (-not $state.created) {
    if ($state.owner -cne $Owner -or $state.source -ine $source -or $state.root -ine $source) { throw 'Original workspace state differs.' }
    Write-Output 'Original release workspace retained.'
    return
  }
  $target = Assert-OwnedWorkspace $state
  # PowerShell 7 removes links themselves rather than traversing their targets.
  # Both the absolute parent and job-specific ownership marker are checked above.
  if (Test-Path -LiteralPath $target) { Remove-Item -LiteralPath $target -Recurse -Force }
  Remove-Item -LiteralPath ($target+'.owner.json') -Force
  Write-Output 'Removed the owned temporary release checkout.'
  return
}

if ($ExpectedCommit -cnotmatch '^[a-f0-9]{40}$' -or
    (Invoke-WorkspaceGit @('-C',$source,'rev-parse','HEAD')) -cne $ExpectedCommit) { throw 'Release source does not match its expected commit.' }
if (-not $StatePath) { $StatePath = Join-Path $temporary ('pinvou-release-workspace-'+[Guid]::NewGuid().ToString('N')+'.json') }
foreach ($path in @($source,$temporary,$StatePath)) { if ($path -match '[\r\n]') { throw 'Workspace path contains a newline.' } }
if (Test-Path -LiteralPath $StatePath) { throw 'Release workspace state already exists.' }
$root = $source; $created = $false
# The locked Node/npm tree reaches 266 characters with a 65-character checkout.
# Keep room for the existing PowerShell 5.1 staging layout and native consumers.
if ($source.Length -gt 56 -or $source -match '\s') {
  $parent = $parents | Where-Object { $_.Length + 8 -le 56 -and $_ -notmatch '\s' -and (Test-Path -LiteralPath $_ -PathType Container) } | Sort-Object Length | Select-Object -First 1
  if (-not $parent) { throw 'No writable short Runner directory is available for Windows staging.' }
  do { $root = Join-Path $parent ('w'+[Guid]::NewGuid().ToString('N').Substring(0,6)) } while ((Test-Path -LiteralPath $root) -or (Test-Path -LiteralPath ($root+'.owner.json')))
  $created = $true
}
$state = [ordered]@{owner=$Owner;source=$source;root=$root;commit=$ExpectedCommit;created=$created;prepared=$false}
if ($created) { Write-WorkspaceJson ($root+'.owner.json') $state }
Write-WorkspaceJson $StatePath $state
if ($env:GITHUB_OUTPUT) { [IO.File]::AppendAllText($env:GITHUB_OUTPUT, "state=$StatePath`n", [Text.UTF8Encoding]::new($false)) }
if ($created) {
  Invoke-WorkspaceGit @('clone','--shared','--no-checkout',$source,$root) | Out-Null
  Invoke-WorkspaceGit @('-C',$root,'checkout','--detach',$ExpectedCommit) | Out-Null
  if ($env:GITHUB_REPOSITORY -match '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
    Invoke-WorkspaceGit @('-C',$root,'remote','set-url','origin',('https://github.com/'+$env:GITHUB_REPOSITORY+'.git')) | Out-Null
  }
}
$state.prepared = $true
Write-WorkspaceJson $StatePath $state
foreach ($path in @($root,$StatePath)) { if ($path -match '[\r\n]') { throw 'Workspace output contains a newline.' } }
if ($env:GITHUB_ENV) {
  [IO.File]::AppendAllText($env:GITHUB_ENV, "PINVOU_WINDOWS_BUILD_ROOT=$root`nRUSTC_WRAPPER=$root/pinvou3-app/src-tauri/scripts/rustc-stack-wrapper.exe`n", [Text.UTF8Encoding]::new($false))
}
if ($env:GITHUB_OUTPUT) {
  [IO.File]::AppendAllText($env:GITHUB_OUTPUT, "root=$root`nstate=$StatePath`n", [Text.UTF8Encoding]::new($false))
}
$state | ConvertTo-Json -Compress
