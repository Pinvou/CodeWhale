//! One host-independent boundary for A-authenticated and B-local input.
//!
//! The injected port adapts the existing DEV-13 session map, ledger, turn
//! lock, executor and journal. This use case owns none of those states.

use serde_json::Value;

use super::ports::{UseCaseError, UseCaseFuture};
use crate::features::files::file_ingest::PublishedAttachment;
use crate::features::sessions::remote_proxy::schema::valid_remote_identifier;

pub(crate) trait SubmitControlledUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn submit(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}

#[derive(Debug)]
pub(crate) enum ControlledInput {
    Remote(RemoteControlledInput),
    Local(LocalControlledInput),
}

#[derive(Debug)]
pub(crate) struct RemoteControlledInput {
    pub(crate) remote_ref: String,
    pub(crate) generation: String,
    pub(crate) title: String,
    pub(crate) code_mode: bool,
    /// Immutable A-side identifiers, extracted from the authenticated frame.
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) input_digest: String,
    pub(crate) payload: Value,
}

pub(crate) struct LocalControlledInput {
    pub(crate) remote_ref: String,
    pub(crate) generation: String,
    pub(crate) expected_session_id: String,
    pub(crate) title: String,
    pub(crate) code_mode: bool,
    /// Present only for a code lane and resolved from B-owned state, never
    /// accepted from the local WebView command.
    pub(crate) workspace_id: Option<String>,
    /// The WebView supplies text and local selections only. Rust has already
    /// resolved attachment paths into B-local, non-serializable descriptors;
    /// task/request IDs, attachment handles, digest and lease fields all come
    /// from Rust.
    pub(crate) text: String,
    pub(crate) skill_ids: Vec<String>,
    pub(crate) attachments: Vec<PublishedAttachment>,
}

impl std::fmt::Debug for LocalControlledInput {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("LocalControlledInput")
            .field("remote_ref", &self.remote_ref)
            .field("generation", &self.generation)
            .field("expected_session_id", &self.expected_session_id)
            .field("code_mode", &self.code_mode)
            .field("workspace_id", &self.workspace_id)
            .field("text_bytes", &self.text.len())
            .field("skill_ids", &self.skill_ids)
            .field("attachment_count", &self.attachments.len())
            .finish()
    }
}

impl ControlledInput {
    fn remote_ref(&self) -> &str {
        match self {
            Self::Remote(input) => &input.remote_ref,
            Self::Local(input) => &input.remote_ref,
        }
    }

    fn generation(&self) -> &str {
        match self {
            Self::Remote(input) => &input.generation,
            Self::Local(input) => &input.generation,
        }
    }
}

pub(crate) struct PreparedControlledTask {
    pub(crate) remote_ref: String,
    pub(crate) b_session_id: String,
    pub(crate) task_id: String,
    pub(crate) client_request_id: String,
    pub(crate) input_digest: String,
    pub(crate) payload: Value,
    /// Non-serializable B-local descriptors paired with the opaque IDs in
    /// `payload.input.attachment_ids`. Remote input uses the upload owner and
    /// therefore leaves this as `None`; B-local input always sets it.
    pub(crate) local_attachments: Option<Vec<PublishedAttachment>>,
}

fn payload_matches_identity(
    payload: &Value,
    session: &str,
    task: &str,
    request: &str,
    digest: &str,
) -> bool {
    let field = |name: &str| payload.get(name).and_then(Value::as_str);
    field("session_id") == Some(session)
        && field("task_id") == Some(task)
        && field("client_request_id") == Some(request)
        && field("input_digest") == Some(digest)
}

#[derive(Debug)]
pub(crate) struct ControlledSubmissionResult {
    pub(crate) b_session_id: String,
    pub(crate) task_id: String,
    pub(crate) status: Value,
}

/// Implemented by a stateless app adapter over the frozen DEV-13 service.
/// The same `admit_and_dispatch` method is used for both sources.
pub(crate) trait ControlledSubmissionPort: Send + Sync {
    /// Resolve the B session against the current server-owned mapping before
    /// `ensure_mapping` can allocate anything. UI strings are never authority.
    fn validate_local_context(&self, input: &LocalControlledInput) -> Result<(), UseCaseError>;

    /// Authenticated A task may create a B session or claim a switched Unknown
    /// lane using the frozen task shape and current live lease.
    fn ensure_remote_mapping(&self, input: &RemoteControlledInput) -> Result<String, UseCaseError>;

