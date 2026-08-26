//! `image_analyze` tool — analyze images using a dedicated vision model.

use std::path::{Component, Path, PathBuf};
use std::time::Duration;

use async_trait::async_trait;
use base64::{Engine as _, engine::general_purpose::STANDARD as BASE64};
use futures_util::StreamExt;
use serde_json::{Value, json};

use crate::client::{ERROR_BODY_MAX_BYTES, bounded_error_text};
use crate::config::VisionModelConfig;
use crate::llm_client::{
    LlmError, RetryConfig, extract_retry_after, sanitize_http_error_body, with_retry,
};
use crate::tools::spec::{
    ToolCapability, ToolContext, ToolError, ToolResult, ToolSpec, required_str,
};

const DEFAULT_VISION_MAX_OUTPUT_TOKENS: u32 = 4096;
const DEFAULT_VISION_REQUEST_TIMEOUT_SECS: u64 = 120;
const MAX_VISION_REQUEST_TIMEOUT_SECS: u64 = 3600;
const MAX_VISION_RESPONSE_BYTES: usize = 8 * 1024 * 1024;

#[derive(Default)]
struct SseLineBuffer {
    pending: Vec<u8>,
    scan_from: usize,
}

impl SseLineBuffer {
    fn push(&mut self, chunk: &[u8]) -> Result<Vec<String>, ToolError> {
        if self.pending.len().saturating_add(chunk.len()) > MAX_VISION_RESPONSE_BYTES {
            return Err(ToolError::execution_failed(format!(
                "Vision SSE frame exceeded {MAX_VISION_RESPONSE_BYTES} bytes"
            )));
        }
        self.pending.extend_from_slice(chunk);

        let mut lines = Vec::new();
        let mut line_start = 0;
        for (index, byte) in self.pending.iter().enumerate().skip(self.scan_from) {
            if *byte == b'\n' {
                lines.push(String::from_utf8_lossy(&self.pending[line_start..index]).into_owned());
                line_start = index + 1;
            }
        }
        if line_start > 0 {
            self.pending.drain(..line_start);
        }
        self.scan_from = self.pending.len();
        Ok(lines)
    }

    fn finish(&mut self) -> Option<String> {
        (!self.pending.is_empty())
            .then(|| String::from_utf8_lossy(&std::mem::take(&mut self.pending)).into_owned())
    }
}

#[derive(Default)]
struct VisionStreamState {
    content: String,
    saw_data_event: bool,
    completed: bool,
    truncated: bool,
}

fn process_sse_line(line: &str, state: &mut VisionStreamState) -> bool {
    let Some(data) = line.trim().strip_prefix("data:").map(str::trim) else {
        return false;
    };
    state.saw_data_event = true;
    if data == "[DONE]" {
        state.completed = true;
        return true;
    }

    let Ok(event) = serde_json::from_str::<Value>(data) else {
        state.truncated = true;
        return false;
    };
    if let Some(delta) = event
        .pointer("/choices/0/delta/content")
        .and_then(Value::as_str)
    {
        state.content.push_str(delta);
    }
    if let Some(reason) = event
        .pointer("/choices/0/finish_reason")
        .and_then(Value::as_str)
    {
        state.completed = true;
        state.truncated |= reason == "length";
    }
    false
}

pub struct ImageAnalyzeTool {
    config: VisionModelConfig,
    client: reqwest::Client,
}

impl ImageAnalyzeTool {
    #[must_use]
    pub fn new(config: VisionModelConfig) -> Self {
        let client = crate::tls::reqwest_client_builder()
            .connect_timeout(Duration::from_secs(30))
            .build()
            .expect("Failed to build HTTP client");
        Self { config, client }
    }

    async fn read_image_file(path: &Path) -> Result<(String, String), ToolError> {
        let bytes = tokio::fs::read(path)
            .await
            .map_err(|e| ToolError::execution_failed(format!("Failed to read image file: {e}")))?;

        let mime_type = Self::detect_mime_type(path)?;
        let base64_data = BASE64.encode(&bytes);
        Ok((base64_data, mime_type))
    }

