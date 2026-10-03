# Windows 双端远程控制：发布、灰度与回滚

> 对应 DEV-25，需求基线为需求文档第 14～20 节。所有发布判断使用稳定旅程 ID，不使用会随文档编辑漂移的自然序号。

## 1. 发布前置门禁

只有以下门禁全部通过且证据属于待发布 commit，才允许把 `PINVOU_DESKTOP_CONTROLLER_ENABLED` 设为精确值 `1`：

| 门禁 | 可执行入口 |
|---|---|
| Relay 全量、容量与限流 | `npm --prefix remote-control-relay test` |
| 客户端全量与构建 | `npm --prefix pinvou3-app test`、`npm --prefix pinvou3-app run build:ui`、`npm --prefix pinvou3-app run test:webui` |
| Rust 远控 | `cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1` |
| 架构 | `python scripts/architecture-guard.py` |
| 敏感面和 DEV-24 遥测 | `node --test remote-control-relay/test/controller-sensitive-scan.test.js remote-control-relay/test/controller-telemetry.test.js` |
| 双 Windows 27 旅程 | `scripts/run-windows-remote-control-e2e.ps1` |
| 真实 Tauri UI | `npm --prefix pinvou3-app run test:remote-control-ui -- --cdp-url <loopback-cdp> --output <new-file>` |
| 证据和最终发布门禁 | `npm --prefix remote-control-relay run release:gate` |

`release:gate` 在开关开启时要求 `PINVOU_REMOTE_RELEASE_EVIDENCE` 与 `PINVOU_RELEASE_COMMIT_SHA`，重新校验 artifact 哈希、27 条旅程、敏感扫描、registry 覆盖率和旧 Relay 回滚预检；缺任何一项都失败。开关关闭时仍验证值只能为空或 `0`，不会删除 registry、pairing tombstone 或回退 state schema。

开关关闭的普通发布包不包含已启用的桌面远控能力；该组合通过不代表桌面远控发布资格通过。公开发布本功能必须在 A/B/Relay 三端精确设为 `1`，并取得全部开启门禁的真实证据，不能以关闭开关的 CI 结果替代。

开关开启的 CI 门禁必须在能读取受保护生产状态的发布 runner 上执行（通过 `PINVOU_REMOTE_RELEASE_RUNNER` 选择），并注入启用/关闭两份受保护环境文件、controller state、旧 endpoint inventory、已部署 gateway 配置、公网入口清单、固定签名证书与受信驱动/安装包哈希。这些生产文件不上传到 evidence artifact；门禁现场重算 inventory/gateway 指纹并与双机证据比对。任一变量、文件或跨 run artifact 不可用时，结果必须为 BLOCKED。

跨 workflow 证据只接受 GitHub API 确认属于当前 commit、固定 workflow ID 与精确 path、当前 ref/repository、成功完成且 digest 与受保护期望值一致的 artifact。首版统一使用受保护远程签名服务与 Ed25519：orchestrator、A、B 分别声明 provider identity、公钥、互异 role/tenant policy 和独立 provider receipt，artifact 必须按 `machine_role` 选择对应 key 验签。provider 证明必须由独立 verifier 产生，绑定本次 nonce、角色/policy、provider/key ID、ACL、measurement、driver SHA 和新鲜时间，并通过 vendor 证书链、根/叶证书 allowlist 与签名校验；driver 不得循环自证。缺少真实 provider 证明时发布资格为 BLOCKED。

远端 UI/process helper 不得从 `PATH` 临时选择 Node。受保护环境提供固定 `node.exe` 路径、SHA-256、Authenticode 发布者和证书指纹；编排器校验后复制到本次 nonce staging，并把版本、摘要、发布者、证书指纹和 artifact 路径写入 manifest provenance。Node artifact 仍由 orchestrator attested key 签名，任一不一致即 BLOCKED。

GitHub protected environment/self-hosted runner、runner OS 与同一 SID 进程、受保护 secrets/vars，以及环境中配置的 Git、GitHub CLI、Node 工具链构成本发布流程的可信执行基座。workflow 从受保护 checkout 直接执行当前 `github.sha` 的编排器和 verifier；安装证明验证器只接受受保护变量给出的 GitHub CLI 绝对路径，不从 `PATH` 回退。此门禁防御不可信网络/Relay、外部下载产物、用户或部署配置、伪造/过期/错 commit 证据、普通路径逃逸/符号链接和敏感信息泄露，并坚持任何失败都不放行。

