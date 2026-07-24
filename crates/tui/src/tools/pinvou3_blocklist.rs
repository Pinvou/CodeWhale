//! pinvou3 工具默认隐藏清单（L1.5）。
//!
//! 列在此处的工具仍然注册进 `ToolRegistry`（仍可被 `tool_search` 激活
//! 调用），但 `ToolRegistry::to_api_tools()` 喂给 LLM 时标记
//! `defer_loading = true`，模型默认看不到。
//!
//! 设计动机与维护流程见 `pinvou3/docs/工具表精简方案.md`。
//!
//! 维护要点：上游每次 rebase 后跑漂移检测（文档 §5.3），新工具默认
//! 进 blocklist 再单独评估是否放出来。

/// 默认对 LLM 隐藏的工具名。维护时请保持按 §3.2 的类别分组排列。
pub const PINVOU3_HIDDEN_TOOLS: &[&str] = &[
    // 状态管理 - durable task（GUI 单 session 不需要持久化）
    "task_create",
    "task_list",
    "task_read",
    "task_cancel",
    "task_gate_run",
    "task_shell_start",
    "task_shell_wait",
    // 状态管理 - PR 跟踪（pinvou3 非 CI 工具）
    "pr_attempt_record",
    "pr_attempt_list",
    "pr_attempt_read",
    "pr_attempt_preflight",
    // 状态管理 - subagent:放出 agent_open→agent_eval→agent_close 一条干净生命周期供模型
    // 直接派/收/关子 agent;spawn 单一走 agent_open,故隐藏实验性 tool_agent;其余仍隐藏。
    // (首轮工具调用漂移已由 session 启动 cache warmup 根治,与放通 subagent 无关。)
    "tool_agent",
    "agent_spawn", // agent_open 的新版本（fork_context=true 路径，与 agent_open 重复）
    "agent_result",
    "agent_cancel",
    "agent_list",
    "resume_agent",
    "delegate_to_agent",
    // 状态管理 - RLM（无持久 REPL 场景）
    "rlm_open",
    "rlm_eval",
    "rlm_configure",
    "rlm_close",
    // 状态管理 - goal（GUI 单 session 不需要持久化目标跟踪）
    "create_goal",
    "get_goal",
    "update_goal",
    // git（模型用 `exec_shell git ...` 替代）。2026-07-03 纯办公定位决策:git_status/git_diff 一并
    // 隐藏(原为代码辅助释放,pinvou3 放弃代码辅助定位后与 log/show/blame 一致全隐藏;需要走 exec_shell git)。
    "git_status",
    "git_diff",
    "git_log",
    "git_show",
    "git_blame",
    // patch / fim（edit_file 已覆盖；patch DSL 弱模型用不好；revert_turn 已释放）
    "apply_patch",
    "fim_edit",
    // 附件预处理（应移到 bridge 上传 pipeline，见文档 §6.1）
    "pandoc_convert",
    "image_ocr",
    // image_analyze 已放出:Qwen3.6 实测有视觉(2026-05-28),vision_config 指向同一 vllm 端点,
    // 用户附图后由 LLM 调 image_analyze(workspace 相对路径)读图。
    // todo 兼容别名（保留 checklist_*；todo_* 是 v0.8.x 之前的 legacy alias）
    "todo_write",
    "todo_add",
    "todo_update",
    "todo_list",
    // Shell 后台管理 + 异步交互变体（exec_shell_wait 已释放供后台轮询）
    "exec_shell_cancel",
    "exec_shell_interact",
    "exec_wait",
    "exec_interact",
    // Automation 持久化（pinvou3 单 session 不需要）
    "automation_create",
    "automation_delete",
    "automation_list",
    "automation_pause",
    "automation_read",
    "automation_resume",
    "automation_run",
    "automation_update",
    // GitHub 集成（普通用户用不到，开发者用 exec_shell gh 替代）
    "github_issue_context",
    "github_pr_context",
    "github_comment",
    "github_close_issue",
    // 杂项 - 金融数据 + 旧版 web_run（保留 fetch_url）
    "finance",
    "web.run",
    // 元工具（pinvou3 普通用户场景用不到）。2026-07-03:diagnostics 一并隐藏(环境自检=报 workspace/
    // git 检测/sandbox/Rust 工具链版本,办公用户用不到;代码诊断走自动 post-edit LSP hook,不依赖此工具)。
    "diagnostics",
    "multi_tool_use.parallel",
    "note",
    "validate_data",
    "run_tests",
    "handle_read",
    "retrieve_tool_result",
    "project_map",
    "recall_archive",
    "review",
    "notify",
    "remember",
    "web_run",
    // v0.8.53 sync 后补漏(2026-06-04 clean re-fork dump 发现这些新/漏工具暴露给了 LLM):
    // 语音(Xiaomi MiMo,v0.8.53 新增,pinvou3 用不到)
    "speech",
    "tts",
    // rlm 会话对象(漏网:rlm_open/eval/configure/close 已隐藏,就它漏了)
    "rlm_session_objects",
    // github(漏网:其余 github_* 已隐藏)
    "github_close_pr",
    // 验证(run_tests 已隐藏;verifier ensemble pinvou3 不用)
    "run_verifiers",
    // 反 slop ledger(底座内部 anti-slop 机制,非 pinvou3 用户工具)
    "slop_ledger_append",
    "slop_ledger_export",
    "slop_ledger_query",
    "slop_ledger_update",
    // tool_search(v0.8.57 上游新增,**v0.8.65 折叠成单工具 `tool_search`** + match 参数):让模型
    // 搜索并**激活 deferred 工具**,会绕过 pinvou3 blocklist(defer 不删除)激活 agent/delegate 等
    // 被隐藏工具。pinvou3 的 active 集完整、无合法 deferred 工具要激活,故彻底禁用。tool_catalog.rs
    // 注入处配套 gate(is_pinvou3_hidden(TOOL_SEARCH_NAME) 为真不注入)→ catalog 根本不含 tool_search。
    // ⚠️ 2026-07-03:v0.8.65 sync 后上游把门控名改成单名 `tool_search`,而此处只有 v0.8.57 双旧名
    //   → gate 名字对不上、tool_search 漏注入(端到端实测 catalog 含 tool_search,门控失效)。
    //   补裸名 `tool_search`(门控真正依赖的名);双旧名保留做前向兼容(当前无对应工具=空防)。
    "tool_search",            // v0.8.65 折叠后单名 —— gate 真正依赖的名
    "tool_search_tool_regex", // v0.8.57 旧双名(前向兼容,当前无对应工具)
    "tool_search_tool_bm25",
];

