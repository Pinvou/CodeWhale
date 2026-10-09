//! Approval fingerprint keys (§5.A).
//!
//! Instead of caching by tool name alone (which would let an approved
//! `exec_shell "cat foo"` silently pass `exec_shell "rm -rf /"`), the
//! approval flow uses a **call fingerprint** — a digest of the tool name
//! and the semantically‑relevant portion of its arguments.
//!
//! ## Two fingerprint shapes
//!
//! There are two key flavours, used for opposite sides of the decision:
//!
//! * [`build_approval_key`] — an **exact** digest of the full arguments.
//!   Used to scope *denials* so that denying one call (e.g. `rm -rf /tmp/x`)
//!   does not also suppress a later, different call to the same tool (#1617).
//!
//!   | Tool           | Exact key                                |
//!   |---------------|------------------------------------------|
//!   | file writes    | `file:<tool_name>:<hash of args>`        |
//!   | shell tools    | `shell:<tool_name>:<hash of args>`       |
//!   | `fetch_url`    | `net:<hostname>`                         |
//!   | everything else| `tool:<tool_name>:<hash of input>`       |
//!
//! * [`build_approval_grouping_key`] — a **lossy / arity-aware** digest.
//!   Used to scope *approvals* so that approving `cargo build` for the
//!   session also covers `cargo build --release` (the v0.8.37 behaviour).
//!
//!   | Tool           | Grouping key                             |
//!   |---------------|------------------------------------------|
//!   | `apply_patch`  | `patch:<hash of file paths>`             |
//!   | shell tools    | `shell:<command prefix>` (+ `@cwd:<operand>` when the call carries one) |
//!   | `fetch_url`    | `net:<hostname>`                         |
//!   | everything else| `tool:<tool_name>:<hash of input>`       |
//!
use std::fmt::Write as _;

use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::command_safety::classify_command;

/// The fingerprint of a tool call — stable enough to match repeated
/// calls but specific enough to avoid privilege confusion.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct ApprovalKey(pub String);

/// Build the approval‑cache key for a tool call.
///
/// The key incorporates the tool name and a canonical digest of the
/// arguments so that denying one call suppresses exact retries, not later
/// invocations of the same tool with different parameters.
#[must_use]
pub fn build_approval_key(tool_name: &str, input: &serde_json::Value) -> ApprovalKey {
    let tool_name = crate::tools::canonical_action::canonical_action_alias(tool_name, input);
    let fingerprint = match tool_name {
        "apply_patch" | "write_file" | "edit_file" | "fim_edit" => {
            format!("file:{tool_name}:{}", hash_json_value(input))
        }
        "exec_shell"
        | "task_shell_start"
        | "exec_shell_wait"
        | "exec_shell_interact"
        | "exec_wait"
        | "exec_interact" => {
            format!("shell:{tool_name}:{}", hash_json_value(input))
        }
        "fetch_url" | "web.fetch" | "web_fetch" => {
            let host = parse_host(input);
            format!("net:{host}")
        }
        _ => format!("tool:{tool_name}:{}", hash_json_value(input)),
    };
    ApprovalKey(fingerprint)
}

