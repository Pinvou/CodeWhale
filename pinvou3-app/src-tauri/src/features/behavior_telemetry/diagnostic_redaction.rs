// architecture-guard: allow-platform-detail -- The closed vocabulary contains a shell tool name as diagnostic data only; it never launches a process or selects an OS adapter. The structured_facts test verifies the same categorical value on every platform.
//! Field-level redaction for explicitly previewed material, not automatic telemetry.
//! Keep this grammar and vocabulary in parity with Relay diagnostic-redaction.js.
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};

const REMOVED: &str = "[removed]";
const CONTENT: &str = "prompt messages requestbody responsebody userinput outputtext body content input output stderr stdout message stack";
const CREDENTIALS: &str = "apikey accesskey accesskeyid secretaccesskey accesstoken refreshtoken idtoken token clientsecret secret password passwd authorization proxyauthorization cookie setcookie privatekey credentials";
const IDS: &str = "requestid taskid sessionid traceid eventid operationid correlationid xrequestid";
const CONTAINERS: &str =
    "error errors headers fault diagnostic details context environment cause result events steps";
const NUMBERS: &str = "httpstatus statuscode exitcode errno inputtokens outputtokens totaltokens limittokens limit retryafterms retryafter deadlinems elapsedms durationms offsetms receivedframes expectedframes attempt attempts";
const BOOLEANS: &str = "billingenabled executionstarted retryable hasattachment responsebodyunavailable retryafterunavailable inferred";
const CATEGORIES: &str = "code errorcode diagnosticcode transportcode component diagnosticcomponent stage phase event status ataskstatus btaskstatus previoustaskstatus relayreason tool filerole op mode remotetaskpermissions";
const SPELLINGS: &str = "api_key access_key access_key_id secret_access_key access_token refresh_token id_token client_secret proxy_authorization set-cookie private_key request_body response_body user_input output_text request_id task_id session_id trace_id event_id operation_id correlation_id x-request-id http_status status_code exit_code input_tokens output_tokens total_tokens limit_tokens retry_after_ms retry_after deadline_ms elapsed_ms duration_ms offset_ms received_frames expected_frames billing_enabled execution_started has_attachment response_body_unavailable retry_after_unavailable error_code diagnostic_code transport_code diagnostic_component a_task_status b_task_status previous_task_status relay_reason file_role remote_task_permissions";
const VOCABULARY: &str = "unknown permission_denied permission_required permission_changed authentication_failed configuration_missing network_unavailable connection_reset dns_failed tls_failed request_timeout rate_limited quota_exhausted context_length_exceeded model_not_found provider_unavailable stream_unexpected_eof response_invalid tool_failed process_failed unsupported validation_failed internal_error shutdown_unconfirmed capability_stale remote_disconnected remote_result_unknown artifact_missing attachment_failed user_reported_quality key_revoked invalid_api_key invalid_api_key_error invalid_authentication_error authentication_error unauthorized forbidden rate_limit_exceeded insufficient_quota upstream_unavailable stream_incomplete upstream_reset deadline_exceeded task_permission_mode_required endpoint_not_live result_delivery_rejected request_started render_worker_failed ECONNRESET ENOENT EACCES EPERM ETIMEDOUT ENOTFOUND EPIPE ECONNREFUSED DEADLINE_EXCEEDED model proxy filesystem configuration transport app assistant acp tool remote_control voice unknown submission queue permission recording recognition output shutdown response_headers response_read forward_response connect read write open spawn succeeded failed waiting running pending interrupted completed rejected cancelled canceled success error ok unavailable renderer shell bash powershell web web_search read_file edit_file search_files model_config yolo plan";

fn contains(list: &str, value: &str) -> bool {
    list.split_whitespace().any(|item| item == value)
}
fn normalize(key: &str) -> String {
    key.chars()
        .filter(|ch| !matches!(ch, '-' | '_'))
        .collect::<String>()
        .to_ascii_lowercase()
}
fn display(key: &str) -> String {
    SPELLINGS
        .split_whitespace()
        .find(|s| normalize(s) == key)
        .unwrap_or(key)
        .to_owned()
}
fn safe_marker(line: &str) -> bool {
    [REMOVED, "[unparsed material removed]", "[truncated]"].contains(&line)
}

