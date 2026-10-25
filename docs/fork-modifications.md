# CodeWhale Fork 修改清单

## 本轮同步接线：冷启动所有权与有界拆除（2026-10-13）

- 按用户最新确认，本轮为远控 v1 接入主模型 Responses（含原生图片输入），不扩视觉兜底协议。显式 openai_responses 与官方 OpenAI GPT 目录路线均导出正确 wire，B 与 Relay 独立复算；伪造 Chat/Responses 标签拒绝，不改写原 SavedModel 预设，不改 CodeWhale 源码。视觉兜底仅接受 Chat；远程 Responses 主模型不会被 Chat-only image_analyze 自动复用，原生图片路径不受影响。普通本地 Responses 行为保留。
- 独立审核发现的协议劈叉由纯 core/model_wire.rs 收敛：既有 host/prefix 规则原样提取，传输路线的 host/vendor/preset/model 协议判定与 bridge 实际落点一致；DeepSeek host 与 vendor 优先级纳入校验。锁定任务不再读取 B 宿主的 PROVIDER/MODEL 环境钉来压制 wire，普通本地环境优先级保留。覆盖主模型接纳/运行时物化、伪造 wire/vendor、Chat 视觉兜底、原生图片、环境隔离及跨 JS/Rust 共享正负向夹具；未发布协议直接同批更新 allowlist 与固定合同摘要，不留开发协议兼容分支。

- main #1808 的有界提交、终态落盘 join 和 Shutdown 拆除，与远控栈的冷启动守卫共存。会话加载仍在 spawn 前；新引擎以 `ColdEngineCustody` 托管，非阻塞 SyncSession 入队后才公开 entry。加载失败不生成引擎；同步失败、取消或公开冲突不得发布不完整引擎。
- 不复用 main 原来的 load-after-spawn 错误分支：其 wrapper 已无调用前提，改由守卫覆盖全部未公开退出路径。Drop 同步 cancel、abort forwarder、按 CancelSubAgents→Shutdown 尝试入队；仅 Shutdown 未送达时，在不持 turn gate 的后台接入 main 的 `bounded_teardown_shutdown`（首发最多 5s，后续重试最多 60s）。含 CancelSubAgents 的剩余操作仍按原 FIFO 有界重试；闭包持有旧 AppEngine，保留其 bridge/私有 Skills，不引用 pool 或新实例。
- 保留 main 两个 Shutdown 通道结果测试；将旧“两处 hydration wrapper 调用”静态钉迁到 preload→spawn→custody→try_sync→publish 顺序和守卫实际有界接线，并补充真实 AppEngine mailbox 满载/单剩 Shutdown、资源托管及不误伤 resident 回归。无 runtime 时继续同步 best effort、不 panic；入队不等于 provider 已 join，持续卡死仍可能留下旧引擎任务。

## 已合入维护分支：压缩检查点角色兼容性

- T7：将带类型的 Agent 拓扑检查点放到最近一次真实用户输入之后、该轮 assistant/tool 链之前，包括压缩摘要位于链尾的情况。Chat Completions 发送前把结构化识别的当前格式摘要移到其保留轮次之前，并把该摘要与拓扑检查点合并进这条 user 出站消息；普通用户引用摘要标记不会触发重排。修复范围是压缩流程自身产生的尾部 `tool -> user`，客户报错原文为 `SSE stream request failed: HTTP 400 Bad Request: response_role: "user"`（SSE 载体）；存储历史及工具调用/结果 ID 不变，会话固定的系统前缀也不变。
- T7 后续批次（2026-09-22/23，#63–#76）：#73 把检查点识别族迁至 `history_recognition` 叶模块（行为不变，识别族与 `compaction/last_round.rs` keep/recompact 过滤的关联已注明）；#65 roster 列举宿主 profiles；restore 载体 provenance 锚定（原 #74 已批准，因服务端分支丢失事故脱绑）由 #76 按同一内容重建并经确认随本批合入（`61cb769be`）。
- 未覆盖的相邻真实 user：多次压缩后保留的**真实**用户消息之间因中间轮次被裁掉而在出站线上相邻（`retained_user_messages` 默认保留 20k token 的用户消息，测试 `compaction_does_not_merge_unrelated_adjacent_user_messages` 固定 `[user, user(summary+prompt), assistant, tool]`）。本修复刻意不合并真实用户轮次——那会改变模型看到的轮次边界——因此若某路由的模板同样拒绝相邻 `user -> user`，该路由在多次压缩后仍会 400。现有证据只有上面那条尾部形态的报错原文，未做客户服务重放，无法判定模板对相邻**真实** user 的容忍度；该场景需要路由相关的出站测试与修复，属本 PR 范围外。
- 父仓配套：`pinvou3-app` 的模型服务错误分类把两种载体的 role/模板 400 都归为请求格式卡片且不提示重试——SSE 包装的 `HTTP 400 ... response_role: "user"` 与流式之外的 `Invalid request (400): ...`（`LlmError::InvalidRequest` 的 Display 文本）。卡片文案只说明请求格式被拒，不声称具体线形已修复，因此上述未覆盖场景不会误导用户。分类器按文本形态匹配、刻意保持宽松：含 `server error` 字样的 400 体仍归可重试的 `server` 类，带引号的 JSON-KV 形态 `{"response_role":"user"}` 不命中 `response_role\s*:` 分支而落回未分类。这两处宽度已登记，如需收紧另开改动。
- 这是可复用的 CodeWhale 修复，已在 `Pinvou/CodeWhale#62` 通过审核并 squash 合入 `pinvou3-clean`（`2ab5e64b5`）；审核证据取自候选分支 head `abadae45d`（其已把维护分支 `92427bd8d` 并入），候选树与 squash 提交树同为 `32880a95`，内容等价。修复前持久化的会话由该修复在会话载入/恢复时自动修正，不再要求新建会话，也没有独立的迁移工具。父仓 gitlink（`7fc36e587`）已包含该提交，公开可达性门禁按过渡期口径验证通过。
- 回归测试 `forkguard_compaction_topology_preserves_tool_round_boundary` 覆盖空/活跃 Agent 拓扑及重复压缩；`forkguard_compaction_tool_round_has_valid_chat_wire_roles` 校验完整出站角色序列和工具 ID；`compaction_marker_quoted_by_user_keeps_wire_order` 覆盖普通用户引用标记；`forkguard_restored_pre_fix_session_has_valid_chat_wire_roles`、`restore_replaces_a_pre_provenance_carrier`、`replay_replaces_a_restored_topology_carrier_instead_of_stacking` 覆盖修复前会话的恢复修复，`restore_keeps_a_user_turn_that_quotes_the_summary_header` 与 `compaction_checkpoint_is_never_the_edit_target` 覆盖恢复层删除边界与 `/edit` 归属。既有拓扑测试继续覆盖终态和用户仿冒消息。
- 审核跟进 `0d679a5e8`：检查点 carrier 改按结构识别（首块以摘要头开头、其余块为引擎写入的来源块），恢复层把遗留 carrier 移到放置锚点、再次压缩丢弃被取代的已恢复 carrier 而不是叠加第二份；保存期放置、恢复修复与请求期重排现在共用同一套锚点定义（此前三份实现已在「哪些消息算 prompt」上漂移）；0.9.6 之前的旧 header carrier 同样被识别与重排；普通用户粘贴完整摘要头不再在恢复时被静默删除，且在被覆盖统计豁免的路径上也不丢；程序生成的检查点不再被 `/edit` 当成可编辑轮次。
- 范围边界：本 PR 不额外提供历史迁移入口；修复前持久化的会话在会话载入/恢复时按上述结构识别与放置锚点自动修复。手动 `/compact` 的旧风险（恢复后的拓扑检查点仍留在工具结果之后）随之消失；本 PR 不覆盖已载入内存副本的在线迁移，也不做客户服务重放验证。压缩流程之外，后台子 Agent 的完成、失败或等待事件，以及 LSP 诊断、步骤预算、子 Agent 协调、输出截断和基准预算提示，都可能在工具结果后产生 `tool -> user`。真实用户的中途补充和中断恢复也可能形成相同序列，但不能在不改变用户意图的前提下重排。这些独立场景需另行设计路由相关的出站测试与修复；CodeWhale 仓库未启用 Issues，暂记录在本清单与 PR 审核讨论中。
- 已知后续项（CodeWhale 侧，非本 PR 阻塞，同样记录于此）：`is_compaction_summary_text` 在 `0d679a5e8` 之后失去最后调用者成为死代码（`pub`，无编译告警）且其文档注释仍描述为在用；`chat.rs` 中退化孤儿 tool-result 分支的注释仍称该请求保留严格模板拒绝的线形，实际会丢弃孤儿并只发一条 `user`；以精确 header 开头的真实轮次在「无系统提示检查点」的恢复路径、以及 `retained_user_messages` 生成的单块保留副本在下一次压缩时被丢弃这条链路，仍缺回归测试。

> 本文是 Pinvou 对 CodeWhale fork 的单一现状清单。
> 维护策略见 [`fork-policy.md`](fork-policy.md)，升级证据见 [`codewhale-upgrade-0.9.5-to-0.9.12.md`](codewhale-upgrade-0.9.5-to-0.9.12.md)。

## T2: shell environment guidance (integrated upstream as CodeWhale #50)

