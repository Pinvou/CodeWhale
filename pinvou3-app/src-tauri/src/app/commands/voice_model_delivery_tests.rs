//! Real loopback wire tests for the production voice HTTP consumer.
use super::*;
use crate::features::assistant::platform::bridge::Pinvou3Bridge;
use crate::features::assistant::runtime_model::RuntimeModelCredential;
use crate::platform::prefs::{ModelPreset, SavedModel};
use axum::{
    Json, Router,
    extract::State,
    http::{HeaderMap, StatusCode},
    routing::post,
};
use std::sync::{Arc, Mutex};

type Captures = Arc<Mutex<Vec<(HeaderMap, Value)>>>;
async fn server(
    status: StatusCode,
    payload: Value,
) -> (String, Captures, tokio::task::JoinHandle<()>) {
    let captures = Captures::default();
    let app = Router::new()
        .route(
            "/{*path}",
            post(
                |State((captures, status, payload)): State<(Captures, StatusCode, Value)>,
                 headers: HeaderMap,
                 Json(body): Json<Value>| async move {
                    captures.lock().unwrap().push((headers, body));
                    (status, Json(payload))
                },
            ),
        )
        .with_state((captures.clone(), status, payload));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}/v1", listener.local_addr().unwrap());
    let handle = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (origin, captures, handle)
}
fn bridge(origin: String, platform: bool, preset: ModelPreset) -> Pinvou3Bridge {
    let mut model = SavedModel::builtin_account_platform();
    model.base_url = origin;
    model.model = "selected-wire-model".into();
    model.preset = preset;
    model.api_key = "PUBLIC_SYNTHETIC_VOICE_KEY".into();
    if !platform {
        model.id = "controlled-third-party-voice".into();
        model.provider_kind = Some("custom".into());
    }
    let mut bridge = Pinvou3Bridge::test_fixture(Some(model));
    bridge.runtime_model_credential = Some(if platform {
        RuntimeModelCredential::account_platform_access_token("PUBLIC_SYNTHETIC_VOICE_KEY".into())
            .unwrap()
    } else {
        // Pin synthetic credentials so a developer's environment/keychain
        // can never supply a real credential to the loopback fixture.
        RuntimeModelCredential::api_key("PUBLIC_SYNTHETIC_VOICE_KEY").unwrap()
    });
    bridge
}
async fn call(bridge: &Pinvou3Bridge, retry: bool) -> AnyResult<(String, bool)> {
    call_voice_postprocess_model(
        bridge,
        "dictation",
        "input text",
        None,
        None,
        retry,
        if retry { 2 } else { 1 },
        "selected-wire-model",
        Duration::from_secs(3),
    )
    .await
}

#[tokio::test]
async fn platform_voice_actual_generations_use_distinct_operation_ids() {
    let (origin, captures, task) = server(
        StatusCode::OK,
        json!({"choices":[{"message":{"content":"output"},"finish_reason":"stop"}]}),
    )
    .await;
    let bridge = bridge(origin, true, ModelPreset::OpenaiCompatible);
    assert_eq!(call(&bridge, false).await.unwrap().0, "output");
    assert_eq!(call(&bridge, true).await.unwrap().0, "output");
    let rows = captures.lock().unwrap();
    assert_eq!(rows.len(), 2);
    let keys: Vec<_> = rows
        .iter()
        .map(|(h, _)| h.get("Idempotency-Key").unwrap().to_str().unwrap())
        .collect();
    assert!(keys.iter().all(|key| uuid::Uuid::parse_str(key).is_ok()));
    assert_ne!(keys[0], keys[1]);
    assert_ne!(rows[0].1["messages"], rows[1].1["messages"]);
    assert_ne!(rows[0].1["max_tokens"], rows[1].1["max_tokens"]);
    assert!(
        rows.iter()
            .all(|(_, b)| b["stream"] == false && b.get("temperature").is_none())
    );
    task.abort();
}

#[tokio::test]
async fn platform_voice_pending_is_not_completed_empty_output() {
    let (origin, captures, task) = server(
        StatusCode::ACCEPTED,
        json!({"status":"RUNNING","usage_request_id":"0199a100-0001-7000-8000-000000000100"}),
    )
    .await;
    let bridge = bridge(origin, true, ModelPreset::OpenaiCompatible);
    assert!(
        call(&bridge, false)
            .await
            .unwrap_err()
            .to_string()
            .contains("reconciling")
    );
    assert_eq!(captures.lock().unwrap().len(), 1);
    task.abort();
}

#[tokio::test]
async fn platform_voice_missing_completion_is_not_content_retry_input() {
    let (origin, captures, task) = server(StatusCode::OK, json!({"status":"REVIEW"})).await;
    let bridge = bridge(origin, true, ModelPreset::OpenaiCompatible);
    assert!(
        call(&bridge, false)
            .await
            .unwrap_err()
            .to_string()
            .contains("no completion")
    );
    assert_eq!(captures.lock().unwrap().len(), 1);
    task.abort();
}

#[tokio::test]
async fn completed_empty_and_length_remain_successful_content_results() {
    for (content, finish, expected) in [("", "stop", false), ("partial", "length", true)] {
        let (origin, _, task) = server(
            StatusCode::OK,
            json!({"choices":[{"message":{"content":content},"finish_reason":finish}]}),
        )
        .await;
        let bridge = bridge(origin, true, ModelPreset::OpenaiCompatible);
        assert_eq!(
            call(&bridge, false).await.unwrap(),
            (content.into(), expected)
        );
        task.abort();
    }
}

#[tokio::test]
async fn third_party_chat_and_anthropic_keep_existing_wire_auth() {
    for (preset, payload) in [
        (
            ModelPreset::OpenaiCompatible,
            json!({"choices":[{"message":{"content":"output"},"finish_reason":"stop"}]}),
        ),
        (
            ModelPreset::Anthropic,
            json!({"content":[{"type":"text","text":"output"}],"stop_reason":"end_turn"}),
        ),
    ] {
        let (origin, captures, task) = server(StatusCode::OK, payload).await;
        let bridge = bridge(origin, false, preset);
        assert_eq!(call(&bridge, false).await.unwrap().0, "output");
        let rows = captures.lock().unwrap();
        assert_eq!(rows.len(), 1);
        assert!(rows[0].0.get("Idempotency-Key").is_none());
        if preset == ModelPreset::Anthropic {
            assert!(rows[0].0.get("x-api-key").is_some());
            assert!(rows[0].1.get("system").is_some());
        } else {
            assert!(rows[0].0.get("authorization").is_some());
        }
        task.abort();
    }
}
