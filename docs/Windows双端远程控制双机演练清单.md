# Windows 双端远程控制：双机演练清单

> 对应需求第 18.3 节。旅程以 `RC-E2E-001`～`RC-E2E-027` 为稳定标识；序号只用于阅读，发布证据和自动化不得依赖文档行号或自然序号。
>
> 机器可读目录：`remote-control-relay/docs/schemas/remote-control-journeys.v1.json`；证据 schema：`remote-control-relay/docs/schemas/remote-control-release-evidence.v1.schema.json`。

## 0. 前置条件

| 项 | 必须满足 |
|---|---|
| 拓扑 | 两台身份不同、可独立隔离网络的 Windows 设备 A/B；生产等价 `wss://` Relay、TLS 终止及无旁路 enrollment gateway |
| 软件 | A/B 使用同一已签名安装包；记录版本、SHA-256、Authenticode 证书指纹和匿名机器身份摘要；受保护构建 provenance/SBOM 必须把安装包绑定当前 commit |
| Enrollment | 官方环境完成既有 endpoint 认领，自托管完成 dry-run/批量 allowlist 或 re-enroll；覆盖率为 100%；生产 `official`/`self_hosted` 且 TOFU=false |
| 回滚 | 提供真实旧 Relay 二进制；只允许在 registry-aware gateway 后、loopback 监听；裸 v2 必须被预检拒绝 |
| 功能开关 | Relay、A、B 都使用 `PINVOU_DESKTOP_CONTROLLER_ENABLED`；开时必须精确为 `1`，关时入口隐藏、底层拒绝、Relay 拒绝，registry/tombstone 保留 |
| 自动化 | 使用 `scripts/run-windows-remote-control-e2e.ps1`；驱动器必须是有效签名的真实应用驱动，不接受 mock Relay、假状态或手填 pass |
| UI | 已启动的真实 Tauri WebView2 开启仅 loopback CDP，通过 `npm --prefix pinvou3-app run test:remote-control-ui -- --cdp-url ... --output ...` 执行真实渲染/交互冒烟 |
| 密钥 | Relay 侧 `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `..._PRIVATE_KEY_B64` 与两端客户端 pin 的 `..._PUBLIC_KEY_B64` 是同一对密钥；两端都设 `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID`（缺失即 `relay_trust_unconfigured` 失败关闭） |
| 私钥纪律 | 私钥只存在于 Relay 部署环境；不得写入仓库、日志、工单或 trace |
| 记录目录 | 每台机器一个 trace 目录（建议 `%LOCALAPPDATA%\Pinvou3\remote-control-drill\<日期>\<角色>\`），只放脱敏 JSON Lines |

## 1. 证据规则

- 每次演练生成一个 `manifest.json`，严格通过 `validate-controller-release-evidence.mjs`；必须绑定 commit、A/B 安装包哈希与签名、受保护构建 provenance/SBOM、Relay 版本/配置指纹、匿名机器身份摘要、UTC 时间和单调时间。首版只支持受保护远程签名服务与 Ed25519；orchestrator、A、B 各有 provider identity、公钥、互异 role/tenant policy 和外部 provider receipt，artifact 必须按 `machine_role` 使用对应 key 签名。生产拒绝 embedded/file private key、测试身份和不受信公钥。
- 27 条旅程必须全部为 `pass`；`blocked`、缺失、重复、乱序、旧 commit、少于 100% registry 覆盖率或缺少文件哈希均失败关闭。
- trace 只允许状态枚举、匿名 SHA-256 摘要、单调时间、序列号、计数、RTT 和固定错误码。禁止连接码、令牌、密钥、Cookie、endpoint/lease/connection 原值、IP/主机名、设备名、prompt/回复、文件名/路径和业务正文。
- 证据目录中的每个 artifact 都必须在 manifest 中登记相对路径、类型和 SHA-256；validator 会重新读取并校验，截图或口头说明不能替代结构化 trace。
- qualification workflow 运行在 GitHub 受保护 environment 的受控 self-hosted runner 上；runner OS、同一 SID 进程、受保护 secrets/vars 以及预配置 Git/GH/Node 绝对路径属于可信执行基座。编排器使用这些固定路径，不从临时 `PATH` 猜测工具；不再为管理员、同 SID 恶意常驻进程、运行中 junction/TOCTOU 替换或 PID 复用自建第二套可信启动链。
- 对不可信边界仍须失败关闭：GitHub API 必须核对 build workflow、run、head/ref、repository、conclusion 与 artifact digest；installer、SBOM、driver/provider 及证据签名必须绑定当前 commit。JSON Schema 使用由 Ajv 生成、版本化且可重现核对的独立 validator；启用发布门禁与 PR `release-contract` 安装锁定的 Relay 测试依赖时，`npm ci` 必须使用 `--ignore-scripts`；生产证据编排不安装 Relay 依赖。`always()` 中任一清理失败必须告警并阻断。
- 旧 Relay driver 启动后必须以 `run_nonce → PID` 写入 durable registry；编排器独立复核实际进程路径和 SHA-256、loopback listener owner、公网 gateway 命中计数与裸 v2 拒绝。清理不依赖启动响应是否成功返回 PID，而是按 nonce 查询全部 owned PID，停止并确认进程退出、registry `active=[]`，随后撤销 route 并验证完整 gateway 状态恢复。脱敏 route spec 的摘要必须在配置、探测和证据中一致。

## 2. 27 条稳定旅程

| 稳定 ID | 操作与通过条件 | 关键证据 |
|---|---|---|
| `RC-E2E-001` | enrolled B 开启，被 Relay 注册并显示 7 位短期码 | 状态序列、码存在但不记值、TTL |
| `RC-E2E-002` | A 配对并进入统一 ChatView | 双端握手状态、generation 摘要 |
| `RC-E2E-003` | 使用 B 模型完成普通任务 | admitted/running/succeeded、journal 序列 |
| `RC-E2E-004` | B 在同一权威 Session 输入下一条消息；A/B 同步且无第二普通 Session | Session 摘要、双端事件序列、执行 owner |
| `RC-E2E-005` | A 临时模型完成任务并按 task 清理凭证 | package 摘要、模型来源、清理结果 |
| `RC-E2E-006` | 触发工具权限并由 A Allow | request/decision/dispatch 唯一序列 |
| `RC-E2E-007` | 第二项权限由 A Deny | 唯一拒绝终态、工具零调用 |
| `RC-E2E-008` | driver 先保持 attachment pending，A 通过 CDP 选择并提交附件，B 收到后 driver 才 finalize | reserve/upload/commit、预算、摘要、UI 动作与 B 接收关联 |
| `RC-E2E-009` | B 生成成果，A 预览和下载 | pending 不可见、任务成功后一次发布、哈希一致 |
| `RC-E2E-010` | 任务中断网，A/B 独立 token 恢复；pending-liveness 禁止业务 | resume 阶段、零业务计数、首个 B liveness |
| `RC-E2E-011` | B 进入网络黑洞并持续制造重连流量 | 单调 deadline `<= 30000 - 实测 RTT - 1000`，终态不晚于该 deadline，重连不得延长、任务/审批/凭证清理 |
| `RC-E2E-012` | 第二个 A claim 同一码 | controller_busy，原会话不受影响 |
| `RC-E2E-013` | Web/controller 双向交叉接入及 Web-to-Web 接管 | 单赢家、v2 envelope/close code、jitter 分布 |
| `RC-E2E-014` | A 决定、超时、B stop 并发 | 唯一终态；B 无 Allow；迟到/跨 epoch 决定拒绝 |
| `RC-E2E-015` | B 立即断开 | 凭证、审批、任务、附件/成果私有副本全部清理 |
| `RC-E2E-016` | Relay 重启 | 旧租约/密钥/token 不恢复，B 获取新码 |
| `RC-E2E-017` | B refresh generation 并注入迟到注册/握手回包 | 先撤旧后显新、迟到回包丢弃 |
| `RC-E2E-018` | A 关窗口后任务继续；显式断开才进入收尾 | 两条状态序列与 30 秒内终态 |
| `RC-E2E-019` | B 重启并模拟同字符串新 pairing | 旧 pairing/tombstone 不关联新设备或旧 lease |
| `RC-E2E-020` | 分别隔离 A、B；恢复后新 sequence 推进，同 sequence 重放不推进 | A/B 心跳隔离、deadline 序列、重放拒绝 |
| `RC-E2E-021` | 轮换 digest key，构造只命中旧 key tombstone 的碰撞；真实旧 Relay 回滚 | 多 key 碰撞、gateway 全入口、裸 v2 拒绝 |
| `RC-E2E-022` | 直接 RTT=5000ms；attestation age=5000ms 且 RTT=5000ms | 候选剩余 24000/19000ms，总存活 ≤30000ms |
| `RC-E2E-023` | 同包创建普通、ACP、RemoteProxy 会话并切换/关闭/恢复 | 三路分别 EnginePool/AcpPool/RemoteControl，无串线 |
| `RC-E2E-024` | 调用旧正文入口、旧 desktop Command/Bridge、旧历史存储 | 注册表与真实调用均不可用，无双写/第二数据源 |
| `RC-E2E-025` | A 断线并结束恢复窗口后打开 RemoteProxy | 历史可读、离线标识；发送/取消/审批/上传零网络帧 |
| `RC-E2E-026` | persist/dispatch/ACK 丢失与 A 强杀重启 | 同 ID 同指纹幂等、异参拒绝、一次副作用；重启后 outcome_unknown |
| `RC-E2E-027` | 全新目录及含废弃 RemoteHistoryStore 文件的升级目录分别启动 | 只注册 SessionStore.RemoteProxy；旧文件不读/不迁移/不改写 |

## 3. 组合门禁

以下不是单项替代关系，必须在同一发布证据中同时通过：

1. 开关关闭：A/B UI 入口隐藏，所有 remote Tauri 网络入口返回 `desktop_remote_control_unsupported`，Relay v1 路由拒绝；registry 与 pairing tombstone 哈希不变。
2. 开关开启：A/B/Relay 都为精确值 `1`，随后执行全部 27 条旅程。
3. 容量/限流：两个 A、两个 B、Web/controller 竞争、连接/消息/字节高水位与廉价前置拒绝均有实际负载 trace。
4. 安全/运维：敏感面扫描、DEV-24 监控阈值、registry 覆盖率、旧 Relay gateway 回滚预检和裸 v2 拒绝都为 pass。
5. UI：B 面板、A 连接页、统一远程 ChatView 均由真实 Tauri WebView2 驱动，覆盖键盘、焦点、`aria-live`、错误恢复、审批、附件和成果。

## 4. 执行入口

```powershell
.\scripts\run-windows-remote-control-e2e.ps1 `
  -AComputerName <A> -BComputerName <B> -CredentialFile <runner-bound-dpapi.clixml> `
  -InstallerPath <signed-installer.exe> -JourneyDriverPath <signed-real-driver.exe> `
  -LegacyRelayBinaryPath <real-versioned-legacy-relay.exe> `
  -EndpointInventoryPath <protected-inventory.json> -GatewayConfigPath <gateway-config.json> `
  -InstallerAttestationBundlePath <github-attestation.sigstore.jsonl> `
  -InstallerSbomPath <installer.spdx.json> `
  -InstallerSbomAttestationBundlePath <sbom-attestation.sigstore.jsonl> `
  -InstallerBuildRepository <owner/repo> `
  -InstallerBuildWorkflow .github/workflows/windows-remote-control-build-evidence.yml `
  -InstallerBuildRunId <protected-build-run-id> `
  -TrustedSignerThumbprint <fixed-certificate-thumbprint> `
  -TrustedSignerPublisher <exact-certificate-subject> `
  -TrustedNodePath <protected-absolute-node.exe> -TrustedNodeSha256 <protected-node-sha256> `
  -TrustedNodeSignerThumbprint <fixed-node-certificate-thumbprint> `
  -TrustedNodeSignerPublisher <exact-node-certificate-subject> `
  -TrustedGhPath <protected-absolute-gh.exe> `
  -ExpectedSourceCommit <current-40-hex-head-sha> -RepositoryRoot <absolute-checkout-directory> `
  -RelayUrl wss://<production-equivalent>/pinvou3/remote/ws `
  -DisabledRelayUrl wss://<feature-off-production-equivalent>/pinvou3/remote/ws `
  -LegacyGatewayUrl wss://<registry-aware-gateway>/pinvou3/remote/ws `
  -EnrollmentDeployment self_hosted `
  -AttestationProvider remote_signing_service -AttestationKeyId <protected-remote-key-id> `
  -AttestationAclSha256 <protected-acl-attestation-sha256> `
  -AttestationProviderMeasurementSha256 <allowlisted-provider-measurement-sha256> `
  -OrchestratorRolePolicySha256 <protected-orchestrator-role-policy-sha256> `
  -ARolePolicySha256 <protected-A-role-policy-sha256> `
  -BRolePolicySha256 <protected-B-role-policy-sha256> `
  -ProviderVerifierPath <signed-independent-provider-verifier.exe> `
  -AttestationProviderVendor <allowlisted-vendor> `
  -TrustedProviderRootCertificateSha256 <allowlisted-root-certificate-sha256> `
  -TrustedProviderVerifierCertificateSha256 <allowlisted-leaf-certificate-sha256> `
  -EvidenceDirectory <new-empty-directory>
```

