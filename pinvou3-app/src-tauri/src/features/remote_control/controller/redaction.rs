//! Shared bounded presentation-text policy. This is not an authorization
//! check, nor a substitute for fixed redaction of unrestricted permission
//! inputs. Both journal text and A07 permission previews use this policy.

pub(crate) fn sanitize_text(text: &str, max_chars: usize) -> String {
    let mut out = String::with_capacity(text.len().min(max_chars));
    let mut sensitive_previous_line = false;
    for line in text.lines() {
        // HTTP obsolete line folding keeps credential values in indented
        // continuation lines. Never reset redaction at that boundary.
        let mut redact_line = sensitive_previous_line && line.starts_with([' ', '\t']);
        let mut tokens = line.split_whitespace().peekable();
        while let Some(token) = tokens.next() {
            let trimmed = token.trim_matches(|ch: char| {
                matches!(
                    ch,
                    '\'' | '"' | '`' | ',' | ';' | '(' | ')' | '[' | ']' | '@'
                )
            });
            let lowered = trimmed.to_ascii_lowercase();
            let credential_scheme = lowered == "bearer"
                || (lowered == "basic"
                    && tokens
                        .peek()
                        .is_some_and(|value| is_basic_credential(value)));
            let field = lowered.trim_start_matches(|ch: char| {
                !ch.is_ascii_alphanumeric() && !matches!(ch, '_' | '-')
            });
            let key = field
                .split([':', '='])
                .next()
                .unwrap_or_default()
                .trim_end_matches(|ch: char| {
                    !ch.is_ascii_alphanumeric() && !matches!(ch, '_' | '-')
                });
            let key_value_syntax = token.contains([':', '='])
                || tokens
                    .peek()
                    .is_some_and(|next| next.starts_with([':', '=']));
            let sensitive_key = key_value_syntax
                && matches!(
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
                        | "signature"
                        | "sig"
                        | "x-amz-signature"
                );
            // A clearly identified credential value may contain whitespace or
            // quoted JSON/shell syntax. Its end is not safely inferable from
            // tokens: fail closed for the remainder of this line.
            redact_line |= credential_scheme || sensitive_key;
            redact_line |= key_value_syntax
                && matches!(
                    key,
                    "authorization" | "proxy-authorization" | "cookie" | "set-cookie"
                );
            let piece = if looks_like_path(trimmed) {
                "[PATH]".to_owned()
            } else if redact_line || looks_like_secret(&lowered) {
                "[REDACTED]".to_owned()
            } else if let Some(url) = sanitize_url_token(token) {
                url
            } else {
                token.to_owned()
            };
            if !out.is_empty() {
                out.push(' ');
            }
            out.push_str(&piece);
            if out.chars().count() >= max_chars {
                return out.chars().take(max_chars).collect();
            }
        }
        sensitive_previous_line = redact_line;
    }
    out
}

fn looks_like_secret(lowered: &str) -> bool {
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

fn is_basic_credential(value: &str) -> bool {
    use base64::Engine as _;
    let value = value.trim_matches(['\'', '"', '`', ',', ';']);
    if !value
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'+' | b'/' | b'='))
    {
        return false;
    }
    // Bound the temporary decode without passing through an oversized value.
    if value.len() > 4096 {
        return true;
    }
    base64::engine::general_purpose::STANDARD
        .decode(value)
        .ok()
        .map(zeroize::Zeroizing::new)
        .is_some_and(|decoded| decoded.contains(&b':'))
}

fn sanitize_url_token(token: &str) -> Option<String> {
    let lowered = token.to_ascii_lowercase();
    let start = lowered
        .find("https://")
        .or_else(|| lowered.find("http://"))?;
    let url = &token[start..];
    let scheme_bytes = if lowered[start..].starts_with("https://") {
        8
    } else {
        7
    };
    let authority = &lowered[start + scheme_bytes..];
    let authority_end = authority
        .find(['/', '?', '#', ')', ']'])
        .unwrap_or(authority.len());
    if authority[..authority_end].contains('@') {
        return Some("[REDACTED]".to_owned());
    }
    let sensitive_start = url.find(['?', '#'])?;
    let suffix_start = url[sensitive_start..]
        .find([')', ']', '>', ',', ';'])
        .map(|offset| sensitive_start + offset)
        .unwrap_or(url.len());
    Some(format!(
        "{}{}{}",
        &token[..start],
        &url[..sensitive_start],
        &url[suffix_start..]
    ))
}

