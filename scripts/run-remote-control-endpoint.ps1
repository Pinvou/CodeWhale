<#
.SYNOPSIS
  以指定 Relay 端点启动 Pinvou3 桌面端（默认云端测试端点），用于 Windows 双端远程控制真机验证。

.DESCRIPTION
  被控端/主控端需要的是**进程环境变量**（应用内没有对应设置项）：
    PINVOU_REMOTE_PUBLIC_URL                 二维码/链接指向的公网地址（可选，缺省走持久配置或生产默认）
    PINVOU_REMOTE_RELAY_WS_URL               Relay WebSocket 地址（DEFAULT_RELAY_WS_URL 的覆盖）
    PINVOU_REMOTE_RUNTIME_RELAY_WS_URL       仅本进程生效的临时覆盖（不写回持久配置）
    PINVOU_REMOTE_RELAY_SIGNING_KEY_ID       信任根 key id（缺失即 relay_trust_unconfigured 失败关闭）
    PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64  信任根公钥（原始 32 字节 ed25519 的 base64）

  取值优先级（manager/persistence.rs:598-628）：RUNTIME_RELAY_WS_URL > RELAY_WS_URL > 持久配置 > 内置生产默认；
  信任根只来自进程环境，没有 UI 或持久化入口。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\scripts\run-remote-control-endpoint.ps1 -DryRun
  powershell -ExecutionPolicy Bypass -File .\scripts\run-remote-control-endpoint.ps1
  powershell -ExecutionPolicy Bypass -File .\scripts\run-remote-control-endpoint.ps1 -Mode Production
#>
[CmdletBinding()]
param(
    [ValidateSet('Test', 'Production')]
    [string]$Mode = 'Test',
    [string]$ExePath,
    [string]$WsUrl = 'wss://pinvou.com/pinvou3/remote-test/ws',
    [string]$PublicUrl = 'https://pinvou.com/pinvou3/remote-test',
    [string]$SigningKeyId = 'relay-test-key-1',
    [string]$SigningPublicKeyB64 = 'otRocTSuXfkyfUKA6Gx71zvxII60UdxGK5h/dJ8aOIg=',
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

if ($Mode -eq 'Test') {
    $env:PINVOU_REMOTE_PUBLIC_URL = $PublicUrl
    $env:PINVOU_REMOTE_RELAY_WS_URL = $WsUrl
    $env:PINVOU_REMOTE_RELAY_SIGNING_KEY_ID = $SigningKeyId
    $env:PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64 = $SigningPublicKeyB64
    Write-Host "已设置测试端点环境变量（仅本进程及其子进程生效）:" -ForegroundColor Cyan
    Write-Host "  PINVOU_REMOTE_PUBLIC_URL   = $PublicUrl"
    Write-Host "  PINVOU_REMOTE_RELAY_WS_URL = $WsUrl"
    Write-Host "  PINVOU_REMOTE_RELAY_SIGNING_KEY_ID = $SigningKeyId"
    Write-Host "  PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64 = $SigningPublicKeyB64"
} else {
    Remove-Item Env:PINVOU_REMOTE_PUBLIC_URL -ErrorAction SilentlyContinue
    Remove-Item Env:PINVOU_REMOTE_RELAY_WS_URL -ErrorAction SilentlyContinue
    Remove-Item Env:PINVOU_REMOTE_RUNTIME_RELAY_WS_URL -ErrorAction SilentlyContinue
    Remove-Item Env:PINVOU_REMOTE_RELAY_SIGNING_KEY_ID -ErrorAction SilentlyContinue
    Remove-Item Env:PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64 -ErrorAction SilentlyContinue
    Write-Host "已清除本进程的测试端点变量，应用将走持久配置或内置生产默认。" -ForegroundColor Cyan
}

if (-not $ExePath) {
    $candidates = @(
        (Join-Path $env:LOCALAPPDATA 'pinvou3\pinvou3-tauri.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\pinvou3\pinvou3-tauri.exe'),
        (Join-Path $PSScriptRoot '..\pinvou3-app\src-tauri\target\release\pinvou3-tauri.exe')
    )
    $ExePath = $candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
}

if ($Mode -eq 'Test') {
    $healthUrl = "$PublicUrl/healthz"
    try {
        $health = Invoke-RestMethod -Uri $healthUrl -TimeoutSec 10
        if ($health.ok) {
            Write-Host "端点健康检查通过：$healthUrl（ok=true）" -ForegroundColor Green
        } else {
            Write-Warning "端点返回 ok != true：$healthUrl"
        }
    } catch {
        Write-Warning "无法从本机访问 $healthUrl（本机对 pinvou.com 可能不可达）；不影响已设置的环境变量。"
    }
}

if (-not $ExePath -or -not (Test-Path -LiteralPath $ExePath)) {
    Write-Warning '未找到 pinvou3-tauri.exe；请用 -ExePath 指定安装目录下的可执行文件。'
    Write-Host '环境变量已就绪：可在同一终端里手动启动应用，例如：'
    Write-Host '  & "$env:LOCALAPPDATA\pinvou3\pinvou3-tauri.exe"'
    exit 0
}

Write-Host "启动：$ExePath"
if ($DryRun) {
    Write-Host '（-DryRun：只设置并打印，不启动应用）'
    exit 0
}
Start-Process -FilePath $ExePath | Out-Null
Write-Host '已启动。被控端请到「设置 → 通用 → 桌面远程控制」点「开启远程控制」获取 7 位连接码。' -ForegroundColor Green
