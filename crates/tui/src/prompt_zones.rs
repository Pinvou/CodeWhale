//! Three-zone prompt contract types for prefix-cache stability (#2264).
//!
//! Divides every request into three rigid zones:
//!
//! ```text
//! ┌─────────────────────────────────────────┐
//! │ PinnedPrefix (frozen after construction) │ ← system prompt + tool catalog
//! │   combined_sha256 computed at freeze()   │   cache hit candidate
//! ├─────────────────────────────────────────┤
//! │ AppendLog (append-only)                  │ ← conversation history
//! │   push() only, no insert / remove / edit │   preserves prefix of prior turns
//! └─────────────────────────────────────────┘
//! ```
//!
//! ## Status
//!
//! `PinnedPrefix` / `FrozenPrefix` / `PrefixDrift` / `AppendLog` are ready
//! for use; `AppendLog` itself is wired through `Session`'s message log.

use crate::models::{Message, SystemPrompt, Tool};
// ── helpers ────────────────────────────────────────────────────────────

fn sha256_hex(bytes: &[u8]) -> String {
    crate::hashing::sha256_hex(bytes)
}

fn system_text(system: Option<&SystemPrompt>) -> String {
    match system {
        Some(SystemPrompt::Text(text)) => text.clone(),
        Some(SystemPrompt::Blocks(blocks)) => {
            let mut text = String::new();
            for block in blocks {
                text.push_str(&block.text);
                text.push('\n');
            }
            text
        }
        None => String::new(),
    }
}

/// Serialize tools to a deterministic, sorted JSON string for hashing.
fn tool_catalog_digest(tools: &[Tool]) -> String {
    let mut serialized: Vec<String> = tools
        .iter()
        .filter_map(|t| serde_json::to_string(t).ok())
        .collect();
    serialized.sort();
    serialized.join("\n")
}

fn combined_hash(system_text: &str, tools: &[Tool]) -> String {
    let system_sha = sha256_hex(system_text.as_bytes());
    let tools_digest = tool_catalog_digest(tools);
    let tools_sha = sha256_hex(tools_digest.as_bytes());
    let combined = format!("{system_sha}:{tools_sha}");
    sha256_hex(combined.as_bytes())
}

// ── FrozenPrefix ───────────────────────────────────────────────────────

/// An immutable frozen prefix — system prompt text + tool catalog,
/// hashed at freeze time. The hash is stable as long as the system prompt
/// text and full tool definitions (name, description, schema) are unchanged.
///
/// Use [`PinnedPrefix::freeze`] to produce one.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FrozenPrefix {
    pub(crate) system_text: String,
    pub(crate) tool_catalog: String,
    pub(crate) combined_sha256: String,
}

impl FrozenPrefix {
    /// Verify that `current_system_text` and `current_tools` match the frozen
    /// prefix. Returns `Ok(())` when stable, `Err(PrefixDrift)` on mismatch.
    ///
    /// Fast path: compares raw text before falling back to SHA-256.
    pub fn verify(
        &self,
        current_system_text: &str,
        current_tools: &[Tool],
    ) -> Result<(), PrefixDrift> {
        let system_changed = current_system_text != self.system_text;
        let current_tool_catalog = tool_catalog_digest(current_tools);
        let tools_changed = current_tool_catalog != self.tool_catalog;

        if !system_changed && !tools_changed {
            return Ok(());
        }

        let current_hash = combined_hash(current_system_text, current_tools);
        Err(PrefixDrift {
            system_changed,
            tools_changed,
            frozen_hash: self.combined_sha256.clone(),
            current_hash,
        })
    }

    /// Returns a short (12-char) human-readable id for display.
    #[must_use]
    pub fn short_id(&self) -> &str {
        if self.combined_sha256.len() >= 12 {
            &self.combined_sha256[..12]
        } else {
            &self.combined_sha256
        }
    }

    /// Returns the full combined SHA-256.
    #[must_use]
    pub fn hash(&self) -> &str {
        &self.combined_sha256
    }
}

// ── PinnedPrefix ───────────────────────────────────────────────────────

/// A mutable prefix builder. Construct from the system prompt and tool
/// catalog, then call [`freeze`](Self::freeze) to produce a [`FrozenPrefix`].
#[derive(Debug, Clone)]
pub struct PinnedPrefix {
    system_text: String,
    tools: Vec<Tool>,
}

impl PinnedPrefix {
    #[must_use]
    pub fn new(system: Option<&SystemPrompt>, tools: Vec<Tool>) -> Self {
        Self {
            system_text: system_text(system),
            tools,
        }
    }

