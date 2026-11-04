//! Session-owned named copies for model references that can outlive a turn.
//! Parser fd paths must never become durable prompt/ACP resource addresses.
use std::ffi::OsStr;
use std::io;
use std::path::{Path, PathBuf};

/// Pin the existing ledger before conversion can block. A late parser must
/// never recreate a session which was deleted while conversion was running.
pub(in crate::features::files) struct ModelReferenceScope {
    session: PathBuf,
    directory: crate::platform::filesystem::PrivateFileDirectory,
}

impl ModelReferenceScope {
    pub(in crate::features::files) fn open(session: &Path) -> io::Result<Self> {
        let directory = crate::platform::filesystem::open_existing_private_file_directory(session)?
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "attachment session missing"))?;
        Ok(Self {
            session: session.to_owned(),
            directory,
        })
    }

    pub(in crate::features::files) fn persist(
        &self,
        name: &str,
        verified_bytes: &[u8],
    ) -> io::Result<PathBuf> {
        use crate::platform::filesystem::{
            reserved_target_is_unchanged, validate_published_path_scope,
        };
        let root_path = self.session.join("attachment_references");
        let root = self
            .directory
            .create_private_child_directory(OsStr::new("attachment_references"))?;
        let identity = uuid::Uuid::new_v4().simple().to_string();
        let directory = root.create_private_child_directory(OsStr::new(&identity))?;
        let path = root_path.join(&identity).join(name);
        let result = (|| {
            directory.atomic_write_private_file(OsStr::new(name), verified_bytes)?;
            let file = directory
                .open_plain_file(OsStr::new(name))?
                .ok_or_else(|| {
                    io::Error::new(io::ErrorKind::NotFound, "attachment reference missing")
                })?;
            if !reserved_target_is_unchanged(&file, &path) {
                return Err(io::Error::other("attachment reference changed"));
            }
            validate_published_path_scope(&path, &self.session)?;
            Ok(path)
        })();
        if result.is_err() {
            // Retain unknown/replaced entries. Session retention owns the directory
            // after success, independently of the upload receipt and its fd owner.
            drop(directory);
            let _ = root.remove_empty_child_directory(OsStr::new(&identity));
        }
        result
    }
}

#[cfg(all(test, target_os = "linux", target_pointer_width = "64"))]
mod tests {
    use super::*;

    #[test]
    fn a_late_conversion_cannot_recreate_a_deleted_session_ledger() {
        let root = tempfile::tempdir().unwrap();
        let session = root.path().join("session");
        std::fs::create_dir(&session).unwrap();
        let scope = ModelReferenceScope::open(&session).unwrap();
        std::fs::remove_dir(&session).unwrap();
        assert_eq!(
            scope
                .persist("content.md", b"late extracted body")
                .unwrap_err()
                .kind(),
            io::ErrorKind::NotFound
        );
        assert!(!session.exists());
        assert_eq!(
            ModelReferenceScope::open(&session).err().unwrap().kind(),
            io::ErrorKind::NotFound
        );
        assert!(!session.exists());
    }
}
