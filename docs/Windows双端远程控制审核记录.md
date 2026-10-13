# Windows 双端远程控制：审核与修复记录

> 本文保留各历史提交当时的审核结论，不替代当前固定 SHA 审核或发布验收。当前范围和 owner 边界以《Windows双端远程控制开发任务拆分》为准。

> A18 补录的 DEV-15 模型链路修复与历史测试数归属当时功能开发，不是本波部署脚本的新增产品实现或当前 head 实跑证据；历史提交与记录保留，不改写或重新认领。

> 用途：记录 DEV-01 起逐任务「独立审核 → 修复 → 复审」的过程与证据。
> 规则：审核结论必须以当前代码与真实测试输出为据；每条审核发现独立提交修复；审核者认为通过后该任务才算完成。

## A09 本轮行为边界（2026-10-07）

- 任务 dispatch 的 executor、事件 sink 与 Running 发布发生 unwind panic 时，注销并取消该任务 token，提交明确失败终态并释放 turn；已由取消或断线提交的终态不被覆盖。各独立 cleanup port 分别隔离 panic，不承诺抛错的 port 已完成资源释放；持久化持续失败仍由 Issue #1476 跟踪，不伪造成功。
- panic hook 仅在远控 Future 的每次 poll、析构与同步 cleanup 的线程局部作用域中隐藏 payload；挂起后恢复原作用域，其他线程及其他功能的 panic 继续交给原 hook。应用的远控与 computer-use 首次注册共用 core 串行链式入口，避免并发 take/set 丢掉另一方 hook；computer-use 原有 worker 脱敏语义不变。不临时交换全局 hook，不把敏感正文写入任务账本；第三方自行替换全局 hook 不在此保证内。进程 abort 不属于 unwind 恢复保证。
- controller WebSocket 每 5 秒发送 transport Ping，连续两个周期无入站响应即拆除当前安装租约并退避重连；发送阻塞也受同一静默截止时间约束，关闭 Close 写为单周期内尽力完成。原始连接成功、Pong 或错误帧不归零重连退避，只有通过当前角色、代际及租约准入的非错误业务控制帧才归零。transport Pong 不更新认证存活证据或恢复预算。
- 恢复窗口忽略仍在线一端合法的旧周期 `controller_liveness`：验证当前 socket、完整 lease context、角色、epoch 和非零 sequence，但不重建 liveness 状态、不发送误导性恢复错误、不延长首次冻结截止时间。非法帧仍拒绝。原审核所称“该错误必然立即终结 Relay lease”不符合当前调用边界；实际风险是 A 将并发错误当作恢复失败。
- 能力显示名按冻结协议统一为 128 个 Unicode 字符；本地工具/技能描述仅截断展示投影，不更改 opaque ID、授权或原始名称。覆盖 128/129/160/161 和多字节边界。
- 此记录描述实现边界，实际测试与独立审核结论在固定提交的 PR 回复中补充；不是发布验收。旧审核记录与原提交历史保留。

## A13 本轮确认与接线边界（2026-10-07）

- 明确披露承接 A12 的四项行为：suspect 前提前索证、以兜底请求计 attestation RTT、挑战应答准入/成功发送记账分离及 64 条/30 秒窗口、Relay 移除死存储；不把这些变更错归为 A13 新增。
- “不存在独立每秒 reaper”的审核判断不成立：命令层 `desktop_remote_controlled::spawn_watchdog_reaper` 由应用 setup 注册，独立于 worker 的两秒 poll；worker 退出不停止该任务。恢复函数注释补清真实入口，不删除正确的有界收口说明。
- 新增 Windows 无窗口 Wry 子进程回归，复用项目单一 Tauri context，通过真实 RelayClient WebSocket reader、manager 过滤/通道与 worker serve 完成握手、断线及恢复，未直接调用 resume helper 替代接线。测试只使用本地 loopback，Relay/A 输入由真实签名夹具模拟，不访问外部服务或真实业务端口。
- 每个负例等待真实 worker 分支处理完成后，再读取内部恢复相位、外显状态和 watchdog 截止；旧代际、错误 activation、外来请求分别不能推进 Prepared/Provisional/Prepared。临时移除三个对应门禁的反向测试分别捕获错误相位/终止，门禁已逐字恢复，不提交测试旁路。
- 首次断线冻结已有认证证据截止，恢复中各阶段不延长；最终认证完成建立新周期后，再次断线冻结该周期已有截止。失败关闭后实际 worker 退出及 generation 通道释放，允许重新订阅。观察接口仅 test 编译，release 没有回调或放宽校验。
- 旧 UI“36 条”明确标为当时历史计数，原数字和原提交 SHA 保留。此记录不是当前固定 SHA 的独立审核/CI 结论，也不替代真实 Relay、双 Windows、渲染/provider/部署或发布验收；#1421 的其他确定性并发及真实三方旅程仍须跟踪。

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

