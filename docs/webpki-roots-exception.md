# webpki-roots 兼容包装层的变更控制

RustSec 在 2026-10-21 修订了 [RUSTSEC-2026-0381](https://github.com/RustSec/advisory-db/blob/main/crates/webpki-roots/RUSTSEC-2026-0381.md)
及 [RUSTSEC-2026-0383](https://github.com/RustSec/advisory-db/blob/main/crates/webpki-root-certs/RUSTSEC-2026-0383.md)，
显式将各自的 `0.26.11` 兼容包装层列为 `unaffected`。#2035 已移除本 PR 早期提交中
针对 0381 的 ID 忽略（main 从未包含该忽略项），不再需要该公告例外；其他版本仍
正常接受扫描，包括包含独立旧根数据的 `webpki-roots 0.26.10`。当前策略对无效忽略
默认告警；与 #1518 引入的 `unused-ignored-advisory = "deny"` 组合后才会拒绝。
本文件和守卫保留原路径，供既有 CI 与历史审核链接继续定位。

已审核的 `webpki-roots 0.26.11` 是兼容包装层：发布归档中的 `Cargo.toml` 唯一正常依赖是
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
守卫及回归；不能只扩大允许范围。这里的机器强制面仅为上述两个 `webpki-roots`
包的版本、来源、校验值及依赖边；`webpki-root-certs`、`rustls-webpki`、`rustls` 的
修复版本仍由锁文件与正常公告扫描约束，没有用此守卫冒称固定所有 TLS 包。
包装层消失或版本改变时应重新评审，并移除或更新专用守卫，不能直接跳过。

**裸 `cargo deny` 不会执行上述图变更守卫**，但会正常扫描这些公告。
CI 的两个共享策略入口在同一个失败即停止的步骤内先执行守卫，再用 `--locked`
扫描，避免元数据解析修改已审查的锁文件。另有 bans 规则独立拒绝受影响的
`webpki-roots >=1.0.0, <1.0.10`，但它不能取代完整图守卫。
两处扫描步骤显式使用 [GitHub 内置 `shell: bash`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idstepsshell)（`bash --noprofile --norc -eo pipefail {0}`），
不依赖 workflow/job 的 shell 默认值；契约测试拒绝删除或替换这两个步骤的 shell。
Draft 且仅改 CLI 路径时，Ready 前不执行该守卫；Ready/Merge Queue 的依赖路由
和本地组合命令才执行相应检查。三份主锁之外的测试 harness 锁和 CodeWhale 锁
不在本守卫覆盖范围，需独立跟进，不能把主锁验证当作它们的扫描结果。

本地需 Python 3.11+ 和 cargo-deny 0.20.2。从仓库根运行以下组合命令；守卫失败
时必须停止，不得直接执行下一条或新增全局忽略：

```sh
python3 scripts/guard-webpki-roots-exception.py && cargo deny --locked --manifest-path pinvou-knowledge/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
python3 scripts/guard-webpki-roots-exception.py && cargo deny --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
python3 scripts/guard-webpki-roots-exception.py && cargo deny --locked --manifest-path pinvou-cli/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources
```

在不支持 `&&` 的旧 PowerShell 中，先检查守卫的 `$LASTEXITCODE` 为 `0`，再执行
对应扫描。回归命令为 `python3 -m unittest discover -s scripts/tests -p 'test_webpki_roots_exception.py'`。
其他公告、许可证、来源和依赖策略均继续按既有规则执行；此变更控制不表示产品的
所有安全风险均已消除，也不替代后续依赖升级。CI 契约检查器只支持其明确识别的
`run:` 标量形式；多行 plain 标量、带块头注释、缩进标记、tag/anchor 等未支持形式须先更新测试，
当前显式失败，不允许把它们静默当作不含审计命令的普通行。
对已识别的标量，检查器按 shell 单词去除引号和转义并识别命令分隔符，
因此 `'cargo' 'deny'`、相邻引号和续行不会漏掉新增扫描。该静态检查不执行 shell，
也不声称能求值变量、`eval` 或外部脚本；引入动态审计入口时必须显式扩展入口契约。
