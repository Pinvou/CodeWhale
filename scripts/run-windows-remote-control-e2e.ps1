[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$AComputerName,
    [Parameter(Mandatory = $true)][string]$BComputerName,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$CredentialFile,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$InstallerPath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$JourneyDriverPath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$LegacyRelayBinaryPath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$EndpointInventoryPath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$GatewayConfigPath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$InstallerAttestationBundlePath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$InstallerSbomPath,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$InstallerSbomAttestationBundlePath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')][string]$InstallerBuildRepository,
    [Parameter(Mandatory = $true)][ValidatePattern('^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$')][string]$InstallerBuildWorkflow,
    [Parameter(Mandatory = $true)][ValidateRange(1, [long]::MaxValue)][long]$InstallerBuildRunId,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-F0-9]{40,64}$')][string]$TrustedSignerThumbprint,
    [Parameter(Mandatory = $true)][ValidateNotNullOrEmpty()][string]$TrustedSignerPublisher,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$TrustedNodePath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$TrustedNodeSha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-F0-9]{40,64}$')][string]$TrustedNodeSignerThumbprint,
    [Parameter(Mandatory = $true)][ValidateNotNullOrEmpty()][string]$TrustedNodeSignerPublisher,
    [Parameter(Mandatory = $true)][ValidateScript({ [System.IO.Path]::IsPathRooted($_) -and [System.IO.File]::Exists($_) })][string]$TrustedGhPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{40}$')][string]$ExpectedSourceCommit,
    [Parameter(Mandatory = $true)][ValidateScript({ [System.IO.Path]::IsPathRooted($_) -and [System.IO.Directory]::Exists($_) })][string]$RepositoryRoot,
    [Parameter(Mandatory = $true)][ValidatePattern('^wss://')][string]$RelayUrl,
    [Parameter(Mandatory = $true)][ValidatePattern('^wss://')][string]$DisabledRelayUrl,
    [Parameter(Mandatory = $true)][ValidatePattern('^wss://')][string]$LegacyGatewayUrl,
    [Parameter(Mandatory = $true)][ValidateSet('official', 'self_hosted')][string]$EnrollmentDeployment,
    [Parameter(Mandatory = $true)][ValidateSet('remote_signing_service')][string]$AttestationProvider,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._:/-]{2,127}$')][string]$AttestationKeyId,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$AttestationAclSha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$AttestationProviderMeasurementSha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$OrchestratorRolePolicySha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$ARolePolicySha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$BRolePolicySha256,
    [Parameter(Mandatory = $true)][ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })][string]$ProviderVerifierPath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9 ._-]{1,63}$')][string]$AttestationProviderVendor,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$TrustedProviderRootCertificateSha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$TrustedProviderVerifierCertificateSha256,
    [Parameter(Mandatory = $true)][string]$EvidenceDirectory
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$RepoRoot = [System.IO.Path]::GetFullPath($RepositoryRoot)
$JourneyCatalog = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'docs', 'schemas', 'remote-control-journeys.v1.json')
$EvidenceSchema = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'docs', 'schemas', 'remote-control-release-evidence.v1.schema.json')
$EvidenceSchemaValidator = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'docs', 'schemas', 'remote-control-release-evidence.v1.validator.mjs')
$EvidenceValidator = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'scripts', 'validate-controller-release-evidence.mjs')
$UiSmokeScript = [System.IO.Path]::Combine($RepoRoot, 'pinvou3-app', 'scripts', 'remote-control-ui-smoke.mjs')
$ProcessGuardScript = [System.IO.Path]::Combine($PSScriptRoot, 'windows-e2e-process-guard.ps1')
$OrchestratorHelperScript = [System.IO.Path]::Combine($PSScriptRoot, 'windows-e2e-orchestrator-helpers.ps1')
$RemoteBase = 'C:\ProgramData\PinvouE2E'
$TimestampFormat = 'yyyy-MM-ddTHH:mm:ss.fffZ'
$RouteSpecPreparer = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'scripts', 'prepare-legacy-gateway-route.mjs')
$ProviderAttestationVerifier = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'scripts', 'verify-provider-attestation.mjs')
$InstallerSupplyChainVerifier = [System.IO.Path]::Combine($RepoRoot, 'remote-control-relay', 'scripts', 'verify-installer-supply-chain.mjs')

function Assert-PublicRelayUrl([string]$Value) {
    $uri = [Uri]$Value
    if ($uri.Scheme -ne 'wss' -or $uri.IsLoopback -or $uri.Host -in @('localhost', '127.0.0.1', '::1') `
        -or -not [string]::IsNullOrEmpty($uri.UserInfo) -or -not [string]::IsNullOrEmpty($uri.Query) `
        -or -not [string]::IsNullOrEmpty($uri.Fragment)) { throw 'e2e_relay_not_production_equivalent' }
}

