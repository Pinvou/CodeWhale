[CmdletBinding()]
param(
    [Parameter()][string]$FilePath,
    [Parameter()][string[]]$Arguments = @(),
    [Parameter()][string]$ExpectedSha256
)

Set-StrictMode -Version Latest

function global:Invoke-PinvouOwnedProcess {
    param(
        [Parameter(Mandatory = $true)][Diagnostics.ProcessStartInfo]$StartInfo,
        [ValidateRange(1, 3600000)][int]$TimeoutMilliseconds = 600000,
        [Threading.CancellationToken]$CancellationToken = [Threading.CancellationToken]::None,
        [switch]$RetainSuccessfulScope
    )
    $source = Join-Path $PSScriptRoot 'windows-owned-process.cs'
    $item = Get-Item -LiteralPath $source -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_process_source_invalid' }
    $expected = Get-Variable -Name PinvouE2EOwnedProcessSha256 -Scope Global -ErrorAction SilentlyContinue
    $sourceHash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -and $sourceHash -ne $expected.Value) { throw 'e2e_process_source_hash_mismatch' }
    # Bind a cached CLR type to the actual staged source, rather than silently
    # reusing an assembly loaded by an earlier remoting run/version.
    $namespace = 'Pinvou.Release.H' + $sourceHash
    $typeName = $namespace + '.OwnedProcess'
    $runner = $typeName -as [type]
    if (-not $runner) {
        $code = [IO.File]::ReadAllText($source)
        if (-not $code.Contains('namespace Pinvou.Release {')) { throw 'e2e_process_source_invalid' }
        Add-Type -TypeDefinition $code.Replace('namespace Pinvou.Release {', ('namespace ' + $namespace + ' {')) -ErrorAction Stop
        $runner = $typeName -as [type]
    }
    $result = $runner::Run($StartInfo, $TimeoutMilliseconds, $CancellationToken, $RetainSuccessfulScope.IsPresent)
    if ($result.ExitCode -ne 0) {
        $result.StdOut = ''
        $result.StdErr = 'e2e_process_nonzero_exit'
    }
    $result
}

function global:Invoke-PinvouCleanupPreservingFailure {
    param(
        [Parameter(Mandatory = $true)][scriptblock]$Cleanup,
        [Management.Automation.ErrorRecord]$PrimaryFailure,
        [Parameter(Mandatory = $true)][ValidatePattern('^[a-z][a-z0-9_]{0,127}$')][string]$FailureCode
    )
    try { & $Cleanup } catch {
        if ($PrimaryFailure) {
            $PrimaryFailure.Exception.Data[$FailureCode] = $true
            [Console]::Error.WriteLine($FailureCode)
        } else { throw }
    }
}

function global:Close-PinvouOwnedProcessScopes {
    $scopes = Get-Variable -Name PinvouE2EOwnedProcessScopes -Scope Global -ErrorAction SilentlyContinue
    if (-not $scopes) { return }
    $failed = $false
    foreach ($scope in $scopes.Value) {
        try { $scope.Dispose() } catch { $failed = $true }
    }
    $global:PinvouE2EOwnedProcessScopes = @()
    if ($failed) { throw 'e2e_process_cleanup_unconfirmed' }
}

function global:ConvertTo-PinvouWindowsCommandLineArgument {
    param([AllowEmptyString()][string]$Value)
    if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') { return $Value }
    $builder = [Text.StringBuilder]::new()
    [void]$builder.Append('"')
    $backslashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq '\') { $backslashes++; continue }
        if ($character -eq '"') {
            [void]$builder.Append(('\' * (($backslashes * 2) + 1)))
            [void]$builder.Append('"')
            $backslashes = 0
            continue
        }
        if ($backslashes -gt 0) { [void]$builder.Append(('\' * $backslashes)); $backslashes = 0 }
        [void]$builder.Append($character)
    }
    if ($backslashes -gt 0) { [void]$builder.Append(('\' * ($backslashes * 2))) }
    [void]$builder.Append('"')
    $builder.ToString()
}

function global:Invoke-PinvouWhitelistedProcess {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$ExpectedSha256,
        [Parameter()][string[]]$Arguments = @(),
        [Parameter()][hashtable]$PurposeEnvironment = @{},
        [Parameter()][hashtable]$AdditionalExpectedSha256 = @{},
        [ValidateRange(1, 3600000)][int]$TimeoutMilliseconds = 600000,
        [Threading.CancellationToken]$CancellationToken = [Threading.CancellationToken]::None,
        [switch]$RetainSuccessfulScope
    )
    if (-not [IO.Path]::IsPathRooted($FilePath)) { throw 'e2e_process_path_not_absolute' }
    $absolute = [IO.Path]::GetFullPath($FilePath)
    $files = @{ $absolute = $ExpectedSha256 }
    foreach ($entry in $AdditionalExpectedSha256.GetEnumerator()) { $files[[IO.Path]::GetFullPath([string]$entry.Key)] = [string]$entry.Value }
    foreach ($entry in $files.GetEnumerator()) {
        $item = Get-Item -LiteralPath $entry.Key -Force -ErrorAction Stop
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'e2e_process_target_invalid' }
        if ((Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash.ToLowerInvariant() -ne $entry.Value) { throw 'e2e_process_hash_mismatch' }
    }

    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $absolute
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.Environment.Clear()
    foreach ($name in @('PATH', 'Path', 'PATHEXT', 'SystemRoot', 'SYSTEMROOT', 'WINDIR', 'TEMP', 'TMP')) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if ($value -and -not $info.Environment.ContainsKey($name)) { $info.Environment[$name] = $value }
    }
    foreach ($entry in $PurposeEnvironment.GetEnumerator()) {
        if ($entry.Key -match '(?i)(password|passwd|secret|token|cookie|private.?key|credential|username|user_name)') { throw 'e2e_process_environment_sensitive_key_forbidden' }
        $info.Environment[[string]$entry.Key] = [string]$entry.Value
    }
    $info.Arguments = (($Arguments | ForEach-Object { ConvertTo-PinvouWindowsCommandLineArgument ([string]$_) }) -join ' ')
    Invoke-PinvouOwnedProcess -StartInfo $info -TimeoutMilliseconds $TimeoutMilliseconds -CancellationToken $CancellationToken -RetainSuccessfulScope:$RetainSuccessfulScope
}

if ($MyInvocation.InvocationName -ne '.') {
    if (-not $FilePath) { throw 'e2e_process_file_missing' }
    if (-not $ExpectedSha256) { throw 'e2e_process_expected_hash_missing' }
    $result = Invoke-PinvouWhitelistedProcess -FilePath $FilePath -ExpectedSha256 $ExpectedSha256 -Arguments $Arguments
    [Console]::Out.Write($result.StdOut)
    if ($result.StdErr) { [Console]::Error.Write($result.StdErr) }
    exit $result.ExitCode
}