    /// B's existing ChatView ID is lookup-only: it must never create a second
    /// session or claim an Unknown lane after a concurrent switch.
    fn ensure_local_mapping(&self, input: &LocalControlledInput) -> Result<String, UseCaseError>;

    fn prepare_remote(
        &self,
        input: RemoteControlledInput,
        b_session_id: &str,
    ) -> Result<PreparedControlledTask, UseCaseError>;

    fn prepare_local(
        &self,
        input: LocalControlledInput,
        b_session_id: &str,
    ) -> Result<PreparedControlledTask, UseCaseError>;

    fn admit_and_dispatch(&self, prepared: PreparedControlledTask) -> UseCaseFuture<'_, Value>;
}

pub(crate) struct ControlledSubmission<P> {
    port: P,
}

impl<P> ControlledSubmission<P> {
    pub(crate) fn new(port: P) -> Self {
        Self { port }
    }
}

impl<P: ControlledSubmissionPort> SubmitControlledUseCase for ControlledSubmission<P> {
    type Request = ControlledInput;
    type Response = ControlledSubmissionResult;

    fn submit(&self, request: ControlledInput) -> UseCaseFuture<'_, ControlledSubmissionResult> {
        Box::pin(async move {
            if !valid_remote_identifier(request.remote_ref())
                || !valid_remote_identifier(request.generation())
            {
                return Err(UseCaseError::new("task_session_not_found", false));
            }
            // Cheap source-shape checks precede allocation. Session mapping
            // still precedes capability/model preparation and admission.
            match &request {
                ControlledInput::Remote(input)
                    if input.task_id.is_empty()
                        || input.client_request_id.is_empty()
                        || input.input_digest.is_empty()
                        || input.payload.get("task_id").and_then(Value::as_str)
                            != Some(input.task_id.as_str())
                        || input
                            .payload
                            .get("client_request_id")
                            .and_then(Value::as_str)
                            != Some(input.client_request_id.as_str())
                        || input.payload.get("input_digest").and_then(Value::as_str)
                            != Some(input.input_digest.as_str()) =>
                {
                    return Err(UseCaseError::new("task_request_invalid", false));
                }
                ControlledInput::Local(input) if input.text.trim().is_empty() => {
                    return Err(UseCaseError::new("task_input_invalid", false));
                }
                _ => {}
            }
            if let ControlledInput::Local(input) = &request {
                self.port.validate_local_context(input)?;
            }
            // Map before any capability/model preparation, even for a B-local
            // first turn. DEV-13 serializes create/lookup under its own lock.
            let b_session_id = match &request {
                ControlledInput::Remote(input) => self.port.ensure_remote_mapping(input)?,
                ControlledInput::Local(input) => self.port.ensure_local_mapping(input)?,
            };
            if b_session_id.is_empty() {
                return Err(UseCaseError::new("task_session_not_found", false));
            }
            let request_remote_ref = request.remote_ref().to_owned();
            let (prepared, a_identity) = match request {
                ControlledInput::Remote(input) => {
                    let identity = (
                        input.task_id.clone(),
                        input.client_request_id.clone(),
                        input.input_digest.clone(),
                    );
                    (
                        self.port.prepare_remote(input, &b_session_id)?,
                        Some(identity),
                    )
                }
                ControlledInput::Local(input) => {
                    if input.expected_session_id != b_session_id {
                        return Err(UseCaseError::new("task_session_not_found", false));
                    }
                    (self.port.prepare_local(input, &b_session_id)?, None)
                }
            };
            if prepared.b_session_id != b_session_id
                || prepared.remote_ref != request_remote_ref
                || prepared.task_id.is_empty()
                || prepared.client_request_id.is_empty()
                || prepared.input_digest.is_empty()
                || !payload_matches_identity(
                    &prepared.payload,
                    &b_session_id,
                    &prepared.task_id,
                    &prepared.client_request_id,
                    &prepared.input_digest,
                )
                || a_identity.is_some_and(|(task, request, digest)| {
                    prepared.task_id != task
                        || prepared.client_request_id != request
                        || prepared.input_digest != digest
                })
            {
                return Err(UseCaseError::new("task_request_invalid", false));
            }
            let task_id = prepared.task_id.clone();
            let status = self.port.admit_and_dispatch(prepared).await?;
            if status.get("task_id").and_then(Value::as_str) != Some(task_id.as_str())
                || status.get("session_id").and_then(Value::as_str) != Some(b_session_id.as_str())
            {
                return Err(UseCaseError::new("task_status_binding_invalid", false));
            }
            Ok(ControlledSubmissionResult {
                b_session_id,
                task_id,
                status,
            })
        })
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    use parking_lot::Mutex;
    use serde_json::json;