/// Build the **grouping** approval key for a tool call.
///
/// Unlike [`build_approval_key`], this collapses argument variants of the
/// same command family onto one key (the v0.8.37 behaviour) so that an
/// "approve for session" decision covers later invocations that differ only
/// by flags. Denials must keep using the exact [`build_approval_key`].
#[must_use]
pub fn build_approval_grouping_key(tool_name: &str, input: &serde_json::Value) -> ApprovalKey {
    let tool_name = crate::tools::canonical_action::canonical_action_alias(tool_name, input);
    let fingerprint = match tool_name {
        "apply_patch" => {
            // B27-3 + A28-3: key on the REAL parser's plan —
            // `preflight_apply_patch`, the same alias-folded judgment every
            // plan-time gate uses. The round-26 ad-hoc scan (a) keyed the
            // top-level override for replace-forms where execution never
            // reads it (an approved patch grant then auto-approved an
            // arbitrary replace-write under the same decoy override), and
            // (b) collected only `+++ b/` headers, keying `+++ a/`-spelled,
            // bare, timestamped, and /dev/null-delete sections into one
            // shared `no_files` family that covers arbitrary targets.
            let paths_hash = hash_patch_paths(input);
            format!("patch:{paths_hash}")
        }
        "exec_shell"
        | "task_shell_start"
        | "exec_shell_wait"
        | "exec_shell_interact"
        | "exec_wait"
        | "exec_interact" => {
            let prefix = command_prefix(input);
            // The approval side judges the resolved effective cwd (the same
            // value exec rule matching sees), so the grant is keyed to the
            // command family AND the cwd operand: a grant approved at the
            // session workspace must not silently cover the same command
            // redirected into another root.
            match shell_cwd_operand(input) {
                // Length-prefix the operand (round-20 B20-2): the raw
                // `prefix@cwd:` concatenation is non-injective — model-craftable
                // spellings of prefix/cwd collide across the boundary.
                // Round-29/30 B2: length-prefix BOTH sides — the prefix
                // comes from classify_command's positional fallback and can
                // itself contain "@cwd:<n>:", which let {"git foo" @ cwd
                // "a@cwd:1:b"} collide with {"git@cwd:9:a & payload" @
                // cwd "b"} (the review's dynamic probe).
                Some(cwd) => format!("shell:{}:{prefix}@cwd:{}:{cwd}", prefix.len(), cwd.len()),
                None => format!("shell:{prefix}"),
            }
        }
        "fetch_url" | "web.fetch" | "web_fetch" => {
            let host = parse_host(input);
            format!("net:{host}")
        }
        // MCP tools are reviewed as kinds: a trusted plugin bundle's MCP
        // tools were human-reviewed at trust time, so the session grant the
        // approval card offers (`2` — "approves for the session") is the
        // reviewed kind, `mcp:<tool>`. Hashing the full params here would
        // make every exact-argument variant its own family and silently
        // narrow the granted kind into a one-call grant (the regression the
        // plugin e2e acceptance catches). Shell keeps its command-family
        // key (R2); this arm never widens shell or file tools.
        name if crate::mcp::McpPool::is_mcp_tool(name) => format!("mcp:{name}"),
        _ => format!("tool:{tool_name}:{}", hash_json_value(input)),
    };
    ApprovalKey(fingerprint)
}

/// Return the canonical command prefix for the shell command in `input`.
///
/// Uses [`classify_command`] from the arity dictionary so that approving
/// `git status` also covers `git status -s` / `git status --porcelain`
/// without also covering `git push`.
fn command_prefix(input: &serde_json::Value) -> String {
    let cmd = input.get("command").and_then(|v| v.as_str()).unwrap_or("");
    let tokens: Vec<&str> = cmd.split_whitespace().collect();
    if tokens.is_empty() {
        return "<empty>".to_string();
    }
    classify_command(&tokens)
}

/// Return the exec `cwd`/`working_dir` operand when the call carries one —
/// the value the approval side resolves and judges as the effective cwd.
fn shell_cwd_operand(input: &serde_json::Value) -> Option<&str> {
    // Round-20 B20-2: mirror the check-side and execution field semantics —
    // `get("cwd")` returning `Some(Value::Null)` used to block the
    // `working_dir` fallback, keying the grant to the no-operand family
    // while the check/exec treated the same call as redirected to
    // `working_dir`. Skip Null (and non-string) values like they do.
    ["cwd", "working_dir"]
        .iter()
        .find_map(|name| input.get(name).and_then(Value::as_str))
        .filter(|cwd| !cwd.is_empty())
}

