param(
  [Parameter(Mandatory = $true)]
  [string]$FilePath,

  [string]$TimestampUrl = $env:PINVOU3_WOSIGN_TIMESTAMP_URL,
  [switch]$ValidateOnly
)

$ErrorActionPreference = "Stop"

function Read-DotEnvValue {
  param(
    [string[]]$Lines,
    [string]$SourceDescription,
    [string]$Name
  )

  foreach ($sourceLine in $Lines) {
    $line = ([string]$sourceLine).TrimStart([char]0xFEFF).Trim()
    if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith("#")) {
      continue
    }
    if ($line.StartsWith("export ")) {
      $line = $line.Substring("export ".Length).TrimStart()
    }

    $separator = $line.IndexOf("=")
    if ($separator -lt 1 -or $line.Substring(0, $separator).Trim() -ne $Name) {
      continue
    }

    $rawValue = $line.Substring($separator + 1).Trim()
    if ($rawValue.StartsWith("'") -or $rawValue.StartsWith('"')) {
      $quote = $rawValue[0]
      $closing = $rawValue.LastIndexOf($quote)
      if ($closing -le 0 -or $rawValue.Substring($closing + 1) -notmatch '^\s*(?:#.*)?$') {
        throw "The value format for $Name in the private secrets file is invalid."
      }
      return $rawValue.Substring(1, $closing - 1)
    }

    return ([regex]::Replace($rawValue, '\s+#.*$', '')).Trim()
  }

  throw "The private secrets source does not define $Name`: $SourceDescription"
}

function Get-PrivateSecretsSource {
  param([string]$Path)

  if (Test-Path -LiteralPath $Path -PathType Leaf) {
    return [pscustomobject]@{
      Lines = @(Get-Content -Encoding UTF8 -LiteralPath $Path)
      Description = $Path
    }
  }

  $gitCommand = Get-Command "git.exe" -ErrorAction SilentlyContinue |
    Select-Object -First 1
  if ($null -eq $gitCommand) {
    $gitCommand = Get-Command "git" -ErrorAction SilentlyContinue |
      Select-Object -First 1
  }
  if ($null -eq $gitCommand) {
    throw "The private secrets file is unavailable and Git was not found."
  }

  $credentialRefs = @(
    "refs/remotes/origin/zwp/builtin-secrets-20260727",
    "refs/remotes/gitlab/zwp/builtin-secrets-20260727"
  )
  foreach ($credentialRef in $credentialRefs) {
    $sourceSpec = "$credentialRef`:scripts/.builtin-secrets.env"
    $previousErrorActionPreference = $ErrorActionPreference
    try {
      $ErrorActionPreference = "Continue"
      & $gitCommand.Source cat-file -e $sourceSpec 2>$null
      $sourceExistsExitCode = $LASTEXITCODE
      if ($sourceExistsExitCode -ne 0) {
        continue
      }
      $sourceLines = @(& $gitCommand.Source show $sourceSpec 2>$null)
      $gitExitCode = $LASTEXITCODE
    } finally {
      $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($gitExitCode -eq 0 -and $sourceLines.Count -gt 0) {
      return [pscustomobject]@{
        Lines = $sourceLines
        Description = $sourceSpec
      }
    }
  }

  throw "The private secrets file is unavailable and the configured GitLab credential ref was not fetched."
}

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot "../../../../../.."))
$SecretsPath = Join-Path $repoRoot "scripts/.builtin-secrets.env"
$privateSecrets = Get-PrivateSecretsSource -Path $SecretsPath
$Thumbprint = Read-DotEnvValue `
  -Lines $privateSecrets.Lines `
  -SourceDescription $privateSecrets.Description `
  -Name "PINVOU3_WOSIGN_THUMBPRINT"
$Password = Read-DotEnvValue `
  -Lines $privateSecrets.Lines `
  -SourceDescription $privateSecrets.Description `
  -Name "PINVOU3_WOSIGN_PASSWORD"

if ([string]::IsNullOrWhiteSpace($Password)) {
  throw "The configured WoSign password must not be empty."
}

function Normalize-Thumbprint {
  param([string]$Value)

  return ([regex]::Replace([string]$Value, "[^0-9A-Fa-f]", "")).ToUpperInvariant()
}

