<#
.SYNOPSIS
  本地单机自测：起一个本地 Relay、生成并落盘信任根，输出客户端需要的环境变量。

.DESCRIPTION
  Windows 双端远程控制的人工端到端自测需要一个可达的 Relay，并且双端必须 pin 同一份
  Relay 签名公钥（否则客户端会以 relay_trust_unconfigured 失败关闭）。

  本脚本只做三件事，且**不修改仓库内任何文件**：
    1. 在 $KeyStore（默认 %LOCALAPPDATA%\Pinvou3\remote-control-selftest）生成/复用：
       - relay-signing-key.json：Ed25519 私钥（PKCS#8 DER, base64）+ 原始 32 字节公钥（base64）
       - relay.env.ps1 / client.env.ps1：两侧环境变量（含生成的 HMAC 材料）
       - relay-state/：Relay 的 controller/enrollment 状态目录
    2. 以 development + TOFU 模式启动本地 Relay（隐藏窗口，日志写入 $KeyStore\relay.log）
    3. 打印客户端（A/B 两个进程）需要的环境变量与单机自测清单

  生成的私钥与 HMAC 材料只允许留在本机该目录，禁止提交或粘贴到 Issue/PR。

.PARAMETER Action
  Prepare（默认）：生成/复用密钥并启动 Relay
  Status：查看 Relay 进程与端口状态
  Stop：停止本脚本启动的 Relay
  EnvOnly：只生成密钥并打印环境变量，不启动 Relay

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\scripts\remote-control-local-selftest.ps1
  powershell -ExecutionPolicy Bypass -File .\scripts\remote-control-local-selftest.ps1 -Action Stop