- 逐条核对：严格 LP 编码（`controller/crypto.rs` 的 `lp()`）、`CodeWrapPublicKey`/`HandshakePublicKey` 与两套 KeyPair 为互不兼容类型（用例 `code_wrap_and_handshake_keys_are_domain_separated_types`）、32 字节 receipt secret（`ReceiptSecret(Zeroizing<[u8;32]>)`）、Finished 绑定角色/transcript/receipt、receipt commitment 与 JS 共享固定向量一致、业务 AEAD 绑定方向+epoch+sequence 且带重放窗口、序列耗尽终态、密文长度在 AEAD 之前拒绝、`SensitiveString` 的 Debug/Display/Serialize 均不透出明文、Relay 签名用 ring ED25519 真实校验并有篡改用例。
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

## 7. DEV-07：Rust 桌面控制 transport 与状态所有者基座

审核轮次 1（审核提交 f9071c1f）

- 逐条核对：`controller/relay_transport.rs` 提供有界双队列（control/business 各自字节预算且互不侵占）、敏感帧硬上限与脱敏 Debug、取消令牌、lease 上下文准入、generation/lease epoch 门、注入式 RTT 采样；Web manager、B manager、A manager 三个状态域可独立启停；租约帧与预认证队列隔离，业务帧必须带当前认证租约（不存在 Web RPC 后门）；`app/commands/mod.rs` 一次性声明 9 个命令模块槽。
- 验收核对：13 条定向用例一一对应验收点（队列满释放预算与取消终态、三个状态域独立停止、敏感帧上限、业务帧需认证租约、租约安装绑定 transport generation、断线使已排队租约帧失效、业务字节不得占用控制预算、stale generation/epoch 在派发前拒绝、RTT 采样使用注入时钟）。
- 观察（无影响）：提交同时向 `Cargo.toml` 增加了 3 行（测试依赖），不在预计文件清单内但属必要构建配置。
- 验证：`cargo test remote_control::controller::relay_transport` 13 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 8. DEV-08：角色隔离 liveness 与 B 本地 30 秒 watchdog

审核轮次 1（审核提交 2c12c15f）

- 逐条核对：`LIVENESS_TOTAL_BUDGET_MS = 30_000`、`LIVENESS_BLACKHOLE_SUSPECT_MS = 15_000`；直接证据用 `received_at + 30000`，再扣除 `measured_rtt + 1000`（用例：`direct_candidate_deadline(100_000, 5_000) == 124_000`，即 24000ms 剩余）；attestation 走 Relay 签名兜底并扣除 `observed_age_ms`、`remaining_ttl_ms`、请求 RTT 与安全裕量（用例断言 119_000，即 19000ms 剩余），跨机 `issued_at_relay` 被有意忽略；新序号可推进候选、同序号只收紧、旧序号拒绝；黑洞 15 秒进入 suspect，B 心跳与普通流量不推进 deadline；Relay 侧 `controller-liveness.js` 只做盲转发与签名新鲜度证明。
- 验收核对：6 条 Rust 用例 + 6 条 Relay 用例覆盖 A 断 B 活、B 断 A 活、黑洞、另一角色/无效/重放心跳、Relay 不可达与本地 deadline 到期，均通过。
- 发现：无 blocker/major；minor 记录：`issued_at_relay` 被忽略属规范要求的保守策略（需求 8.1），已由用例锁定，无需修改。
- 验证：`cargo test remote_control::controller::liveness` 6 通过 / 0 失败；`node --test remote-control-relay/test/controller-liveness.test.js` 6 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 9. DEV-09：双端 provisional/pending-liveness resume

