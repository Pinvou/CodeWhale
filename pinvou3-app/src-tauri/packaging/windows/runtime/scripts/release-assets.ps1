# Release transport for the immutable manifest selected by the parent gitlink.
# No credentials, latest-release lookup, or Git LFS downloads are used here.
function Assert-RuntimeReleaseUri {
  param([Uri]$Uri)
  if ($Uri.Scheme -cne 'https' -or -not $Uri.IsDefaultPort -or
      $Uri.UserInfo -or $Uri.Host -notin @('github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com', 'github-releases.githubusercontent.com')) {
    throw 'Windows runtime downloads require an approved GitHub HTTPS endpoint.'
  }
}

function Get-RuntimeReleaseProxy {
  param([System.Collections.IDictionary]$Environment = [Environment]::GetEnvironmentVariables())
  $setting = [string]$Environment['HTTPS_PROXY']
  if (-not $setting) { $setting = [string]$Environment['https_proxy'] }
  if (-not $setting) { return $null }
  $uri = $null
  if (-not [Uri]::TryCreate($setting, [UriKind]::Absolute, [ref]$uri) -or $uri.Scheme -notin @('http', 'https')) {
    throw 'Windows runtime HTTPS_PROXY must specify an HTTP(S) proxy.'
  }
  $address = [UriBuilder]::new($uri)
  $address.UserName = ''; $address.Password = ''
  $proxy = [Net.WebProxy]::new($address.Uri)
  if ($uri.UserInfo) {
    $parts = $uri.UserInfo.Split(':', 2)
    $name = [Uri]::UnescapeDataString($parts[0])
    $secret = if ($parts.Count -gt 1) { [Uri]::UnescapeDataString($parts[1]) } else { '' }
    $proxy.Credentials = [Net.NetworkCredential]::new($name, $secret)
  }
  $noProxy = [string]$Environment['NO_PROXY']
  if (-not $noProxy) { $noProxy = [string]$Environment['no_proxy'] }
  $bypass = @()
  foreach ($value in $noProxy.Split(',')) {
    $entry = $value.Trim()
    if (-not $entry) { continue }
    if ($entry -eq '*') { $bypass += '.*'; continue }
    if ($entry -match '^(?<host>\*?\.?[A-Za-z0-9_.-]+)(?::(?<port>[0-9]+))?$') {
      $hostName = $Matches['host'].TrimStart('*', '.')
      $scheme = 'https?://'
      $port = '(?::[0-9]+)?'
      if ($Matches['port']) {
        $number = [int]$Matches['port']
        if ($number -eq 443) { $scheme = 'https://'; $port = '(?::443)?' }
        elseif ($number -eq 80) { $scheme = 'http://'; $port = '(?::80)?' }
        else { $port = ':' + [Regex]::Escape($Matches['port']) }
      }
      $bypass += '^' + $scheme + '(?:[^/:]+\.)*' + [Regex]::Escape($hostName) + $port + '$'
    }
  }
  $proxy.BypassList = [string[]]$bypass
  return $proxy
}