本威胁模型明确不防御受信 runner 上的管理员、同一 SID 恶意常驻进程、运行中 junction/TOCTOU 替换或 PID 复用攻击，也不为 Git/GH/Node 自建二次可信启动链。普通路径边界仍以 canonical relative path 校验，所有 staging/证据目标拒绝 reparse point，外部下载产物必须通过 GitHub API 的 workflow/run/head/ref/repository/conclusion 与 artifact digest 校验。JSON Schema 由 Ajv 编译为版本化、可重现核对的单文件 validator；清理在 `always()` 中执行，失败告警并阻断。生产证据与发布门禁不安装 Relay 依赖；PR `release-contract` 如需安装 Relay 测试依赖，必须为 `npm ci` 指定 `--ignore-scripts`。

旧 Relay 启动时，受信 driver 必须在返回进程信息前把 `run_nonce → pid` 原子写入 durable registry，使响应丢失时仍能按本次 nonce 清理。编排器核对实际进程的可执行路径、SHA-256、loopback listener 与 owner PID；清理时先查 registry、停止本次 owned 进程、确认进程退出且 registry `active=[]`。脱敏 route spec 必须严格解析、敏感扫描、登记 SHA-256，并由 configure/probe 显式接收该摘要；owned route 在 `finally` 中撤销并复核公网路由消失和原配置恢复。任何一步失败均阻断发布。

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

## 6. Feature flag 关闭组合门禁

1. A/B 平台能力返回 `desktopRemoteControl=false`，设置入口不可见。
2. 即使绕过 UI 调用，A connect 与 B start 等网络入口返回 `desktop_remote_control_unsupported`，不打开 socket、不创建租约。
3. Relay 不创建 controller runtime，v1 controller 子协议/消息被拒绝，既有 Web v2 行为保持。
4. 关闭前后 registry、pairing tombstone 与 digest keyring 指纹一致。
5. 再次开启只恢复新配对入口，不恢复旧连接、旧 lease、旧 token 或旧 generation。

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

## 8. 生产 controller lane 历史启用记录（2026-09-19，当时采用安全降级）