审核轮次 1（审核提交 5db233a0）

- 逐条核对：A/B 角色绑定 token 摘要（`resume_token_a_sha256`/`_b_sha256`，只在激活提交时原子轮换）；双方 prepare → `resume_provisional` → B activation ACK → `resume_activated_pending_liveness` → 首个 post-activation liveness 与 Relay 确认 → 双方 ready → `controller_resumed` 的完整链路；每个阶段都校验 `original_deadline_ms` 必须等于租约既有 `resume_deadline_ms`，因此 prepare/ACK/重试都不能把 DEV-08 的 30 秒预算重置；phase 白名单（`provisional`/`confirmed`/`pending_liveness`/`liveness_confirmed`）保证未激活前不承载业务；失败路径统一走 `#terminate`。
- 验收核对：用例覆盖单边 prepare、跨角色/失效/重放 token、ACK 不能跳过任一 E2E 确认、单边 prepare/ready 在原 deadline 内过期、提交失败走同一收尾、最终投递丢失时终止而非留下 active lease（不存在 Relay active / B terminal 分裂）。
- 发现：无新增 blocker/major；DEV-09 修改 `server.js`/`controller-router.js` 的边界问题已在 DEV-06 条目登记并通过 `6e8f5a31` 提供 hook 注入点解决。
- 验证：`cargo test remote_control::controller::resume` 4 通过 / 0 失败；`node --test remote-control-relay/test/controller-resume.test.js` 8 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 10. DEV-10：B 被控端 lifecycle、generation、watchdog 与 Tauri 命令

审核轮次 1（审核提交 66cf47be）

- 逐条核对：开启/刷新各自生成新 generation，回包按 generation 硬门丢弃（`stale_results_cannot_replace_a_new_generation`）；refresh 走 retire→Relay 撤销确认→本地清理→新 generation，撤销无法确认时失败关闭（`failed_revocation_fails_closed_and_rejects_workspace_paths_in_projection`）；状态枚举完整；本地 30 秒 watchdog 在 Relay 黑洞下仍能本地终止（`watchdog_blackhole_reconnects_then_fails_locally_without_relay`）；stop/立即断开无需网络即清空 lease/token/审批/敏感上下文（`stop_and_disconnect_clear_all_sensitive_context_without_network`）；工作区授权入口只接受真实可浏览目录。
- 内存边界：`controlled.rs` 与 `controlling.rs` 均无任何持久化调用（`std::fs`/`persist`/`write_atomic` 零命中），设备名、连接码、lease 与 token 只存在内存。
- 不做项核对：B 侧命令面只有 start/refresh/stop/disconnect_now/status/poll_watchdog/authorize_workspace/list_workspaces，无任何 Allow/Approve 入口；非 Windows 平台 `is_supported=false` 且命令返回 unsupported。
- 验证：`cargo test remote_control::controller::controlled` 7 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 11. DEV-11：A 主控端 claim、连接生命周期与远程历史壳层

审核轮次 1（审核提交 fa77e16e）

- 逐条核对：连接码粘贴/大小写/分隔符归一化（`pairing_code_normalization_and_invalid_characters_fail_closed`）；连接结果按 intent+generation 门控（`connection_result_is_generation_and_intent_gated`）；服务端失效原因统一为有界错误码且不自动重试旧码（`public_connection_errors_are_bounded_and_old_code_is_not_retried`）；显式断开关闭 live API 而离线历史保留（`explicit_disconnect_closes_live_api_but_history_remains_offline`）；恢复过程不回显旧 token（`transport_disconnect_and_resume_do_not_expose_the_old_token`）；进程重启后无残留操作（`process_restart_uses_an_empty_manager_and_no_stale_operations`）。
- 存储边界：`controlling.rs` 无持久化调用，历史只保存标题/脱敏设备名/终态引用，不含连接码与设备凭证。
- 验证：`cargo test remote_control::controller::controlling` 6 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 12. DEV-12：B 能力快照、模式可见性与代码工作区约束

审核轮次 1（审核提交 faaf6024）

