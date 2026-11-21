#Requires -Version 7.0
param([string]$NodeRuntimeZip)
$ErrorActionPreference='Stop'
$helper=Join-Path $PSScriptRoot '../windows-release-workspace.ps1'
$tempBase=[IO.Path]::GetTempPath().TrimEnd('\')
# Keep every fixture and clone inside a registered process scope, including
# cancellation paths where this script's finally block cannot execute.
$ownedShortScope=$env:PINVOU_CI_FIXTURE_ROOT -and $tempBase -ieq $env:PINVOU_CI_FIXTURE_ROOT -and
 $tempBase.Length -le 35 -and $tempBase -notmatch '\s' -and
 (Test-Path -LiteralPath (Join-Path $tempBase '.pinvou-fixture-scope'))
if(-not $ownedShortScope){
 $candidate=[IO.DirectoryInfo]$PSScriptRoot
 while($candidate -and ($candidate.FullName.Length+10 -gt 35 -or $candidate.FullName -match '\s')){$candidate=$candidate.Parent}
 if(-not $candidate -or -not $candidate.Parent){throw 'No short non-root checkout ancestor is available for the workspace fixture'}
 $receiptParent=if($env:PINVOU_CI_RESOURCE_LEDGER){$env:PINVOU_CI_RESOURCE_LEDGER}else{[IO.Path]::GetTempPath()}
 $receipt=Join-Path $receiptParent ('workspace-test-'+[Guid]::NewGuid().ToString('N')+'.json')
 $python=if($env:PINVOU_CI_PYTHON){$env:PINVOU_CI_PYTHON}else{'python'}
 $child=@('-NoProfile','-NonInteractive','-File',$PSCommandPath)
 if($NodeRuntimeZip){$child+=@('-NodeRuntimeZip',[IO.Path]::GetFullPath($NodeRuntimeZip))}
 Write-Output "Workspace input TEMP length $($tempBase.Length); supervisor receipt: $receipt"
 & $python (Join-Path $PSScriptRoot '../ci-fixture-process.py') --short-workspace-parent $candidate.FullName --receipt $receipt -- (Join-Path $PSHOME 'pwsh.exe') @child
 if($LASTEXITCODE -ne 0){throw "Supervised workspace regression failed ($LASTEXITCODE); original receipt retained: $receipt"}
 return
}
$fixtureParent=$tempBase
$labelLength=48-$fixtureParent.Length-1
# A 48-character parent forces the created checkout to the supported 56-character limit.
$label=('rwt-'+[Guid]::NewGuid().ToString('N')+[Guid]::NewGuid().ToString('N')).Substring(0,$labelLength)
$fixture=Join-Path $fixtureParent $label
# Creation without -Force refuses a pre-existing fixture rather than adopting it.
New-Item -ItemType Directory -Path $fixture -ErrorAction Stop|Out-Null
Write-Output "Workspace fixture: TEMP length $($tempBase.Length), parent length $($fixtureParent.Length), fixture length $($fixture.Length)."
$source=Join-Path $fixture ('nested/checkout-'+('long-path-'*5));$statePath=Join-Path $fixture 'state.json'
$outside=Join-Path $fixture 'unrelated';$owner='workspace-test/'+[Guid]::NewGuid().ToString('N')
$savedEnv=@{};foreach($key in @('GITHUB_ENV','GITHUB_OUTPUT','GITHUB_REPOSITORY')){$savedEnv[$key]=[Environment]::GetEnvironmentVariable($key);[Environment]::SetEnvironmentVariable($key,$null)}
function Git-Test {
 param([string[]]$Arguments)
 $saved=$ErrorActionPreference;$ErrorActionPreference='Continue'
 try{$output=& git @Arguments 2>&1;$code=$LASTEXITCODE}finally{$ErrorActionPreference=$saved}
 if($code -ne 0){throw 'Fixture git failed'}
 return (($output|Out-String).Trim())
}
try {
 New-Item -ItemType Directory -Path $source,$outside -Force|Out-Null
 [IO.File]::WriteAllText((Join-Path $source 'fixture.txt'),'original source')
 [IO.File]::WriteAllText((Join-Path $outside 'sentinel.txt'),'retain this unrelated directory')
 Git-Test @('init','--quiet',$source)|Out-Null
 Git-Test @('-C',$source,'add','fixture.txt')|Out-Null
 Git-Test @('-C',$source,'-c','user.name=Workspace Fixture','-c','user.email=fixture@example.invalid','commit','--quiet','-m','fixture')|Out-Null
 $commit=Git-Test @('-C',$source,'rev-parse','HEAD')
 # actions/checkout uses a shallow repository by default.
 [IO.File]::WriteAllText((Join-Path $source '.git/shallow'),"$commit`n")
 & $helper -SourceRoot $source -ExpectedCommit $commit -TemporaryRoot $fixture -StatePath $statePath -Owner $owner|Out-Null
 $state=Get-Content -LiteralPath $statePath -Raw|ConvertFrom-Json
 if(-not $state.created -or -not $state.prepared -or $state.root.Length -ne 56){throw 'Long checkout was not shortened to the tested boundary'}
 if((Git-Test @('-C',$state.root,'rev-parse','HEAD')) -ne $commit){throw 'Pinned source differs'}
 if((Git-Test @('-C',$state.root,'rev-parse','--is-shallow-repository')) -ne 'true'){throw 'Shallow checkout handling differs'}
 if([IO.File]::ReadAllText((Join-Path $state.root 'fixture.txt')) -ne 'original source'){throw 'Working files differ'}
 # A short source containing whitespace must also be relocated, independently
 # of the length trigger. Its original source must survive cleanup.
 $spaceSource=Join-Path $fixture 'a b';$spaceState=Join-Path $fixture 'space-state.json'
 Git-Test @('clone','--quiet','--shared',$source,$spaceSource)|Out-Null
 if($spaceSource.Length -gt 56){throw 'Whitespace case must exercise a short source'}
 & $helper -SourceRoot $spaceSource -ExpectedCommit $commit -TemporaryRoot $fixture -StatePath $spaceState -Owner ($owner+'/space')|Out-Null
 $space=Get-Content -LiteralPath $spaceState -Raw|ConvertFrom-Json
 if(-not $space.created -or $space.root -match '\s' -or $space.root.Length -gt 56){throw 'Short whitespace checkout was not relocated'}
 & $helper -Mode Cleanup -SourceRoot $spaceSource -TemporaryRoot $fixture -StatePath $spaceState -Owner ($owner+'/space')|Out-Null
 if((Test-Path -LiteralPath $space.root) -or -not(Test-Path -LiteralPath (Join-Path $spaceSource 'fixture.txt'))){throw 'Whitespace cleanup did not preserve the original source'}
 # A target substitution must never remove an unrelated sibling.
 $tampered=Join-Path $fixture 'tampered.json';$bad=$state|Select-Object *;$bad.root=$outside
 [IO.File]::WriteAllText($tampered,($bad|ConvertTo-Json))
 $rejected=$false;try{& $helper -Mode Cleanup -SourceRoot $source -TemporaryRoot $fixture -StatePath $tampered -Owner $owner|Out-Null}catch{$rejected=$true}
 if(-not $rejected -or -not(Test-Path -LiteralPath (Join-Path $outside 'sentinel.txt'))){throw 'Unsafe cleanup target was accepted'}
 # Real Node/npm files exercise the PowerShell 5.1 path limit when supplied.
 if($NodeRuntimeZip){
  $app=Join-Path $state.root 'pinvou3-app';$destination=Join-Path $app ('src-tauri/target/windows-runtime/.tmp-'+('a'*32)+'/expanded/node')
  $resolver=(Resolve-Path (Join-Path $PSScriptRoot '../../pinvou3-app/src-tauri/packaging/windows/runtime/scripts/resolve-runtime.ps1')).Path
  $probe=Join-Path $fixture 'expand-real-node.ps1'
  $code="`$ErrorActionPreference='Stop'; . '"+$resolver.Replace("'","''")+"' -ImportFunctionsOnly; Expand-FlattenedRuntime -ZipPath '"+$NodeRuntimeZip.Replace("'","''")+"' -Destination '"+$destination.Replace("'","''")+"' -RequiredFile 'node.exe'"
  [IO.File]::WriteAllText($probe,$code,[Text.UTF8Encoding]::new($false))
  & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $probe
  if($LASTEXITCODE -ne 0 -or -not(Test-Path -LiteralPath (Join-Path $destination 'node.exe'))){throw 'Real PowerShell 5.1 Node expansion failed'}
  if(-not(Test-Path -LiteralPath (Join-Path $destination 'node_modules/npm/bin/npm-cli.js'))){throw 'Real Node archive must include npm'}
  # Verify the complete copied file set and its deepest member, rather than
  # accepting an early node.exe copy as evidence for npm's nested paths.
  Add-Type -AssemblyName System.IO.Compression.FileSystem
  $archive=[IO.Compression.ZipFile]::OpenRead($NodeRuntimeZip)
  try {
   $nodeEntries=@($archive.Entries|Where-Object {$_.FullName -match '(^|/)node\.exe$'})
   if($nodeEntries.Count -ne 1){throw 'Node archive root is ambiguous'}
   $prefix=$nodeEntries[0].FullName.Substring(0,$nodeEntries[0].FullName.Length-'node.exe'.Length)
   $files=@($archive.Entries|Where-Object {$_.FullName.StartsWith($prefix,[StringComparison]::Ordinal) -and -not $_.FullName.EndsWith('/')})
   if(@(Get-ChildItem -LiteralPath $destination -File -Recurse).Count -ne $files.Count){throw 'Expanded Node/npm file count differs from archive'}
   foreach($entry in $files){
    $copied=Join-Path $destination $entry.FullName.Substring($prefix.Length).Replace('/','\')
    if(-not(Test-Path -LiteralPath $copied -PathType Leaf) -or (Get-Item -LiteralPath $copied).Length -ne $entry.Length){throw 'Expanded Node/npm member differs from archive'}
   }
   $deepest=$files|Sort-Object {$_.FullName.Length} -Descending|Select-Object -First 1
   $deepPath=Join-Path $destination $deepest.FullName.Substring($prefix.Length).Replace('/','\')
   $stream=$deepest.Open();$sha=[Security.Cryptography.SHA256]::Create()
   try{$expected=[BitConverter]::ToString($sha.ComputeHash($stream)).Replace('-','').ToLowerInvariant()}finally{$stream.Dispose();$sha.Dispose()}
   if((Get-FileHash -LiteralPath $deepPath -Algorithm SHA256).Hash.ToLowerInvariant() -cne $expected){throw 'Deepest Node/npm member content differs'}
   Write-Output "Real Node/npm copy: $($files.Count) files, deepest destination length $($deepPath.Length), TEMP length $($tempBase.Length)."
  } finally {$archive.Dispose()}
 }
 # A naturally short checkout is reused and must survive cleanup.
 $shortState=Join-Path $fixture 'short-state.json'
 & $helper -SourceRoot $state.root -ExpectedCommit $commit -TemporaryRoot $fixture -StatePath $shortState -Owner ($owner+'/second')|Out-Null
 $second=Get-Content -LiteralPath $shortState -Raw|ConvertFrom-Json
 if($second.created -or $second.root -ne $state.root){throw 'Short checkout was unnecessarily copied'}
 & $helper -Mode Cleanup -SourceRoot $state.root -TemporaryRoot $fixture -StatePath $shortState -Owner ($owner+'/second')|Out-Null
 if(-not(Test-Path -LiteralPath $state.root)){throw 'Original short checkout was deleted'}
 # Cleanup must unlink a junction without following it into unrelated data.
 New-Item -ItemType Junction -Path (Join-Path $state.root 'outside-link') -Target $outside|Out-Null
 & $helper -Mode Cleanup -SourceRoot $source -TemporaryRoot $fixture -StatePath $statePath -Owner $owner|Out-Null
 if((Test-Path -LiteralPath $state.root) -or -not(Test-Path -LiteralPath (Join-Path $outside 'sentinel.txt')) -or -not(Test-Path -LiteralPath (Join-Path $source 'fixture.txt'))){throw 'Cleanup removed unrelated data or retained the checkout'}
 Write-Output 'Windows release workspace: pinning, short paths and cleanup isolation passed.'
 if($NodeRuntimeZip){Write-Output 'Real Node/npm archive expansion passed under Windows PowerShell 5.1.'}
} finally {
 if($spaceState -and (Test-Path -LiteralPath $spaceState)){
  $pendingSpace=Get-Content -LiteralPath $spaceState -Raw|ConvertFrom-Json
  if($pendingSpace.created -and ((Test-Path -LiteralPath $pendingSpace.root) -or (Test-Path -LiteralPath ($pendingSpace.root+'.owner.json')))){
   & $helper -Mode Cleanup -SourceRoot $spaceSource -TemporaryRoot $fixture -StatePath $spaceState -Owner ($owner+'/space')|Out-Null
  }
 }
 if((Test-Path -LiteralPath $statePath) -and $state -and $state.created -and (Test-Path -LiteralPath $state.root)){
  & $helper -Mode Cleanup -SourceRoot $source -TemporaryRoot $fixture -StatePath $statePath -Owner $owner|Out-Null
 }
 foreach($key in $savedEnv.Keys){[Environment]::SetEnvironmentVariable($key,$savedEnv[$key])}
 $resolved=[IO.Path]::GetFullPath($fixture)
 if([IO.Path]::GetDirectoryName($resolved) -ine $fixtureParent -or (Split-Path -Leaf $resolved) -cne $label){throw 'Unexpected workspace fixture cleanup path'}
 if(Test-Path -LiteralPath $resolved){
  if((Get-Item -LiteralPath $resolved -Force).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Workspace fixture root became a reparse point'}
  Remove-Item -LiteralPath $resolved -Recurse -Force
 }
}
