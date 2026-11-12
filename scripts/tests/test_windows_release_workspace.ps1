#Requires -Version 7.0
param([string]$NodeRuntimeZip)
$ErrorActionPreference='Stop'
$helper=Join-Path $PSScriptRoot '../windows-release-workspace.ps1'
$tempBase=[IO.Path]::GetTempPath().TrimEnd('\')
$labelLength=48-$tempBase.Length-1
if($labelLength -lt 12){throw 'Test requires a temporary parent shorter than 36 characters'}
# A 48-character parent forces the created checkout to the supported 56-character limit.
$label=('rwt-'+[Guid]::NewGuid().ToString('N')+[Guid]::NewGuid().ToString('N')).Substring(0,$labelLength)
$fixture=Join-Path $tempBase $label
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
 if((Test-Path -LiteralPath $statePath) -and $state -and $state.created -and (Test-Path -LiteralPath $state.root)){
  & $helper -Mode Cleanup -SourceRoot $source -TemporaryRoot $fixture -StatePath $statePath -Owner $owner|Out-Null
 }
 foreach($key in $savedEnv.Keys){[Environment]::SetEnvironmentVariable($key,$savedEnv[$key])}
 $resolved=[IO.Path]::GetFullPath($fixture);$temp=[IO.Path]::GetFullPath([IO.Path]::GetTempPath())
 if($resolved.StartsWith($temp,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path -Leaf $resolved) -ceq $label){Remove-Item -LiteralPath $resolved -Recurse -Force -ErrorAction SilentlyContinue}
}
