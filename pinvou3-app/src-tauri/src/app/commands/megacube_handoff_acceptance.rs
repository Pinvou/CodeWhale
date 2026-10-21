//! Private opt-in stdio adapter over production registration, session binding
//! and EnginePool inference. The frontend controller can be tested against real
//! persisted state without automating or interrupting the user's native window.
use std::io::Write;
use std::sync::Arc;

use anyhow::{Context, Result, bail};
use serde_json::{Value, json};

use crate::features::assistant::engine_pool::EnginePool;
use crate::features::assistant::product_runtime::{EnginePoolRuntime, TurnHandle, TurnInput};
use crate::features::sessions::SessionStore;
use crate::platform::credential_store::SystemCredentialStore;
use crate::platform::prefs::{SavedModel, UserPrefs};

pub fn run() -> Result<()> {
    let profile = std::env::var_os("PINVOU3_HOME")
        .map(std::path::PathBuf::from)
        .context("PINVOU3_HOME required")?;
    if std::env::var("PINVOU3_MEGACUBE_ACCEPTANCE").as_deref() != Ok("1")
        || !profile.is_absolute()
        || !profile.components().any(|part| part.as_os_str() == ".qa")
        || !profile
            .file_name()
            .is_some_and(|name| name.to_string_lossy().starts_with("mcp-switch-real-"))
        || profile.is_symlink()
    {
        bail!("explicit isolated .qa/mcp-switch-real-* profile and opt-in required");
    }
    crate::features::assistant::product_runtime::headless_bridge::run_windowless_host(
        |pool, store| async move {
            let pool = Arc::new(pool);
            let runtime = EnginePoolRuntime::new(pool.clone());
            println!("{}", json!({"ready":true}));
            std::io::stdout().flush()?;
            loop {
                let line = tokio::task::spawn_blocking(|| {
                    let mut line = String::new();
                    std::io::stdin()
                        .read_line(&mut line)
                        .map(|count| (count, line))
                })
                .await??;
                if line.0 == 0 {
                    break;
                }
                if line.1.len() > 262144 {
                    bail!("request too large");
                }
                let request: Value = serde_json::from_str(&line.1)?;
                if request["command"] == "quit" {
                    break;
                }
                let result = dispatch(&request, &pool, &store, &runtime).await;
                let reply = match result {
                    Ok(value) => json!({"id":request["id"],"result":value}),
                    Err(error) => json!({"id":request["id"],"error":error.to_string()}),
                };
                println!("{reply}");
                std::io::stdout().flush()?;
            }
            Ok(())
        },
    )
}

fn text<'a>(request: &'a Value, key: &str) -> Result<&'a str> {
    request[key]
        .as_str()
        .with_context(|| format!("missing {key}"))
}