> 以下为当时环境和操作的历史快照，不是当前生产部署授权或验收证据。A18 已移除生产预检的 development + TOFU 降级门禁：当前必须 `NODE_ENV=production`、`PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=self_hosted`、`PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=false`。`official` live claim 尚未接线，预检及启动均明确失败关闭；存量端点清单、registry 迁移和不可旁路 gateway 仍为 [#1428](https://github.com/Pinvou/pinvou3/issues/1428) 的发布阻塞。本次只修复代码与文档，未核验或改变线上状态。

### 8.1 变更内容

1. 生产 Relay（`pinvou-remote-relay.service`，`/opt/pinvou-remote-relay`，端口 8787）升级到含 controller-v1 的版本（原先为 v2-only 旧代码）。
2. 新增 drop-in `/etc/systemd/system/pinvou-remote-relay.service.d/40-controller.conf`，`EnvironmentFile=/etc/pinvou-remote-relay-controller.env`（**0600**，私钥不出主机）。
3. 环境文件要点：`PINVOU_DESKTOP_CONTROLLER_ENABLED=1`、`NODE_ENV=development`、`PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT=development`、`PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU=true`、enrollment/pairing HMAC 各一组、`PINVOU_REMOTE_RELAY_SIGNING_KEY_ID=prod-relay-key-1`、`PINVOU_REMOTE_CONTROLLER_STATE_PATH=/var/lib/pinvou-remote-relay/state/controller-state.json`（目录 0700）、`PINVOU_REMOTE_STATE_PATH=/opt/pinvou-remote-relay/data/relay-state.json`（沿用既有状态，未迁移）。
4. 当前生产部署脚本必须拒绝环境文件符号链接，校验其为 `root:root`、`0600`，并严格验证本节开头的 production + self_hosted + TOFU=false 配置及两条状态路径，不再接纳第 3 项历史降级配置；脚本原子维护 `40-controller.conf`，在本机执行无状态变更的 controller-v1 路由烟测，并在本机与公网健康检查中确认 `desktop_controller_enabled=true`。任一检查失败都必须整体恢复旧版 JS 模块和 systemd drop-in，禁止以 Web v2 正常但桌面控制不可用或新旧模块混装的状态通过发布。

### 8.2 当时为什么启用 TOFU（历史原因，不再作为生产例外）

- 生产状态文件只有 `version` 与 `revoked_endpoints`，**没有端点清单**：存量端点只存在于内存，无法做「registry 覆盖率 100%」的迁移。
- 一旦启用 controller runtime，v2 的 `desktop_endpoint_register` 也会先过 registry（`server.js:886`）；在 `official` 且无 TOFU 的模式下，全部存量端点重连会被 `enrollment_required` 拒绝。
- 因此当时经用户确认，**历史生产配置采用 `development + TOFU`**：未登记客户端可自助注册。这是当时的安全降级记录，不是当前生产例外；正式发布前必须补齐端点清单与 registry 迁移，关闭 TOFU，不能以保留旧配置绕过门禁。

### 8.3 信任根（公钥随包内置，私钥只在部署环境）

| 项 | 值 |
|---|---|
| key id | `prod-relay-key-1` |
| 公钥（原始 32 字节 ed25519，base64） | `nGlRK79H+YdNVhmZOwkI2T83uhiEBqfZeFbMrJ28LQM=` |
| 客户端内置位置 | `manager::PRODUCTION_PROFILE`（提交 `225fa4cd`），用户无需再配置 `PINVOU_REMOTE_RELAY_SIGNING_*` |
| 私钥 | 仅 `/etc/pinvou-remote-relay-controller.env`（0600），不入库、不入日志；轮换时需同步更新内置公钥并随客户端发版 |

### 8.4 备份与回滚

- 全量备份：`/root/pinvou-prod-backups/pinvou-remote-relay-20260919T134907Z.tgz`（代码 + unit + drop-ins，314 MB）。
- 部署时快照：`/opt/pinvou-remote-relay/backups/20260919-224624`（旧代码/旧 web/dist/旧 package 文件，脚本自动写入）。
- **最小回滚（关闭 controller lane，恢复 v2-only 行为）**：
  ```bash
  sudo rm -f /etc/systemd/system/pinvou-remote-relay.service.d/40-controller.conf
  sudo systemctl daemon-reload && sudo systemctl restart pinvou-remote-relay.service
  curl -fsS https://pinvou.com/pinvou3/remote/healthz   # 期望 200
  ```
- **彻底回滚代码**：用 `/opt/pinvou-remote-relay/backups/20260919-224624` 覆盖 `server.js`/`web/dist`/package 文件后重启（或解压全量备份 tarball）。
- 关闭 controller lane 后，客户端配对会失败关闭为 `invalid_message`/`endpoint_offline`，属预期。

### 8.5 验收证据（2026-09-19）

| 检查 | 结果 |
|---|---|
| 服务 | `pinvou-remote-relay.service` active；启动日志 `protocol=v2+controller-v1` |
| 公网 healthz | `https://pinvou.com/pinvou3/remote/healthz` → 200 `{ok:true}` |
| 存量 v2 用户 | 重启后桌面端重连，`desktop_open` 由 3 回升到 8，`web_client_open=0` 与变更前一致，无注册被拒 |
| controller lane（公网） | 从 `wss://pinvou.com/pinvou3/remote/ws` 发 `controller_pairing_claim` → 收到控制器族错误信封 `{"v":1,"type":"error","code":"invalid_message"}` |
| 回滚路径 | 部署过程中真实触发过一次自动回滚（1s 健康检查误判），成功恢复旧版本；之后脚本已改为 30s 有界重试 |

### 8.6 仍未完成（发布门禁）

- 真实双机 21 条旅程、三面 UI 浏览器冒烟（见 7.1）——在取得证据前不得发布。
- 端点清单与 registry 迁移、关闭 TOFU（见 8.2）。
- 设置页「服务端点」下拉（生产/测试切换）尚未实现，当前切换靠 `PINVOU_REMOTE_ENDPOINT_PROFILE` 或 `~/.pinvou3/web-relay.json` 的 `profile` 字段。
