//! Presentation-token classification inherited from A07. Not an authorization
//! policy or a replacement for fixed redaction of unrestricted permission inputs.
//! A08 owns bounded streaming and URL/path handling in its journal sanitizer.

pub(super) fn looks_like_secret(lowered: &str) -> bool {
    [
        "sk-",
        "token=",
        "token:",
        "access_token=",
        "access_token:",
        "access-token=",
        "access-token:",
        "api_key",
        "api-key",
        "secret=",
        "secret:",
        "password=",
        "password:",
        "passwd=",
        "passwd:",
        "pwd=",
        "pwd:",
        "client_secret=",
        "client_secret:",
        "client-secret=",
        "client-secret:",
        "refresh_token=",
        "refresh_token:",
        "refresh-token=",
        "refresh-token:",
        "signature=",
        "signature:",
        "x-amz-signature=",
        "sig=",
        "sig:",
    ]
    .iter()
    .any(|marker| lowered.contains(marker))
}

/// Classify only a field name, never a value or an entire free-text sentence.
/// Streaming consumers resolve a separated ':'/'=' before entering line redaction.
pub(super) fn is_credential_field(lowered: &str) -> bool {
    let field = lowered
        .trim_start_matches(|ch: char| !ch.is_ascii_alphanumeric() && !matches!(ch, '_' | '-'));
    let key = field
        .split([':', '='])
        .next()
        .unwrap_or_default()
        .trim_end_matches(|ch: char| !ch.is_ascii_alphanumeric() && !matches!(ch, '_' | '-'));
    matches!(
        key,
        "token"
            | "access_token"
            | "access-token"
            | "api_key"
            | "api-key"
            | "x-api-key"
            | "secret"
            | "password"
            | "passwd"
            | "pwd"
            | "client_secret"
            | "client-secret"
            | "refresh_token"
            | "refresh-token"
            | "proxy-authorization"
            | "set-cookie"
            | "signature"
            | "sig"
            | "x-amz-signature"
            | "authorization"
            | "cookie"
    )
}

pub(super) fn is_basic_credential(value: &str) -> bool {
    use base64::Engine as _;
    let value = value.trim_matches(['\'', '"', '`', ',', ';']);
    if !value
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'+' | b'/' | b'='))
    {
        return false;
    }
    if value.len() > 4096 {
        return true;
    }
    base64::engine::general_purpose::STANDARD
        .decode(value)
        .ok()
        .map(zeroize::Zeroizing::new)
        .is_some_and(|decoded| decoded.contains(&b':'))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quoted_field_names_match_without_treating_prose_as_a_field() {
        for raw in [
            "token",
            "'api_key'",
            "{\"api_key\":",
            "x-api-key:",
            "access-token=",
            "secret",
            "signature:",
            "cookie:a=x",
            "authorization:bearer",
        ] {
            assert!(is_credential_field(raw), "{raw}");
        }
        for raw in [
            "normal",
            "token budget",
            "https://example.com/docs",
            "english 日本語",
        ] {
            assert!(!is_credential_field(raw), "{raw}");
        }
    }

    #[test]
    fn wrapped_secret_markers_include_colon_and_equals_variants() {
        for raw in [
            "'sk-fixture'",
            "[token=fixture]",
            "secret:fixture",
            "access_token:fixture",
            "sig:fixture",
        ] {
            assert!(looks_like_secret(raw), "{raw}");
        }
        assert!(!looks_like_secret("ordinary 中文 text"));
    }
}
