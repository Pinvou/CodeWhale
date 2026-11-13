//! Shared route grammar for associating and cleaning up store-managed models.
//! Parsing does not require a running store backend, so imported settings retain
//! the same conservative cleanup behavior on every platform.

pub(super) fn chat_base(value: &str) -> Option<String> {
    let mut url = reqwest::Url::parse(value).ok()?;
    if !matches!(url.scheme(), "http" | "https")
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return None;
    }
    let host = url.host_str()?;
    let host = host.trim_start_matches('[').trim_end_matches(']');
    let host = host.strip_suffix('.').unwrap_or(host);
    if !host.eq_ignore_ascii_case("localhost")
        && !host
            .parse::<std::net::IpAddr>()
            .is_ok_and(|address| address.is_loopback())
    {
        return None;
    }
    match url.path().trim_end_matches('/') {
        "/v1" | "/v1/chat/completions" => url.set_path("/v1"),
        _ => return None,
    }
    Some(url.to_string())
}

#[cfg(test)]
mod tests {
    use super::chat_base;

    #[test]
    fn association_and_cleanup_share_local_chat_route_grammar_on_every_platform() {
        for (input, expected) in [
            ("http://127.0.0.1:8000/v1", "http://127.0.0.1:8000/v1"),
            (
                "http://127.0.0.2:8000/v1/chat/completions/",
                "http://127.0.0.2:8000/v1",
            ),
            ("https://LOCALHOST.:8443/v1/", "https://localhost.:8443/v1"),
            ("http://[::1]:8000/v1", "http://[::1]:8000/v1"),
            ("http://127.1/v1", "http://127.0.0.1/v1"),
        ] {
            assert_eq!(chat_base(input).as_deref(), Some(expected), "{input}");
            assert!(super::super::is_store_managed_route(input), "{input}");
        }
        for input in [
            "https://example.com/v1",
            "http://192.168.1.1/v1",
            "http://0.0.0.0/v1",
            "http://[::]/v1",
            "http://localhost../v1",
            "http://localhost.example.com/v1",
            "ftp://127.0.0.1/v1",
            "ws://127.0.0.1/v1",
            "http://name:secret@127.0.0.1/v1",
            "http://127.0.0.1/v1?key=secret",
            "http://127.0.0.1/v1#fragment",
            "http://127.0.0.1/generate",
            "http://127.0.0.1/v1/models",
            "not a URL",
        ] {
            assert!(chat_base(input).is_none(), "{input}");
            assert!(!super::super::is_store_managed_route(input), "{input}");
        }
    }
}