function Write-RuntimeReleaseDownload {
  param([Uri]$Uri, [string]$Destination)
  Add-Type -AssemblyName System.Net.Http
  $handler = New-Object System.Net.Http.HttpClientHandler
  $handler.AllowAutoRedirect = $false
  # .NET Framework does not consume HTTPS_PROXY automatically, unlike the
  # existing Git/Node download tools. Preserve the Runner's explicit routing.
  $proxy = Get-RuntimeReleaseProxy
  if ($null -ne $proxy) { $handler.Proxy = $proxy; $handler.UseProxy = $true }
  $client = New-Object System.Net.Http.HttpClient($handler)
  $client.Timeout = [TimeSpan]::FromMinutes(10)
  $client.DefaultRequestHeaders.UserAgent.ParseAdd('pinvou-windows-runtime/1')
  $previousTls = [Net.ServicePointManager]::SecurityProtocol
  [Net.ServicePointManager]::SecurityProtocol = $previousTls -bor [Net.SecurityProtocolType]::Tls12
  try {
    for ($redirect = 0; $redirect -le 5; $redirect++) {
      Assert-RuntimeReleaseUri -Uri $Uri
      $response = $client.GetAsync($Uri, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
      try {
        $status = [int]$response.StatusCode
        if ($status -in @(301, 302, 303, 307, 308)) {
          if ($null -eq $response.Headers.Location) { throw 'Runtime asset redirect has no location.' }
          $Uri = [Uri]::new($Uri, $response.Headers.Location)
          continue
        }
        if ($status -ne 200) { throw "Runtime Release download failed with HTTP $status." }
        $input = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $output = [IO.File]::Open($Destination, [IO.FileMode]::CreateNew)
        try { $input.CopyTo($output) } finally { $output.Dispose(); $input.Dispose() }
        return
      } finally { $response.Dispose() }
    }
    throw 'Too many redirects downloading the pinned runtime Release asset.'
  } finally {
    [Net.ServicePointManager]::SecurityProtocol = $previousTls
    $client.Dispose()
    $handler.Dispose()
  }
}

function Get-RuntimeReleaseChildPath {
  param([string]$Root, [string]$Relative)
  if ($Relative -cnotmatch '^[A-Za-z0-9_-][A-Za-z0-9_./-]*$' -or
      @($Relative.Split('/') | Where-Object { $_ -in @('', '.', '..') }).Count -gt 0) {
    throw 'Invalid path in the pinned Windows runtime manifest.'
  }
  $base = [IO.Path]::GetFullPath($Root).TrimEnd('\', '/')
  $target = [IO.Path]::GetFullPath((Join-Path $base $Relative.Replace('/', [IO.Path]::DirectorySeparatorChar)))
  if (-not $target.StartsWith($base + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Windows runtime payload must remain inside its owned directory.'
  }
  $current = $target
  while ($current.Length -ge $base.Length) {
    if (Test-Path -LiteralPath $current) {
      if (((Get-Item -LiteralPath $current -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'Windows runtime payload/cache paths must not contain reparse points.'
      }
    }
    if ($current -eq $base) { break }
    $current = Split-Path -Parent $current
  }
  return $target
}

function Test-RuntimeReleaseFile {
  param([string]$Path, $Entry)
  if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
  if ((Get-Item -LiteralPath $Path).Length -ne [long]$Entry.bytes) { return $false }
  return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant() -ceq [string]$Entry.sha256
}

function Move-RuntimeReleaseFile {
  param([string]$Temporary, [string]$Destination)
  if (Test-Path -LiteralPath $Destination) {
    [IO.File]::Replace($Temporary, $Destination, [NullString]::Value)
  } else {
    [IO.File]::Move($Temporary, $Destination)
  }
}

function Sync-RuntimeReleaseAssets {
  param(
    [string]$RuntimeRoot, [string]$CacheRoot, $Lock, [string]$Commit,
    [switch]$OnnxOnly, [switch]$ForceDownload,
    [scriptblock]$Download = { param($Uri, $Destination) Write-RuntimeReleaseDownload -Uri $Uri -Destination $Destination }
  )
  if ($Commit -cnotmatch '^[a-f0-9]{40}$' -or [string]$Lock.source.commit -cne $Commit -or
      [string]$Lock.source.url -cnotmatch '^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\.git$') {
    throw 'Runtime Release must be bound to the locked GitHub repository and gitlink.'
  }
  $repository = $Matches[1]
  if ([string]$Lock.manifest.path -cne 'windows-runtime.manifest.json' -or
      [string]$Lock.manifest.sha256 -cnotmatch '^[a-f0-9]{64}$') {
    throw 'Invalid locked Windows runtime manifest identity.'
  }
  $manifestPath = Get-RuntimeReleaseChildPath -Root $RuntimeRoot -Relative 'windows-runtime.manifest.json'
  if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf) -or
      (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant() -cne [string]$Lock.manifest.sha256) {
    throw 'Runtime manifest SHA256 differs from the parent lock; no assets were downloaded.'
  }
  $manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
  if ([int]$manifest.schemaVersion -ne 2 -or [string]$manifest.target -cne 'windows-x86_64' -or @($manifest.files).Count -eq 0) {
    throw 'Unsupported runtime Release manifest.'
  }
  $names = @{}
  $entries = @($manifest.files)
  # Validate the entire file set before any network request or payload mutation.
  foreach ($entry in $entries) {
    if ([string]$entry.path -cnotmatch '^payload/' -or [string]$entry.sha256 -cnotmatch '^[a-f0-9]{64}$' -or
        [long]$entry.bytes -le 0 -or [long]$entry.bytes -ge 2GB) {
      throw 'Invalid runtime Release payload identity.'
    }
    $null = Get-RuntimeReleaseChildPath -Root $RuntimeRoot -Relative ([string]$entry.path)
    $name = [IO.Path]::GetFileName([string]$entry.path)
    if ($names.ContainsKey($name)) { throw 'Runtime Release asset names must be unique.' }
    $names[$name] = $true
  }
  if ($OnnxOnly) {
    $entries = @($entries | Where-Object { [string]$_.component -ceq 'onnxruntime' })
    if ($entries.Count -ne 1) { throw 'The runtime manifest must declare exactly one ONNX asset.' }
  }
  $tag = 'runtime-' + $Commit
  foreach ($entry in $entries) {
    $target = Get-RuntimeReleaseChildPath -Root $RuntimeRoot -Relative ([string]$entry.path)
    if (-not $ForceDownload -and (Test-RuntimeReleaseFile -Path $target -Entry $entry)) { continue }
    $cached = Get-RuntimeReleaseChildPath -Root $CacheRoot -Relative ([string]$entry.sha256)
    New-Item -ItemType Directory -Path $CacheRoot -Force | Out-Null
    if ($ForceDownload -or -not (Test-RuntimeReleaseFile -Path $cached -Entry $entry)) {
      $temporary = $cached + '.download-' + [Guid]::NewGuid().ToString('N')
      try {
        $name = [IO.Path]::GetFileName([string]$entry.path)
        $uri = [Uri]("https://github.com/$repository/releases/download/$tag/$name")
        & $Download $uri $temporary
        if (-not (Test-RuntimeReleaseFile -Path $temporary -Entry $entry)) {
          throw "Pinned runtime Release asset size/SHA256 mismatch: $name"
        }
        Move-RuntimeReleaseFile -Temporary $temporary -Destination $cached
      } finally {
        if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
      }
    }
    New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
    $temporary = $target + '.materialize-' + [Guid]::NewGuid().ToString('N')
    try {
      [IO.File]::Copy($cached, $temporary, $false)
      if (-not (Test-RuntimeReleaseFile -Path $temporary -Entry $entry)) { throw 'Runtime cache changed while copying.' }
      Move-RuntimeReleaseFile -Temporary $temporary -Destination $target
    } finally {
      if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
    }
  }
  Write-Host ("Verified Windows runtime Release assets: {0}; tag: {1}" -f $entries.Count, $tag)
}
