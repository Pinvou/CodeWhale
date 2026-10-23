use super::*;
use crate::tools::spec::ToolContext;
use std::path::PathBuf;

struct ArtifactRootRestore(Option<PathBuf>);

impl Drop for ArtifactRootRestore {
    fn drop(&mut self) {
        crate::artifacts::set_test_artifact_sessions_root(self.0.take());
    }
}

fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("test runtime")
}

#[test]
fn raw_pdf_production_path_preserves_exact_bytes_without_extractor() {
    let _lock = crate::artifacts::TEST_ARTIFACT_SESSIONS_GUARD
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let temporary = tempfile::tempdir().expect("artifact root");
    let prior =
        crate::artifacts::set_test_artifact_sessions_root(Some(temporary.path().join("sessions")));
    let _restore = ArtifactRootRestore(prior);
    let bytes = b"%PDF-1.7\nraw fixture that is intentionally not parseable\n%%EOF";
    let missing = temporary.path().join("definitely-not-pdftotext");
    let document = runtime()
        .block_on(extract_fetched_document(
            Format::Raw,
            "https://example.com/raw.pdf",
            "application/pdf",
            bytes,
            true,
            None,
            PdfTextCommand::test(missing.as_os_str(), Duration::from_millis(50), None),
        ))
        .expect("signed raw PDF must bypass the missing extractor");
    let (content, artifact) = render_extracted(
        "https://example.com/raw.pdf",
        "application/pdf",
        Format::Raw,
        document,
        bytes,
        &ToolContext::new("."),
    )
    .expect("raw PDF preservation must not require pdftotext");
    let artifact = artifact.expect("raw PDF artifact");
    assert!(content.contains("PDF response saved"), "{content}");
    assert_eq!(std::fs::read(artifact.absolute_path).unwrap(), bytes);
}

#[test]
fn raw_pdf_spoofs_and_contradictory_media_mime_create_no_artifact() {
    let _lock = crate::artifacts::TEST_ARTIFACT_SESSIONS_GUARD
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let temporary = tempfile::tempdir().expect("artifact root");
    let sessions = temporary.path().join("sessions");
    let prior = crate::artifacts::set_test_artifact_sessions_root(Some(sessions.clone()));
    let _restore = ArtifactRootRestore(prior);
    let missing = temporary.path().join("definitely-not-pdftotext");
    let request = PdfTextCommand::test(missing.as_os_str(), Duration::from_millis(50), None);
    let runtime = runtime();

    for (url, content_type, bytes, expected) in [
        (
            "https://example.com/download",
            "application/pdf",
            b"plain text pretending to be a PDF".as_slice(),
            "PDF signature",
        ),
        (
            "https://example.com/spoof.pdf",
            "text/plain",
            b"plain text pretending to be a PDF".as_slice(),
            "PDF signature",
        ),
        (
            "https://example.com/signed",
            "image/png",
            b"%PDF-1.7\n%%EOF".as_slice(),
            "did not match its PDF bytes",
        ),
    ] {
        let error = runtime
            .block_on(extract_fetched_document(
                Format::Raw,
                url,
                content_type,
                bytes,
                true,
                None,
                request,
            ))
            .expect_err("invalid PDF response must fail before raw preservation");
        assert!(error.to_string().contains(expected), "{error}");
    }
    assert!(
        !sessions.exists(),
        "rejected bytes must not create artifacts"
    );
}

#[tokio::test]
async fn fetched_pdf_missing_helper_is_a_failed_typed_outcome() {
    let temporary = tempfile::tempdir().expect("tempdir");
    let missing = temporary.path().join("definitely-not-pdftotext");
    let error = extract_fetched_document(
        Format::Text,
        "https://example.com/document.pdf",
        "application/pdf",
        b"%PDF-1.7\n%%EOF",
        true,
        None,
        PdfTextCommand::test(missing.as_os_str(), Duration::from_secs(1), None),
    )
    .await
    .expect_err("missing helper must fail the fetched PDF call");
    let payload = match &error {
        ToolError::NotAvailable { message } => {
            serde_json::from_str::<Value>(message).expect("structured unavailable payload")
        }
        other => panic!("unexpected error: {other:?}"),
    };
    assert_eq!(payload["type"], "binary_unavailable");
    assert_eq!(
        crate::tools::spec::ToolExecutionOutcome::from_legacy(Err(error)).status,
        crate::tools::spec::ToolTerminalStatus::Failed
    );
}

fn fetched_text(
    body: &str,
    status: u16,
    extra_header: &str,
) -> super::super::web::fetch::FetchedPayload {
    super::super::web::fetch::FetchedPayload {
        url: "https://example.com/retained-response".to_string(),
        status,
        headers: BTreeMap::from([("x-response-evidence".to_string(), extra_header.to_string())]),
        content_type: "text/plain; charset=utf-8".to_string(),
        bytes: std::sync::Arc::new(body.as_bytes().to_vec()),
        truncated: false,
        cache_hit: false,
        retries: 1,
        redirects: 2,
    }
}

