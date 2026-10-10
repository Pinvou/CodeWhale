# Windows 双端远程控制：发布、灰度与回滚

> 对应 DEV-25，需求基线为需求文档第 14～20 节。所有发布判断使用稳定旅程 ID，不使用会随文档编辑漂移的自然序号。

## 1. 发布前置门禁

只有以下门禁全部通过且证据属于待发布 commit，才允许把 `PINVOU_DESKTOP_CONTROLLER_ENABLED` 设为精确值 `1`：

| 门禁 | 命令 / 证据 | 说明 |
|---|---|---|
| Relay 全量 | `npm --prefix remote-control-relay test` | 含 enrollment registry、迁移、发码、槽位、握手、liveness、resume、telemetry |
| 客户端全量 | `cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1` | A/B 状态机、任务台账、传输、审批、模型包、journal |
| 前端与桥 | `npm --prefix pinvou3-app test`、`npm --prefix pinvou3-app run build:ui`、`npm --prefix pinvou3-app run test:webui` | WebUI v2 行为与旧 fixture 不回归 |
| 架构守卫 | `python scripts/architecture-guard.py` | 依赖方向与平台边界 |
| 发布门槛清单 | `node --test remote-control-relay/test/controller-release-gates.test.js` | 本文件的必备章节、回滚约束与残余风险 |
| 双 Windows 27 旅程 | `scripts/run-windows-remote-control-e2e.ps1` | 必须完整真实双机证据 |
| 真实 Tauri UI | `npm --prefix pinvou3-app run test:remote-control-ui -- --cdp-url <loopback-cdp> --output <new-file>` | 真实渲染与交互 |
| 最终发布门禁 | `npm --prefix remote-control-relay run release:gate` | 证据属于待发布 commit，任何失败不放行 |
| 泄露扫描 | 下述三个真实 Rust 用例、`node --test remote-control-relay/test/controller-sensitive-scan.test.js remote-control-relay/test/controller-telemetry.test.js` | 真实错误投影、Debug 脱敏、受控枚举与哨兵扫描；零匹配视为失败 |

### 1.1 泄露扫描的非空执行证据

已删除的 Rust `controller::telemetry` 不再是验证入口。以下每条必须实际报告 `1 passed; 0 failed`，不能仅用 cargo 退出码 0 或 `0 tests` 作为通过证据：

```bash
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::relay_runtime::tests::log_allowlist_rejects_identifier_shaped_values_and_secrets -- --exact --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::relay_runtime::tests::real_remote_failure_projections_do_not_persist_any_sensitive_category -- --exact --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::resume::tests::resume_debug_redacts_current_and_retired_material -- --exact --test-threads=1
```

发布门槛测试校验这些入口与现有 Rust 源码一致；这项静态合同不能代替上述实际执行，也不能代替真实双机证据。

`release:gate` 在开关开启时要求 `PINVOU_REMOTE_RELEASE_EVIDENCE` 与 `PINVOU_RELEASE_COMMIT_SHA`，重新校验 artifact 哈希、27 条旅程、敏感扫描、registry 覆盖率和旧 Relay 回滚预检；缺任何一项都失败。开关关闭时仍验证值只能为空或 `0`，不会删除 registry、pairing tombstone 或回退 state schema。

开关关闭的普通发布包不包含已启用的桌面远控能力；该组合通过不代表桌面远控发布资格通过。公开发布本功能必须在 A/B/Relay 三端精确设为 `1`，并取得全部开启门禁的真实证据，不能以关闭开关的 CI 结果替代。

1. **Registry 覆盖率**：在仓库根目录运行 `remote-control-relay/scripts/enrollment-migrate.js` 的 dry-run，统计已迁移 endpoint 比例。未达到 100% 之前不开启公网强制门禁。
2. **打开桌面控制能力开关**：只打开 `desktop_remote_control` 能力，不动 enrollment registry、pairing tombstone 与 digest keyring。
3. **自托管升级说明**：自托管 Relay 必须先升级到 registry-aware 版本，或在所有入口前挂 enrollment gateway（无旁路）。
4. **容量与限流**：确认连接码领取、claim、握手速率限制与告警阈值生效；观察爆破告警与容量水位。
5. **故障注入**：单边网络隔离、Relay 重启、A/B 单边断线，确认 30 秒预算、resume 与终态清理符合需求。