/// 工具名是否在 pinvou3 隐藏清单内。
///
/// **测试豁免**: `PINVOU3_BLOCKLIST_OVERRIDE` env var 列出的工具名(逗号分隔)
/// 即便在 PINVOU3_HIDDEN_TOOLS 里也返回 false。供 L1 dialog harness 临时启用
/// 特定工具评估能力(例:`PINVOU3_BLOCKLIST_OVERRIDE=agent_spawn,agent_eval`)。
/// 生产场景不 set 这个 env,blocklist 行为不变。
#[inline]
pub fn is_pinvou3_hidden(name: &str) -> bool {
    if !PINVOU3_HIDDEN_TOOLS.contains(&name) {
        return false;
    }
    if let Ok(override_list) = std::env::var("PINVOU3_BLOCKLIST_OVERRIDE") {
        if override_list.split(',').any(|t| t.trim() == name) {
            return false;
        }
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    /// [pinvou3-fork] Golden 名单守护:blocklist 的**精确**内容必须 == golden。sync 后上游
    /// 增/删 model-facing 工具、或 pinvou3 调整 blocklist,都会让本测试 fail → 强制逐项确认
    /// (上游新增→评估是否补黑名单;上游改名/折叠→门控名是否跟上;pinvou3 有意增删→更新此
    /// golden)。避免"静默漂移":2026-07-03 tool_search 折叠单名事故正是"改名"没被间接检查
    /// (对比 ToolSpec name / grep 新增注入路径)抓到,见 `docs/工具表精简方案.md` §8.2。配套
    /// `engine::tests::forkguard_yolo_no_deferred_activator_first_class` 守护注入层结果。
    #[test]
    fn forkguard_blocklist_golden() {
        use std::collections::BTreeSet;
        let actual: BTreeSet<&str> = PINVOU3_HIDDEN_TOOLS.iter().copied().collect();
        let golden: BTreeSet<&str> = [
            "task_create",
            "task_list",
            "task_read",
            "task_cancel",
            "task_gate_run",
            "task_shell_start",
            "task_shell_wait",
            "pr_attempt_record",
            "pr_attempt_list",
            "pr_attempt_read",
            "pr_attempt_preflight",
            "tool_agent",
            "agent_spawn",
            "agent_result",
            "agent_cancel",
            "agent_list",
            "resume_agent",
            "delegate_to_agent",
            "rlm_open",
            "rlm_eval",
            "rlm_configure",
            "rlm_close",
            "create_goal",
            "get_goal",
            "update_goal",
            "git_status",
            "git_diff",
            "git_log",
            "git_show",
            "git_blame",
            "apply_patch",
            "fim_edit",
            "pandoc_convert",
            "image_ocr",
            "todo_write",
            "todo_add",
            "todo_update",
            "todo_list",
            "exec_shell_cancel",
            "exec_shell_interact",
            "exec_wait",
            "exec_interact",
            "automation_create",
            "automation_delete",
            "automation_list",
            "automation_pause",
            "automation_read",
            "automation_resume",
            "automation_run",
            "automation_update",
            "github_issue_context",
            "github_pr_context",
            "github_comment",
            "github_close_issue",
            "finance",
            "web.run",
            "diagnostics",
            "multi_tool_use.parallel",
            "note",
            "validate_data",
            "run_tests",
            "handle_read",
            "retrieve_tool_result",
            "project_map",
            "recall_archive",
            "review",
            "notify",
            "remember",
            "web_run",
            "speech",
            "tts",
            "rlm_session_objects",
            "github_close_pr",
            "run_verifiers",
            "slop_ledger_append",
            "slop_ledger_export",
            "slop_ledger_query",
            "slop_ledger_update",
            "tool_search",
            "tool_search_tool_regex",
            "tool_search_tool_bm25",
        ]
        .into_iter()
        .collect();
        let missing: Vec<&str> = golden.difference(&actual).copied().collect();
        let extra: Vec<&str> = actual.difference(&golden).copied().collect();
        assert!(
            missing.is_empty() && extra.is_empty(),
            "blocklist 名单漂移!golden 有但当前缺={missing:?};当前多出={extra:?}。\
             上游新增 model-facing 工具→评估补黑名单;上游改名/折叠→门控名跟上;\
             pinvou3 有意增删→更新此 golden。"
        );
    }

    #[test]
    fn hides_known_state_management_tools() {
        assert!(is_pinvou3_hidden("task_create"));
        assert!(is_pinvou3_hidden("tool_agent")); // spawn 单一走 agent_open,tool_agent 隐藏
        assert!(is_pinvou3_hidden("rlm_eval"));
        assert!(is_pinvou3_hidden("pr_attempt_record"));
    }

    #[test]
    fn keeps_core_tools_visible() {
        assert!(!is_pinvou3_hidden("read_file"));
        assert!(!is_pinvou3_hidden("write_file"));
        assert!(!is_pinvou3_hidden("append_file"));
        assert!(!is_pinvou3_hidden("exec_shell"));
        assert!(!is_pinvou3_hidden("web_search"));
        assert!(!is_pinvou3_hidden("checklist_write"));
        assert!(!is_pinvou3_hidden("update_plan"));
        // 视觉:Qwen3.6 有视觉能力,image_analyze 已放出供 LLM 读用户附图
        assert!(!is_pinvou3_hidden("image_analyze"));
        // subagent 干净生命周期可见:agent_open(spawn) → agent_eval(收) → agent_close(关)
        assert!(!is_pinvou3_hidden("agent_open"));
        assert!(!is_pinvou3_hidden("agent_eval"));
        assert!(!is_pinvou3_hidden("agent_close"));
        // spawn 单一走 agent_open:实验性 tool_agent + id-API 重复链路仍隐藏
        // (用 delegate_to_agent 而非 agent_spawn 当代表,避开 env_override 测试的全局 env 竞争)
        assert!(is_pinvou3_hidden("tool_agent"));
        assert!(is_pinvou3_hidden("delegate_to_agent"));
    }

    #[test]
    fn hides_legacy_todo_aliases_keeps_checklist() {
        assert!(is_pinvou3_hidden("todo_write"));
        assert!(is_pinvou3_hidden("todo_add"));
        assert!(!is_pinvou3_hidden("checklist_write"));
        assert!(!is_pinvou3_hidden("checklist_add"));
    }

    /// L1 harness 用 PINVOU3_BLOCKLIST_OVERRIDE 临时启用工具评估能力。
    /// 生产场景不 set 这个 env,blocklist 行为不变。
    #[test]
    fn env_override_unhides_listed_tools() {
        // SAFETY: 测试是 single-threaded(`cargo test` 单文件内串行),
        // 且测试函数末尾 remove_var 复原。2024 edition std::env::set_var
        // 标 unsafe 因多线程 race,本场景不 race。
        unsafe {
            // baseline: agent_spawn / agent_result 在 blocklist 里
            // (agent_eval/open/close 已放出,不再适合做 override 例子)
            std::env::remove_var("PINVOU3_BLOCKLIST_OVERRIDE");
            assert!(is_pinvou3_hidden("agent_spawn"));
            assert!(is_pinvou3_hidden("agent_result"));

            // 设 env 解锁 agent_spawn + agent_result
            std::env::set_var("PINVOU3_BLOCKLIST_OVERRIDE", "agent_spawn, agent_result");
            assert!(
                !is_pinvou3_hidden("agent_spawn"),
                "agent_spawn 应被 env 豁免"
            );
            assert!(
                !is_pinvou3_hidden("agent_result"),
                "agent_result 应被 env 豁免"
            );
            // 未列出的工具仍隐藏
            assert!(
                is_pinvou3_hidden("task_create"),
                "task_create 未列入 override,仍隐藏"
            );
            // 核心工具不受影响
            assert!(!is_pinvou3_hidden("write_file"));

            std::env::remove_var("PINVOU3_BLOCKLIST_OVERRIDE");
        }
    }
}