- 逐条核对：快照只含 opaque ID/显示名、模式与预授权工作区；绑定 connection/lease epoch/relay epoch/generation 与 revision，30 秒过期；跨 generation 或过期快照被拒（`capability_snapshot_stale`/`capability_snapshot_expired`）；描述符容量与重复校验、`workspace_for_task` 仅作提示并要求任务接纳时重新校验 B 策略；序列化文本不含路径、api_key、secret。
- 命令层：`desktop_remote_capabilities` 只投影已安装工具与启用 Skill 的 id/显示名，并从 ACP 池读取 code agent readiness。
- 验证：`cargo test remote_control::controller::capabilities` 4 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 13. DEV-13：B 权威 Session、远程任务 ledger 与幂等执行

审核轮次 1（为本轮实现的提交 59ab94a9；审核者=会话内独立复核角色）

- 逐条核对：绑定 connection/lease/generation 的准入、`task_id + client_request_id` 幂等（同参回放/异参拒绝）、per-session turn lock、终态唯一且不可复活、dispatch 原子边界与 `result_unknown`、持久化失败失败关闭、重启后终态可证明、台账不含 prompt/宿主路径，均已由 `controller/task.rs` 用例覆盖（19 条）。
- 发现（major，已修复）：`cancel()` 只把台账标记为 `cancel_requested`，从未取消执行器的 `CancellationToken`，取消进行中任务实际不生效（违反 RC-T-006 与「所有已接纳任务最终有可验证终态」）。
  - 修复提交：`8e2e6445` — 服务维护 task_id→取消令牌注册表；`cancel()` 命中 `CancellationRequested` 时取消令牌；新增用例证明挂起执行器被真实中断并落 `cancelled` 终态。
- 验证：`cargo test remote_control::controller::task` 19 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 14. DEV-14：共享 transfer core、绑定与资源预算

审核轮次 1（提交 046b3391）

- 逐条核对：方向化 opaque handle（`ctr-up-`/`ctr-dn-`，`from_id` 由前缀推导方向）、连接/任务预算、块顺序与重复块幂等、滚动摘要与 mismatch 失败关闭、TTL 过期、按 task/connection/session 清理、Web handle 与 controller handle 类型隔离（`transfer_unknown`），均有用例（16 条）。
- 发现（major，已修复，与 DEV-17/18 同源）：`expire()` 无任何调用方，见第 16 节。
- 验证：`cargo test controller::transfer` 27 通过 / 0 失败（含后续新增用例）。
- 结论：**REVIEW: PASS**。

## 15. DEV-15：`temporary_model_package/v1` 构包、校验与全终态清理

审核轮次 1（提交 35ca4293）

- 逐条核对：v1 严格解析（未知字段/枚举/组合拒绝）、SavedModel→route 投影与不可导出原因、loopback/私网/metadata SSRF 拒绝、任务绑定、内存态存储与幂等释放、清理 hook 接入 DEV-13，均有用例（12 条）。
- 发现（major，已修复）：任务提交路径未校验 `controller_temporary` 是否真装有与该任务绑定的临时包，存在「拿不到凭证仍提交」的口子，与 RC-M-006/RC-M-009 冲突。
  - 修复提交：`7e296f7f` — 提交前要求存在 active_task == task_id 的临时包，否则 `model_package_missing`；新增用例覆盖无包、正确绑定与跨任务复用三种情形。
- 发现（major，已修复）：临时模型包此前只参与准入校验，未注入 chat executor/`EnginePool`，导致“我的模型”仍由 B 模型执行。现已接通 A 端任务级模型来源与加密模型包、B 端绑定安装、显式运行时模型/凭据覆盖及终态引擎回收；缺包、无凭据或绑定错误均失败关闭，不回退 B 模型。日志只记录 `model_source` 与安装阶段，不记录路由密钥。
- 回归补充（已修复）：临时包生成器一度把 `lease_epoch` 与 token 上限编码为 JSON number，而冻结协议规定为规范十进制 string，导致 A 在发送前以 `task_request_invalid` 拒绝，B 解析器也无法读取共享 fixture。现已统一双端编解码，并增加“生成包通过冻结协议校验”和“B 解析共享 fixture”两条跨边界测试。
- 验证：`cargo test remote_control::controller::temporary_model` 13 通过；`cargo test remote_control::controller::task` 24 通过；`cargo test remote_control::controller::controlling_session` 9 通过；`cargo test features::assistant::remote_turn` 3 通过；`cargo check --no-default-features --features local-embed` 通过。
- 结论：**REVIEW: PASS**。

