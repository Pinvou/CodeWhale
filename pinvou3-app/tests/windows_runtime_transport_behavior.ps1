# Invoked within the Release harness's owned process and fixture directory.
# Controlled handlers exercise the real production Write loop. A separate real
# loopback probe exercises cancellation of HttpClient's actual response stream.
Add-Type -AssemblyName System.Net.Http
if (-not ('Pinvou.RuntimeRelease.Tests.Handler' -as [type])) {
  if ($PSVersionTable.PSVersion.Major -lt 6) {
    Add-Type -Path (Join-Path $PSScriptRoot 'windows_runtime_transport_fixture.cs') -ReferencedAssemblies @([Net.Http.HttpClient].Assembly.Location, 'System.dll', 'System.Core.dll')
  } else {
    Add-Type -Path (Join-Path $PSScriptRoot 'windows_runtime_transport_fixture.cs')
  }
}
$originalFactory = ${function:New-RuntimeReleaseHttpClient}
try {
  function New-RuntimeReleaseHttpClient {
    $script:transportHandler = [Pinvou.RuntimeRelease.Tests.Handler]::new($script:transportMode)
    $client = [Net.Http.HttpClient]::new($script:transportHandler)
    # Give normal fixture responses time for cold PowerShell/.NET startup.
    # Deliberate stalls still exercise the original 300 ms deadline.
    $client.Timeout = [TimeSpan]::FromMilliseconds($(if ($script:transportMode -in @('body-stall', 'headers-stall')) { 300 } else { 2000 }))
    return $client
  }
  foreach ($mode in @('ok', 'oversize', 'short', 'body-stall', 'headers-stall', 'redirect', 'unsafe', 'loop', '404', 'ok')) {
    $script:transportMode = $mode
    $destination = Join-Path $testRoot ('transport-' + [Guid]::NewGuid().ToString('N'))
    $clock = [Diagnostics.Stopwatch]::StartNew()
    $failed = $false
    $exceptionType = 'none'
    $previousTls = [Net.ServicePointManager]::SecurityProtocol
    try { Write-RuntimeReleaseDownload -Uri 'https://github.com/example/asset' -Destination $destination -ExpectedBytes 3 }
    catch { $failed = $true; $exceptionType = $_.Exception.GetBaseException().GetType().Name }
    $clock.Stop()
    Assert-True ($failed -eq ($mode -notin @('ok', 'redirect'))) "Production transport outcome differs: $mode ($exceptionType, $($clock.ElapsedMilliseconds) ms)"
    Assert-True ($clock.Elapsed.TotalSeconds -lt 5) "Transport did not honor its deadline: $mode"
    Assert-True ($script:transportHandler.Disposed) "HTTP handler was not disposed: $mode"
    Assert-True ([Net.ServicePointManager]::SecurityProtocol -eq $previousTls) 'TLS state must be restored after all exits'
    if ($script:transportHandler.Body) { Assert-True $script:transportHandler.Body.Disposed "Response body was not closed: $mode" }
    if ($mode -eq 'unsafe') { Assert-True ($script:transportHandler.Requests -eq 1) 'Untrusted redirect must fail before the next request' }
    if ($mode -eq 'loop') { Assert-True ($script:transportHandler.Requests -eq 6) 'Redirect request bound must remain six' }
    if (Test-Path -LiteralPath $destination) {
      Assert-True ((Get-Item -LiteralPath $destination).Length -le 3) 'No over-limit byte may be written'
      if (-not $failed) { Assert-True ([IO.File]::ReadAllText($destination) -ceq 'abc') 'Production bytes differ' }
      # Exclusive re-open proves no output handle survives failure or success.
      $probe = [IO.File]::Open($destination, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
      $probe.Dispose()
      Remove-Item -LiteralPath $destination
    }
  }
  # The real Sync default callback must pass the manifest byte budget, preserve
  # old verified files and remove partial downloads before the next invocation.
  . (Join-Path $PSScriptRoot '../src-tauri/packaging/windows/runtime/scripts/resolve-runtime.ps1') -ImportFunctionsOnly
  $stagingParent = $testRoot
  $resolverLockPath = Join-Path $testRoot '.transport-resolver.lock'
  $transportRoot = Join-Path $testRoot 'transport-runtime'
  $transportCache = Join-Path $testRoot 'transport-cache'
  New-Item -ItemType Directory -Path $transportRoot -ErrorAction Stop | Out-Null
  $hash = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'
  $manifestFile = Join-Path $transportRoot 'windows-runtime.manifest.json'
  $manifest = @{schemaVersion=2;target='windows-x86_64';files=@(@{path='payload/asset.zip';component='node';bytes=3;sha256=$hash})}
  [IO.File]::WriteAllText($manifestFile, ($manifest | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
  $transportLock = @{source=@{url='https://github.com/example/runtime.git';commit=('c'*40)};manifest=@{path='windows-runtime.manifest.json';sha256=(Get-FileHash -LiteralPath $manifestFile -Algorithm SHA256).Hash.ToLowerInvariant()}}
  $sync = @{RuntimeRoot=$transportRoot;CacheRoot=$transportCache;Lock=$transportLock;Commit=('c'*40)}
  foreach ($mode in @('ok', 'body-stall', 'oversize', 'ok')) {
    $script:transportMode = $mode
    $lease = Enter-ResolverLock -TimeoutSeconds 0
    try {
      if ($mode -eq 'ok') { Sync-RuntimeReleaseAssets @sync -ForceDownload }
      else { Assert-Rejected { Sync-RuntimeReleaseAssets @sync -ForceDownload } "Sync must reject $mode" }
    } finally { $lease.Dispose() }
    Assert-True ([IO.File]::ReadAllText((Join-Path $transportRoot 'payload/asset.zip')) -ceq 'abc') 'Failed Sync must preserve the old verified target'
    Assert-True ([IO.File]::ReadAllText((Join-Path $transportCache $hash)) -ceq 'abc') 'Failed Sync must preserve the old verified cache'
    Assert-True (@(Get-ChildItem -LiteralPath $transportCache -Filter '*.download-*').Count -eq 0) 'Failed Sync must retire partial downloads'
    $nextLease = Enter-ResolverLock -TimeoutSeconds 0; $nextLease.Dispose()
  }
} finally { ${function:New-RuntimeReleaseHttpClient} = $originalFactory }

$server = [Pinvou.RuntimeRelease.Tests.Loopback]::new()
$handler = [Net.Http.HttpClientHandler]::new(); $handler.UseProxy = $false
$client = [Net.Http.HttpClient]::new($handler); $client.Timeout = [TimeSpan]::FromSeconds(5)
$response = $null; $body = $null; $output = [IO.MemoryStream]::new()
$deadline = [Threading.CancellationTokenSource]::new()
try {
  $response = $client.GetAsync($server.Uri, [Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
  $body = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
  $deadline.CancelAfter(300)
  $clock = [Diagnostics.Stopwatch]::StartNew()
  Assert-Rejected { [Pinvou.RuntimeRelease.Body]::Copy($body, $output, 3, $deadline.Token) } 'Real stalled HttpClient body must be cancelled'
  Assert-True ($clock.Elapsed.TotalSeconds -lt 5 -and $output.Length -le 3) 'Real body deadline or size boundary failed'
} finally {
  if ($body) { $body.Dispose() }; if ($response) { $response.Dispose() }
  $output.Dispose(); $client.Dispose(); $deadline.Dispose(); $server.Dispose()
}
Write-Host 'Production HTTP loop: bounded bytes, header/body deadlines, redirect rejection, disposal and next-call recovery passed; real loopback body cancellation passed.'
