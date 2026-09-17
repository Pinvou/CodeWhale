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

## 3. DEV-03：Relay 发码、全局摘要占用与 tombstone/keyring

审核轮次 1（审核提交 9be011fb、fd58c78c、80a067a4）

- 逐条核对：31 字符集与 CSPRNG 发码、5 分钟 TTL、32 次碰撞重采样与耗尽密封错误、多 digest key（keyring）查重与 keyring revision CAS、先 tombstone 后返回码（同一 CAS 写入 tombstone+registration，CAS 成功后才返回 sealed 结果）、同请求同参幂等与异参/跨 relay epoch 拒绝、持久化失败不返回码、重启后隔离与旧 pairing 不可恢复，均已实现。
- 发现（major，已修复）：验收要求「代码库不存在 B/A 生成路径；码明文不进日志或遥测」，但原测试只断言持久化状态中无明文，缺少日志/信道扫描与客户端生成路径检查。
  - 修复提交：`0ce3d2c0` — 真实注册后用一次性密封信封解出明文，断言明文不出现在 wire 投影、注册期 console 输出与持久化状态；扫描三个 pairing 模块不得有任何日志调用；断言客户端 `controlled.rs` 只有 `validate_pairing_code`、无码生成与随机数调用。
- 验证：`node --test remote-control-relay/test/pairing-code.test.js` 12 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 4. DEV-04：共享交互槽位、claim reservation 与竞态仲裁

审核轮次 1（审核提交 62a98e50）

- 逐条核对：`EndpointInteractionSlots` 以 revision CAS 串行化 Web lease / desktop reservation / controller lease；claim 归一化（含 IPv6 source 身份）与来源/连接/全局三档限流；reservation owner 绑定；10s reserved / 20s handshake 超时；断线（owner/endpoint）清理；失败自动换码通知（`onPairingInvalidated`，失败可重试）；统一 `pairing_code_invalid`；Web-to-Web 接管保留。
- 验收核对：14 条用例覆盖「两个 A claim 与 Web join 共享同一槽位」「已有 Web owner 时 claim 不消费码」「A/Web 同时竞争只有一个 CAS 赢家」「stale revision 拒绝且保留 Web-to-Web 接管」「pending reservation 晋升 controller lease 不终结 pairing」，与规范验收一致。
- 发现（minor，仅登记）：`PROTOCOL.md` 不在 DEV-04 的预计文件清单内（该文档由 DEV-00 定义、DEV-06 冻结），本任务为其新增的消息族补了 30 行说明；属文档边界偏差，无功能影响。
- 验证：`node --test remote-control-relay/test/controller-reservation.test.js` 14 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 5. DEV-05：A/B 密码学原语、规范 receipt 与敏感序列化

审核轮次 1（审核提交 d4c2c72e 与修复 57e0c79b、fc64574c、9f75a354、84bb1166）

- 逐条核对：严格 LP 编码（`lp()` 于 crypto.rs:64）、`CodeWrapPublicKey`/`HandshakePublicKey` 与两套 KeyPair 为互不兼容类型（用例 `code_wrap_and_handshake_keys_are_domain_separated_types`）、32 字节 receipt secret（`ReceiptSecret(Zeroizing<[u8;32]>)`）、Finished 绑定角色/transcript/receipt、receipt commitment 与 JS 共享固定向量一致、业务 AEAD 绑定方向+epoch+sequence 且带重放窗口、序列耗尽终态、密文长度在 AEAD 之前拒绝、`SensitiveString` 的 Debug/Display/Serialize 均不透出明文、Relay 签名用 ring ED25519 真实校验并有篡改用例。
- 固定向量：`frozen_cross_language_crypto_vector_matches_every_stage` 同时校验 shared secret、Finished key、业务 root/派生 key、nonce、AAD 与密文，与 `remote-control-relay/test/fixtures/controller-protocol-v1.json` 完全一致。
- 发现（minor，仅登记）：与 DEV-04 相同，`PROTOCOL.md` 属 DEV-00/DEV-06 的文档边界，本任务的修复提交对它做了 79 行说明性更新；无功能影响。
- 验证：`cargo test controller::crypto` 13 通过 / 0 失败；`node --test remote-control-relay/test/controller-protocol.test.js` 20 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 6. DEV-06：Relay 中央组合、两阶段握手与 controller lease 原子建立

审核轮次 1（审核提交 b96d7abf）

- 逐条核对：`controller-lease.js` 在一次 CAS 中消费 pairing 并创建 controller lease（`consumeForControllerLease`），A/B 各自独立 resume token 只持久化 sha256；双 receipt 提交后才产出 `controller_joined`；`controller-router.js` 组合 DEV-01～04 handler 并暴露 liveness/resume hook 槽；`server.js` 完成 dispatcher 接线；claim socket 临时身份、消息顺序与超时、失败终结 pairing 均有实现。
- 验收核对：用例覆盖「两个相反 receipt 原子消费 pairing 且只产生一个 lease」「commitment/Finished/receipt 的顺序、角色、socket 与重放」「原子提交失败不产生 lease 且终结 pairing」「握手超时与任一端断线使 pairing 失效」「router 组合与显式断开」。
- 发现（major，已修复）：DEV-06 承诺「为 DEV-08/09 冻结 liveness/resume hook 槽，后续不再修改 server.js/router」，但 DEV-09 的提交 5db233a0 仍需修改 `server.js`（6 行：import/构造 `ControllerResumeHooks`）与 `controller-router.js`（50 行），说明冻结槽缺少可扩展注入点。
  - 修复提交：`6e8f5a31` — router 新增 `additionalHooks` 注入数组（保持原有 hook 形状校验），新增用例证明新 hook 可在不改动 router/server 接线的情况下参与分发，非法 hook 失败关闭。
- 验证：`node --test remote-control-relay/test/controller-handshake.test.js remote-control-relay/test/controller-router.test.js` 10 通过 / 0 失败；三个真实三方握手用例（`relay.test.js`）此前单独运行 25 通过 / 0 失败。
- 结论：**REVIEW: PASS**。