/// Hash the write targets of a patch input, taken from the REAL parser's
/// plan (`preflight_apply_patch`): the top-level override keys ONLY in the
/// patch form (execution's PathOverride-wins semantics; replace-form top
/// levels are decoys), otherwise the touched file set keys as parsed —
/// every accepted `+++` spelling (`a/`/`b/` prefixes, bare, tab timestamps)
/// normalizes to the execution target, and deletions key under a separate
/// delete marker so a delete grant never covers writes (B27-3/A28-3).
fn hash_patch_paths(input: &serde_json::Value) -> String {
    use std::collections::hash_map::DefaultHasher;
    use std::hash::{Hash, Hasher};

    let mut folded = input.clone();
    if super::file::apply_param_aliases(&mut folded, super::file::PATH_ALIASES, "apply_patch")
        .is_err()
    {
        // Alias conflict: execution fails the call too; a distinct family
        // keeps the key from silently matching a folded spelling.
        return "alias_conflict".to_string();
    }
    match super::apply_patch::preflight_apply_patch(&folded) {
        Ok(plan) => {
            // Round-29/30 B2: domain-separate every hashed item — a bare
            // `str` marker and a touched-file name feed the IDENTICAL byte
            // stream, so hash("override", P) == hash(touched ["override",
            // P]) and one approved override patch could auto-approve a
            // differently-shaped replace patch (dynamically reproduced in
            // the round-29 review). Tag each item class and length-prefix
            // every string so no spelling of one class impersonates another.
            let mut hasher = DefaultHasher::new();
            fn feed(tag: u8, value: &str, hasher: &mut DefaultHasher) {
                tag.hash(hasher);
                value.len().hash(hasher);
                value.hash(hasher);
            }
            if let Some(override_path) = plan.path_override.as_deref() {
                feed(1, "override", &mut hasher);
                feed(2, override_path, &mut hasher);
            } else {
                for path in &plan.touched_files {
                    feed(3, path, &mut hasher);
                }
                for path in &plan.deletes {
                    feed(4, "delete", &mut hasher);
                    feed(5, path, &mut hasher);
                }
            }
            format!("{:x}", hasher.finish())
        }
        Err(_) => "unparseable".to_string(),
    }
}

/// Parse the host portion from a URL input.
fn parse_host(input: &serde_json::Value) -> String {
    let url = input.get("url").and_then(|v| v.as_str()).unwrap_or("");

    if let Ok(parsed) = reqwest::Url::parse(url) {
        parsed.host_str().unwrap_or(url).to_string()
    } else {
        url.to_string()
    }
}

fn hash_json_value(value: &Value) -> String {
    let mut canonical = String::new();
    push_canonical_json(value, &mut canonical);

    let digest = Sha256::digest(canonical.as_bytes());
    let mut short = String::with_capacity(16);
    for byte in &digest[..8] {
        write!(&mut short, "{byte:02x}").expect("writing to String cannot fail");
    }
    short
}