#>
[CmdletBinding()]
param(
    [ValidateSet('Prepare', 'Status', 'Stop', 'EnvOnly')]
    [string]$Action = 'Prepare',
    [int]$Port = 8787,
    [string]$KeyStore = (Join-Path $env:LOCALAPPDATA 'Pinvou3\remote-control-selftest'),
    [string]$NodeExe = 'node'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$relayDir = Join-Path $repoRoot 'remote-control-relay'
$pidFile = Join-Path $KeyStore 'relay.pid'
$logFile = Join-Path $KeyStore 'relay.log'
$keyFile = Join-Path $KeyStore 'relay-signing-key.json'

function Write-Section([string]$text) {
    Write-Host ''
    Write-Host "== $text" -ForegroundColor Cyan
}

function Assert-Node {
    $resolved = Get-Command $NodeExe -ErrorAction SilentlyContinue
    if (-not $resolved) { throw "未找到 node（可用 -NodeExe 指定路径）。" }
}

function Initialize-KeyStore {
    if (-not (Test-Path -LiteralPath $KeyStore)) {
        New-Item -ItemType Directory -Path $KeyStore -Force | Out-Null
    }
    $stateDir = Join-Path $KeyStore 'relay-state'
    if (-not (Test-Path -LiteralPath $stateDir)) {
        New-Item -ItemType Directory -Path $stateDir -Force | Out-Null
    }
    return $stateDir
}

function New-OrLoadSigningKey([string]$stateDir) {
    if (Test-Path -LiteralPath $keyFile) {
        return Get-Content -LiteralPath $keyFile -Raw | ConvertFrom-Json
    }
    # Ed25519：私钥导出为 PKCS#8 DER（Relay 期望的格式），公钥取 SPKI DER 末 32 字节
    # （客户端 pin 的 raw ed25519 公钥），并立刻做一次签名/验签自检。
    $generator = @'
const crypto = require('node:crypto');
const fs = require('node:fs');
const [ , , keyPath, keyId ] = process.argv;
const { privateKey, publicKey } = crypto.generateKeyPairSync('ed25519');
const pkcs8 = privateKey.export({ type: 'pkcs8', format: 'der' });
const spki = publicKey.export({ type: 'spki', format: 'der' });
const rawPublic = spki.subarray(spki.length - 32);
const message = Buffer.from('pinvou-remote-control-selftest', 'utf8');
const signature = crypto.sign(null, message, privateKey);
const reconstructed = crypto.createPublicKey({
  key: Buffer.concat([spki.subarray(0, spki.length - 32), rawPublic]),
  format: 'der',
  type: 'spki',
});
if (!crypto.verify(null, message, reconstructed, signature)) {
  throw new Error('selftest_key_roundtrip_failed');
}
fs.writeFileSync(keyPath, JSON.stringify({
  signing_key_id: keyId,
  private_key_pkcs8_der_b64: pkcs8.toString('base64'),
  public_key_raw_b64: rawPublic.toString('base64'),
  generated_at: new Date().toISOString(),
}, null, 2));
console.log(JSON.stringify({ ok: true, key_id: keyId }));
'@
    $generatorPath = Join-Path $KeyStore 'generate-signing-key.cjs'
    Set-Content -LiteralPath $generatorPath -Value $generator -Encoding UTF8
    $keyId = 'selftest-relay-key-1'
    & $NodeExe $generatorPath $keyFile $keyId | Out-Null
    if ($LASTEXITCODE -ne 0) { throw '生成 Relay 签名密钥失败。' }
    return Get-Content -LiteralPath $keyFile -Raw | ConvertFrom-Json
}

function Write-EnvFiles {
    param([string]$stateDir, [object]$key)

    $enrollmentHmac = -join ((48..57) + (65..90) + (97..122) | Get-Random -Count 48 | ForEach-Object { [char]$_ })
    $pairingHmac = -join ((48..57) + (65..90) + (97..122) | Get-Random -Count 48 | ForEach-Object { [char]$_ })
    $wsUrl = "ws://127.0.0.1:$Port/pinvou3/remote/ws"
    $controllerState = Join-Path $stateDir 'controller-state.json'
    # legacy 文件必须在 controller 状态目录之外，否则 enrollment 配置失败关闭。
    $legacyState = Join-Path $KeyStore 'legacy-web-state.json'

    $relayEnv = @(
        '#' + ' 本地自测 Relay 环境（由 scripts/remote-control-local-selftest.ps1 生成，禁止提交）'
        "`$env:NODE_ENV = 'development'"
        "`$env:PINVOU_REMOTE_HOST = '127.0.0.1'"
        "`$env:PORT = '$Port'"
        "`$env:PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT = 'development'"
        "`$env:PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU = 'true'"
        "`$env:PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID = 'selftest-enrollment-hmac-1'"
        "`$env:PINVOU_REMOTE_ENROLLMENT_HMAC_KEY = '$enrollmentHmac'"
        "`$env:PINVOU_REMOTE_PAIRING_HMAC_KEY_ID = 'selftest-pairing-hmac-1'"
        "`$env:PINVOU_REMOTE_PAIRING_HMAC_KEY = '$pairingHmac'"
        "`$env:PINVOU_REMOTE_CONTROLLER_STATE_PATH = '$controllerState'"
        "`$env:PINVOU_REMOTE_STATE_PATH = '$legacyState'"
        "`$env:PINVOU_REMOTE_RELAY_SIGNING_KEY_ID = '$($key.signing_key_id)'"
        "`$env:PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64 = '$($key.private_key_pkcs8_der_b64)'"
    ) -join "`n"
    $relayEnvPath = Join-Path $KeyStore 'relay.env.ps1'
    Set-Content -LiteralPath $relayEnvPath -Value $relayEnv -Encoding UTF8

    $clientEnv = @(
        '#' + ' A/B 客户端环境（两个进程都要设置；禁止提交）'
        "`$env:PINVOU_REMOTE_RELAY_SIGNING_KEY_ID = '$($key.signing_key_id)'"
        "`$env:PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64 = '$($key.public_key_raw_b64)'"
        "`$env:PINVOU_REMOTE_RUNTIME_RELAY_WS_URL = '$wsUrl'"
    ) -join "`n"
    $clientEnvPath = Join-Path $KeyStore 'client.env.ps1'
    Set-Content -LiteralPath $clientEnvPath -Value $clientEnv -Encoding UTF8

    return [pscustomobject]@{
        RelayEnvPath  = $relayEnvPath
        ClientEnvPath = $clientEnvPath
        WsUrl         = $wsUrl
        PublicKeyB64  = $key.public_key_raw_b64
        KeyId         = $key.signing_key_id
    }
}

function Get-RelayProcess {
    if (-not (Test-Path -LiteralPath $pidFile)) { return $null }
    $relayPid = (Get-Content -LiteralPath $pidFile -Raw).Trim()
    if (-not $relayPid) { return $null }
    return Get-Process -Id ([int]$relayPid) -ErrorAction SilentlyContinue
}

function Test-LocalPort([int]$localPort) {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $async = $client.BeginConnect('127.0.0.1', $localPort, $null, $null)
        if (-not $async.AsyncWaitHandle.WaitOne(2000)) { return $false }
        $client.EndConnect($async)
        return $true
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

function Get-PortOwner([int]$localPort) {
    $connection = Get-NetTCPConnection -LocalPort $localPort -State Listen -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if (-not $connection) { return $null }
    return Get-Process -Id $connection.OwningProcess -ErrorAction SilentlyContinue
}

function Start-LocalRelay([string]$relayEnvPath) {
    $existing = Get-RelayProcess
    if ($existing) {
        Write-Host "Relay 已在运行（PID $($existing.Id)）。"
        return
    }
    $script = Join-Path $relayDir 'server.js'
    if (-not (Test-Path -LiteralPath $script)) { throw "未找到 $script" }
    $command = "& '$relayEnvPath'; & '$NodeExe' '$script' *>> '$logFile'"
    $process = Start-Process -FilePath 'powershell' -WindowStyle Hidden -PassThru -ArgumentList @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $command
    )
    Set-Content -LiteralPath $pidFile -Value $process.Id -Encoding ASCII
    Start-Sleep -Seconds 3
    $running = Get-RelayProcess
    if (-not $running) {
        Write-Host "Relay 启动失败，日志：$logFile" -ForegroundColor Red
        if (Test-Path -LiteralPath $logFile) { Get-Content -LiteralPath $logFile -Tail 20 }
        throw '本地 Relay 未能保持运行。'
    }
    if (Test-LocalPort -localPort $Port) {
        Write-Host "Relay 已监听 127.0.0.1:$Port（WebSocket 路径 /pinvou3/remote/ws）。"
    } else {
        Write-Host "Relay 进程在运行但端口未就绪，请查看日志：$logFile" -ForegroundColor Yellow
    }
}

function Stop-LocalRelay {
    $process = Get-RelayProcess
    if (-not $process) {
        Write-Host '未发现由本脚本启动的 Relay 进程。'
    } else {
        Stop-Process -Id $process.Id -Force
        Write-Host "已停止 Relay 包装进程（PID $($process.Id)）。"
    }
    # node 是包装进程的子进程，父进程退出后仍可能持有端口：按端口回收。
    $owner = Get-PortOwner -localPort $Port
    if ($owner) {
        Stop-Process -Id $owner.Id -Force
        Write-Host "已停止 Relay 服务进程（PID $($owner.Id)）。"
    }
    Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
}

function Show-ClientInstructions([object]$KeyInfo, [string]$clientEnvPath, [string]$wsUrl) {
    Write-Section '客户端（A/B 两个进程都要设置）'
    Write-Host "环境变量文件：$clientEnvPath"
    Write-Host "  PINVOU_REMOTE_RELAY_SIGNING_KEY_ID      = $($KeyInfo.signing_key_id)"
    Write-Host "  PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64 = $($KeyInfo.public_key_raw_b64)"
    Write-Host "  PINVOU_REMOTE_RUNTIME_RELAY_WS_URL      = $wsUrl"
    Write-Host '启动示例（在 pinvou3-app 仓库根目录）：'
    Write-Host "  powershell -NoProfile -Command `". '$clientEnvPath'; ./pinvou3-app/run-dev.sh`""

    Write-Section '本地单机自测清单'
    Write-Host '0) 先跑自动化门禁（不需要 Relay）：cargo test remote_control:: / node --test / architecture-guard'
    Write-Host '1) 进程 B：开启桌面远程控制 → 预期出现 7 位连接码、状态 waiting_connection'
    Write-Host '2) 进程 A：输入连接码连接 → 预期 connecting → safety_handshake → connected（无错误码）'
    Write-Host '3) A 发一句普通文本 → 预期 B 侧任务 admitted/running，A 侧看到 B 模型回复（journal 正文）'
    Write-Host '4) A 点断开 → 预期 A 回到 disconnected；B 收到 controller_disconnect，状态 stopped 且任务被中断清理'
    Write-Host '5) 断线恢复（可选）：任务运行中停掉 Relay 再拉起，30 秒内 A/B 应完成 resume，否则失败关闭为端点不可达'
    Write-Host '6) 记录观测点：状态序列、有界错误码、任务终态、心跳序号；禁止记录连接码/令牌/设备名/正文'
}

$stateDir = Initialize-KeyStore
Assert-Node

if ($Action -eq 'Status') {
    $process = Get-RelayProcess
    if ($process) { Write-Host "Relay 运行中：PID $($process.Id)，日志 $logFile" }
    else { Write-Host 'Relay 未运行。' }
    $owner = Get-PortOwner -localPort $Port
    if ($owner) { Write-Host "端口 $Port 由 PID $($owner.Id) 持有。" }
    exit 0
}
if ($Action -eq 'Stop') { Stop-LocalRelay; exit 0 }

$key = New-OrLoadSigningKey -stateDir $stateDir
$envInfo = Write-EnvFiles -stateDir $stateDir -key $key
Write-Host "密钥与环境文件目录：$KeyStore（禁止提交；签名自检已通过）"

if ($Action -eq 'EnvOnly') {
    Show-ClientInstructions -KeyInfo $key -clientEnvPath $envInfo.ClientEnvPath -wsUrl $envInfo.WsUrl
    exit 0
}

Start-LocalRelay -relayEnvPath $envInfo.RelayEnvPath
Show-ClientInstructions -KeyInfo $key -clientEnvPath $envInfo.ClientEnvPath -wsUrl $envInfo.WsUrl
Write-Host ''
Write-Host "停止 Relay：powershell -ExecutionPolicy Bypass -File .\scripts\remote-control-local-selftest.ps1 -Action Stop"
