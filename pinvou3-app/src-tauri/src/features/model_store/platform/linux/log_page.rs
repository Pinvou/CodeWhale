//! Bounded log observations. Disk receipts retain the complete original lines.
//! `next` is an absolute line cursor; `next_byte` continues one oversized line.
use serde::{Deserialize, Serialize};

pub(super) const MAX_PAGE_BYTES: usize = 512 * 1024;
const CONTENT_BYTES: usize = MAX_PAGE_BYTES - 2048;
const MAX_LINES: usize = 500;
const SUMMARY_CHARS: usize = 1000;
const MAX_RESULT_BYTES: usize = 256 * 1024;

#[derive(Clone, Copy, Default, Deserialize, Serialize)]
pub(super) struct Options {
    #[serde(default)]
    pub from_byte: usize,
    pub limit: Option<usize>,
    pub tail: Option<usize>,
    pub preview_chars: Option<usize>,
}

impl Options {
    pub fn validate(self) -> Result<Self, String> {
        if self.limit.is_some_and(|n| n == 0 || n > MAX_LINES)
            || self.tail.is_some_and(|n| n == 0 || n > MAX_LINES)
            || self
                .preview_chars
                .is_some_and(|n| n == 0 || n > SUMMARY_CHARS)
            || (self.from_byte != 0 && (self.tail.is_some() || self.preview_chars.is_some()))
        {
            return Err("invalid_log_page".into());
        }
        Ok(self)
    }
}

#[derive(Serialize)]
pub(super) struct Fragment {
    pub line: usize,
    pub offset: usize,
    pub text: String,
    pub complete: bool,
}

#[derive(Serialize)]
pub(super) struct Page {
    pub from: usize,
    pub lines: Vec<String>,
    pub next: usize,
    pub next_byte: usize,
    pub total: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fragment: Option<Fragment>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub truncated_lines: Vec<usize>,
}

fn encoded_char_len(c: char) -> usize {
    match c {
        '"' | '\\' | '\n' | '\r' | '\t' | '\u{8}' | '\u{c}' => 2,
        '\u{0}'..='\u{1f}' => 6,
        _ => c.len_utf8(),
    }
}

/// Find a UTF-8 boundary without allocating/encoding the unbounded remainder.
fn prefix(text: &str, budget: usize) -> (usize, usize) {
    let mut used = 2; // JSON string quotes.
    let mut end = 0;
    for (offset, c) in text.char_indices() {
        let cost = encoded_char_len(c);
        if used + cost > budget {
            break;
        }
        used += cost;
        end = offset + c.len_utf8();
    }
    (end, used)
}

pub(super) fn summary(text: &str) -> (String, bool) {
    let end = text
        .char_indices()
        .nth(SUMMARY_CHARS)
        .map_or(text.len(), |(i, _)| i);
    (text[..end].to_owned(), end != text.len())
}