开关开启的 CI 门禁必须在能读取受保护生产状态的发布 runner 上执行（通过 `PINVOU_REMOTE_RELEASE_RUNNER` 选择），并注入启用/关闭两份受保护环境文件、controller state、旧 endpoint inventory、已部署 gateway 配置、公网入口清单、固定签名证书与受信驱动/安装包哈希。这些生产文件不上传到 evidence artifact；门禁现场重算 inventory/gateway 指纹并与双机证据比对。任一变量、文件或跨 run artifact 不可用时，结果必须为 BLOCKED。

跨 workflow 证据只接受 GitHub API 确认属于当前 commit、固定 workflow ID 与精确 path、当前 ref/repository、成功完成且 digest 与受保护期望值一致的 artifact。首版统一使用受保护远程签名服务与 Ed25519：orchestrator、A、B 分别声明 provider identity、公钥、互异 role/tenant policy 和独立 provider receipt，artifact 必须按 `machine_role` 选择对应 key 验签。provider 证明必须由独立 verifier 产生，绑定本次 nonce、角色/policy、provider/key ID、ACL、measurement、driver SHA 和新鲜时间，并通过 vendor 证书链、根/叶证书 allowlist 与签名校验；driver 不得循环自证。缺少真实 provider 证明时发布资格为 BLOCKED。

远端 UI/process helper 不得从 `PATH` 临时选择 Node。受保护环境提供固定 `node.exe` 路径、SHA-256、Authenticode 发布者和证书指纹；编排器校验后复制到本次 nonce staging，并把版本、摘要、发布者、证书指纹和 artifact 路径写入 manifest provenance。Node artifact 仍由 orchestrator attested key 签名，任一不一致即 BLOCKED。

GitHub protected environment/self-hosted runner、runner OS 与同一 SID 进程、受保护 secrets/vars，以及环境中配置的 Git、GitHub CLI、Node 工具链构成本发布流程的可信执行基座。workflow 从受保护 checkout 直接执行当前 `github.sha` 的编排器和 verifier；安装证明验证器只接受受保护变量给出的 GitHub CLI 绝对路径，不从 `PATH` 回退。此门禁防御不可信网络/Relay、外部下载产物、用户或部署配置、伪造/过期/错 commit 证据、普通路径逃逸/符号链接和敏感信息泄露，并坚持任何失败都不放行。

本威胁模型明确不防御受信 runner 上的管理员、同一 SID 恶意常驻进程、运行中 junction/TOCTOU 替换或 PID 复用攻击，也不为 Git/GH/Node 自建二次可信启动链。普通路径边界仍以 canonical relative path 校验，所有 staging/证据目标拒绝 reparse point，外部下载产物必须通过 GitHub API 的 workflow/run/head/ref/repository/conclusion 与 artifact digest 校验。JSON Schema 由 Ajv 编译为版本化、可重现核对的单文件 validator；清理在 `always()` 中执行，失败告警并阻断。生产证据编排不安装 Relay 依赖；启用发布门禁与 PR `release-contract` 使用锁定的 Relay 依赖执行敏感扫描，必须为 `npm ci` 指定 `--ignore-scripts`。

旧 Relay 启动时，受信 driver 必须在返回进程信息前把 `run_nonce → pid` 原子写入 durable registry，使响应丢失时仍能按本次 nonce 清理。编排器核对实际进程的可执行路径、SHA-256、loopback listener 与 owner PID；清理时先撤销 owned route 并复核公网路由消失和原配置恢复，再查 registry、停止本次 owned 进程、确认进程退出且 registry `active=[]`。脱敏 route spec 必须严格解析、敏感扫描、登记 SHA-256，并由 configure/probe 显式接收该摘要。任何一步失败均阻断发布。