    use super::*;

    #[derive(Default)]
    struct FakeDev13Adapter {
        trace: Mutex<Vec<&'static str>>,
        admitted: Mutex<Vec<(String, String, String)>>,
        created: AtomicUsize,
        mapped: AtomicBool,
        active: AtomicBool,
        local_number: AtomicUsize,
        local_remap_on_lookup: AtomicBool,
        mutate_remote_id: AtomicBool,
        wrong_status_binding: AtomicBool,
    }

    impl ControlledSubmissionPort for FakeDev13Adapter {
        fn validate_local_context(&self, input: &LocalControlledInput) -> Result<(), UseCaseError> {
            self.trace.lock().push("validate_local_context");
            if input.text.trim().len() > 64 * 1024 {
                return Err(UseCaseError::new("task_input_invalid", false));
            }
            if input.remote_ref != "ref-1"
                || input.generation != "generation-1"
                || input.expected_session_id != "b-session-1"
                || !self.mapped.load(Ordering::SeqCst)
            {
                return Err(UseCaseError::new("task_session_not_found", false));
            }
            Ok(())
        }

        fn ensure_remote_mapping(
            &self,
            input: &RemoteControlledInput,
        ) -> Result<String, UseCaseError> {
            self.trace.lock().push("ensure_remote_mapping");
            if input.remote_ref != "ref-1" || input.generation != "generation-1" {
                return Err(UseCaseError::new("task_session_not_found", false));
            }
            if !self.mapped.swap(true, Ordering::SeqCst) {
                self.created.fetch_add(1, Ordering::SeqCst);
            }
            Ok("b-session-1".into())
        }

        fn ensure_local_mapping(
            &self,
            input: &LocalControlledInput,
        ) -> Result<String, UseCaseError> {
            self.trace.lock().push("ensure_local_mapping");
            if !self.mapped.load(Ordering::SeqCst) || input.expected_session_id != "b-session-1" {
                return Err(UseCaseError::new("task_session_not_found", false));
            }
            if self.local_remap_on_lookup.load(Ordering::SeqCst) {
                return Ok("b-session-2".into());
            }
            Ok("b-session-1".into())
        }

        fn prepare_remote(
            &self,
            input: RemoteControlledInput,
            b_session_id: &str,
        ) -> Result<PreparedControlledTask, UseCaseError> {
            self.trace.lock().push("prepare_remote");
            let mut payload = input.payload;
            payload["session_id"] = b_session_id.into();
            let task_id = if self.mutate_remote_id.load(Ordering::SeqCst) {
                "rewritten-task".to_owned()
            } else {
                input.task_id
            };
            payload["task_id"] = task_id.clone().into();
            Ok(PreparedControlledTask {
                remote_ref: input.remote_ref,
                b_session_id: b_session_id.into(),
                task_id,
                client_request_id: input.client_request_id,
                input_digest: input.input_digest,
                payload,
                local_attachments: None,
            })
        }

        fn prepare_local(
            &self,
            input: LocalControlledInput,
            b_session_id: &str,
        ) -> Result<PreparedControlledTask, UseCaseError> {
            self.trace.lock().push("prepare_local");
            let number = self.local_number.fetch_add(1, Ordering::SeqCst);
            let task_id = format!("b-task-{number}");
            let client_request_id = format!("b-request-{number}");
            let input_digest = "a".repeat(64);
            let payload = json!({
                "session_id": b_session_id,
                "task_id": task_id,
                "client_request_id": client_request_id,
                "input_digest": input_digest,
                "input": { "text": input.text },
            });
            Ok(PreparedControlledTask {
                remote_ref: input.remote_ref,
                b_session_id: b_session_id.into(),
                task_id,
                client_request_id,
                input_digest,
                payload,
                local_attachments: None,
            })
        }

