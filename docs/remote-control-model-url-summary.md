# 临时模型 URL 摘要

`controller/temporary_model.rs` 的 `ExportRoute` 对外提供两种不同用途的表示：

- `to_wire_value` 和通过现有校验的运行时模型保留原始连接 URL，供任务协议和模型连接使用。
- `to_redacted_value` 与 `Debug` 仅保留 HTTP(S) URL 的协议、主机、端口和路径，移除用户信息、全部 query 和 fragment。主模型与视觉回退模型共用这条规则；无法解析或不支持的 URL 显示为 `[REDACTED]`。

该处理不改变现有协议的 URL 准入规则。协议仍拒绝 fragment、用户信息和 IPv6 authority 等既有不支持形式；摘要可展示其脱敏结果并不代表该地址可以用于运行时连接。

这不是任意文本中的秘密识别器：主机和路径仍用于定位模型端点，不应在这些字段中放置秘密。原始协议产物携带任务所需的凭据，不能作为可公开日志使用。目前该摘要的已知使用范围为内核和测试，不据此宣称已经发生生产 UI 或日志泄露。

定向验证入口为 `cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib --no-default-features --locked temporary_model::tests -- --test-threads=1`，应通过仓库的 `scripts/ci-fixture-process.py` 生命周期边界执行。