脚本要求 `-ExpectedSourceCommit` 与当前 `git HEAD` 一致，CI 还核对 `GITHUB_SHA`；受保护 Node/GH 路径和信任参数不可省略。当前服务端仅支持生产 `self_hosted` 且关闭 TOFU，`official` live claim 尚未接线，预检及启动明确失败关闭。A/B 必须是不同机器身份，驱动器和安装包必须由参数指定的固定发布证书签名；两台机器都会独立启动应用和受信驱动，任一缺失即中止，不生成 PASS manifest。

旧 Relay 输入是受保护配置而不是说明性附件：inventory 与 gateway config 都采用 `additionalProperties=false` 的严格结构，各有且仅有一个启用项，并以同一 `route_id`、`target=legacy_relay`、`no_bypass=true` 和目标公网 URL 摘要对应。编排器只生成经敏感扫描的最小 route spec 传给 A，绝不复制完整受保护配置；driver 激活的 owned route 必须绑定 run nonce、route ID、两份配置摘要和原状态摘要。无论后续成功或失败，`finally` 都必须撤销公网 route、恢复原状态并主动复核，清理失败阻断发布。

安装包由受保护的 `windows-remote-control-build-evidence` workflow 从当前 commit 构建。`gh attestation download` 在空目录生成的唯一 `sha256-*.jsonl` 是版本化证据，不能把 stdout 当 bundle；安装包和 SBOM 分别 attestation。编排器与 release gate 对二者执行 `gh attestation verify`，核对 repository、workflow、run、head SHA、原始 subject 名称与摘要。SPDX 2.3 必须从最终 NSIS staging manifest 生成，逐文件覆盖 package-lock、Cargo.lock、公开/私有 runtime 映射、最终 resources、生成配置/hooks、主 binary 和 installer，并包含完整 `DEPENDS_ON`/`CONTAINS` 关系。Windows 发布复用通过门禁的同一安装包字节；手填 hash、自报 `verified=true` 或重新构建另一个包均不能替代该证明。

