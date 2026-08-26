use encoding_rs::{CoderResult, Encoding};
use std::sync::{Arc, Mutex};

/// Stateful decoder for output collected from a shell pipe.
///
/// Shell output is expected to be UTF-8. On Windows, native console programs
/// can still write the process ANSI code page even after the shell has been
/// configured for UTF-8, so invalid UTF-8 switches the stream to that code
/// page. Keeping decoder state also prevents a UTF-8 or legacy multi-byte
/// character split across two polling reads from becoming U+FFFD.
pub(super) struct ShellStreamDecoder {
    pending_utf8: Vec<u8>,
    legacy_encoding: Option<&'static Encoding>,
    stream_decoder: Option<encoding_rs::Decoder>,
    finished: bool,
}

impl Default for ShellStreamDecoder {
    fn default() -> Self {
        Self::new(system_legacy_encoding())
    }
}

impl ShellStreamDecoder {
    fn new(legacy_encoding: Option<&'static Encoding>) -> Self {
        Self {
            pending_utf8: Vec::new(),
            legacy_encoding,
            stream_decoder: None,
            finished: false,
        }
    }

    pub(super) fn decode(&mut self, bytes: &[u8], last: bool) -> String {
        if self.finished {
            return String::new();
        }

        if let Some(decoder) = self.stream_decoder.as_mut() {
            let decoded = decode_legacy_chunk(decoder, bytes, last);
            self.finished = last;
            return decoded;
        }

        self.pending_utf8.extend_from_slice(bytes);
        let mut decoded = String::new();

        match std::str::from_utf8(&self.pending_utf8) {
            Ok(valid) => {
                decoded.push_str(valid);
                self.pending_utf8.clear();
                self.finished = last;
                decoded
            }
            Err(error) if error.error_len().is_none() && !last => {
                let valid_up_to = error.valid_up_to();
                let valid_prefix = std::str::from_utf8(&self.pending_utf8[..valid_up_to])
                    .expect("Utf8Error::valid_up_to must delimit valid UTF-8");
                decoded.push_str(valid_prefix);
                self.pending_utf8.drain(..valid_up_to);
                decoded
            }
            Err(_) => {
                if let Some(encoding) = self.legacy_encoding {
                    let mut decoder = encoding.new_decoder_without_bom_handling();
                    decoded.push_str(&decode_legacy_chunk(&mut decoder, &self.pending_utf8, last));
                    self.pending_utf8.clear();
                    self.stream_decoder = Some(decoder);
                } else {
                    decoded.push_str(&String::from_utf8_lossy(&self.pending_utf8));
                    self.pending_utf8.clear();
                }
                self.finished = last;
                decoded
            }
        }
    }
}

fn decode_legacy_chunk(decoder: &mut encoding_rs::Decoder, mut bytes: &[u8], last: bool) -> String {
    let capacity = decoder
        .max_utf8_buffer_length(bytes.len())
        .unwrap_or_else(|| bytes.len().saturating_mul(4).saturating_add(16));
    let mut output = String::with_capacity(capacity);
    loop {
        let (result, read, _) = decoder.decode_to_string(bytes, &mut output, last);
        bytes = &bytes[read..];
        match result {
            CoderResult::InputEmpty => return output,
            CoderResult::OutputFull => {
                let additional = decoder
                    .max_utf8_buffer_length(bytes.len())
                    .unwrap_or_else(|| bytes.len().saturating_mul(4).saturating_add(16))
                    .max(16);
                output.reserve(additional);
            }
        }
    }
}

pub(super) fn decode_shell_bytes(bytes: &[u8], last: bool) -> String {
    decode_shell_bytes_with_legacy(bytes, system_legacy_encoding(), last)
}

fn decode_shell_bytes_with_legacy(
    bytes: &[u8],
    legacy_encoding: Option<&'static Encoding>,
    last: bool,
) -> String {
    ShellStreamDecoder::new(legacy_encoding).decode(bytes, last)
}

#[cfg(windows)]
fn system_legacy_encoding() -> Option<&'static Encoding> {
    // SAFETY: GetACP takes no arguments and has no caller-owned lifetime.
    legacy_encoding_for_code_page(unsafe { windows::Win32::Globalization::GetACP() })
}