安装包不能只靠人工填写 SHA-256 关联源码。`windows-remote-control-build-evidence` 在受保护 `windows-release` environment 从当前 SHA 构建安装包，并分别为 installer 与 SPDX 2.3 生成 GitHub artifact attestation；`gh attestation download` 的唯一 `sha256-*.jsonl` 文件才是 bundle。编排器和 release gate 都执行 `gh attestation verify`，以 attestation 中的原始 subject 名称和摘要关联下载后可能重命名的文件。SPDX 必须以最终 NSIS staging manifest 为事实源，逐文件覆盖 package-lock、Cargo.lock、所有公开/私有 runtime 映射、最终 `resources`、生成的配置与 installer hooks、主二进制和 installer，并包含完整 `DEPENDS_ON`/`CONTAINS` 关系；manifest 中任一 staged file 缺失、重复归属或多出都会失败。正式 Windows artifact 必须复用已 qualification 的相同安装包字节。自报布尔值、只填写 hash 或门禁后重建另一个安装包一律失败关闭。

## 2. Enrollment 迁移与灰度

1. 从受保护的旧 endpoint inventory 运行 `enrollment-migrate.js --coverage`；覆盖率未达 100% 不进入灰度。
2. 官方环境由已认证旧 socket 完成 challenge/proof 认领，保留 endpoint ID/token/Web 链接；身份冲突必须重新分享。
3. 自托管先 `--dry-run`，再用稳定 request ID 执行批量 allowlist/re-enroll；re-enroll 会明确使旧 Web 链接失效。
4. 生产只允许 `NODE_ENV=production`、独立验证过的 `self_hosted`，且 `PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false`。当前 `official` live claim 尚未接线，预检及启动明确失败关闭；不得以开发 TOFU 作为生产降级。
5. 先验证开关关闭组合门禁，再在 Relay、A、B 同时设为 `1`；按 registry 覆盖率和 endpoint allowlist 小范围灰度，而不是删除持久状态。
6. 观察 DEV-24 固定枚举指标；任何敏感面哨兵、未 enrolled 注册、裸 v2 路由或旁路出现即停止。

## 3. 真实双机 E2E

唯一旅程目录是 `remote-control-relay/docs/schemas/remote-control-journeys.v1.json`，包含 `RC-E2E-001`～`RC-E2E-027`。详细操作与证据字段见《Windows双端远程控制双机演练清单》。以下能力属于本次发布必须通过的完整链路，不得再标记为“首版隐藏”或以 `blocked` 代替通过：

- A/B 模型路径、临时模型、代码/普通会话；
- Allow/Deny/timeout/B stop 审批竞态；
- 附件 reserve/upload/commit 与成果两阶段发布；
- 双端 journal/历史、离线只读 RemoteProxy；
- 两 A、两个 B、Web/controller 交叉、Web-to-Web 既有接管；
- A/B 单边隔离、Relay 重启、refresh generation、`deadline <= 30000 - 实测 RTT - 1000` 的单调安全预算；
- digest key 轮换碰撞、真实旧 Relay gateway 回滚、裸 v2 拒绝；
- 幂等台账、结果未知、旧入口与废弃 RemoteHistoryStore 零读取。

真实旧 Relay 演练不是自报布尔值：inventory/config 使用拒绝额外字段的严格结构且各只有一个 enabled route；编排器仅将经敏感扫描的最小 route spec 传到 A，通过受信 driver 激活“指定公网 gateway → 本机 loopback 旧 Relay”，并独立核对旧进程 executable hash、listener owner PID。原状态摘要由完整规范化 inventory、完整规范化 config 与 driver 实际读取的 gateway control-plane 状态共同计算；撤销后必须重新读取完整 control-plane 状态并与原摘要交叉验证。配置/探测证据绑定 run nonce、route ID、`no_bypass`、config/inventory/listener 摘要、随机请求 nonce、前后流量计数及关联响应；裸 v2 使用独立、签名且角色为 A 的 artifact。owned route 必须在 `finally` 撤销并主动验证公网路由消失和原配置恢复，随后才停止旧进程；任一清理失败都阻断发布。

