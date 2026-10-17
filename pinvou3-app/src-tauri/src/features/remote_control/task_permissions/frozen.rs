//! Immutable B-owned session permission facts, independent of connection maps
//! and the bounded task ledger. Missing legacy facts are never inferred.

use std::ffi::OsStr;
use std::io::Read as _;

use serde::{Deserialize, Serialize};

use super::CHANGED;
use crate::features::sessions::{SerializableMode, SessionStore, validate_session_id};
use crate::platform::filesystem::{
    MovePlainFileOutcome, PrivateFileDirectory, open_existing_private_file_directory,
    open_private_file_directory,
};

const FILE_NAME: &str = "remote-task-permission.json";
const MAX_FILE_BYTES: u64 = 4096;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct FrozenPermission {
    version: u8,
    session_id: String,
    pub(super) code: bool,
    pub(super) mode: SerializableMode,
}

fn failure() -> String {
    CHANGED.to_owned()
}

fn read_from(
    directory: &PrivateFileDirectory,
    session_id: &str,
) -> Result<Option<FrozenPermission>, String> {
    let Some(file) = directory
        .open_plain_file(OsStr::new(FILE_NAME))
        .map_err(|_| failure())?
    else {
        return Ok(None);
    };
    let mut bytes = Vec::new();
    file.take(MAX_FILE_BYTES + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| failure())?;
    if bytes.len() as u64 > MAX_FILE_BYTES {
        return Err(failure());
    }
    let fact: FrozenPermission = serde_json::from_slice(&bytes).map_err(|_| failure())?;
    if fact.version != 1 || fact.session_id != session_id {
        return Err(failure());
    }
    Ok(Some(fact))
}

pub(super) fn read(
    store: &SessionStore,
    session_id: &str,
) -> Result<Option<FrozenPermission>, String> {
    validate_session_id(session_id).map_err(|_| failure())?;
    let path = store.manager.sessions_dir().join(session_id);
    let Some(directory) = open_existing_private_file_directory(&path).map_err(|_| failure())?
    else {
        return Ok(None);
    };
    read_from(&directory, session_id)
}

/// Only the creator's fresh, unclaimed session may enter this path. Publish a
/// complete fact with cross-instance no-clobber CAS, not atomic replacement.
/// A different winner or a damaged existing fact is never overwritten.
pub(super) fn register_new(
    store: &SessionStore,
    session_id: &str,
    code: bool,
    mode: SerializableMode,
) -> Result<bool, String> {
    validate_session_id(session_id).map_err(|_| failure())?;
    let expected = FrozenPermission {
        version: 1,
        session_id: session_id.to_owned(),
        code,
        mode,
    };
    let directory = open_private_file_directory(&store.manager.sessions_dir().join(session_id))
        .map_err(|_| failure())?;
    if let Some(existing) = read_from(&directory, session_id)? {
        return if existing == expected {
            Ok(false)
        } else {
            Err(failure())
        };
    }
    let stage = format!(".remote-permission-{}.json", uuid::Uuid::new_v4());
    let payload = serde_json::to_vec(&expected).map_err(|_| failure())?;
    let publish = (|| {
        directory.atomic_write_private_file(OsStr::new(&stage), &payload)?;
        directory.move_plain_file_to(OsStr::new(&stage), &directory, OsStr::new(FILE_NAME))
    })();
    // Only our unique stage can be removed. Even after a partial failure,
    // never delete a fact that may already belong to a concurrent claimant.
    let _ = directory.remove_plain_file(OsStr::new(&stage));
    // Reopen from the store's live path: an old pinned directory can outlive
    // deletion on Unix, and is not proof that this session still owns a fact.
    if read(store, session_id)? != Some(expected) {
        return Err(failure());
    }
    match publish {
        Ok(MovePlainFileOutcome::Moved) => Ok(true),
        Ok(MovePlainFileOutcome::AlreadyMoved) => Ok(false),
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => Ok(false),
        _ => Err(failure()),
    }
}
