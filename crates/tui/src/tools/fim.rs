//! FIM (Fill-in-the-Middle) edit tool.
//!
//! Reads a file, finds `prefix_anchor` and `suffix_anchor`, calls the active
//! route's `/beta/completions` FIM endpoint, and writes the generated middle
//! content back into the file. The URL is built from the session's own base URL
//! (`crates/tui/src/client.rs:3484`), so this works on any ChatCompletions
//! provider — it is not DeepSeek-specific.

use std::fs;

use async_trait::async_trait;
use serde_json::{Value, json};
use thiserror::Error;

use crate::client::DeepSeekClient;

use super::spec::{
    ApprovalRequirement, ToolCapability, ToolContext, ToolError, ToolResult, ToolSpec,
    optional_u64, required_str,
};

/// Result of a FIM edit operation
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct FimEditResult {
    pub success: bool,
    pub path: String,
    pub generated_text: String,
    pub prefix_end: usize,
    pub suffix_start: usize,
    pub message: String,
}

/// Tool for performing Fill-in-the-Middle edits via the active route's FIM API.
/// (`DeepSeekClient` is the historical name of the shared provider client; it is
/// not a DeepSeek-only type.)
pub struct FimEditTool {
    pub client: Option<DeepSeekClient>,
    pub model: String,
}

impl FimEditTool {
    #[must_use]
    pub fn new(client: Option<DeepSeekClient>, model: String) -> Self {
        Self { client, model }
    }
}

// === Errors ===

#[derive(Debug, Error)]
enum FimError {
    #[error("Prefix anchor not found in file: '{0}'")]
    PrefixNotFound(String),
    #[error("Suffix anchor not found after prefix anchor: '{0}'")]
    SuffixNotFound(String),
    #[error(
        "Prefix anchor matches {1} locations in the file: '{0}'. Use a longer anchor that appears exactly once."
    )]
    PrefixAmbiguous(String, usize),
    #[error(
        "Suffix anchor matches {1} locations after the prefix anchor: '{0}'. Use a longer anchor that appears exactly once."
    )]
    SuffixAmbiguous(String, usize),
    #[error("Prefix and suffix anchors overlap (suffix starts at {0}, prefix ends at {1})")]
    AnchorsOverlap(usize, usize),
    #[error("FIM API call failed: {0}")]
    ApiFailed(String),
}