fn looks_like_path(value: &str) -> bool {
    let lowered = value.to_ascii_lowercase();
    if lowered.contains("https://") || lowered.contains("http://") {
        return false;
    }
    let bytes = value.as_bytes();
    if bytes.len() >= 3
        && bytes[0].is_ascii_alphabetic()
        && bytes[1] == b':'
        && matches!(bytes[2], b'\\' | b'/')
    {
        return true;
    }
    let separators = value.matches(['\\', '/']).count();
    separators >= 2
        || (separators == 1 && value.len() > 1 && (value.starts_with('/') || value.contains('\\')))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quoted_and_header_credentials_are_redacted() {
        for raw in [
            "'sk-test-fixture'",
            "[SK-test-fixture]",
            "`token=fixture`",
            "(api_key=fixture)",
            "Bearer plain-fixture",
            "Authorization: Bearer plain-fixture",
            "Cookie: a=fixture b=fixture",
            r#"{"api_key": "plain-fixture"}"#,
            r#"{"api_key" : "plain-fixture"}"#,
            "token= plain-fixture",
            "secret = plain-fixture",
            "X-Api-Key: plain-fixture",
            "access-token: plain-fixture",
            "signature: plain-fixture",
            "token: plain-fixture",
            "secret:plain-fixture",
            r#"secret = "plain fixture phrase""#,
            r#"{"api_key" : "plain fixture phrase"}"#,
            "Authorization:Bearer plain-fixture",
            "Cookie:a=fixture b=fixture",
            "password: plain-fixture",
            "PASSWORD = plain fixture phrase",
            r#"{"password" : "plain fixture phrase"}"#,
            "passwd=plain-fixture",
            "pwd : plain-fixture",
            "client_secret=plain-fixture",
            "refresh-token: plain-fixture",
            "Basic dXNlcjpwbGFpbi1maXh0dXJl",
            "Authorization: Basic\n cGxhaW4tZml4dHVyZQ==",
            "Proxy-Authorization: Basic\r\n dXNlcjpwbGFpbi1maXh0dXJl",
            "Set-Cookie:\r\n\tplain-fixture",
            "Cookie:\n\tplain-fixture\n another-fixture",
        ] {
            let sanitized = sanitize_text(raw, 4096);
            assert!(!sanitized.contains("fixture"));
            assert!(!sanitized.contains("cGxhaW4tZml4dHVyZQ"));
            assert!(!sanitized.contains("dXNlcjpwbGFpbi1maXh0dXJl"));
            assert!(sanitized.contains("[REDACTED]"));
        }
        assert_eq!(
            sanitize_text("Authorization: fixture\nnormal text", 4096),
            "[REDACTED] [REDACTED] normal text"
        );
        assert_eq!(
            sanitize_text("basic alphabet and basic algebra", 4096),
            "basic alphabet and basic algebra"
        );
        assert_eq!(
            sanitize_text("Authorization: fixture\r\n\r\n indented normal text", 4096),
            "[REDACTED] [REDACTED] indented normal text"
        );
    }

    #[test]
    fn paths_and_url_secrets_are_hidden_but_public_text_remains() {
        assert_eq!(
            sanitize_text("`C:\\Users\\fixture\\note.txt`", 4096),
            "[PATH]"
        );
        assert_eq!(sanitize_text("[/tmp/fixture.txt]", 4096), "[PATH]");
        assert_eq!(
            sanitize_text("https://user:fixture@example.com/a", 4096),
            "[REDACTED]"
        );
        assert_eq!(
            sanitize_text("[docs](https://example.com/a?page=1#top)", 4096),
            "[docs](https://example.com/a)"
        );
        assert_eq!(
            sanitize_text("普通文本 English 日本語 https://example.com/docs", 4096),
            "普通文本 English 日本語 https://example.com/docs"
        );
        assert_eq!(
            sanitize_text("token budget = 10 normal: text", 4096),
            "token budget = 10 normal: text"
        );
        assert_eq!(sanitize_text("中文测试", 3), "中文测");
    }
}
