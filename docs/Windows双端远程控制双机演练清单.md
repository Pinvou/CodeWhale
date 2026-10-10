# Windows 双端远程控制：双机演练清单

> 用途：执行 `Windows双端远程控制发布与回滚.md` 第 3 节要求的真实双机 E2E（需求文档 18.3 的 21 条旅程），并保留可复现、已脱敏的证据。
> 关联：发布门禁见发布文档第 1、7 节；单机自测（无需部署）见 `scripts/remote-control-local-selftest.ps1`；任务进度见 `Windows双端远程控制端到端补齐任务.md` 第 11 节。

## 0. 前置条件

| 项 | 要求 |
|---|---|
| 部署 | 生产等价环境的 Relay（TLS 终止 + 网关）已就绪；registry 覆盖率 100%（仓库根目录下 `remote-control-relay/scripts/enrollment-migrate.js` dry-run 输出为证据）；enrollment gateway 已挂在所有入口前且不可旁路 |
| 密钥 | Relay 侧 `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `..._PRIVATE_KEY_B64` 与两端客户端 pin 的 `..._PUBLIC_KEY_B64` 是同一对密钥；两端都设 `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID`（缺失即 `relay_trust_unconfigured` 失败关闭） |
| 私钥纪律 | 私钥只存在于 Relay 部署环境；不得写入仓库、日志、工单或 trace |
| 机器 | 两台隔离 Windows 设备（A 主控端 / B 被控端），可独立断网；建议都开启系统时钟同步 |
| 客户端版本 | 两端同一版本（冻结协议 v1），并记录版本号 |
| 首版能力开关 | 临时模型、代码模式、附件、成果导出、工具审批按首版隐藏；**第 4～8 条旅程因此标记为「首版隐藏」，不得记为通过** |
| 记录目录 | 每台机器一个 trace 目录（建议 `%LOCALAPPDATA%\Pinvou3\remote-control-drill\<日期>\<角色>\`），只放脱敏 JSON Lines |

## 1. trace 记录格式（JSON Lines，一条观测一行）

允许字段（**只允许这些**）：

```json
{"ts":"2026-09-19T10:12:33+08:00","role":"A|B|relay","step":9,"phase":"reconnecting",
 "result":"pass|fail|blocked","error_code":"endpoint_offline|null","generation_fp":"ab12cd34ef56",
 "lease_fp":"ab12cd34ef56","heartbeat_seq_a":null,"heartbeat_seq_b":12,"liveness_epoch_fp":"9f8e7d6c5b4a",
 "rtt_ms":null,"observed_age_ms":null,"remaining_ttl_ms":null,"task_status":"running",
 "journal_epoch":"epoch-3","event_sequence":7,"notes":""}