async fn dispatch(
    request: &Value,
    pool: &Arc<EnginePool>,
    store: &SessionStore,
    runtime: &EnginePoolRuntime,
) -> Result<Value> {
    match text(request, "command")? {
        // Opt-in tests exercise the exact desktop observation/action commands.
        // The host's existing isolated-profile guard applies to these routes.
        "guidance-observe" => Ok(json!({
            "device": super::cms::cms_device_status(Some(true)).await.map_err(anyhow::Error::msg)?,
            "service": super::cms::cms_service_status().await.map_err(anyhow::Error::msg)?,
            "status": super::cms::cms_status().await.map_err(anyhow::Error::msg)?,
            "models": super::cms::cms_models().await.map_err(anyhow::Error::msg)?,
            "tasks": super::cms::cms_tasks().await.map_err(anyhow::Error::msg)?,
            "discovery": super::monitor::discover_local_vllm(Some(serde_json::from_value(json!({
                "customPort": request.get("port").and_then(Value::as_u64)
            }))?)).await.map_err(anyhow::Error::msg)?,
        })),
        "guidance-start" => {
            super::cms::cms_model_action(text(request, "card")?.to_owned(), "enable".into())
                .await
                .map_err(anyhow::Error::msg)
        }
        "guidance-action" => super::cms::cms_model_action(
            text(request, "card")?.to_owned(),
            text(request, "action")?.to_owned(),
        )
        .await
        .map_err(anyhow::Error::msg),
        "guidance-connection" => {
            let port = request
                .get("port")
                .and_then(Value::as_u64)
                .filter(|port| *port > 0 && *port <= u16::MAX as u64)
                .context("valid loopback port required")?;
            Ok(serde_json::to_value(
                super::settings::test_model_connection(
                    format!("http://127.0.0.1:{port}/v1"),
                    String::new(),
                    None,
                )
                .await
                .map_err(anyhow::Error::msg)?,
            )?)
        }
        "models" => Ok(serde_json::to_value(
            super::settings::list_models()
                .await
                .map_err(anyhow::Error::msg)?,
        )?),
        "save" => {
            let model: SavedModel = serde_json::from_value(request["model"].clone())?;
            if model.is_builtin_llmapi() {
                bail!("builtin model immutable");
            }
            let id = model.id.clone();
            // The exact shared core used by the desktop command, including
            // cross-process settings lock and credential commit ordering.
            tokio::task::spawn_blocking(move || {
                super::settings::save_model_inner(model, &SystemCredentialStore::new())
            })
            .await?
            .map_err(anyhow::Error::msg)?;
            pool.mark_model_updated(&id);
            Ok(json!({"saved":id}))
        }
        "default" => {
            let id = text(request, "modelId")?.to_owned();
            UserPrefs::update_transaction(|prefs| {
                if prefs.model_by_id(&id).is_none() {
                    return Err("unknown model".into());
                }
                prefs.advanced.active_model_id = Some(id);
                Ok(())
            })
            .map_err(anyhow::Error::msg)?;
            Ok(json!({"saved":true}))
        }
        "create" => {
            let model_id = text(request, "modelId")?.to_owned();
            let session = store.create_new(
                "acceptance".into(),
                Some(model_id),
                crate::platform::paths::pinvou3_home().join("workspace"),
            )?;
            Ok(json!({"sessionId":session.metadata.id}))
        }
        "binding" => Ok(json!(store.session_model_id(text(request, "sessionId")?))),
        "switch" => {
            pool.switch_idle_session_model(
                text(request, "sessionId")?,
                Some(text(request, "modelId")?.into()),
                request["expectedModelId"].as_str().map(str::to_owned),
                request["expectedDefaultModelId"]
                    .as_str()
                    .map(str::to_owned),
            )
            .await?;
            Ok(json!({"selected":true}))
        }
        "infer" | "begin" | "infer-agent" => {
            let session_id = text(request, "sessionId")?.to_owned();
            let input = TurnInput {
                session_id,
                content: text(request, "prompt")?.to_owned(),
                mode: deepseek_tui::AppMode::Agent,
                // The explicit isolated-profile/feature guards above also
                // cover the real product tool loop used by live benchmarks.
                restrict_tools: request["command"] != "infer-agent",
                eval_tool_policy: None,
            };
            let handle = runtime.submit(&input).await?;
            if request["command"] == "begin" {
                return Ok(json!({"turnId":handle.turn_id}));
            }
            complete(runtime, handle).await
        }
        "finish" => {
            complete(
                runtime,
                TurnHandle {
                    session_id: text(request, "sessionId")?.to_owned(),
                    turn_id: text(request, "turnId")?.to_owned(),
                },
            )
            .await
        }
        _ => bail!("unknown acceptance command"),
    }
}

async fn complete(runtime: &EnginePoolRuntime, handle: TurnHandle) -> Result<Value> {
    let result = tokio::time::timeout(
        std::time::Duration::from_secs(90),
        runtime.wait_for_completion(&handle),
    )
    .await??;
    Ok(json!({"status":result.status,"text":result.assistant_text,"error":result.error}))
}