    /// Freeze this prefix into an immutable [`FrozenPrefix`].
    #[must_use]
    pub fn freeze(&self) -> FrozenPrefix {
        let tool_catalog = tool_catalog_digest(&self.tools);
        let combined_sha256 = combined_hash(&self.system_text, &self.tools);

        FrozenPrefix {
            system_text: self.system_text.clone(),
            tool_catalog,
            combined_sha256,
        }
    }
}

// ── PrefixDrift ────────────────────────────────────────────────────────

/// Describes how the current prefix differs from the frozen baseline.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PrefixDrift {
    pub system_changed: bool,
    pub tools_changed: bool,
    pub frozen_hash: String,
    pub current_hash: String,
}

impl std::fmt::Display for PrefixDrift {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let cause = match (self.system_changed, self.tools_changed) {
            (true, true) => "system prompt and tool set",
            (true, false) => "system prompt",
            (false, true) => "tool set",
            (false, false) => "unknown component",
        };
        write!(
            f,
            "prefix drift: {cause} changed (frozen={}, current={})",
            &self.frozen_hash[..12.min(self.frozen_hash.len())],
            &self.current_hash[..12.min(self.current_hash.len())]
        )
    }
}

// ── AppendLog ──────────────────────────────────────────────────────────

/// Append-only conversation history. Derefs to `&[Message]` via
/// [`Deref`](std::ops::Deref) for transparent read access; mutations go
/// through explicit methods (`push`, `truncate_to`, `trim_front`, `clear`)
/// whose names make cache impact obvious.
///
/// Phase 4: backing store for `Session.messages` (#2264).
#[derive(Debug, Clone)]
pub struct AppendLog {
    messages: Vec<Message>,
}

impl AppendLog {
    pub fn new() -> Self {
        Self {
            messages: Vec::new(),
        }
    }

    /// Append a message to the log. A single-message push is the cheapest
    /// mutation for prefix-cache stability — it extends the byte sequence
    /// without disturbing earlier turns.
    pub fn push(&mut self, message: Message) {
        self.messages.push(message);
    }

    /// Append multiple messages in one operation (fewer cache-line
    /// invalidations than repeated `push`).
    pub fn push_batch(&mut self, batch: Vec<Message>) {
        self.messages.extend(batch);
    }

    /// Truncate to keep only the first `new_len` messages.
    /// Discards newer messages (and their prefix-cache contribution)
    /// from the tail.
    pub fn truncate_to(&mut self, new_len: usize) {
        self.messages.truncate(new_len);
    }

    /// Remove `count` messages from the front (oldest first).
    /// Cache-destroying: drops the prefix that earlier turns share.
    pub fn trim_front(&mut self, count: usize) {
        if count >= self.messages.len() {
            self.messages.clear();
        } else {
            self.messages.drain(0..count);
        }
    }

    /// Remove all messages. Resets cache state completely.
    pub fn clear(&mut self) {
        self.messages.clear();
    }

    /// Return a mutable reference to the last message, if any.
    /// Prefer this over `last_mut()` on the inner vec — the name signals
    /// that only the most recent turn's content is being modified.
    #[must_use]
    pub fn last_mut(&mut self) -> Option<&mut Message> {
        self.messages.last_mut()
    }

    /// Consume and return the inner `Vec<Message>`.
    #[must_use]
    pub fn into_inner(self) -> Vec<Message> {
        self.messages
    }
}

impl Default for AppendLog {
    fn default() -> Self {
        Self::new()
    }
}

impl From<Vec<Message>> for AppendLog {
    fn from(messages: Vec<Message>) -> Self {
        Self { messages }
    }
}

impl From<AppendLog> for Vec<Message> {
    fn from(log: AppendLog) -> Self {
        log.messages
    }
}

impl std::ops::Deref for AppendLog {
    type Target = Vec<Message>;

    fn deref(&self) -> &Self::Target {
        &self.messages
    }
}