```

字段说明：

- `generation_fp` / `lease_fp` / `liveness_epoch_fp`：对原值取 SHA-256 后的前 12 个十六进制字符，**永不记录原值**。
- `phase`：`disabled|reserving|waiting_connection|safety_handshake|connected|reconnecting|refreshing|stopped|error`（B 侧）或 A 侧 `idle|connecting|safety_handshake|connected|reconnecting|disconnected|error`。
- `error_code`：只允许冻结闭集（`pairing_code_invalid|controller_busy|version_incompatible|endpoint_offline|invalid_message|unsupported_capability|permission_already_decided|permission_expired|result_unknown` 等）。
- `notes`：只能是判定说明；不得含正文、路径、设备名。

禁止出现在 trace 中的内容：连接码、resume/role token、`endpoint_id` / `controller_lease_id` / `connection_id` 原值、IP 或主机名、设备显示名、prompt 与模型回复、文件名与路径、Cookie 与任何凭据。

观测来源：A 端状态/会话/任务/journal 事件，B 端状态事件，Relay 访问日志与审计事件（`controller_*`），以及两端的窗口操作时间点。

## 2. 21 条旅程：操作、预期观测点与 trace 字段

> 「首版隐藏」条目按保守默认值执行：确认入口不出现、能力未协商，然后记为 `blocked`，**不得记为通过**。

| # | 前置 | 操作 | 预期观测点 | 需保留的 trace 字段 |
|---:|---|---|---|---|
| 1 | B 已 enrolled，远程控制未开启 | B 开启远程控制 | 状态 `disabled → reserving → waiting_connection`；UI 出现 7 位连接码与倒计时；无错误码 | `phase` 序列、`code_present=true`（不记值）、`remaining_ttl_ms`、耗时 |
| 2 | 完成 #1 | A 输入连接码连接 | A：`connecting → safety_handshake → connected`；B：`waiting_connection → safety_handshake → connected`；握手失败时错误码必须在闭集内 | `phase` 序列（两端）、握手耗时、`error_code`、`generation_fp` |
| 3 | 完成 #2 | A 发一句普通文本任务（B 模型） | 任务 `admitted/running → succeeded`；A 看到 B 模型回复正文；B 侧 journal 有 `user_message`/`assistant_delta`/`terminal` | `task_status` 序列、`journal_epoch`+`event_sequence`、`task_fp`、端到端时间 |
| 4 | 完成 #2 | 尝试使用 A 临时模型 | **首版隐藏**：UI 无入口、能力未协商；不出现半通路 | `result=blocked`、能力快照中缺少该 family |
| 5 | 完成 #2 | 触发工具权限并由 A 允许 | **首版隐藏**：远程会话禁用工具；B 面板无 Allow 入口 | `result=blocked`、能力快照中缺少工具 family |
| 6 | 完成 #2 | 触发第二项权限并由 A 拒绝 | **首版隐藏**（同 #5） | `result=blocked` |
| 7 | 完成 #2 | 上传附件并由 B 处理 | **首版隐藏**：附件入口不可见、传输未协商 | `result=blocked` |
| 8 | 完成 #2 | B 生成成果，A 预览并下载 | **首版隐藏**：成果入口不可见 | `result=blocked` |
| 9 | 任务运行中 | 断网后恢复，双端各自 resume | `reconnecting → resume 序列（prepare/provisional/confirm/activation_ack/post_activation_liveness/ready_ack/resumed）→ connected`；pending-liveness 期间业务帧与心跳计数为 0；首个 B liveness 被两端确认后才恢复业务 | 两端 `phase` 序列、`activation_fp`、pending 期间业务帧计数、恢复耗时（<30s） |
| 10 | 任务运行中 | 隔离 B 网络 >30s 并持续制造重连流量 | B 的本地 watchdog 截止时间不被延长；任务被取消/中断、待审批被拒绝、临时模型与凭证被清理；旧令牌无法恢复（`resume_token_invalid` / 恢复失败关闭） | watchdog 截止时间序列、`task_status=interrupted/result_unknown`、清理 scope、旧令牌尝试结果、`error_code` |
| 11 | 完成 #2 | 第二个 A 尝试用同一连接码接入 | 第二个 A 收到 `controller_busy`（闭集错误码）；原 A 会话不受影响、任务继续 | `error_code`、两个 A 的 `generation_fp`、原会话任务状态 |
| 12 | WebUI 已连接 / 桌面已连接 | 交叉接入：WebUI 在线时桌面 claim；桌面在线时 WebUI join；另验证 Web-to-Web 既有接管 | 桌面 claim 在 Web 占用时被拒（`controller_busy`/`endpoint_not_found`，不顶替）；Web join 得到 v2 envelope `{"v":2,"type":"error","code":"endpoint_not_found",...}` 且关闭码 1008；Web-to-Web 接管行为与旧版本一致 | 两个方向的 envelope 原文（不含敏感）、close code、既有接管结果 |
| 13 | 完成 #2 | B 查看权限视图；制造 A 决定 / A 超时 / B stop 竞态 | B 侧无 Allow 入口；三路竞态只产生唯一终态，且无重复终态投影 | 竞态注入顺序、终态、重复终态计数=0、`error_code` |
| 14 | 任务与待审批存在 | B 立即断开 | 所有凭证与待审批立即失效；旧 lease/generation 帧不再产生任何投影；B 投影 `stopped` | 清理 scope、待审批归零、旧帧丢弃计数、`phase` |
| 15 | 完成 #2 | 重启 Relay | 旧连接不恢复；B 自动申请并由 Relay 生成新码并可再次配对 | `relay_epoch_fp` 变化、旧 generation 帧丢弃、新码展示时间 |
| 16 | 完成 #2 | B 刷新连接码；注入旧注册/握手的迟到回包 | 旧 generation 完整撤销后才显示新码；迟到回包被丢弃，不影响新代际 | 刷新前后的 `generation_fp`、撤销完成时间 vs 新码展示时间、迟到回包丢弃计数 |
| 17 | 任务运行中 | A 仅关闭窗口；随后显式断开 | 关窗口后任务继续、连接保持；显式断开后进入 30 秒收尾并产生明确终态 | 两条路径的时间点与 `phase`、终态、收尾耗时 |
| 18 | 完成 #2 | B 关闭再开启；再模拟「同字符串新 pairing」 | 旧 pairing 与旧连接不可恢复；新 pairing 不能关联到旧设备/旧租约 | pairing tombstone 命中、新旧 `lease_fp` 差异、`error_code` |
| 19 | 完成 #2 | 分别隔离 A 与 B，再恢复直连 | A 断 B 活：B 心跳不延长任务存活；B 断 A 活：Relay 不把 A 心跳当作 B 在线；恢复后新 A sequence 能推进 deadline，同 sequence 重放不能推进 | `heartbeat_seq_a` 与 deadline 变化、重放拒绝计数、两端 `phase` |
| 20 | 具备 digest key 轮换与 gateway 环境 | 轮换 digest key；构造只在旧 key tombstone 中命中的码；演练旧 Relay + gateway 回滚与裸 v2 回滚 | 新 key 下仍判为碰撞；旧 Relay 置于 gateway 后可安全工作；裸 v2 回滚被拒绝 | key id、碰撞判定结果、gateway 预检输出、回滚拒绝错误码 |
| 21 | 完成 #2 | 注入直接 E2E liveness RTT=5000ms，以及 attestation `observed_age_ms=5000`、请求 RTT=5000ms | 候选剩余分别为 24000ms / 19000ms；从对应 A liveness 观察点断言总存活 ≤30000ms | `rtt_ms`、`observed_age_ms`、`remaining_ttl_ms`、总存活时长 |

## 3. 判定准则

1. **任一 blocker 失败即阻断灰度**；`blocked` 不等于 `pass`，必须写明阻塞原因与后续排期。
2. 最容易假通过的条目（必须逐字段核对 trace）：#9、#10、#19、#21（预算与恢复）、#11～#13（竞态与唯一终态）、#16（旧代际撤销顺序）、#20（轮换与回滚）。
3. 两端 `phase` 序列与有界错误码必须来自真实事件，不接受口头描述或截图替代结构化 trace。
4. 任何一次演练中若出现敏感信息进入日志或 trace，立即停止，按 `CONTRIBUTING.md` 的 Security 条目私密报告；不得在公开 Issue 粘贴敏感材料。

## 4. 结果汇总模板

```text
演练日期 / 版本 / 设备指纹（不记主机名）：
#1 pass  #2 pass  #3 pass  #4 blocked(首版隐藏)  #5 blocked(首版隐藏)
#6 blocked(首版隐藏)  #7 blocked(首版隐藏)  #8 blocked(首版隐藏)
#9 ...  #21 ...
汇总：pass N / fail M / blocked K；trace 目录：<路径>；是否允许进入灰度：是/否
```

## 5. 与单机自测的分工

| 层次 | 是否需要部署 | 覆盖 | 入口 |
|---|---|---|---|
| 自动化门禁 | 不需要 | 状态机、协议契约、加密通道、UI 契约、架构边界 | `cargo test remote_control::`、`node --test`、`python scripts/architecture-guard.py` |
| 单机自测 | 需要本地起 Relay（非部署） | 真实 WebSocket 往返、真实加密通道、连接码/任务/断开/可选恢复 | `scripts/remote-control-local-selftest.ps1` + 两端本地进程 |
| 双机演练 | 需要生产等价部署 | 本文件第 2 节的 21 条旅程（含网络隔离、Relay 重启、交叉接入、轮换与回滚） | 本文件 |
