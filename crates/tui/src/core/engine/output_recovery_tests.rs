//! Exercise preservation through the real serial/parallel dispatch and fan-out.

use super::*;
use crate::tools::spec::{ToolCapability, ToolContext, ToolSpec};
use base64::Engine as _;
use std::sync::Arc;

struct RecoveryFixture {
    parallel: bool,
    rendezvous: Option<Arc<tokio::sync::Barrier>>,
}

fn fixture_output(tag: &str) -> String {
    format!("{tag}:START\n{}\n{tag}:END 中文", "x".repeat(20_000))
}

#[async_trait::async_trait]
impl ToolSpec for RecoveryFixture {
    fn name(&self) -> &'static str {
        "recovery_fixture"
    }

    fn description(&self) -> &'static str {
        "Return deterministic evidence without tool-time truncation."
    }

    fn input_schema(&self) -> Value {
        json!({"type": "object", "properties": {"tag": {"type": "string"}}, "required": ["tag"]})
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::ReadOnly]
    }

    fn supports_parallel(&self) -> bool {
        self.parallel
    }

    async fn execute(&self, input: Value, _: &ToolContext) -> Result<ToolResult, ToolError> {
        if let Some(rendezvous) = &self.rendezvous {
            rendezvous.wait().await;
        }
        Ok(ToolResult::success(fixture_output(
            input["tag"].as_str().expect("fixture tag"),
        )))
    }
}

fn assert_dispatch_preserves_output(parallel: bool) {
    let _env = lock_test_env();
    let _workshop = crate::tools::large_output_router::active_workshop_test_guard();
    with_artifact_home(|home| {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(async {
                let mock = Arc::new(crate::llm_client::mock::MockLlmClient::new(vec![]));
                let (mut engine, handle) = Engine::new_with_model_client(
                    EngineConfig {
                        workspace: home.to_path_buf(),
                        snapshots_enabled: false,
                        ..Default::default()
                    },
                    &Config::default(),
                    mock,
                );
                engine.api_provider = ApiProvider::Deepseek;
                engine.session.model = ROUTE_128K.to_string();
                engine.session.id = SESSION_6508.to_string();
                engine.active_route_limits = None;
                assert_eq!(budget_128k(), 15_360);

                // This output exceeds the model view but not adaptive/classic
                // spillover. Only the turn-loop context-preservation wiring
                // under test can publish its artifact before fan-out.
                let mut unexecuted = ToolResult::success(fixture_output("preflight"));
                assert!(view_128k("recovery_fixture", &unexecuted).needs_full_output_artifact);
                assert!(
                    crate::tools::truncate::apply_spillover_with_artifact(
                        &mut unexecuted,
                        "preflight",
                        "recovery_fixture",
                        SESSION_6508,
                    )
                    .is_none()
                );
                assert!(session_artifact_files(home).is_empty());

                let context = ToolContext::new(home).with_state_namespace(SESSION_6508);
                let mut registry = crate::tools::ToolRegistry::new(context.clone());
                registry.register(Arc::new(RecoveryFixture {
                    parallel,
                    rendezvous: parallel.then(|| Arc::new(tokio::sync::Barrier::new(2))),
                }));
                let mut catalog = registry.to_api_tools();
                let policy = policy_for_catalog(
                    catalog.clone(),
                    None,
                    None,
                    crate::tui::approval::ApprovalMode::Suggest,
                );
                let mut active = policy.active_names.clone();
                let count = if parallel { 2 } else { 1 };
                let plans = (0..count)
                    .map(|index| ToolExecutionPlan {
                        index,
                        id: format!("recovery-{index}"),
                        name: "recovery_fixture".to_string(),
                        input: json!({"tag": format!("call-{index}")}),
                        caller: None,
                        interactive: false,
                        approval_required: false,
                        approval_description: String::new(),
                        approval_force_prompt: false,
                        supports_parallel: parallel,
                        read_only: true,
                        detached_start: false,
                        resources: vec![],
                        blocked_error: None,
                        guard_result: None,
                    })
                    .collect();
                let mut mode = AppMode::Agent;
                let mut questions = true;
                let outcomes = tokio::time::timeout(
                    Duration::from_secs(15),
                    engine.execute_planned_tools(
                        plans,
                        "recovery-turn",
                        "",
                        &mut catalog,
                        &policy,
                        &mut active,
                        Some(&registry),
                        Arc::new(tokio::sync::RwLock::new(())),
                        None,
                        &crate::sandbox::SandboxPolicy::default(),
                        &mut mode,
                        &mut questions,
                    ),
                )
                .await
                .expect("parallel fixtures must rendezvous in the parallel arm");

                let mut rx = handle.rx_event.write().await;
                let completions: HashMap<_, _> = std::iter::from_fn(|| rx.try_recv().ok())
                    .filter_map(|event| match event {
                        Event::ToolCallComplete { id, result, .. } => Some((id, result)),
                        _ => None,
                    })
                    .collect();
                assert_eq!(completions.len(), count);
                let mut refs = HashSet::new();
                for (index, outcome) in outcomes.into_iter().enumerate() {
                    let outcome = outcome.expect("dispatched result");
                    let output = outcome
                        .terminal
                        .into_legacy_result()
                        .expect("fixture succeeded");
                    let original = fixture_output(&format!("call-{index}"));
                    assert_eq!(output.content, original);
                    let ui = completions[&outcome.id].as_ref().expect("UI completion");
                    assert_eq!(ui.content, output.content);
                    assert_eq!(ui.metadata, output.metadata);
                    let view = view_128k("recovery_fixture", &output);
                    let reference =
                        output.metadata.as_ref().expect("preserved before fan-out")["artifact_id"]
                            .as_str()
                            .expect("real artifact ref");
                    assert!(
                        refs.insert(reference.to_string()),
                        "calls must not share artifacts"
                    );
                    assert!(!view.needs_full_output_artifact);
                    assert!(view.text.contains(&format!("ref=\"{reference}\"")));
                    assert!(view.text.chars().count() <= budget_128k());
                    let retrieved = crate::tools::tool_result_retrieval::RetrieveToolResultTool
                        .execute(
                            json!({"ref": reference, "mode": "bytes", "offset": 0,
                                "length": original.len(), "max_bytes": 131_072}),
                            &context,
                        )
                        .await
                        .expect("retrieve advertised ref under its origin session");
                    let payload: Value =
                        serde_json::from_str(&retrieved.content).expect("bytes response");
                    let bytes = base64::engine::general_purpose::STANDARD
                        .decode(payload["data"].as_str().expect("base64 payload"))
                        .expect("valid base64");
                    assert_eq!(bytes, original.as_bytes());
                }
                assert_eq!(session_artifact_files(home).len(), count);
            });
    });
}

#[test]
fn forkguard_turn_loop_serial_preserves_output_before_fanout() {
    assert_dispatch_preserves_output(false);
}

#[test]
fn forkguard_turn_loop_parallel_preserves_output_before_fanout() {
    assert_dispatch_preserves_output(true);
}