function Resolve-SignToolPath {
  $signToolCommand = Get-Command "signtool.exe" -ErrorAction SilentlyContinue |
    Select-Object -First 1
  if ($null -ne $signToolCommand -and
      -not [string]::IsNullOrWhiteSpace([string]$signToolCommand.Source) -and
      (Test-Path -LiteralPath $signToolCommand.Source -PathType Leaf)) {
    return [System.IO.Path]::GetFullPath($signToolCommand.Source)
  }

  $candidatePaths = [System.Collections.Generic.List[string]]::new()
  foreach ($environmentVariableName in @("WindowsSdkVerBinPath", "WindowsSdkBinPath")) {
    $sdkBinPath = [Environment]::GetEnvironmentVariable($environmentVariableName)
    if ([string]::IsNullOrWhiteSpace($sdkBinPath)) {
      continue
    }
    $candidatePaths.Add((Join-Path $sdkBinPath "signtool.exe"))
    $candidatePaths.Add((Join-Path $sdkBinPath "x64\signtool.exe"))
  }

  $windowsSdkDirectory = [Environment]::GetEnvironmentVariable("WindowsSdkDir")
  if (-not [string]::IsNullOrWhiteSpace($windowsSdkDirectory)) {
    $windowsSdkBinDirectory = Join-Path $windowsSdkDirectory "bin"
    $candidatePaths.Add((Join-Path $windowsSdkBinDirectory "x64\signtool.exe"))
    if (Test-Path -LiteralPath $windowsSdkBinDirectory -PathType Container) {
      Get-ChildItem -LiteralPath $windowsSdkBinDirectory -Directory -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending |
        ForEach-Object {
          $candidatePaths.Add((Join-Path $_.FullName "x64\signtool.exe"))
        }
    }
  }

  $programFilesX86 = [Environment]::GetEnvironmentVariable("ProgramFiles(x86)")
  if (-not [string]::IsNullOrWhiteSpace($programFilesX86)) {
    $windowsKitsBinDirectory = Join-Path $programFilesX86 "Windows Kits\10\bin"
    if (Test-Path -LiteralPath $windowsKitsBinDirectory -PathType Container) {
      Get-ChildItem -LiteralPath $windowsKitsBinDirectory -Directory -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending |
        ForEach-Object {
          $candidatePaths.Add((Join-Path $_.FullName "x64\signtool.exe"))
        }
    }
  }

  foreach ($candidatePath in $candidatePaths) {
    if (Test-Path -LiteralPath $candidatePath -PathType Leaf) {
      return [System.IO.Path]::GetFullPath($candidatePath)
    }
  }

  throw "Windows SDK SignTool was not found. Install the Windows SDK signing tools or add signtool.exe to PATH."
}

function Assert-AuthenticodeSignature {
  param(
    [string]$Path,
    [string]$SignToolPath,
    [string]$ExpectedThumbprint
  )

  $verifyArguments = @("verify", "/pa", "/all", "/v", "/tw", $Path)
  Write-Host "Authenticode policy and timestamp verification started: $Path"
  & $SignToolPath @verifyArguments
  $verifyExitCode = $LASTEXITCODE
  if ($verifyExitCode -ne 0) {
    throw "SignTool verification failed with exit code $verifyExitCode for: $Path"
  }

  try {
    $embeddedCertificate =
      [System.Security.Cryptography.X509Certificates.X509Certificate]::CreateFromSignedFile(
        $Path
      )
    $signerCertificate =
      [System.Security.Cryptography.X509Certificates.X509Certificate2]::new(
        $embeddedCertificate
      )
  } catch {
    throw "Unable to read the embedded signer certificate after signing: $Path ($($_.Exception.Message))"
  }

  $normalizedSignerThumbprint = Normalize-Thumbprint -Value $signerCertificate.Thumbprint
  if ($normalizedSignerThumbprint -ne $ExpectedThumbprint) {
    throw "Embedded signer thumbprint does not match the configured certificate: $Path"
  }

  Write-Host "Authenticode policy, timestamp, and signer thumbprint verified: $normalizedSignerThumbprint"
}

$normalizedThumbprint = Normalize-Thumbprint -Value $Thumbprint
if ($normalizedThumbprint -notmatch "^[0-9A-F]{40}$") {
  throw "The configured WoSign certificate thumbprint must contain 40 hexadecimal characters."
}

if ([string]::IsNullOrWhiteSpace($TimestampUrl)) {
  $TimestampUrl = "http://timestamp.digicert.com"
}

$resolvedToolPath = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot "wosigncodecmd.exe"))
if (-not (Test-Path -LiteralPath $resolvedToolPath -PathType Leaf)) {
  throw "WoSign command-line tool was not found: $resolvedToolPath"
}

$companionToolPath = Join-Path ([System.IO.Path]::GetDirectoryName($resolvedToolPath)) "wosigncode.exe"
if (-not (Test-Path -LiteralPath $companionToolPath -PathType Leaf)) {
  throw "WoSign companion tool was not found: $companionToolPath"
}
$toolDirectory = [System.IO.Path]::GetDirectoryName($resolvedToolPath)
$signToolPath = Resolve-SignToolPath

$resolvedFilePath = [System.IO.Path]::GetFullPath($FilePath)
if (-not (Test-Path -LiteralPath $resolvedFilePath -PathType Leaf)) {
  throw "File to sign was not found: $resolvedFilePath"
}

if ($ValidateOnly) {
  Push-Location -LiteralPath $toolDirectory
  try {
    & $resolvedToolPath "help" 2>&1 | Out-Null
    $validationExitCode = $LASTEXITCODE
  } finally {
    Pop-Location
  }
  if ($validationExitCode -ne 0) {
    throw "WoSign command-line tool validation failed with exit code $validationExitCode."
  }

  Write-Host "Windows SDK SignTool available at: $signToolPath"
  Write-Host "WoSign configuration validated for: $resolvedFilePath"
  exit 0
}

$signArguments = @(
  "sign",
  "/tp", $normalizedThumbprint,
  "/p", $Password,
  "/hide",
  "/isf",
  "/c",
  "/dig", "sha256",
  "/tr", $TimestampUrl,
  "/file", $resolvedFilePath
)

Write-Host "WoSign signing started: $resolvedFilePath"
Push-Location -LiteralPath $toolDirectory
try {
  & $resolvedToolPath @signArguments
  $signExitCode = $LASTEXITCODE
} finally {
  Pop-Location
}
Write-Host "WoSign command exited with code $signExitCode for: $resolvedFilePath"
if ($signExitCode -ne 0) {
  throw "WoSign failed with exit code $signExitCode while signing: $resolvedFilePath"
}

Assert-AuthenticodeSignature `
  -Path $resolvedFilePath `
  -SignToolPath $signToolPath `
  -ExpectedThumbprint $normalizedThumbprint
Write-Host "WoSign signing completed: $resolvedFilePath"
