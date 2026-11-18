# chrono 停止维护提示的临时例外

2026-11-17 评估并批准仅对 [RUSTSEC-2026-0543](https://rustsec.org/advisories/RUSTSEC-2026-0543.html)
设置临时例外。该公告将所有 chrono 版本标为 `unmaintained`，没有可升级的修复版本；
本次例外接受维护停止风险，不表示 chrono 没有漏洞，也不豁免其他公告。
RustSec 数据库在 2026-11-17 10:14:35 UTC 的提交
`233155b4025b854c68a4b199c0e32d7fd53291a2` 纳入该公告，解释了同日之前通过、之后失败的审计。

## 生效边界与到期

- 只忽略该公告 ID，不修改 `unmaintained`、`unsound`、`yanked` 或其他审计门禁。
- 知识库、桌面端、CLI 与 controller-crypto-harness 四个 Cargo.lock 必须各自只有一个 chrono 0.4.45，且来自
  crates.io，校验值为 `1aa79e62e7697b8e29b513a68abacf485adcd1fe8284a4316c5ae868e6633327`。
  版本、来源、校验值变化或文件缺失均拒绝，不自动接受新版本。
- **2026-12-17 00:00 UTC（北京时间 08:00）起拒绝检查通过**，无宽限期、命令行日期
  覆盖或自动延期。CI fast-gate 每次运行都检查到期，两个 cargo-deny 消费者也先检查。
- `unused-ignored-advisory = "deny"` 保留；迁移完成后同时移除 ID、守卫、工作流入口和相关回归。

CI 固定的 cargo-deny 0.20.2 不支持 ignore 的原生到期字段；此版本须配合上述守卫。
将来升级 cargo-deny 时可以另行评估原生 expiry 支持；升级不能替代四份锁的身份核对。
CI 的 cargo-deny 实际扫描知识库、桌面端两张依赖图；CLI 与 controller-crypto-harness
在 CI 中仅由守卫核对固定依赖，不能据此声称其独立依赖图已完成公告扫描。
两个审计入口的守卫顺序由 `scripts/tests/test_webpki_roots_exception.py` 跨层回归校验。

本地须从仓库根以失败即停方式执行以下组合（CLI 审计为额外手工验证）：

```sh
python3 scripts/guard-webpki-roots-exception.py &&
python3 scripts/guard-chrono-maintenance-exception.py &&
cargo deny --locked --manifest-path pinvou-knowledge/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources &&
cargo deny --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources &&
cargo deny --locked --manifest-path pinvou-cli/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
```

## 后续迁移验收

迁移由 #2702 跟踪，通过独立 PR 处理直接依赖及上游传递依赖，避免把接口迁移混入审计修复。
只移除本仓直接依赖并不能移除所有 chrono：仍须核对 oauth2、rmcp、serde_with、
uiautomation 及 Arrow/Parquet 等传递使用面，必要时推动或等待上游兼容更新。
知识库 Unix 秒可使用标准库；桌面端与 CLI 的日期解析、RFC3339 格式、时区、DST 和
CodeWhale 兼容边界需要逐项审查，再选择维护中的库。迁移必须覆盖 epoch 前后、负数小数秒
取整、序列化格式和时区行为，验证四处依赖图确实无受该例外覆盖的 chrono 后删除例外。
到期前应在 #2702 复核迁移进展与上游可用性；到期仍未完成时门禁照常失败。
若确需延期，须另提明确日期和风险、取得维护者明确批准并重新审核，不能由本方案自动延期。
若新的 chrono 漏洞公告进入 CI 扫描的两张依赖图，仍由 cargo-deny 阻断并立即重评；
另外两张图的独立公告审计需另行验证，不能由固定版本守卫代替。