// ── tests ──────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::ContentBlock;
    use crate::models::Role;

    fn make_tool(name: &str) -> Tool {
        Tool {
            name: name.to_string(),
            description: String::new(),
            input_schema: serde_json::Value::Null,
            tool_type: None,
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }
    }

    fn make_message(role: &str, text: &str) -> Message {
        Message {
            role: Role::from(role),
            content: vec![ContentBlock::Text {
                text: text.to_string(),
                cache_control: None,
            }],
        }
    }

    // ── FrozenPrefix / PinnedPrefix ────────────────────────────────

    #[test]
    fn freeze_produces_stable_hash() {
        let tools = vec![make_tool("read"), make_tool("write")];
        let sys = SystemPrompt::Text("hello world".to_string());

        let a = PinnedPrefix::new(Some(&sys), tools.clone()).freeze();
        let b = PinnedPrefix::new(Some(&sys), tools).freeze();

        assert_eq!(a.combined_sha256, b.combined_sha256);
        assert_eq!(a.hash(), b.hash());
        assert_eq!(a.short_id(), b.short_id());
    }

    #[test]
    fn freeze_tool_order_is_stable() {
        let sys = SystemPrompt::Text("system".to_string());
        let tools_a = vec![make_tool("b"), make_tool("a")];
        let tools_b = vec![make_tool("a"), make_tool("b")];

        let a = PinnedPrefix::new(Some(&sys), tools_a).freeze();
        let b = PinnedPrefix::new(Some(&sys), tools_b).freeze();

        assert_eq!(a.combined_sha256, b.combined_sha256);
    }

    #[test]
    fn freeze_empty_tools() {
        let sys = SystemPrompt::Text("system".to_string());
        let frozen = PinnedPrefix::new(Some(&sys), vec![]).freeze();
        assert!(frozen.tool_catalog.is_empty());
        assert!(!frozen.combined_sha256.is_empty());
        assert_eq!(frozen.short_id().len(), 12);
    }

    #[test]
    fn freeze_no_system() {
        let tools = vec![make_tool("t1")];
        let frozen = PinnedPrefix::new(None, tools).freeze();
        assert!(frozen.system_text.is_empty());
        assert!(frozen.tool_catalog.contains("t1"));
    }

    #[test]
    fn verify_passes_when_stable() {
        let sys = SystemPrompt::Text("system".to_string());
        let tools = vec![make_tool("a")];
        let frozen = PinnedPrefix::new(Some(&sys), tools.clone()).freeze();

        assert!(frozen.verify("system", &tools).is_ok());
    }

    #[test]
    fn verify_detects_system_change() {
        let sys = SystemPrompt::Text("old".to_string());
        let tools = vec![make_tool("a")];
        let frozen = PinnedPrefix::new(Some(&sys), tools.clone()).freeze();

        let drift = frozen.verify("new", &tools).unwrap_err();
        assert!(drift.system_changed);
        assert!(!drift.tools_changed);
    }

    #[test]
    fn verify_detects_tool_change() {
        let sys = SystemPrompt::Text("system".to_string());
        let tools_a = vec![make_tool("a")];
        let frozen = PinnedPrefix::new(Some(&sys), tools_a).freeze();

        let tools_b = vec![make_tool("b")];
        let drift = frozen.verify("system", &tools_b).unwrap_err();
        assert!(!drift.system_changed);
        assert!(drift.tools_changed);
    }

    #[test]
    fn verify_detects_both_changes() {
        let sys = SystemPrompt::Text("old".to_string());
        let tools = vec![make_tool("a")];
        let frozen = PinnedPrefix::new(Some(&sys), tools).freeze();

        let drift = frozen.verify("new", &[make_tool("b")]).unwrap_err();
        assert!(drift.system_changed);
        assert!(drift.tools_changed);
    }

    #[test]
    fn verify_detects_schema_change() {
        let sys = SystemPrompt::Text("system".to_string());
        let tool_a = make_tool("a");
        let mut tool_a_v2 = make_tool("a");
        tool_a_v2.description = "updated desc".to_string();

        let frozen = PinnedPrefix::new(Some(&sys), vec![tool_a]).freeze();
        let drift = frozen.verify("system", &[tool_a_v2]).unwrap_err();
        // Same name, different schema — should detect the change.
        assert!(drift.tools_changed);
    }

    #[test]
    fn prefix_drift_display_is_readable() {
        let drift = PrefixDrift {
            system_changed: true,
            tools_changed: false,
            frozen_hash: "a".repeat(64),
            current_hash: "b".repeat(64),
        };
        let display = drift.to_string();
        assert!(display.contains("system prompt"));
        assert!(display.contains("aaaaaaaaaaaa"));
        assert!(display.contains("bbbbbbbbbbbb"));
    }

    // ── AppendLog ─────────────────────────────────────────────────

    #[test]
    fn append_log_push_and_iter() {
        let mut log = AppendLog::new();
        assert!(log.is_empty());

        log.push(make_message("user", "hello"));
        log.push(make_message("assistant", "hi"));

        assert_eq!(log.len(), 2);
        assert!(!log.is_empty());

        let messages: Vec<_> = log.iter().collect();
        assert_eq!(messages.len(), 2);
    }
}