## 16. DEV-16 / DEV-17 / DEV-18

审核轮次 1（提交 21b0be5f、a870b176、c4647f7a）

- DEV-16 逐条核对：绑定字段完整、首决定 CAS、同值幂等/异值 `permission_already_decided`、过期即拒绝、B/Local 调用 `permission_authority_invalid`、B stop 与 Allow 竞态的 dispatch 边界（先到拒绝 / 已提交只报 executed-or-unknown）、预览与范围脱敏，均有用例（12 + 3 条）。发现：无 blocker/major；登记缺口——审批请求尚未由执行路径产出并 dispatch 到 ACP，需引擎层接线。
- DEV-17 逐条核对：upload 串接共享 core 与 staging、完成句柄一次性、重复块不重复落盘、非法块/摘要失败同时清 staging 与台账、目标路径由 SessionStore 私有根解析、文件名二次清洗，均有用例（20 + 3 条）。
- DEV-18 逐条核对：opaque artifact ID 索引与 session 绑定、三档预览、分块下载顺序与摘要、源长度变化/跨 session/删除竞态失败关闭、投影不含路径，均有用例（8 + 3 条）。
- 发现（major，已修复，DEV-17/18 同源）：`TransferRegistry::expire()` 无调用方，未完成或断线遗留传输不会在有界时间内清理（RC-F-003）。
  - 修复提交：`9f74ba43` — upload/download 各自暴露 `expire()`（upload 同时 abort staging），两个运行时启动 60 秒周期回收；新增用例锁定 TTL 内不回收、过期恰好一次、预算归零、staging 清理与句柄失效。
- 验证：`cargo test controller::transfer` 27 通过 / 0 失败。
- 结论：**REVIEW: PASS**。

## 17. DEV-19：权威事件 journal、快照恢复与 A 独立历史

审核轮次 1（提交 5739c02d）

- 逐条核对：epoch+单调 sequence、有界容量与字节预算、重复事件幂等、每任务唯一终态 CAS、gap→快照（含 base sequence 与 digest）、payload key allowlist 与凭证/路径脱敏；A 侧独立历史可持久化、原子写、损坏或版本不符失败关闭、去重且不复写首个终态、删除本地副本不影响 B，均有用例（9 + 8 + 3 条）。
- 登记缺口（未修）：journal 目前只被任务状态 sink 填充，用户消息/流式文本/思考/工具/审批/附件/成果事件尚未由执行路径产出，需与引擎/执行适配一起接线。
- 验证：`cargo test controller::journal` 9 通过；`controller::history` 8 通过；`desktop_remote_history` 3 通过。
- 结论：**REVIEW: PASS**（含 1 项已登记缺口）。

## 18. DEV-20：前端桥、独立状态域与迟到结果防护

审核轮次 1（提交 5cf44980）

- 逐条核对：lib.rs 单一注册点 manage B/A/能力/任务/模型/审批/附件/成果/历史状态并注册全部 desktop_remote_* 命令；启动执行一次重启收尾；前端 `remote-control-state.js` 以 intent+generation/lease 门丢弃迟到结果、能力快照按 connection+revision 丢弃、任务事件去重且不回退终态；bridge 不接触 API Key/credential_ref/SavedModel 正文；Web lane 挂空桩且 WebUI v2 契约不变，均有 node 契约用例（8 + 5 条）与 bridge 域契约锁定。
- 发现：无 blocker/major。观察：`desktopRemoteControl` 域为桌面专属，`web_bridge_domain_contract` 已按 desktop-only 处理。
- 验证：`node --test desktop_remote_state.test.mjs` 8 通过；bridge/web 域契约 5 通过；`npm run build:ui` 通过。
- 结论：**REVIEW: PASS**。

## 19. DEV-21 / DEV-22 / DEV-23：B 面板、A 连接壳与远程会话 UI

