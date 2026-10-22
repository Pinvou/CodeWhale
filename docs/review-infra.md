# 评审基建脚本（review infra）

本仓自上游 Pinvou/pinvou-agent（社区仓，引入基线 `349569ac6`）引入两枚评审基建
脚本。它们源自该仓 PR #455 评审中的真实事故教训，与桥接/产品代码无耦合，逻辑
语义与上游逐字一致（路径、钉子清单经核对本仓现状后无需改动，见下文适配核对）。

## scripts/test-registry-guard.py — 测试注册守卫

对 `pinvou3-app/src-tauri` crate 的 `cargo test --lib -- --list` 注册表断言：

1. 每条测试路径至多注册一次（重复的 `#[test]` 属性会静默吞掉相邻函数的属性，
   且膨胀与缩减相互抵消，跑总数看不出来——上游 #455 round-26/28 两次真实发生）；
2. `REQUIRED_PINS` 中每个 consent/rollback 关键回归钉恰好出现一次，且未被
   `#[ignore]`（源码级扫描，stable libtest `--list` 无法区分 ignored）。

用法（需先有已构建的 lib 测试二进制，脚本复用 cargo 产物，秒级完成）：

```bash
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml \
  --lib --features benchmark-hooks --locked --no-run
python3 scripts/test-registry-guard.py
```

`REQUIRED_PINS` 全部 12 个钉子（install consent 回滚对、marketplace scope
清理/热刷新钉、secrets resync 双钉、plugin_import 回滚钉、recycle_bin 三钉、
legacy scope fail-closed 双钉）已逐条核对在本仓 `src-tauri` 中存在且语义相同，
清单原样保留。新增 consent/rollback 关键钉时应同步扩充该清单。

平台注意：其中 `secret_values_resync_*` 两钉为 `#[cfg(unix)]`（需要不可读文件
fixture）。`cfg(unix)` 在 macOS 与 Linux 均生效——本仓 macOS 实测全绿（4714 条
唯一测试路径、12 钉齐）；仅 Windows 本地会报这两个 miss，属预期缺位而非属性
失窃。上游脚本头注释写"local run only goes green on Linux"，按本仓实测 macOS
同样可绿，完整门禁仍以 Linux CI rust-test 腿为准。

## scripts/audit-merge-authored.py — 合并解析作者审计

对 `BASE..HEAD`（BASE 默认 `origin/main`，可传参覆盖）内每个 merge commit，
报告"合并树相对两个父提交都新增"的标识符（≥6 字符）：这类内容从未出现在任何
可评审的非合并 diff 里，逐提交评审线程看不见（disclosure-only，恒退出 0，
是评审输入不是裁决）。

```bash
python3 scripts/audit-merge-authored.py            # BASE=origin/main
python3 scripts/audit-merge-authored.py <base-ref>
```

已知边界（上游 round-26 minor 13）：只覆盖"新增"方向；删除型合并解析对本检查
不可见，由评审的 file-list / deleted-symbol 手法兜底。干净输出只能读作"没有
解析新增的标识符"，不能读作"没有解析内容"。

## 与现有守卫的关系

| 守卫 | 职责 | 与本组关系 |
|---|---|---|
| `scripts/architecture-guard.py` | 前端/Rust 模块边界与依赖方向 | 不重叠：本组不检查边界 |
| `scripts/validate-commit-msg.py` | 提交信息格式与 DCO | 不重叠 |
| `scripts/fork-guard.sh` | CodeWhale fork 指纹与行为名下限 | 不重叠：本组只看本仓 src-tauri 测试注册表与合并提交 |
| `scripts/community-backflow*.mjs/.py` | 社区回流队列自动化 | 互补：回流合并后的解析内容可用 audit-merge-authored 事后审计 |
| `cargo test --lib` | 测试正确性 | 守卫只读注册表，不执行测试；重复注册造成的静默吞钉由本组补盲区 |

## CI 接线（follow-up，本批未做）

本批刻意不改 `.github/workflows`，避免扩大触发面。上游接线方式供后续参照
（上游 `pr-check.yml`）：

- `audit-merge-authored.py`：PR/merge_group 事件上以 `origin/<base_ref>` 运行，
  disclosure-only；紧随 fork-guard 指纹层之后，秒级。
- `test-registry-guard.py`：Linux rust-test 腿，`cargo test --lib --no-run`
  （`--features benchmark-hooks`，与全量 lib tests 共用一次编译链接）之后、
  `cargo test --lib` 之前运行；macOS/Windows 腿不接（unix pins 预期缺位）。

后续接线 PR 应复用上述位置，并确认本仓 runner 内存边界后再启用 `--no-run`
共用编译的写法。