pub(super) struct Redactor<'a> {
    scope: &'a str,
}
impl<'a> Redactor<'a> {
    pub(super) fn new(scope: &'a str) -> Self {
        Self { scope }
    }

    fn alias(&self, kind: &str, value: &Value) -> Value {
        let material = json!([self.scope, kind, value]).to_string();
        let digest = Sha256::digest(material.as_bytes());
        let hex: String = digest
            .iter()
            .take(12)
            .map(|byte| format!("{byte:02x}"))
            .collect();
        json!(format!("[{kind}:{hex}]"))
    }
    fn scalar(&self, key: &str, value: &Value) -> Value {
        if contains(IDS, key) {
            return if value
                .as_str()
                .is_some_and(|s| !s.is_empty() && s.len() <= 1024)
            {
                self.alias("id", value)
            } else {
                json!(REMOVED)
            };
        }
        if contains(BOOLEANS, key) {
            return if value.is_boolean() {
                value.clone()
            } else {
                json!(REMOVED)
            };
        }
        if contains(NUMBERS, key) || (key == "status" && value.is_number()) {
            let number = value.as_f64().filter(|n| {
                n.is_finite()
                    && n.fract() == 0.0
                    && match key {
                        "httpstatus" | "statuscode" | "status" => (100.0..=599.0).contains(n),
                        "exitcode" => (f64::from(i32::MIN)..=f64::from(u32::MAX)).contains(n),
                        _ => (0.0..=1_000_000_000.0).contains(n),
                    }
            });
            return if let Some(number) = number {
                json!(number as i64)
            } else {
                json!(REMOVED)
            };
        }
        if contains(CATEGORIES, key) {
            if key == "remotetaskpermissions" && value.is_null() {
                return Value::Null;
            }
            if let Some(text) = value.as_str().filter(|s| s.len() <= 128) {
                return if contains(VOCABULARY, text) {
                    value.clone()
                } else {
                    self.alias("value", value)
                };
            }
        }
        json!(REMOVED)
    }
    fn visit(&self, value: &Value, depth: usize) -> Value {
        if depth > 16 {
            return json!(REMOVED);
        }
        if let Some(items) = value.as_array() {
            return Value::Array(
                items
                    .iter()
                    .take(128)
                    .map(|item| self.visit(item, depth + 1))
                    .collect(),
            );
        }
        let Some(fields) = value.as_object() else {
            return json!(REMOVED);
        };
        let mut result = Map::new();
        for (key, item) in fields.iter().take(256) {
            let name = normalize(key);
            if result.contains_key(&display(&name)) {
                result.insert(display(&name), json!(REMOVED));
                continue;
            }
            let cleaned = if contains(CONTENT, &name) || contains(CREDENTIALS, &name) {
                json!(REMOVED)
            } else if contains(CONTAINERS, &name) {
                self.visit(item, depth + 1)
            } else if known_scalar(&name) {
                self.scalar(&name, item)
            } else {
                result.insert("unknown_fields_removed".into(), json!(true));
                continue;
            };
            result.insert(display(&name), cleaned);
        }
        Value::Object(result)
    }
    pub(super) fn redact(&self, input: &str) -> String {
        if input.len() > 65536 {
            return "[unparsed material removed]".into();
        }
        // Parse complete pretty JSON before considering individual log records.
        if let Ok(value) = serde_json::from_str::<Value>(input) {
            return self.visit(&value, 0).to_string();
        }
        let lines: Vec<_> = input
            .split('\n')
            .map(|s| s.strip_suffix('\r').unwrap_or(s))
            .collect();
        if input.trim_start().starts_with(['{', '['])
            && !lines.iter().all(|line| {
                line.trim().is_empty()
                    || safe_marker(line)
                    || serde_json::from_str::<Value>(line)
                        .is_ok_and(|v| v.is_object() || v.is_array())
            })
        {
            return "[unparsed material removed]".into();
        }
        let mut output = Vec::new();
        let mut blocked = false;
        for line in lines {
            if blocked {
                output.push(REMOVED.to_owned());
                continue;
            }
            if safe_marker(line) {
                output.push(line.to_owned());
                continue;
            }
            if let Ok(value) = serde_json::from_str::<Value>(line) {
                output.push(self.visit(&value, 0).to_string());
                continue;
            }
            if let Some(pairs) = key_values(line).filter(|p| !p.is_empty()) {
                let mut record = Map::new();
                for (name, value) in pairs {
                    if record.contains_key(&display(&name)) {
                        record.insert(display(&name), json!(REMOVED));
                        blocked = true;
                        continue;
                    }
                    if contains(CONTENT, &name) || contains(CREDENTIALS, &name) {
                        record.insert(display(&name), json!(REMOVED));
                        blocked = true;
                    } else if known_scalar(&name) {
                        let cleaned = self.scalar(&name, &value);
                        if cleaned.as_str() == Some(REMOVED)
                            || (contains(CATEGORIES, &name)
                                && cleaned.as_str().is_some_and(|s| s.starts_with("[value:")))
                        {
                            blocked = true;
                        }
                        record.insert(display(&name), cleaned);
                    } else {
                        record.insert("unknown_fields_removed".into(), json!(true));
                        blocked = true;
                    }
                }
                output.push(Value::Object(record).to_string());
            } else {
                let risky = risky_prose(line);
                output.push(if risky {
                    REMOVED.into()
                } else {
                    line.to_owned()
                });
                if risky {
                    blocked = true;
                }
            }
        }
        output.join("\n")
    }
    pub(super) fn bounded(&self, input: &str, max: usize, utf16: bool) -> String {
        let cleaned = self.redact(input);
        let width = |ch: char| if utf16 { ch.len_utf16() } else { ch.len_utf8() };
        if cleaned.chars().map(width).sum::<usize>() <= max {
            return cleaned;
        }
        let suffix = "\n[truncated]";
        let mut units = 0;
        let mut result = String::new();
        for ch in cleaned.chars() {
            if units + width(ch) > max.saturating_sub(suffix.len()) {
                break;
            }
            units += width(ch);
            result.push(ch);
        }
        result.push_str(suffix);
        result
    }
}