    fn resolve_image_path(workspace: &Path, image_path: &str) -> Result<PathBuf, ToolError> {
        let image_path_buf = Path::new(image_path);
        if image_path_buf.components().any(|c| {
            matches!(
                c,
                Component::Prefix(_) | Component::RootDir | Component::ParentDir
            )
        }) {
            return Err(ToolError::execution_failed(
                "image_path must be a relative path within the workspace and cannot escape it.",
            ));
        }

        let workspace = workspace.canonicalize().map_err(|e| {
            ToolError::execution_failed(format!("Failed to resolve workspace path: {e}"))
        })?;
        let candidate = workspace.join(image_path_buf);
        let resolved = candidate.canonicalize().map_err(|e| {
            ToolError::execution_failed(format!("Failed to resolve image file: {e}"))
        })?;
        if !resolved.starts_with(&workspace) {
            return Err(ToolError::execution_failed(
                "image_path must resolve within the workspace and cannot escape it.",
            ));
        }
        Ok(resolved)
    }

    fn detect_mime_type(path: &Path) -> Result<String, ToolError> {
        let extension = path
            .extension()
            .and_then(|e| e.to_str())
            .unwrap_or("")
            .to_lowercase();

        match extension.as_str() {
            "png" => Ok("image/png".to_string()),
            "jpg" | "jpeg" => Ok("image/jpeg".to_string()),
            "gif" => Ok("image/gif".to_string()),
            "webp" => Ok("image/webp".to_string()),
            "bmp" => Ok("image/bmp".to_string()),
            _ => Err(ToolError::execution_failed(format!(
                "Unsupported image format: {extension}"
            ))),
        }
    }

    fn base_url(&self) -> String {
        self.config
            .base_url
            .clone()
            .unwrap_or_else(|| "https://api.openai.com/v1".to_string())
    }

    fn api_key(&self) -> String {
        self.config.api_key.clone().unwrap_or_default()
    }

    fn is_xiaomi_mimo_model(model: &str) -> bool {
        let normalized = model.trim().to_ascii_lowercase();
        let normalized = normalized.strip_prefix("xiaomi/").unwrap_or(&normalized);
        normalized.starts_with("mimo-")
    }

    fn uses_max_completion_tokens(config: &VisionModelConfig) -> bool {
        if Self::is_xiaomi_mimo_model(&config.model) {
            return true;
        }

        let base_url = config.base_url.as_deref().unwrap_or_default();
        let Ok(url) = reqwest::Url::parse(base_url) else {
            return false;
        };
        let Some(domain) = url.domain() else {
            return false;
        };

        domain.eq_ignore_ascii_case("xiaomimimo.com")
            || domain.to_ascii_lowercase().ends_with(".xiaomimimo.com")
    }

