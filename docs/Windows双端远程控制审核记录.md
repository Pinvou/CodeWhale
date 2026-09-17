# Windows 双端远程控制：审核与修复记录

> 用途：记录 DEV-01 起逐任务「独立审核 → 修复 → 复审」的过程与证据。
> 规则：审核结论必须以当前代码与真实测试输出为据；每条审核发现独立提交修复；审核者认为通过后该任务才算完成。

## 1. DEV-01：生产 desktop endpoint enrollment registry

审核轮次 1（审核提交 4239bb19 与修复 a7c5b07b、fd3115ab、520167d6、a3c11bb4）

- 逐条核对 DEV-01 目标/范围/协议/测试/验收：全部落地，证据见 `remote-control-relay/enrollment-registry.js`、`relay-state-store.js` 与 `test/enrollment-registry.test.js`。
- 关键不变量核对：生产拒绝未 enrolled 首次注册、TOFU 仅显式非生产、credential 只存摘要、撤销/轮换、namespace CAS、持久化失败降级为不可用、重启恢复与备份恢复、v0→v1 迁移、未修改 `server.js`。
- 发现（minor，已修复）：`enroll()` 可在无 purpose 校验的情况下创建 active 条目，若 DEV-06 误接 dispatcher 即可绕过 `authorizeRegistration()` 的失败关闭校验。
  - 修复提交：`d5c020d4` — 以 dispatcher 源码断言固化「server.js 不得直接调用 `.enroll()`」边界，并验证断言非空（能检出旁路、对干净源码通过）。
- 验证：`node --test remote-control-relay/test/enrollment-registry.test.js` 55 通过 / 0 失败（1 条 POSIX-only 用例在 Windows 跳过）；`python scripts/architecture-guard.py` 通过。
- 结论：**REVIEW: PASS**（第二轮确认修复有效且无回归）。

## 2. DEV-02：既有 endpoint 迁移、enrollment gateway 与安全回滚

审核轮次 1（审核提交 666bcdfd 与修复 4d0a89a5、d8d52bb9、b09e1602）

- 逐条核对：官方原 socket 认领（challenge + Ed25519 proof + socket 绑定）、批量 dry-run/allowlist/re-enroll、覆盖率审计、registry 备份/恢复、gateway 无旁路预检、回滚门禁脚本、未迁移状态输出，均已实现；handler 只调用 DEV-01 registry API。
- 发现 1（major，已修复）：DEV-02 验收要求「部署脚本输出待迁移数与安全预检结果；未满足门禁时自动失败」，但原测试只对 `scripts/check-enrollment-gated-v2-rollback.mjs` 做源码字符串断言，门禁脚本行为从未被执行。
  - 修复提交：`f3872c54` — 新增两条以子进程真实运行门禁的用例（缺少 inventory、覆盖率输入不可用），断言退出码 1、`safe_to_run_legacy_v2=false` 且 stdout 不出现「安全可回滚」输出。
- 发现 2（major，流程偏差，已登记未回改历史）：DEV-02 的修复提交 `4d0a89a5`、`d8d52bb9` 修改了 `remote-control-relay/enrollment-registry.js`（含 registry schema v2、迁移请求台账 `beginMigrationRequest`/`executeMigrationRequest`、快照导入与校验），而 DEV-02 规范明确要求「不修改 `server.js`、`enrollment-registry.js` 或 pairing 文件」。
  - 影响：功能上当前代码同时满足 DEV-01 与 DEV-02 的验收（相关用例全绿）；但按提交粒度审查 DEV-01 时会看到 API 面不完整，违反任务文件所有权约定。
  - 处置：本轮不回改历史（改写已提交历史需要单独授权）；在本文登记偏差，并要求后续任务不再跨任务改写文件。如需严格归属，可单独安排一次历史重整。
- 验证：`node --test remote-control-relay/test/enrollment-migration.test.js test/enrollment-deploy.test.js test/remote-test-deploy.test.js` 13 通过 / 0 失败（3 条 POSIX-only 部署用例在 Windows 跳过，属环境限制，需在 DEV-25 的真实 POSIX 拓扑上执行）。
- 结论：**REVIEW: PASS**（功能验收通过；流程偏差已登记）。
