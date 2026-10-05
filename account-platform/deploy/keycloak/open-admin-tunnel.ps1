[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$Server,

    [Parameter(Mandatory)]
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$User,

    [ValidateRange(1024, 65535)]
    [int]$LocalPort = 18081,

    [ValidateRange(1, 65535)]
    [int]$RemotePort = 18080,

    [string]$IdentityFile = ''
)

$sshArguments = @(
    '-N',
    '-T',
    '-o', 'ExitOnForwardFailure=yes',
    '-o', 'ServerAliveInterval=30',
    '-L', ('127.0.0.1:{0}:127.0.0.1:{1}' -f $LocalPort, $RemotePort)
)

if ($IdentityFile) {
    if (-not (Test-Path -LiteralPath $IdentityFile -PathType Leaf)) {
        throw "SSH identity file does not exist: $IdentityFile"
    }
    $sshArguments += @('-i', (Resolve-Path -LiteralPath $IdentityFile).Path)
}

Write-Host ('Open http://127.0.0.1:{0}/admin/ after the tunnel starts. Press Ctrl+C to close it.' -f $LocalPort)
& ssh @sshArguments ('{0}@{1}' -f $User, $Server)
if ($LASTEXITCODE -ne 0) {
    throw "SSH tunnel exited with code $LASTEXITCODE."
}
