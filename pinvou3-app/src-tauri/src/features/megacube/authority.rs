//! Local fallback evidence is independent of download tool registration.
//! These receipts prevent an unsafe fallback; they never grant model ownership.
use std::io::{ErrorKind, Read};
use std::path::Path;

const INDEPENDENT_MARKER: &str = ".independent-records-v1";
const INDEPENDENT_ORIGIN: &[u8] = b"pinvou-independent-installation-records-v1\n";
const LOCAL_RECEIPT: &str = "local-device-observed-v1";

pub(super) fn present_or_unknown(path: &Path) -> bool {
    match std::fs::symlink_metadata(path) {
        Err(error) if error.kind() == ErrorKind::NotFound => {
            // ENOENT can also hide a dangling ancestor link. Stop at the first
            // real parent directory; links/errors are uncertainty, not absence.
            for parent in path.ancestors().skip(1) {
                match std::fs::symlink_metadata(parent) {
                    Err(error) if error.kind() == ErrorKind::NotFound => continue,
                    Ok(metadata) => return !metadata.is_dir(),
                    Err(_) => return true,
                }
            }
            false
        }
        _ => true,
    }
}

fn independent_records(records: &Path) -> bool {
    let marker = records.join(INDEPENDENT_MARKER);
    let Ok(metadata) = std::fs::symlink_metadata(&marker) else {
        return false;
    };
    if !metadata.is_file() || metadata.len() != INDEPENDENT_ORIGIN.len() as u64 {
        return false;
    }
    let mut origin = Vec::new();
    if std::fs::File::open(marker)
        .and_then(|file| file.take(64).read_to_end(&mut origin))
        .is_err()
        || origin != INDEPENDENT_ORIGIN
    {
        return false;
    }
    let Ok(entries) = std::fs::read_dir(records) else {
        return false;
    };
    for entry in entries {
        let Ok(entry) = entry else { return false };
        if entry.file_name() == INDEPENDENT_MARKER {
            continue;
        }
        if !["tasks", "locks", "artifacts", "validations"]
            .iter()
            .any(|name| entry.file_name() == *name)
            || entry.file_type().map_or(true, |kind| !kind.is_dir())
        {
            return false;
        }
    }
    true
}

fn expected(home: &Path, socket: &Path, explicit: bool) -> bool {
    let root = home.join("megacube");
    if explicit
        || present_or_unknown(socket)
        || [
            root.join(LOCAL_RECEIPT),
            root.join("factory-receipt.json"),
            root.join("cmsd-component/service.json"),
            root.join("cmsd-component/backup.json"),
            root.join("cmsd-component/operation.json"),
        ]
        .iter()
        .any(|path| present_or_unknown(path))
    {
        return true;
    }
    let records = root.join("installations");
    match std::fs::symlink_metadata(&records) {
        Err(error) if error.kind() == ErrorKind::NotFound => present_or_unknown(&records),
        Ok(metadata) if metadata.is_dir() => !independent_records(&records),
        // Old unmarked/empty directories cannot prove which writer created
        // them. Broken records, links and denied metadata stay unconfirmed.
        _ => true,
    }
}

pub(super) fn local_authority_expected() -> bool {
    let explicit = ["CMSD_SOCKET", "PINVOU3_CMSD_UNIT"]
        .iter()
        .any(|key| std::env::var_os(key).is_some_and(|value| !value.is_empty()));
    expected(
        &crate::platform::paths::pinvou3_home(),
        &super::socket_path(),
        explicit,
    ) || present_or_unknown(Path::new(super::client::DEFAULT_SOCKET_PATH))
}

pub(super) fn remember_local_device() -> Result<(), String> {
    let path = crate::platform::paths::pinvou3_home()
        .join("megacube")
        .join(LOCAL_RECEIPT);
    crate::platform::filesystem::atomic_write_private_anchored(&path, b"cmsd\n")
        .map_err(|error| format!("device_record_failed: {error}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn remote_or_download_records_are_not_local_authority_evidence() {
        let home = tempfile::tempdir().unwrap();
        let socket = home.path().join("missing.sock");
        assert!(!expected(home.path(), &socket, false));
        assert!(expected(home.path(), &socket, true));
        let records = home.path().join("megacube/installations");
        std::fs::create_dir_all(records.join("tasks")).unwrap();
        assert!(expected(home.path(), &socket, false), "legacy ambiguity");
        std::fs::write(records.join(INDEPENDENT_MARKER), INDEPENDENT_ORIGIN).unwrap();
        assert!(!expected(home.path(), &socket, false));
        std::fs::create_dir_all(home.path().join("megacube/remote-connections")).unwrap();
        assert!(!expected(home.path(), &socket, false));
        std::fs::write(records.join(".origin-pending"), b"uncertain").unwrap();
        assert!(expected(home.path(), &socket, false));
        std::fs::remove_file(records.join(".origin-pending")).unwrap();
        std::fs::create_dir(records.join("ownership")).unwrap();
        assert!(expected(home.path(), &socket, false));
    }

    #[test]
    fn receipts_corrupt_records_and_metadata_errors_keep_local_fallback_closed() {
        for name in [
            LOCAL_RECEIPT,
            "factory-receipt.json",
            "cmsd-component/service.json",
            "cmsd-component/backup.json",
            "cmsd-component/operation.json",
            "installations",
        ] {
            let home = tempfile::tempdir().unwrap();
            let path = home.path().join("megacube").join(name);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, b"invalid or old receipt").unwrap();
            assert!(expected(home.path(), &home.path().join("missing"), false));
        }
        let home = tempfile::tempdir().unwrap();
        let file = home.path().join("file");
        std::fs::write(&file, b"not a directory").unwrap();
        assert!(present_or_unknown(&file.join("socket")));
        assert!(expected(home.path(), &file.join("socket"), false));
    }
}
