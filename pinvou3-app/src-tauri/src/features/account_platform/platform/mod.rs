//! Platform capability boundary for desktop OIDC.

#[cfg(any(target_os = "windows", target_os = "macos", target_os = "linux"))]
pub const fn desktop_oidc_supported() -> bool {
    true
}

#[cfg(not(any(target_os = "windows", target_os = "macos", target_os = "linux")))]
pub const fn desktop_oidc_supported() -> bool {
    false
}

#[cfg(all(test, target_os = "windows"))]
mod tests {
    use codewhale_secrets::{DefaultKeyringStore, KeyringStore};

    struct OwnedEntry(DefaultKeyringStore);
    impl Drop for OwnedEntry {
        fn drop(&mut self) {
            let _ = self.0.delete("isolated-diagnostic-entry");
        }
    }

    #[test]
    fn actual_windows_credential_store_roundtrip_uses_only_own_random_service() {
        let service = format!(
            "pinvou-account-a03-diagnostic-{}",
            super::super::random_urlsafe(18)
        );
        let entry = OwnedEntry(DefaultKeyringStore::new(service));
        entry
            .0
            .probe()
            .expect("native credential store must be reachable");
        assert!(entry.0.get("isolated-diagnostic-entry").unwrap().is_none());
        entry
            .0
            .set(
                "isolated-diagnostic-entry",
                "public-nonsecret-diagnostic-value",
            )
            .unwrap();
        assert!(
            entry.0.get("isolated-diagnostic-entry").unwrap().as_deref()
                == Some("public-nonsecret-diagnostic-value")
        );
        entry.0.delete("isolated-diagnostic-entry").unwrap();
        assert!(entry.0.get("isolated-diagnostic-entry").unwrap().is_none());
    }
}