#[async_trait]
impl ToolSpec for FimEditTool {
    fn name(&self) -> &'static str {
        "fim_edit"
    }

    fn description(&self) -> &'static str {
        "Edit a file using Fill-in-the-Middle (FIM) completion. Provide a file path, \
         prefix_anchor (text that appears before the section to replace), and \
         suffix_anchor (text that appears after the section to replace). The tool \
         calls the active route's fill-in-the-middle completion endpoint to \
         generate replacement content; this requires the active provider to \
         expose a fill-in-the-middle completions endpoint, and the call fails \
         otherwise. Each anchor must appear exactly once — the prefix anywhere \
         in the file, the suffix after the prefix — and ambiguous anchors are \
         refused with their match count (occurrences are counted without \
         overlap)."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "Path to the file to edit (relative to workspace)"
                },
                "prefix_anchor": {
                    "type": "string",
                    "description": "Text anchor marking the end of the prefix. Everything up to and including this anchor is kept as-is before the generated middle. Must appear exactly once in the file (counted without overlap)."
                },
                "suffix_anchor": {
                    "type": "string",
                    "description": "Text anchor marking the start of the suffix. Everything from this anchor onward is kept as-is after the generated middle. Must appear exactly once after the prefix anchor (counted without overlap)."
                },
                "max_tokens": {
                    "type": "integer",
                    "description": "Maximum tokens to generate (default: 1024)"
                }
            },
            "required": ["path", "prefix_anchor", "suffix_anchor"]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![
            ToolCapability::WritesFiles,
            ToolCapability::RequiresApproval,
        ]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Suggest
    }

    async fn execute(&self, input: Value, context: &ToolContext) -> Result<ToolResult, ToolError> {
        let path = required_str(&input, "path")?;
        let prefix_anchor = required_str(&input, "prefix_anchor")?;
        let suffix_anchor = required_str(&input, "suffix_anchor")?;
        let max_tokens = optional_u64(&input, "max_tokens", 1024)?;

        // 1. Read the file
        let resolved = context.resolve_path(path)?;
        let content = fs::read_to_string(&resolved).map_err(|e| {
            ToolError::execution_failed(format!("Failed to read {}: {}", resolved.display(), e))
        })?;

        // 2. Find prefix anchor
        let prefix_pos = content.find(prefix_anchor).ok_or_else(|| {
            ToolError::execution_failed(
                FimError::PrefixNotFound(prefix_anchor.to_string()).to_string(),
            )
        })?;
        // Without this check an anchor that repeats would be matched only at
        // its first occurrence and silently generate the middle at the wrong
        // site, so ambiguity is refused instead of guessed.
        let prefix_matches = content.matches(prefix_anchor).count();
        if prefix_matches > 1 {
            return Err(ToolError::execution_failed(
                FimError::PrefixAmbiguous(prefix_anchor.to_string(), prefix_matches).to_string(),
            ));
        }
        let prefix_end = prefix_pos + prefix_anchor.len();

        // 3. Find suffix anchor (after prefix anchor)
        let suffix_region = &content[prefix_end..];
        let suffix_pos = suffix_region.find(suffix_anchor).ok_or_else(|| {
            ToolError::execution_failed(
                FimError::SuffixNotFound(suffix_anchor.to_string()).to_string(),
            )
        })?;
        let suffix_matches = suffix_region.matches(suffix_anchor).count();
        if suffix_matches > 1 {
            return Err(ToolError::execution_failed(
                FimError::SuffixAmbiguous(suffix_anchor.to_string(), suffix_matches).to_string(),
            ));
        }
        let suffix_start = prefix_end + suffix_pos;

        // 4. Validate anchors don't overlap
        if suffix_start < prefix_end {
            return Err(ToolError::execution_failed(
                FimError::AnchorsOverlap(suffix_start, prefix_end).to_string(),
            ));
        }

        // 5. Extract prefix and suffix for the FIM API
        let fim_prompt = content[..prefix_end].to_string();
        let fim_suffix = content[suffix_start..].to_string();

        // 6. Call FIM API
        let generated_text = match self.client.as_ref() {
            Some(client) => client
                .fim_completion(&self.model, &fim_prompt, &fim_suffix, max_tokens as u32)
                .await
                .map_err(|e| {
                    ToolError::execution_failed(FimError::ApiFailed(e.to_string()).to_string())
                })?,
            None => {
                return Err(ToolError::execution_failed(
                    "FIM API client not available".to_string(),
                ));
            }
        };

        // 7. Build the new content and write it back
        let generated_len = generated_text.len();
        let new_content = format!("{fim_prompt}{generated_text}{fim_suffix}");
        crate::utils::write_atomic_workspace(&resolved, new_content.as_bytes()).map_err(|e| {
            ToolError::execution_failed(format!("Failed to write {}: {}", resolved.display(), e))
        })?;

        let result = FimEditResult {
            success: true,
            path: path.to_string(),
            generated_text,
            prefix_end,
            suffix_start,
            message: format!(
                "FIM edit applied to `{path}`. Generated {generated_len} bytes between prefix_anchor end (byte {prefix_end}) and suffix_anchor start (byte {suffix_start}).",
            ),
        };

        ToolResult::json(&result).map_err(|e| ToolError::execution_failed(e.to_string()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fim_edit_is_write_capable_but_not_read_only() {
        let tool = FimEditTool::new(None, "fim-model".to_string());
        let capabilities = tool.capabilities();

        assert!(capabilities.contains(&ToolCapability::WritesFiles));
        assert!(!capabilities.contains(&ToolCapability::ReadOnly));
        assert!(!tool.is_read_only());
    }

    fn workspace_with(content: &str) -> (tempfile::TempDir, ToolContext) {
        let tmp = tempfile::tempdir().expect("tempdir");
        let context = ToolContext::new(tmp.path().to_path_buf());
        std::fs::write(tmp.path().join("sample.txt"), content).expect("write fixture");
        (tmp, context)
    }

    /// Run `execute` and return the error text; every caller asserts on the
    /// specific error it expects.
    async fn execution_error(input: serde_json::Value, context: &ToolContext) -> String {
        FimEditTool::new(None, "fim-model".to_string())
            .execute(input, context)
            .await
            .expect_err("expected the fim_edit call to fail")
            .to_string()
    }

    #[tokio::test]
    async fn ambiguous_prefix_anchor_is_refused_not_first_occurrence_edited() {
        let (_tmp, context) = workspace_with("fn a() {}\nfn main() {}\nfn a() {}\nlet _ = a;\n");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "fn a() {}",
                "suffix_anchor": "let _ = a;",
            }),
            &context,
        )
        .await;
        assert!(message.contains("matches 2 locations"), "{message}");
        assert!(message.contains("exactly once"), "{message}");
    }

    #[tokio::test]
    async fn ambiguous_suffix_anchor_is_refused() {
        let (_tmp, context) = workspace_with("head\nreturn 1\nmid\nreturn 1\ntail\n");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "head",
                "suffix_anchor": "return 1",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("Suffix anchor matches 2 locations"),
            "{message}"
        );
    }

    #[tokio::test]
    async fn unique_anchors_pass_validation_and_reach_the_fim_call() {
        let (_tmp, context) = workspace_with("head\nmid\ntail\n");
        // With no client configured the call fails at the API boundary; that
        // error proves the anchor validation accepted the unique anchors.
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "head",
                "suffix_anchor": "tail",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("FIM API client not available"),
            "unique anchors must reach the API boundary: {message}"
        );
    }

    #[tokio::test]
    async fn empty_prefix_anchor_is_refused_with_char_boundary_count() {
        // An empty anchor matches at every char boundary including EOF, so
        // the count is characters + 1 — not bytes + 1. The multi-byte
        // fixture pins the char-boundary semantics against a byte-offset
        // refactor ("hé\n" = 3 chars / 4 bytes → 4 locations, not 5).
        let (_tmp, context) = workspace_with("hé\n");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "",
                "suffix_anchor": "é",
            }),
            &context,
        )
        .await;
        assert!(message.contains("matches 4 locations"), "{message}");
        assert!(message.contains("exactly once"), "{message}");
    }

    #[tokio::test]
    async fn suffix_occurrences_before_the_prefix_do_not_count() {
        // Only the region after the prefix participates in the uniqueness
        // check: an earlier occurrence is invisible, and one suffix after
        // the prefix passes validation.
        let (_tmp, context) = workspace_with("return 1\nhead\nmid\nreturn 1\ntail\n");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "head",
                "suffix_anchor": "return 1",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("FIM API client not available"),
            "one suffix after the prefix must pass validation: {message}"
        );
    }

    #[tokio::test]
    async fn suffix_only_before_the_prefix_is_not_found() {
        let (_tmp, context) = workspace_with("return 1\nhead\nmid\ntail\n");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "head",
                "suffix_anchor": "return 1",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("Suffix anchor not found after prefix anchor"),
            "{message}"
        );
    }

    #[test]
    fn fim_copy_pins_the_non_overlapping_counting_contract() {
        // std matches() counts non-overlapping occurrences, so a
        // self-similar anchor ("aa" in "aaa") passes with count 1. The
        // copy must disclose that counting rule or "exactly once" reads
        // as overlapping occurrences to the model.
        let tool = FimEditTool::new(None, "fim-model".to_string());
        let description = tool.description();
        assert!(description.contains("exactly once"), "{description}");
        assert!(
            description.contains("counted without overlap"),
            "{description}"
        );
        let schema = tool.input_schema();
        let prefix = schema["properties"]["prefix_anchor"]["description"]
            .as_str()
            .expect("prefix_anchor description");
        let suffix = schema["properties"]["suffix_anchor"]["description"]
            .as_str()
            .expect("suffix_anchor description");
        assert!(prefix.contains("counted without overlap"), "{prefix}");
        assert!(suffix.contains("counted without overlap"), "{suffix}");
    }

    #[tokio::test]
    async fn empty_prefix_anchor_on_an_empty_file_passes_to_the_fim_call() {
        // A non-empty file refuses an empty prefix because it matches at
        // every char boundary. On an empty file the single boundary
        // yields count 1, so validation passes and the call reaches the
        // API boundary — the base behavior, not a refusal.
        let (_tmp, context) = workspace_with("");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "",
                "suffix_anchor": "",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("FIM API client not available"),
            "empty prefix on an empty file must pass validation: {message}"
        );
    }

    #[tokio::test]
    async fn empty_suffix_after_an_eof_ending_prefix_passes_to_the_fim_call() {
        // A prefix ending at EOF leaves an empty suffix region, where an
        // empty suffix matches exactly once, so validation passes and the
        // call reaches the API boundary — the base behavior, not a refusal.
        let (_tmp, context) = workspace_with("tail anchor");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "tail anchor",
                "suffix_anchor": "",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("FIM API client not available"),
            "empty suffix after an EOF-ending prefix must pass validation: {message}"
        );
    }

    #[tokio::test]
    async fn self_overlapping_prefix_is_counted_without_overlap() {
        // "aa" overlaps itself inside "aaa": an overlapping counter would
        // report 2 locations and refuse, but occurrences are counted
        // without overlap — the rule the schema copy discloses — so the
        // single site passes and the call reaches the API boundary.
        let (_tmp, context) = workspace_with("aaa\nbbb\n");
        let message = execution_error(
            serde_json::json!({
                "path": "sample.txt",
                "prefix_anchor": "aa",
                "suffix_anchor": "b\n",
            }),
            &context,
        )
        .await;
        assert!(
            message.contains("FIM API client not available"),
            "a self-overlapping prefix with one non-overlapping site must pass validation: {message}"
        );
    }
}