#[test]
fn forkguard_fetch_url_full_envelope_round_trips_before_context_cut() {
    use base64::Engine as _;

    let _workshop = crate::tools::large_output_router::active_workshop_test_guard();
    let _lock = crate::artifacts::TEST_ARTIFACT_SESSIONS_GUARD
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let temporary = tempfile::tempdir().unwrap();
    let sessions = temporary.path().join("sessions");
    let prior = crate::artifacts::set_test_artifact_sessions_root(Some(sessions.clone()));
    let _restore = ArtifactRootRestore(prior);
    let window = crate::route_budget::route_context_window_tokens(
        crate::config::ApiProvider::Deepseek,
        "deepseek-v3.2-128k",
        None,
    );
    let budget = crate::route_budget::route_inline_char_budget(Some(window));
    let large_body = format!("BODY-START\n{}\nBODY-END 中文", "界".repeat(budget + 1_024));
    let large_header = format!("HEADER-START{}HEADER-END", "h".repeat(budget + 1_024));

    runtime().block_on(async {
        for (index, (body, status, header)) in [
            (large_body.as_str(), 200, "header evidence"),
            (large_body.as_str(), 404, "error response evidence"),
            ("tiny body", 200, large_header.as_str()),
        ]
        .into_iter()
        .enumerate()
        {
            let session = format!("fetch-full-{index}");
            let context = ToolContext::new(".")
                .with_state_namespace(&session)
                .with_route_context_window(window);
            // Exercise the actual post-network producer pipeline. Transport,
            // SSRF and HTTP byte-limit behavior are unchanged and not mocked
            // into a claim of network execution by this deterministic test.
            let mut result = render_fetched_response(
                fetched_text(body, status, header),
                Format::Raw,
                &[],
                &context,
            )
            .await
            .expect("render actual fetched payload");
            let full: Value =
                serde_json::from_str(&result.content).expect("in-process JSON contract");
            assert_eq!(full["content"], body);
            assert_eq!(full["headers"]["x-response-evidence"], header);
            assert_eq!(full["status"], status);
            assert_eq!(
                full["receipt"],
                json!({"cache_hit": false, "retries": 1, "redirects": 2})
            );
            assert_eq!(full["truncated"], false);
            assert_eq!(result.success, status == 200);
            assert!(result.content.chars().count() > budget);
            assert!(!sessions.join(&session).join("artifacts").exists());
            let original = result.content.clone();

            assert!(
                crate::tools::truncate::preserve_full_output_for_model_context(
                    &mut result,
                    &format!("fetch-{index}"),
                    "fetch_url",
                    &session,
                )
            );
            let reference = result.metadata.as_ref().unwrap()["artifact_id"]
                .as_str()
                .expect("full-envelope ref");
            let view = crate::core::engine::compact_tool_result_for_route(
                crate::config::ApiProvider::Deepseek,
                "deepseek-v3.2-128k",
                None,
                "fetch_url",
                &result,
            );
            assert!(view.chars().count() <= budget);
            assert!(view.contains(&format!("ref=\"{reference}\"")));
            let mut restored = Vec::new();
            while restored.len() < original.len() {
                let retrieved = crate::tools::tool_result_retrieval::RetrieveToolResultTool
                    .execute(
                        json!({"ref": reference, "mode": "bytes", "offset": restored.len(),
                            "length": (original.len() - restored.len()).min(131_072),
                            "max_bytes": 131_072}),
                        &context,
                    )
                    .await
                    .expect("one-hop full-envelope retrieval");
                let payload: Value = serde_json::from_str(&retrieved.content).unwrap();
                let bytes = base64::engine::general_purpose::STANDARD
                    .decode(payload["data"].as_str().unwrap())
                    .unwrap();
                assert!(!bytes.is_empty());
                restored.extend(bytes);
            }
            assert_eq!(restored, original.as_bytes());
            let recovered: Value = serde_json::from_slice(&restored).unwrap();
            assert_eq!(recovered["content"], body);
            assert_eq!(recovered["headers"]["x-response-evidence"], header);
            assert_eq!(
                std::fs::read_dir(sessions.join(&session).join("artifacts"))
                    .unwrap()
                    .count(),
                1
            );
        }
    });
}

#[test]
fn forkguard_fetch_url_small_envelope_stays_parseable_and_artifact_free() {
    let _workshop = crate::tools::large_output_router::active_workshop_test_guard();
    let _lock = crate::artifacts::TEST_ARTIFACT_SESSIONS_GUARD
        .lock()
        .unwrap_or_else(|error| error.into_inner());
    let temporary = tempfile::tempdir().unwrap();
    let sessions = temporary.path().join("sessions");
    let prior = crate::artifacts::set_test_artifact_sessions_root(Some(sessions.clone()));
    let _restore = ArtifactRootRestore(prior);
    let context = ToolContext::new(".")
        .with_state_namespace("fetch-small")
        .with_route_context_window(128_000);
    let body = "short quoted \"正文\"\n";
    let result = runtime()
        .block_on(render_fetched_response(
            fetched_text(body, 200, "small"),
            Format::Raw,
            &[],
            &context,
        ))
        .unwrap();
    let payload: Value =
        serde_json::from_str(&result.content).expect("RLM still receives valid JSON");
    assert_eq!(payload["content"], body);
    assert!(result.metadata.is_none());
    let view = crate::core::engine::compact_tool_result_for_route(
        crate::config::ApiProvider::Deepseek,
        "deepseek-v3.2-128k",
        None,
        "fetch_url",
        &result,
    );
    assert_eq!(view, result.content);
    assert!(!sessions.join("fetch-small").join("artifacts").exists());
}