- Integration status: resolved by the 2026-09-11 backlog batch. [CodeWhale PR #50](https://github.com/Pinvou/CodeWhale/pull/50) merged the re-ported candidate as `1d9ee26e6` (a re-port of [fork PR #42](https://github.com/Pinvou/CodeWhale/pull/42) onto v0.9.12 r1), and parent PR #482 advanced the parent gitlink to `ae7e3fb36f89486f30d41b28ae0eaaa516ae4740`, which includes it. The public-submodule gate passes in transition mode: the immutable `pinvou-v0.9.12-r1` tag stays at `1fafee7e26b60a59457a43bce50c63aa2ad9dbaf` while the gitlink rides the public maintenance branch ahead of it, so this PR no longer pins a separate candidate ref.
- Review: [Pinvou/CodeWhale#42](https://github.com/Pinvou/CodeWhale/pull/42), from `zhuowp/CodeWhale:fix/shell-environment-guidance`. The reusable upstream contribution is [Hmbown/CodeWhale#5900](https://github.com/Hmbown/CodeWhale/pull/5900); its source review and compiled verification are separate from the fork integration that has now landed.

- The `Bash` tool description and `command` parameter now share guidance from the existing execution dispatcher. PowerShell, cmd, POSIX sh, Bash, zsh, and other custom shells receive matching syntax guidance. Tool names, permissions, execution, aliases, and read-only argv behavior remain compatible. On the v0.9.12 re-port, the model-visible lowercase `bash` contract surface is unchanged; extending the aligned guidance to that surface is a separate scope decision for review.
- The shell guidance fix is one of the registered post-r1 commits (CodeWhale PR #50, `1d9ee26e6`) inside parent gitlink `ae7e3fb36f89486f30d41b28ae0eaaa516ae4740`; at review time the candidate drifted 3 files, +207/-2 above the r1 head. Upstream main still contained the login-shell-only description when inspected on 2026-09-05.
- Coverage: `shell_guidance_matches_each_interpreter`, `forkguard_shell_catalog_guidance_matches_execution`, and the opt-in `export_shell_guidance_eval_fixture` for live model comparisons. The application removes Unix-specific default examples from shared instructions, browser HTTP verification, and attachment analysis guidance.
- Live-model methodology, measured results, and verification limits: [shell guidance evaluation](shell-guidance-evaluation.md).
- Review follow-up: preserve zsh's `=command` warning and provide Bash/POSIX quoting, pipelines, heredocs or syntax exclusions, and utility-portability guidance. Unix `$SHELL` paths represented as `Custom` receive the same guidance as built-in Bash/sh variants; cmd and fish have their own syntax notes. Covered by `shell_guidance_preserves_unix_shell_contracts`.
- Guidance selection uses one `match`, with a PowerShell-family guard shared with execution and a dedicated text constant. This structural refactor preserves custom PowerShell detection and model-visible wording.
- All shell-specific guidance now lives in named constants, including cmd, fish, and the shared fallback. A before/after comparison across 14 shell cases confirmed identical output after constant extraction.
- Following the 40-call curl ablation, remove the tool-level curl alias reminder only. Preserve the other PowerShell guidance and application instructions used in that experiment; both arms achieved 19/20 correct executions with no shell mismatch errors.

## Web JSONPath upstream backport (integrated; historical candidate evidence, #756)

- [上游 #7046](https://github.com/codewhale-hq/Codewhale/pull/7046) 于 2026-10-22 以 `b9619942905ac4b62752a579ca8937467d0486fc` 合入。`4164aa6fd` 在公开维护提交 `083cebfd9` 上通过 `git cherry-pick -x` 保留原补丁、消息及贡献者署名；包含该 pick 的候选分支 tip `4bf5e0894` 保留在 `integration/jsonpath-upstream-7046`，用于历史溯源。实际维护分支通过 CodeWhale#117 的 squash `ea9fdc90f` 集成，父仓已由 Official #2087（community#750 的回流 PR）消费此基线。此项属于既有 T2，不新增 fork 主题。
- 边界：本次回移只对齐上游 Web schema 文案和真实 parser 契约，不改变 parser、网络策略或执行。更完整的双引号键/非负索引/ASCII 点字段/对象通配符说明及既有带引号键内 `]` 扫描限制留作后续；当前状态、计数和测试由既有基线登记负责。

## T9：Custom 路线的配置 wire 直达运行时（父仓 #625 × CodeWhale #79）

- 状态：已随 2026-10-08/09 批次收口。底座侧 [Pinvou/CodeWhale#79](https://github.com/Pinvou/CodeWhale/pull/79) 以 squash `3035a022e`（`fix(route): honor custom table wire on runtime routes`，2026-10-08）合入 `pinvou3-clean`，并随该批次把登记头推进至 `0bc868ff0`（上游 v0.9.12 之上 57 个提交）；候选期结束——该批次父仓 gitlink 即公开维护分支头，当时 `scripts/fork-guard.sh` 的 `EXPECTED_HEAD`/`EXPECTED_COMMITS=57` 为该历史批次登记值，候选重钉义务解除。候选评审史：round-5 收口于 `86dd8e8d2`（方言别名单点解析、身份 scope 读取、app-server ingress 关门、preflight 按表方言校验、捕获/回放边角与 fixture/helper 整理，别名表共享与方言警告收窄）；round-6 十一路从零复审再收口于 `842c4a4bb`（authority-glued query URL 指纹碰撞闭合、wire 绑定客户端内部重解析钉、方言警告表名与发射钉、/provider 行协议钉、endpoint 指纹降级方向文档）；本节登记随父仓 #625 落地。
- 问题：命名 `[providers.<name>]` 表的 `wire = "responses"` 从不到达真正服务轮次的客户端。`RouteRequest` 没有 wire 通道，`candidate.protocol()` 恒为 Custom 的向后兼容静态策略（`Fixed(ChatCompletions)`），而 #3384 宿主解析路线架构下每个 `Op::SendMessage` 都携带宿主解析的路线、`install_resolved_runtime_route` 经 `DeepSeekClient::from_candidate`（绑定 `candidate.protocol()`）重建客户端——配置写着 Responses、实际 POST `{base}/chat/completions`。认得该覆盖的只有环境性 spawn 客户端（`DeepSeekClient::new` 经 `provider_wire_format_for_config`）与 `provider_capability_with_wire`，二者都不在逐轮传输路径上。品悟父仓 PR #625 以命名表把 GPT 目录模型改走 Responses 线，三轮评审未察觉，正因运行时路径零测试钉（评审第四轮以可执行探针实证 `candidate.protocol() == ChatCompletions`）。
- 修复：`RouteRequest.wire_override`（仅 `ProviderKind::Custom` 认，内建保持描述符策略）；tui 路线层 `custom_wire_override_for` 读活动表方言（与 `provider_wire_format_for_config` 同一 `provider_wire_dialect` 来源，三处构造性一致），客户端内 `rebound_for_model_protocol`/逐请求路由/限额查找钉传输自身 wire，端点域客户端不因引擎内换模型被降级。加密推理捕获从 Codex 专属放宽到所有发送 `include: ["reasoning.encrypted_content"]` 的 Responses 路线（Codex + Custom 表；DeepSeek 明文 `reasoning_text` 与 Concentrate 契约不变）；回放门按捕获 provider 串匹配，并收窄到提供方身份（命名表为 `custom/<table>`，一张表的加密推理不再回放到另一张表）；捕获随之放宽到所有发送该 include 的 Responses 路线（OpenCode Zen 花名册一并纳入，include 与捕获按构造覆盖同一路线集）。round-5 评审跟进收口剩余缝隙：方言别名表在 config crate 单点解析（`wire_dialect_override`；拼错的方言保持降级到旧 Chat 策略，已钉），运行时路线从身份 scope 后的配置读覆盖（非当前 custom 身份不再借用全局选中表的 wire），app-server `/v1/chat/completions` 入口线程同一方言使其 Chat-only 守卫对 `wire = "responses"` 表关门而非把 chat body 打向 Responses-only 端点，preflight（`route_is_valid_for_model`）按表自身方言校验，捕获/回放边角（Chat wire 不捕获；缺失或空 `encrypted_content` 跳过且流不断）均为突变验证过的钉。消费者普查：环境客户端、capability 报告、运行时候选三者对同一表方言构造性一致；config-free 的 `resolve_route_candidate*` 包装保持静态策略（仅校验/展示，不建客户端）。
- 覆盖：config 侧 `custom_wire_override_mints_a_wire_true_candidate`、`wire_override_is_ignored_for_builtin_kinds`、`custom_wire_override_edges_stay_fail_closed`；tui 侧 `forkguard_named_table_wire_responses_reaches_the_runtime_candidate`、`forkguard_named_table_wire_anthropic_reaches_the_runtime_candidate`、`forkguard_named_table_without_wire_keeps_the_chat_default`、`forkguard_named_table_unrecognized_wire_keeps_the_chat_default`、`forkguard_identity_pinned_route_reads_the_pinned_tables_wire`、`forkguard_ambient_wire_override_does_not_leak_onto_pinned_chat_tables`、`forkguard_custom_responses_route_turn_client_posts_to_the_responses_endpoint`（wiremock 断言 POST 路径 `/v1/responses` 与 `store:false`/`include`/逐字模型）、`forkguard_custom_anthropic_route_turn_client_posts_to_the_messages_endpoint`、`forkguard_custom_responses_stream_captures_encrypted_reasoning_as_opaque_state`、`forkguard_custom_responses_captured_state_replays_on_the_next_turn`、`forkguard_custom_responses_replays_only_exact_model_opaque_reasoning_state`、`forkguard_fixed_endpoint_legacy_state_without_fingerprint_keeps_replaying`、`forkguard_repointed_builtin_legacy_state_without_fingerprint_fails_closed`、`forkguard_custom_chat_stream_does_not_capture_encrypted_reasoning`、`forkguard_custom_responses_capture_tolerates_missing_or_empty_encrypted_content`。guard 指纹 14 条（T9）。
- 父仓配套（同 PR）：`uses_responses_wire` 增加 `DEEPSEEK_MODEL` 环境钉让位与官方 OpenAI 端点边界（非官方宿主保持 Chat，`openai_responses` 预设组是任意端点的显式 opt-in）；`findCloudProviderForModel` 自定义组仅匹配同 preset 记录（vm 实证回归钉）；`RESPONSES_ROUTE_PROVIDER` 注释改述真实引擎落点。round-5 评审收口 gate/落地劈叉：`provider()` 的 `OpenaiResponses` 臂施加与 wire 门相同的操作员压制（裸 `DEEPSEEK_MODEL` 钉把该组落回 vendor "openai" Chat 路线，gate 成为唯一裁决者，双向钉死）；官方宿主判定先小写再剥 `/v1` 后缀（`https://api.openai.com/V1` 计为官方）；新增测试钉 chat 时代存储的 `off` 仍逐字上线（引擎映射为 low）。app 侧无法直读候选协议（`ResolvedRuntimeRoute.candidate` 为 `pub(crate)`），wire 真值由底座测试与 guard 指纹承载。

## 2026-10-15：引擎自起轮次保留同模型宿主限额（CodeWhale #108）

- [Pinvou/CodeWhale#108](https://github.com/Pinvou/CodeWhale/pull/108) 已合入公开维护提交 `13ee4dfd2349f8ece3d2b7169031d09189cc9ec7`；候选 `d2ad2895497f898f5801b74acf85a24244aa2416` 与合入提交同树 `06f4b017cb9cff784268a30906a07aa31c9d1736`，不把候选 SHA 用作父仓 gitlink。
- 在既有 #107 客户端修复之上，`current_runtime_route` 为 `/edit` 重发、subagent 完成恢复、空闲 shell 唤醒和 goal 续跑重新推导路由时保留已安装的宿主限额。仅当限额绑定当前 session model、重新解析的 wire model 相同且新旧限额确有差异时重申；模型漂移、wire 漂移与值相同路径保持原有解析。未新增 App 侧限额状态或替代底座能力。
- `active_route_limits_model` 随每个限额写点同步；identity-keyed resolver 仅向 crate 内开放。该修复不改变 #54 的多根 `workspace_roots` 授权，也不替代 #107；既有能力与指纹全部保留。`Cargo.lock` 相对 `803e672bb` 同 blob `ea9bd73310d193445f6d7852b3b383e6f551b29b`，没有依赖升级。
- 四条原结果测试：`current_runtime_route_carries_installed_host_limits`、`current_runtime_route_does_not_carry_limits_across_wire_drift`、`current_runtime_route_does_not_carry_limits_across_model_drift`、`current_runtime_route_leaves_matching_limits_unstamped`。父仓补充这些测试的源码指纹；源码存在、同树 CI 和本轮实际运行结果须分别披露，静态计数不能当作执行通过。
- 合入前公开 review/inline 列表为空；PR 作者提供了测试与突变验证说明，后续评论同时披露既有全套并发 flaky。不能据此声称本轮重跑完整 Rust 或已获独立外审批准。当前适用门禁和父仓完整原生 CI 仍以对应精确 head 的实际结果为准。

## T10：Tachyon（驭光）本地引擎思考强度方言（CodeWhale #113，随 2026-10-20 批次收口）

- 状态：已随 2026-10-20 批次收口。[Pinvou/CodeWhale#113](https://github.com/Pinvou/CodeWhale/pull/113) 以 squash `083cebfd9`（`feat(provider): add Tachyon reasoning controls`，2026-10-20）合入 `pinvou3-clean`，登记头随之推进至 `083cebfd9`（上游 v0.9.12 之上 76 个提交）。同批次 #112（`fix(tui): align model-facing text with actual behavior`）为模型面文本与实现对齐的修复批次（RLM 元数据只教真实 REPL 面、工具描述失实声明逐项更正），不单列主题。
- 问题：引擎团队《思考强度对接》通知（2026-10）：Tachyon 本地引擎 wire 与 vLLM 同族（`chat_template_kwargs.enable_thinking` + `reasoning_effort`），但有效档为 none/low/medium/xhigh——`high` 会被产物模板 400 拒绝，最深档是 `xhigh`。复用 vllm 方言会把 max/xhigh 硬降级为 `high`（不可上线），未识别 provider 名又兜底 DeepSeek 方言，任何取值都变不出 `xhigh`。
- 修复（Pinvou 私有方言，不回馈上游）：config 侧注册 `ProviderKind::Tachyon` 与 provider 元数据（registered-only，不进 ALL 可选目录；`[providers.tachyon]` 表、`TACHYON_BASE_URL` 环境钉、keyless CredentialHelp、golden route ids/export 同步，登记数 49）；设置序列化统计 registered-only 表，内建上下文窗口校验纳入 Tachyon，inventory 仅认活跃/显式配置的路线（registered-only 身份与免密默认不推断可运行服务）。tui 侧 `ApiProvider::Tachyon` 查表/槽位/免密/持久化全链；`apply_reasoning_effort` 三档位族：off 族 `enable_thinking=false`，low~high 族 low/medium 透传且 `high` 防御性映射 medium（永不发 high），xhigh/max 族直发 `reasoning_effort=xhigh`（不复用 vllm 的 max→high 降级）；`provider_accepts_reasoning_content` 纳入 Tachyon——目录内推理 id 回放为 ON（与 vllm 的刻意差异，本条即 T10 登记），目录未知 id 解码依赖宿主显式 `separate_field` 配置；`TransportKind::for_kind` 补 Tachyon→LocalRuntime；`should_skip_secret_store_for_provider`、`route_declares_unknown_output_ceiling`、pricing/scorecard/route_billing 本地免计费与输出天花板名单、`providers_sorted_for_display` 过滤（registered-only 意图）、provider_picker 价格标签与流可见性、model_inventory 本地标签与默认模型、doctor 鉴权标签均补臂；round-2 加固：`root_deepseek_model_is_foreign_to_direct_provider` 的"可宿主 DeepSeek 模型 id"组在 tui 与 config 两处镜像名单补 Tachyon，route_billing 本地免计费回归循环补 Tachyon，workspace `.env` 凭据白名单排除 `TACHYON_API_KEY`（keyless 默认；shell 环境与 provider 表仍可用，取舍加注）；评审后段加固：`normalize_for_route` 补 Tachyon 臂（High→Medium、Max→XHigh、其余恒等——未编目 Tachyon 不再落入通用收缩），回执/档位别名（minimal/ultra）归一到生效线上档。
- 覆盖：登记头处 8 个 `forkguard_tachyon_*` 测试——tui 侧 `forkguard_tachyon_effort_ladder_never_emits_high`、`forkguard_tachyon_max_maps_to_xhigh`、`forkguard_tachyon_stream_separates_reasoning_for_unknown_model_ids`、`forkguard_tachyon_picker_preserves_normalized_effort_on_reopen`、`forkguard_tachyon_thinking_capability_is_route_scoped`、`forkguard_tachyon_fleet_accepts_explicit_reasoning_for_served_ids`、`forkguard_tachyon_inventory_requires_an_active_or_configured_route`（inventory 仅认活跃/显式配置路线），config 侧 `forkguard_tachyon_only_provider_table_survives_config_store_save`（仅配置 Tachyon 的设置经 ConfigStore save/reload 与二次重写存活；复退旧实现四测失败）；config 侧零上下文窗口拒绝参数化；route_billing 本地免计费回归。forkguard 门禁命令（`cargo test -p codewhale-tui --lib --locked forkguard_ -- --test-threads=1`）在登记头 `083cebfd9`（Linux）实测 214 过/0 挂；#113 合并评审记录为 212。

## T2：保留 Windows Python 标准流默认编码（已合入；原候选验证记录，CodeWhale#115）

- 每次普通 shell 执行都会叠加 hook/task 环境（包括空映射）。旧 `CommandSpec::with_env` 会替换整个构造器映射，丢弃 `PYTHONIOENCODING=utf-8`，导致 Python 回退到 Windows 本地编码，而 PowerShell 和工具输出读取器仍期望 UTF-8。修复改为合并叠加值，并按 Windows 大小写不敏感规则处理显式覆盖；环境清理器、显式覆盖值及 Python 文件/默认文本编码策略不变。
- 共用 shell 构造器覆盖前台、后台/PTY、交互式和编码 PowerShell 重试。既有直接 `CommandSpec::program` 消费方（只读命令的直接 argv 路径及 `codew sandbox run`）从未设置此 shell 默认值，不在本修复的覆盖声明内。
- 已合入：当前 gitlink 为公开维护头 `ea9fdc90f1`，包含 #115/#116/#117；原候选提交的测试证据仍按下述执行边界保留。
- 父仓祖先门禁回归同时隔离 fixture Git 命令与门禁子进程的继承 `GIT_*` 变量；污染环境指向独立临时仓库时，三种真实拓扑条件仍得到预期结果，外部仓库、工作区和索引逐字节不变。明确断言拓扑/指纹分界，避免静默跳过新插入的层。
- 默认 Windows 子进程环境回归持有进程环境锁，在空/无关叠加测试前设置 GBK 哨兵，防止继承 UTF-8 设置而掩盖缺陷。探测使用实际 shell 语法，仅 PowerShell 执行编码命令，并强制 30 秒超时及子进程清理。此前作者在外层 `PYTHONIOENCODING=utf-8` 下恢复旧替换逻辑，强化回归失败（0 通过、1 失败），恢复最终源码后通过。
- 原生端到端回归覆盖实际小写 `bash` 工具和后台 ShellManager，在 GBK 父环境下检查 Python stdout/stderr 及 64 KiB 前置输出。需 PATH 中的 Windows Python 和 PowerShell dispatcher；从 PowerShell 或 cmd 运行且取消 `SHELL` 设置。显式命令：`cargo test -p codewhale-tui --lib --locked forkguard_windows_python_stdio -- --ignored --test-threads=1`，可用 `CODEWHALE_TEST_PYTHON` 选择解释器。
- 既有作者本地 Windows 验证：`cargo test -p codewhale-tui --lib --locked sandbox::tests -- --test-threads=1` 通过 23 项（19 项 `sandbox::tests::*` + 4 项 `repl::sandbox::tests::*`；Linux 因 cfg 差异选择 26 项）。两个显式工具测试分别用系统 Python 3.14 和捆绑 Python 3.13.14 通过（共四次），另一个 Git Bash 环境探测通过一次。此前恢复原构造器时，两个工具测试在两种解释器下均失败，表现为 Python 中文乱码而 PowerShell 中文正常。格式、架构和快速指纹检查通过。这些是既有作者证据，不代表后续文档修订重新执行了原生测试。
- 父仓 Windows regression 步骤新增执行默认值合并、shell 默认值接线及大小写覆盖三个确定性筛选器；每个筛选器都要求至少执行一项测试。`91e262ba2` 的远端 [Windows regression job](https://github.com/Pinvou/pinvou-agent/actions/runs/39761405525/job/119767350755) 已实际执行并通过这三个筛选器；该证据对应这个旧头及不变的底座，不代替后续提交的 CI。两个需 Python 的显式测试仍为作者本地证据。`pinvou3-clean` CI 仅 Ubuntu，不运行 Windows 限定回归。Linux 上的 `windows_shell_default_env_forces_python_pipe_stdio_utf8` 只验证 helper 返回值，不验证构造器接线；Windows 大小写覆盖逻辑也需原生 Windows 回归。上游 codewhale-hq/Codewhale#7041 是未来上游 Windows CI 的贡献路径，不证明该 CI 已执行。未验证客户设备、PowerShell 7 或安装包发布。

## 0. 当前状态（2026-10-23）

- 当前公开维护头与父仓 gitlink：`ea9fdc90f1c90335554833195eeeb847c83d82dd`（2026-10-23）。CodeWhale#115 已 squash 为 `7d26369934`，#116 已 squash 为 `e9a7f48da1`；本轮仅更新父仓引用，没有推进维护分支。 #117 已由远端于 2026-10-23 合入为 `ea9fdc90f`。
- 实测历史：上游 v0.9.12 `dcd4c200f` 之上 79 个提交，r1 `1fafee7e2` 之上 64 个；77 个带 DCO sign-off。未签名的 squash 为 #107 `f27672935` 与本次 #116 `e9a7f48da1`，不能继续声称只有一个未签名提交。保留已发布历史，不改写维护分支。
- 不可变 tag `pinvou-v0.9.12-r3` 仍在 `61cb769be5b33abc64f64da4272f5b39a8b6c1fd`；r1 仍在 `1fafee7e26b60a59457a43bce50c63aa2ad9dbaf`。公开门禁仍要求 gitlink 等于维护头并验证 r3 标签，未放宽任何条件。
- 实测 drift：504 files, +86011/-37667，净增 48344；231 个独立 `forkguard_*` 行为名（登记下限仍为 64）。原生 Windows 证据仅适用于原记录的执行提交，不扩展为本次集成树已执行；当前父仓 CI 和最终集成树审核仍须独立确认。

### 本次维护集成

- Official 既有父仓回归继续保留：Community #709 对引擎自起 `EditLastTurn` 重发轮保留宿主申报限额的端到端覆盖，以及 `edit_last_turn` 注释与 ADR 0006 对齐，不由本轮替换。

- 父仓适配（沿用既有登记）：v0.9.12 EngineConfig、Agent/Plan 模式、逐轮 reasoning/安全、ExtraTools、owner 事件隔离、Automation v3/v4 数据兼容、rusqlite 0.40.2、Shell 任务来源对账；消费方 PR #396（execpolicy）、#408（轮次取消）、#444（蜂群）、#468（computer-use）、#472（一键导出）依赖已登记底座能力

- #115 保留 shell 默认环境并合并叠加，Windows 显式变量名按大小写不敏感覆盖；#116 已以 [`e9a7f48da1`](https://github.com/Pinvou/CodeWhale/commit/e9a7f48da1) 合入，将返回的失败结果标志写入实际历史并验证会话往返。两项均在 T2 内，不增加主题。
- #116 选择性前移上游 `2ae7460e4` 的 `is_error` 映射；该映射不在 v0.9.12 基线内，上游同一提交的取消 metadata 与 shell receipt 改动未纳入。成功返回但 `success=false` 的结果以及中断且未执行的标记记为失败，正文保留；原有执行错误、成功及 schema 加载但未执行的成功语义不变。
- 影响面为持久化历史的 `is_error`、恢复后 Anthropic 请求中的失败标志、CLI failure 计数、会话选择器预览、runtime API 会话 JSON/工具回执，以及 Pinvou 应用恢复工具卡片时由 `!is_error` 回填的成功状态（`bridge-shared-helpers.js`）。实时工具卡片与 ACP Failed 状态原已依据 `ToolResult.success`，不因这次历史标志映射而改变；CodeWhale TUI 恢复历史的渲染器不生成 ToolResult 卡片。子 Agent 记录器（`tools/subagent/mod.rs`）与 TUI `SyncSession` 镜像（`tui/ui/event_loop.rs`）原有的标志丢失仍是后续项，不声称 #116 已覆盖。
- 指纹同时登记 Windows 默认值/构造器/显式覆盖/原生工具测试及 `forkguard_tool_failure_flag_survives_recording_and_round_trip`。底座合入不等于客户环境或发布验证通过。

### 轮次绑定取消：宿主 stop 按轮身份分派（父仓适配，本 PR）

- 底座半边已并入登记批次：CodeWhale PR #38（squash `f5c68cab8`，见第 3 节提交序列与 T1 保留内容）把共享 cancel 槽升级为 `TurnCancelSlot { turn_id, token }`，并提供宿主入口 `EngineHandle::cancel_turn(turn_id, reason, mode) -> bool`（身份校验、steer 处置与 token 克隆在同一把槽锁内完成）与收口期 `EngineHandle::publish_stop_disposition(reason, mode)`（只发布 stop 处置、绝不触发任何 token）。
- 底座候选半边（复审轮新增，封闭边界 ③）：`Op::SendMessage` / `Op::EditLastTurn` 增加宿主提交关联字段 `submission_id`，由该轮的 `TurnStarted` 逐字回显；全部运行时自启路径（idle 子代理完成、后台 shell 唤醒、goal 延续、composer shell 命令）恒回显 `None`；线协议 `EventMsg::TurnStarted` 携带同一增量字段（`serde(default)` 缺省缺席，向后兼容）。回归 `forkguard_turn_started_echoes_submission_id_self_starts_stay_none` 钉死回显契约：宿主提交轮的 `TurnStarted` 回显自身令牌、自启轮恒 `None`。该提交已随 #58 以 squash `f05b9acfa` 合入（2026-09-17），并随 2026-09-17 批次把公开 `pinvou3-clean` 推进至 `92427bd8d`。
- 父仓配套（本 PR，与底座共同封闭 pinvou-agent#254）：`EnginePool::cancel` 的取消闭包在 `arm_pending_cancel_and_cancel` 的同一 state 锁临界区内领取 (epoch、已观察 turn_id、收口期) 同源身份快照，经 `turn_bound_cancel_action` 纯函数分派——绑定命中走 `cancel_turn_with_mode`（槽已切换时底座整体跳过、不发布处置）；目标轮尚未被 forwarder 观察到（submit→TurnStarted 窗口）与终态收口期收敛为仅 `publish_stop_disposition`、绝不开火——窗口内引擎槽可能已是延迟观察到的自主续跑轮活 token，无差别开火即 #254，真正在途的目标轮由同临界区内 arm 的 `pending_cancel` 在 TurnStarted 后按 turn 绑定重放取消（槽已切走则底座身份校验把重放整体丢弃）；forwarder 的 `pending_cancel` 重放改为 turn 绑定，且仅当事件回显的提交令牌与武装时记录的一致才消费重放：宿主提交路径（用户发送、多智能体、评测重放、编辑上一轮）经 `Pinvou3Bridge::next_submission_id` 统一铸造 `sub-<uuid>` 令牌并在武装时记入 lifecycle，超越自启轮的 `TurnStarted`（无令牌或异令牌）既不能消费重放、也不能把取消引到自身，宿主提交轮自身的回显到达时重放按 turn 绑定精确落地（app 回归 `overtaking_self_started_turn_started_cannot_consume_the_replay`）。消费闸门仅由提交令牌构成：武装时记录的 epoch 仅作溯源、不参与消费判定——窗口内无关自主轮可以完整跑完 start→terminal 生命周期，使目标轮自身的 `TurnStarted` 以 newly-active 身份推进 epoch，若以 epoch 为消费闸门会在回显精确匹配时误拒、静默丢失用户 stop（app 回归 `pending_stop_replay_survives_an_autonomous_lifecycle_before_the_target_starts` 按 started→terminal→目标 started 的 forwarder 顺序钉死该时序）。武装缺令牌今日不可达（全部宿主提交路径均铸造令牌），arm 站点会发出 `log::warn`，且无令牌重放与任何回显（包括 `None`）都不匹配、永不可消费（回归 `pending_cancel_armed_without_submission_id_is_never_consumed`），消费闸门因此不可能被静默重开。已知边界：① 级联取消（`Op::CancelSubAgents`）未随裁决收敛，绑定跳过时仍取消引擎当前全部子智能体（N 的遗留清理是停止契约，与 N+1 刚派生的子智能体在 app 侧不可区分）；② 入口即 idle 的 backstop（stop=clear 契约）刻意保留无绑定开火——该路径无目标轮可裁决，若引擎已自主续跑，命中其活 token 是 clear 契约的预期语义，瞄准已结束轮的 stop 入口快照为 Some、不会走到该分支；③ 原登记的追击超越窗口（自启后续轮的 `TurnStarted` 抢先消费重放并把取消引到自身、宿主 stop 静默丢失）已由本轮底座提交关联回显 + 父仓回显匹配消费闸门封闭（见上两条），该边界不再保留；④ 计划轮监督器（`wait_for_scheduled_terminal`）保留其先于 turn 绑定契约的无绑定 `cancel_with_reason(External)` 开火（自动化到期/停止权威，语义即整体叫停引擎当前活动）：开火点在观察到计划轮自身 `Started` 信号之后；先于该观察到达的取消请求会先闩锁，待 `Started` 观察到时才开火（等待超 30 秒则按 Timed out 放弃），落在该观察间隙内的自启续跑会承受这次无绑定命中——非本 PR 引入，保留原状；改走 turn 绑定 `cancel_turn` 属自动化契约变更，留作后续工作；⑤ 引擎回收（`reclaim_engine_entry` 经 `engine.cancel_current()`，删除/换模型/闲置收割）保留无绑定开火：回收即引擎拆除——条目已先行摘除、forwarder 随即中止并按需补发 Interrupted 终态、`Op::CancelSubAgents`+`Op::Shutdown` 按同通道 FIFO 收尾，轮次随引擎消亡，不存在「错误轮幸存」，若引擎已自主续跑而 forwarder 未观察，命中其活 token 属回收契约的预期语义。另一处已知有损角落：stop 武装后，若一条超越的自主续跑先跑完 start→terminal 重开 reserve 闸门、用户又在目标轮自身 `TurnStarted` 被处理前发送新消息，`reserve()` 的整体清理会丢弃该武装（重放无回显可匹配、静默失效）；窗口极窄且可恢复——目标轮真正开跑后再次 stop 即按 BoundTurn 精确命中。
- 候选登记已回收：#58 落地后父仓 gitlink 随登记批次重钉（回收时为 `92427bd8d`，`EXPECTED_COMMITS` 相应推进），`CANDIDATE_HEAD`/`CANDIDATE_COMMITS=29` 机制移除；其后 #552 批次已把登记头推进至 `7fc36e587`（`EXPECTED_COMMITS` 36，见第 0 节表格）；指纹层保留底座回显契约回归与父仓超越序回归两条锚点；`verify-public-submodule.sh` 恢复绿（过渡期断言 gitlink = `pinvou3-clean` 分支头）。


### 本轮维护头对齐

固定维护提交 `803e672bb` 已包含 CodeWhale #92 的逻辑调用幂等身份契约：同一逻辑调用的重试复用 ID，不同工具循环调用生成独立 ID，沿现有 T1 宿主与模型调用边界登记。#92 最终 PR 树与维护合入 `0cb1aec47` 的树完全相同，25 个操作身份测试声明及已有指纹在当前维护头保留；这是源码保全证据，实际测试执行结果单独记录。主线 `3118c36b8` 尚无该能力，旧内容分支 `8b5a86390` 也不是维护头，不作为最终 gitlink。后续 #102 测试与未用 API 清理、#96 会话导出、#103 checkpoint 与 #104 toolbox 变更沿既有主题维护；不能仅以维护头身份代替回归验证。

## 1. 为什么本次使用 clean re-fork

旧 r13 相对 v0.9.5 修改 110 个文件。与 v0.9.12 对照时，104 个旧修改文件也被上游改动，直接三方移植预计产生 57 个冲突文件。与此同时，上游已经吸收或重构了大量旧 patch，包括会话快照/恢复、编辑上一轮、压缩后 token、provider/model 路由、原生搜索、Windows UTF-8 Shell、JSON schema 修复与任务基础设施。

因此本次从官方 `v0.9.12` tag 直接建立新分支，只重表达仍然缺失且必须位于底座生命周期内的语义；没有 merge 或 cherry-pick 旧 r13 冲突树。

## 2. 旧 r13 逐项处置

| 处置 | 能力 | v0.9.12 r1 结果 |
|---|---|---|
| 上游已有，删除 fork | 会话 snapshot/recovery、edit-last-turn、post-compaction tokens、route budget、严格直连模型大小写、JSON 容器修复、厂商原生搜索、Windows UTF-8、provider pin 等 | 采用上游实现和测试，不复制旧代码 |
| 语义迁移 | `Yolo`/`Auto` 模式、全局 reasoning、旧 custom-tools/disabled-skills 接口 | 映射到 `Agent` + approval/trust、逐轮 `Op::SendMessage` reasoning、`ExtraTools` 与显式 Skills 根 |
| 继续保留 | 宿主 facade/route limits、可靠 steer 与批量取消、MCP secret resolver、raw worker ledger、逐轮最终分发安全、64 KiB File 上限、prompt ownership（含 locale bookend 瘦身接线，只约束回复语言、不约束思考语言）、Automation conversation/schema/lifecycle | 重写到 v0.9.12 当前 Engine/Task/Prompt 结构并增加结果式测试 |
| 继续保留但默认关闭 | r13 benchmark eval controls | 在 v0.9.12 架构上恢复无歧义 missing-read-action repair；仅由 benchmark feature 显式启用，桌面默认路径关闭，父仓 `benchmark-hooks` 负责编排与观测。final-only-after-tool-budget 自 2026-09-11 起宿主不再 arm（工具调用轮数上限已整体移除，budget 永不耗尽，arm 是死代码），底座侧机制与其 forkguard 测试保留，作为未来上游化/删除决策的候选 |
| 继续保留 | #35 API 搜索链直接以 Bing 收尾的覆盖 | 配置型 API backend 失败后直接落到免密 Bing。DuckDuckGo 的内部 Bing fallback 只覆盖空结果/challenge，不覆盖连接失败；在 DuckDuckGo 不可达的网络中把它作为外层尾部会提前返回错误，因此恢复直接 Bing 尾部并以结果式测试锁定全部 API provider |
| 采用上游删除 | stuck/read-repeat/coaching guards | 接受上游 `b39cf5650` 的处置：旧 stuck 指纹不含结果摘要，会把活跃 job 的重复 poll 误判为无进展；不恢复旧 guard 或相关环境开关，继续依靠有限 `max_steps`、每轮墙钟和取消边界（宿主自 2026-09-11 起不设工具调用轮数预算） |

### 上游测试处置注记

以下上游测试不是静默删除，而是因为 Pinvou 已登记的产品语义与上游默认语义不同而替换：

| 上游测试 | v0.9.12 r1 处置与原因 |
|---|---|
| `full_access_auto_approves_non_bypassable_registered_tools` | 高层执行测试替换为 `full_access_blocks_non_bypassable_registered_tools_without_prompting`；Full Access 不得绕过注册工具的 non-bypassable approval。上游 resolver 的低层对照测试仍保留，反转只发生在 Pinvou 最终 Engine dispatch 边界 |
| `discover_for_workspace_and_dir_merges_workspace_and_configured_sources` | 恢复上游默认 merge 路径测试；另以 `forkguard_explicit_skills_dir_excludes_ambient_workspace_sources` 覆盖 Pinvou 宿主选用的显式单根路径 |
| `system_prompt_merges_workspace_and_configured_skills_dir` | 恢复上游默认 composer 测试；另以 `forkguard_system_prompt_uses_only_explicit_configured_skills_dir` 锁定安装宿主 composer 后的 ambient workspace 隔离 |

## 3. 当前提交序列

| Commit | 主题 | 说明 |
|---|---|---|
| `38dd961ea` | T1 | 重建 v0.9.12 宿主 facade、路由与嵌入边界 |
| `a5c12e203` | T2 | 保留工具兼容、MCP secret 与执行期安全入口 |
| `7dc1a429a` | T3 | 恢复宿主静态 prompt 所有权 |
| `02c0faa27` | T4 | 保留 Automation 与 Task 的 Pinvou 运行归属 |
| `b4c02616b` | T1–T4 收口 | 可靠 steer、受限控制面、最终分发、ambient 隔离、宿主 prompt-only profile/显式 Skills 根、生命周期回归及当前 Rust 发布 lint 兼容 |
| `dbd1b7cb3` | T1/T2/T4 评审修复 | 恢复 feature-gated benchmark eval controls，并补齐 64 KiB 写入、session cancel、终态删除、受限轮 idle deferral 与 MCP 隐藏/拒绝的结果式守护 |
| `fe0cd7551` | T1/T2/T3 评审收口 | 恢复上游对照测试并注明产品反转；增加 steer 真实 channel/turn-loop 回归；登记 Permissions fragment 上游化债务 |
| `ff299f94b` | T2 复审修复 | 恢复配置型 API 搜索链的可达 Bing 尾部；移除空的 benchmark observability feature；补回评测兼容路径的设计理由注释 |
| `54819b0d6` | 发布门禁 fast-follow | 为手动精确 SHA CI 补齐 migration manifest 比较基线；修正 rustdoc 私有链接与过时步数说明；将 18 个仅供下游宿主兼容的宽 facade 从生成 API 文档中隐藏，不改变编译 API 或运行行为 |
| `ff9959bfc` | 发布门禁跟进 | 删除已由当前阻断结果式回归替代、且产品语义已明确反转的上游 Full Access 比较 helper；不抬高 dead-code 预算 |
| `6615af7ca` | 文档复审收口 | 删除 4 处指向不存在的 fork-policy 小节引用，直接说明测试所锁定的产品反转与显式宿主边界 |
| `409138dbe` | 运行时契约发布门禁 | 精确登记官方 v0.9.12 `automation`/`tasks` 路由字段与 Agent 精简的净增长，以及 Pinvou r1 写入上限和 host prompt-only profile 的模型可见增长；只抬高 Act/Operate full 的真实上限，同时收紧 active 与 Plan full，工具 identity 不变 |
| `881cf4444` | 精确 SHA CI 收口 | macOS npm wrapper 在可选 sccache 丢失后的冷启动 release build 可使用 60 分钟上限，并以 wiring 回归锁定例外只作用于该 job |
| `baa87f4de` | T4 复审修复 | offline-misfire skip 只作用于 recurring，使过期一次性任务仍精确持久入队一次后暂停 |
| `1fafee7e2` | T2 复审修复 | 所有搜索后端不可用时恢复可操作且脱敏的 provider/config 配置提示 |
| `a7215d3c4` | 门禁修复 | 恢复 pinvou3-clean 分支保护的必需合并检查（#49） |
| `bf435e5e8` | T4 修复 | artifact 清理失败时保持终端任务内存一致（#47） |
| `c3a35216e` | T2 文档 | 模型面工具描述与提示词对齐实际行为，36 个工具文件（#41） |
| `a89048184` | T2 修复 | finance 工具接入网络策略闸门（fail-closed）、verifier 不再教退役工具名、notify 配置方法合同钉、fleet-manager 技能补 `resume`/`stop --all`（#31） |
| `ff4add43e` | T2 增强 | execpolicy phase-2：cmd.exe 单字母斜杠旗标跳过、deny 中段通配符、`.exe` 后缀折叠、绝对路径规则精确匹配、规则集跨克隆活共享、subagent 工具调用过同一 execpolicy 门（#37） |
| `f5c68cab8` | T1 修复 | 共享取消槽绑定轮次身份：`TurnCancelSlot` 原子换装、`cancel_turn(turn_id)` 身份校验+处置+令牌同锁、`publish_stop_disposition` 收口期只发处置不开火（#38，修 #254 底座半边） |
| `3f8a25eef` | 文档 | CHANGELOG 补记 API 搜索链直接无 key Bing 尾部与大陆网络成因（#39） |
| `dd7b7785f` | T6 新增 | subagent 自适应限流：`DynamicGate` 可缩容 launch gate + `RateLimitGovernor` 60s 滑窗 AIMD（≥2 事件或 >30% 减半、≥4 暂停、时间驱动探活自愈），429 重试尊重 Retry-After/全抖动退避（#43） |
| `1d9ee26e6` | T2 修复 | Bash 工具指引与执行同一 shell dispatcher 派生（Windows PowerShell 不再收到 login-shell 指引）（#50，取代 r13 线 #42） |
| `68461e84e` | T2 增强 | 工具结果经 `metadata.images` 携带图片：与用户附件同路径的 `<image>` 三元组、单结果上限 2 张、坏图降级不失败轮次（#48，computer-use 截图回传基础） |
| `09ebba3fa` | T5 新增 | 会话全保真 tar.xz 导出：`session_export` 模块 + `codewhale sessions export` CLI，liblzma 静态压缩，刻意不脱敏与 `/export` 互补（#51） |
| `6ae5b1734` | T1 修复 | GLM-5.3/5.3-Flash forced-thinking：`disabled` 改写为 `enabled`+`low`，effort 别名映射到 low/high/max（#52，默认 Z.ai 模型 `off` 档必现报错的修复） |
| `ae7e3fb36` | T1 修复 | BigModel 通用端点 `open.bigmodel.cn/api/paas/v4` 纳入第一方 Chat 路由谓词，推理控制在两 host 统一（#53） |
| `72e98fd89` | T2 修复 | 模型可见文本不再指名当前环境缺失的工具，避免误导工具选择（#56） |
| `f05b9acfa` | T1 新增 | `TurnStarted` 事件回显 submission id，宿主可对齐提交与启动轮次（#58） |
| `102da17a1` | T3 修复 | `project_instructions` 与 constitution 来源标签改为相对化，避免绝对路径泄漏（#59） |
| `889fc2f99` | 文档 | TUI changelog 切片重同步、surface facts 与测试 clippy 清理（#60） |
| `92427bd8d` | T2 修复 | stopship 侦察改经 `tool_search` 与 `grep_files` 路由（#61） |
| `2ab5e64b5` | T1 修复 | 压缩交接保留工具轮边界（#62） |
| `ce783728c` | T2 修复 | computer-use 尊重缩放栅格契约、ssh 元素状态留在宿主侧（#57） |
| `7fc36e587` | T3 治理 | subagent governor 评审跟进：按 tokio Semaphore 重建 DynamicGate（#55） |
| `c4e6caf9` | T1 修复 | Windows PowerShell 回退受策略绕过与命令行上限约束，行为测试迁入 forkguard 前缀。父仓 Windows Rust 门禁执行两个 PowerShell 回归过滤器并拒绝零匹配，`fork-guard` 以 15 条源码/行为指纹固定该主题（#66） |
| `8e5988119` | CI 同步 | CNB 镜像在 `CNB_GIT_TOKEN` 未配置的仓库探针跳过而非红检（#64） |
| `6f780290f` | T1/T2/T3 修复 | agent/search/vision 工具面与运行时对齐，MCP boot 失败/恢复简报区分处理，直呼兜底话术并入 `HANDLE_READ_ACTIVATION_HINT` 共享常量（#67），fork-guard 两条指纹随之更新 |
| `d349f2537` | T4 修复 | 有限超时治理（21 面审计高影响集）：外部工具审批与 `request_user_input` 人工等待不再 300s 自动拒绝并移出轮墙钟预算，后台空闲判定把运行中工具视为进展，MCP `tools/call` 预算 60s→1800s 且 `read_timeout` 继续约束快速请求，OAuth 回调 300s→900s，审批取消发 `approval.decided{interrupted:true}`（#63） |
| `c5fadb86d6` | T7 新增 | subagent roster 动作列出宿主呈报的 profile：roster `profile_query` 支持过滤宿主 profiles，修复 `get()` 混同 host id 的去重（#65） |
| `594860ce9` | T1/T3 修复 | 评估 #67 与上游重叠面并采纳上游四处做法：恢复历史隐藏内部运行时 handoff（保留 MCP system cell 显式例外）、read 结果按预算自限后免二次压缩（metadata 记实际字节为对上游的有意偏离）、行摘要器输出有界 child 路由回执（#68） |
| `37c658da7` | T1 修复 | MCP boot 流式就绪：pending 连接逐个 settle 即时上架、boot 窗口内 turn catalog `tool_search` 重查、空结果与未配置区分并给 boot-aware 提示，收窄 boot 追加与 pool 诊断（#69） |
| `7a7f65e8fd` | T3 修复 | fresh-review 残差收口：rlm `handle_read` 配对描述教激活路径（stdout 预览与截断标记同范），registry 上限 pin 列出第四个 eight 侧并加编译期+运行时双钉，submission 回显 forkguard 补齐全部四条自启路径（#71） |
| `7f04c907d1` | T3 修复 | 祖先链与项目规则来源标签仓库相对化（git root 相对、正斜杠），checkout 移动/大小写改变不再扰动 pinned 系统提示前缀，双 checkout 字节一致回归（#70） |
| `ba2243edd5` | T2 修复 | computer-use `screenshot(region)` 按裁剪区在返回 raster 上绑定原点：darwin/linux/win32 三后端坐标映射修正，zoom 契约延伸到截图路径（#72） |
| `ba2a07768f` | T7 重构 | 压缩检查点识别族（摘要标记、provenance 常量、header 谓词、checkpoint 识别）原样迁至 `history_recognition` 叶模块，解除 `compaction`⇄`runtime_handoff` 依赖环，`compaction` re-export 保持路径兼容（#73） |
| `684a93101c` | T2 修复 | Windows cfg(test) 门控：`tools/process.rs` 的 unix 专属测试 helper（`use super::*` 与 `shell_command`）随唯一 `#[cfg(unix)]` 测试一起门控，修复父仓 windows-rust-test 的 `-D warnings` 编译失败（#77） |
| `61cb769be` | T7 修复 | restore 载体 provenance 锚定：带戳载体存在时仅锚定/删除真实载体，无戳遗留历史保留松散替换，粘贴摘要不再被误删或窃锚；镜像 wire 侧双序回归测试（#76，替换因服务端分支丢失事故无法合并的 #74） |
| `3035a022e` | T1 修复 | 逐轮路由解析尊重自定义 wire：`RouteRequest` 增加 `wire_override`（仅 Custom 生效，内建保持 descriptor 策略），tui 路由层从活动 provider 表带出 dialect，客户端内部 rebind/请求/限额解析钉住传输自身 wire，receipts/preflight/逐轮客户端读同一协议；此前具名 `[providers.<name>]` 表配 `wire = "responses"/"anthropic"` 会被逐轮路径静默降级为 Chat Completions（#79） |
| `41a039268` | T7 修复 | restore 无检查点不得删史：`restore_compaction_checkpoint` 的载体删除扫描改为仅在存在 provenance 带戳载体时运行，无戳历史的松散匹配不再把以摘要头开头的真实用户轮误删（#88） |
| `3788c4161` | T5 修复 | 会话归档导出封闭在会话存储内：`session_artifacts_dir` 不再信任拼接后的 `<sessions>/<id>/artifacts` 路径（`is_dir` 跟随符号链接会把归档遍历重定向出会话存储），目录级收口先于成员级符号链接跳过生效；`write_session_archive` 第二参数语义改为会话存储根、由引擎侧自行推导并收口 artifacts 目录（#89） |
| `182f85411` | T4 修复 | #63 超时审计复审收口（十轮评审）：snapshot restore/history 解析器拒绝截断 git 捕获、plugin 面 drain 上限记为截断证据、app-server 流失败写回 consumed seq、任务初始化失败先做陈旧锁清扫、丢弃的 forced-approval publish 记日志不再吞掉、尾部 drain 跳过活性心跳、超时 gate 不伪造 exit code、陈旧 ref-update 锁清扫，及 mcp/runtime/tasks/subagents 十余处文档如实披露（#75） |
| `7624cd3db` | T4 特性 | human-wait 墙钟暂停（自主 #75 剥离，按 12-agent 对抗复审重建）：可交付决策面（runtime API 的 `decide_approval`/`submit_user_input`）上的审批/用户输入等待窗暂停任务墙钟并让 idle 时钟窗闭后全新重计，TUI 私有任务运行时无投递通道故不武装该窗、墙钟照常到期；聚合 24h fail-safe 上限跨整任务求和，超限以 `HumanWaitTimeout` 终态；parked 轮询自 200ms 捕获节奏倍退至 2s 上限、决策到达广播提前唤醒；逐 claim 突变验证红绿互钉（#78） |
| `906ffa979` | T5 修复 | Windows 硬链接计数改走稳定句柄 API：`number_of_links` 仍 unstable（`windows_by_handle`），父仓 Windows 腿 E0658；以 `GetFileInformationByHandle` 读取（与 Runtime store 身份检查同机制），无法打开/查询按单链接放行、由归档写入端的流式读取大声暴露真实问题；unix/fallback 臂仅增加未用 path 参数保持单一调用点（#94） |
| `0bc868ff0` | T4 修复 | drain-cap 测试的四个名字改为显式导入、不再经 tests mod 的 unix 门控 glob（#77 同形）解析：测试本身平台无关（tokio duplex），Windows lib test 编译此前 7 处 E0425/E0433（#95） |
| `44921cf02` | T2 修复 | 浮动 stable 工具链（1.99.0，2026-10-01 滚出）编译与 lint 复绿：`mark_taken` 的 `Atomic::fetch_update` 已更名 `try_update` 并弃用，MSRV 1.88 无新名，唯一调用点 `#[allow(deprecated)]` 并注明缘由（对旧工具链惰性）；顺带清掉 1.99 clippy 对存量代码的三处新发现（`agent_plugin` 单元素 `for key in ["args"]` 循环展开、错误串逐字节不变；`workflow_panel` 两处非捕获 Copy 闭包的多余 `&bounded` 借用）；首证于父仓 windows-rust-test `codewhale-tui` 编译红（#80） |
| `f27672935` | T1 修复 | 同 wire model/protocol 的 model-aware 请求及 subagent/fleet rebind 保留宿主冻结 route limits，避免无宿主 overrides 的静态重解析将声明输出额度降回未知模型 8192 默认钳制；跨模型/协议继续重建（#107，公开提交未含 DCO trailer） |
| `7d2636993` | T2 fix | Preserve shell defaults when task environment overlays are merged; explicit Windows names override case-insensitively. CodeWhale#115 adds generic overlay and Windows foreground/background Python stdio regressions; child-env sanitization and public signatures are unchanged. |
| `e9a7f48da` | T2 fix | Preserve failed tool results as `is_error:true` during history recording. Existing upstream round-trip regression covers HTTP/tool errors, success and schema-loaded non-execution. CodeWhale#116; no public signature/dependency change. |
| `ea9fdc90f` | T2 help/contract | Document the already supported Web JSONPath subset; one schema-description string changes, with a resultful parser-contract regression. No execution/parser/API/dependency change (CodeWhale#117). |

除 #116 squash `e9a7f48da` 与 #107 squash `f27672935`（其合并提交信息未保留 sign-off 尾部，已留言知会上游维护者，见 CodeWhale#107）外，所有提交都含 DCO `Signed-off-by`。`b4c02616b` 包含大部分跨主题收口，历史粒度确实不利于 bisect；分支公开进入评审后没有为历史美化 force-push，而是追加带 sign-off 的提交修复评审和发布门禁问题，并用本表、指纹和行为测试弥补审计粒度。不可变 tag 已创建，后续不得重写。

## 4. T1 — 宿主嵌入与路由边界

### 保留内容

- 对宿主公开 `AppMode`、`ApprovalMode`、Automation、Task、route 和 worker ledger API。当前兼容 facade 还包含 18 个 `pub mod`，父仓全部 Rust 源码中有 61 个文件、340 处直接引用；这些模块作为不稳定下游兼容桥刻意不进入生成 API 文档。这是已登记的收窄债务，不在本次热修中做破坏性改名。
- F05 账户调用幂等（原固定补丁 `8b5a86390`，已通过 CodeWhale #92 纳入 2026-10-14 当时的维护提交 `803e672bb`）：模型逻辑调用幂等属于 T1 的执行期边界。宿主仅在账户 Gateway 路由的 `Config.request_idempotency_header` 显式启用 `Idempotency-Key`；字段不接受配置文件输入，普通模型保持原 HTTP 行为。`ModelClient` 与 `LlmClient` 的 operation-aware 默认方法兼容既有实现，具体 transport 按每个逻辑调用发送随机 UUID，而不是按 session、prompt 或静态 header 生成键。HTTP 重试、HTTP/1.1 后备及引擎内外层相同发送事实的流恢复复用该 ID；实际 prepared wire body、endpoint 或 provider 改变，或完整模型响应已经被接受后再继续推理、执行工具后跟进，均形成新模型调用和新 ID。新用户提交相同 prompt 也产生新 ID。Gateway 仍拒绝同键不同付款事实，不放宽其 fingerprint。此标识仅在 transport 生命周期内，不写入模型消息、session 历史、日志或 pinned prompt，KV-cache 前缀不变。回退是清除宿主 opt-in；原补丁以 `44921cf02` 为唯一父提交，父仓原 F05 提交历史继续保留；CodeWhale #92 的最终候选树与外部维护合入 `0cb1aec47` 完全相同，该批次曾以包含该能力的 `803e672bb` 为固定 gitlink；当前维护头以第 0 节为准。公开维护头严格校验、宿主 opt-in 回归和真实账户验收分别核验，源码包含性不替代门禁或验收。当前结果式回归名为 `forkguard_model_operation_http_retry_and_new_calls`、`forkguard_model_operation_opt_in_and_wire_identity`、`forkguard_model_operation_inner_and_outer_stream_retries`、`forkguard_model_operation_tool_followup_gets_new_id`；真实账户业务验收由父仓执行，loopback transport 与受控 Engine 回归不冒称模型或银行验收。通用接口可上游化，账户产品路由选择仍留在父仓。
- A04 调用幂等的额外生产边界由 `forkguard_model_operation_http1_stream_keeps_id`、`forkguard_model_operation_child_retries_keep_id`、`forkguard_model_operation_vision_http_retry_keeps_id` 覆盖：子代理每个模型 round 的 timeout/transient 外层重试复用调用 ID，`image_analyze` 的独立 HTTP 重试从冻结路由 opt-in 传递同一调用 ID。不同工具调用或后继模型 round 仍分别生成新 ID；这些 loopback HTTP 回归不包含真实模型验收。
- `forkguard_model_operation_preserved_partial_changes_call` 经真实引擎流恢复验证：交互模式已保留部分 assistant 输出并改变后续请求正文时，后续发送必须采用新的调用 ID。未产生内容且发送事实未变的恢复仍复用原 ID；不通过放宽 Gateway 指纹掩盖正文变化。
- 显式启用调用幂等的路由绕过进程内非流式 response cache，避免同正文缓存跳过新调用或付款上下文的 Gateway 派发；普通模型保留既有确定性请求缓存。相同 HTTP 回归以 `temperature=0` 验证账户每次新调用实际发出不同 ID，同时普通路由的重复确定性请求仍命中本地缓存。
- `resolve_runtime_route_with_limits` 保留 wire model、context/output 上限与 embedding alias。
- #107 的 `client.rs` 保留同模型请求及同 wire model/protocol rebind 的冻结宿主额度；跨模型仍重解析，协议改变仍走既有重建路径。本轮随集成分支继承已合入的底座修复，gitlink 无独有增量，不修改底座源码、配置或依赖；保留已有下列两条结果式测试和对应指纹，不新增底座测试。
- `EngineConfig.session_id` 在 Engine spawn 前绑定，所有子智能体/工作流事件带 owner session 并由宿主按 owner 过滤。
- `EngineHandle::steer` 返回 opaque id；`withdraw_steer` 区分已撤回与非 pending；中断、停止、压缩、换会话和 Engine drop 都使每个 id 恰好进入 committed/dropped 终态。
- `CancelSubAgents` 对当前 session 的子智能体做幂等批量取消。
- 共享取消槽带轮次身份（`TurnCancelSlot`）：`handle_send_message` 与用户 `!` shell 轮先铸 turn id 再原子安装令牌；宿主 `cancel_turn(turn_id)` 在槽锁内做身份校验、steer 处置发布与令牌解析，失配零副作用，陈旧代际的取消不再误杀引擎自启（空闲子代理完成/后台 shell 唤醒/goal 续轮）的新轮。`cancel_with_mode` 保留发射当前令牌语义给单用户前端；`publish_stop_disposition` 供宿主收口期履行 stop=clear 合同而不开火任何令牌（#38，修 pinvou-agent#254 底座半边）。
- Z.ai 第一方 Chat 路由谓词覆盖 `api.z.ai` 两产品与 BigModel 通用端点 `open.bigmodel.cn/api/paas/v4`（tui 侧谓词委托 config crate 单一实现）；GLM-5.3/5.3-Flash 为 forced-thinking：`thinking.type: "disabled"` 改写为 `enabled` + `reasoning_effort: "low"`（厂商迁移建议），effort 别名映射到 low/high/max，未知值省略由 API 默认接管；work_graph 收据约束同路由不得声称 API 未接受的 effective off（#52/#53）。
- 模型侧仍以 v0.9.12 的封闭角色集为执行姿态；仅允许宿主在当前 route Config 中显式注入的 prompt-only profile 贡献身份、描述和人设。Personal/Workspace/Plugin profile 及 model/provider/reasoning/permission/delegation pin 均 fail closed。

### 关键测试

- `forkguard_embedding_route_limits_preserve_wire_alias`
- `client::tests::model_aware_same_model_request_keeps_host_declared_route_limits`
- `client::tests::rebinding_the_same_model_keeps_host_declared_route_limits`
- `forkguard_steer_lifecycle_withdrawal_is_bounded_and_prevents_commit`
- `forkguard_steer_lifecycle_late_withdraw_reconciles_committed_state`
- `forkguard_steer_channel_commits_live_and_drops_withdrawn_input_in_turn_loop`
- `forkguard_cancel_all_running_is_session_scoped_and_idempotent`
- `forkguard_host_profile_overlay_is_config_only_and_prompt_only`
- `forkguard_cancel_turn_binding_spares_unnamed_turns_and_hits_the_observed_turn`
- `forkguard_idle_subagent_completion_self_start_ignores_a_stale_previous_turn_cancel`
- `engine_handle_stop_disposition_publishes_without_firing_any_token`
- `zai_forced_thinking_models_never_send_thinking_disabled`
- `restored_zai_forced_thinking_models_cannot_claim_effective_off`
- `zai_bigmodel_adjacent_routes_stay_fail_closed`
- `zai_chat_route_matching_is_exact`

## 5. T2 — 工具兼容与命令执行安全

### 保留内容

- `ExtraTools` 允许 app 在 Agent/Plan 原生注册宿主工具，不复制底座工具循环。
- MCP secret 只经宿主 resolver 注入；不写进程环境或普通配置文件。
- `SetDisallowedTools` 是逐会话/逐轮的权限塑形：更新后继续在该会话的 catalog 和最终调用边界拒绝匹配工具，但不热断开共享 `McpPool` 中已经建立的 server 连接。全局断连会干扰仍获授权的其他会话；底层连接由正常 pool/session 生命周期回收，安全边界由 catalog + 最终 dispatch 的 fail-closed 双检提供。
- `TurnToolSecurityPolicy` 把精确工具白名单、只读动作与 trusted external paths 下沉到每轮执行。
- 受限轮禁止动态工具、MCP、子智能体和未授权控制操作；新的显式用户消息才可安装替代权限。
- 工具在最终 backend dispatch 前再次做 exact/read-only 校验；Full Access 也不能绕过 non-bypassable approval。
- `File` 的旧/低层写入入口保留 64 KiB 硬上限；受限日志和审计不记录参数、结果或错误私密内容。
- execpolicy deny 表达力 phase-2（#37）：cmd.exe 单字母斜杠旗标（`/f` 形）可跳过而多字符 `/tmp` 类 POSIX 路径保持位置匹配；规则中段 `*` 通配零或多个命令令牌（DFS 有界）；deny 命令词单向折叠一个 `.exe` 后缀；rooted（`/`、`~/`、Windows 盘符）File 路径规则经分隔符/大小写折叠后精确匹配工作区外调用；规则集经 `Arc<RwLock>` 跨克隆活共享（`set_ruleset` 对全部派生执行器生效），`approved_for_session` 刻意克隆私有；subagent 工具调用在执行信封之后过与主线相同的 execpolicy 门（`Block` 镜像主线拒绝文案，`Prompt` 按继承的 auto-approve 姿态裁决），堵住「主线被拒转交子代理」的绕过口。均为嵌入方通用能力，候选上游化。
- 工具结果可经 `ToolResult.metadata["images"]`（文件路径数组）把图片产物交给视觉模型（#48）：引擎回合循环按与用户附件相同的 `<image path>` 三元组追加到工具结果消息，单结果上限 2 张、5 MB 共享上限、坏图/超数降级为纯文本并 `warn`，错误路径结果不附图；wire builder、会话持久化、压缩与非视觉路由剥离因走既有 `ImageUrl` 块路径而零改动。computer-use 截图回传的基础。
- 模型面文档与工具门控对齐（#31/#41/#50）：finance 工具两端点 host 前置过 `NetworkPolicyDecider`（`Deny`/未决 `Prompt` fail-closed，与 web_search/speech 同构错误形状）；verifier 背景 metadata 不再教退役工具名 `exec_shell_wait`；notify 工具对 `[notifications].method = "off"` 的合同有回归钉（配置 off 静默但工具结果仍报成功、settings 安装时机、方法变体 round-trip）；Bash 工具描述与 `command` 字段指引由与执行同一 shell dispatcher 派生（PowerShell/Bash/sh/zsh/cmd/fish 各有具体语法指引，Windows PowerShell 不再收到 login-shell 措辞）。

### 关键测试

- `forkguard_tool_failure_flag_survives_recording_and_round_trip`
- `forkguard_exact_dispatch_rejects_forged_backends`
- `forkguard_read_only_turn_rejects_write_at_final_dispatch`
- `forkguard_restricted_tool_audit_redacts_private_payload`
- `forkguard_restricted_planning_log_redacts_private_input`
- `forkguard_queued_control_op_keeps_restricted_turn_authority`
- `forkguard_queued_goal_edit_and_mcp_keep_restricted_authority`
- `forkguard_restricted_turn_defers_idle_subagent_completion_until_new_message`
- `forkguard_restricted_turn_defers_idle_shell_wake_until_new_message`
- `forkguard_denied_mcp_is_absent_from_catalog_and_blocked_at_execution`
- `forkguard_denied_mcp_tool_error_matches_the_unknown_tool_error`
- `forkguard_api_provider_chain_tail_is_bing`
- `all_unavailable_returns_actionable_error_without_private_details`
- `forkguard_mcp_secret_resolver_supplies_values_without_process_env_writes`
- `forkguard_write_primitive_enforces_the_64kib_boundary`
- `forkguard_write_file_enforces_the_64kib_boundary`
- `forkguard_benchmark_controls_are_explicit_and_default_off`
- `forkguard_benchmark_repairs_only_unambiguous_read_actions`
- `forkguard_benchmark_repairs_read_schema_and_attachments`
- `forkguard_benchmark_budget_truncates_batch_and_clears_followup_tool_surface`
- `forkguard_benchmark_final_only_rejects_repeated_tool_only_responses`
- `forkguard_benchmark_turn_repairs_file_aliases_before_execution`
- `forkguard_subagent_execpolicy_deny_matches_main_line`
- `forkguard_tool_result_images_reach_model_as_image_blocks`
- `forkguard_tool_result_without_images_key_is_unchanged`
- `forkguard_tool_result_bad_images_degrade_to_text_only_result`
- `forkguard_tool_result_images_are_capped_per_result`
- `finance_fails_closed_when_network_policy_denies_endpoint_host`
- `finance_fails_closed_on_prompt_when_default_is_prompt`
- `settings_installs_configured_method_from_config`
- `forkguard_shell_catalog_guidance_matches_execution`

## 6. T3 — 嵌入上下文与 Skills 来源

### 保留内容

- app 安装静态 prompt composer 后，底座不再追加 ambient AGENTS/project context、repo law、用户 constitution、continual harness 或重复 core profile。
- 显式 `skills_dir` 是唯一文件系统 Skills 根；插件 Skills 只能通过显式 registry 合并。
- Permissions/World State 指令片段有独立 100 KiB 窄上限，其他片段仍用 40 KiB 默认上限。
- working-set 路径分析只剥离前导内部 `<system-reminder>`，不改变发送给模型的正文。

### 关键测试

- `forkguard_runtime_loader_ignores_ambient_project_authority`
- `forkguard_explicit_skills_dir_excludes_ambient_workspace_sources`
- `forkguard_instruction_fragment_preserves_explicit_host_budget`
- `forkguard_working_set_ignores_leading_system_reminder_paths`

## 7. T4 — Automation 与运行生命周期

### 保留内容

- 每个 Automation 的 id 作为稳定 `conversation_key`；每次 run 仍是独立 Task。
- Task writer 使用上游 v3；reader 仅额外接受历史 Pinvou v4，v5 及以后 fail closed。
- `ThreadCreated` 在 turn 链接前持久化真实线程；`ExecutionTask` 只公开宿主所需 getters。
- 离线超过 60 秒的 recurring slot 不补跑；存在 queued/running attempt 时不重叠，直接推进到首个未来 slot。调度去重刻意扫描保留期内的完整 run 历史，而非只看最近一页，避免同一 scheduled slot 或较早 active run 被较新的终态记录挤出窗口后重复执行；有限保留策略约束扫描成本。
- 终态 run 清理对应 terminal Task；删除 Automation 不复活记录，也不遗失已经 enqueue 的 run。

### 关键测试

- `forkguard_automation_enqueue_preserves_settings_and_conversation_owner`
- `forkguard_scheduler_skips_offline_backfill_and_overlapping_runs`
- `forkguard_once_schedule_missed_while_offline_enqueues_exactly_one_run`
- `forkguard_accepts_legacy_v4_but_rejects_newer_task_schema`
- `forkguard_terminal_task_delete_refuses_active_and_is_idempotent`
- `forkguard_terminal_automation_run_delete_refuses_active_and_is_idempotent`

## 8. T5 — 会话全保真导出归档（追加减量主题）

### 保留内容

- `session_export` 模块 + `codewhale sessions export <id> [--output] [--compression 0-9] [--skip-artifacts] [--force]` CLI 子命令（#51）：把 `SavedSession` 全量（standalone system prompt、含 thinking/tool_use/tool_result 的全部轮次、branch journal、hydration 后的 approval receipts）流式打包为 `.tar.xz`（liblzma 静态压缩，vendored 镜像 mimalloc 先例）。
- 归档布局（格式版本 1）：`session.json`（`/load` 全保真恢复）、`container.json`（版本容忍 `/resume` 导入对话层）、`artifacts/**`（仅常规文件；符号链接跳过防跨界读取；成员按记录尺寸有界读，导出中途缩水则整次失败而非零填充错位）、`manifest.json` 最后写入。
- 输出经同父目录临时文件 + rename 原子落盘；CLI `--force` 才覆盖已存在目标。内容刻意不脱敏（属主完整日志，与分享向且脱敏的 `/export` Markdown 互补），模块文档与登记写明该区分。
- 上游化意图：归档格式、CLI 子命令与测试为通用基础能力，作为整体推上游后消除 drift。

### 关键测试

- `forkguard_session_archive_export_roundtrips_full_context`
- `forkguard_session_archive_includes_artifacts_and_respects_skip`
- `forkguard_session_archive_rejects_artifact_shorter_than_recorded_size`
- `session_archive_replaces_existing_output_atomically`
- `session_archive_rejects_out_of_range_compression_level`

## 9. T6 — 蜂群限流自适应调度（追加减量主题）

### 保留内容

- `DynamicGate` 取代固定容量 `Semaphore` 作为 launch gate（#43）：容量可运行时缩放（允许低于活跃持有数，超额持有者自然跑完），许可经 oneshot 预计数交接，取消安全的两条路径（未授予陈旧队列条目由授予方跳过、已派发许可由 Drop 再释放）杜绝丢唤醒。
- `RateLimitGovernor` 共享于 engine 并经 `SubAgentRuntime` 派生树继承（manager 单一 spawn 咽喉点盖章）：60s 滑窗内 ≥2 次限流事件或 >30% 比例（带 attempts≥2 体积守卫）容量减半，≥4 次暂停新发射（容量 0，在飞不受影响）；每 3 次连续成功 +1（AIMD）；暂停只能由窗口排空解除，外部限流变更不可静默解除；队列侧 5s 周期 `recover_if_window_drained` 时间驱动探活防「在飞全跑完、无成功信号、冻结到墙钟」。
- 429 重试尊重 `Retry-After`，无则 250ms 起 120s 封顶的全抖动指数退避防同源惊群；`QuotaExhausted` 不当瞬时限流，保留既有致命/checkpoint 路径；治理器只观察不延迟在飞调用。配置的 launch_concurrency 经治理器应用（`update_runtime_limits` 立即生效于活 gate）。
- 配套消费方为父仓蜂群模式（PR #444）；治理器本身为嵌入方通用能力。

### 关键测试

- `forkguard_rate_limit_governor_pauses_and_time_recovers_after_window_drains`
- gate cancel-before-dispatch 与多任务 abort/pause 压力测试（限时排空回满容量，丢唤醒/许可泄漏/陈旧条目吞槽均红）

## 10. T8 — roster 列出宿主呈现的 prompt-only profiles（追加减量主题）

### 保留内容

- 底座 `agent` 工具的 `action=roster` 在既有内置 `members` 之外，额外列出引擎 fleet 名册中宿主呈现的 prompt-only profiles：新增增量载荷键 `host_profiles`（按 member id 排序，上限 48）、`host_profile_count` 与 `host_profiles_truncated`；内置 `members` 的既有键保持不变。
- 无分页面上的发现通道：roster 广告化 `profile_query` 关键词过滤（roster 作用域、大小写不敏感子串匹配 member id 与展示描述，在截断前生效），48 条上限外的宿主 profiles 可继续被发现；`host_profile_count`/`host_profiles_truncated` 如实描述过滤后的集合，空白或非字符串输入不过滤。广告 schema 由此 12→13 字段（`SUBAGENTS.md` 已同步）。
- 动机：底座契约本就允许宿主呈现 profile id（嵌入方经 `fleet.profiles` 配置注入），但没有面向模型的发现通道，注入的 profiles 因此不可被模型发现；roster 扩展补上这一通道，模型经 `profile=<member_id>` 直接选定；profile 池超过列表上限时，`profile_query` 保证截断尾部仍可发现。
- 配套消费方为父仓蜂群模式二期（一期为父仓 PR #444）；roster 增量载荷为嵌入方通用能力，适合上游化。

- 落位：本主题原以五个提交随父仓蜂群二期 PR 携带——`8b132a182`（roster 增量载荷键 `host_profiles`）、`9322fc5f6`（广告化 `profile_query` 发现通道）、`df5df5f24`（评审钉点补测）、`0576deb2c`（fleet 宿主 id 按 `get()` 解析键确定性去重）、`4767c2816`（`load` 在 `merge_member` 之前的同键去重对齐）——#65 经 squash 合入为 `c5fadb86d`，波次 SHA 不再单独公开可达（第 13 节教训登记）。

### 关键测试

- `agent_roster_lists_host_presented_profiles_spawn_resolves`（roster 列出的 member_id 与 `resolve_spawn_role_with_host_profiles` 接受的 `profile=` 逐字配对）
- `agent_roster_never_lists_ids_that_spawn_a_different_profile`（大小写/空白变体 id 碰撞：列表不得广告解析到他人 prompt 的 id，每个展示 member_id 均解析回自身）
- `fleet::roster::tests::from_host_config_collapses_ids_that_get_resolves_together`（`from_host_config` 按 `get()` 解析键确定性去重、保留首中成员）
- `agent_roster_truncates_host_profiles_at_the_listing_cap`（50 个 profiles 截断到 48，`host_profile_count`/`host_profiles_truncated` 如实上报）
- `agent_roster_profile_query_discovers_members_beyond_the_listing_cap`（50 个 profiles 截断到 48 后，`profile_query` 按描述与 id 关键词发现截断尾部的第 49+ 个成员、零匹配与空白输入如实上报、发现 id 经 `profile=` 可解析）
- `agent_tool_schema_bounds_fields_by_explicit_action`（roster 分支广告 `profile_query`）
- `agent_roster_action_and_spawn_resolve_the_same_roles`（无宿主 profiles 时空列表形状稳定，内置 `members` 键不变）

## 11. 父仓适配边界

- `pinvou3-app` 负责产品工具白名单、AppMode 到 approval/trust 的映射、reasoning effort、会话 owner 过滤和定时会话创建。
- bridge 保留 v0.9.12 的有限 `max_steps` 轮次预算、read denylist、bubblewrap、MCP OAuth、goal loop 与 telemetry 安全默认值；产品构建不再叠加基准专用的每轮工具上限（见 §2 的 2026-09-11 行），runaway 防护由底座的步数/墙钟/取消边界承担。
- 自 2026-09-15 起，宿主把每轮模型步数预算统一为 2,000（`HOST_STEP_BUDGET`）：主 turn 默认值不再跟随底座 `EngineConfig::default()` 的 200；`build_dt_config` 向 `[subagents] default_max_steps` 注入 2,000（等于底座 `MAX_SUBAGENT_STEPS` 硬上限，显式正值亦被钳制到同一上限；显式 `max_steps: 0` 仍是底座的无界哨兵，宿主不改写工具参数）。`advanced.max_steps` 仍可显式覆盖主 turn。GUI、CLI 与 benchmark 共用 `build_engine_config`/`build_dt_config`，由 `engine_config_defaults_to_unified_step_budget_and_respects_override` 锁定。这是 fork 侧默认值（非宿主强制的硬上限），属登记的 fork 独立行为。
- `session_id` 必须在 `Engine::spawn` 前进入 `EngineConfig`；不得事后依赖事件猜归属。
- 旧的全局 disabled-skills 调用已删除；包开关通过显式 bundle/registry 和每会话 disallowed tools 生效。
- Community 回流扫描、依赖调度、跨平台 preflight、PR provenance、Merge Queue 策略及其
  三平台 CI 门禁。这些属于父仓治理基础设施，不修改 CodeWhale gitlink 或 fork 行为；由
  `community-backflow`、CI 策略测试和 `fork-guard.sh --fast` 的父仓联动检查守护。
- CI 守卫回流批次（父仓 PR #1518）：`fork-guard` 指纹补齐开源侧 12 条行为 pin——T2 computer-use zoom 子栅格换算（#57/#72 主题）、T6 DynamicGate 取消授权重派回归（#43/#55 主题）、T7 压缩检查点角色兼容回归 ×10（见文首主题，其中 4 条为无 `forkguard_` 前缀的上游原生测试名，仅入指纹层：`restored_completion_does_not_join_an_ordinary_user_turn`、`restore_keeps_a_user_turn_that_quotes_the_summary_header`、`restore_replaces_a_pre_provenance_carrier`、`compaction_checkpoint_is_never_the_edit_target`）——并采纳 `test-registry-guard.py` 接入 pr-check rust-test 腿（防 `#[test]` 重复注册互吞，以完整 libtest 路径钉 12 个 consent/rollback 关键回归——跨模块同名顶替不通过；`#[ignore]` 检出自 `src/lib.rs` 模块图（外链 `mod` 与 `#[path]` 接线，bin 根不进扫描）起扫、按 inline mod 栈以完整模块路径归属，同文件兄弟 inline module 的同名 ignored 不再误伤真实 pin，词法不可信（括号失衡）文件按函数叶名失败关闭；属性↔函数（及 `#[path]`↔`mod`）绑定判定统一跑在去注释词法文本上——注释既不能伪造命中（块注释伪 `#[ignore]`/伪函数不可见）也不能切断真实绑定（属性与 fn 之间的注释行按 rustc 语义保持附着），且转义字符随反斜杠整体擦除（残留转义引号会被属性解析误读为未闭合字符串而吞掉绑定函数，round-10 评审）；`#[path]` 的路径值只从词法状态确认的真实字符串 token 读取（属性注释里的引号不参与取值，round-10 评审），值不可靠解析时显式报错退出而不静默跳过被接线模块，字符串未闭合使文件进入词法不可信集合按叶名失败关闭；双向回归、注释化接线回归与转义/注释取值回归入树（round-9/10 评审）；feature set 对齐闭源 rust-test 腿的 `--no-default-features`，两条 `#[cfg(unix)]` pin 在 Linux/macOS 全量命中（守卫接线在 Linux rust-test 腿，Windows 本地运行会缺这两条属预期））。另新增 `check-lock-registry.py`（fast-gate 每 PR 必跑：package-lock 的 resolved 禁止 npmmirror 镜像，lockfileVersion 须为 3，team-relay-hub 锁的 16 条 `@img/*` libc 精确映射（含 glibc/musl 取值）缺失、漂移或换值即红——npm 10 重写 lock 会静默剥掉该字段，glibc/musl 换值会装错 ABI）。round-11 评审加固：`#[path]` 值读取按行推进（值换行或本行被注释占满时原实现无限重扫同一行挂死守卫进程），raw string 值端到端接线（strip 层 raw string 开引号随前缀整体擦除——残留引号会被属性解析误读为未闭合字符串，其后的 `#[ignore]` 检出不可见），静态扫描边界（宏生成 `#[ignore]`、嵌套 inline module 内 `#[path]`、raw-ident `mod` 不进扫描）已在守卫 docstring 文档化；`check-lock-registry.py` 镜像黑名单扩展至 taobao/tencent/huawei/cnpm 并大小写不敏感（原仅匹配 npmmirror 子串）；gate-policy 侧补 Windows runtime-manifest 契约步钉、`--locked` 计数与 zram 断言改按可执行行（注释抵消攻击失效）、锁守卫 submodule 检出前位置钉；deny.toml 以 `unused-ignored-advisory = "deny"` 令 zip@8.6.0 例外失效即红（摘除协议自强制），CI 两侧 cargo-deny 固定 0.20.2（PackageSpec 例外路由随版本可能漂移）。round-12 评审加固（2026-10-17）：`#[path]` 接线扩展到 inline module 内（`mod outer { #[path = "real.rs"] mod routed; }`，rustc 解析规则——顶层按声明文件目录、inline 内按外链同款 child_dir+inline 栈目录——经 rustc 实证），消除该形态下 `#[ignore]` 漏检假绿（round-11 曾把该缺口以正向测试钉为预期行为，本轮翻转）；`#[path]` 与 `mod` 声明之间的其他属性（惯用 `#[path]`+`#[cfg]` 平台接线形态）不再断开配对（路径值在属性 run 内跨条目保持 pending）；rustfmt 换行的泛型签名（参数 `(` 落在次行）不再静默漏检（前瞻在首个 `{`/`;` 处止步）；同行内联模块的外链 `mod`（`mod s { mod x; }`）按令牌时刻栈解析，不再错置目录；main() 新增必需 pin 源归属失败关闭网——已注册但其源不在扫描图内的 pin（未建模接线/宏生成/raw-ident）显式报红而非信任注册表；`check-lock-registry.py` 镜像黑名单补 bare-domain npmmirror 及 aliyun/ustc/163/tuna 并大小写不敏感，npm libc 序列化阈值精确为 ≥11.11.0；deny.toml 的 zip@8.6.0 PackageSpec yank 例外随上游 2026-10-17 全量解除 yank 移除，改为按 id 忽略 RUSTSEC-2026-0367/0368/0369（均仅修于 zip ≥9，calamine 0.36.1 钉 zip ^8.6 不可升；unused-ignored-advisory=deny 令每条失效即红）；gate-policy 补 cargo-deny 四步钉（安装版本 0.20.2 + advisories 检查命令）、manifest 契约步补 `if:` 禁令、`--locked` 计数改按去注释代码行；validate-commit-msg 浅克隆边界 merge 提交经 cat-file raw object 读亲本（%P 在浅边界被 graft 为空，GitHub 自动 merge 提交不再误校验）；wrapper 链式冒烟探针改逐参独立断言（argv 合并参数变异不再逃逸）。round-13 至 round-18 评审加固（2026-10-17/19）：外层 inline 模块自身的 `#[path]` 按 rustc thread_files 规则重定向嵌套声明的解析目录（未接线残留同名文件不再伪造归属）；cfg 互斥同名内联声明的目录冲突按失败关闭退出并给出两处声明位置（单槽顶替废除），non-mod-rs 文件内联模块自身 path 的解析基址回退声明文件目录（清除 pending stem）；同名声明的目录覆盖改按（物理基址, 文本）对比较，显式 path 声明改按（模块名, 声明行）从常规候选排除（round-18）——cfg 互斥的普通孪生声明保留常规探测，rustc 在另一平台实际加载分支中的 `#[ignore]` 不再漏检；同一物理文件在多个模块上下文逐一入图，模块环按 rustc 同款报错失败关闭，守卫位置断言平台原生精确比较。round-19 评审加固（2026-10-23）：扫描 visited 上下文升级为（物理文件, 模块路径, 目录 ownership）三元组——cfg 互斥 wired/plain 孪生声明在同一模块路径下抵达同一物理文件时按两种 ownership 各扫一遍（FIFO 竞争不再丢弃 rustc 在另一宿主实际加载分支的子源解析，任一候选源的 `#[ignore]` 即红，评审四控 0/1/0/1→1/1/0/1）；显式 path 排除的（模块名, 声明行）身份对同行孪生声明按失败关闭拒绝——同一物理行同名声明数超过 `#[path]` run 可接线数时接线方无法区分（rustfmt::skip 同行布局），SystemExit 给出行号与拆行指引，rustfmt 默认输出永不触发（评审 wired-live/plain-ignored 假绿闭合）。round-20 评审加固（2026-10-23）：属性 run 与 `mod` 声明的绑定盲区按失败关闭收口——`#[path]` run 收行与 `mod` 声明同行（rustfmt::skip 手工 packed 布局）、路径值包裹于 `#[cfg_attr(...)]`（谓词不可静态判定）、路径属性头出现在代码之后的中行位置、`mod` 关键字的名/终结符跨行四种形态一律 SystemExit 拒绝（此前 packed 布局不产生 wired 条目，round-19 拒绝网与按声明排除均不生效，常规位同名文件伪造 pin 归属假绿，维护者四控 0/1/0/1 闭合）；`#` 与 `[` 间空格（`# [path = ...]`，rustc 词法合法）与 UTF-8 BOM 前缀改按普通接线建模（原 head 正则/行首探测静默丢弃接线）；gate-policy 补 rust-lint 磁盘步精确配方钉（此前仅钉命令存在，`--min-free-gib 1` 变异全绿）、smoke 链式探针五条断言全钉（原仅钉 2 条，argv 内容删除不红）、deny.toml `unused-ignored-advisory = "deny"` 可执行行钉、全仓自有 Cargo.lock 的 zip 8.6.0 精确钉（id 级忽略无版本维度，带内漂移曾静默通过，升级强制重评估）；锁守卫失败文案收敛为镜像黑名单语义（原措辞超出实现的官方 registry 白名单强度）并更正注释里的历史计数；clipboard-win 5.4.1 随上游 yank 升至 5.4.3（`yanked = "deny"` 硬门禁按设计如实触发，deny 四项本机复跑全绿）。round-21 评审加固（2026-10-23）：pinvou-cli 自有 Cargo.lock 同步升至 clipboard-win 5.4.3（此前仅 src-tauri 一锁 bump，同款 yank 修一处漏一处；pinvou-cli 无 deny 腿，该锁此前无任何门禁可见 yank 状态）；锁守卫补结构性 libc 规则——`os=["linux"]` 而 `libc` 缺失的 @img/* 包一律红（冻结表只见其所列包，sharp 未来新增 linux 平台包且被 npm<11.11 重写剥字段时会从集合差旁路漏过；npm 从不剥 `os`，故该二元组即剥字段签名）+ registry-guard 补 `-- --list --format terse` argv 尾与「仅 `<path>: test` 行入册」解析契约四测（此前 m9/m9b 变异全仓套件存活——main() 级测试整体替换 list_tests、唯一 subprocess 测试喂空 stdout）。round-22 rebase 加固（2026-10-24）：main 自 #2087 后前进至 31a84e23a8（#2106–#2113 回流波，community#761 已把三 workspace 收敛到 calamine 0.37 + 单一 zip 9.0.5 并退役 RUSTSEC-2026-0367/0368/0369 id 忽略、clipboard-win 已自行升至 5.4.3）——deny.toml 冲突消解为 main 现行 posture + 本 PR 的 `unused-ignored-advisory = "deny"` 保留（对新唯一的 RUSTSEC-2026-0387 crypto-bigint 临时例外继续自强制；本机 cargo-deny 0.20.2 新鲜 DB 四检查绿 + 植入失效 zip 例外变异实测 advisories FAILED）；gate-policy 的 zip 锁精确钉自 8.6.0 重定基为 9.0.5（main 退役 id 忽略的前提即「图内单一 zip 9.0.5」，带内漂移仍强制同提交重评）；两枚 clipboard-win bump 提交与两枚 yank 期 deny 伪影提交按「内容已在主线/被最终态吸收」自动或显式丢弃（重放链 51 提交，加本轮 zip 钉重定基新提交后分支 52 提交）；20 个差异文件中 16 个与 rebase 前逐字节一致，pr-check.yml 本 PR 变更行逐字节一致（差异全部为 main 侧新增内容引起的上下文偏移），deny.toml、test_ci_gate_policy.py 与本文档为本轮刻意改动。round-22 从零复审修复（同日）：registry-guard 补中行 ignore 头拒绝——run 扫描只从行首起 run，`rustfmt::skip` 手工 packed 布局（`mod tests { #[ignore]`）或任意代码后随行的 `#[ignore]` 头对扫描器不可见，而 fn 自身次行 `#[test]` 仍可归属，ignore 检出与归属失败关闭网均不触发（L1/L15 双 lane 在真实 REQUIRED_PIN 上 exit 0 亲证，rustfmt --check 稳定该布局）；两处静默跳过位点（外层 run 闸与内层 run 收行 break）对携带 ignore token 的中行头显式 SystemExit（按 ignore 限定，round-19 中行 `#[cfg]` 孪生声明的歧义拒绝路径不变），四种形态回归+正反控入树；gate-policy 的 rust_full 路由钉自字面 assertIn 升级为行为回放（dorny some-with-excludes 下追加 `!path` 负向条目曾全绿绕过守卫路由——变异亲证），锁守卫/注册守卫/manifest 三步与 smoke 链式探针步改精确整行钉（echo 中和类变异此前全绿）；deny.toml [bans] 补 crypto-bigint 漏洞窗口封禁（`>0.5.5, <0.7.6`，id 级 RUSTSEC-2026-0387 忽略无版本维度，半升级形态曾静默绿）并加 gate-policy 钉；锁守卫 14 处计数口径与注释三处同步更正。round-23 复审修复（2026-10-24，维护者 round-20 起两项 open P2 + 本轮同类洞）：registry-guard 接线侧「入图」三洞闭合——中行 `#[cfg_attr(pred, path = "x.rs")]` 头与直接 `#[path]` 头同样拒绝（原中行探测只匹配直接头，run 级 cfg_attr 拒绝对非行首行不执行，常规同名文件顶替接线源：维护者四控 0/1/0/1）；`#` 与 `[` 跨行拆分的属性头（rustc 合法分词）在词法层重接后重扫，path 接线与 `#[ignore]` 该写法均入图（原逐行探测全部漏过）；共享 run 收行的普通内联 opener（`#[cfg(windows)] mod outer {`）入 `_record_inline_decl`（原未记录，cfg 互斥 `#[path]` 孪生的目录覆盖静默重定向本分支子模块，另一宿主真实源的 `#[ignore]` 漏检）；Round23WiringEntryTests 四测对修复前守卫全红，真实树扫描逐字节一致。锁守卫：结构性规则改按完整包路径记账（嵌套重复表包剥 libc 不再被顶层副本遮蔽）、`os` 字段归一化（列表/字符串/大小写）、非字符串 resolved 作结构化失败而非 traceback、重复表包 libc 值冲突报错。gate-policy：yanked/unused-ignored-advisory 钉改整行精确且唯一；lock/manifest 步 `if:` 禁令按去注释文本；deny 两步 `if:` 钉改折叠感知等价比较（`>-` 语义等价不再误红）；registry-guard 步补「位于产物回收步之前」位置钉；rust-lint 磁盘步余量门对齐 rust-test 的 24 GiB 并钉精确配方；smoke 链式探针补退出码透传断言（CHAIN_PROBE_EXIT=42 经 wrapper 原样穿出，sh/rs 本机双验）并钉整行。validate-commit-msg 套件补真实 depth-1 克隆端到端（浅探测与 raw 亲本读取不再仅被 mock 覆盖）。均为父仓治理基础设施，不修改 CodeWhale gitlink 或 fork 行为。
- Shell 任务对账优先使用快照与完成事件携带的稳定 `origin_tool_call_id`（上游 v0.9.12 行为，Hmbown/CodeWhale #5869）：host monitor 与 Tauri/Web 桥优先回写来源工具卡，仅对无来源旧任务按命令文本回退；来源卡被压缩或重载清除的已识别终态根任务不追加到当前时间线尾部，运行中任务保持合成状态卡可见（`shell_task_projection.test.mjs`、`forkguard_shell_monitor_assigns_identical_commands_by_stable_origin`）。
- 来源范围语义：shell 任务的 `origin_tool_call_id` 是产生它的唯一 root 轮内工具调用；子智能体任务只携带 `owner_agent_id`、来源为空，走无来源对账路径。只读 `multi_tool_use.parallel` 子调用虽共享包装调用的来源，但 shell 工具带 `ExecutesCode`、不能进入只读并行，该共享对 shell 任务不会发生。消费方必须保留 owner 区分，且不得让一个任务抢占已绑定另一任务的卡片。
- 持锁副作用 await 上界：会话 turn gate 持有期间的四类副作用 await 以 `TURN_GATE_AWAIT_TIMEOUT`（5s）为上界（issue #255）——取消阶段二级联 send、shell 清理 join、回收 shutdown send 与 shell reclaim finalize。超时放弃该次副作用并记日志：被放弃的级联 send 因 tokio mpsc send 的取消安全语义未入队，且所有持锁发送方与下一轮 `SendMessage` 串行于同一把 turn gate，不会迟到误杀经 gate 提交的新轮（引擎自治轮不经 gate 启动、由 forwarder 在 `TurnStarted` 时认领，属既有窗口）；超时的 reclaim finalize 转入后台继续收尾并保守预置 cleanup_failed，不会永久卡死会话的 active scope（rebind 与 delete 重置 registry 可解）；panic 的 finalize 如实判为「未结算」并以一次全新的 detached 收尾重试替代（finalize 幂等），终态绝不因收尾中途死亡而谎报已验证的干净 shell 状态——detached 收尾落定前，会话的下一发可能以 scope 未关瞬态失败、落定后重发即成功（rebind 与 delete 会重置 registry，天然免疫）。清理调用不再经 turn gate 串行，重叠的击杀梯次与后台 worker 清扫一律由 registry 级 cleanup gate 串行——否则并发的 `cleanup_scope_once` 会对 `PendingKill.attempts` 双计数、在任务尚在退出时耗尽 `MAX_KILL_ATTEMPTS` 永久停止重试。回收 shutdown send 超时后，未送达的收尾 op 由不持 gate 的 detached 任务按原 FIFO 顺序重试补投（每个 op 以 60s 为上界）：engine 自持有 tx_op 克隆、entry 摘除不会关闭其 ops 通道，run loop 只能经正常 `Shutdown` 路径退出，不补投则引擎任务存活到进程退出；重试亦超时则放弃并记日志，此时引擎任务滞留是持续卡死本身的结果，delete/evict 已先行返回时，滞留引擎迟到的终态落盘会在已删除的会话目录下留下孤儿文件（无害：会话列表只读顶层 `<id>.json`，不会出现幽灵会话）。提交侧的持锁等待亦已上界（issue #525，#255 残余的发送半边）：用户/评测/重发消息的提交 send 以 `TURN_GATE_AWAIT_TIMEOUT` 为上界，超时即以干净错误令预约失败（tokio 有界 reserve/send 的取消安全语义保证超时的 op 未入队、轮次确定未启动；transcript 规则随预约 Drop 回滚，已准备的 shell scope 由其自身 Drop 弃置），向已卡死引擎发新消息不再经 gate 重新阻塞 evict/delete；spawn 失败拆除的 Shutdown send 同受上界，未送达的 op 交由不持 gate 的 detached 重试补投；reclaim 终态落盘 join 受上界，超时终态如实预置 Failed（未经核实的落盘不得谎报干净，detached 落盘仍可稍后落定）；forwarder abort-join 受上界，超时 detach 放任（forwarder 不持 gate/pool/会话表状态，无法再阻塞 evict/delete）。仍无界的持锁等待不止 scheduled 轮的整轮 gate 持有与其无界 send（既有设计，#525 明示不在范围内）：冷 spawn 时在 gate 下物化会话 skill 的 spawn_blocking join、以及每次提交经 `prepare_turn` 读取 shell 基线任务清单的 spawn_blocking join 同样不经引擎通道，暂依赖磁盘/互斥锁健康假设（后者仅对锁健康敏感），后续可按同一上界收口。行为测试 `forkguard_cancel_holds_turn_lock_boundedly`、`forkguard_bounded_join_reports_panicked_task_as_not_settled`、`forkguard_bounded_join_detaches_task_that_outlives_budget`、`forkguard_reclaim_shutdown_sends_bounded_and_retried`、`forkguard_reclaim_shutdown_retry_gives_up_within_patience`、`forkguard_reclaim_cleanup_failed_preset_clears_on_success`、`forkguard_background_cleanup_sweep_takes_cleanup_gate`、`forkguard_reserved_submit_send_fails_cleanly_on_wedged_engine`、`forkguard_spawn_failure_teardown_shutdown_bounded_and_retried`、`forkguard_reclaimed_persistence_join_detaches_task_that_outlives_budget` 由 fork-guard 第 3 层执行。

## 12. 软上限评估与后续减量

历史净增 17336 行（该数字测量于 2026-09-22/23 批次波次头 `4767c2816`——该波次头现已不在公开 refs 上，沿开源仓同文口径沿用；登记头 drift 现值为 `445 files, +67393/-21049`、净增 46344（`13ee4dfd2`，见 §0），`252 files, +36785/-5754`、净增 31031 为 `0bc868ff0` 时点值）；当前净增以第 0 节登记为准，超过 1500 行软上限；`engine.rs`、`turn_loop.rs` 和 `engine/tests.rs` 也超过单文件 200 行提示线。原因是状态机、最终分发安全、“完整专家人设仅进入被选子智能体”的 spawn 边界、bundle Skills 排除 ambient 文件源的权限边界，以及评审要求的结果式生命周期/评测回归必须和 v0.9.12 原生 Engine 同步，拆成 app 侧镜像会形成更危险的双状态源。当前 Rust/Clippy/rustdoc 兼容只包含等价重写、`Default` 补全、窄 lint 说明、文档可达性修复和无调用测试 helper 清理，不改变公开函数签名或新增运行语义。

后续减量顺序：

1. 将可靠 steer lifecycle 与逐轮 exact/read-only dispatch 作为通用 host API 上游化。
2. 将 Automation misfire/no-overlap/conversation ownership 上游化。
3. 将 execpolicy phase-2 表达力（斜杠旗标/通配符/`.exe` 折叠/绝对路径）与 subagent 接线作为通用 deny 语义上游化。
4. 将 T5 会话归档导出（格式 + CLI + 测试）作为整体上游化。
5. 将 T6 限流治理器作为通用 fan-out 调度上游化。
6. 将 GLM-5.3 forced-thinking 方言与 BigModel 路由谓词随 provider 方言族上游化。
7. 将 `FragmentId::Permissions` 的显式 100 KiB 宿主预算做成通用的可配置 fragment limit，上游化后删除固定例外。
8. 父仓先迁移到明确 re-export API，再分批收窄当前 18 个 `pub mod` 兼容 facade。
9. 上游提供完整 static-composer/explicit-skills-root 契约后删除 T3 patch。
10. 每次上游 release 重新核对已吸收项，不保留兼容壳。

### 有意分歧登记

以下各项是父仓（闭源仓）相对开源仓的有意行为分歧：双方可共存、不构成同步缺陷，也不进入上述减量顺序；每项登记分歧内容、取舍口径与钉定位置，避免后续同步或回流时被当作漂移“修复”。

1. 行为/设备遥测：开源仓经社区决议（Pinvou/pinvou-agent#616）移除 opt-in telemetry client；闭源仓按企业口径保留该客户端与设备运营统计链路（`pinvou3-app/src-tauri/src/features/behavior_telemetry.rs` 为 #616 对应的 opt-in client，`pinvou3-app/src-tauri/src/platform/telemetry.rs` 为设备登记/用量统计，设计见 `docs/设备运营统计-初版.md`），随行的 access-policy 差异一并保留。此分歧属发布面差异，不回流、不上游化。本轮在父仓扩展双端远控观测：`features/behavior_telemetry/remote_execution.rs` 保留受授权代际约束的本地上下文，以实际执行回合绑定模型/工具观测；启动恢复、关闭和删除沿用原采集边界。Relay 的 `remote-control-relay/remote-telemetry.js` 按匿名任务键归并 A/B 事实，并复用已有贡献裁剪与被动端活跃排除，详见 `docs/remote-control-telemetry.md`。这些改动均在 app/Relay 层，不新增 CodeWhale 底座分歧，不改变 gitlink 或引擎指纹；行为覆盖由独立遥测测试及 Relay 聚合回归承载。
2. 语音 closeVoice/dismissVoice 语义分化：闭源对完成态语音通知的 dismiss 保留草稿 provenance、仅进行中录音视为显式取消，与开源的 dismissal-sweep 时序不同；该时序适配由 `pinvou3-app/tests/voice_operation_lifecycle.test.mjs` 钉定，不随开源实现回移。
3. （已退役，2026-10-23 #701 对齐 T4b 改写）原登记「开源 bridge shared-cluster 机制本仓无此文件、闭源有意不采纳」已与现状不符：本仓自 #2040 起持有 `pinvou3-app/src/shared/bridge-shared-helpers.js`，#701 对齐（T0–T4b）后桥接面已全面对齐上游 shared-cluster 形态——共享函数以 helpers 单源 + 两 lane 委托壳为准，不再按「逐 lane 内联镜像」口径同步。该条不再作为分歧保留，同步以共享簇为准；仅 §12 其余条目登记的 lane 级 F 面（遥测、llmApi 闭源服务面、桥消息谓词等）继续按各条口径保留。
4. wire DTO 闭源超集：闭源 wire DTO 字段为开源面的超集，已在 Pinvou/pinvou3#463 登记，以该登记为准，此处不重复展开。
5. settings 簇 llmApi/行为遥测/分析供给 39 函数为闭源服务面（#701 对齐 F 登记补录，原 docs 全树无账）：`tauri/bridge/settings.js` 与 `web/bridge.js` 的 llmApi 账号/供给/管理面（`refreshLlmApi`/`refreshLlmApiOnStartup`/`prepareBackendManagedModel`/`loginLlmApiUser`/`saveLlmApiUserSession`/`retryLlmApiProvisioning`/`setLlmApiUserEnabled`/`getLlmApiAdminOverview`/`llmApiBackendUserState`/`applyAuthoritativeLlmApiError`/`selectedModelIdForSession`/`modelAvailabilityError`/`observeInvokeError` 等 ~22 函数）、行为遥测 5 函数（`loadBehaviorTelemetryStatus`/`setBehaviorTelemetryEnabled`/`behaviorTelemetryControlPending`/`behaviorTelemetryStatusNeedsPoll`/`scheduleBehaviorTelemetryStatusPoll`）、分析/提供商面（`analyticsOperationId`/`analyticsProviderKey`/`hasConfiguredUserModel`/`trackProviderSetup`/`testSearchProvider`/`getEffectiveModelConfig`）及 `saveSettingsAndRestart`/`saveSearchSettingsAndRestart`。上游无对应物（配套 Rust `features/llmapi_hub/`、`features/behavior_telemetry.rs` 同属此面），对齐批次不得跟删；tauri lane `loadEffectiveModelConfig` 的 `modelAvailabilityError` 熔断属本面（web lane 不加熔断，两 lane 本就分叉）。
6. web access/relay、remote proxy 会话视图、materializeSession 为闭源必须保留面（函数级点名，防对齐批次误删）：web access/relay 6 函数 `refreshWebAccessStatus`/`enableWebAccess`/`disableWebAccess`/`rotateWebAccessLink`/`getWebRelaySettings`/`setWebRelayAddress`（上游曾两轮删除：#684/#701）；remote proxy 会话视图 8 函数 `isKnownRemoteProxy`/`isRemoteProxyView`/`isListedRemoteProxy`/`readRemoteProxyHistory`/`selectRemoteProxyHistory`/`deleteRemoteProxyHistory` 及同域 2 函数（随 Windows 双端远控文档族）；`materializeSession`（`tauri/bridge/sessions.js`，团队接力等显式工作入口依赖的稳定 session id 物化）。
7. `src/shared/bridge-messages.js` 两谓词为闭源语义必须保留面：`isContextUpdateRuntimeMessage`（CodeWhale prompt-cache delta 以 user-role 持久化的识别，`bridge-messages.js` 与 `shared/internal-message.mjs` 镜像）、`isKnownInternalMetadataBlock`（`<turn_meta_unchanged/>`/turn_meta 块识别；官方两 lane `userMessageDisplayText` 以它就地过滤元数据块，helpers/上游版无此过滤——这是 persistMessagesFor/userMessageDisplayText 官方 lane 本体不壳化的原因之一）。上游全史无此两符号，T7 压缩检查点主题不覆盖其点名保留。
8. #2030 披露的 Rust/前端保留面（原仅存 PR 正文，现落 docs 为持久 registry）：①`discard_aux_session` 全链（Rust 命令+RPC opcode+allowlist；JS facade `auxChatDiscard` 已随 #701 X 清理删除，Rust 命令面属后端批次）；②`UserPrefs::save`；③`run_captured_with_timeout`/`CLI_RUN_TIMEOUT`（连接器超时钳制，闭源连接器面不回流）；④engine_pool send 门 + reserved 包装、`load_for_engine_spawn`；⑤`pet_window` 再导出；⑥SettingsView reasoningEffort；⑦icons.jsx 既有导出面；⑧AcpRuntimeNotices i18n 回退；⑨ChatView 工作台引用；⑩`test_ci_gate_policy.py` helpers（main #1973 重写后有新消费）；⑪ima 同意门测试定位（app/connector_consent_tests.rs，上游在 ima.rs）；⑫extraction 内联 `__pycache__` 判定（避免 runtime_bundle→marketplace 环边）；⑬`tests/team_relay_avatar.test.mjs`（官方独有测试，reduced-motion 钉）；⑭acp_providers_contract/protocol_tests 两断言按官方保留面适配。以上均为语义映射保留面、非遗漏。

## 13. 发布与回退

- 公开回退点是不可变 tag `pinvou-v0.9.5-r13`；本地 `backup/pre-v0.9.12-sync` 不是发布前提。
- r3 已收口（2026-09-23）：不可变 `pinvou-v0.9.12-r3` 切在合并头 `61cb769be5b33abc64f64da4272f5b39a8b6c1fd`，`pinvou3-clean` 分支头、tag 与父仓 gitlink 三方相等，以 `scripts/verify-public-submodule.sh` 验证公开可达性；回退点保持 `pinvou-v0.9.12-r2`（钉在其收口 `6f780290f1c35e8a3c5dff86b4f76da142744b0c`）；r3 tag 在父仓引用前因 #77 修复自 `ba2a07768f` 重切；下次过渡期自 r3 起，按 `docs/fork-policy.md` 第 0 节豁免执行。
- r2 已收口：不可变 `pinvou-v0.9.12-r2` 切在合并头 `6f780290f1c35e8a3c5dff86b4f76da142744b0c`，`pinvou3-clean` 分支头、tag 与父仓 gitlink 三方相等，以 `scripts/verify-public-submodule.sh` 验证公开可达性；回退点保持 `pinvou-v0.9.12-r1`（钉在 r1 收口 `1fafee7e2`）；下次过渡期自 r2 起，按 `docs/fork-policy.md` 第 0 节豁免执行。
- 教训登记（2026-10-15）：gh squash 合并自定义 body 只保留主题列表、丢失 sign-off 尾部，使 #107 squash `f27672935` 成为 71 个登记提交中唯一未签者；「全部带签」的失实由 #705 写入、经 #712 延续，跨两个登记批才在 #706 订正。squash 合并必须把 `Signed-off-by:` 尾部写进提交信息；每个登记批必须按 `git rev-list --grep=^Signed-off-by:` 实数签数，不得沿用上一批的计数叙述。
- 发布过程中只为精确 head 的受保护分支更新临时移除无法在该维护分支触发的 required status contexts，完成快进后立即恢复原保护配置；未关闭 force-push 防护，也未重写已发布 tag。
- 教训登记（2026-09-23）：#65 原计划随父仓蜂群二期 PR 保持提交落位合入（不 squash），实际经 squash 合入——曾钉定的波次头 `4767c2816` 自公共 refs 悬空、fast-gate 可达性校验红，蜂群二期分支当日重钉到维护分支头恢复 gitlink=公开分支头相等。后续底座合入一律在合入后立即把 gitlink 重钉到公开分支头，不得钉定合入前的波次 SHA；r3 收口已恢复三方相等，公开可达性继续以 `scripts/verify-public-submodule.sh` 验证。
- 后续发布仍不得降低公开校验或把本地 object 当成发布成功。

### F05 成熟调用身份的宿主承接

本节记录 2026-10-14 当时固定维护提交 `803e672bb87f5ee4e0ab56f1e23fa408ddbeeaa0` 的宿主承接；当前维护头以第 0 节为准。该批次沿用其逐调用身份（原 F05 补丁 `8b5a86390` 的能力已通过 CodeWhale #92 合入），不新增底座协议。账户运行时凭据向真实 `DtConfig.request_idempotency_header` 传递 `Idempotency-Key`；本地模型代理读取并原样转发底座的单次调用 ID，缺失或重复字段拒绝，不能按用户轮次和正文生成替代身份。同一用户轮次的不同模型调用各有身份，轮次统计的 `X-Pinvou-Logical-Turn-ID` 仍是独立分组字段。普通自配模型不启用该标记。该底座历史证据继承不等于新宿主或真实供应商验收通过。

### F05 公开维护头 #107 的宿主限制修复

消费已外部批准并合入的 `f276729350a501153969fc648986b6352ee438f2`（直接继承 `480bebf61`），沿既有 T1 宿主模型路由边界登记。同一 wire model/protocol 保留构造客户端时冻结的宿主上下文/输出限制，避免无宿主覆盖的静态目录重解析降级；跨模型仍重新解析身份和协议，不扩大任意模型的限制。公开提交已有 `model_aware_same_model_request_keeps_host_declared_route_limits`、`rebinding_the_same_model_keeps_host_declared_route_limits` 两条结果式测试；父仓新增指纹绑定这两条回归及真实生产条件，不重复实现底座修复。公开 PR #107 的最终审核头 `6e3c5c58b` 与维护合入提交内容相同；外部批准/公开 check 的结果归属原提交，不冒本机重跑或新父仓原生 CI。维护提交消息未含 DCO 行，因此当前总数表述为公开维护提交数，不声称全部带 sign-off；父仓本次提交仍遵守有效 DCO。不可变 r3 标签和严格 gitlink=公开维护分支头门禁保留。


### F05 公开维护头 #54 的单执行根兼容与 Web 元数据边界

本节记录 2026-10-14 批次：消费当时已外部批准并合入的 `803e672bb87f5ee4e0ab56f1e23fa408ddbeeaa0`（当前维护头以第 0 节为准），直接继承 #107 的 `f27672935`。#54 最终审核头 `be2cd01efd6fb606e19765c7e00c565191ebea56` 与维护合入提交树同为 `31a81e54a288045aca63786288601a98b94b1f8b`；外部 7 项成功 source checks 属于该原提交，不代表新父仓或本机编译已验证。新增能力沿既有 T1 宿主配置/Session 和 T2 工具权限登记，不增加主题。该底座批次实际为 100 个文件、+14904/-945；既包含 workspace-roots 生命周期，也改变单根 operand cwd、symlink 与缺失路径后 `..` 的判断，不能以“单根入口未变”声称底座行为不变。

2026-10-14 当时的父仓适配：`build_engine_config` 的输入仅为 `EngineConfig::default()`，显式传空 `workspace_roots`；普通/项目绑定/计划和多智能体配置只用既有 execution 根，`SessionRoots.ledger` 仍仅提供 `subagent_state_root`。当时 `sync_session` 同样显式空附件，由底座替换为当前 primary 的单根集合，不继承上个 Session 的附件。六个既有 `ExecPolicyContext` 测试补齐空根字段，原 deny、wrapper 和 approval 断言保留；该批次未新增磁盘根登记、multi-root UI 或授权入口。

2026-10-22 集成 main 工作区能力后的宿主接线：默认配置仍为空附件；实际非辅助会话的 spawn、普通同步和发布前冷同步都消费当前会话冻结的 keychain，不把 `SessionRoots.ledger` 当作工具授权。底座 `SyncSession` 会无条件替换根集合，因此冷同步不能再发送空附件清掉刚由 spawn 安装的 keychain。两同步入口共用 `sync_session_workspace_roots`，辅助会话不调用 resolver，旧无快照会话仍按空附件退化为 primary 单根。普通 `send`、冷同步 `try_send` 和公开 entry 的顺序不变；新增两个宿主 resolver/payload 回归，保留 main 的 keychain payload 与本分支轮次所有权回归。Web 逐根脱敏改用等价的完整 `std::path::PathBuf` 名称，并同步指纹；未改 CodeWhale 源码或 gitlink。这些指纹只登记覆盖，隔离函数测试不代替完整原生应用验收。

持久化或导入的 `SessionMetadata.workspace_roots` 仍可能非空。普通 Web/Relay 元数据、Web saved session 和 native-code Web session list 已消费同一 `redact_session_metadata_for_web_in_place`；现在每项复用原 `redact_workspace_path_for_web`，仅传末级目录名。Native 元数据保留原始根集合。父仓行为测试 `forkguard_web_session_projection_redacts_attached_roots_and_keeps_native_metadata` 对 Unix/Windows 非空集合断言实际序列化 JSON、两个 Web 投影、Native 原值和旧空字段省略；`forkguard_engine_config_keeps_only_execution_root` 断言实际宿主配置不纳 ledger，底座原单根物化回归另由指纹绑定，原 scheduled 测试继续断言空附件。

新增 guard 指纹绑定上述生产接线与父仓回归，以及底座 `normalize_workspace_roots_puts_cwd_first_and_dedups`、`normalize_workspace_roots_drops_empty_and_relative_entries`、`sync_session_workspace_roots_ride_the_wire_only_when_present`、`sync_session_replaces_workspace_roots_for_the_next_turn`、`tool_context_for_turn_materializes_session_workspace_roots`、`workspace_write_drops_a_root_that_canonicalizes_to_the_filesystem_root`、`exec_judged_cwd_resolves_absent_interior_behind_symlink`。这些名字及源码指纹是覆盖登记，实际执行结果另行记录；不可变 r3 标签、#92 幂等身份与 #107 三条指纹、严格公开维护头门禁均保留。

## 14. 2026-10-22 #701 桥接面对齐（T0）

- T0-a（纯搬运，本批）：官方 `pinvou3-app/src/shared/bridge-shared-helpers.js` 以下游缺失的 community 合并面对齐。搬运基准取 community `ef98a715ee`（#746 `0baf7fa6c` 的父提交，即 #701 落定、#746/#749 未入的版本）；#746（tool 卡重放索引/toolMeta LRU）与 #749（artifact replay 查找快路径）对 helpers 的全部 hunk 本批不搬运，分别留待 T2/T3 行为 PR。共搬入 77 个共享函数（a1 清单 73 个 C1 + auxChat 簇依赖 + 4 个 C1 消费者强依赖的小函数：`upsertRecentRunRow`、`readDraftWorkspaceRecents`、`writeDraftWorkspaceRecents`、`sessionMentionSplitter`），新增 `auxChat` cluster、`STATE_SLICE_FIELDS` IIFE 静态导出，cluster 导出名单补齐至上游口径；sharedBridgeBase 作用域随搬运补入上游常量 `DRAFT_WORKSPACE_RECENTS_KEY`、`REBIND_RECONCILE_WINDOW_MS`、`PinvouFU`+`fmtMiB`/`fmtKiB`/`fmtDuration`/`fmtTok`。共享函数计数 238 → 315。官方 lane 文件与 index.html 本批零改动（内联残留壳化、lane deps 接线与 `STATE_SLICE_FIELDS` 消费方改指留待 T0-c/T0-d/T0-e）；`FORWARDER_DEP_NAMES` 补 9 个 lane 前向器名，官方 lane 传入的同名 deps 继续覆盖共享实现，运行时行为不因本批改变。
- 同名 body 漂移按 a1 处置：6 处以官方 lane 新版语义修正 helpers 陈旧副本（`isAbsPath` UNC 分支、`queuedPayloadEnvelope` block-aware fallback、`rebuiltQueuedPayload`/`rebuiltQueuedMetaPayload` 委托 `rebuiltQueuedFromEnvelope`、`applyDeletedSession` listener fan-out、`clearVoiceInput` 补 `abandonCompletedVoiceResult("recognition")` 并保留 cell `.value` 形态）；`mergeScheduledTaskRecentRuns`、`loadScheduledTaskRecentRuns`、`loadSettings`（#468 computer_use 主开关冷启动回填，`state.computerUse` 判空守卫）以上游为准。
- F 保留（裁决项，T0 期间一律保守保留）：① `personaName` 的 `cn_name` 兜底保留官方版，不随上游删除；② tauri lane `loadEffectiveModelConfig` 的 `modelAvailabilityError` 熔断（`tauri/bridge/settings.js` 与 `settings.js` 可变槽位）零触碰，helpers 新增的是上游状态写版，T0-c 壳化时必须作为 lane 差异保留，否则官方设置页熔断回归。官方独有 12 符号（X/F：compactNow、markResolved、dismissVllmSetup、getEffectiveModelConfig、clearVoiceInput 及 7 个导出面残差）本批不删，清理留待 T0-c。
- 测试面：`bridge_domain_protocol.test.mjs` 的 `expectedSharedBaseHash` 随 helpers 扩容重算（`4e63e15a…` → `96fda42a…`，仅换期望常量并注明新增 10 个 invoke 命令面）；`scheduled_tasks_unit.test.js` 的 `scheduledTaskBackendInput(input)` 计数钉 helpers 1 → 2（createScheduledTask/updateScheduledTask 均入 helpers，官方两 lane 本地副本壳化前保留）。全量 `npm run test:node` 与基线持平无新增失败；helpers 文件 eslint 0 error（warning 621 → 1003，+382 全部为上游正文逐字带入的风格/复杂度告警，T0-c 壳化后预计回落）。

### 14.1 T0-b（合并簇抽取，本批）

- 抽取基准与 T0-a 一致（community `ef98a715ee`，#746/#749 未入）。11 个 C2 合并簇中 6 个符号随本批进 helpers（全部与上游正文逐字一致，函数级 diff 校验通过）：`rerenderFromMessagesCore(lane, opts)`、`liveChatItemKeptForHydration`、`resolveMemoryCandidate(command,args,…)`、`loadPersonaEventsForSession`、`loadPinvouReviewsForSession`、静态面 `buildAssistantDoneTimelineRecord`（挂在 `root.PinvouBridgeShared`，与 `STATE_SLICE_FIELDS` 同模式）；共享函数计数 315 → 320（另 +1 静态面）。
- `rerenderFromMessagesCore`：web/bridge.js 与 tauri/bridge.js 两份 237/279 行镜像合并为参数化 Core，lane 差异（steer 标记、toolMeta 重建/清理策略、careful_blocked 卡形状、未决工具 sweep）按上游样板经 lane hooks 注入，两 lane 的 `rerenderFromMessages` 改为薄包装（保留 `pe` 快照嵌套工厂 `web:189622`/`tauriMain:99856`）。**不带** #746 tool 卡索引、**不带** #749 产物快路径。随合并对齐一项 web lane 既有行为：web 重放时 `userMessageDisplayText` 第二参由 `isScheduledRunSession(activeSessionId)` 收敛为恒 `true`（上游 #701 形态，内部信封重放时始终剥离；桌面 lane 原已如此）。
- 其余委托化：web/tauri 的 `liveChatItemKeptForHydration` 过滤内联改委托（web 随之对齐桌面的“后台 Shell 运行卡在权威重载时保留”语义）；web ×3 + tauri memory ×3 的 confirm/ignore/never MemoryCandidate 合并为 `resolveMemoryCandidate` 薄包装（`candidateCardPatcher` = lane `patchItemByIdFor` 以非碰撞名注入 web 与 tauriMemory cluster）；`buildAssistantDoneTimelineRecord` 统一后 tauri chat-events.js 与 web turn-terminal.js 只留转发；侧车读失败上报 `reportSidecarReadFailure` 随 loaders 引入（web/tauriMain/tauriSessions cluster 各注入，官方 loaders 由静默吞错变为 console.warn 后降级 []）；`tauri/bridge/sessions.js` 的 workspace-recents 经典脚本镜像（remember/maybePrune/forget + 本地 storage key）改为 tauriSessions cluster 前向器，双真相源在桥侧合一（`shared/workspace-recents.js` ES 模块按上游保留）。deps 解构补回 `candidateCardPatcher`、`reportSidecarReadFailure`（T0-a 按 C2 面剔除，本批随抽取回补）。
- 显式不做：`applyToolItemResult`（#746 抽取，留 T2）；`sessionMentionSplitter`（helpers 内部消费 T0-a 已就位，上游无导出无 lane 委托点；三份 lane 函数副本内的 window-global 内联查找随 T0-c 壳化收敛；`persistMessagesFor` 标题提取两处内联上游亦保留）；`upsertRecentRunRow`（helpers 符号 T0-a 已入、web cluster 已导出，web/tauri scheduled 的 lane 本地 `mergeScheduledTaskRecentRuns` 副本壳化属 T0-c）；`reportSidecarWriteFailure`（上游 scene 迁移写路径专用，官方 scene sync 形态不同，不在本批簇内）。两处 F（`personaName` cn_name 兜底、tauri `modelAvailabilityError` 熔断）零接触；官方独有 12 符号未动。
- 测试面：`bridge_domain_protocol.test.mjs` 重算三钉——`expectedSharedBaseHash`（`96fda42a…` → `67958905…`，74 签名：helpers 新增 `get_session_persona_events`/`get_session_pinvou_reviews`/`invoke(command, args)` 参数化入口）、memory 域摘要（三支 pending-memory invoke 移入 helpers）、sessions 域摘要（两支 sidecar invoke 移入 helpers）；`chat_turn_error_isolation.test.mjs`（terminalSandbox 补载 helpers、live 过滤钉改“lane 委托 + helpers 谓词体”双钉）、`artifact_deliverable_logic.test.js`（isDeliverable 门控钉改 helpers + lane 委托钉）、`web_access_contract.test.mjs`（plan_card 分支钉同款改法）——均为实现移位引发的钉点适配，测试逻辑未改写。全量 `npm run test:node` 与基线持平（1769 pass / 0 fail / 19 skipped）；全部改动文件 eslint 0 error，warning 7300 → 7110（净 −190，lane 镜像删除抵消 helpers 新增）。

### 14.2 T0-d（tool-store-events 搬迁 FFE4-1，本批）

- 新增 `src/shared/tool-store-events.js`（与上游 `ef98a715ee` 逐字一致）：`window.PinvouToolStoreEvents = Object.freeze({notifyComposerToolsChanged, notifyChatRoundCommitted})`，事件名 `pinvou:tools-changed` / `pinvou:chat-round-committed` 为跨层契约；`src/index.html` 在 bridge-messages 与 bridge-shared-helpers 之间装载（脚本相对序与上游 index.html:225 一致）；`knip.json` entry 同步。旧位 `src/features/tools/tool-events.js` 收缩为 re-export（上游逐字 + 1 条 organizeImports 抑制，导出序被 web_access_contract 钉定），feature 侧消费面（ToolStoreView、CodexAcpView、composer-shared）经 re-export 零改动。web/bridge.js ×5、tauri interaction.js ×2、chat.js ×1、chat-events.js ×1 共 9 处桥接裸 `CustomEvent` 内联派发改指 helper（#2030 因缺载该脚本白屏的前科，载入序为硬约束）。
- 官方独有消费语义保留：ChatView 对 `pinvou:web-connection` 的额外补刷监听（round-13 minor 3）；web 首轮提交处 `notifyChatRoundCommitted()` 之后的内联语音遥测收尾；web_access_contract 的双 lane `>=7/>=1` 计数钉。
- 测试同步：chat 域摘要 `2d422ec1…→9f3c8bdd…`（chat-events 监听体改经 helper，invoke/listen 命令面无增删）；session_mention 中继腿钉改双端钉（上游逐字）；web_access_contract tools-changed 重派发正则改 helper 形态 + FFE4-1 shared 钉。

### 14.3 T0-e（测试换装 + lane 依赖门，本批）

- **换装（9）**：`session_nav_race_web`（上游整版，+3 个 web 权威重载/后台 shell 卡保留/discardPlan 回写行为测试，分块下载桩与 bridge-messages vm 装载随迁）、`chat_usage_context_window`（includes 现代化 + tauri monitor 钉落点迁 helpers）、`bridge_state_domains`（上游版 + 官方 RemoteProxy 惰性镜像测试回填）、`tauri_bridge_state_identity`（#701 `loadSessionModel(null)` 驱动段换装，保留官方 L47 正则适配、remote-control 微任务沉降与 #727 单修订发布块）、`pinvou_scene_sidecar`（官方基座 + 上游 persona/review 共享装载器段：委托钉 + helpers 本体执行；上游 scene/steered 字节对比段因官方 bridge.js 核心壳化 defer 于 T4 不采纳）、`send_draft_restore_logic`（上游 voice override + 语音任务受理块，受理断言按官方双冻结形态展开）、`multiagent_plan_normalize`（spawned_at_ms 间距钉放宽至上游 4800）、`monitor_bridge_format`（上游结构 + web 侧 pollBackendStatus 重入守卫钉；桌面守卫属 #2030 F8 skipped 面，随行为批移植后补钉）、`markdown_bridge_fallback_safety`（includes 现代化；Case 4 官方双 lane 内联 escapeHtml 钉保留至 T4 壳化）。
- **语义合并已完成确认（5）**：`zap_withdraw_window`、`turn_error_display_contract`、`chat_turn_error_isolation`、`rebind_artifact_reconcile`、`session_mention`、`session_message_block`、`voice_input_error_logic`（官方版已含上游全部可适用钉 + fork 适配，逐文件比对确认无缺）；`chat_draft_workspace_logic` 与上游逐字一致。**换装 defer（1）**：`voice_operation_lifecycle` 上游 harness 以上游形沙箱直驱 lane 本体，官方 voice lane 保留 completeVoiceSubmission/trackVoiceTerminal 遥测调用点与双受理冻结（F 面），27/29 红为大面积 harness 重铺，defer 至语音 lane F reconcile 批。
- **移植（2+1 defer）**：`voice_mode_parity.test.mjs` 整版移植（web lane `normalizeVoiceMode` 携带上游 round-9 别名修复——官方 web 副本正是该测试要防的漂移体，`voice_input_error_logic` 同名钉同步改别名形态）；`bridge_lane_deps.test.mjs`（836 行 AST 门）整版移植后按官方现状重 derive：`sharedFnCount` 312→313（+官方基座 `clearVoiceInput` 合并体）、`tauriArtifacts` 无 `hasCommand`（官方 legacy listDeliverableIndex 不经 v2 协商，共享体带 typeof 守卫）、`tauriScheduled` +`createScheduledTask`（遥测 F 面）、`tauriMain` +`reportSidecarReadFailure`（官方独有）、`web` 簇 86 键官方集（+9 路由前向器/createScheduledTask，−上游 24 个官方未消费键）、`web:247496`（nestedRestoreUiTurnState）官方无装载点、随 web C 面批次补，`EXPECTED_FORWARDER_DEP_NAMES` 32 = 上游 23 + 官方 9 路由名（收缩与 web create 瘦身同批）。偏移别名 key（`tauriChat:31666`/`tauriMain:99856`/`tauriSessions:47342`/`web:158313`/`web:189622`）两仓逐字相同——数字为随代码携带的历史标签，无需重算；fork lane 文件 team-relay.js/desktop-remote-control.js 无 create( 装载点（25 个站点扫描实证），不入簇表。`deliverable_index_scan_error.test.mjs` defer 至 TB（官方 v2 命令面随 T0-fix 决议延后，先行移植必红）。
- **src 对齐（3 处，均为上游既有行为/结构，官方 web/状态表面缺口）**：web/bridge.js `discardPlan` 成功路径补 `planResolutionConfirmed` 回写一行（桌面 interaction.js 已有，上游 #701 同形）；web/bridge.js `normalizeVoiceMode` 补 voice_edit/draft_edit 别名（上游 round-9 修复，桌面/策略已在位）；`STATE_SLICE_FIELDS` 消费方改指——tauri/bridge.js 与 web/bridge/domain-adapter.js 删本地表改消费 `window.PinvouBridgeShared.STATE_SLICE_FIELDS`，共享表补 5 个官方 fork 条目（chat.remoteProxyDraftError、settings.behaviorTelemetry、llmapi、teamRelay、desktopRemoteControl）。
- **X 残差收口**：helpers 7 个零消费导出面残差（isTmpPath/memoryWriteLabel/memoryWriteStatusLabel/normalizeMemoryCandidateText/refreshPersonas/stripInternalToolRuntimeSuffix/upsertPendingMemoryCandidate）从 web/tauriMain/tauriArtifactTracker/tauriMemory/tauriPersonas 五簇导出名单移除（基座定义与内部消费保留，与上游导出面一致）；`tests/helpers/native-tool-output-harness.mjs` 脚本清单正则补 `shared/tool-store-events.js`（index.html 载入序）。协议摘要与 expectedSharedBaseHash 为 invoke/listen 命令签名面，本批无命令增删，实测全量协议测试通过、无需重算。
- 测试结果：全量 `npm run test:node` 1857 tests / 1840 pass / 0 fail / 17 skipped（较 T0-d 基线 1835 净增 5：voice_mode_parity +1、bridge_lane_deps +1、session_nav_race_web +3）；ui_smoke 58/58、kb_smoke 55/55（build:ui 后）；biome 改动文件 0 error；architecture-guard passed。

### 2026-10-22 主线 #701 提取与 Account 桥接组合

本次组合保留第 14 节的 shared helpers、独立 tool-store 事件与主线 hydration 修复。
Account 的模型引用切片（当前、草稿、默认及目录完成标记）进入同一个共享注册表；
terminal builder 继续保留同轮次 billing_notice，桌面原有错误转换先于共享记录写入。
具备 Account 专用语义的目录读取、typed model selection 和 timeline ownership 保留在
原 lane；草稿选择不写全局默认，传输失败不冒充权威空目录。当前公开的有效模型配置和
compactNow 调用面继续保留，并未沿主线旧消费者清单静默退休。

组合回归位于 `account_bridge_shared_composition.test.mjs`，执行实际共享实现和 Web
domain adapter；桌面 terminal 测试提取实际生产函数并使用受控依赖。此类 Node 验证
不等同完整原生构建、真实 Engine 或浏览器验收；先前现场与原生未验证边界保持。

### 14.4 T2（community#746 tool 卡重放索引回流，本批）

下列 T2 说明与计数保留 main #2076 当时的历史语境，包括当时的 llmapi 启动桩差异；它们不是当前 Account 组合的新增验收，也不恢复已退休的旧模型权威。当前组合另见末尾 2026-10-23 记录。

- 移植基准 community `0baf7fa6c`（#746 合并版）对 helpers 的全部 4 个 hunk，落到 T0 之后的树（helpers 簇即 #746 前夜 `ef98a715ee` 形态，逐字落位、零冲突）：① `updateToolItem` 的结果写入器抽为共享私有函数 `applyToolItemResult`（web+tauriTerminal 簇内）；② `rerenderFromMessagesCore` 重放期建 `firstToolItemById` Map 并经 `backfillToolItem` 回填——普通 string toolId 走 Map 首卡匹配（与 `updateToolItem` 语义一致，不再全量扫描 chatItems），legacy 非 string ID 委托 lane.updateToolItem 保留严格相等扫描；③ 重放工具卡 `addChatItem` 时登记首卡；④ careful 拦截与 load_skill 两处结果回填改走 `backfillToolItem`。官方挂起回流项 #2044（community#746）由此解锁。无 invoke/listen 命令面增删，`bridge_domain_protocol` 全量实测通过，expectedSharedBaseHash 无需重算。
- 测试移植（3 文件 + 1 新文件）：`conversation_replay_tool_backfill.test.mjs` 上游逐字新文件（tauri/web ×7 共 14 tests，真实 helpers + host hooks vm 装载：完整卡保序/首重复卡/工具卡 meta 生命周期、legacy ID 严格相等、结构化输出身份、专用卡排除、重放/renderer 失败后索引重建、string 回填不回扫先序卡、host 特有未决 sweep）；`session_nav_race_web` +1（web 冷重放重复 ID 首卡匹配 + 缓存会话导航往返）；`tauri_bridge_state_identity` 追加重放段（桌面公开冷载路径 + 重复 ID + 不可变订阅 + 旧发布工具卡不被后续导航变异）。
- 官方 F 面适配（本批唯一 fork 差异点）：官方 tauri 启动链独有 llmapi 刷新循环（上游无 `refreshLlmApiOnStartup`），且 vm 沙箱注入的是真 `setTimeout`——追加段逐字桩对 `get_llmapi_status` 返 `null` 时该循环以 2s/5s/10s/30s 封顶间隔永不退出，钉死 `node --test` 子进程（全套件挂死）。桩对该命令改应答 `{}`（`llmApiBackendUserState` 兜底判 `not_exists` 已知终态，循环即退出），与本文件既有段"未接管命令返 `{}`"的桩约定一致；其余逐字保持。
- lane 依赖门：`bridge_lane_deps` `sharedFnCount` 313→314（上游 #746 后 312→313 + 官方基座 `clearVoiceInput`；新增 `applyToolItemResult` 为私有、仅参数依赖——AST 范围审计 `updateToolItem` 只新增该 helper、replay 另加标准 Map，无新簇依赖/导出/转发，REQUIRED 不变）。官方 F 面与 T0 固化形态零回退：cn_name 兜底、modelAvailabilityError 熔断、clearVoiceInput 门注释均保持；#749 artifact 快路径仍按拆批边界留 T3。
- 测试结果：全量 `npm run test:node` 1872 tests / 1855 pass / 0 fail / 17 skipped（较 T0-e 基线 1857/1840/0/17 净增 15 且全过：conversation_replay_tool_backfill +14、session_nav_race_web +1；build 产物就位后 skip 数持平）；build:ui 后 ui_smoke 58/58、kb_smoke 55/55；biome 改动文件 0 error（tests 目录在 biome 忽略域，helpers 实测通过）。

### 14.5 T3（community#749 重放期 artifact 查找跳过回流，本批）

- 移植基准 community `fa00cc870`（#749 perf: skip absent artifact lookups during replay 合并版，父提交即 T2 基准 `0baf7fa6c`）对 helpers 的全部 4 个 hunk（35+/7−，与上游逐字一致仅行号偏移）：`rerenderFromMessagesCore` 开建 `artifactBasenames` membership 集合（惰性分配、单次重放生命周期、失败重放不留索引），配 `mayHavePresentedArtifact`/`appendReplayedArtifact`/`findReplayedArtifact`/`updateReplayedArtifact` 四个私有包装——membership 只证明 miss，可能命中仍走 live helpers 的精确路径优先/basename 兜底/更新边界；5 处 artifact append 位点全部登记，restore/gap-fallback/write 兜底的 7 处查找与追加改走包装。官方挂起回流项 #2058（community#749）由此解锁。#749 提交面其余为纯新增，无 lane/协议面改动；无 invoke/listen 增删，`bridge_domain_protocol` 全量实测通过，expectedSharedBaseHash 无需重算；`bridge_lane_deps` 新增 4 函数均为 `rerenderFromMessagesCore` 内嵌私有，sharedFnCount 门不受影响（实测通过）。
- 测试与文档移植（2 新文件，上游逐字）：`tests/artifact_replay_lookup.test.mjs`（tauri/web ×18 共 36 tests：membership 惰性分配、负查找不回扫普通卡、显式卡保序/稳定 ID/绝对路径升级、user 边界与 output 续卡语义、unsupported 路径强转与过滤禁用、跨重放/renderer 失败不残留索引；harness 以 `.rerenderFromMessagesCore({`…`}, opts);` 字面量切片装载真实 host lane hooks——实测本树 tauri/web lane 钩子对象与上游 `fa00cc870` 逐字一致，零适配）；`docs/artifact-replay-performance.md` 随迁。
- 官方 F 面与 T0/T2 固化形态零回退：cn_name 兜底、modelAvailabilityError 熔断、clearVoiceInput 门注释、tool-store-events 载入序（含 native-tool-output-harness 正则已含 `tool-store-events.js` 的 #2061 适配形态）均未触碰。
- 测试结果：全量 `npm run test:node` 1908 tests / 1891 pass / 0 fail / 17 skipped（较 T2 基线 1872/1855/0/17 净增 36 且全过：artifact_replay_lookup 新文件；build:ui/build:web 产物就位后 skip 持平）；build:ui 后 ui_smoke 58/58、kb_smoke 55/55；biome 改动文件 0 error（tests 目录在 biome 忽略域，helpers 实测通过）。
### 14.6 T4b（tauri 桥核心壳化 + 定案裁决清理，本批）

- 上游参照 community `349569ac6`。tauri/bridge.js 核心壳化（T0-c defer 清偿）：`escapeHtml` 内联体删除（安全原语单源进 helpers，注释改写为上游转发形态）、`renderMarkdown`/`onSessionEvent`/`subscriptionStateValue` 换簇委托壳（与上游 `return pinvouSharedtauriMain().x(...)` 形态逐字对齐）；桌面 `listDeliverableIndex` 恢复壳化（T0-fix defer 清偿：官方 v2 命令已随 TB 入主线 `src-tauri/src/app/commands/artifacts.rs`，KnowledgeView 已是 accept-both 双形状，lane 委托 helpers v2 协商核，version-locked 桌面不传 `hasCommand` 按 v2 可用处理）。三个 lane 本体按 F 保留并加登记注释：`bt`（官方叠 uiDictionary 懒载兜底，zap_withdraw_window 钉住）、`persistMessagesFor`（官方按 user 轮次顺延命名，scheduled_tasks_unit 场景 1b 钉住，与 T4a web lane 同判）、`userMessageDisplayText`（isKnownInternalMetadataBlock 桥消息 F 面，见 §12 分歧 7）。
- 定案裁决落地（a7-f-registry §6）：裁决③ helpers 合并体 `clearVoiceInput` 整删（定义+web/tauriMain/tauriVoice 三簇导出+deps 解构 `abandonCompletedVoiceResult` 回收；两 lane 本地覆盖与 facade、voice domain 契约面按上游原意保留——features/voice-composer 活消费，上游同形）；`dismissVllmSetup`/`markResolved` 经 T0-c 已清零，本批复核无残留。裁决⑤ `openExternalUrl`/`openInSystem`/`clearAttachments` 全桥面删除（tauri artifacts.js、tauri facade、web bridge.js、domain-adapter artifacts/attachments 域、`bridge_domain_contract` 两清单；clearAttachments 的 `finishAttachmentTelemetry` 包装位随迁，遥测函数本体因 removeAttachment/上传路径仍消费而保留）；Rust `open_in_system`/`open_external_url` 命令有 features 层活消费（main.jsx/ToolStoreView/KnowledgeView/team-relay），按"只删 JS 面"执行、Rust 零改动。
- 测试钉点同步（实现移位适配，逻辑未改写）：`bridge_domain_protocol` artifacts 域摘要重算（`fc915765…`→`e4cd72cc…`：list_deliverable_index/list_workspace_files 回 helpers 共享基座、open_in_system/open_external_url 包装位删除）；`bridge_lane_deps` `sharedFnCount` 314→313（=上游 #746 后值，clearVoiceInput 删除）+ tauriArtifacts 注释更新；`markdown_bridge_fallback_safety` Case 4 改上游双 lane 转发钉；`markdown_syntax_highlight` 渲染器钉改 helpers+桌面转发钉。
- M1/M3/M4/M5 补登记随本 PR 落 §12（分歧 5-8）；分歧 3 同 PR 改写退役。M6 以最小存在性门禁落地：`scripts/fork-guard.sh` APP 指纹层补 F-A1/F-A2/F-A3/F-B1/F-B2/F-B3/F-B4/F-C1 及裁决①②保留面的 grep -F 存在性钉（--fast 层即生效）。
- voice harness 重铺实测：上游 `voice_operation_lifecycle.test.mjs` 直驱官方语音 lane = 2/29（红全部为 harness 级：官方 lane 保留 completeVoiceSubmission/trackVoiceTerminal 遥测调用点、双受理冻结、本地 clearVoiceInput 覆盖等 F 面，上游 harness 以上游语音结构切片）——官方 83/83 版本保持，结论登记为"上游 harness 依赖上游语音结构，官方语义等价保留"，非失败项。
- 测试结果：全量 `npm run test:node` 1890 tests / 1871 pass / 0 fail / 19 skipped（与 T4b 前 HEAD 基线 1871/0/19 持平，无新增失败）；build:ui 后 ui_smoke 58/58、kb_smoke 55/55；biome 改动文件 0 error；architecture-guard passed。

## 2026-10-23 consumed baseline: Python stdio environment defaults (T2)

CI public-baseline verification detected maintenance head `7d2636993` replacing `083cebfd9`. This parent consumes the already merged CodeWhale#115; it does not create another foundation patch or behavior topic. `CommandSpec::with_env` now merges through `with_env_var` so shell defaults survive empty/nonempty task overlays. On Windows, explicit environment names override earlier spellings case-insensitively; child environment sanitization still applies. Public signatures and dependency files are unchanged, so no app API adapter is needed.

Existing upstream regressions cover generic overlays, Windows shell defaults and both Python execution lanes. Fingerprints pin those tests. Native Windows execution belongs to the corresponding upstream/parent CI; local Linux checks do not claim to exercise those cfg(windows) tests. The immutable r3 closure tag remains `61cb769be`. Recount:77 registered commits /76 commits with a signoff (the historical #107 exception is unchanged).

## 2026-10-23 consumed baseline: failed tool result flags (T2)

The live maintenance head advanced again to `e9a7f48da` (CodeWhale#116) while #115 validation was running. The only production change passes `(!output.success).then_some(true)` into the existing history-content builder instead of discarding the failure flag. The added upstream regression persists/restores HTTP failures, execution errors, success and schema-loaded non-execution. Public signatures, stored schema and dependencies are unchanged; no app adapter or new fork patch is needed. The existing T2 tool-result boundary owns this fix.

Recount at this head:78 registered commits,76 with a signoff; #116's squash message introduces a second missing-footer exception alongside historical #107. This inventory records the public commit as observed; it does not claim an upstream report or rewrite. The parent repository's own commits remain signed. The immutable closure tag is unchanged.

## 2026-10-23 consumed help-only baseline: JSONPath subset (T2)

The public verifier advanced to `ea9fdc90f` (CodeWhale#117) between the parent and child pushes. The entire production delta from `e9a7f48da` is one Web `fields.description` string; execution, parsing, public signatures, dependencies and persisted schema are unchanged. The added resultful test drives advertised selectors through the real parser and verifies unsupported recursive descent/filters. The new fingerprint is under existing T2; no new fork patch/topic is introduced. Recount79 registered/77 signed commits; #107/#116 remain the only missing-footer exceptions.

The preceding e9 execution regressions are retained as such. This help-only update is checked through current consumer compilation, Clippy and the current foundation schema/fork contracts; retained prior full-suite counts must not be presented as fresh runs at this documentation-only head.
### 2026-10-23 主线 #2076 T2 与 Account 桥接组合

本次组合同时保留上面的 Account 模型引用、权威目录快照、typed selection、草稿导航及 compactNow 公共面，与 14.4 的工具卡重放索引。共享顶层函数计数为 315（本分支 314 + 私有 applyToolItemResult；main 为 314），新结果写入器只依赖参数，Map 仅属于单次重放，不改变各 lane 的 deps/forwarders/导出。

组合验证继续使用 conversation_replay_tool_backfill、session_nav_race_web、tauri_bridge_state_identity、account_bridge_shared_composition、bridge_lane_deps 和 bridge_domain_protocol 等实际生产实现回归。无 invoke/listen 命令面或 CodeWhale gitlink/Rust 改动，协议摘要、既有 fork 指纹和行为名保持；没有为计数变化伪造新命令摘要。作者证据及私有因果控制另存，不继承 14.4 的历史全量结果，也不冒称本组合完成真实原生、Engine、浏览器或现场验收。

### 2026-10-23 复审对齐合并（main 3555d5567 → PR #2053，复审侧执行）

- 对齐范围：#2027 失败诊断 feedback 面（4 命令 + web 平台拒绝桩）、T3/T4a/T4b 桥接壳化、#2081/#2083/#2084/#2085 回流与 0.10.14。冲突消解保全双方：feedback 诊断面并入 tauri facade 与 web domain-adapter；Account 侧 models/interaction 扩面（getEffectiveModelConfig、loadEffectiveModelConfig、switchModelRef、compactNow）保留。web lane create deps 沿用 main T4b 壳化形态（98 键）：lane 侧 freshBuffer 等同名函数已是委托壳，不得再作为 forwarder dep 传回共享核（否则 base 重绑定到壳造成自引用递归——freshBuffer 栈溢出被 ensureSession catch 吞掉、equipPersona 静默放弃，实测复现后按 main 形态消除）。
- **F-B1 退役登记（覆盖 M6 两条钉子）**：旧 llmapi 闭源服务面 `prepareBackendManagedModel` / `modelAvailabilityError`（tauri settings lane）随 Account Platform 集成本PR整体退役——llmapi JS 面全部删除（facade 簇、启动刷新循环、web `llmapi: {}` 域、契约清单），模型权威改由 Account 模型门禁（getEffectiveModelConfig/loadEffectiveModelConfig）承担。fork-guard M6 对应两条 F-B1 钉子撤销，其余 F-A/F-B2/B3/B4/F-C1/裁决①钉全部保留且实测通过。Rust 侧 llmapi_hub 的 11 文件处置属本 PR 主体变更，其完整性由 PR 评审独立核验。
- 组合实测：`sharedFnCount` = 314（main T4b 后 313 + Account 公共 compactNow；T2 期 315 口径已被 T4b 裁决③ clearVoiceInput 整删覆盖）；`bridge_domain_protocol` artifacts 域摘要维持 main T4b 值 `e4cd72cc…`（Account 组合未触碰 artifacts 域命令面）；web lane deps 按 main T4b 形态 98 键实测对齐；`bridge_lane_deps` / `bridge_domain_protocol` / `bridge_domain_contract` / pet 三件套实测通过。上文 2026-10-23 组合记录中"共享顶层函数计数为 315"为该次组合时的历史口径，非当前合并树数值。