fn known_scalar(name: &str) -> bool {
    [IDS, NUMBERS, BOOLEANS, CATEGORIES]
        .iter()
        .any(|list| contains(list, name))
}
fn risky_prose(line: &str) -> bool {
    let lower = line.to_ascii_lowercase();
    if line.chars().any(|ch| "=:{[<\\/@%\"'`".contains(ch) || ch.is_control() || matches!(ch, '\u{200b}'..='\u{200f}' | '\u{202a}'..='\u{202e}' | '\u{2060}'..='\u{206f}' | '\u{feff}')) { return true; }
    if line.contains("~~~")
        || line.as_bytes().windows(20).any(|word| {
            (word[..4].eq_ignore_ascii_case(b"akia") || word[..4].eq_ignore_ascii_case(b"asia"))
                && word[4..].iter().all(u8::is_ascii_alphanumeric)
        })
    {
        return true;
    }
    if [
        "bearer",
        "password",
        "passwd",
        "token",
        "secret",
        "cookie",
        "authorization",
        "api_key",
        "api-key",
        "apikey",
        "prompt",
        "messages",
        "content",
        "body",
        "sk-",
        "ghp_",
        "gho_",
        "ghu_",
        "ghs_",
        "ghr_",
        "github_pat_",
        "begin",
    ]
    .iter()
    .any(|marker| lower.contains(marker))
    {
        return true;
    }
    line.split(|ch: char| !ch.is_ascii_alphanumeric() && !"+_-".contains(ch))
        .any(|word| word.len() >= 32)
}
fn key_values(line: &str) -> Option<Vec<(String, Value)>> {
    let mut offset = 0;
    let bytes = line.as_bytes();
    let mut pairs = Vec::new();
    while offset < bytes.len() {
        while offset < bytes.len() && bytes[offset].is_ascii_whitespace() {
            offset += 1;
        }
        if offset == bytes.len() {
            break;
        }
        let key_start = offset;
        if !bytes[offset].is_ascii_alphabetic() {
            return None;
        }
        offset += 1;
        while offset < bytes.len()
            && (bytes[offset].is_ascii_alphanumeric() || b"_-".contains(&bytes[offset]))
        {
            offset += 1;
        }
        if offset - key_start > 64 {
            return None;
        }
        let key = normalize(&line[key_start..offset]);
        while offset < bytes.len() && bytes[offset].is_ascii_whitespace() {
            offset += 1;
        }
        if offset == bytes.len() || !b"=:".contains(&bytes[offset]) {
            return None;
        }
        offset += 1;
        while offset < bytes.len() && bytes[offset].is_ascii_whitespace() {
            offset += 1;
        }
        let start = offset;
        if bytes.get(offset) == Some(&b'"') {
            offset += 1;
            while offset < bytes.len() && bytes[offset] != b'"' {
                if bytes[offset] == b'\\' {
                    offset += 1;
                }
                offset += 1;
            }
            if bytes.get(offset) != Some(&b'"') {
                return None;
            }
            offset += 1;
        } else {
            while offset < bytes.len() && !bytes[offset].is_ascii_whitespace() {
                offset += 1;
            }
        }
        if start == offset || (offset < bytes.len() && !bytes[offset].is_ascii_whitespace()) {
            return None;
        }
        let raw = &line[start..offset];
        let value = serde_json::from_str(raw).unwrap_or_else(|_| json!(raw));
        pairs.push((key, value));
        if pairs.len() > 256 {
            return None;
        }
    }
    Some(pairs)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bounded_output_preserves_utf16_pairs_and_counts_expanded_bytes() {
        let redactor = Redactor::new("bounded-test-scope");
        let notes = redactor.bounded(
            &format!("{}{}", "中".repeat(5986), "🦀".repeat(20)),
            6000,
            true,
        );
        assert_eq!(notes.encode_utf16().count(), 6000);
        assert!(notes.ends_with("\n[truncated]"));
        assert!(notes.contains('🦀'));
        assert!(!notes.contains('\u{fffd}'));
        let odd_boundary = redactor.bounded(
            &format!("{}{}", "中".repeat(5987), "🦀".repeat(20)),
            6000,
            true,
        );
        assert_eq!(odd_boundary.encode_utf16().count(), 5999);
        assert!(!odd_boundary.contains('🦀'));
        assert!(odd_boundary.ends_with("\n[truncated]"));
        let attachment = redactor.bounded(&"request_id=x\n".repeat(4000), 65536, false);
        assert_eq!(attachment.len(), 65536);
        assert!(attachment.ends_with("\n[truncated]"));
        assert!(!attachment.contains("request_id=x"));
        assert_eq!(redactor.bounded("短い説明", 6000, true), "短い説明");
    }

    #[test]
    fn structured_facts_survive_sensitive_parents_and_unknown_names() {
        let redactor = Redactor::new("private-test-scope");
        let value = redactor.redact(r#"{"http_status":4.01e2,"input_tokens":1e4,"error":{"code":"key_revoked"},"credentials":{"code":"PRIVATE_FIXTURE"},"PRIVATE_KEY_NAME":"PRIVATE_FIXTURE"}"#);
        let parsed: Value = serde_json::from_str(&value).unwrap();
        assert_eq!(parsed["http_status"], 401);
        assert_eq!(parsed["input_tokens"], 10000);
        assert_eq!(parsed["error"]["code"], "key_revoked");
        assert!(!value.contains("PRIVATE"));
        assert_eq!(
            redactor.redact("tool=powershell"),
            r#"{"tool":"powershell"}"#
        );
    }

    #[test]
    fn unframed_or_invalid_fields_never_resume_inside_private_content() {
        let redactor = Redactor::new("private-test-scope");
        for input in [
            "prompt=hello\nAlice Person\nhttp_status=401",
            "input_tokens=first\nAlice Person\nhttp_status=401",
            "unknown_field=first\nAlice Person\nhttp_status=401",
            "code=custom_private_code\nAlice Person\nhttp_status=401",
            "```\nAlice Person\nhttp_status=401",
        ] {
            let cleaned = redactor.redact(input);
            assert!(!cleaned.contains("Alice"));
            assert!(!cleaned.contains("401"));
        }
    }

    #[test]
    fn scopes_preserve_local_links_and_do_not_link_reports() {
        let text = "request_id=PRIVATE_ID\nrequest_id=PRIVATE_ID";
        let first = Redactor::new("private-scope-one").redact(text);
        let lines: Vec<_> = first.lines().collect();
        assert_eq!(lines[0], lines[1]);
        assert!(!first.contains("PRIVATE"));
        assert_ne!(first, Redactor::new("private-scope-two").redact(text));
    }
}