/// Successful operation results are normally a small fixed object. A malformed
/// card name or unusually large residual-path result must not consume the log
/// transport budget; keep the original outcome in its immutable receipt.
pub(super) fn bounded_result(mut value: serde_json::Value) -> (serde_json::Value, bool) {
    struct Limit(usize);
    impl std::io::Write for Limit {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            if bytes.len() > self.0 {
                return Err(std::io::Error::other("log_result_limit"));
            }
            self.0 -= bytes.len();
            Ok(bytes.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    if serde_json::to_writer(Limit(MAX_RESULT_BYTES), &value).is_ok() {
        return (value, false);
    }
    // These are the free display strings in actual lifecycle outcomes. Keep
    // identity, cleanup paths, booleans and counters intact for their callers.
    if let Some(fields) = value.as_object_mut() {
        for key in ["model_name", "summary", "link_sync_error"] {
            if let Some(field) = fields.get_mut(key)
                && let Some(text) = field.as_str()
            {
                *field = serde_json::Value::String(summary(text).0);
            }
        }
    }
    if serde_json::to_writer(Limit(MAX_RESULT_BYTES), &value).is_ok() {
        (value, true)
    } else {
        // Future/corrupt persisted outcomes have no assumed schema. Status is
        // still truthful, and the original receipt is retained for inspection.
        (serde_json::Value::Null, true)
    }
}

pub(super) fn page(lines: &[String], from: usize, options: Options) -> Result<Page, String> {
    let options = options.validate()?;
    let mut start = options
        .tail
        .map_or(from, |tail| from.max(lines.len().saturating_sub(tail)))
        .min(lines.len());
    if options.tail.is_some() && options.preview_chars.is_some() {
        // A tail observation must contain the newest lines even when all 500
        // previews do not fit together. Count the same encoded costs backwards.
        let mut used = 0;
        let mut newest_start = lines.len();
        for index in (start..lines.len()).rev() {
            let line = &lines[index];
            let end = line
                .char_indices()
                .nth(options.preview_chars.unwrap_or(SUMMARY_CHARS))
                .map_or(line.len(), |(i, _)| i);
            let (_, cost) = prefix(&line[..end], CONTENT_BYTES);
            let cost = cost + 22;
            if used + cost > CONTENT_BYTES {
                break;
            }
            used += cost;
            newest_start = index;
        }
        start = newest_start;
    }
    let mut page = Page {
        from: start,
        lines: Vec::new(),
        next: start,
        next_byte: 0,
        total: lines.len(),
        fragment: None,
        truncated_lines: Vec::new(),
    };
    if options.from_byte != 0 && start == lines.len() {
        return Err("invalid_log_cursor".into());
    }
    let mut used = 0;
    for (index, line) in lines
        .iter()
        .enumerate()
        .skip(start)
        .take(options.limit.unwrap_or(MAX_LINES))
    {
        let offset = if index == start { options.from_byte } else { 0 };
        if offset > line.len() || !line.is_char_boundary(offset) {
            return Err("invalid_log_cursor".into());
        }
        let preview_end = options.preview_chars.map_or(line.len(), |limit| {
            line.char_indices()
                .nth(limit)
                .map_or(line.len(), |(i, _)| i)
        });
        let text = &line[offset..preview_end];
        let (end, cost) = prefix(text, CONTENT_BYTES.saturating_sub(used));
        if end < text.len() && !page.lines.is_empty() {
            break;
        }
        if offset != 0 || end < text.len() {
            if end == 0 && !text.is_empty() {
                return Err("invalid_log_page_budget".into());
            }
            let complete = offset + end == line.len();
            page.fragment = Some(Fragment {
                line: index,
                offset,
                text: text[..end].to_owned(),
                complete,
            });
            page.next = index + usize::from(complete);
            page.next_byte = if complete { 0 } else { offset + end };
            break;
        }
        page.lines.push(text.to_owned());
        page.next = index + 1;
        used += cost + 1;
        if preview_end < line.len() {
            page.truncated_lines.push(index);
            used += 21; // Absolute usize index plus separator in the metadata.
        }
    }
    Ok(page)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn terminal_backlog_pages_have_absolute_cursors_and_encoded_byte_bounds() {
        let lines = vec!["x".repeat(1024); 20_000];
        let mut from = 0;
        let mut collected = Vec::new();
        let mut pages = 0;
        while from < lines.len() {
            let page = page(&lines, from, Options::default()).unwrap();
            assert!(serde_json::to_vec(&page).unwrap().len() < MAX_PAGE_BYTES);
            assert_eq!(page.total, lines.len());
            assert_eq!(page.from, from);
            assert!(page.next > from);
            from = page.next;
            collected.extend(page.lines);
            pages += 1;
        }
        assert!(pages > 3);
        assert_eq!(collected, lines);
    }

    #[test]
    fn oversized_utf8_and_escaped_nul_line_is_reassembled_without_loss() {
        let original = "猫\0\\\"".repeat(600_000);
        let lines = vec![original.clone(), "last".into()];
        let (mut from, mut from_byte) = (0, 0);
        let mut restored = String::new();
        let mut fragments = 0;
        while from == 0 {
            let page = page(
                &lines,
                from,
                Options {
                    from_byte,
                    ..Options::default()
                },
            )
            .unwrap();
            assert!(serde_json::to_vec(&page).unwrap().len() < MAX_PAGE_BYTES);
            assert_eq!(page.total, 2);
            let fragment = page.fragment.unwrap();
            assert_eq!(fragment.offset, from_byte);
            assert_eq!(fragment.line, 0);
            assert_eq!(fragment.complete, page.next == 1);
            restored.push_str(&fragment.text);
            from = page.next;
            from_byte = page.next_byte;
            fragments += 1;
        }
        assert!(fragments > 3);
        assert_eq!(restored, original);
        assert_eq!(
            page(&lines, from, Options::default()).unwrap().lines,
            ["last"]
        );
    }

    #[test]
    fn bounded_results_preserve_lifecycle_identity_and_cleanup_evidence() {
        let value = serde_json::json!({"card_id":"card", "version_id":"v1", "purged_weights":true,
            "freed_bytes":123, "remaining_paths":["/retained/original/path"], "summary":"猫\0".repeat(100_000)});
        let (bounded, truncated) = bounded_result(value);
        assert!(truncated);
        assert_eq!(bounded["card_id"], "card");
        assert_eq!(bounded["version_id"], "v1");
        assert_eq!(
            bounded["remaining_paths"],
            serde_json::json!(["/retained/original/path"])
        );
        assert_eq!(bounded["freed_bytes"], 123);
        assert_eq!(bounded["purged_weights"], true);
        assert!(serde_json::to_vec(&bounded).unwrap().len() < MAX_RESULT_BYTES);
    }

    #[test]
    fn preview_tail_retains_true_totals_and_marks_truncated_absolute_lines() {
        let lines = vec!["old".into(), "猫\0".repeat(600_000), "last".into()];
        let page = page(
            &lines,
            0,
            Options {
                tail: Some(2),
                preview_chars: Some(1000),
                ..Options::default()
            },
        )
        .unwrap();
        assert_eq!((page.from, page.next, page.total), (1, 3, 3));
        assert_eq!(page.truncated_lines, [1]);
        assert_eq!(page.lines[0].chars().count(), 1000);
        assert_eq!(page.lines[1], "last");
        assert!(serde_json::to_vec(&page).unwrap().len() < MAX_PAGE_BYTES);
        assert!(
            self::page(
                &lines,
                1,
                Options {
                    from_byte: 1,
                    ..Options::default()
                }
            )
            .is_err()
        );
    }
}