双机 helper 每次使用随机 run nonce 创建独占 staging 目录，采用同卷原子落位并拒绝已有目录、普通路径越界或重解析点。复制前在编排机生成全部输入哈希，复制后 A/B 对安装包、driver、provider verifier、UI smoke、process guard、orchestrator helper 及回滚输入逐文件重算；远端 Node 使用受保护环境配置的固定绝对路径。清理只允许删除带本次 nonce owner marker 的目录，并在每个清理步骤失败后继续执行其余清理，最终聚合失败阻断。

审批和附件旅程使用 `journey-prepare → CDP 实际操作 → journey-finalize` 协调。ChatView、审批按钮和附件输入的 `data-coordination-id` 来自真实 pending permission/task 状态；CDP 只按 exact ID 操作，零个或多个 pending 都失败。manifest、journey 和 UI artifact 交叉核对 coordination digest；附件还必须由 B 独立生成并签名 receive artifact，且文件摘要一致后才能完成。

## 4. 监控与容量阈值

| 指标 | 发布阈值 |
|---|---|
| pairing/claim 无效或限流 | 60 秒累计 10 次触发 `pairing_bruteforce_suspected` |
| busy/容量拒绝 | 5 分钟累计 5 次触发 `controller_capacity_pressure` |
| resume 失败 | 5 分钟累计 3 次触发 `controller_recovery_failure_spike` |
| handshake rejected | 连续 10 分钟 >5% 阻断扩量 |
| liveness/watchdog expired | 任一出现即停止扩量并核查 30 秒预算 |
| task 幂等冲突 | 任一出现即核对 stable request ID 与 fingerprint |
| attachment/成果预算拒绝 | 持续出现即停止扩量并复核客户端/容量配置 |

发布证据只允许聚合枚举和匿名摘要；禁止连接码、endpoint/lease/token/key、IP、设备名、正文、工具参数、文件名和路径。受保护的 stats 端点只允许 `operations_admin`。

## 5. 回滚

- 首选回滚是把 Relay、A、B 的统一开关关闭；入口隐藏/拒绝，但 registry、tombstone、digest keyring 和 state schema 原样保留。
- 最低回滚版本必须 registry-aware。真实旧 v2 二进制只能监听 literal loopback，并置于唯一 enrollment gateway 后；nginx 所有公开 websocket 路径必须指向 gateway，不能直达旧 Relay。
- 发布前执行 `scripts/check-enrollment-gated-v2-rollback.mjs`：要求 endpoint inventory 覆盖率 100%、逐个公网入口 marker/升级/匿名拒绝通过、OS 监听检查无 wildcard；任一步失败都输出 `safe_to_run_legacy_v2=false`。
- 裸 v2、`localhost` 作为监听证明、无 inventory、TOFU、无法确认全入口或 gateway 可旁路时，回滚必须被拒绝。
- 回滚演练保存旧二进制哈希、gateway 配置指纹、入口检查和旧 Web 链接结果，不保存任何凭证或业务正文。