/// Best-effort mapping for the Windows ANSI process code page. This is only a
/// fallback after UTF-8 decoding fails: it cannot identify OEM code pages or
/// arbitrary encodings selected independently by a child program.
// Only the Windows `system_legacy_encoding` calls this in production; the
// mapping table itself is exercised cross-platform by the unit tests.
#[cfg_attr(not(windows), allow(dead_code))]
fn legacy_encoding_for_code_page(code_page: u32) -> Option<&'static Encoding> {
    match code_page {
        65001 => None,
        874 => Some(encoding_rs::WINDOWS_874),
        936 => Some(encoding_rs::GBK),
        950 => Some(encoding_rs::BIG5),
        932 => Some(encoding_rs::SHIFT_JIS),
        949 => Some(encoding_rs::EUC_KR),
        1250 => Some(encoding_rs::WINDOWS_1250),
        1251 => Some(encoding_rs::WINDOWS_1251),
        1252 => Some(encoding_rs::WINDOWS_1252),
        1253 => Some(encoding_rs::WINDOWS_1253),
        1254 => Some(encoding_rs::WINDOWS_1254),
        1255 => Some(encoding_rs::WINDOWS_1255),
        1256 => Some(encoding_rs::WINDOWS_1256),
        1257 => Some(encoding_rs::WINDOWS_1257),
        1258 => Some(encoding_rs::WINDOWS_1258),
        _ => None,
    }
}

#[cfg(not(windows))]
fn system_legacy_encoding() -> Option<&'static Encoding> {
    None
}

pub(super) fn take_delta_from_buffer(
    buffer: &Arc<Mutex<Vec<u8>>>,
    cursor: &mut usize,
    decoder: &mut ShellStreamDecoder,
    last: bool,
) -> (String, usize, usize) {
    let guard = buffer.lock().unwrap_or_else(|e| e.into_inner());
    let total = guard.len();
    let start = (*cursor).min(total);
    // Clone only the unread portion (the delta), not the entire accumulated buffer.
    // Long-running processes can produce megabytes of output; cloning the full
    // buffer on every poll held the ShellManager mutex for O(total_bytes) time.
    let delta = guard[start..].to_vec();
    *cursor = total;
    drop(guard);
    let delta_len = delta.len();
    (decoder.decode(&delta, last), delta_len, total)
}

/// Read only the tail of a byte buffer and return (total_len, tail_string).
///
/// Avoids cloning the full buffer when only a trailing excerpt is needed
/// (e.g. for the job-panel display). A small boundary margin ensures that a
/// tail beginning in a UTF-8 or legacy multi-byte character is trimmed away
/// before the requested character limit is returned.
pub(super) fn tail_from_buffer(
    buffer: &Arc<Mutex<Vec<u8>>>,
    max_tail_chars: usize,
    last: bool,
) -> (usize, String) {
    let guard = buffer.lock().unwrap_or_else(|e| e.into_inner());
    let total = guard.len();
    let tail = tail_from_bytes_with_legacy(&guard, max_tail_chars, last, system_legacy_encoding());
    (total, tail)
}

pub(super) fn tail_from_bytes(bytes: &[u8], max_tail_chars: usize, last: bool) -> String {
    tail_from_bytes_with_legacy(bytes, max_tail_chars, last, system_legacy_encoding())
}

fn tail_from_bytes_with_legacy(
    bytes: &[u8],
    max_tail_chars: usize,
    last: bool,
    legacy_encoding: Option<&'static Encoding>,
) -> String {
    let total = bytes.len();
    let bytes_to_read = max_tail_chars.saturating_mul(4).saturating_add(16);
    let raw_start = total.saturating_sub(bytes_to_read);
    let tail_str = if let Some(aligned_start) = stable_utf8_tail_start(bytes, raw_start, last) {
        decode_shell_bytes_with_legacy(&bytes[aligned_start..], legacy_encoding, last)
    } else {
        decode_shell_bytes_with_legacy(&bytes[raw_start..], legacy_encoding, last)
    };
    tail_text(&tail_str, max_tail_chars)
}

fn stable_utf8_tail_start(bytes: &[u8], raw_start: usize, last: bool) -> Option<usize> {
    let mut aligned_start = raw_start;
    while aligned_start < bytes.len() && (bytes[aligned_start] & 0xC0) == 0x80 {
        aligned_start += 1;
    }

    if aligned_start != raw_start {
        if aligned_start == bytes.len() {
            return None;
        }
        let mut character_start = raw_start;
        let lower_bound = raw_start.saturating_sub(3);
        while character_start > lower_bound && (bytes[character_start] & 0xC0) == 0x80 {
            character_start -= 1;
        }
        if std::str::from_utf8(&bytes[character_start..aligned_start]).is_err() {
            return None;
        }
    }

    match std::str::from_utf8(&bytes[aligned_start..]) {
        Ok(_) => Some(aligned_start),
        Err(error) if !last && error.error_len().is_none() => Some(aligned_start),
        Err(_) => None,
    }
}