        fn admit_and_dispatch(&self, prepared: PreparedControlledTask) -> UseCaseFuture<'_, Value> {
            Box::pin(async move {
                self.trace.lock().push("admit_and_dispatch");
                if self.active.swap(true, Ordering::SeqCst) {
                    return Err(UseCaseError::new("task_session_busy", true));
                }
                self.admitted.lock().push((
                    prepared.task_id.clone(),
                    prepared.client_request_id.clone(),
                    prepared.b_session_id.clone(),
                ));
                let session_id = if self.wrong_status_binding.load(Ordering::SeqCst) {
                    "another-session"
                } else {
                    &prepared.b_session_id
                };
                Ok(json!({
                    "status": "admitted",
                    "task_id": prepared.task_id,
                    "session_id": session_id,
                }))
            })
        }
    }

    fn remote() -> ControlledInput {
        ControlledInput::Remote(RemoteControlledInput {
            remote_ref: "ref-1".into(),
            generation: "generation-1".into(),
            title: String::new(),
            code_mode: false,
            task_id: "a-task-1".into(),
            client_request_id: "a-request-1".into(),
            input_digest: "b".repeat(64),
            payload: json!({
                "session_id": "ref-1",
                "task_id": "a-task-1",
                "client_request_id": "a-request-1",
                "input_digest": "b".repeat(64),
            }),
        })
    }

    fn local() -> ControlledInput {
        ControlledInput::Local(LocalControlledInput {
            remote_ref: "ref-1".into(),
            generation: "generation-1".into(),
            expected_session_id: "b-session-1".into(),
            title: String::new(),
            code_mode: false,
            workspace_id: None,
            text: "B first turn".into(),
            skill_ids: Vec::new(),
            attachments: Vec::new(),
        })
    }

    #[tokio::test]
    async fn either_source_first_uses_one_mapping_before_preparation_and_one_admission() {
        for first_local in [false, true] {
            let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
            if first_local {
                // The B ChatView has a B session only because the capability
                // request already established and projected the mapping.
                use_case.port.mapped.store(true, Ordering::SeqCst);
                use_case.port.created.store(1, Ordering::SeqCst);
            }
            let first = if first_local { local() } else { remote() };
            let result = use_case.submit(first).await.unwrap();
            assert_eq!(result.b_session_id, "b-session-1");
            assert_eq!(result.status["session_id"], "b-session-1");
            if first_local {
                assert!(result.task_id.starts_with("b-task-"));
            } else {
                assert_eq!(result.task_id, "a-task-1");
            }
            assert_eq!(use_case.port.created.load(Ordering::SeqCst), 1);
            let admitted = use_case.port.admitted.lock().clone();
            assert_eq!(admitted.len(), 1);
            assert_eq!(admitted[0].2, "b-session-1");
            if first_local {
                assert!(admitted[0].0.starts_with("b-task-"));
                assert!(admitted[0].1.starts_with("b-request-"));
            } else {
                assert_eq!(admitted[0].0, "a-task-1");
                assert_eq!(admitted[0].1, "a-request-1");
            }
            let trace = use_case.port.trace.lock().clone();
            let expected = if first_local {
                vec![
                    "validate_local_context",
                    "ensure_local_mapping",
                    "prepare_local",
                    "admit_and_dispatch",
                ]
            } else {
                vec![
                    "ensure_remote_mapping",
                    "prepare_remote",
                    "admit_and_dispatch",
                ]
            };
            assert_eq!(trace, expected);

            let second = if first_local { remote() } else { local() };
            let error = use_case.submit(second).await.unwrap_err();
            assert_eq!(error.code(), "task_session_busy");
            assert!(error.is_retryable());
            assert_eq!(use_case.port.created.load(Ordering::SeqCst), 1);
            // The caller retains its input and can retry after the DEV-13
            // terminal transition releases the per-session turn lock.
            use_case.port.active.store(false, Ordering::SeqCst);
            let retry = if first_local { remote() } else { local() };
            let result = use_case.submit(retry).await.unwrap();
            assert_eq!(result.b_session_id, "b-session-1");
            assert_eq!(use_case.port.created.load(Ordering::SeqCst), 1);
            assert_eq!(use_case.port.admitted.lock().len(), 2);
        }
    }

    #[tokio::test]
    async fn remote_ids_are_immutable_and_local_cannot_claim_another_session() {
        let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
        let remote_result = use_case.submit(remote()).await.unwrap();
        assert_eq!(remote_result.task_id, "a-task-1");
        use_case.port.active.store(false, Ordering::SeqCst);
        use_case.port.mutate_remote_id.store(true, Ordering::SeqCst);
        assert_eq!(
            use_case.submit(remote()).await.unwrap_err().code(),
            "task_request_invalid"
        );
        let mut forged = remote();
        if let ControlledInput::Remote(input) = &mut forged {
            input.client_request_id = "forged".into();
        }
        assert_eq!(
            use_case.submit(forged).await.unwrap_err().code(),
            "task_request_invalid"
        );
        let mut wrong_local = local();
        if let ControlledInput::Local(input) = &mut wrong_local {
            input.expected_session_id = "another-session".into();
        }
        assert_eq!(
            use_case.submit(wrong_local).await.unwrap_err().code(),
            "task_session_not_found"
        );
        assert_eq!(
            use_case
                .port
                .trace
                .lock()
                .iter()
                .filter(|step| **step == "admit_and_dispatch")
                .count(),
            1
        );
    }

    #[tokio::test]
    async fn status_must_be_bound_to_the_same_authoritative_task_and_session() {
        let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
        use_case
            .port
            .wrong_status_binding
            .store(true, Ordering::SeqCst);
        assert_eq!(
            use_case.submit(remote()).await.unwrap_err().code(),
            "task_status_binding_invalid"
        );
    }

    #[tokio::test]
    async fn malformed_input_cannot_allocate_a_session_mapping() {
        let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
        let mut forged = remote();
        if let ControlledInput::Remote(input) = &mut forged {
            input.task_id.clear();
        }
        assert_eq!(
            use_case.submit(forged).await.unwrap_err().code(),
            "task_request_invalid"
        );
        let mut blank = local();
        if let ControlledInput::Local(input) = &mut blank {
            input.text = "   ".into();
        }
        assert_eq!(
            use_case.submit(blank).await.unwrap_err().code(),
            "task_input_invalid"
        );
        assert_eq!(use_case.port.created.load(Ordering::SeqCst), 0);
        assert!(use_case.port.trace.lock().is_empty());
    }

    #[tokio::test]
    async fn forged_b_context_fails_before_mapping_or_preparation() {
        let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
        assert_eq!(
            use_case.submit(local()).await.unwrap_err().code(),
            "task_session_not_found"
        );
        let mut forged = local();
        if let ControlledInput::Local(input) = &mut forged {
            input.expected_session_id = "another-session".into();
        }
        assert_eq!(
            use_case.submit(forged).await.unwrap_err().code(),
            "task_session_not_found"
        );
        assert_eq!(use_case.port.created.load(Ordering::SeqCst), 0);
        assert_eq!(
            use_case.port.trace.lock().as_slice(),
            &["validate_local_context", "validate_local_context"]
        );
        for mutate in ["remote_ref", "generation", "oversized"] {
            let mut input = local();
            if let ControlledInput::Local(local) = &mut input {
                match mutate {
                    "remote_ref" => local.remote_ref = "bad/slash".into(),
                    "generation" => local.generation = "bad/slash".into(),
                    _ => local.text = "é".repeat(32 * 1024 + 1),
                }
            }
            assert!(use_case.submit(input).await.is_err(), "{mutate}");
            assert_eq!(use_case.port.created.load(Ordering::SeqCst), 0);
        }
    }

    #[tokio::test]
    async fn switched_b_mapping_cannot_redirect_an_expected_chat_session() {
        let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
        use_case.port.mapped.store(true, Ordering::SeqCst);
        use_case
            .port
            .local_remap_on_lookup
            .store(true, Ordering::SeqCst);
        assert_eq!(
            use_case.submit(local()).await.unwrap_err().code(),
            "task_session_not_found"
        );
        assert_eq!(use_case.port.created.load(Ordering::SeqCst), 0);
        assert!(use_case.port.admitted.lock().is_empty());
        assert_eq!(
            use_case.port.trace.lock().as_slice(),
            &["validate_local_context", "ensure_local_mapping"]
        );
    }

    #[tokio::test]
    async fn oversized_b_input_never_reaches_mapping_even_when_bound() {
        let use_case = ControlledSubmission::new(FakeDev13Adapter::default());
        use_case.port.mapped.store(true, Ordering::SeqCst);
        use_case.port.created.store(1, Ordering::SeqCst);
        let mut oversized = local();
        if let ControlledInput::Local(input) = &mut oversized {
            input.text = "é".repeat(32 * 1024 + 1);
        }
        assert_eq!(
            use_case.submit(oversized).await.unwrap_err().code(),
            "task_input_invalid"
        );
        assert_eq!(
            use_case.port.trace.lock().as_slice(),
            &["validate_local_context"]
        );
        assert_eq!(use_case.port.created.load(Ordering::SeqCst), 1);
        assert_eq!(
            use_case.port.trace.lock().as_slice(),
            &["validate_local_context"]
        );
    }
}
