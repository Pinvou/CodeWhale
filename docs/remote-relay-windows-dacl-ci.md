# Windows Relay DACL 持续验证

`windows-relay-dacl` 在 Relay 相关 Draft / Ready PR 和 Merge Queue 上使用 Windows runner、Node 24 及系统 Windows PowerShell，保留既有 Ubuntu 全 Relay 检查。它列入 required-gate 的依赖与失败汇总，不允许权限测试失败、跳过或零测试成为通过。

本地同等入口：`node scripts/run-windows-relay-dacl.mjs`。先安装项目 Relay lockfile 依赖；脚本在非 Windows 显式拒绝，而不是报告 skip。

本点仅接线既有真实用例：生产 RelayStateStore 默认保护/核验，无注入权限替身；随后真实 Get-Acl 读取专属目录、marker、primary、backup 与 lock，检查 owner、protected DACL、主体集合、FullControl 及继承规则。状态与临时测试目录由原 fixture 管理，不接触应用运行目录。

其他 Windows 单元测试中的 protect/verify 注入错误仅为错误控制流证据，不能冒称真实 DACL。权限篡改后的异常状态保全、其它 Windows Relay 生命周期及已知 rollback / gateway readiness 失败仍需独立验收；此点不是整个 Windows Relay、部署或跨机器资格。