fn push_canonical_json(value: &Value, out: &mut String) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(value) => {
            out.push_str("bool:");
            out.push_str(if *value { "true" } else { "false" });
        }
        Value::Number(value) => {
            out.push_str("number:");
            // Avoid allocating via value.to_string().
            if let Some(n) = value.as_f64() {
                let _ = write!(out, "{n}");
            } else if let Some(n) = value.as_i64() {
                let _ = write!(out, "{n}");
            } else if let Some(n) = value.as_u64() {
                let _ = write!(out, "{n}");
            } else {
                out.push_str(&value.to_string());
            }
        }
        Value::String(value) => {
            out.push_str("string:");
            // Emit JSON-encoded string without an intermediate allocation.
            out.push('"');
            for ch in value.chars() {
                match ch {
                    '"' => out.push_str("\\\""),
                    '\\' => out.push_str("\\\\"),
                    '\n' => out.push_str("\\n"),
                    '\r' => out.push_str("\\r"),
                    '\t' => out.push_str("\\t"),
                    c if c.is_control() => {
                        let _ = write!(out, "\\u{:04x}", c as u32);
                    }
                    c => out.push(c),
                }
            }
            out.push('"');
        }
        Value::Array(items) => {
            out.push('[');
            for (index, item) in items.iter().enumerate() {
                if index > 0 {
                    out.push(',');
                }
                push_canonical_json(item, out);
            }
            out.push(']');
        }
        Value::Object(map) => {
            let mut entries = map.iter().collect::<Vec<_>>();
            entries.sort_by_key(|(key, _)| *key);

            out.push('{');
            for (index, (key, value)) in entries.into_iter().enumerate() {
                if index > 0 {
                    out.push(',');
                }
                let encoded_key =
                    serde_json::to_string(key).expect("serializing an object key cannot fail");
                out.push_str(&encoded_key);
                out.push(':');
                push_canonical_json(value, out);
            }
            out.push('}');
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn different_commands_different_keys() {
        let key_a = build_approval_key("exec_shell", &json!({"command": "ls"}));
        let key_b = build_approval_key("exec_shell", &json!({"command": "rm -rf /tmp"}));
        assert_ne!(key_a, key_b);
    }

    #[test]
    fn same_command_same_key() {
        let key_a = build_approval_key("exec_shell", &json!({"command": "cargo build --release"}));
        let key_b = build_approval_key("exec_shell", &json!({"command": "cargo build --release"}));
        assert_eq!(key_a, key_b);
    }

    #[test]
    fn shell_keys_include_full_command_arguments() {
        let key_a = build_approval_key("exec_shell", &json!({"command": "cargo build"}));
        let key_b = build_approval_key("exec_shell", &json!({"command": "cargo build --release"}));
        assert_ne!(key_a, key_b);
    }

    #[test]
    fn grouping_key_collapses_shell_flag_variants() {
        let key_a = build_approval_grouping_key("exec_shell", &json!({"command": "cargo build"}));
        let key_b =
            build_approval_grouping_key("exec_shell", &json!({"command": "cargo build --release"}));
        assert_eq!(
            key_a, key_b,
            "approving a command family must cover later flag variants"
        );
    }

    #[test]
    fn grouping_key_grants_mcp_tools_as_reviewed_kinds() {
        // A session grant for a reviewed plugin MCP tool is the kind
        // (`mcp:<tool>`), not the exact arguments: the plugin e2e acceptance
        // approves the echo kind once and later variants of the same reviewed
        // tool must not re-prompt. R2's shell command-family scoping is
        // untouched — this is the MCP arm only.
        let key_a = build_approval_grouping_key(
            "mcp_plugin-4-demo-local_echo",
            &json!({"text": "acceptance", "hang": false}),
        );
        let key_b = build_approval_grouping_key(
            "mcp_plugin-4-demo-local_echo",
            &json!({"text": "acceptance", "hang": true}),
        );
        assert_eq!(
            key_a, key_b,
            "a reviewed MCP kind grant covers argument variants of that tool"
        );
        let key_c = build_approval_grouping_key("mcp_plugin-4-demo-local_kick", &json!({"x": 1}));
        assert_ne!(key_a, key_c, "a different MCP tool is a different kind");
        // The exact-call key stays per-arguments so denials still suppress
        // only exact retries.
        let exact_a = build_approval_key(
            "mcp_plugin-4-demo-local_echo",
            &json!({"text": "acceptance", "hang": false}),
        );
        let exact_b = build_approval_key(
            "mcp_plugin-4-demo-local_echo",
            &json!({"text": "acceptance", "hang": true}),
        );
        assert_ne!(exact_a, exact_b, "denial keys remain argument-exact");
    }

    #[test]
    fn grouping_key_still_separates_distinct_commands() {
        let key_a = build_approval_grouping_key("exec_shell", &json!({"command": "git status"}));
        let key_b = build_approval_grouping_key("exec_shell", &json!({"command": "git push"}));
        assert_ne!(key_a, key_b);
    }

    /// Round-29/30 B2 pin: the patch marker and touched-file name spaces
    /// are domain-separated — an approved override-form grant must never
    /// auto-approve a replace-form patch whose touched list is literally
    /// ["override", <the approved target>] (the review's dynamic collision).
    #[test]
    fn grouping_key_separates_override_form_from_listed_override_name() {
        let target = "/work/.env";
        let override_form = serde_json::json!({
            "path": target,
            "patch": "@@
-a
+b
"
        });
        let listed_form = serde_json::json!({
            "replace": [
                { "path": "override", "content": "x" },
                { "path": target, "content": "y" }
            ]
        });
        let a = build_approval_grouping_key("apply_patch", &override_form);
        let b = build_approval_grouping_key("apply_patch", &listed_form);
        assert_ne!(
            a, b,
            "the override marker and a touched file named 'override' must not collide"
        );
    }

    /// Round-29/30 B2 pin (shell side): a prefix containing the "@cwd:<n>:"
    /// spelling must not collide with a genuine cwd-carrying key.
    #[test]
    fn grouping_key_separates_embedded_cwd_spelling_from_real_operand() {
        let weird_cwd = serde_json::json!({
            "command": "git foo",
            "cwd": "a@cwd:1:b"
        });
        let payload_prefix = serde_json::json!({
            "command": "git@cwd:9:a & payload",
            "cwd": "b"
        });
        let a = build_approval_grouping_key("exec_shell", &weird_cwd);
        let b = build_approval_grouping_key("exec_shell", &payload_prefix);
        assert_ne!(
            a, b,
            "an embedded '@cwd:' spelling in either operand must not forge the other key"
        );
    }

    #[test]
    fn grouping_key_collapses_patch_body_for_same_path() {
        let key_a = build_approval_grouping_key(
            "apply_patch",
            &json!({"replace": [{"path": "a.rs", "content": "x"}]}),
        );
        let key_b = build_approval_grouping_key(
            "apply_patch",
            &json!({"replace": [{"path": "a.rs", "content": "y"}]}),
        );
        assert_eq!(
            key_a, key_b,
            "approving a patch family must cover later edits to the same path"
        );
    }

    #[test]
    fn grouping_key_rekeys_on_the_top_level_path_override() {
        // Round-26 M26-3: execution's PathOverride wins over the payload
        // headers (they become decoys), so the grouping key must cover the
        // override — the same patch body under two different targets used
        // to share one grant key, letting a session-approved family
        // auto-approve a redirect to an arbitrary file (the B20-2 shell
        // class, on the patch arm). Alias spellings fold first, exactly
        // like preflight/execute.
        let patch = "@@ -1,2 +1,2 @@\n old\n-value\n+new-value\n";
        let decoy = build_approval_grouping_key("apply_patch", &json!({"patch": patch}));
        for alias in ["path", "file_path", "filePath"] {
            let redirected = build_approval_grouping_key(
                "apply_patch",
                &json!({alias: ".git/hooks/pre-commit", "patch": patch}),
            );
            assert_ne!(
                redirected, decoy,
                "an {alias} override must re-key the grant family away from the header set"
            );
        }
        // The same override target under different alias spellings is ONE
        // family (the fold runs before hashing).
        let canonical = build_approval_grouping_key(
            "apply_patch",
            &json!({"path": ".git/hooks/pre-commit", "patch": patch}),
        );
        let alias_spellings = build_approval_grouping_key(
            "apply_patch",
            &json!({"filePath": ".git/hooks/pre-commit", "patch": patch}),
        );
        assert_eq!(
            canonical, alias_spellings,
            "alias spellings of the same override collapse to one family"
        );

        // B27-3: in the REPLACE form the top-level path is a decoy execution
        // never reads — the key must follow the entries, so an approved
        // patch-form override grant cannot cover a replace-write hiding
        // behind the same decoy.
        let replace_decoy = build_approval_grouping_key(
            "apply_patch",
            &json!({"path": ".git/hooks/pre-commit", "replace": [{"path": "notes.txt", "content": "x"}]}),
        );
        let replace_plain = build_approval_grouping_key(
            "apply_patch",
            &json!({"replace": [{"path": "notes.txt", "content": "x"}]}),
        );
        assert_eq!(
            replace_decoy, replace_plain,
            "a replace-form top-level path is a decoy and must not key the family"
        );
        assert_ne!(
            replace_plain,
            build_approval_grouping_key(
                "apply_patch",
                &json!({"replace": [{"path": "other.txt", "content": "x"}]}),
            ),
            "different replace targets are different families"
        );

        // A28-3: header spellings the old ad-hoc scan missed (`+++ a/`, bare
        // `+++ x`) key to the REAL target, and a single-file grant must not
        // cover a two-file patch whose second section rides such a spelling.
        // `+++ a/x`-spelled and `+++ b/x`-spelled writes of the same target
        // are ONE family (the parser normalizes both to x) — the old ad-hoc
        // `+++ b/`-only scan keyed the a-spelling into the shared `no_files`
        // family, whose grant then covered arbitrary targets (A28-3).
        let b_spelled = build_approval_grouping_key(
            "apply_patch",
            &json!({"patch": "--- a/.env\n+++ b/.env\n@@ -1,1 +1,1 @@\n-old\n+new\n"}),
        );
        let a_spelled = build_approval_grouping_key(
            "apply_patch",
            &json!({"patch": "--- b/.env\n+++ a/.env\n@@ -1,1 +1,1 @@\n-old\n+new\n"}),
        );
        assert_eq!(
            b_spelled, a_spelled,
            "header-prefix spellings of the same target collapse to one family"
        );
        assert_ne!(
            b_spelled,
            build_approval_grouping_key(
                "apply_patch",
                &json!({"patch": "--- a/other.txt\n+++ b/other.txt\n@@ -1,1 +1,1 @@\n-old\n+new\n"}),
            ),
            "different targets stay different families"
        );
        // Deletions key under a delete marker: a delete grant covers no write.
        let deletion = build_approval_grouping_key(
            "apply_patch",
            &json!({"patch": "--- a/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-gone\n"}),
        );
        let write_same_path = build_approval_grouping_key(
            "apply_patch",
            &json!({"patch": "--- a/old.txt\n+++ b/old.txt\n@@ -1,1 +1,1 @@\n-old\n+new\n"}),
        );
        assert_ne!(
            deletion, write_same_path,
            "a deletion keys apart from a write of the same path"
        );
    }

    #[test]
    fn grouping_key_treats_replace_and_legacy_changes_as_the_same_path_set() {
        let canonical = build_approval_grouping_key(
            "apply_patch",
            &json!({"replace": [{"path": "a.rs", "content": "new"}]}),
        );
        let legacy = build_approval_grouping_key(
            "apply_patch",
            &json!({"changes": [{"path": "a.rs", "content": "new"}]}),
        );

        assert_eq!(canonical, legacy);
    }

    #[test]
    fn denial_key_stays_exact_while_grouping_key_collapses() {
        let exact_a = build_approval_key("exec_shell", &json!({"command": "cargo build"}));
        let exact_b =
            build_approval_key("exec_shell", &json!({"command": "cargo build --release"}));
        assert_ne!(exact_a, exact_b, "denials must remain exact-call scoped");

        let group_a = build_approval_grouping_key("exec_shell", &json!({"command": "cargo build"}));
        let group_b =
            build_approval_grouping_key("exec_shell", &json!({"command": "cargo build --release"}));
        assert_eq!(group_a, group_b, "approvals must group by command family");
    }

    #[test]
    fn shell_grouping_key_rekeys_on_the_cwd_operand() {
        let at_workspace =
            build_approval_grouping_key("exec_shell", &json!({"command": "git status"}));
        let redirected = build_approval_grouping_key(
            "exec_shell",
            &json!({"command": "git status", "cwd": "/attached/repo"}),
        );
        assert_ne!(
            at_workspace, redirected,
            "a grant approved at the session workspace must not cover the same command redirected into another root"
        );

        let same_redirect = build_approval_grouping_key(
            "exec_shell",
            &json!({"command": "git status -s", "cwd": "/attached/repo"}),
        );
        assert_eq!(
            redirected, same_redirect,
            "the same command family in the same cwd stays one grant"
        );

        let via_working_dir = build_approval_grouping_key(
            "exec_shell",
            &json!({"command": "git status", "working_dir": "/attached/repo"}),
        );
        assert_eq!(
            redirected, via_working_dir,
            "cwd and working_dir are the same operand for the grant key"
        );

        // Round-20 B20-2: `cwd: null` must not block the `working_dir`
        // fallback — the null spelling used to key the NO-OPERAND family
        // while the check and execution treated the same call as redirected,
        // letting a session-approved plain command run in the attached root
        // unprompted.
        let null_cwd_with_working_dir = build_approval_grouping_key(
            "exec_shell",
            &json!({"command": "git status", "cwd": null, "working_dir": "/attached/repo"}),
        );
        assert_eq!(
            redirected, null_cwd_with_working_dir,
            "a null cwd plus working_dir keys as the redirected operand"
        );
        assert_ne!(
            at_workspace, null_cwd_with_working_dir,
            "the redirected spelling must not collide with the no-operand family"
        );
    }

    #[test]
    fn patch_keys_differ_by_path() {
        let key_a = build_approval_key(
            "apply_patch",
            &json!({"replace": [{"path": "a.rs", "content": "x"}]}),
        );
        let key_b = build_approval_key(
            "apply_patch",
            &json!({"replace": [{"path": "b.rs", "content": "x"}]}),
        );
        assert_ne!(key_a, key_b);
    }

    #[test]
    fn patch_keys_differ_by_body_for_same_path() {
        let key_a = build_approval_key(
            "apply_patch",
            &json!({"replace": [{"path": "a.rs", "content": "x"}]}),
        );
        let key_b = build_approval_key(
            "apply_patch",
            &json!({"replace": [{"path": "a.rs", "content": "y"}]}),
        );
        assert_ne!(key_a, key_b);
    }

    #[test]
    fn net_keys_differ_by_host() {
        let key_a = build_approval_key("fetch_url", &json!({"url": "https://example.com"}));
        let key_b = build_approval_key("fetch_url", &json!({"url": "https://other.org"}));
        assert_ne!(key_a, key_b);
    }

    #[test]
    fn generic_tool_keys_include_arguments() {
        let key_a = build_approval_key("read_file", &json!({"path": "a.txt"}));
        let key_b = build_approval_key("read_file", &json!({"path": "b.txt"}));
        assert_ne!(key_a, key_b);
        assert!(key_a.0.starts_with("tool:read_file:"));
    }

    #[test]
    fn generic_tool_same_arguments_reuse_key() {
        let input = json!({"path": "a.txt"});
        let key_a = build_approval_key("edit_file", &input);
        let key_b = build_approval_key("edit_file", &input);
        assert_eq!(key_a, key_b);
    }

    #[test]
    fn input_hash_is_stable_across_object_key_order() {
        let key_a = build_approval_key("write_file", &json!({"path": "a.txt", "content": "x"}));
        let key_b = build_approval_key("write_file", &json!({"content": "x", "path": "a.txt"}));
        assert_eq!(key_a, key_b);
    }

    #[test]
    fn lowercase_primitives_share_legacy_approval_keys() {
        let shell = json!({"command": "cargo test"});
        assert_eq!(
            build_approval_key("bash", &shell),
            build_approval_key("exec_shell", &shell)
        );
        let write = json!({"path": "a.txt", "content": "x"});
        assert_eq!(
            build_approval_key("write", &write),
            build_approval_key("write_file", &write)
        );
        let edit = json!({
            "path": "a.txt",
            "edits": [{"oldText": "x", "newText": "y"}]
        });
        assert_eq!(
            build_approval_key("edit", &edit),
            build_approval_key("edit_file", &edit)
        );
    }

    #[test]
    fn canonical_json_omits_trailing_commas() {
        let mut canonical = String::new();
        push_canonical_json(&json!({"b": [true, false], "a": {"x": 1}}), &mut canonical);

        assert_eq!(
            canonical,
            r#"{"a":{"x":number:1},"b":[bool:true,bool:false]}"#
        );
        assert!(!canonical.contains(",]"));
        assert!(!canonical.contains(",}"));
    }
}