    fn request_payload(&self, prompt: &str, image_data: &str, mime_type: &str) -> Value {
        let mut payload = json!({
            "model": self.config.model,
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": prompt},
                        {
                            "type": "image_url",
                            "image_url": {
                                "url": format!("data:{};base64,{}", mime_type, image_data)
                            }
                        }
                    ]
                }
            ],
            "temperature": 0.7
        });

        let token_limit_field = if Self::uses_max_completion_tokens(&self.config) {
            "max_completion_tokens"
        } else {
            "max_tokens"
        };
        payload[token_limit_field] = json!(DEFAULT_VISION_MAX_OUTPUT_TOKENS);
        if let Some(stream) = self.config.stream {
            payload["stream"] = json!(stream);
        }

        payload
    }

    fn request_timeout(&self) -> Duration {
        let seconds = self
            .config
            .request_timeout_secs
            .filter(|seconds| *seconds > 0)
            .unwrap_or(DEFAULT_VISION_REQUEST_TIMEOUT_SECS)
            .min(MAX_VISION_REQUEST_TIMEOUT_SECS);
        Duration::from_secs(seconds)
    }

    fn retry_config(&self) -> RetryConfig {
        RetryConfig {
            max_retries: 3,
            initial_delay: 1.0,
            max_delay: 30.0,
            enabled: self.config.retry_on_transient_errors.unwrap_or(true),
            ..Default::default()
        }
    }

    fn parse_non_streaming_body(&self, body: &[u8]) -> Result<(String, String, bool), ToolError> {
        let json: Value = serde_json::from_slice(body).map_err(|error| {
            ToolError::execution_failed(format!("Vision API returned invalid JSON: {error}"))
        })?;
        let content = json
            .pointer("/choices/0/message/content")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string();
        let model = json
            .get("model")
            .and_then(Value::as_str)
            .unwrap_or(&self.config.model)
            .to_string();
        let truncated = json
            .pointer("/choices/0/finish_reason")
            .and_then(Value::as_str)
            == Some("length");
        Ok((content, model, truncated))
    }

    fn vision_result(
        &self,
        content: String,
        model: String,
        truncated: bool,
    ) -> Result<ToolResult, ToolError> {
        if content.trim().is_empty() {
            return Err(ToolError::execution_failed(
                "Vision API returned no usable content",
            ));
        }

        let mut result = json!({
            "analysis": content,
            "model": model,
        });
        if truncated {
            result["truncated"] = json!(true);
        }
        ToolResult::json(&result).map_err(|error| {
            ToolError::execution_failed(format!("Failed to serialize result: {error}"))
        })
    }

    async fn read_bounded_body(
        response: reqwest::Response,
        deadline: tokio::time::Instant,
    ) -> Result<Vec<u8>, ToolError> {
        let mut body = Vec::new();
        let mut stream = response.bytes_stream();
        loop {
            let chunk = tokio::time::timeout_at(deadline, stream.next())
                .await
                .map_err(|_| ToolError::execution_failed("Vision API response timed out"))?;
            let Some(chunk) = chunk else { break };
            let chunk = chunk.map_err(|error| {
                ToolError::execution_failed(format!("Failed to read Vision API response: {error}"))
            })?;
            if body.len().saturating_add(chunk.len()) > MAX_VISION_RESPONSE_BYTES {
                return Err(ToolError::execution_failed(format!(
                    "Vision API response exceeded {MAX_VISION_RESPONSE_BYTES} bytes"
                )));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(body)
    }

    async fn execute_non_streaming(
        &self,
        response: reqwest::Response,
        deadline: tokio::time::Instant,
    ) -> Result<ToolResult, ToolError> {
        let body = Self::read_bounded_body(response, deadline).await?;
        let (content, model, truncated) = self.parse_non_streaming_body(&body)?;
        self.vision_result(content, model, truncated)
    }

    async fn execute_streaming(
        &self,
        response: reqwest::Response,
        deadline: tokio::time::Instant,
    ) -> Result<ToolResult, ToolError> {
        let mut raw_body = Vec::new();
        let mut line_buffer = SseLineBuffer::default();
        let mut state = VisionStreamState::default();
        let mut stream = response.bytes_stream();

        'response: loop {
            let chunk = match tokio::time::timeout_at(deadline, stream.next()).await {
                Ok(Some(Ok(chunk))) => chunk,
                Ok(Some(Err(_))) | Err(_) => {
                    state.truncated = true;
                    break;
                }
                Ok(None) => break,
            };
            if raw_body.len().saturating_add(chunk.len()) > MAX_VISION_RESPONSE_BYTES {
                return Err(ToolError::execution_failed(format!(
                    "Vision API response exceeded {MAX_VISION_RESPONSE_BYTES} bytes"
                )));
            }
            raw_body.extend_from_slice(&chunk);
            for line in line_buffer.push(&chunk)? {
                if process_sse_line(&line, &mut state) {
                    break 'response;
                }
            }
        }

        if !state.completed
            && let Some(line) = line_buffer.finish()
        {
            let _ = process_sse_line(&line, &mut state);
        }

        if !state.saw_data_event {
            let (content, model, truncated) = self.parse_non_streaming_body(&raw_body)?;
            return self.vision_result(content, model, truncated || state.truncated);
        }

        state.truncated |= !state.completed;
        self.vision_result(state.content, self.config.model.clone(), state.truncated)
    }
}

