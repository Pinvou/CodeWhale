[CmdletBinding()]
param(
    [Parameter()][string]$FilePath,
    [Parameter()][string[]]$Arguments = @(),
    [Parameter()][string]$ExpectedSha256
)

Set-StrictMode -Version Latest

function global:ConvertTo-PinvouWindowsCommandLineArgument {
    param([AllowEmptyString()][string]$Value)
    if ($Value -notmatch '[\s"]') { return $Value }
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
        [Parameter()][hashtable]$AdditionalExpectedSha256 = @{}
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
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $info
    try {
        if (-not $process.Start()) { throw 'e2e_process_start_failed' }
        $stdout = $process.StandardOutput.ReadToEnd()
        $stderr = $process.StandardError.ReadToEnd()
        $process.WaitForExit()
        [pscustomobject]@{ ExitCode = $process.ExitCode; StdOut = $stdout; StdErr = $stderr; ProcessId = $process.Id }
    } finally { $process.Dispose() }
}

if ($MyInvocation.InvocationName -ne '.') {
    if (-not $FilePath) { throw 'e2e_process_file_missing' }
    if (-not $ExpectedSha256) { throw 'e2e_process_expected_hash_missing' }
    $result = Invoke-PinvouWhitelistedProcess -FilePath $FilePath -ExpectedSha256 $ExpectedSha256 -Arguments $Arguments
    [Console]::Out.Write($result.StdOut)
    if ($result.StdErr) { [Console]::Error.Write($result.StdErr) }
    exit $result.ExitCode
}
