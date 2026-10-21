# webpki-roots 兼容包装层的受限例外

RUSTSEC-2026-0381 的版本范围仍匹配 `webpki-roots 0.26.11`。
已审核的该版本是兼容包装层：发布归档中的 `Cargo.toml` 唯一正常依赖是
`parent = { package = "webpki-roots", version = "1" }`，`src/lib.rs` 仅重导出
`parent::TLS_SERVER_ROOTS`，没有独立根证书数据或构建脚本。三份锁文件现均将
该依赖解析到公告已修复的 `1.0.10`。这样保留现有 FastEmbed / ORT 运行时，
包括 Windows 支持，无需为消除包装层版本告警升级运行时主版本。

`scripts/guard-webpki-roots-exception.py` 在不联网、不加载 submodule 的情况下，
同时核对 Knowledge、桌面应用和 CLI 的三份锁文件。只接受官方 crates.io 来源、
唯一的 `0.26.11` 与 `1.0.10` 包、已审核校验值以及精确依赖边：

| 版本 | 发布归档 SHA-256 |
| --- | --- |
| 0.26.11 | `521bc38abb08001b01866da9f51eb7c5d647a19260e00054a8c7fd5f9e57f7a9` |
| 1.0.10 | `1c1c8e20c8a38fc7d65abfe74eb7280e4aa667e8eec8bc84996d2874810748d9` |

缺失或损坏锁文件、降级、新版本、异源包、校验值变化、重复包和依赖边变化都会
拒绝。未来任何相关版本更新必须重新核对发布归档、公告修复范围与依赖图，更新
守卫及回归；不能只扩大允许范围。包装层消失时应移除本例外并同步移除专用守卫。

`deny.toml` 的公告忽略本身只能按 ID 生效，**裸 `cargo deny` 不会执行上述守卫**。
CI 的两个共享策略入口在同一个失败即停止的步骤内先执行守卫，再用 `--locked`
扫描，避免元数据解析修改已审查的锁文件。另有 bans 规则独立拒绝受影响的
`webpki-roots >=1.0.0, <1.0.10`，但它不能取代完整图守卫。

本地需 Python 3.11+ 和 cargo-deny 0.20.2。从仓库根运行以下组合命令；守卫失败
时必须停止，不得直接执行下一条或新增全局忽略：

```sh
python3 scripts/guard-webpki-roots-exception.py && cargo deny --locked --manifest-path pinvou-knowledge/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
python3 scripts/guard-webpki-roots-exception.py && cargo deny --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
python3 scripts/guard-webpki-roots-exception.py && cargo deny --locked --manifest-path pinvou-cli/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
```

在不支持 `&&` 的旧 PowerShell 中，先检查守卫的 `$LASTEXITCODE` 为 `0`，再执行
对应扫描。回归命令为 `python3 -m unittest discover -s scripts/tests -p 'test_webpki_roots_exception.py'`。
其他公告、许可证、来源和依赖策略均继续按既有规则执行；此例外不表示产品的
所有安全风险均已消除，也不替代后续依赖升级。
