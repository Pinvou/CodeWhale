# 定时会话令牌核算（EngineCumulative）

> 英文镜像：[`scheduled-token-accounting.en.md`](./scheduled-token-accounting.en.md)（两文内容一一对应，分歧以中文原文为准）。

本页描述定时会话的引擎累计令牌核算内核：`ScheduledTokenAccounting::EngineCumulative { base_total_tokens, engine_total_tokens }` 变体，以及 `persist_scheduled_engine_state` 的配对匹配。该内核随 Community PR [pinvou-agent#865](https://github.com/Pinvou/pinvou-agent/pull/865) 回流落地（来源是对齐闭源官方构建会话侧改动的移植）；回流期间恰逢本仓第一轮 Rust 死代码清扫把单变体前身（仅 `PreservePersisted`）与其管道整体移除，本 PR 因此在清扫后的树上重新落地完整内核并扩出累计变体。生存期总额统一经 `scheduled_lifetime_total` 计算（base + engine 饱和相加），`persist_scheduled_engine_state` 与 `persist_scheduled_token_total` 共享同一编码，合并结果替换持久化总额而非累加。`scheduled_lifetime_total` 是随本内核一并落地的加固归并——两处各自裸写 `saturating_add` 没有任何标记要求它们保持一致；归并后两处无法各自漂移。

生产者刻意未接线：本仓没有在测试之外构造 `EngineCumulative` 的代码，接线只需要应用侧的一个后续 PR——spawn 基线已在 `assistant/engine.rs` 于每次 spawn 时从持久化总额读取（`scheduled_base_total_tokens`），跨回合累计已在转发器 `TurnComplete` 分支累加，生产者只需在 `SessionUpdated` 分支改构造 `EngineCumulative`。但要注意：`SessionUpdated` 事件本身不携带用量，这样改构造后写入的值恰好等于上一次 `TurnComplete` 已经持久化的值（会计上是空操作）；真正的增量落地仍走 `TurnComplete` 分支对 `persist_scheduled_token_total` 的直写。该直写不可删除、也不可改成枚举路径——`TurnComplete` 没有消息载荷，构造不出完整的 `ScheduledEngineState`；两条路径必须继续共享 `scheduled_lifetime_total` 同一编码（由回归测试的两条腿分别钉住）。

来源对齐批次中另一涉及 workspace 字段的半块改动被判定为破坏性，未随本内核移植：`session.metadata.workspace` 保持由 profile 派生的既有语义。

核算合同：`engine_total_tokens` 是该引擎自 spawn 起的累计用量，不是逐次增量；合并结果替换持久化总额，过期的 base 允许把总额调低，因此每次重 spawn 都必须从持久化总额重读基线。饱和钳制与「替换而非 max()」的语义均由回归测试钉住（base + engine 溢出饱和于 `u64::MAX`；过期基线 `{100, 5}` → 105）。枚举与共享编码已登记为 `scripts/fork-guard.sh` 的 APP 指纹，防止未来的死代码清扫或对齐同步无声移除内核：

```sh
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib scheduled_engine
```