审核轮次 1（提交 f2796d15、d38abd0b、9f18061e）

- 逐条核对：B 面板含开关、7 位码+秒级倒计时（aria-live 节流）、刷新、全状态、主控设备、模型来源、预授权工作区、立即断开、停止远程任务与只读审批，且无 Allow/Approve 入口；A 连接壳做码归一化、统一失效文案、隐藏窗口≠断开、显式断开；远程会话窗口覆盖模型来源（仅传 opaque modelId）、模式由能力快照决定、任务状态三语映射、result_unknown 提示、审批 A 决定、成果下载与离线副本标识；三语词条与 `ui_language_coverage` 通过。
- 发现（major，已修复，DEV-21 关联）：B 面板与命令面都依赖 watchdog，但见第 14 节 DEV-10 修复；面板本身不轮询。
- 登记缺口（未修）：三个 UI 面未跑浏览器 smoke（puppeteer），当前证据为源码契约 + vite build；发布前需补 UI 冒烟与真机可访问性检查。
- 验证：`controlled_device_panel_contract` 5、`connect_remote_device_contract` 5、`remote_conversation_contract` 5 通过；`npm run build:ui` 通过。
- 结论：**REVIEW: PASS**（含 1 项已登记缺口）。

## 20. DEV-24：遥测、审计、安全扫描与运维指标

审核轮次 1（提交 9525e932）

- 逐条核对：Relay 侧 `controller-telemetry.js` 与 Rust 侧 `controller/telemetry.rs` 都实现 stage/outcome/reason/latency 闭集、字段 allowlist 与身份/内容字段拒绝、匿名聚合、客户端去重、哨兵扫描（sk-*、Bearer、Authorization、私钥、vault://），并有用例（6 + 5 条）。
- 登记缺口（未修）：两侧模块尚未在 server.js 端点与 app 事件点实际埋点调用，属「统一 schema 与门禁已就绪、调用点待接线」状态；已在 DEV-25 runbook 的门禁中登记。
- 验证：`node --test controller-telemetry.test.js` 6 通过；`cargo test controller::telemetry` 5 通过。
- 结论：**REVIEW: PASS**（含 1 项已登记缺口）。

## 21. DEV-25：公网部署迁移、灰度、真实双机 E2E 与发布门禁

审核轮次 1（提交 788d87b8）

- 逐条核对：runbook 覆盖前置门禁命令、灰度步骤（registry 覆盖率、能力开关、自托管升级、容量限流、故障注入）、18.3 的 21 条旅程（并点名最易假通过的第 9～10/13/16/19/20/21 条）、监控阈值、回滚约束（只关能力、不删 registry/tombstone、拒绝裸 v2）与残余风险（受信 Relay，不抵御恶意主动 MITM）；`controller-release-gates.test.js` 把 runbook 章节、门禁命令、回滚不删持久状态、gateway 无旁路与遥测枚举固化为断言（6 条）。
- 登记缺口（未修）：真实双机/公网 E2E 未执行（本环境无法自证）；feature flag 配置与 CI workflow 尚未落成可执行产物。
- 验证：`node --test controller-release-gates.test.js` 6 通过。
- 结论：**REVIEW: PASS**（含 2 项已登记缺口，发布前必须补齐）。

## 22. 本轮汇总

- 已修复并独立提交：`d5c020d4`（DEV-01 旁路守卫）、`f3872c54`（DEV-02 门禁行为验证）、`0ce3d2c0`（DEV-03 明文扫描）、`6e8f5a31`（DEV-06 hook 注入点）、`8e2e6445`（DEV-13 取消真正生效）、`ec85eb72`（DEV-10 watchdog 周期推进）、`9f74ba43`（DEV-17/18 传输周期回收）、`7e296f7f`（DEV-15 临时模型缺失失败关闭）。
- 已登记但未修复（需引擎/部署层排期）：临时模型注入引擎、审批 dispatch 到 ACP、journal 事件源接线、遥测调用点接线、真实双机 E2E 与 feature flag/CI 产物、三个 UI 面的浏览器冒烟。
- 最终回归：`cargo test remote_control::` 245 通过 / 0 失败；`architecture-guard` 通过。
