Set-StrictMode -Version Latest

function global:Get-PinvouSha256Hex {
    param([Parameter(Mandatory = $true)][byte[]]$Bytes)
    $algorithm = [Security.Cryptography.SHA256]::Create()
    try { ([BitConverter]::ToString($algorithm.ComputeHash($Bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $algorithm.Dispose() }
}

function Assert-PinvouJourneyElapsed {
    param([long]$ObservedMilliseconds, [long]$TerminalElapsedMilliseconds, [long]$ToleranceMilliseconds = 1500)
    if ($ObservedMilliseconds -le 0 -or $TerminalElapsedMilliseconds -le 0 -or [Math]::Abs($ObservedMilliseconds - $TerminalElapsedMilliseconds) -gt $ToleranceMilliseconds) { throw 'e2e_monotonic_elapsed_mismatch' }
}

function Assert-PinvouRouteStateBinding {
    param([string]$CanonicalInputDigest, [string]$ControlPlaneBeforeDigest, [string]$ConfiguredDigest, [string]$RestoredControlPlaneDigest, [string]$RestoredOriginalDigest, [string]$RestoredStateDigest)
    if ($CanonicalInputDigest -notmatch '^[a-f0-9]{64}$' -or $ControlPlaneBeforeDigest -notmatch '^[a-f0-9]{64}$') { throw 'e2e_legacy_gateway_original_state_mismatch' }
    $payload = [Text.Encoding]::UTF8.GetBytes((@('pinvou-gateway-state-v1', $CanonicalInputDigest, $ControlPlaneBeforeDigest) | ConvertTo-Json -Compress))
    $expected = Get-PinvouSha256Hex $payload
    if ($ConfiguredDigest -ne $expected -or $RestoredControlPlaneDigest -ne $ControlPlaneBeforeDigest -or $RestoredOriginalDigest -ne $expected -or $RestoredStateDigest -ne $expected) { throw 'e2e_legacy_gateway_original_state_mismatch' }
}

function Invoke-PinvouCleanupSteps {
    param([Parameter(Mandatory = $true)][scriptblock[]]$Actions)
    $errors = [Collections.Generic.List[string]]::new()
    for ($index = 0; $index -lt $Actions.Count; $index++) { try { & $Actions[$index] } catch { $errors.Add("step-$index`:$($_.Exception.Message)") } }
    if ($errors.Count -gt 0) { throw ('e2e_cleanup_failed:' + ($errors -join '|')) }
}

function global:Assert-PinvouChildPath {
    param([string]$Root, [string]$Candidate, [string]$ErrorCode)
    $root = [IO.Path]::GetFullPath($Root).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $candidate = [IO.Path]::GetFullPath($Candidate)
    $rootUri = [Uri]::new($root + [IO.Path]::DirectorySeparatorChar)
    $candidateUri = [Uri]::new($candidate)
    $relativeUri = $rootUri.MakeRelativeUri($candidateUri)
    $relative = [Uri]::UnescapeDataString($relativeUri.ToString()).Replace('/', [IO.Path]::DirectorySeparatorChar)
    if ($relativeUri.IsAbsoluteUri -or [IO.Path]::IsPathRooted($relative) -or $relative -eq '..' -or $relative.StartsWith(('..' + [IO.Path]::DirectorySeparatorChar), [StringComparison]::Ordinal)) { throw $ErrorCode }
    $candidate
}

function global:Get-PinvouChildRelativePath {
    param([string]$Root, [string]$Candidate, [string]$ErrorCode)
    $root = [IO.Path]::GetFullPath($Root).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    $candidate = Assert-PinvouChildPath $root $Candidate $ErrorCode
    $relativeUri = [Uri]::new($root + [IO.Path]::DirectorySeparatorChar).MakeRelativeUri([Uri]::new($candidate))
    [Uri]::UnescapeDataString($relativeUri.ToString()).Replace('/', [IO.Path]::DirectorySeparatorChar)
}

function New-PinvouRunStaging {
    param([Parameter(Mandatory = $true)][string]$BaseRoot, [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$RunNonce, [Parameter(Mandatory = $true)][datetime]$StartedAtUtc)
    $base = [IO.Path]::GetFullPath($BaseRoot).TrimEnd([IO.Path]::DirectorySeparatorChar)
    if (-not (Test-Path -LiteralPath $base)) { New-Item -ItemType Directory -Path $base -ErrorAction Stop | Out-Null }
    $baseItem = Get-Item -LiteralPath $base -Force -ErrorAction Stop
    if (($baseItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_staging_base_reparse_point' }
    $target = Assert-PinvouChildPath $base (Join-Path $base $RunNonce) 'e2e_staging_path_escape'
    if (Test-Path -LiteralPath $target) { throw 'e2e_staging_already_exists' }
    New-Item -ItemType Directory -Path $target -ErrorAction Stop | Out-Null
    try {
        $marker = [ordered]@{ schema = 'pinvou-e2e-staging'; version = 1; run_nonce = $RunNonce; started_at = $StartedAtUtc.ToUniversalTime().ToString('O') }
        [IO.File]::WriteAllText((Join-Path $target '.pinvou-e2e-owner.json'), (($marker | ConvertTo-Json -Compress) + "`n"), [Text.UTF8Encoding]::new($false))
        New-Item -ItemType Directory -Path (Join-Path $target 'output') -ErrorAction Stop | Out-Null
        $global:PinvouE2ERunRoot = $target; $global:PinvouE2EOutput = Join-Path $target 'output'; $global:PinvouE2ERunNonce = $RunNonce
        [pscustomobject]@{ root = $target; output = $global:PinvouE2EOutput }
    } catch {
        if (Test-Path -LiteralPath $target) { Remove-Item -LiteralPath $target -Recurse -Force -ErrorAction SilentlyContinue }
        throw
    }
}

function global:Assert-PinvouTrustedStagedFile {
    param([Parameter(Mandatory = $true)][string]$RelativePath, [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$Sha256)
    if (-not $global:PinvouE2ERunRoot) { throw 'e2e_staging_not_initialized' }
    $path = Assert-PinvouChildPath $global:PinvouE2ERunRoot (Join-Path $global:PinvouE2ERunRoot $RelativePath) 'e2e_trusted_file_escape'
    $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_trusted_file_invalid' }
    if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $Sha256) { throw ('e2e_trusted_file_hash_mismatch:' + $RelativePath) }
    $path
}

function global:Assert-PinvouTrustedNode {
    $path = Assert-PinvouTrustedStagedFile 'pinvou-node.exe' $global:PinvouE2ENodeSha256
    $signature = Get-AuthenticodeSignature -LiteralPath $path
    if ($signature.Status -ne 'Valid' -or -not $signature.SignerCertificate -or $signature.SignerCertificate.Thumbprint.ToUpperInvariant() -ne $global:PinvouE2ENodeSignerThumbprint -or $signature.SignerCertificate.Subject -ne $global:PinvouE2ENodeSignerPublisher) { throw 'e2e_trusted_node_signature_mismatch' }
    $path
}

function global:Invoke-PinvouTrustedGuard {
    param([Parameter(Mandatory = $true)][string]$FilePath, [string[]]$Arguments = @(), [string[]]$AdditionalFilePaths = @(), [switch]$ReturnProcessResult)
    $root = [IO.Path]::GetFullPath($global:PinvouE2ERunRoot)
    $target = Assert-PinvouChildPath $root $FilePath 'e2e_process_target_not_attested'
    $targetRelative = Get-PinvouChildRelativePath $root $target 'e2e_process_target_not_attested'
    if (-not $global:PinvouE2EExpectedFileHashes.ContainsKey($targetRelative)) { throw 'e2e_process_target_not_attested' }
    $additionalExpected = @{}
    foreach ($additionalPath in $AdditionalFilePaths) {
        $absoluteAdditional = Assert-PinvouChildPath $root $additionalPath 'e2e_process_dependency_not_attested'
        $relative = Get-PinvouChildRelativePath $root $absoluteAdditional 'e2e_process_dependency_not_attested'
        if (-not $global:PinvouE2EExpectedFileHashes.ContainsKey($relative)) { throw 'e2e_process_dependency_not_attested' }
        $additionalExpected[$absoluteAdditional] = $global:PinvouE2EExpectedFileHashes[$relative]
    }
    $result = Invoke-PinvouWhitelistedProcess -FilePath $target -ExpectedSha256 $global:PinvouE2EExpectedFileHashes[$targetRelative] -Arguments $Arguments -AdditionalExpectedSha256 $additionalExpected
    $global:LASTEXITCODE = $result.ExitCode
    if ($ReturnProcessResult) { $result } else { if ($result.StdErr) { [Console]::Error.Write($result.StdErr) }; $result.StdOut }
}

function global:Invoke-PinvouTrustedNode {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $node = Assert-PinvouTrustedNode
    $uiScript = Assert-PinvouTrustedStagedFile 'remote-control-ui-smoke.mjs' $global:PinvouE2EUiSmokeSha256
    Invoke-PinvouTrustedGuard -FilePath $node -Arguments $Arguments -AdditionalFilePaths @($uiScript)
}

function global:Assert-PinvouExactProperties {
    param([Parameter(Mandatory = $true)]$Value, [Parameter(Mandatory = $true)][string[]]$Names, [string]$ErrorCode = 'e2e_schema_invalid')
    if ($null -eq $Value -or $Value -is [Array] -or $Value -is [string] -or $Value -is [ValueType]) { throw $ErrorCode }
    $actual = @($Value.PSObject.Properties.Name)
    if ($actual.Count -ne $Names.Count -or @($actual | Where-Object { $_ -notin $Names }).Count -ne 0) { throw $ErrorCode }
}

function global:Assert-PinvouLegacyOwnershipRegistry {
    param([Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$RunNonce, [Parameter(Mandatory = $true)]$Registry, [Nullable[int]]$CallerObservedPid)
    Assert-PinvouExactProperties $Registry @('schema', 'version', 'run_nonce', 'mock', 'active', 'terminated') 'e2e_legacy_ownership_query_failed'
    if ($Registry.schema -ne 'pinvou-legacy-owned-processes' -or [long]$Registry.version -ne 1 -or $Registry.run_nonce -ne $RunNonce -or $Registry.mock -isnot [bool] -or $Registry.mock -ne $false -or $Registry.active -isnot [Array] -or $Registry.terminated -isnot [Array]) { throw 'e2e_legacy_ownership_query_failed' }
    $seen = [Collections.Generic.HashSet[int]]::new()
    foreach ($pidValue in @($Registry.active) + @($Registry.terminated)) {
        if (($pidValue -isnot [int] -and $pidValue -isnot [long]) -or [int]$pidValue -le 0 -or -not $seen.Add([int]$pidValue)) { throw 'e2e_legacy_ownership_pid_invalid' }
    }
    if ($null -ne $CallerObservedPid -and -not $seen.Contains($CallerObservedPid.Value)) { throw 'e2e_legacy_ownership_caller_pid_missing' }
    [pscustomobject]@{ active = @($Registry.active | ForEach-Object { [int]$_ }); terminated = @($Registry.terminated | ForEach-Object { [int]$_ }) }
}

function Assert-PinvouStagedFiles {
    param([Parameter(Mandatory = $true)][object[]]$ExpectedFiles)
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($expected in $ExpectedFiles) {
        if (-not $seen.Add([string]$expected.RelativePath)) { throw 'e2e_staged_file_duplicate' }
        [void](Assert-PinvouTrustedStagedFile ([string]$expected.RelativePath) ([string]$expected.Sha256))
    }
}

function Remove-PinvouRunStaging {
    param([Parameter(Mandatory = $true)][string]$BaseRoot, [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$RunNonce)
    $base = [IO.Path]::GetFullPath($BaseRoot).TrimEnd([IO.Path]::DirectorySeparatorChar)
    $target = Assert-PinvouChildPath $base (Join-Path $base $RunNonce) 'e2e_staging_cleanup_boundary_invalid'
    if (-not (Test-Path -LiteralPath $target)) { return }
    $item = Get-Item -LiteralPath $target -Force -ErrorAction Stop
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_staging_cleanup_reparse_point' }
    $marker = Get-Content -LiteralPath (Join-Path $target '.pinvou-e2e-owner.json') -Raw -Encoding UTF8 -ErrorAction Stop | ConvertFrom-Json
    if ($marker.schema -ne 'pinvou-e2e-staging' -or $marker.run_nonce -ne $RunNonce) { throw 'e2e_staging_cleanup_owner_mismatch' }
    Remove-Item -LiteralPath $target -Recurse -Force -ErrorAction Stop
    if (Test-Path -LiteralPath $target) { throw 'e2e_staging_cleanup_incomplete' }
}
