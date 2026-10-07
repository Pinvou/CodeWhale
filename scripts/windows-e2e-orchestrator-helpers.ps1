Set-StrictMode -Version Latest

function global:Get-PinvouSha256Hex {
    param([Parameter(Mandatory = $true)][byte[]]$Bytes)
    $algorithm = [Security.Cryptography.SHA256]::Create()
    try { ([BitConverter]::ToString($algorithm.ComputeHash($Bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $algorithm.Dispose() }
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