function Get-PinvouFileSha256([string]$Path) {
    (Get-FileHash -LiteralPath ([System.IO.Path]::GetFullPath($Path)) -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-PinvouSha256Hex {
    param([Parameter(Mandatory = $true)][byte[]]$Bytes)
    $algorithm = [System.Security.Cryptography.SHA256]::Create()
    try { ([System.BitConverter]::ToString($algorithm.ComputeHash($Bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $algorithm.Dispose() }
}

function Assert-PinvouPathInside([string]$Root, [string]$Candidate, [string]$ErrorCode) {
    $fullRoot = [System.IO.Path]::GetFullPath($Root).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
    $fullCandidate = [System.IO.Path]::GetFullPath($Candidate)
    $rootUri = [System.Uri]::new($fullRoot + [System.IO.Path]::DirectorySeparatorChar)
    $candidateUri = [System.Uri]::new($fullCandidate)
    $relativeUri = $rootUri.MakeRelativeUri($candidateUri)
    $relative = [System.Uri]::UnescapeDataString($relativeUri.ToString()).Replace('/', [System.IO.Path]::DirectorySeparatorChar)
    if ($relativeUri.IsAbsoluteUri -or [System.IO.Path]::IsPathRooted($relative) -or $relative -eq '..' `
        -or $relative.StartsWith(('..' + [System.IO.Path]::DirectorySeparatorChar), [System.StringComparison]::Ordinal)) { throw $ErrorCode }
    $fullCandidate
}

function Assert-PinvouJourneyElapsed {
    param([long]$ObservedMilliseconds, [long]$TerminalElapsedMilliseconds, [long]$ToleranceMilliseconds = 1500)
    if ($ObservedMilliseconds -le 0 -or $TerminalElapsedMilliseconds -le 0 -or [Math]::Abs($ObservedMilliseconds - $TerminalElapsedMilliseconds) -gt $ToleranceMilliseconds) {
        throw 'e2e_monotonic_elapsed_mismatch'
    }
}

function Assert-PinvouRouteStateBinding {
    param([string]$CanonicalInputDigest, [string]$ControlPlaneBeforeDigest, [string]$ConfiguredDigest, [string]$RestoredControlPlaneDigest, [string]$RestoredOriginalDigest, [string]$RestoredStateDigest)
    if ($CanonicalInputDigest -notmatch '^[a-f0-9]{64}$' -or $ControlPlaneBeforeDigest -notmatch '^[a-f0-9]{64}$') { throw 'e2e_legacy_gateway_original_state_mismatch' }
    $payload = [Text.Encoding]::UTF8.GetBytes((@('pinvou-gateway-state-v1', $CanonicalInputDigest, $ControlPlaneBeforeDigest) | ConvertTo-Json -Compress))
    $expected = Get-PinvouSha256Hex $payload
    if ($ConfiguredDigest -ne $expected -or $RestoredControlPlaneDigest -ne $ControlPlaneBeforeDigest -or $RestoredOriginalDigest -ne $expected -or $RestoredStateDigest -ne $expected) {
        throw 'e2e_legacy_gateway_original_state_mismatch'
    }
}

function Get-PinvouAuthenticodeIdentity([string]$Path, [string]$ExpectedThumbprint, [string]$ExpectedPublisher) {
    $signature = Get-AuthenticodeSignature -LiteralPath $Path
    if ($signature.Status -ne 'Valid' -or -not $signature.SignerCertificate) { throw 'e2e_artifact_signature_invalid' }
    $thumbprint = $signature.SignerCertificate.Thumbprint.ToUpperInvariant(); $publisher = $signature.SignerCertificate.Subject
    if ($thumbprint -ne $ExpectedThumbprint -or $publisher -ne $ExpectedPublisher) { throw 'e2e_artifact_untrusted_signer' }
    [pscustomobject]@{ Thumbprint = $thumbprint; Publisher = $publisher }
}

function Get-TrustedSignature([string]$Path) {
    Get-PinvouAuthenticodeIdentity $Path $TrustedSignerThumbprint $TrustedSignerPublisher
}

function Get-PinvouFileVersion([string]$Path) {
    [Diagnostics.FileVersionInfo]::GetVersionInfo([IO.Path]::GetFullPath($Path)).ProductVersion
}

function Invoke-PinvouLocalWhitelistedProcess {
    param([Parameter(Mandatory = $true)][string]$FilePath, [string[]]$Arguments = @(), [hashtable]$PurposeEnvironment = @{})
    if (-not [System.IO.Path]::IsPathRooted($FilePath)) { throw 'e2e_local_process_path_not_absolute' }
    $info = [System.Diagnostics.ProcessStartInfo]::new()
    $info.FileName = [System.IO.Path]::GetFullPath($FilePath)
    $info.UseShellExecute = $false; $info.CreateNoWindow = $true
    $info.RedirectStandardOutput = $true; $info.RedirectStandardError = $true
    $info.Environment.Clear()
    foreach ($name in @('PATH', 'Path', 'PATHEXT', 'SystemRoot', 'SYSTEMROOT', 'WINDIR', 'TEMP', 'TMP')) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if ($value -and -not $info.Environment.ContainsKey($name)) { $info.Environment[$name] = $value }
    }
    foreach ($entry in $PurposeEnvironment.GetEnumerator()) {
        if ($entry.Key -match '(?i)(password|passwd|secret|token|cookie|private.?key|credential|username|user_name)') { throw 'e2e_process_environment_sensitive_key_forbidden' }
        $info.Environment[[string]$entry.Key] = [string]$entry.Value
    }
    $quoted = foreach ($argumentValue in $Arguments) {
        $argument = [string]$argumentValue
        if ($argument.Length -gt 0 -and $argument -notmatch '[\s"]') { $argument; continue }
        $builder = [Text.StringBuilder]::new('"'); $backslashes = 0
        foreach ($character in $argument.ToCharArray()) {
            if ($character -eq '\') { $backslashes += 1; continue }
            if ($character -eq '"') { [void]$builder.Append(('\' * ($backslashes * 2 + 1))); [void]$builder.Append('"') }
            else { [void]$builder.Append(('\' * $backslashes)); [void]$builder.Append($character) }
            $backslashes = 0
        }
        [void]$builder.Append(('\' * ($backslashes * 2))); [void]$builder.Append('"'); $builder.ToString()
    }
    $info.Arguments = $quoted -join ' '
    $process = [System.Diagnostics.Process]::new(); $process.StartInfo = $info
    if (-not $process.Start()) { throw 'e2e_process_start_failed' }
    $stdout = $process.StandardOutput.ReadToEnd(); $stderr = $process.StandardError.ReadToEnd(); $process.WaitForExit()
    [pscustomobject]@{ ExitCode = $process.ExitCode; StdOut = $stdout; StdErr = $stderr; ProcessId = $process.Id }
}

function Invoke-PinvouVerifiedLocalProcess {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$ExpectedSha256,
        [Parameter(Mandatory = $true)][ValidatePattern('^[A-F0-9]{40,64}$')][string]$ExpectedThumbprint,
        [Parameter(Mandatory = $true)][string]$ExpectedPublisher,
        [string[]]$Arguments = @(), [hashtable]$PurposeEnvironment = @{},
        [hashtable]$AdditionalExpectedSha256 = @{}
    )
    $absolute = [System.IO.Path]::GetFullPath($FilePath)
    if ((Get-PinvouFileSha256 $absolute) -ne $ExpectedSha256) { throw 'e2e_local_process_hash_mismatch' }
    $signature = Get-AuthenticodeSignature -LiteralPath $absolute
    if ($signature.Status -ne 'Valid' -or -not $signature.SignerCertificate `
        -or $signature.SignerCertificate.Thumbprint.ToUpperInvariant() -ne $ExpectedThumbprint `
        -or $signature.SignerCertificate.Subject -ne $ExpectedPublisher) { throw 'e2e_local_process_signature_mismatch' }
    foreach ($entry in $AdditionalExpectedSha256.GetEnumerator()) {
        if ((Get-PinvouFileSha256 ([string]$entry.Key)) -ne [string]$entry.Value) { throw 'e2e_local_dependency_hash_mismatch' }
    }
    Invoke-PinvouLocalWhitelistedProcess -FilePath $absolute -Arguments $Arguments -PurposeEnvironment $PurposeEnvironment
}

function Assert-PinvouStrictJsonElement {
    param([System.Text.Json.JsonElement]$Element)
    if ($Element.ValueKind -eq [System.Text.Json.JsonValueKind]::Object) {
        $names = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
        foreach ($property in $Element.EnumerateObject()) {
            if (-not $names.Add($property.Name)) { throw 'e2e_json_duplicate_property' }
            Assert-PinvouStrictJsonElement $property.Value
        }
    } elseif ($Element.ValueKind -eq [System.Text.Json.JsonValueKind]::Array) {
        foreach ($item in $Element.EnumerateArray()) { Assert-PinvouStrictJsonElement $item }
    }
}

function ConvertFrom-PinvouStrictJsonBytes {
    param([Parameter(Mandatory = $true)][byte[]]$Bytes)
    $options = [System.Text.Json.JsonDocumentOptions]::new()
    $options.AllowTrailingCommas = $false
    $options.CommentHandling = [System.Text.Json.JsonCommentHandling]::Disallow
    $options.MaxDepth = 64
    $document = [System.Text.Json.JsonDocument]::Parse([ReadOnlyMemory[byte]]::new($Bytes), $options)
    try { Assert-PinvouStrictJsonElement $document.RootElement } finally { $document.Dispose() }
    $utf8 = [System.Text.UTF8Encoding]::new($false, $true)
    Microsoft.PowerShell.Utility\ConvertFrom-Json -InputObject $utf8.GetString($Bytes)
}

function Assert-PinvouExactJsonProperties {
    param([Parameter(Mandatory = $true)]$Value, [Parameter(Mandatory = $true)][string[]]$Names, [Parameter(Mandatory = $true)][string]$ErrorCode)
    if ($null -eq $Value -or $Value -is [Array] -or $Value -is [string] -or $Value -is [ValueType]) { throw $ErrorCode }
    $expected = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($name in $Names) { if (-not $expected.Add($name)) { throw $ErrorCode } }
    $count = 0
    foreach ($property in $Value.PSObject.Properties) { $count += 1; if (-not $expected.Remove([string]$property.Name)) { throw $ErrorCode } }
    if ($count -ne $Names.Count -or $expected.Count -ne 0) { throw $ErrorCode }
}

# REMOTE_STAGING_SCRIPT_BEGIN
$RemoteCreateStaging = {
    param([string]$BaseRoot, [string]$RunNonce, [datetime]$StartedAtUtc)
    $ErrorActionPreference = 'Stop'
    if ($RunNonce -notmatch '^[a-f0-9]{64}$') { throw 'e2e_staging_nonce_invalid' }
    $base = [System.IO.Path]::GetFullPath($BaseRoot).TrimEnd([System.IO.Path]::DirectorySeparatorChar)
    [void][System.IO.Directory]::CreateDirectory($base)
    if ((([System.IO.File]::GetAttributes($base)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_staging_base_reparse_point' }
    $target = [System.IO.Path]::Combine($base, $RunNonce)
    $baseUri = [System.Uri]::new($base + [System.IO.Path]::DirectorySeparatorChar)
    $relativeUri = $baseUri.MakeRelativeUri([System.Uri]::new($target))
    $relative = [System.Uri]::UnescapeDataString($relativeUri.ToString()).Replace('/', [System.IO.Path]::DirectorySeparatorChar)
    if ($relativeUri.IsAbsoluteUri -or [System.IO.Path]::IsPathRooted($relative) -or $relative -ne $RunNonce) { throw 'e2e_staging_path_escape' }
    if ([System.IO.Directory]::Exists($target) -or [System.IO.File]::Exists($target)) { throw 'e2e_staging_already_exists' }
    $temporary = [System.IO.Path]::Combine($base, ('.init-' + $RunNonce))
    try {
        if ([System.IO.Directory]::Exists($temporary) -or [System.IO.File]::Exists($temporary)) { throw 'e2e_staging_temporary_exists' }
        [void][System.IO.Directory]::CreateDirectory($temporary)
        if ((([System.IO.File]::GetAttributes($temporary)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_staging_temporary_reparse_point' }
        $marker = [ordered]@{ schema = 'pinvou-e2e-staging'; version = 1; run_nonce = $RunNonce; started_at = $StartedAtUtc.ToUniversalTime().ToString('O') }
        [System.IO.File]::WriteAllText(
            [System.IO.Path]::Combine($temporary, '.pinvou-e2e-owner.json'),
            (($marker | ConvertTo-Json -Compress) + "`n"),
            [System.Text.UTF8Encoding]::new($false)
        )
        [void][System.IO.Directory]::CreateDirectory([System.IO.Path]::Combine($temporary, 'output'))
        [System.IO.Directory]::Move($temporary, $target); $temporary = $null
        $global:PinvouE2ERunRoot = $target; $global:PinvouE2EOutput = [System.IO.Path]::Combine($target, 'output'); $global:PinvouE2ERunNonce = $RunNonce
        [pscustomobject]@{ root = $target; output = $global:PinvouE2EOutput }
    } finally {
        if ($temporary -and [System.IO.Directory]::Exists($temporary)) { [System.IO.Directory]::Delete($temporary, $true) }
    }
}
# REMOTE_STAGING_SCRIPT_END

# REMOTE_BOOTSTRAP_SCRIPT_BEGIN
$RemoteBootstrap = {
    param([string]$Root, [string]$ExpectedFilesJson, [string]$AppThumbprint, [string]$AppPublisher, [string]$NodeThumbprint, [string]$NodePublisher)
    $ErrorActionPreference = 'Stop'
    if ($global:PinvouE2ERunRoot -ne $Root) { throw 'e2e_staging_session_binding_lost' }
    $rootPath = [System.IO.Path]::GetFullPath($Root).TrimEnd([System.IO.Path]::DirectorySeparatorChar)
    if (-not [System.IO.Directory]::Exists($rootPath) -or (([System.IO.File]::GetAttributes($rootPath)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_bootstrap_root_reparse_point' }
    $rootUri = [System.Uri]::new($rootPath + [System.IO.Path]::DirectorySeparatorChar)
    try { $expectedFiles = @(($ExpectedFilesJson | ConvertFrom-Json)) } catch { throw 'e2e_bootstrap_manifest_invalid' }
    if (-not $expectedFiles -or $expectedFiles.Count -gt 32) { throw 'e2e_bootstrap_manifest_invalid' }
    $hashes = @{}
    foreach ($expected in $expectedFiles) {
        $relativePath = [string]$expected.RelativePath; $expectedSha = [string]$expected.Sha256; $signaturePolicy = [string]$expected.SignaturePolicy
        if ($hashes.ContainsKey($relativePath) -or $expectedSha -notmatch '^[a-f0-9]{64}$' -or $signaturePolicy -notin @('none', 'app', 'node')) { throw 'e2e_bootstrap_manifest_invalid' }
        $candidate = [System.IO.Path]::GetFullPath([System.IO.Path]::Combine($rootPath, $relativePath))
        $relativeUri = $rootUri.MakeRelativeUri([System.Uri]::new($candidate))
        $normalized = [System.Uri]::UnescapeDataString($relativeUri.ToString()).Replace('/', [System.IO.Path]::DirectorySeparatorChar)
        if ($relativeUri.IsAbsoluteUri -or [System.IO.Path]::IsPathRooted($normalized) -or $normalized -eq '..' -or $normalized.StartsWith(('..' + [System.IO.Path]::DirectorySeparatorChar), [System.StringComparison]::Ordinal)) { throw 'e2e_bootstrap_path_escape' }
        $cursor = $rootPath
        foreach ($part in $normalized.Split([System.IO.Path]::DirectorySeparatorChar, [System.StringSplitOptions]::RemoveEmptyEntries)) {
            $cursor = [System.IO.Path]::Combine($cursor, $part)
            if (-not [System.IO.File]::Exists($cursor) -and -not [System.IO.Directory]::Exists($cursor)) { throw 'e2e_bootstrap_file_missing' }
            if ((([System.IO.File]::GetAttributes($cursor)) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_bootstrap_reparse_point' }
        }
        if (-not [System.IO.File]::Exists($candidate)) { throw 'e2e_bootstrap_file_missing' }
        if ((Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expectedSha) { throw ('e2e_bootstrap_hash_mismatch:' + $relativePath) }
        if ($signaturePolicy -ne 'none') {
            $signature = Microsoft.PowerShell.Security\Get-AuthenticodeSignature -LiteralPath $candidate
            $expectedThumbprint = if ($signaturePolicy -eq 'node') { $NodeThumbprint } else { $AppThumbprint }
            $expectedPublisher = if ($signaturePolicy -eq 'node') { $NodePublisher } else { $AppPublisher }
            if ($signature.Status -ne 'Valid' -or -not $signature.SignerCertificate `
                -or $signature.SignerCertificate.Thumbprint.ToUpperInvariant() -ne $expectedThumbprint `
                -or $signature.SignerCertificate.Subject -ne $expectedPublisher) { throw ('e2e_bootstrap_signature_mismatch:' + $relativePath) }
        }
        $hashes[$relativePath] = $expectedSha
    }
    . ([System.IO.Path]::Combine($rootPath, 'windows-e2e-orchestrator-helpers.ps1'))
    . ([System.IO.Path]::Combine($rootPath, 'windows-e2e-process-guard.ps1'))
    $global:PinvouE2EExpectedFileHashes = $hashes
    $global:PinvouE2ENode = [System.IO.Path]::Combine($rootPath, 'pinvou-node.exe')
    $global:PinvouE2ENodeSha256 = $hashes['pinvou-node.exe']; $global:PinvouE2ENodeSignerThumbprint = $NodeThumbprint; $global:PinvouE2ENodeSignerPublisher = $NodePublisher
    $global:PinvouE2EHelperSha256 = $hashes['windows-e2e-orchestrator-helpers.ps1']; $global:PinvouE2EProcessGuardSha256 = $hashes['windows-e2e-process-guard.ps1']; $global:PinvouE2EUiSmokeSha256 = $hashes['remote-control-ui-smoke.mjs']
    @{ verified_file_count = $hashes.Count; helper_loaded = $true }
}
# REMOTE_BOOTSTRAP_SCRIPT_END

function Add-LocalArtifact([Collections.Generic.List[object]]$Artifacts, [string]$Root, [string]$RelativePath, [string]$Kind, [string]$MachineRole = 'orchestrator') {
    if ($MachineRole -notin @('A', 'B', 'orchestrator')) { throw 'e2e_artifact_role_invalid' }
    $path = Assert-PinvouPathInside $Root (Join-Path $Root $RelativePath) 'e2e_local_artifact_path_invalid'
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw ('e2e_artifact_missing:' + $RelativePath) }
    $sha = Get-PinvouFileSha256 $path
    $Artifacts.Add([ordered]@{ path = $RelativePath.Replace('\', '/'); sha256 = $sha; kind = $Kind; machine_role = $MachineRole; signature = '' })
    $sha
}

function Copy-VerifiedRemoteArtifact($Session, [string]$RemotePath, [string]$Root, [string]$RelativePath, [string]$Kind, [Collections.Generic.List[object]]$Artifacts, [string]$MachineRole) {
    [void](Assert-PinvouPathInside $RemoteOutput $RemotePath 'e2e_remote_artifact_path_invalid')
    Invoke-Command -Session $Session -ScriptBlock {
        param($AllowedRoot, $Candidate)
        $root = [IO.Path]::GetFullPath($AllowedRoot).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
        $path = [IO.Path]::GetFullPath($Candidate)
        $rootItem = Get-Item -LiteralPath $root -Force -ErrorAction Stop
        if (($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_remote_artifact_reparse_point' }
        $rootUri = [Uri]::new($root + [IO.Path]::DirectorySeparatorChar)
        $candidateUri = [Uri]::new($path)
        $relativeUri = $rootUri.MakeRelativeUri($candidateUri)
        $relative = [Uri]::UnescapeDataString($relativeUri.ToString()).Replace('/', [IO.Path]::DirectorySeparatorChar)
        if ($relativeUri.IsAbsoluteUri -or $relative -eq '..' -or $relative.StartsWith(('..' + [IO.Path]::DirectorySeparatorChar), [StringComparison]::Ordinal) -or [IO.Path]::IsPathRooted($relative)) { throw 'e2e_remote_artifact_path_invalid' }
        $cursor = $root
        foreach ($part in $relative.Split([IO.Path]::DirectorySeparatorChar, [StringSplitOptions]::RemoveEmptyEntries)) {
            $cursor = Join-Path $cursor $part
            $item = Get-Item -LiteralPath $cursor -Force -ErrorAction Stop
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_remote_artifact_reparse_point' }
        }
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw 'e2e_remote_artifact_missing' }
    } -ArgumentList $RemoteOutput, $RemotePath
    $localPath = Assert-PinvouPathInside $Root (Join-Path $Root $RelativePath) 'e2e_local_artifact_path_invalid'
    New-Item -ItemType Directory -Path (Split-Path -Parent $localPath) -Force | Out-Null
    Copy-Item -LiteralPath $RemotePath -Destination $localPath -FromSession $Session
    Add-LocalArtifact $Artifacts $Root $RelativePath $Kind $MachineRole
}

Assert-PublicRelayUrl $RelayUrl
Assert-PublicRelayUrl $DisabledRelayUrl
Assert-PublicRelayUrl $LegacyGatewayUrl
$ghExecutable = [System.IO.Path]::GetFullPath($TrustedGhPath)
$actualHead = $ExpectedSourceCommit
if ($actualHead -notmatch '^[a-f0-9]{40}$' -or $env:GITHUB_SHA -and $env:GITHUB_SHA -ne $actualHead) { throw 'e2e_source_head_mismatch' }
$ghVersion = Invoke-PinvouLocalWhitelistedProcess -FilePath $ghExecutable -Arguments @('--version')
if ($ghVersion.ExitCode -ne 0 -or $ghVersion.StdOut -notmatch '(?m)^gh version \d+\.') { throw 'e2e_gh_tool_unavailable' }
$trustedSourceHashes = @{}
$trustedSourceBytes = @{}
foreach ($trustedSource in @(
    @($JourneyCatalog, ''), @($EvidenceSchema, ''), @($EvidenceSchemaValidator, ''), @($EvidenceValidator, ''), @($UiSmokeScript, ''),
    @($ProcessGuardScript, 'scripts/windows-e2e-process-guard.ps1'),
    @($OrchestratorHelperScript, 'scripts/windows-e2e-orchestrator-helpers.ps1'),
    @($RouteSpecPreparer, ''), @($ProviderAttestationVerifier, ''), @($InstallerSupplyChainVerifier, '')
)) {
    $trustedAbsolute = [System.IO.Path]::GetFullPath([string]$trustedSource[0])
    $item = Get-Item -LiteralPath $trustedAbsolute -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_source_file_invalid' }
    $trustedSourceHashes[$trustedAbsolute] = Get-PinvouFileSha256 $trustedAbsolute
    $trustedSourceBytes[$trustedAbsolute] = [IO.File]::ReadAllBytes($trustedAbsolute)
}
$routeSourceHashes = @{ $RouteSpecPreparer = $trustedSourceHashes[$RouteSpecPreparer] }
$providerSourceHashes = @{ $ProviderAttestationVerifier = $trustedSourceHashes[$ProviderAttestationVerifier] }
$supplyChainSourceHashes = @{ $InstallerSupplyChainVerifier = $trustedSourceHashes[$InstallerSupplyChainVerifier] }
$validatorSourceHashes = @{
    $EvidenceValidator = $trustedSourceHashes[$EvidenceValidator]
    $ProviderAttestationVerifier = $trustedSourceHashes[$ProviderAttestationVerifier]
    $InstallerSupplyChainVerifier = $trustedSourceHashes[$InstallerSupplyChainVerifier]
    $JourneyCatalog = $trustedSourceHashes[$JourneyCatalog]
    $EvidenceSchema = $trustedSourceHashes[$EvidenceSchema]
    $EvidenceSchemaValidator = $trustedSourceHashes[$EvidenceSchemaValidator]
}
$runNonceBytes = [byte[]]::new(32)
[Security.Cryptography.RandomNumberGenerator]::Fill($runNonceBytes)
$runNonce = ([BitConverter]::ToString($runNonceBytes)).Replace('-', '').ToLowerInvariant()
$runStartedAt = [DateTime]::UtcNow.ToString($TimestampFormat)
$RemoteRoot = Join-Path $RemoteBase $runNonce
$RemoteOutput = Join-Path $RemoteRoot 'output'
$runClock = [Diagnostics.Stopwatch]::StartNew()
$installer = (Resolve-Path -LiteralPath $InstallerPath).Path
$driver = (Resolve-Path -LiteralPath $JourneyDriverPath).Path
$legacyRelay = (Resolve-Path -LiteralPath $LegacyRelayBinaryPath).Path
$inventory = (Resolve-Path -LiteralPath $EndpointInventoryPath).Path
$gatewayConfig = (Resolve-Path -LiteralPath $GatewayConfigPath).Path
$buildAttestationBundle = (Resolve-Path -LiteralPath $InstallerAttestationBundlePath).Path
$installerSbom = (Resolve-Path -LiteralPath $InstallerSbomPath).Path
$installerSbomAttestationBundle = (Resolve-Path -LiteralPath $InstallerSbomAttestationBundlePath).Path
$providerVerifier = (Resolve-Path -LiteralPath $ProviderVerifierPath).Path
$trustedNode = (Resolve-Path -LiteralPath $TrustedNodePath).Path
if ([IO.Path]::GetFileName($trustedNode) -ne 'node.exe') { throw 'e2e_trusted_node_name_invalid' }
$installerSigner = Get-TrustedSignature $installer
$driverSigner = Get-TrustedSignature $driver
$providerVerifierSigner = Get-TrustedSignature $providerVerifier
$nodeSignature = Get-PinvouAuthenticodeIdentity $trustedNode $TrustedNodeSignerThumbprint $TrustedNodeSignerPublisher
$actualNodeSha256 = Get-PinvouFileSha256 $trustedNode
if ($actualNodeSha256 -ne $TrustedNodeSha256) { throw 'e2e_trusted_node_hash_mismatch' }
$trustedNodeVersion = Get-PinvouFileVersion $trustedNode
if ($trustedNodeVersion -notmatch '^\d+\.\d+\.\d+(?:\.\d+)?$') { throw 'e2e_trusted_node_version_invalid' }
$installerSha = Get-PinvouFileSha256 $installer
$driverSha = Get-PinvouFileSha256 $driver
$providerVerifierSha = Get-PinvouFileSha256 $providerVerifier
$uiSmokeSha = $trustedSourceHashes[[IO.Path]::GetFullPath($UiSmokeScript)]
$processGuardSha = $trustedSourceHashes[[IO.Path]::GetFullPath($ProcessGuardScript)]
$orchestratorHelperSha = $trustedSourceHashes[[IO.Path]::GetFullPath($OrchestratorHelperScript)]
$legacyRelaySha = Get-PinvouFileSha256 $legacyRelay
$inventorySha = Get-PinvouFileSha256 $inventory
$gatewayConfigSha = Get-PinvouFileSha256 $gatewayConfig
$gatewayUrlSha = Get-PinvouSha256Hex ([Text.Encoding]::UTF8.GetBytes(([Uri]$LegacyGatewayUrl).AbsoluteUri))
$buildAttestationBundleSha = Get-PinvouFileSha256 $buildAttestationBundle
$installerSbomSha = Get-PinvouFileSha256 $installerSbom
$installerSbomAttestationBundleSha = Get-PinvouFileSha256 $installerSbomAttestationBundle
$installerVersion = Get-PinvouFileVersion $installer
$driverVersion = Get-PinvouFileVersion $driver
$legacyRelayVersion = Get-PinvouFileVersion $legacyRelay
if ($installerVersion -notmatch '^\d+\.\d+\.\d+(?:\.\d+)?$' -or $driverVersion -notmatch '^\d+\.\d+\.\d+(?:\.\d+)?$' -or $legacyRelayVersion -notmatch '^[A-Za-z0-9._+-]{1,64}$') { throw 'e2e_binary_version_invalid' }
$releaseVersion = (Get-Content -LiteralPath (Join-Path $RepoRoot 'VERSION') -Raw -Encoding UTF8).Trim()
if ($installerVersion -ne $releaseVersion) { throw 'e2e_installer_source_version_mismatch' }
$evidenceRoot = [IO.Path]::GetFullPath($EvidenceDirectory)
if (Test-Path -LiteralPath $evidenceRoot) { throw 'e2e_evidence_directory_exists' }
New-Item -ItemType Directory -Path $evidenceRoot | Out-Null
New-Item -ItemType Directory -Path (Join-Path $evidenceRoot 'build') -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $evidenceRoot 'rollback') -Force | Out-Null
$sessions = @()
$legacyRuntimePid = $null
$legacyConfiguration = $null
$routeSpec = $null
try {
$routeSpecPath = Join-Path $evidenceRoot 'rollback\legacy-route-spec.json'
$routePrepare = Invoke-PinvouVerifiedLocalProcess -FilePath $trustedNode -ExpectedSha256 $TrustedNodeSha256 -ExpectedThumbprint $TrustedNodeSignerThumbprint -ExpectedPublisher $TrustedNodeSignerPublisher -Arguments @($RouteSpecPreparer, '--inventory', $inventory, '--config', $gatewayConfig, '--gateway-url', $LegacyGatewayUrl, '--run-nonce', $runNonce, '--output', $routeSpecPath) -AdditionalExpectedSha256 $routeSourceHashes
if ($routePrepare.ExitCode -ne 0) { throw 'e2e_legacy_gateway_inputs_invalid' }
$routeSpecBytes = [IO.File]::ReadAllBytes($routeSpecPath)
$legacyRouteSpecSha = Get-PinvouSha256Hex $routeSpecBytes
$routeSpec = ConvertFrom-PinvouStrictJsonBytes $routeSpecBytes
Assert-PinvouExactJsonProperties $routeSpec @('schema', 'version', 'run_nonce', 'route_id', 'gateway_url_sha256', 'target', 'no_bypass', 'endpoint_inventory_sha256', 'gateway_config_sha256', 'canonical_inventory_sha256', 'canonical_gateway_config_sha256', 'canonical_input_state_digest') 'e2e_legacy_gateway_route_spec_invalid'
if ($routeSpec.schema -ne 'pinvou-legacy-gateway-route-spec' -or $routeSpec.version -ne 1 -or $routeSpec.route_id -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$' `
    -or $routeSpec.endpoint_inventory_sha256 -ne $inventorySha -or $routeSpec.gateway_config_sha256 -ne $gatewayConfigSha -or $routeSpec.gateway_url_sha256 -ne $gatewayUrlSha `
    -or $routeSpec.canonical_inventory_sha256 -notmatch '^[a-f0-9]{64}$' -or $routeSpec.canonical_gateway_config_sha256 -notmatch '^[a-f0-9]{64}$' -or $routeSpec.canonical_input_state_digest -notmatch '^[a-f0-9]{64}$' `
    -or $routeSpec.run_nonce -ne $runNonce -or $routeSpec.no_bypass -ne $true -or $routeSpec.target -ne 'legacy_relay') { throw 'e2e_legacy_gateway_route_spec_invalid' }
$routeSpecText = [Text.UTF8Encoding]::new($false, $true).GetString($routeSpecBytes)
if ($routeSpecText -match '(?i)(password|secret|token|cookie|credential|authorization|private_key)') { throw 'e2e_legacy_gateway_route_spec_sensitive' }

$orchestratorKeyId = "$AttestationKeyId/orchestrator"
$driverProvenanceResult = Invoke-PinvouVerifiedLocalProcess -FilePath $driver -ExpectedSha256 $driverSha -ExpectedThumbprint $TrustedSignerThumbprint -ExpectedPublisher $TrustedSignerPublisher -Arguments @('provenance', '--json', '--attestation-provider', $AttestationProvider, '--key-id', $orchestratorKeyId, '--role', 'orchestrator', '--role-policy-sha256', $OrchestratorRolePolicySha256)
$driverProvenance = $driverProvenanceResult.StdOut | ConvertFrom-Json
if ($driverProvenanceResult.ExitCode -ne 0 -or $driverProvenance.schema -ne 'pinvou-e2e-driver-provenance' -or $driverProvenance.source_commit_sha -ne $actualHead -or $driverProvenance.sha256 -ne $driverSha -or $driverProvenance.mock -ne $false -or $driverProvenance.attestation_identity -ne 'production' -or $driverProvenance.attestation_public_key_sha256 -notmatch '^[a-f0-9]{64}$' -or $driverProvenance.attestation_public_key_spki_pem -notmatch '^-----BEGIN PUBLIC KEY-----' `
    -or $driverProvenance.attestation_provider -ne $AttestationProvider -or $driverProvenance.attestation_key_id -ne $orchestratorKeyId `
    -or $driverProvenance.non_exportable -ne $true -or $driverProvenance.private_key_embedded -ne $false `
    -or $driverProvenance.acl_attestation_sha256 -ne $AttestationAclSha256 `
    -or $driverProvenance.provider_measurement_sha256 -ne $AttestationProviderMeasurementSha256 `
    -or $driverProvenance.supports_provider_key_handles -ne $true -or $driverProvenance.provider_attestation_path) { throw 'e2e_driver_provenance_invalid' }
$providerReceiptPath = Join-Path $evidenceRoot 'build\attestation-provider.receipt.json'
$providerResult = Invoke-PinvouVerifiedLocalProcess -FilePath $providerVerifier -ExpectedSha256 $providerVerifierSha -ExpectedThumbprint $TrustedSignerThumbprint -ExpectedPublisher $TrustedSignerPublisher -Arguments @('attest-provider', '--role', 'orchestrator', '--role-policy-sha256', $OrchestratorRolePolicySha256, '--provider', $AttestationProvider, '--key-id', $orchestratorKeyId, '--run-nonce', $runNonce, '--driver-sha256', $driverSha, '--output', $providerReceiptPath)
if ($providerResult.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $providerReceiptPath -PathType Leaf)) { throw 'e2e_provider_attestation_unavailable' }
$verifiedProviderPath = Join-Path $evidenceRoot 'build\attestation-provider.json'
$providerVerifyResult = Invoke-PinvouVerifiedLocalProcess -FilePath $trustedNode -ExpectedSha256 $TrustedNodeSha256 -ExpectedThumbprint $TrustedNodeSignerThumbprint -ExpectedPublisher $TrustedNodeSignerPublisher -Arguments @($ProviderAttestationVerifier, '--input', $providerReceiptPath, '--output', $verifiedProviderPath, '--role', 'orchestrator', '--role-policy-sha256', $OrchestratorRolePolicySha256, '--provider', $AttestationProvider, '--key-id', $orchestratorKeyId, '--run-nonce', $runNonce, '--acl-sha256', $AttestationAclSha256, '--measurement-sha256', $AttestationProviderMeasurementSha256, '--driver-sha256', $driverSha, '--vendor', $AttestationProviderVendor, '--root-certificate-sha256', $TrustedProviderRootCertificateSha256, '--verifier-certificate-sha256', $TrustedProviderVerifierCertificateSha256) -AdditionalExpectedSha256 $providerSourceHashes
if ($providerVerifyResult.ExitCode -ne 0) { throw 'e2e_provider_attestation_invalid' }
$orchestratorProviderIdentity = $providerVerifyResult.StdOut | ConvertFrom-Json
if ($orchestratorProviderIdentity.ok -ne $true -or $orchestratorProviderIdentity.role -ne 'orchestrator' -or $orchestratorProviderIdentity.provider -ne $AttestationProvider -or $orchestratorProviderIdentity.key_id -ne $orchestratorKeyId `
    -or $orchestratorProviderIdentity.public_key_sha256 -notmatch '^[a-f0-9]{64}$' -or $orchestratorProviderIdentity.public_key_spki_pem -notmatch '^-----BEGIN PUBLIC KEY-----') { throw 'e2e_provider_attestation_identity_invalid' }
Remove-Item -LiteralPath $providerReceiptPath -Force

$buildVerificationPath = Join-Path $evidenceRoot 'build\installer-provenance.json'
$buildVerificationResult = Invoke-PinvouVerifiedLocalProcess -FilePath $trustedNode -ExpectedSha256 $TrustedNodeSha256 -ExpectedThumbprint $TrustedNodeSignerThumbprint -ExpectedPublisher $TrustedNodeSignerPublisher -Arguments @($InstallerSupplyChainVerifier, '--installer', $installer, '--bundle', $buildAttestationBundle, '--sbom', $installerSbom, '--sbom-bundle', $installerSbomAttestationBundle, '--repository', $InstallerBuildRepository, '--workflow', $InstallerBuildWorkflow, '--run-id', [string]$InstallerBuildRunId, '--source-commit', $actualHead, '--gh-binary', $ghExecutable, '--output', $buildVerificationPath) -AdditionalExpectedSha256 $supplyChainSourceHashes
if ($buildVerificationResult.ExitCode -ne 0) { throw 'e2e_installer_supply_chain_invalid' }
$buildStatement = Get-Content -LiteralPath $buildVerificationPath -Raw -Encoding UTF8 | ConvertFrom-Json
if ($buildStatement.source_commit_sha -ne $actualHead -or $buildStatement.installer_sha256 -ne $installerSha -or $buildStatement.attestation_bundle_sha256 -ne $buildAttestationBundleSha -or $buildStatement.sbom_sha256 -ne $installerSbomSha -or $buildStatement.sbom_attestation_bundle_sha256 -ne $installerSbomAttestationBundleSha `
    -or $buildStatement.installer_subject_name -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{1,127}\.exe$' -or $buildStatement.sbom_subject_name -ne 'installer-sbom.spdx.json') { throw 'e2e_installer_supply_chain_invalid' }
$catalogBytes = [byte[]]$trustedSourceBytes[[IO.Path]::GetFullPath($JourneyCatalog)]
if ((Get-PinvouSha256Hex $catalogBytes) -ne $trustedSourceHashes[[IO.Path]::GetFullPath($JourneyCatalog)]) { throw 'e2e_journey_catalog_invalid' }
$catalog = ConvertFrom-PinvouStrictJsonBytes $catalogBytes
if ($catalog.schema -ne 'pinvou-remote-control-journey-catalog' -or $catalog.version -ne 1 -or $catalog.journeys.Count -ne 27) { throw 'e2e_journey_catalog_invalid' }

$artifacts = [Collections.Generic.List[object]]::new()
New-Item -ItemType Directory -Path (Join-Path $evidenceRoot 'binaries') -Force | Out-Null
Copy-Item -LiteralPath $installer -Destination (Join-Path $evidenceRoot 'binaries\pinvou-installer.exe')
Copy-Item -LiteralPath $driver -Destination (Join-Path $evidenceRoot 'binaries\pinvou-e2e-driver.exe')
Copy-Item -LiteralPath $legacyRelay -Destination (Join-Path $evidenceRoot 'binaries\legacy-relay.bin')
Copy-Item -LiteralPath $trustedNode -Destination (Join-Path $evidenceRoot 'binaries\pinvou-node.exe')
Copy-Item -LiteralPath $buildAttestationBundle -Destination (Join-Path $evidenceRoot 'build\installer-attestation.sigstore.jsonl')
Copy-Item -LiteralPath $installerSbom -Destination (Join-Path $evidenceRoot 'build\installer-sbom.spdx.json')
Copy-Item -LiteralPath $installerSbomAttestationBundle -Destination (Join-Path $evidenceRoot 'build\installer-sbom-attestation.sigstore.jsonl')
[void](Add-LocalArtifact $artifacts $evidenceRoot 'binaries\pinvou-installer.exe' 'installer')
[void](Add-LocalArtifact $artifacts $evidenceRoot 'binaries\pinvou-e2e-driver.exe' 'driver')
[void](Add-LocalArtifact $artifacts $evidenceRoot 'binaries\legacy-relay.bin' 'legacy-relay')
[void](Add-LocalArtifact $artifacts $evidenceRoot 'binaries\pinvou-node.exe' 'toolchain')
$buildProvenanceArtifactSha = Add-LocalArtifact $artifacts $evidenceRoot 'build\installer-provenance.json' 'check'
$buildAttestationBundleArtifactSha = Add-LocalArtifact $artifacts $evidenceRoot 'build\installer-attestation.sigstore.jsonl' 'check'
$installerSbomArtifactSha = Add-LocalArtifact $artifacts $evidenceRoot 'build\installer-sbom.spdx.json' 'log'
$installerSbomAttestationBundleArtifactSha = Add-LocalArtifact $artifacts $evidenceRoot 'build\installer-sbom-attestation.sigstore.jsonl' 'check'
$providerAttestationArtifactSha = Add-LocalArtifact $artifacts $evidenceRoot 'build\attestation-provider.json' 'check'
$attestationIdentities = [Collections.Generic.List[object]]::new()
$attestationIdentities.Add([ordered]@{ role = 'orchestrator'; role_policy_sha256 = $OrchestratorRolePolicySha256; identity = 'production'; algorithm = 'ed25519'; public_key_spki_pem = $orchestratorProviderIdentity.public_key_spki_pem; public_key_sha256 = $orchestratorProviderIdentity.public_key_sha256; run_nonce = $runNonce; provider = $AttestationProvider; key_id = $orchestratorKeyId; non_exportable = $true; acl_attestation_sha256 = $AttestationAclSha256; provider_measurement_sha256 = $AttestationProviderMeasurementSha256; provider_vendor = $AttestationProviderVendor; provider_root_certificate_sha256 = $TrustedProviderRootCertificateSha256; provider_verifier_certificate_sha256 = $TrustedProviderVerifierCertificateSha256; provider_attestation_artifact_sha256 = $providerAttestationArtifactSha })
$recordedLegacyRouteSpecSha = Add-LocalArtifact $artifacts $evidenceRoot 'rollback\legacy-route-spec.json' 'rollback'
if ($recordedLegacyRouteSpecSha -ne $legacyRouteSpecSha) { throw 'e2e_legacy_gateway_route_spec_changed' }
if ($buildAttestationBundleArtifactSha -ne $buildAttestationBundleSha -or $installerSbomArtifactSha -ne $installerSbomSha -or $installerSbomAttestationBundleArtifactSha -ne $installerSbomAttestationBundleSha) { throw 'e2e_installer_provenance_copy_mismatch' }

$journeyEvidence = [Collections.Generic.List[object]]::new()
$uiArtifacts = [Collections.Generic.List[object]]::new()
$flagOffEvidence = [Collections.Generic.List[object]]::new()
    $sessionCredential = Import-Clixml -LiteralPath $CredentialFile
    if ($sessionCredential -isnot [pscredential]) { throw 'e2e_credential_file_invalid' }
    try {
        $sessions += New-PSSession -ComputerName $AComputerName -Credential $sessionCredential
        $sessions += New-PSSession -ComputerName $BComputerName -Credential $sessionCredential
    } finally {
        $sessionCredential = $null
        Remove-Item -LiteralPath $CredentialFile -Force -ErrorAction Stop
    }
    $machineFacts = foreach ($index in 0..1) {
        Invoke-Command -Session $sessions[$index] -ScriptBlock {
            $uuid = (Get-CimInstance Win32_ComputerSystemProduct).UUID
            [pscustomobject]@{
                Role = @('A', 'B')[$using:index]
                IdentityDigest = $(
                    $algorithm = [Security.Cryptography.SHA256]::Create()
                    try { ([BitConverter]::ToString($algorithm.ComputeHash([Text.Encoding]::UTF8.GetBytes($uuid)))).Replace('-', '').ToLowerInvariant() }
                    finally { $algorithm.Dispose() }
                )
                WindowsBuild = [Environment]::OSVersion.Version.Build.ToString()
            }
        }
    }
    if ($machineFacts[0].IdentityDigest -eq $machineFacts[1].IdentityDigest) { throw 'e2e_machines_not_isolated' }

    foreach ($index in 0..1) {
        $session = $sessions[$index]
        $staging = Invoke-Command -Session $session -ScriptBlock $RemoteCreateStaging -ArgumentList $RemoteBase, $runNonce, ([DateTime]::Parse($runStartedAt))
        if ($staging.root -ne $RemoteRoot -or $staging.output -ne $RemoteOutput) { throw 'e2e_remote_staging_invalid' }
        Copy-Item -LiteralPath $installer -Destination "$RemoteRoot\pinvou-installer.exe" -ToSession $session
        Copy-Item -LiteralPath $driver -Destination "$RemoteRoot\pinvou-e2e-driver.exe" -ToSession $session
        foreach ($trustedRemoteSource in @(
            @('remote-control-ui-smoke.mjs', $UiSmokeScript),
            @('windows-e2e-process-guard.ps1', $ProcessGuardScript),
            @('windows-e2e-orchestrator-helpers.ps1', $OrchestratorHelperScript)
        )) {
            $trustedSourcePath = [IO.Path]::GetFullPath([string]$trustedRemoteSource[1])
            $trustedSourceBase64 = [Convert]::ToBase64String([byte[]]$trustedSourceBytes[$trustedSourcePath])
            Invoke-Command -Session $session -ScriptBlock {
                param($Destination, $Base64, $ExpectedSha256)
                $bytes = [Convert]::FromBase64String($Base64)
                $algorithm = [Security.Cryptography.SHA256]::Create()
                try { $actual = ([BitConverter]::ToString($algorithm.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() } finally { $algorithm.Dispose() }
                if ($actual -ne $ExpectedSha256) { throw 'e2e_committed_source_transfer_hash_mismatch' }
                [IO.File]::WriteAllBytes([IO.Path]::GetFullPath($Destination), $bytes)
            } -ArgumentList (Join-Path $RemoteRoot ([string]$trustedRemoteSource[0])), $trustedSourceBase64, $trustedSourceHashes[$trustedSourcePath]
        }
        Copy-Item -LiteralPath $providerVerifier -Destination "$RemoteRoot\pinvou-provider-verifier.exe" -ToSession $session
        Copy-Item -LiteralPath $trustedNode -Destination "$RemoteRoot\pinvou-node.exe" -ToSession $session
        if ($index -eq 0) {
            Copy-Item -LiteralPath $legacyRelay -Destination "$RemoteRoot\legacy-relay.exe" -ToSession $session
            $routeSpecBase64 = [Convert]::ToBase64String($routeSpecBytes)
            Invoke-Command -Session $session -ScriptBlock {
                param($Destination, $Base64, $ExpectedSha256)
                $bytes = [Convert]::FromBase64String($Base64)
                $algorithm = [Security.Cryptography.SHA256]::Create()
                try { $actual = ([BitConverter]::ToString($algorithm.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() } finally { $algorithm.Dispose() }
                if ($actual -ne $ExpectedSha256) { throw 'e2e_legacy_gateway_route_spec_transfer_mismatch' }
                [IO.File]::WriteAllBytes([IO.Path]::GetFullPath($Destination), $bytes)
            } -ArgumentList "$RemoteRoot\legacy-route-spec.json", $routeSpecBase64, $legacyRouteSpecSha
        }
        $expectedStagedFiles = @(
            [pscustomobject]@{ RelativePath = 'pinvou-installer.exe'; Sha256 = $installerSha; SignaturePolicy = 'app' },
            [pscustomobject]@{ RelativePath = 'pinvou-e2e-driver.exe'; Sha256 = $driverSha; SignaturePolicy = 'app' },
            [pscustomobject]@{ RelativePath = 'remote-control-ui-smoke.mjs'; Sha256 = $uiSmokeSha; SignaturePolicy = 'none' },
            [pscustomobject]@{ RelativePath = 'windows-e2e-process-guard.ps1'; Sha256 = $processGuardSha; SignaturePolicy = 'none' },
            [pscustomobject]@{ RelativePath = 'windows-e2e-orchestrator-helpers.ps1'; Sha256 = $orchestratorHelperSha; SignaturePolicy = 'none' },
            [pscustomobject]@{ RelativePath = 'pinvou-provider-verifier.exe'; Sha256 = $providerVerifierSha; SignaturePolicy = 'app' }
            [pscustomobject]@{ RelativePath = 'pinvou-node.exe'; Sha256 = $TrustedNodeSha256; SignaturePolicy = 'node' }
        )
        if ($index -eq 0) {
            $expectedStagedFiles += [pscustomobject]@{ RelativePath = 'legacy-relay.exe'; Sha256 = $legacyRelaySha; SignaturePolicy = 'none' }
            $expectedStagedFiles += [pscustomobject]@{ RelativePath = 'legacy-route-spec.json'; Sha256 = $legacyRouteSpecSha; SignaturePolicy = 'none' }
        }
        $expectedStagedJson = @($expectedStagedFiles) | ConvertTo-Json -Depth 3 -Compress
        $bootstrapResult = Invoke-Command -Session $session -ScriptBlock $RemoteBootstrap -ArgumentList $RemoteRoot, $expectedStagedJson, $TrustedSignerThumbprint, $TrustedSignerPublisher, $TrustedNodeSignerThumbprint, $TrustedNodeSignerPublisher
        if ($bootstrapResult.helper_loaded -ne $true -or $bootstrapResult.verified_file_count -ne $expectedStagedFiles.Count) { throw 'e2e_remote_bootstrap_invalid' }
        $role = @('A', 'B')[$index]
        $runtimeJson = Invoke-Command -Session $session -ScriptBlock {
            param($ExpectedInstallerSha, $ExpectedDriverSha, $ExpectedVerifierSha, $SignerThumbprint, $SignerPublisher, $Role, $Relay, $DisabledRelay, $Commit)
            foreach ($item in @(
                @((Join-Path $global:PinvouE2ERunRoot 'pinvou-installer.exe'), $ExpectedInstallerSha),
                @((Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe'), $ExpectedDriverSha),
                @((Join-Path $global:PinvouE2ERunRoot 'pinvou-provider-verifier.exe'), $ExpectedVerifierSha)
            )) {
                if ((Get-FileHash -LiteralPath $item[0] -Algorithm SHA256).Hash.ToLowerInvariant() -ne $item[1]) { throw 'e2e_remote_hash_mismatch' }
                $signature = Get-AuthenticodeSignature -LiteralPath $item[0]
                if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Thumbprint.ToUpperInvariant() -ne $SignerThumbprint -or $signature.SignerCertificate.Subject -ne $SignerPublisher) { throw 'e2e_remote_signature_mismatch' }
            }
            $driverPath = Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe'
            $install = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-installer.exe') -Arguments @('/S') -ReturnProcessResult
            if ($install.ExitCode -ne 0) { throw 'e2e_install_failed' }
            $disabledRuntime = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('start-role', '--role', $Role, '--relay-url', $DisabledRelay, '--feature-flag', '0', '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0 -or $disabledRuntime.app_started -ne $true -or $disabledRuntime.mock -ne $false) { throw ('e2e_flag_off_role_start_failed:' + $Role) }
            $flagOff = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('verify-feature-off', '--role', $Role, '--relay-url', $DisabledRelay, '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0) { throw ('e2e_flag_off_probe_failed:' + $Role) }
            Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('stop-role', '--role', $Role) | Out-Null
            if ($LASTEXITCODE -ne 0) { throw ('e2e_flag_off_role_stop_failed:' + $Role) }
            $output = Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('start-role', '--role', $Role, '--relay-url', $Relay, '--feature-flag', '1', '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)
            if ($LASTEXITCODE -ne 0) { throw ('e2e_role_start_failed:' + $Role) }
            [ordered]@{ runtime = ($output | ConvertFrom-Json); flag_off = $flagOff } | ConvertTo-Json -Depth 8 -Compress
        } -ArgumentList $installerSha, $driverSha, $providerVerifierSha, $TrustedSignerThumbprint, $TrustedSignerPublisher, $role, $RelayUrl, $DisabledRelayUrl, $actualHead
        $roleResult = $runtimeJson | ConvertFrom-Json
        $runtime = $roleResult.runtime
        if ($runtime.schema -ne 'pinvou-remote-control-role-runtime' -or $runtime.role -ne $role -or $runtime.app_started -ne $true -or $runtime.driver_sha256 -ne $driverSha -or $runtime.source_commit_sha -ne $actualHead -or $runtime.mock -ne $false) { throw ('e2e_role_runtime_invalid:' + $role) }
        if ($runtime.cdp_url -notmatch '^http://(?:127\.0\.0\.1|localhost):\d+$') { throw ('e2e_role_cdp_invalid:' + $role) }
        $machineFacts[$index] | Add-Member -NotePropertyName CdpUrl -NotePropertyValue $runtime.cdp_url
        $flagOff = $roleResult.flag_off
        if ($flagOff.schema -ne 'pinvou-remote-control-feature-off-observation' -or $flagOff.role -ne $role -or $flagOff.source_commit_sha -ne $actualHead -or $flagOff.mock -ne $false `
            -or $flagOff.successful_socket_count -ne 0 -or $flagOff.relay_allocation_delta -ne 0 -or $flagOff.registry_fingerprint_before -ne $flagOff.registry_fingerprint_after `
            -or $flagOff.tombstone_fingerprint_before -ne $flagOff.tombstone_fingerprint_after -or $flagOff.relay_rejected -ne $true `
            -or $flagOff.ui_entry_hidden -ne $true -or $flagOff.registered_command_count -lt 1 -or $flagOff.all_commands_fail_closed -ne $true `
            -or $flagOff.error_code -ne 'desktop_remote_control_unsupported') { throw ('e2e_flag_off_observation_invalid:' + $role) }
        $relative = "flag-off/$role.json"
        $sha = Copy-VerifiedRemoteArtifact $session $flagOff.artifact_path $evidenceRoot $relative 'check' $artifacts $role
        $flagOffEvidence.Add([ordered]@{ role = $role; sha256 = $sha; successful_socket_count = 0; relay_allocation_delta = 0; state_unchanged = $true; relay_rejected = $true; ui_entry_hidden = $true; command_surface_checked = $true })
    }

    foreach ($index in 0..1) {
        $role = @('A', 'B')[$index]; $rolePolicy = @($ARolePolicySha256, $BRolePolicySha256)[$index]; $roleKeyId = "$AttestationKeyId/$role"
        $remoteReceipt = "$RemoteOutput\provider-$role.receipt.json"
        $identityJson = Invoke-Command -Session $sessions[$index] -ScriptBlock {
            param($Provider, $KeyId, $Role, $RolePolicy, $RunNonce, $DriverSha, $Receipt)
            $driverPath = Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe'
            $provenanceOutput = Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('provenance', '--json', '--attestation-provider', $Provider, '--key-id', $KeyId, '--role', $Role, '--role-policy-sha256', $RolePolicy)
            if ($LASTEXITCODE -ne 0) { throw ('e2e_role_provenance_failed:' + $Role) }
            $providerOutput = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-provider-verifier.exe') -Arguments @('attest-provider', '--role', $Role, '--role-policy-sha256', $RolePolicy, '--provider', $Provider, '--key-id', $KeyId, '--run-nonce', $RunNonce, '--driver-sha256', $DriverSha, '--output', $Receipt)
            if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $Receipt -PathType Leaf)) { throw ('e2e_role_provider_attestation_failed:' + $Role) }
            $provenanceOutput
        } -ArgumentList $AttestationProvider, $roleKeyId, $role, $rolePolicy, $runNonce, $driverSha, $remoteReceipt
        $roleProvenance = $identityJson | ConvertFrom-Json
        if ($roleProvenance.attestation_key_id -ne $roleKeyId -or $roleProvenance.attestation_provider -ne $AttestationProvider -or $roleProvenance.attestation_public_key_sha256 -notmatch '^[a-f0-9]{64}$' -or $roleProvenance.non_exportable -ne $true -or $roleProvenance.private_key_embedded -ne $false) { throw ('e2e_role_provenance_invalid:' + $role) }
        $receiptLocal = Join-Path $evidenceRoot "build\attestation-provider-$role.receipt.json"
        Copy-Item -LiteralPath $remoteReceipt -Destination $receiptLocal -FromSession $sessions[$index]
        $verifiedRelative = "build/attestation-provider-$role.json"; $verifiedLocal = Join-Path $evidenceRoot $verifiedRelative
        $verifyRole = Invoke-PinvouVerifiedLocalProcess -FilePath $trustedNode -ExpectedSha256 $TrustedNodeSha256 -ExpectedThumbprint $TrustedNodeSignerThumbprint -ExpectedPublisher $TrustedNodeSignerPublisher -Arguments @($ProviderAttestationVerifier, '--input', $receiptLocal, '--output', $verifiedLocal, '--role', $role, '--role-policy-sha256', $rolePolicy, '--provider', $AttestationProvider, '--key-id', $roleKeyId, '--run-nonce', $runNonce, '--acl-sha256', $AttestationAclSha256, '--measurement-sha256', $AttestationProviderMeasurementSha256, '--driver-sha256', $driverSha, '--vendor', $AttestationProviderVendor, '--root-certificate-sha256', $TrustedProviderRootCertificateSha256, '--verifier-certificate-sha256', $TrustedProviderVerifierCertificateSha256) -AdditionalExpectedSha256 $providerSourceHashes
        if ($verifyRole.ExitCode -ne 0) { throw ('e2e_role_provider_attestation_invalid:' + $role) }
        $verifiedRoleIdentity = $verifyRole.StdOut | ConvertFrom-Json
        if ($verifiedRoleIdentity.ok -ne $true -or $verifiedRoleIdentity.role -ne $role -or $verifiedRoleIdentity.provider -ne $AttestationProvider -or $verifiedRoleIdentity.key_id -ne $roleKeyId `
            -or $verifiedRoleIdentity.public_key_sha256 -notmatch '^[a-f0-9]{64}$' -or $verifiedRoleIdentity.public_key_spki_pem -notmatch '^-----BEGIN PUBLIC KEY-----') { throw ('e2e_role_provider_identity_invalid:' + $role) }
        Remove-Item -LiteralPath $receiptLocal -Force
        $providerSha = Add-LocalArtifact $artifacts $evidenceRoot $verifiedRelative 'check' $role
        $attestationIdentities.Add([ordered]@{ role = $role; role_policy_sha256 = $rolePolicy; identity = 'production'; algorithm = 'ed25519'; public_key_spki_pem = $verifiedRoleIdentity.public_key_spki_pem; public_key_sha256 = $verifiedRoleIdentity.public_key_sha256; run_nonce = $runNonce; provider = $AttestationProvider; key_id = $roleKeyId; non_exportable = $true; acl_attestation_sha256 = $AttestationAclSha256; provider_measurement_sha256 = $AttestationProviderMeasurementSha256; provider_vendor = $AttestationProviderVendor; provider_root_certificate_sha256 = $TrustedProviderRootCertificateSha256; provider_verifier_certificate_sha256 = $TrustedProviderVerifierCertificateSha256; provider_attestation_artifact_sha256 = $providerSha })
        $machineFacts[$index] | Add-Member -NotePropertyName Attestation -NotePropertyValue $verifiedRoleIdentity
    }

    $legacyRuntimeJson = Invoke-Command -Session $sessions[0] -ScriptBlock {
        param($ExpectedSha, $ExpectedVersion, $Gateway, $Commit, $RunNonce)
        $legacyPath = Join-Path $global:PinvouE2ERunRoot 'legacy-relay.exe'; $driverPath = Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe'
        if ((Get-FileHash -LiteralPath $legacyPath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $ExpectedSha) { throw 'e2e_legacy_remote_hash_mismatch' }
        $runtime = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-start', '--binary', $legacyPath, '--expected-version', $ExpectedVersion, '--expected-sha256', $ExpectedSha, '--bind-host', '127.0.0.1', '--gateway-url', $Gateway, '--run-nonce', $RunNonce, '--persist-ownership-before-response', '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput) -AdditionalFilePaths @($legacyPath)) | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0 -or $runtime.pid -le 0 -or $runtime.mock -ne $false) { throw 'e2e_legacy_start_failed' }
        $registry = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-owned-pids', '--run-nonce', $RunNonce, '--json', '--durable-registry-only')) | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0) { throw 'e2e_legacy_ownership_query_failed' }
        $ownership = Assert-PinvouLegacyOwnershipRegistry -RunNonce $RunNonce -Registry $registry -CallerObservedPid ([Nullable[int]][int]$runtime.pid)
        if ($ownership.active.Count -ne 1 -or [int]$ownership.active[0] -ne [int]$runtime.pid) { throw 'e2e_legacy_ownership_registry_mismatch' }
        $process = CimCmdlets\Get-CimInstance Win32_Process -Filter ("ProcessId=" + [int]$runtime.pid) -ErrorAction Stop
        if (-not [string]::Equals([IO.Path]::GetFullPath([string]$process.ExecutablePath), [IO.Path]::GetFullPath($legacyPath), [StringComparison]::OrdinalIgnoreCase) `
            -or (Get-FileHash -LiteralPath $process.ExecutablePath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $ExpectedSha) { throw 'e2e_legacy_process_identity_invalid' }
        $listeners = @(NetTCPIP\Get-NetTCPConnection -OwningProcess $runtime.pid -State Listen -ErrorAction Stop)
        if ($listeners.Count -ne 1 -or $listeners[0].LocalAddress -notin @('127.0.0.1', '::1') -or $listeners[0].LocalPort -ne $runtime.listener_port) { throw 'e2e_legacy_listener_not_bound' }
        [ordered]@{ runtime = $runtime; registry_active_count = $ownership.active.Count; observed_executable_sha256 = $ExpectedSha; observed_listener_port = $listeners[0].LocalPort; observed_listener_owner_pid = [int]$listeners[0].OwningProcess } | ConvertTo-Json -Depth 8 -Compress
    } -ArgumentList $legacyRelaySha, $legacyRelayVersion, $LegacyGatewayUrl, $actualHead, $runNonce
    $legacyResult = $legacyRuntimeJson | ConvertFrom-Json
    $legacyRuntime = $legacyResult.runtime
    # Assign immediately after the process-producing call so the outer finally
    # owns cleanup before any configuration, probe or evidence operation.
    $legacyRuntimePid = [int]$legacyRuntime.pid
    if ($legacyRuntime.schema -ne 'pinvou-legacy-relay-runtime' -or $legacyRuntime.pid -le 0 -or $legacyRuntime.version -ne $legacyRelayVersion -or $legacyRuntime.sha256 -ne $legacyRelaySha `
        -or $legacyRuntime.listener_host -notin @('127.0.0.1', '::1') -or $legacyResult.observed_listener_port -ne $legacyRuntime.listener_port `
        -or $legacyResult.observed_executable_sha256 -ne $legacyRelaySha -or $legacyResult.observed_listener_owner_pid -ne $legacyRuntime.pid `
        -or [long]$legacyResult.registry_active_count -ne 1) { throw 'e2e_legacy_runtime_invalid' }
    $requestNonceBytes = [byte[]]::new(32)
    [Security.Cryptography.RandomNumberGenerator]::Fill($requestNonceBytes)
    $legacyRequestNonce = ([BitConverter]::ToString($requestNonceBytes)).Replace('-', '').ToLowerInvariant()
    $legacyFlowJson = Invoke-Command -Session $sessions[0] -ScriptBlock {
        param($Gateway, $GatewaySha, $RouteSpecSha, $RouteId, $RunNonce, $Pid, $ListenerPort, $Nonce, $Commit)
        $driverPath = Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe'; $routeSpecPath = Join-Path $global:PinvouE2ERunRoot 'legacy-route-spec.json'
        $listenerUrl = 'ws://127.0.0.1:' + $ListenerPort
        $configuration = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-configure-gateway', '--gateway-url', $Gateway, '--listener-url', $listenerUrl, '--expected-pid', [string]$Pid, '--route-spec', $routeSpecPath, '--route-spec-sha256', $RouteSpecSha, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--require-no-bypass', '--require-full-control-plane-snapshot', '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput) -AdditionalFilePaths @($routeSpecPath)) | ConvertFrom-Json
        $configureExitCode = $LASTEXITCODE
        try {
            if ($configureExitCode -ne 0 -or $configuration.route_activated -ne $true -or $configuration.mock -ne $false) { throw 'e2e_legacy_gateway_activation_failed' }
            $before = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-traffic-counter', '--gateway-url', $Gateway, '--expected-pid', [string]$Pid, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--source-commit', $Commit)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0 -or $before.schema -ne 'pinvou-legacy-relay-traffic-counter' -or $before.pid -ne $Pid -or [long]$before.count -lt 0 -or $before.mock -ne $false) { throw 'e2e_legacy_counter_before_invalid' }
            $probe = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-probe', '--gateway-url', $Gateway, '--listener-url', $listenerUrl, '--expected-pid', [string]$Pid, '--request-nonce', $Nonce, '--route-spec', $routeSpecPath, '--route-spec-sha256', $RouteSpecSha, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--expected-route-state-digest', $configuration.route_state_digest, '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput) -AdditionalFilePaths @($routeSpecPath)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0) { throw 'e2e_legacy_probe_failed' }
            $after = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-traffic-counter', '--gateway-url', $Gateway, '--expected-pid', [string]$Pid, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--source-commit', $Commit)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0 -or $after.schema -ne 'pinvou-legacy-relay-traffic-counter' -or $after.pid -ne $Pid -or [long]$after.count -le [long]$before.count -or $after.mock -ne $false) { throw 'e2e_legacy_counter_after_invalid' }
            $bareV2 = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-bare-v2-probe', '--gateway-url', $Gateway, '--request-nonce', $Nonce, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--role', 'A', '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0 -or $bareV2.schema -ne 'pinvou-legacy-bare-v2-rejection' -or $bareV2.gateway_url_sha256 -ne $GatewaySha `
                -or $bareV2.request_nonce -ne $Nonce -or $bareV2.response_kind -ne 'upgrade_rejected' -or $bareV2.rejected -ne $true -or $bareV2.mock -ne $false) { throw 'e2e_legacy_bare_v2_probe_invalid' }
        } finally {
            $restoration = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-deconfigure-gateway', '--gateway-url', $Gateway, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--restore-original-state', $configuration.original_state_digest, '--canonical-input-state-digest', $configuration.canonical_input_state_digest, '--expected-control-plane-state-digest', $configuration.control_plane_state_before_digest, '--require-full-control-plane-reread', '--require-public-route-withdrawn', '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)) | ConvertFrom-Json
            if ($LASTEXITCODE -ne 0 -or $restoration.owned_route_removed -ne $true -or $restoration.public_route_withdrawn -ne $true `
                -or $restoration.restored_state_digest -ne $configuration.original_state_digest -or $restoration.mock -ne $false) { throw 'e2e_legacy_gateway_restore_failed' }
        }
        [ordered]@{ configuration = $configuration; probe = $probe; bare_v2 = $bareV2; restoration = $restoration; counter_before = [long]$before.count; counter_after = [long]$after.count } | ConvertTo-Json -Depth 10 -Compress
    } -ArgumentList $LegacyGatewayUrl, $gatewayUrlSha, $legacyRouteSpecSha, $routeSpec.route_id, $runNonce, $legacyRuntimePid, $legacyRuntime.listener_port, $legacyRequestNonce, $actualHead
    $legacyFlow = $legacyFlowJson | ConvertFrom-Json
    $legacyConfiguration = $legacyFlow.configuration
    $legacyProbe = $legacyFlow.probe
    $legacyBareV2 = $legacyFlow.bare_v2
    $legacyRestoration = $legacyFlow.restoration
    Assert-PinvouRouteStateBinding $routeSpec.canonical_input_state_digest $legacyConfiguration.control_plane_state_before_digest $legacyConfiguration.original_state_digest $legacyRestoration.control_plane_state_after_digest $legacyRestoration.original_state_digest $legacyRestoration.restored_state_digest
    if ($legacyConfiguration.schema -ne 'pinvou-legacy-relay-configuration' -or $legacyConfiguration.pid -ne $legacyRuntimePid `
        -or $legacyConfiguration.gateway_url_sha256 -ne $gatewayUrlSha -or $legacyConfiguration.endpoint_inventory_sha256 -ne $inventorySha `
        -or $legacyConfiguration.gateway_config_sha256 -ne $gatewayConfigSha -or $legacyConfiguration.listener_port -ne $legacyRuntime.listener_port -or $legacyConfiguration.run_nonce -ne $runNonce -or $legacyConfiguration.route_id -ne $routeSpec.route_id -or $legacyConfiguration.no_bypass -ne $true `
        -or $legacyConfiguration.route_activated -ne $true -or $legacyConfiguration.source_commit_sha -ne $actualHead -or $legacyConfiguration.mock -ne $false `
        -or $legacyProbe.schema -ne 'pinvou-legacy-relay-probe' -or $legacyProbe.pid -ne $legacyRuntimePid `
        -or $legacyProbe.gateway_url_sha256 -ne $gatewayUrlSha -or $legacyProbe.endpoint_inventory_sha256 -ne $inventorySha `
        -or $legacyProbe.gateway_config_sha256 -ne $gatewayConfigSha -or $legacyProbe.listener_port -ne $legacyRuntime.listener_port -or $legacyProbe.run_nonce -ne $runNonce -or $legacyProbe.route_id -ne $routeSpec.route_id -or $legacyProbe.no_bypass -ne $true -or $legacyProbe.route_state_digest -ne $legacyConfiguration.route_state_digest `
        -or $legacyProbe.request_nonce -ne $legacyRequestNonce -or $legacyProbe.response_nonce -ne $legacyRequestNonce `
        -or [long]$legacyFlow.counter_before -ne [long]$legacyProbe.traffic_counter_before `
        -or [long]$legacyFlow.counter_after -ne [long]$legacyProbe.traffic_counter_after `
        -or [long]$legacyFlow.counter_after -le [long]$legacyFlow.counter_before `
        -or $legacyProbe.gateway_traffic_observed -ne $true -or $legacyProbe.listener_flow_observed -ne $true `
        -or $legacyProbe.response_correlated -ne $true `
        -or $legacyBareV2.gateway_url_sha256 -ne $gatewayUrlSha -or $legacyBareV2.request_nonce -ne $legacyRequestNonce `
        -or $legacyBareV2.response_kind -ne 'upgrade_rejected' -or $legacyBareV2.rejected -ne $true -or $legacyBareV2.mock -ne $false `
        -or $legacyBareV2.run_nonce -ne $runNonce -or $legacyBareV2.route_id -ne $routeSpec.route_id -or $legacyBareV2.role -ne 'A' `
        -or $legacyRestoration.run_nonce -ne $runNonce -or $legacyRestoration.route_id -ne $routeSpec.route_id -or $legacyRestoration.owned_route_removed -ne $true -or $legacyRestoration.public_route_withdrawn -ne $true `
        -or $routeSpec.canonical_input_state_digest -ne $legacyConfiguration.canonical_input_state_digest -or $legacyRestoration.canonical_input_state_digest -ne $routeSpec.canonical_input_state_digest `
        -or $legacyConfiguration.control_plane_state_before_digest -notmatch '^[a-f0-9]{64}$' -or $legacyRestoration.control_plane_state_before_digest -ne $legacyConfiguration.control_plane_state_before_digest -or $legacyRestoration.control_plane_state_after_digest -ne $legacyConfiguration.control_plane_state_before_digest `
        -or $legacyRestoration.original_state_digest -ne $legacyConfiguration.original_state_digest -or $legacyRestoration.restored_state_digest -ne $legacyConfiguration.original_state_digest `
        -or $legacyProbe.source_commit_sha -ne $actualHead -or $legacyProbe.mock -ne $false) { throw 'e2e_legacy_probe_invalid' }
    $legacyRuntimeSha = Copy-VerifiedRemoteArtifact $sessions[0] $legacyRuntime.artifact_path $evidenceRoot 'rollback/legacy-runtime.json' 'rollback' $artifacts 'A'
    $legacyConfigurationSha = Copy-VerifiedRemoteArtifact $sessions[0] $legacyConfiguration.artifact_path $evidenceRoot 'rollback/legacy-configuration.json' 'rollback' $artifacts 'A'
    $legacyProbeSha = Copy-VerifiedRemoteArtifact $sessions[0] $legacyProbe.artifact_path $evidenceRoot 'rollback/legacy-probe.json' 'rollback' $artifacts 'A'
    $legacyBareV2Sha = Copy-VerifiedRemoteArtifact $sessions[0] $legacyBareV2.artifact_path $evidenceRoot 'rollback/legacy-bare-v2.json' 'rollback' $artifacts 'A'
    $legacyRestorationSha = Copy-VerifiedRemoteArtifact $sessions[0] $legacyRestoration.artifact_path $evidenceRoot 'rollback/legacy-restoration.json' 'rollback' $artifacts 'A'

    $initialUi = @(@{ Role = 'B'; Phase = 'controlled-panel'; Index = 1 }, @{ Role = 'A'; Phase = 'settings'; Index = 0 })
    foreach ($item in $initialUi) {
        $remoteUi = "$RemoteOutput\ui-$($item.Role)-$($item.Phase).json"
        Invoke-Command -Session $sessions[$item.Index] -ScriptBlock {
            param($Cdp, $Phase, $Output)
            Invoke-PinvouTrustedNode -Arguments @((Join-Path $global:PinvouE2ERunRoot 'remote-control-ui-smoke.mjs'), '--cdp-url', $Cdp, '--phase', $Phase, '--output', $Output)
            if ($LASTEXITCODE -ne 0) { throw ('e2e_ui_smoke_failed:' + $Phase) }
        } -ArgumentList $machineFacts[$item.Index].CdpUrl, $item.Phase, $remoteUi
        $relative = "ui/$($item.Role)-$($item.Phase).json"
        $sha = Copy-VerifiedRemoteArtifact $sessions[$item.Index] $remoteUi $evidenceRoot $relative 'ui-smoke' $artifacts $item.Role
        $uiArtifacts.Add([ordered]@{ role = $item.Role; phase = $item.Phase; sha256 = $sha })
    }

    foreach ($journey in $catalog.journeys) {
        # Evidence uses the orchestrator's single monotonic clock. Driver-local
        # clocks are process-relative and cannot be compared across journeys.
        $journeyMonotonicStart = [long]$runClock.ElapsedMilliseconds
        $phase = switch ($journey.id) { 'RC-E2E-002' { 'connected-chat' }; 'RC-E2E-006' { 'approval-allow' }; 'RC-E2E-007' { 'approval-deny' }; 'RC-E2E-008' { 'attachment' }; 'RC-E2E-009' { 'artifact' }; default { $null } }
        $interactive = $journey.id -in @('RC-E2E-006', 'RC-E2E-007', 'RC-E2E-008')
        $watchdogJourney = $journey.id -eq 'RC-E2E-011'
        $prepared = $null
        if ($interactive -or $watchdogJourney) {
            $preparedJson = Invoke-Command -Session $sessions[0] -ScriptBlock {
                param($JourneyId, $Relay, $RemoteB, $Commit)
                $output = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments @('journey-prepare', '--journey', $JourneyId, '--relay-url', $Relay, '--peer', $RemoteB, '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)
                if ($LASTEXITCODE -ne 0) { throw ('e2e_journey_prepare_failed:' + $JourneyId) }
                $output
            } -ArgumentList $journey.id, $RelayUrl, $BComputerName, $actualHead
            $prepared = $preparedJson | ConvertFrom-Json
            $allowedPendingStates = if ($watchdogJourney) { @('watchdog_pending') } else { @('permission_pending', 'attachment_pending') }
            if ($prepared.schema -ne 'pinvou-remote-control-journey-pending' -or $prepared.journey_id -ne $journey.id `
                -or $prepared.pending_state -notin $allowedPendingStates -or $prepared.coordination_id -notmatch '^[a-f0-9]{64}$' `
                -or $prepared.pending_count -ne 1 -or $prepared.coordination_source -notin @('permission_request', 'prepared_task', 'watchdog') `
                -or $prepared.source_commit_sha -ne $actualHead -or $prepared.mock -ne $false) { throw ('e2e_journey_prepare_invalid:' + $journey.id) }
        }
        if ($phase -and $interactive) {
            $remoteUi = "$RemoteOutput\ui-A-$phase.json"
            Invoke-Command -Session $sessions[0] -ScriptBlock {
                param($Cdp, $Phase, $CoordinationId, $Output)
                Invoke-PinvouTrustedNode -Arguments @((Join-Path $global:PinvouE2ERunRoot 'remote-control-ui-smoke.mjs'), '--cdp-url', $Cdp, '--phase', $Phase, '--coordination-id', $CoordinationId, '--output', $Output)
                if ($LASTEXITCODE -ne 0) { throw ('e2e_ui_smoke_failed:' + $Phase) }
            } -ArgumentList $machineFacts[0].CdpUrl, $phase, $prepared.coordination_id, $remoteUi
            $relative = "ui/A-$phase.json"
            $sha = Copy-VerifiedRemoteArtifact $sessions[0] $remoteUi $evidenceRoot $relative 'ui-smoke' $artifacts 'A'
            $uiEvidence = Get-Content -LiteralPath (Join-Path $evidenceRoot $relative) -Raw -Encoding UTF8 | ConvertFrom-Json
            $coordinationDigest = Get-PinvouSha256Hex ([Text.Encoding]::UTF8.GetBytes($prepared.coordination_id))
            if ($uiEvidence.coordination_digest -ne $coordinationDigest) { throw ('e2e_ui_coordination_digest_mismatch:' + $journey.id) }
            $uiArtifacts.Add([ordered]@{ role = 'A'; phase = $phase; sha256 = $sha })
        }
        if ($interactive -or $watchdogJourney) {
            $resultJson = Invoke-Command -Session $sessions[0] -ScriptBlock {
                param($JourneyId, $CoordinationId, $Commit, $RequireUi, $WaitTerminal)
                $arguments = @('journey-finalize', '--journey', $JourneyId, '--coordination-id', $CoordinationId, '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput)
                if ($RequireUi) { $arguments += '--require-ui-action' }
                if ($WaitTerminal) { $arguments += @('--wait-for-terminal', '--reject-active-return') }
                $output = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments $arguments
                if ($LASTEXITCODE -ne 0) { throw ('e2e_journey_finalize_failed:' + $JourneyId) }
                $output
            } -ArgumentList $journey.id, $prepared.coordination_id, $actualHead, $interactive, $watchdogJourney
        } else {
            $resultJson = Invoke-Command -Session $sessions[0] -ScriptBlock {
                param($JourneyId, $Relay, $RemoteB)
                $output = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments @('run', '--journey', $JourneyId, '--relay-url', $Relay, '--peer', $RemoteB, '--output-root', $global:PinvouE2EOutput)
                if ($LASTEXITCODE -ne 0) { throw ('e2e_journey_failed:' + $JourneyId) }
                $output
            } -ArgumentList $journey.id, $RelayUrl, $BComputerName
        }
        $journeyMonotonicEnd = [long]$runClock.ElapsedMilliseconds
        $parsed = $resultJson | ConvertFrom-Json
        if ($parsed.schema -ne 'pinvou-remote-control-journey-result' -or $parsed.journey_id -ne $journey.id -or $parsed.result -ne 'pass' -or $parsed.source_commit_sha -ne $actualHead -or $parsed.mock -ne $false) { throw ('e2e_journey_result_invalid:' + $journey.id) }
        if ($interactive -and ($parsed.coordination_id -ne $prepared.coordination_id -or $parsed.ui_action_applied -ne $true)) { throw ('e2e_journey_ui_not_correlated:' + $journey.id) }
        if ($journey.id -eq 'RC-E2E-008' -and $parsed.attachment_received_by_controlled_side -ne $true) { throw 'e2e_attachment_not_received' }
        $controlledAttachment = $null
        if ($journey.id -eq 'RC-E2E-008') {
            $controlledJson = Invoke-Command -Session $sessions[1] -ScriptBlock {
                param($CoordinationId, $CoordinationDigest, $AttachmentSha, $Commit, $RunNonce, $Provider, $KeyId)
                $output = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments @('verify-attachment-received', '--coordination-id', $CoordinationId, '--coordination-digest', $CoordinationDigest, '--expected-attachment-sha256', $AttachmentSha, '--role', 'B', '--run-nonce', $RunNonce, '--source-commit', $Commit, '--evidence-relative-path', 'checks/RC-E2E-008-controlled_attachment_received.json', '--evidence-kind', 'check', '--attestation-provider', $Provider, '--key-id', $KeyId, '--output-root', $global:PinvouE2EOutput)
                if ($LASTEXITCODE -ne 0) { throw 'e2e_controlled_attachment_receipt_failed' }
                $output
            } -ArgumentList $prepared.coordination_id, $coordinationDigest, $uiEvidence.attachment_sha256, $actualHead, $runNonce, $AttestationProvider, "$AttestationKeyId/B"
            $controlledAttachment = $controlledJson | ConvertFrom-Json
            if ($controlledAttachment.schema -ne 'pinvou-controlled-attachment-receipt-result' -or $controlledAttachment.role -ne 'B' `
                -or $controlledAttachment.coordination_digest -ne $coordinationDigest -or $controlledAttachment.attachment_sha256 -ne $uiEvidence.attachment_sha256 `
                -or $controlledAttachment.received -ne $true -or $controlledAttachment.artifact_signature -notmatch '^[A-Za-z0-9+/]{80,128}={0,2}$' `
                -or $controlledAttachment.public_key_sha256 -ne $machineFacts[1].Attestation.public_key_sha256 -or $controlledAttachment.mock -ne $false) { throw 'e2e_controlled_attachment_receipt_invalid' }
        }
        if ($journey.id -eq 'RC-E2E-011') {
            $observation = $parsed.deadline_observation
            # The watchdog budget is at most 30s minus measured RTT and the
            # mandatory 1s safety reserve. It is not the journey wall time.
            $observationBytes = [Text.Encoding]::UTF8.GetBytes(($observation | ConvertTo-Json -Compress))
            $deadlineValidation = Invoke-PinvouVerifiedLocalProcess -FilePath $trustedNode -ExpectedSha256 $TrustedNodeSha256 -ExpectedThumbprint $TrustedNodeSignerThumbprint -ExpectedPublisher $TrustedNodeSignerPublisher -Arguments @($EvidenceValidator, '--deadline-observation-base64', [Convert]::ToBase64String($observationBytes)) -AdditionalExpectedSha256 $validatorSourceHashes
            if ($deadlineValidation.ExitCode -ne 0) { throw 'e2e_monotonic_deadline_invalid' }
            $actualElapsed = $journeyMonotonicEnd - $journeyMonotonicStart
            Assert-PinvouJourneyElapsed $actualElapsed ([long]$observation.terminal_elapsed_ms)
        }
        $traceRelative = "traces/$($journey.id).json"
        $traceSha = Copy-VerifiedRemoteArtifact $sessions[0] $parsed.trace_path $evidenceRoot $traceRelative 'trace' $artifacts 'A'
        $checks = [Collections.Generic.List[object]]::new()
        foreach ($requiredCheck in $journey.required_checks) {
            if ($requiredCheck -eq 'controlled_attachment_received') {
                if (-not $controlledAttachment) { throw 'e2e_controlled_attachment_receipt_missing' }
                $relative = "checks/$($journey.id)-$requiredCheck.json"
                $checkSha = Copy-VerifiedRemoteArtifact $sessions[1] $controlledAttachment.artifact_path $evidenceRoot $relative 'check' $artifacts 'B'
                $artifactEntry = @($artifacts | Where-Object { $_.sha256 -eq $checkSha -and $_.machine_role -eq 'B' })
                if ($artifactEntry.Count -ne 1) { throw 'e2e_controlled_attachment_artifact_invalid' }
                $artifactEntry[0].signature = $controlledAttachment.artifact_signature
                $checks.Add([ordered]@{ id = $requiredCheck; result = 'pass'; artifact_sha256 = $checkSha })
                continue
            }
            $matching = @($parsed.checks | Where-Object { $_.id -eq $requiredCheck -and $_.result -eq 'pass' })
            if ($matching.Count -ne 1) { throw ('e2e_required_check_missing:' + $journey.id + ':' + $requiredCheck) }
            $relative = "checks/$($journey.id)-$requiredCheck.json"
            $checkSha = Copy-VerifiedRemoteArtifact $sessions[0] $matching[0].artifact_path $evidenceRoot $relative 'check' $artifacts 'A'
            $checks.Add([ordered]@{ id = $requiredCheck; result = 'pass'; artifact_sha256 = $checkSha })
        }
        $driverExpectedCheckCount = @($journey.required_checks | Where-Object { $_ -ne 'controlled_attachment_received' }).Count
        if (@($parsed.checks).Count -ne $driverExpectedCheckCount) { throw ('e2e_check_set_invalid:' + $journey.id) }
        $journeyEvidence.Add([ordered]@{
            id = $journey.id; result = 'pass'
            started_at = [DateTime]::Parse($parsed.started_at).ToUniversalTime().ToString($TimestampFormat)
            finished_at = [DateTime]::Parse($parsed.finished_at).ToUniversalTime().ToString($TimestampFormat)
            monotonic_start_ms = $journeyMonotonicStart; monotonic_end_ms = $journeyMonotonicEnd
            trace_sha256 = $traceSha
            coordination_digest = $(if ($interactive) { $coordinationDigest } else { $null })
            deadline_observation = $(if ($journey.id -eq 'RC-E2E-011') { $parsed.deadline_observation } else { $null })
            checks = @($checks)
        })
        if ($phase -in @('connected-chat', 'artifact')) {
            $remoteUi = "$RemoteOutput\ui-A-$phase.json"
            if ($phase -eq 'artifact') {
                $fault = Invoke-Command -Session $sessions[0] -ScriptBlock {
                    param($Commit)
                    Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments @('inject-fault', '--fault', 'artifact-download-once', '--source-commit', $Commit)
                    if ($LASTEXITCODE -ne 0) { throw 'e2e_artifact_fault_injection_failed' }
                } -ArgumentList $actualHead | ConvertFrom-Json
                if ($fault.schema -ne 'pinvou-e2e-fault-injection' -or $fault.fault -ne 'artifact-download-once' -or $fault.armed -ne $true -or $fault.source_commit_sha -ne $actualHead -or $fault.mock -ne $false) { throw 'e2e_artifact_fault_injection_invalid' }
            }
            Invoke-Command -Session $sessions[0] -ScriptBlock {
                param($Cdp, $Phase, $Output)
                Invoke-PinvouTrustedNode -Arguments @((Join-Path $global:PinvouE2ERunRoot 'remote-control-ui-smoke.mjs'), '--cdp-url', $Cdp, '--phase', $Phase, '--output', $Output)
                if ($LASTEXITCODE -ne 0) { throw ('e2e_ui_smoke_failed:' + $Phase) }
            } -ArgumentList $machineFacts[0].CdpUrl, $phase, $remoteUi
            $relative = "ui/A-$phase.json"
            $sha = Copy-VerifiedRemoteArtifact $sessions[0] $remoteUi $evidenceRoot $relative 'ui-smoke' $artifacts 'A'
            $uiArtifacts.Add([ordered]@{ role = 'A'; phase = $phase; sha256 = $sha })
        }
    }

    $gateResults = Invoke-Command -Session $sessions[0] -ScriptBlock {
        param($Relay, $LegacyGateway, $LegacyPid, $LegacySha, $LegacyListenerPort, $Commit, $RunNonce, $RouteId, $ProbeArtifact, $BareV2Artifact, $RestorationArtifact)
        $output = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments @('release-preflight', '--relay-url', $Relay, '--source-commit', $Commit, '--output-root', $global:PinvouE2EOutput, '--require-sensitive-scan', '--require-telemetry-thresholds', '--require-registry-coverage', '100', '--require-real-legacy-rollback', '--legacy-gateway-url', $LegacyGateway, '--legacy-pid', [string]$LegacyPid, '--legacy-sha256', $LegacySha, '--legacy-listener-port', [string]$LegacyListenerPort, '--run-nonce', $RunNonce, '--route-id', $RouteId, '--legacy-probe-artifact', $ProbeArtifact, '--legacy-bare-v2-artifact', $BareV2Artifact, '--legacy-restoration-artifact', $RestorationArtifact, '--require-route-restored')
        if ($LASTEXITCODE -ne 0) { throw 'e2e_release_preflight_failed' }
        $output
    } -ArgumentList $RelayUrl, $LegacyGatewayUrl, $legacyRuntime.pid, $legacyRelaySha, $legacyRuntime.listener_port, $actualHead, $runNonce, $routeSpec.route_id, $legacyProbe.artifact_path, $legacyBareV2.artifact_path, $legacyRestoration.artifact_path | ConvertFrom-Json
    if ($gateResults.schema -ne 'pinvou-remote-control-release-preflight' -or $gateResults.source_commit_sha -ne $actualHead -or $gateResults.mock -ne $false -or $gateResults.registry_coverage_percent -ne 100 -or $gateResults.production_equivalent -ne $true -or $gateResults.enrollment_deployment -ne $EnrollmentDeployment `
        -or $gateResults.legacy_pid -ne $legacyRuntime.pid -or $gateResults.legacy_sha256 -ne $legacyRelaySha -or $gateResults.legacy_listener_port -ne $legacyRuntime.listener_port -or $gateResults.bare_v2_rejected -ne $true `
        -or $gateResults.run_nonce -ne $runNonce -or $gateResults.route_id -ne $routeSpec.route_id -or $gateResults.route_restored -ne $true `
        -or $gateResults.probe_artifact_sha256 -ne $legacyProbeSha -or $gateResults.bare_v2_artifact_sha256 -ne $legacyBareV2Sha -or $gateResults.restoration_artifact_sha256 -ne $legacyRestorationSha) { throw 'e2e_release_preflight_invalid' }
    $gates = [ordered]@{}
    foreach ($gateName in @('sensitive_scan', 'telemetry_thresholds', 'registry_coverage', 'rollback_preflight')) {
        $gate = $gateResults.gates.$gateName
        if ($gate.result -ne 'pass') { throw ('e2e_release_gate_failed:' + $gateName) }
        $relative = "gates/$gateName.json"
        $sha = Copy-VerifiedRemoteArtifact $sessions[0] $gate.artifact_path $evidenceRoot $relative 'check' $artifacts 'A'
        $gates[$gateName] = [ordered]@{ result = 'pass'; artifact_sha256 = $sha }
    }
    $flagOffSummary = [ordered]@{ schema = 'pinvou-remote-control-feature-off-suite'; version = 1; result = 'pass'; source_commit_sha = $actualHead; relay_rejected = $true; roles = @($flagOffEvidence) }
    $flagOffSummaryPath = Join-Path $evidenceRoot 'gates\feature_flag_off.json'
    New-Item -ItemType Directory -Path (Split-Path -Parent $flagOffSummaryPath) -Force | Out-Null
    [IO.File]::WriteAllText($flagOffSummaryPath, (($flagOffSummary | ConvertTo-Json -Depth 6 -Compress) + "`n"), [Text.UTF8Encoding]::new($false))
    $flagOffSummarySha = Add-LocalArtifact $artifacts $evidenceRoot 'gates\feature_flag_off.json' 'check'
    $gates.feature_flag_off = [ordered]@{ result = 'pass'; artifact_sha256 = $flagOffSummarySha }
    $uiSummary = [ordered]@{ schema = 'pinvou-remote-control-ui-suite'; version = 1; result = 'pass'; artifacts = @($uiArtifacts) }
    $uiSummaryPath = Join-Path $evidenceRoot 'gates\ui_smoke.json'
    New-Item -ItemType Directory -Path (Split-Path -Parent $uiSummaryPath) -Force | Out-Null
    [IO.File]::WriteAllText($uiSummaryPath, (($uiSummary | ConvertTo-Json -Depth 5 -Compress) + "`n"), [Text.UTF8Encoding]::new($false))
    $uiSummarySha = Add-LocalArtifact $artifacts $evidenceRoot 'gates\ui_smoke.json' 'ui-smoke'
    $gates.ui_smoke = [ordered]@{ result = 'pass'; artifact_sha256 = $uiSummarySha }

    foreach ($artifact in $artifacts) {
        if ($artifact.signature) {
            if ($artifact.machine_role -ne 'B' -or $artifact.path -ne 'checks/RC-E2E-008-controlled_attachment_received.json') { throw 'e2e_pre_attested_artifact_forbidden' }
            continue
        }
        $artifactPath = Assert-PinvouPathInside $evidenceRoot (Join-Path $evidenceRoot $artifact.path) 'e2e_local_artifact_path_invalid'
        $identity = @($attestationIdentities | Where-Object { $_.role -eq $artifact.machine_role })
        if ($identity.Count -ne 1) { throw ('e2e_artifact_attestation_identity_missing:' + $artifact.machine_role) }
        if ($artifact.machine_role -eq 'orchestrator') {
            $attestationProcess = Invoke-PinvouVerifiedLocalProcess -FilePath $driver -ExpectedSha256 $driverSha -ExpectedThumbprint $TrustedSignerThumbprint -ExpectedPublisher $TrustedSignerPublisher -Arguments @('attest-file', '--json', '--provider', $AttestationProvider, '--key-id', $identity[0].key_id, '--role-policy-sha256', $identity[0].role_policy_sha256, '--path', $artifactPath, '--relative-path', $artifact.path, '--kind', $artifact.kind, '--machine-role', $artifact.machine_role, '--run-nonce', $runNonce, '--source-commit', $actualHead)
            $attestationResult = $attestationProcess.StdOut | ConvertFrom-Json
            $attestationExitCode = $attestationProcess.ExitCode
        } else {
            $roleIndex = if ($artifact.machine_role -eq 'A') { 0 } else { 1 }; $remoteAttestPath = "$RemoteOutput\attest-$([Guid]::NewGuid().ToString('N')).bin"
            Copy-Item -LiteralPath $artifactPath -Destination $remoteAttestPath -ToSession $sessions[$roleIndex]
            try {
                $attestationJson = Invoke-Command -Session $sessions[$roleIndex] -ScriptBlock {
                    param($Provider, $KeyId, $RolePolicy, $Path, $Relative, $Kind, $Role, $RunNonce, $Commit)
                    $output = Invoke-PinvouTrustedGuard -FilePath (Join-Path $global:PinvouE2ERunRoot 'pinvou-e2e-driver.exe') -Arguments @('attest-file', '--json', '--provider', $Provider, '--key-id', $KeyId, '--role-policy-sha256', $RolePolicy, '--path', $Path, '--relative-path', $Relative, '--kind', $Kind, '--machine-role', $Role, '--run-nonce', $RunNonce, '--source-commit', $Commit)
                    if ($LASTEXITCODE -ne 0) { throw 'e2e_remote_artifact_attestation_failed' }; $output
                } -ArgumentList $AttestationProvider, $identity[0].key_id, $identity[0].role_policy_sha256, $remoteAttestPath, $artifact.path, $artifact.kind, $artifact.machine_role, $runNonce, $actualHead
                $attestationResult = $attestationJson | ConvertFrom-Json; $attestationExitCode = 0
            } finally { Invoke-Command -Session $sessions[$roleIndex] -ScriptBlock { Remove-Item -LiteralPath $using:remoteAttestPath -Force -ErrorAction Stop } }
        }
        if ($attestationExitCode -ne 0 -or $attestationResult.schema -ne 'pinvou-e2e-artifact-attestation' `
            -or $attestationResult.sha256 -ne $artifact.sha256 `
            -or $attestationResult.relative_path -ne $artifact.path `
            -or $attestationResult.kind -ne $artifact.kind `
            -or $attestationResult.machine_role -ne $artifact.machine_role `
            -or $attestationResult.run_nonce -ne $runNonce `
            -or $attestationResult.source_commit_sha -ne $actualHead `
            -or $attestationResult.public_key_sha256 -ne $identity[0].public_key_sha256 `
            -or $attestationResult.provider -ne $AttestationProvider -or $attestationResult.key_id -ne $identity[0].key_id `
            -or $attestationResult.non_exportable -ne $true -or $attestationResult.private_key_embedded -ne $false `
            -or $attestationResult.signature -notmatch '^[A-Za-z0-9+/]{80,128}={0,2}$' `
            -or $attestationResult.mock -ne $false) { throw ('e2e_artifact_attestation_invalid:' + $artifact.path) }
        $artifact.signature = $attestationResult.signature
    }

    $runClock.Stop()
    $manifest = [ordered]@{
        schema = 'pinvou-remote-control-release-evidence'; version = 1; commit_sha = $actualHead
        provenance = [ordered]@{ kind = 'physical-dual-windows'; orchestrator = 'pinvou-windows-e2e-v1'; source_commit_sha = $actualHead; synthetic = $false; node_version = $trustedNodeVersion; node_sha256 = $TrustedNodeSha256; node_artifact_path = 'binaries/pinvou-node.exe'; node_authenticode_thumbprint = $TrustedNodeSignerThumbprint; node_publisher = $TrustedNodeSignerPublisher }
        attestations = @($attestationIdentities)
        relay = [ordered]@{ version = $gateResults.relay_version; config_fingerprint = $gateResults.config_fingerprint; public_origin_digest = $gateResults.public_origin_digest; registry_coverage_percent = 100; production_equivalent = $true; enrollment_deployment = $EnrollmentDeployment }
        legacy_relay = [ordered]@{ version = $legacyRelayVersion; sha256 = $legacyRelaySha; artifact_path = 'binaries/legacy-relay.bin'; endpoint_inventory_sha256 = $inventorySha; gateway_config_sha256 = $gatewayConfigSha; pid = [int]$legacyRuntime.pid; listener_host = $legacyRuntime.listener_host; listener_port = [int]$legacyRuntime.listener_port; route_id = $routeSpec.route_id; route_spec_artifact_sha256 = $legacyRouteSpecSha; runtime_artifact_sha256 = $legacyRuntimeSha; configuration_artifact_sha256 = $legacyConfigurationSha; probe_artifact_sha256 = $legacyProbeSha; bare_v2_artifact_sha256 = $legacyBareV2Sha; restoration_artifact_sha256 = $legacyRestorationSha; gateway_url_sha256 = $gatewayUrlSha; request_nonce = $legacyRequestNonce; traffic_counter_before = [long]$legacyProbe.traffic_counter_before; traffic_counter_after = [long]$legacyProbe.traffic_counter_after; gateway_traffic_observed = $true; response_correlated = $true; bare_v2_rejected = $true; route_restored = $true }
        driver = [ordered]@{ version = $driverVersion; sha256 = $driverSha; authenticode_thumbprint = $driverSigner.Thumbprint; publisher = $driverSigner.Publisher; artifact_path = 'binaries/pinvou-e2e-driver.exe'; source_commit_sha = $actualHead; attestation_public_key_sha256 = $orchestratorProviderIdentity.public_key_sha256; attestation_provider = $AttestationProvider; attestation_key_id = $orchestratorKeyId; private_key_embedded = $false }
        installers = @(
            [ordered]@{ role = 'A'; version = $installerVersion; sha256 = $installerSha; authenticode_thumbprint = $installerSigner.Thumbprint; publisher = $installerSigner.Publisher; artifact_path = 'binaries/pinvou-installer.exe'; attested_subject_name = $buildStatement.installer_subject_name; sbom_attested_subject_name = $buildStatement.sbom_subject_name; source_commit_sha = $actualHead; build_provenance_artifact_sha256 = $buildProvenanceArtifactSha; build_attestation_bundle_artifact_sha256 = $buildAttestationBundleArtifactSha; sbom_artifact_sha256 = $installerSbomArtifactSha; sbom_attestation_bundle_artifact_sha256 = $installerSbomAttestationBundleArtifactSha },
            [ordered]@{ role = 'B'; version = $installerVersion; sha256 = $installerSha; authenticode_thumbprint = $installerSigner.Thumbprint; publisher = $installerSigner.Publisher; artifact_path = 'binaries/pinvou-installer.exe'; attested_subject_name = $buildStatement.installer_subject_name; sbom_attested_subject_name = $buildStatement.sbom_subject_name; source_commit_sha = $actualHead; build_provenance_artifact_sha256 = $buildProvenanceArtifactSha; build_attestation_bundle_artifact_sha256 = $buildAttestationBundleArtifactSha; sbom_artifact_sha256 = $installerSbomArtifactSha; sbom_attestation_bundle_artifact_sha256 = $installerSbomAttestationBundleArtifactSha }
        )
        machines = @(
            [ordered]@{ role = 'A'; identity_digest = $machineFacts[0].IdentityDigest; windows_build = $machineFacts[0].WindowsBuild },
            [ordered]@{ role = 'B'; identity_digest = $machineFacts[1].IdentityDigest; windows_build = $machineFacts[1].WindowsBuild }
        )
        started_at = $runStartedAt; finished_at = [DateTime]::UtcNow.ToString($TimestampFormat)
        monotonic_start_ms = 0; monotonic_end_ms = [long]$runClock.ElapsedMilliseconds
        journeys = @($journeyEvidence); artifacts = @($artifacts); gates = $gates
    }
    $manifestPath = Join-Path $evidenceRoot 'manifest.json'
    [IO.File]::WriteAllText($manifestPath, (($manifest | ConvertTo-Json -Depth 12 -Compress) + "`n"), [Text.UTF8Encoding]::new($false))
    $validatorEnvironment = @{
        PINVOU_RELEASE_COMMIT_SHA = $actualHead; PINVOU_TRUSTED_WINDOWS_SIGNER_THUMBPRINT = $TrustedSignerThumbprint
        PINVOU_TRUSTED_WINDOWS_SIGNER_PUBLISHER = $TrustedSignerPublisher; PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY_SHA256 = $inventorySha
        PINVOU_TRUSTED_E2E_NODE_SHA256 = $TrustedNodeSha256; PINVOU_TRUSTED_E2E_NODE_SIGNER_THUMBPRINT = $TrustedNodeSignerThumbprint
        PINVOU_TRUSTED_E2E_NODE_SIGNER_PUBLISHER = $TrustedNodeSignerPublisher
        PINVOU_REMOTE_GATEWAY_CONFIG_SHA256 = $gatewayConfigSha; PINVOU_TRUSTED_E2E_ATTESTATION_PUBLIC_KEY_SHA256 = $orchestratorProviderIdentity.public_key_sha256
        PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER = $AttestationProvider; PINVOU_TRUSTED_E2E_ATTESTATION_KEY_ID = $orchestratorKeyId
        PINVOU_TRUSTED_E2E_ATTESTATION_ACL_SHA256 = $AttestationAclSha256; PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_MEASUREMENT_SHA256 = $AttestationProviderMeasurementSha256
        PINVOU_TRUSTED_E2E_ATTESTATION_PROVIDER_VENDOR = $AttestationProviderVendor; PINVOU_TRUSTED_E2E_PROVIDER_ROOT_CERTIFICATE_SHA256 = $TrustedProviderRootCertificateSha256
        PINVOU_TRUSTED_E2E_ORCHESTRATOR_ROLE_POLICY_SHA256 = $OrchestratorRolePolicySha256; PINVOU_TRUSTED_E2E_A_ROLE_POLICY_SHA256 = $ARolePolicySha256; PINVOU_TRUSTED_E2E_B_ROLE_POLICY_SHA256 = $BRolePolicySha256
        PINVOU_TRUSTED_E2E_PROVIDER_VERIFIER_CERTIFICATE_SHA256 = $TrustedProviderVerifierCertificateSha256; PINVOU_RELEASE_BUILD_PROVENANCE_SHA256 = $buildProvenanceArtifactSha
        PINVOU_RELEASE_BUILD_REPOSITORY = $InstallerBuildRepository; PINVOU_RELEASE_BUILD_WORKFLOW = $InstallerBuildWorkflow; PINVOU_RELEASE_BUILD_RUN_ID = [string]$InstallerBuildRunId
        PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT = $EnrollmentDeployment; PINVOU_TRUSTED_E2E_DRIVER_SHA256 = $driverSha
        PINVOU_RELEASE_INSTALLER_SHA256 = $installerSha; PINVOU_RELEASE_VERSION = $installerVersion
    }
    $validationProcess = Invoke-PinvouVerifiedLocalProcess -FilePath $trustedNode -ExpectedSha256 $TrustedNodeSha256 -ExpectedThumbprint $TrustedNodeSignerThumbprint -ExpectedPublisher $TrustedNodeSignerPublisher -Arguments @($EvidenceValidator, $manifestPath) -PurposeEnvironment $validatorEnvironment -AdditionalExpectedSha256 $validatorSourceHashes
    if ($validationProcess.ExitCode -ne 0) { throw 'e2e_evidence_validation_failed' }
} finally {
    $cleanupErrors = [Collections.Generic.List[string]]::new()
    $cleanupRouteId = if ($routeSpec) { [string]$routeSpec.route_id } else { '' }
    $cleanupOriginalStateDigest = if ($legacyConfiguration) { [string]$legacyConfiguration.original_state_digest } else { '' }
    $cleanupCanonicalInputDigest = if ($routeSpec) { [string]$routeSpec.canonical_input_state_digest } else { '' }
    $cleanupControlPlaneDigest = if ($legacyConfiguration) { [string]$legacyConfiguration.control_plane_state_before_digest } else { '' }
    for ($cleanupIndex = 0; $cleanupIndex -lt $sessions.Count; $cleanupIndex++) {
        $session = $sessions[$cleanupIndex]
        $cleanupLegacyPid = if ($cleanupIndex -eq 0) { $legacyRuntimePid } else { $null }
        $cleanupLegacyOwned = $cleanupIndex -eq 0
        try {
            Invoke-Command -Session $session -ScriptBlock {
                $remoteCleanupErrors = [Collections.Generic.List[string]]::new()
                $remoteRunRoot = $using:RemoteRoot
                $driverPath = Join-Path $remoteRunRoot 'pinvou-e2e-driver.exe'
                if ($using:cleanupLegacyOwned) {
                    $ownedProcessesConfirmedExited = $false
                    try {
                        if (-not (Test-Path -LiteralPath $driverPath)) { throw 'e2e_cleanup_driver_missing' }
                        $routeCleanup = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-deconfigure-gateway', '--gateway-url', $using:LegacyGatewayUrl, '--run-nonce', $using:runNonce, '--route-id', $using:cleanupRouteId, '--restore-original-state', $using:cleanupOriginalStateDigest, '--canonical-input-state-digest', $using:cleanupCanonicalInputDigest, '--expected-control-plane-state-digest', $using:cleanupControlPlaneDigest, '--require-full-control-plane-reread', '--require-public-route-withdrawn', '--idempotent', '--no-artifact', '--source-commit', $using:actualHead)) | ConvertFrom-Json
                        if ($LASTEXITCODE -ne 0 -or $routeCleanup.owned_route_removed -ne $true -or $routeCleanup.public_route_withdrawn -ne $true -or $routeCleanup.original_state_digest -ne $using:cleanupOriginalStateDigest -or $routeCleanup.restored_state_digest -ne $using:cleanupOriginalStateDigest -or $routeCleanup.control_plane_state_after_digest -ne $using:cleanupControlPlaneDigest) { throw 'e2e_legacy_gateway_cleanup_restore_failed' }
                    } catch { $remoteCleanupErrors.Add(('route:' + $_.Exception.Message)) }
                    try {
                        $owned = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-owned-pids', '--run-nonce', $using:runNonce, '--json', '--durable-registry-only')) | ConvertFrom-Json
                        if ($LASTEXITCODE -ne 0) { throw 'e2e_legacy_ownership_query_failed' }
                        $beforeOwnership = Assert-PinvouLegacyOwnershipRegistry -RunNonce $using:runNonce -Registry $owned -CallerObservedPid $using:cleanupLegacyPid
                        Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-stop', '--all-owned', '--run-nonce', $using:runNonce) | Out-Null
                        if ($LASTEXITCODE -ne 0) { throw 'e2e_legacy_cleanup_command_failed' }
                        foreach ($ownedPid in @($beforeOwnership.active)) {
                            for ($attempt = 0; $attempt -lt 20; $attempt++) {
                                if (-not (Get-Process -Id $ownedPid -ErrorAction SilentlyContinue)) { break }
                                Start-Sleep -Milliseconds 100
                            }
                            if (Get-Process -Id $ownedPid -ErrorAction SilentlyContinue) { throw 'e2e_legacy_cleanup_process_alive' }
                        }
                        $remaining = (Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('legacy-owned-pids', '--run-nonce', $using:runNonce, '--json', '--durable-registry-only', '--include-terminated')) | ConvertFrom-Json
                        if ($LASTEXITCODE -ne 0) { throw 'e2e_legacy_cleanup_registry_not_empty' }
                        $afterOwnership = Assert-PinvouLegacyOwnershipRegistry -RunNonce $using:runNonce -Registry $remaining -CallerObservedPid $null
                        if ($afterOwnership.active.Count -ne 0) { throw 'e2e_legacy_cleanup_registry_not_empty' }
                    } catch { $remoteCleanupErrors.Add(('legacy-process:' + $_.Exception.Message)) }
                }
                try {
                    if (Test-Path -LiteralPath $driverPath) {
                        Invoke-PinvouTrustedGuard -FilePath $driverPath -Arguments @('stop-role', '--all') | Out-Null
                        if ($LASTEXITCODE -ne 0) { throw 'e2e_role_cleanup_failed' }
                    }
                } catch { $remoteCleanupErrors.Add(('role-process:' + $_.Exception.Message)) }
                try {
                    $base = [IO.Path]::GetFullPath($using:RemoteBase).TrimEnd([IO.Path]::DirectorySeparatorChar)
                    $target = [IO.Path]::GetFullPath((Join-Path $base $using:runNonce))
                    if ((Split-Path -Parent $target) -ne $base -or (Split-Path -Leaf $target) -ne $using:runNonce) { throw 'e2e_staging_cleanup_boundary_invalid' }
                    if (Test-Path -LiteralPath $target) {
                        $targetItem = Get-Item -LiteralPath $target -Force -ErrorAction Stop
                        if (($targetItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_staging_cleanup_reparse_point' }
                        $marker = Get-Content -LiteralPath (Join-Path $target '.pinvou-e2e-owner.json') -Raw -Encoding UTF8 -ErrorAction Stop | ConvertFrom-Json
                        if ($marker.schema -ne 'pinvou-e2e-staging' -or $marker.run_nonce -ne $using:runNonce) { throw 'e2e_staging_cleanup_owner_mismatch' }
                        Remove-Item -LiteralPath $target -Recurse -Force -ErrorAction Stop
                    }
                    if (Test-Path -LiteralPath $target) { throw 'e2e_remote_cleanup_incomplete' }
                } catch { $remoteCleanupErrors.Add(('files:' + $_.Exception.Message)) }
                if ($remoteCleanupErrors.Count -gt 0) { throw ('e2e_remote_cleanup_failed:' + ($remoteCleanupErrors -join '|')) }
            } -ErrorAction Stop
        } catch {
            $cleanupErrors.Add(('role-' + @('A', 'B')[$cleanupIndex] + ':' + $_.Exception.Message))
        }
        try {
            Remove-PSSession -Session $session -ErrorAction Stop
        } catch {
            $cleanupErrors.Add(('session-' + @('A', 'B')[$cleanupIndex] + ':' + $_.Exception.Message))
        }
    }
    if ($cleanupErrors.Count -gt 0) { throw ('e2e_cleanup_failed:' + ($cleanupErrors -join '|')) }
}