#[async_trait]
impl ToolSpec for ImageAnalyzeTool {
    fn name(&self) -> &str {
        "image_analyze"
    }

    fn description(&self) -> &str {
        "Analyze an image using the configured vision model. \
         Supports PNG, JPEG, GIF, WebP, and BMP formats."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "image_path": {
                    "type": "string",
                    "description": "Path to the image file to analyze"
                },
                "prompt": {
                    "type": "string",
                    "description": "Optional prompt to guide the analysis."
                }
            },
            "required": ["image_path"]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::ReadOnly]
    }

    async fn execute(&self, input: Value, context: &ToolContext) -> Result<ToolResult, ToolError> {
        let image_path = required_str(&input, "image_path")?;
        let prompt = input
            .get("prompt")
            .and_then(|v| v.as_str())
            .unwrap_or("Describe this image in detail.");

        let resolved_path = Self::resolve_image_path(&context.workspace, image_path)?;
        let (image_data, mime_type) = Self::read_image_file(&resolved_path).await?;

        let payload = self.request_payload(prompt, &image_data, &mime_type);

        let url = format!("{}/chat/completions", self.base_url());
        let api_key = self.api_key();

        let retry_config = self.retry_config();
        let deadline = tokio::time::Instant::now() + self.request_timeout();

        let response = tokio::time::timeout_at(
            deadline,
            with_retry(
                &retry_config,
                || {
                    let client = self.client.clone();
                    let url = url.clone();
                    let api_key = api_key.clone();
                    let payload = payload.clone();
                    async move {
                        let response = client
                            .post(&url)
                            .header("Content-Type", "application/json")
                            .header("Authorization", format!("Bearer {api_key}"))
                            .json(&payload)
                            .send()
                            .await
                            .map_err(|e| LlmError::from_reqwest(&e))?;

                        let status = response.status();
                        if !status.is_success() {
                            let retry_after = extract_retry_after(response.headers());
                            let error_text =
                                bounded_error_text(response, ERROR_BODY_MAX_BYTES).await;
                            let error_text = sanitize_http_error_body(
                                Some("Vision provider"),
                                status.as_u16(),
                                &error_text,
                            );
                            return Err(LlmError::from_http_response_with_retry_after(
                                status.as_u16(),
                                &error_text,
                                retry_after,
                            ));
                        }
                        Ok(response)
                    }
                },
                None,
            ),
        )
        .await
        .map_err(|_| ToolError::execution_failed("Vision API request timed out"))?
        .map_err(|e| ToolError::execution_failed(format!("Vision API request failed: {e}")))?;

        if self.config.stream == Some(true) {
            self.execute_streaming(response, deadline).await
        } else {
            self.execute_non_streaming(response, deadline).await
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    #[cfg(unix)]
    fn create_file_symlink(
        target: &std::path::Path,
        link: &std::path::Path,
    ) -> std::io::Result<()> {
        std::os::unix::fs::symlink(target, link)
    }

    #[cfg(windows)]
    fn create_file_symlink(
        target: &std::path::Path,
        link: &std::path::Path,
    ) -> std::io::Result<()> {
        std::os::windows::fs::symlink_file(target, link)
    }

    fn fake_config() -> VisionModelConfig {
        VisionModelConfig {
            model: "test-vision-model".to_string(),
            api_key: Some("test-key".to_string()),
            base_url: Some("https://example.invalid/v1".to_string()),
            request_timeout_secs: None,
            stream: None,
            retry_on_transient_errors: None,
        }
    }

    async fn serve_slow_error_body() -> String {
        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind slow test server");
        let address = listener.local_addr().expect("slow test server address");

        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.expect("accept test request");
            let mut buffer = [0_u8; 4096];
            let _ = socket.read(&mut buffer).await.expect("read test request");
            socket
                .write_all(
                    b"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 100\r\nConnection: close\r\n\r\n",
                )
                .await
                .expect("write slow response headers");
            tokio::time::sleep(Duration::from_secs(2)).await;
        });

        format!("http://{address}/v1")
    }

    async fn execute_against(
        mut config: VisionModelConfig,
        status: u16,
        content_type: &str,
        body: &str,
    ) -> (Result<ToolResult, ToolError>, Value) {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/chat/completions"))
            .respond_with(ResponseTemplate::new(status).set_body_raw(body, content_type))
            .mount(&server)
            .await;
        config.base_url = Some(format!("{}/v1", server.uri()));
        let workspace = tempdir().expect("workspace tempdir");
        std::fs::write(workspace.path().join("image.png"), b"test image")
            .expect("write test image");
        let context = ToolContext::new(workspace.path().to_path_buf());
        let result = ImageAnalyzeTool::new(config)
            .execute(json!({"image_path": "image.png"}), &context)
            .await;
        let requests = server.received_requests().await.expect("captured request");
        let request = serde_json::from_slice(&requests[0].body).expect("JSON request body");
        (result, request)
    }

    fn result_json(result: ToolResult) -> Value {
        serde_json::from_str(&result.content).expect("tool result JSON")
    }

    #[test]
    fn tool_metadata_is_read_only_and_named_image_analyze() {
        let tool = ImageAnalyzeTool::new(fake_config());
        assert_eq!(tool.name(), "image_analyze");
        assert!(tool.capabilities().contains(&ToolCapability::ReadOnly));
    }

    #[test]
    fn mime_type_detection_covers_common_formats() {
        for (ext, expected) in [
            ("png", "image/png"),
            ("PNG", "image/png"),
            ("jpg", "image/jpeg"),
            ("jpeg", "image/jpeg"),
            ("gif", "image/gif"),
            ("webp", "image/webp"),
            ("bmp", "image/bmp"),
        ] {
            let path = std::path::PathBuf::from(format!("test.{ext}"));
            let mime = ImageAnalyzeTool::detect_mime_type(&path)
                .unwrap_or_else(|_| panic!("must detect {ext}"));
            assert_eq!(mime, expected);
        }
    }

    #[test]
    fn mime_type_detection_rejects_unsupported_extension() {
        let path = std::path::PathBuf::from("test.svg");
        let err = ImageAnalyzeTool::detect_mime_type(&path)
            .expect_err("svg is intentionally out of scope for vision tool");
        assert!(err.to_string().contains("Unsupported image format"));
    }

    #[test]
    fn generic_vision_payload_uses_max_tokens() {
        let tool = ImageAnalyzeTool::new(fake_config());

        let payload = tool.request_payload("describe", "abc123", "image/png");

        assert_eq!(
            payload.get("max_tokens").and_then(Value::as_u64),
            Some(u64::from(DEFAULT_VISION_MAX_OUTPUT_TOKENS))
        );
        assert!(payload.get("max_completion_tokens").is_none());
        assert!(
            payload.get("stream").is_none(),
            "the default request shape must remain non-streaming"
        );
    }

    #[test]
    fn vision_payload_only_emits_stream_when_explicitly_configured() {
        for (configured, expected) in [(Some(true), Some(true)), (Some(false), Some(false))] {
            let mut config = fake_config();
            config.stream = configured;
            let payload =
                ImageAnalyzeTool::new(config).request_payload("describe", "abc123", "image/png");

            assert_eq!(payload.get("stream").and_then(Value::as_bool), expected);
        }
    }

    #[test]
    fn request_controls_preserve_defaults_and_apply_safe_bounds() {
        let default_tool = ImageAnalyzeTool::new(fake_config());
        assert_eq!(
            default_tool.request_timeout(),
            Duration::from_secs(DEFAULT_VISION_REQUEST_TIMEOUT_SECS)
        );
        assert!(default_tool.retry_config().enabled);

        let mut config = fake_config();
        config.request_timeout_secs = Some(u64::MAX);
        config.retry_on_transient_errors = Some(false);
        let configured_tool = ImageAnalyzeTool::new(config);
        assert_eq!(
            configured_tool.request_timeout(),
            Duration::from_secs(MAX_VISION_REQUEST_TIMEOUT_SECS)
        );
        assert!(!configured_tool.retry_config().enabled);
    }

    #[test]
    fn sse_buffer_preserves_utf8_split_across_chunks() {
        let event = "data: {\"choices\":[{\"delta\":{\"content\":\"图\"}}]}\n";
        let bytes = event.as_bytes();
        let split = event.find('图').expect("multibyte character") + 1;
        let mut buffer = SseLineBuffer::default();
        let mut lines = buffer.push(&bytes[..split]).expect("first chunk");
        lines.extend(buffer.push(&bytes[split..]).expect("second chunk"));

        assert_eq!(lines, vec![event.trim_end()]);
        let mut state = VisionStreamState::default();
        assert!(!process_sse_line(&lines[0], &mut state));
        assert_eq!(state.content, "图");
    }

    #[test]
    fn empty_vision_content_is_always_an_error() {
        let tool = ImageAnalyzeTool::new(fake_config());
        for truncated in [false, true] {
            let error = tool
                .vision_result(String::new(), "test-model".to_string(), truncated)
                .expect_err("empty content cannot be a successful analysis");
            assert!(error.to_string().contains("no usable content"));
        }
    }

    #[test]
    fn done_only_stream_is_not_misclassified_as_json() {
        let mut state = VisionStreamState::default();

        assert!(process_sse_line("data: [DONE]", &mut state));
        assert!(state.saw_data_event);
        assert!(state.completed);
    }

    #[tokio::test]
    async fn execute_streaming_accumulates_deltas() {
        let mut config = fake_config();
        config.stream = Some(true);
        let body = concat!(
            "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n"
        );

        let (result, request) = execute_against(config, 200, "text/event-stream", body).await;
        let result = result_json(result.expect("streaming response succeeds"));

        assert_eq!(request.get("stream").and_then(Value::as_bool), Some(true));
        assert_eq!(
            result.get("analysis").and_then(Value::as_str),
            Some("hello world")
        );
        assert!(result.get("truncated").is_none());
    }

    #[tokio::test]
    async fn execute_streaming_marks_clean_eof_without_terminal_event_as_truncated() {
        let mut config = fake_config();
        config.stream = Some(true);
        let body = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n";

        let (result, _) = execute_against(config, 200, "text/event-stream", body).await;
        let result = result_json(result.expect("partial content remains usable"));

        assert_eq!(
            result.get("analysis").and_then(Value::as_str),
            Some("partial")
        );
        assert_eq!(result.get("truncated").and_then(Value::as_bool), Some(true));
    }

    #[tokio::test]
    async fn execute_streaming_falls_back_to_ordinary_json() {
        let mut config = fake_config();
        config.stream = Some(true);
        let body = r#"{"model":"server-model","choices":[{"message":{"content":"fallback"},"finish_reason":"stop"}]}"#;

        let (result, _) = execute_against(config, 200, "application/json", body).await;
        let result = result_json(result.expect("ordinary JSON fallback succeeds"));

        assert_eq!(
            result.get("analysis").and_then(Value::as_str),
            Some("fallback")
        );
        assert_eq!(
            result.get("model").and_then(Value::as_str),
            Some("server-model")
        );
    }

    #[tokio::test]
    async fn execute_non_streaming_rejects_empty_content() {
        let body = r#"{"choices":[{"message":{"content":""},"finish_reason":"length"}]}"#;

        let (result, request) = execute_against(fake_config(), 200, "application/json", body).await;
        let error = result.expect_err("empty response must fail");

        assert!(request.get("stream").is_none());
        assert!(error.to_string().contains("no usable content"));
    }

    #[tokio::test]
    async fn request_deadline_covers_error_response_body() {
        let workspace = tempdir().expect("workspace tempdir");
        std::fs::write(workspace.path().join("image.png"), b"test image")
            .expect("write test image");
        let context = ToolContext::new(workspace.path().to_path_buf());
        let mut config = fake_config();
        config.base_url = Some(serve_slow_error_body().await);
        config.request_timeout_secs = Some(1);
        config.retry_on_transient_errors = Some(false);

        let result = tokio::time::timeout(
            Duration::from_secs(2),
            ImageAnalyzeTool::new(config).execute(json!({"image_path": "image.png"}), &context),
        )
        .await
        .expect("the tool must enforce its own deadline")
        .expect_err("an incomplete error response must fail");

        assert!(result.to_string().contains("request timed out"));
    }

    #[test]
    fn xiaomi_mimo_vision_payload_uses_max_completion_tokens() {
        let mut config = fake_config();
        config.model = "mimo-v2.5".to_string();
        config.base_url = Some("https://api.xiaomimimo.com/v1".to_string());
        let tool = ImageAnalyzeTool::new(config);

        let payload = tool.request_payload("describe", "abc123", "image/png");

        assert_eq!(
            payload.get("max_completion_tokens").and_then(Value::as_u64),
            Some(u64::from(DEFAULT_VISION_MAX_OUTPUT_TOKENS))
        );
        assert!(payload.get("max_tokens").is_none());
    }

    #[test]
    fn xiaomi_mimo_vision_payload_uses_max_completion_tokens_with_custom_proxy() {
        let mut config = fake_config();
        config.model = "mimo-v2.5".to_string();
        config.base_url = Some("https://vision-proxy.example.invalid/v1".to_string());
        let tool = ImageAnalyzeTool::new(config);

        let payload = tool.request_payload("describe", "abc123", "image/png");

        assert_eq!(
            payload.get("max_completion_tokens").and_then(Value::as_u64),
            Some(u64::from(DEFAULT_VISION_MAX_OUTPUT_TOKENS))
        );
        assert!(payload.get("max_tokens").is_none());
    }

    #[tokio::test]
    async fn execute_rejects_absolute_path() {
        // Trust-boundary pin: image_path must stay inside the workspace
        // — an absolute path or a `..`-traversing path must reject
        // before any base64 / API call.
        let tmp = tempdir().expect("tempdir");
        let ctx = ToolContext::new(tmp.path().to_path_buf());
        let tool = ImageAnalyzeTool::new(fake_config());
        let outside_workspace = if cfg!(windows) {
            r"C:\Windows\System32\drivers\etc\hosts"
        } else {
            "/etc/hosts"
        };
        let err = tool
            .execute(json!({"image_path": outside_workspace}), &ctx)
            .await
            .expect_err("absolute path must reject");
        assert!(
            err.to_string()
                .contains("relative path within the workspace"),
            "error must call out the workspace boundary; got {err}"
        );
    }

    #[tokio::test]
    async fn execute_rejects_parent_dir_traversal() {
        let tmp = tempdir().expect("tempdir");
        let ctx = ToolContext::new(tmp.path().to_path_buf());
        let tool = ImageAnalyzeTool::new(fake_config());
        let err = tool
            .execute(json!({"image_path": "../escape.png"}), &ctx)
            .await
            .expect_err("`..`-traversal must reject");
        assert!(
            err.to_string()
                .contains("relative path within the workspace"),
            "error must call out the workspace boundary; got {err}"
        );
    }

    #[tokio::test]
    async fn execute_rejects_symlink_that_resolves_outside_workspace() {
        let workspace = tempdir().expect("workspace tempdir");
        let outside = tempdir().expect("outside tempdir");
        let outside_image = outside.path().join("outside.png");
        std::fs::write(&outside_image, b"not a real png").expect("write outside image");
        let link = workspace.path().join("linked.png");
        if let Err(err) = create_file_symlink(&outside_image, &link) {
            eprintln!("skipping symlink assertion: {err}");
            return;
        }

        let ctx = ToolContext::new(workspace.path().to_path_buf());
        let tool = ImageAnalyzeTool::new(fake_config());
        let err = tool
            .execute(json!({"image_path": "linked.png"}), &ctx)
            .await
            .expect_err("symlink target outside workspace must reject before reading");
        assert!(
            err.to_string().contains("resolve within the workspace"),
            "error must call out the canonical workspace boundary; got {err}"
        );
    }
}
