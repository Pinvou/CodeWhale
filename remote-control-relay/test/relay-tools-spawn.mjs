import { spawnSync } from "node:child_process";

// 自托管 windows-light runner 高负载下，powershell.exe/bash.exe/node 子进程
// 可能超过单次 spawnSync 预算（CI 实证 ETIMEDOUT：round-5 起三个测试文件
// 先后各挂一次，见 PR #2626）。对超时定向重试一次；持续挂死仍在第二次
// 超时后如实失败。其余 spawn 错误（ENOENT 等）不重试。
export function spawnSyncWithRetry(command, args, options) {
  for (let attempt = 0; ; attempt += 1) {
    const result = spawnSync(command, args, options);
    if (!result.error || result.error.code !== "ETIMEDOUT" || attempt >= 1) return result;
  }
}
