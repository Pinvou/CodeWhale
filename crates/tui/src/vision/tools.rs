//! `image_analyze` tool — analyze images using a dedicated vision model.

use std::path::Path;
use std::time::Duration;

use async_trait::async_trait;
use base64::{Engine as _, engine::general_purpose::STANDARD as BASE64};
use serde_json::{Value, json};

use crate::config::VisionModelConfig;
use crate::llm_client::{LlmError, RetryConfig, sanitize_http_error_body, with_retry};
use crate::tools::spec::{
    ToolCapability, ToolContext, ToolError, ToolResult, ToolSpec, required_str,
};
use crate::vision::image_input::{read_image_sniffed, resolve_workspace_image_path};

const DEFAULT_VISION_MAX_OUTPUT_TOKENS: u32 = 4096;

pub struct ImageAnalyzeTool {
    config: VisionModelConfig,
    client: reqwest::Client,
}

impl ImageAnalyzeTool {
    #[must_use]
    pub fn new(config: VisionModelConfig) -> Self {
        let client = crate::tls::reqwest_client_builder()
            .timeout(Duration::from_secs(120))
            .build()
            .expect("Failed to build HTTP client");
        Self { config, client }
    }

    async fn read_image_file(path: &Path) -> Result<(String, String), ToolError> {
        // Size cap and real-signature detection are enforced inside the
        // shared helper — the file extension is never trusted.
        let (bytes, mime_type) = read_image_sniffed(path)
            .await
            .map_err(|e| ToolError::execution_failed(e.to_string()))?;
        let base64_data = BASE64.encode(&bytes);
        Ok((base64_data, mime_type.to_string()))
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
        // 不带 temperature:部分模型(kimi-for-coding、gpt-5 系推理模型等)只接受
        // 默认值,显式 temperature 会被 400 拒绝("only 1 is allowed");省略时
        // provider 应用各自默认值,对所有模型都合法。模型目录 supported_parameters
        // 也无一声明 temperature。
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
            ]
        });

        let token_limit_field = if Self::uses_max_completion_tokens(&self.config) {
            "max_completion_tokens"
        } else {
            "max_tokens"
        };
        payload[token_limit_field] = json!(DEFAULT_VISION_MAX_OUTPUT_TOKENS);

        payload
    }
}

#[async_trait]
impl ToolSpec for ImageAnalyzeTool {
    fn name(&self) -> &str {
        "image_analyze"
    }

    fn description(&self) -> &str {
        "Analyze an image in the session workspace using the configured vision model. \
         Supports PNG, JPEG, GIF, WebP, and BMP formats. \
         Use this only for workspace images that were NOT provided as native image blocks \
         of the current user message; images already attached to the current message are \
         visible to the model directly and must not be re-analyzed. \
         仅用于读取 workspace 中未作为当前消息原生图片块提供的图片；已随当前消息提供的图片不需要重复调用。"
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

        let resolved_path = resolve_workspace_image_path(&context.workspace, Path::new(image_path))
            .map_err(|e| ToolError::execution_failed(e.to_string()))?;
        let (image_data, mime_type) = Self::read_image_file(&resolved_path).await?;

        let payload = self.request_payload(prompt, &image_data, &mime_type);

        let url = format!("{}/chat/completions", self.base_url());
        let api_key = self.api_key();

        let retry_config = RetryConfig {
            max_retries: 3,
            initial_delay: 1.0,
            max_delay: 30.0,
            enabled: true,
            ..Default::default()
        };

        let response = with_retry(
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
                        let error_text = response
                            .text()
                            .await
                            .unwrap_or_else(|_| "Unknown error".to_string());
                        let error_text = sanitize_http_error_body(
                            Some("Vision provider"),
                            status.as_u16(),
                            &error_text,
                        );
                        return Err(LlmError::from_http_response(status.as_u16(), &error_text));
                    }
                    Ok(response)
                }
            },
            None,
        )
        .await
        .map_err(|e| ToolError::execution_failed(format!("Vision API request failed: {e}")))?;

        let json: Value = response
            .json()
            .await
            .map_err(|e| ToolError::execution_failed(format!("Failed to parse response: {e}")))?;

        let content = json
            .get("choices")
            .and_then(|c| c.get(0))
            .and_then(|c| c.get("message"))
            .and_then(|m| m.get("content"))
            .and_then(|c| c.as_str())
            .unwrap_or("")
            .to_string();

        let model = json
            .get("model")
            .and_then(|m| m.as_str())
            .unwrap_or(&self.config.model)
            .to_string();

        let result = json!({
            "analysis": content,
            "model": model,
        });

        ToolResult::json(&result)
            .map_err(|e| ToolError::execution_failed(format!("Failed to serialize result: {e}")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

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
        }
    }

    #[test]
    fn tool_metadata_is_read_only_and_named_image_analyze() {
        let tool = ImageAnalyzeTool::new(fake_config());
        assert_eq!(tool.name(), "image_analyze");
        assert!(tool.capabilities().contains(&ToolCapability::ReadOnly));
    }

    #[tokio::test]
    async fn read_image_file_sniffs_real_signature_regardless_of_extension() {
        // A PNG file named `.jpg` must be detected as image/png — the real
        // file header wins over the extension.
        let tmp = tempdir().expect("tempdir");
        let path = tmp.path().join("mislabeled.jpg");
        std::fs::write(&path, [0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00])
            .expect("write png bytes");

        let (_data, mime) = ImageAnalyzeTool::read_image_file(&path)
            .await
            .expect("png signature detected");
        assert_eq!(mime, "image/png");
    }

    #[tokio::test]
    async fn read_image_file_rejects_forged_extension() {
        let tmp = tempdir().expect("tempdir");
        let path = tmp.path().join("fake.png");
        std::fs::write(&path, b"this is not a real image").expect("write garbage");

        let err = ImageAnalyzeTool::read_image_file(&path)
            .await
            .expect_err("forged extension must reject");
        assert!(
            err.to_string().contains("unsupported image format"),
            "error must call out the unrecognized signature; got {err}"
        );
    }

    #[tokio::test]
    async fn read_image_file_enforces_size_cap_before_reading() {
        let tmp = tempdir().expect("tempdir");
        let path = tmp.path().join("huge.png");
        let file = std::fs::File::create(&path).expect("create");
        file.set_len(crate::vision::image_input::MAX_IMAGE_BYTES + 1)
            .expect("set len");

        let err = ImageAnalyzeTool::read_image_file(&path)
            .await
            .expect_err("oversized image must reject before reading");
        assert!(
            err.to_string().contains("大小上限"),
            "error must call out the size limit; got {err}"
        );
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

    /// pinvou3 fork 行为锚点:payload 绝不携带 temperature——kimi-for-coding 等模型
    /// 只接受默认值,显式 temperature 会被 400 拒绝。
    #[test]
    fn forkguard_vision_payload_omits_temperature() {
        for model in ["deepseek-v4-pro", "kimi-for-coding", "gpt-5.6-terra", "mimo-v2.5"] {
            let mut config = fake_config();
            config.model = model.to_string();
            let tool = ImageAnalyzeTool::new(config);

            let payload = tool.request_payload("describe", "abc123", "image/png");

            assert!(
                payload.get("temperature").is_none(),
                "temperature must be omitted for {model}; got {payload}"
            );
        }
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