A/B 的远端 helper 必须落入本次随机 run nonce 专属目录，拒绝已有目录、普通路径越界和任一重解析点。复制前后核对安装包、driver、provider verifier、UI smoke、process guard、orchestrator helper 与回滚输入摘要，Node 使用受保护环境配置的固定绝对路径。清理只删除 owner marker 与本次 nonce 一致的目录；各远端清理动作独立执行并聚合结果，任一失败均阻断。

provider 证明由独立、签名且受保护的 verifier 在 run nonce 生成后出具，必须绑定 provider/key ID、角色策略、ACL、measurement、driver SHA 和新鲜时间；validator 验证 vendor 证书链、根/叶证书 allowlist 与证明签名。driver 只能使用受保护远程签名服务的不可导出 key handle 做 Ed25519 artifact 签名，不得自行生成 provider 证明；没有真实远程签名服务证明时结果为 BLOCKED。

正式证据由受保护的 `windows-remote-control-evidence` workflow 运行并上传；发布 workflow 会通过 GitHub API 复核证据 run 的 `head_sha`、workflow 路径/ID、成功结论以及选中 artifact 的 GitHub SHA-256 digest，不信任 manifest 自报的 run 身份。

当前工作站没有两台隔离 Windows、生产等价公网 Relay、签名安装包和真实旧 Relay 二进制时，预期结果是门禁失败/blocked；不得创建替代 manifest 或把本地单元测试写成双机 pass。