fn tail_text(text: &str, max_chars: usize) -> String {
    if text.chars().count() <= max_chars {
        return text.to_string();
    }
    let tail = text
        .chars()
        .rev()
        .take(max_chars)
        .collect::<Vec<_>>()
        .into_iter()
        .rev()
        .collect::<String>();
    format!("...{tail}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn forkguard_shell_output_decoder_preserves_utf8_across_poll_boundaries() {
        let mut decoder = ShellStreamDecoder::new(None);
        let bytes = "中文".as_bytes();

        let first = decoder.decode(&bytes[..1], false);
        let second = decoder.decode(&bytes[1..4], false);
        let third = decoder.decode(&bytes[4..], true);

        assert_eq!(format!("{first}{second}{third}"), "中文");
        assert!(!format!("{first}{second}{third}").contains('\u{FFFD}'));
    }

    #[test]
    fn legacy_multibyte_split_across_polls_uses_decoder_state() {
        let (encoded, _, _) = encoding_rs::GBK.encode("中文");
        let mut decoder = ShellStreamDecoder::new(Some(encoding_rs::GBK));

        let prefix = decoder.decode(b"error: ", false);
        let first = decoder.decode(&encoded[..1], false);
        let second = decoder.decode(&encoded[1..3], false);
        let third = decoder.decode(&encoded[3..], true);

        assert_eq!(format!("{prefix}{first}{second}{third}"), "error: 中文");
    }

    #[test]
    fn complete_decoder_uses_configured_legacy_encoding() {
        let (encoded, _, _) = encoding_rs::GBK.encode("中文");
        assert_eq!(
            decode_shell_bytes_with_legacy(&encoded, Some(encoding_rs::GBK), true),
            "中文"
        );
    }

    #[test]
    fn running_full_decoder_keeps_incomplete_utf8_private_until_final() {
        assert_eq!(decode_shell_bytes(b"ready \xE4", false), "ready ");
        assert_eq!(decode_shell_bytes(b"ready \xE4", true), "ready \u{FFFD}");
    }

    #[test]
    fn ansi_code_page_mapping_includes_supported_encodings() {
        let cases = [
            (874, Some(encoding_rs::WINDOWS_874)),
            (932, Some(encoding_rs::SHIFT_JIS)),
            (936, Some(encoding_rs::GBK)),
            (949, Some(encoding_rs::EUC_KR)),
            (950, Some(encoding_rs::BIG5)),
            (1250, Some(encoding_rs::WINDOWS_1250)),
            (1251, Some(encoding_rs::WINDOWS_1251)),
            (1252, Some(encoding_rs::WINDOWS_1252)),
            (1253, Some(encoding_rs::WINDOWS_1253)),
            (1254, Some(encoding_rs::WINDOWS_1254)),
            (1255, Some(encoding_rs::WINDOWS_1255)),
            (1256, Some(encoding_rs::WINDOWS_1256)),
            (1257, Some(encoding_rs::WINDOWS_1257)),
            (1258, Some(encoding_rs::WINDOWS_1258)),
            (65001, None),
            (437, None),
        ];

        for (code_page, expected) in cases {
            assert_eq!(legacy_encoding_for_code_page(code_page), expected);
        }
    }

    #[test]
    fn tail_decoder_does_not_split_utf8_character() {
        let buffer = Arc::new(Mutex::new("prefix中文尾部".as_bytes().to_vec()));
        let (_, tail) = tail_from_buffer(&buffer, 4, true);
        assert_eq!(tail, "...中文尾部");
        assert!(!tail.contains('\u{FFFD}'));
    }

    #[test]
    fn long_utf8_tail_starts_on_a_character_boundary() {
        let text = format!("{}中文尾部", "界".repeat(64));
        let bytes = text.into_bytes();
        let unaligned = bytes.len() - 31;
        assert_eq!(bytes[unaligned] & 0xC0, 0x80);
        let aligned = stable_utf8_tail_start(&bytes, unaligned, true)
            .expect("UTF-8 continuation must align to the next character");
        assert!(std::str::from_utf8(&bytes[aligned..]).is_ok());
        let buffer = Arc::new(Mutex::new(bytes));

        let (_, tail) = tail_from_buffer(&buffer, 4, true);

        assert_eq!(tail, "...中文尾部");
        assert!(!tail.contains('\u{FFFD}'));
    }

    #[test]
    fn long_legacy_tail_uses_injected_windows_encoding() {
        let text = format!("{}中文尾部", "前".repeat(64));
        let (encoded, _, _) = encoding_rs::GBK.encode(&text);

        let tail = tail_from_bytes_with_legacy(&encoded, 4, true, Some(encoding_rs::GBK));

        assert_eq!(tail, "...中文尾部");
        assert!(!tail.contains('\u{FFFD}'));
    }

    #[test]
    fn cp1252_continuation_range_bytes_use_raw_legacy_window() {
        let bytes = vec![0x80; 64];

        let tail = tail_from_bytes_with_legacy(&bytes, 4, true, Some(encoding_rs::WINDOWS_1252));

        assert_eq!(tail, "...€€€€");
    }
}
