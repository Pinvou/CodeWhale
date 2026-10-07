# Windows 双端远程控制：发布、灰度与回滚

> 对应任务：DEV-25。需求基线：[`Windows双端远程控制需求文档.md`](./Windows双端远程控制需求文档.md) 第 14～20 节。
>
> 适用范围：公网 Relay（`remote-control-relay/`）+ Windows 客户端（`pinvou3-app/`）。

## 1. 发布前置门禁

以下门禁全部通过才可以进入灰度；任一条失败即阻断，不允许用 mock 结果替代真实证据。

| 门禁 | 命令 / 证据 | 说明 |
|---|---|---|
| Relay 全量 | `npm --prefix remote-control-relay test` | 含 enrollment registry、迁移、发码、槽位、握手、liveness、resume、telemetry |
| 客户端全量 | `cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml remote_control:: -- --test-threads=1` | A/B 状态机、任务台账、传输、审批、模型包、journal |
| 前端与桥 | `npm --prefix pinvou3-app test`、`npm --prefix pinvou3-app run build:ui`、`npm --prefix pinvou3-app run test:webui` | WebUI v2 行为与旧 fixture 不回归 |
| 架构守卫 | `python scripts/architecture-guard.py` | 依赖方向与平台边界 |
| 发布门槛清单 | `node --test remote-control-relay/test/controller-release-gates.test.js` | 本文件的必备章节、回滚约束与残余风险 |
| 泄露扫描 | 下述三个真实 Rust 用例、`node --test remote-control-relay/test/controller-sensitive-scan.test.js remote-control-relay/test/controller-telemetry.test.js` | 真实错误投影、Debug 脱敏、受控枚举与哨兵扫描；零匹配视为失败 |

### 1.1 泄露扫描的非空执行证据

已删除的 Rust `controller::telemetry` 不再是验证入口。以下每条必须实际报告 `1 passed; 0 failed`，不能仅用 cargo 退出码 0 或 `0 tests` 作为通过证据：

```bash
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::relay_runtime::tests::log_allowlist_rejects_identifier_shaped_values_and_secrets -- --exact --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::relay_runtime::tests::real_remote_failure_projections_do_not_persist_any_sensitive_category -- --exact --test-threads=1
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml features::remote_control::controller::resume::tests::resume_debug_redacts_current_and_retired_material -- --exact --test-threads=1
```

发布门槛测试校验这些入口与现有 Rust 源码一致；这项静态合同不能代替上述实际执行，也不能代替真实双机证据。

## 2. 灰度步骤

1. **Registry 覆盖率**：先统计已迁移 endpoint 比例（`scripts/enrollment-migrate.js` 的 dry-run 输出）。未达到 100% 之前不开启公网强制门禁。
2. **打开桌面控制能力开关**：只打开 `desktop_remote_control` 能力，不动 enrollment registry、pairing tombstone 与 digest keyring。
3. **自托管升级说明**：自托管 Relay 必须先升级到 registry-aware 版本，或在所有入口前挂 enrollment gateway（无旁路）。
4. **容量与限流**：确认连接码领取、claim、握手速率限制与告警阈值生效；观察爆破告警与容量水位。
5. **故障注入**：单边网络隔离、Relay 重启、A/B 单边断线，确认 30 秒预算、resume 与终态清理符合需求。

## 3. 真实双机 E2E 旅程

必须使用两台隔离 Windows 设备与生产等价 TLS/网关环境，逐条执行需求 18.3 的 21 个旅程；其中以下条目最容易假通过，需要保留可复现 trace（不含敏感正文）：

- 旅程 9～10、19、21：断线恢复、30 秒预算、RTT/attestation 余量（24000ms / 19000ms）。
- 旅程 11～12：第二个 A、Web/controller 交叉接入、Web-to-Web 既有接管。
- 旅程 13：A 决定 / 超时 / B stop 竞态的唯一终态，B 侧不得出现 Allow。
- 旅程 16：刷新 generation 与旧注册/握手迟到回包丢弃。
- 旅程 20：digest key 轮换后的碰撞判定，以及「真实旧 Relay + gateway」的安全回滚与裸 v2 回滚拒绝。

## 4. 监控与阈值

| 指标 | 用途 | 建议阈值 |
|---|---|---|
| `claim|busy|slot_conflict` 速率 | 设备被争抢/爆破 | 突增 5 分钟即告警 |
| `handshake|rejected|*` 比例 | 握手/收据异常 | 连续 10 分钟 > 5% |
| `liveness|timeout|watchdog_expired` | 30 秒预算触发 | 任一出现即排查 |
| `resume|rejected|resume_failed` | 恢复失败 | 10 分钟 > 2% |
| `task|rejected|task_conflict` | 任务幂等冲突 | 出现即确认是否 A 重试 |
| `attachment|rejected|transfer_budget` | 传输预算 | 持续出现需复核客户端行为 |

所有指标只允许 DEV-24 枚举内的聚合计数，禁止携带连接码、endpoint/lease/token、IP、设备名、prompt/回复、文件名与路径。

## 5. 回滚

- 灰度关闭只禁用 `desktop_remote_control` 能力：不删除 registry、不删除 pairing tombstone、不回退 state schema。
- 最低回滚版本必须是 registry-aware；否则必须保证所有入口都位于同一 registry gateway 之后且不可旁路。
- 裸 v2 回滚（绕过 enrollment gateway 的旧 Relay）一律拒绝；发布前用 `enrollment-gateway-preflight` 预检全入口。
- 回滚演练需记录：待迁移 endpoint 数、gateway 预检结果、旧 Web 分享链接可用性、桌面能力关闭后的行为。

## 6. 残余风险与声明

- 信任模型以「受信 Relay」为前提：本功能不声称抵御恶意 Relay 的主动 MITM；该风险在发布说明中保留并明示。
- 连接码是 5 分钟短期凭证，不承诺 7 位字符串永久作废；UI 文案不得使用「永久作废」表述。
- 首版仅 Windows 声明能力；macOS/Linux 返回 `unsupported` 并隐藏入口。