当前生产脚本的自动回滚只恢复代码/包/Web/schema/drop-in，不备份或还原部署环境文件、registry 与身份；不能把旧代码配上新 self_hosted/TOFU=false 环境就当作业务恢复，空 registry 仍会拒绝存量端点。回滚健康探针弱于部署侧，HTTP 200 或 unit active 不证明 controller/telemetry/存量注册与旧 Web 可用。上述运维验收与故障矩阵继续受 [#1428](https://github.com/Pinvou/pinvou3/issues/1428)、[#1429](https://github.com/Pinvou/pinvou3/issues/1429) 门禁约束，未取证则不得部署/灰度，不通过恢复 TOFU 或裸 v2 绕过。

## 6. Feature flag 关闭组合门禁

当前身份握手基于受信 Relay 和受信认证控制面，不声称抵御恶意 Relay 的主动 MITM；installer/Provider 证据不能替代这项信任前提。

构建与运行开关是两层独立约束：构建时 `PINVOU_DESKTOP_CONTROLLER_ENABLED=0`（以及未明确开启的 release/custom profile）将关闭状态固化到产物，运行时设为 `1` 或不设变量都不能重新开启。启用产物必须以构建值 `1` 生成并取得本节之外的全部发布验收证据；受保护的 installer 证据 workflow 固定构建值 `1`。debug 默认开启只用于本地开发，不提供发布资格。

仅构建值为 `1` 的 Windows A/B 产物允许运行时未设变量或精确 `1` 开启；运行时 `0` 或其他值关闭。非 Windows 平台始终 unsupported。Relay 不使用桌面构建值，仍要求运行时精确 `1`、`self_hosted` 与 TOFU=false；当前 `official` 认证模式未接线，不能作为已支持的生产验收组合。普通 Windows 发布继续构建关闭远控的产物，不以“免验收”隐式开放功能。

遥测快照由受信生产聚合服务提供，并非独立的实际会话取证；门禁预期 commit、URL、token-file 和回滚输入来自受保护配置，部署环境文件不得覆盖这些预期值。真实双机与 Provider 验收仍单独必需。

1. A/B 平台能力返回 `desktopRemoteControlSupported=false`，设置入口不可见。
2. 即使绕过 UI 调用，A connect 与 B start 等网络入口返回 `desktop_remote_control_unsupported`，不打开 socket、不创建租约。
3. Relay 不创建 controller runtime，v1 controller 子协议/消息被拒绝，既有 Web v2 行为保持。
4. 关闭前后 registry、pairing tombstone 与 digest keyring 指纹一致。
5. build=0 的产物不能通过运行期开关重新开启；须安装经完整验收的 build=1 产物。build=1 的运行期开关从 0 重新开启时仅恢复新配对入口，不恢复旧连接、旧 lease、旧 token 或旧 generation。

原21条演练与历史36条源码契约不是当前发布收据；真实双机、真实Tauri UI与真机断线验收仍须按当前27条目录取得，不用mock冒烟替代。

## 7. 证据归档

`remote-control-relay/docs/schemas/remote-control-release-evidence.v1.schema.json` 和严格 validator 共同约束 manifest：

> 当前传播仅核对代码与门禁合同，不代表真实双 Windows、生产等价部署或 DEV-25 发布证据已经取得。缺少真实证据时发布资格仍为 `BLOCKED`，不能以历史 pilot、单元测试或本次传播替代。

- commit SHA；A/B 安装包版本、SHA-256、Authenticode 指纹；
- Relay 版本、配置指纹、公网 origin 摘要、100% registry coverage、生产等价声明；
- A/B 匿名机器身份摘要与 Windows build；
- UTC 与单调时间范围；27 个稳定 journey ID 及每项 trace/check artifact SHA-256；
- flag-off、敏感扫描、监控阈值、registry、rollback、真实 UI smoke 六个组合 gate；UI smoke 内含设置、B 面板、连接后 ChatView、Allow、Deny、附件、成果七个逐动作阶段。

validator 拒绝额外字段、敏感字段名、绝对/逃逸路径、缺文件、哈希不一致、旧 commit、`blocked`/`fail`、重复或乱序旅程。CI 只能消费 validator 的退出码，不能用“源码包含某字符串”代替执行。

## 8. 残余风险

- 信任模型明确假设 Relay 是受信认证控制面；端到端加密用于防被动抓包、日志/存储读取和旁路密钥替换，**不声称抵御恶意 Relay 的主动 MITM**。解决该风险需要独立设备认证或用户比较码，不属于首版。
- 模型 endpoint 最终出站仍可能二次解析、使用代理或跟随重定向；按需求第 10.5 节登记并持续评审。
- 当前开发工作站若缺两台隔离 Windows、生产等价 Relay、签名包或真实旧二进制，最终 release gate 应失败。这是正确的 blocked 状态，不得伪造 pass 或证据。
## 附录：历史排期与部署快照

> 以下仅保留前序波次的历史记录，不是当前发布许可、现行功能合同或最新验收结果。当前发布门禁以第 1～7 节的 27 条真实旅程和 evidence validator 为准；旧 21 条旅程、三面冒烟及未完成能力清单不能替代现行要求。未核验或修改线上状态。

| 项 | 处置 |
|---|---|
| A 端临时模型、代码模式、附件与成果导出 | UI 按首版开关隐藏；能力未声明时不暴露入口 |
| 远程普通会话的工具审批 | 未闭环前禁用工具，B 侧不出现 Allow 入口 |
| 历史登记：Relay WS 子协议与 `controller_capabilities` 首帧 | **子协议限制已被后续实现取代（superseded）**：当前 A 端通过 `relay_client::controller_websocket_request` 声明 `pinvou-desktop-controller-v1`，Relay 通过 `remote-control-relay/server.js` 的 `handleProtocols` 协商。独立 `controller_capabilities` handler 仍未接入，当前能力通过 register/claim 与 `controller_joined` 协商；不再以本历史行声称当前客户端不声明子协议，也不将其视为发布验收。 |

### 7.3 后续排期（不阻断首版灰度）

| 项 | 说明 |
|---|---|
| 设备显示名 | 冻结协议 v1 的 `controller_joined`/`pairing_register` 不携带对端显示名，A 端用占位 `remote-device`、B 端留空由 UI 兜底；需协议扩展 |
| B 回复非流式 | 首版在轮次结束后从 B 的 `SessionStore` 取文本，A 端看到的是整轮完成后的回复 |
| A 端快照引导 | 首次收到 journal 事件时直接从该事件起算；完整快照引导属可选优化 |
| 附件/成果事件回传 | wire `event_kind` 只用冻结闭集，进行中状态、附件与快照不回传（属后续任务） |
| 跨 lease 任务清理语义 | B 端 `cancel` 只校验帧租约与任务存在性；旧 lease 遗留任务由 stop/refresh/远端断开的清理端口负责 |
| `DesktopControlledCleanup` 适配器测试 | 仓库无 `mock_app` 先例，当前由 manager 侧三条路径用例覆盖触发面，Tauri 级测试列入后续 |
| remote_control 之外的既有失败 | 全量 `cargo test --lib` 有 6～8 条与本功能无关的既有失败（进程树终止超时、marketplace 工具缺失等），属环境限制，已在任务文档登记 |

### 7.4 当前自动化恢复接线回归的证据边界

`app/commands/remote_control/reader_resume_tests.rs` 的独立子进程创建真实 Windows Wry 宿主，调用 feature 中的共享恢复夹具，经真实 RelayClient reader、RemoteControlManager 和 ControlledRelayWorker 执行恢复。前序 A08 的宿主分层、断线 prepared 屏障、错误 final liveness 拒绝、retained epoch 非空断言和旧 reader 围栏在 A13 中继续保留；不重新启用旧 feature 内宿主初始化，也不以非 Connected 投影的 `None == None` 声称覆盖内部 epoch。

此用例仅在 Windows 编译与执行，须取得当前提交适用 Windows 原生 CI 的真实执行结果；Linux / macOS 的条件排除或 Draft skip 不是该用例的通过证据。子进程整体 15 秒、父进程 25 秒和局部等待 3 秒均为失败退出的 watchdog，不是平台启动性能承诺；本轮未完成冷启动及高负载 Windows runner 的时限余量验收。

它使用真实 loopback WebSocket 与认证测试材料，没有真实 renderer、在线 provider、生产网关或双 Windows 设备，不能替代第 7.1 节的发布验收。测试数量以当前提交实际输出为准。

## 8. 生产 controller lane 启用历史记录（2026-09-19，不是当前安全豁免）

本节仅保留当次结论，管理入口、登录账号、服务/状态/密钥布局及精确备份定位在不入库的运维清单登记。没有在本轮重新部署或修改线上服务。

A18 已移除生产预检的 development + TOFU 降级门禁：当前必须 `NODE_ENV=production`、`PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted`、`PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false`；`official` live claim 尚未接线，预检及启动均失败关闭。历史降级没有可沿用的永久豁免：在下次新/重部署或正式发布之前（以先发生者为截止），必须取得端点清单、100% registry 迁移与全入口网关预检证据并关闭 TOFU，仍受 [#1428](https://github.com/Pinvou/pinvou3/issues/1428) 阻断。本次只修复代码与文档，未核验或改变线上状态。

### 8.1 历史变更与信任根

当次将旧 v2-only Relay 升级为 controller-v1，使用权限受限的环境文件注入密钥与 HMAC，状态保留到代码目录之外。私钥仅在部署环境，0600 文件、0700 目录，不入库、不入日志。

随客户端公开发布的生产信任根继续为 `prod-relay-key-1` / `nGlRK79H+YdNVhmZOwkI2T83uhiEBqfZeFbMrJ28LQM=`；对应 `manager::PRODUCTION_PROFILE`，原提交 `225fa4cd`。脱敏不改变该公钥或用户公开访问地址；密钥轮换必须随客户端更新并验证。

当前脚本的通用托管合同（不是实际线上资产清单）：拒绝 controller 环境文件符号链接，校验 `root:root`、`0600`、production + self_hosted + TOFU=false 以及预检定义的状态路径；原子维护 `40-controller.conf`，本机执行无状态变更的 controller-v1 路由烟测，并在本机与公网健康检查中确认 `desktop_controller_enabled=true`。失败须恢复旧版 JS 模块和 systemd drop-in，不允许新旧模块混装或只以 Web v2 正常作为发布成功。

### 8.2 历史 TOFU 降级与硬截止

当次 legacy 状态只有 version 与 revoked_endpoints，没有完整端点清单，不能证明 registry 覆盖率 100%。历史上经确认临时使用 development + TOFU 接纳未登记客户端；这不是当前部署方案或永久豁免。

在下次新/重部署或正式发布之前（以先发生者为截止），必须取得端点清单、100% registry 迁移及全入口网关预检证据，使用 official/self_hosted 并关闭 TOFU。未完成就阻断，不因本次文档修改宣称完成迁移。

### 8.3 历史备份、回滚与验收边界

当次有完整备份和脚本版本快照；准确定位与恢复命令仅在受控运维清单保存，不在本文提供能直接针对真实部署执行的删除、停启或覆盖命令。历史曾通过关闭 controller lane 或还原旧代码恢复服务，但不是当前允许沿用的裸 v2 回滚路径。

当前任何回滚必须先按第 2 节证明 enrollment gateway 不可旁路，并通过 `scripts/check-enrollment-gated-v2-rollback.mjs`。关闭 controller、旧裸 v2 或 TOFU 均不能替代此门禁；不能因健康接口成功宣称安全回滚合格。

当次记录包含服务 active、公开 healthz 成功、存量 v2 用户重连、故意不完整 controller claim 返回 invalid_message，以及一次健康误判触发自动回滚。上述是历史观测，不是当前部署、完整配对或发布验收。当前生产脚本执行最多 30 次健康重试，每次请求有超时；这不是总计 30 秒的截止时间或启动性能保证。

仍需真实双 Windows 21 条旅程、三面 UI smoke、端点清单/registry 迁移与关闭 TOFU；未取得证据不得发布。历史设置页 profile 切换尚未实现的记录不作为当前功能范围，当前源码及端点配置才是实际依据。
