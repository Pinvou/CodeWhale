//! Immutable archive formats. Quarantine preserves pending facts without
//! converting them to a terminal result or restoring execution authority.
use anyhow::{Result, bail};
use serde::{Deserialize, Serialize};

use super::schema::{MAX_REMOTE_PROXY_FILE_BYTES, RemoteProxyRecord};

// The versioned envelope must not consume an already-full active file's
// recovery headroom. The inner record retains the original strict bound.
pub(super) const MAX_ARCHIVE_SNAPSHOT_BYTES: usize = MAX_REMOTE_PROXY_FILE_BYTES + 64;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(untagged)]
pub(super) enum ArchiveSnapshot {
    Quarantined(QuarantinedSnapshot),
    /// Keep existing pending-free archive bytes readable and unchanged.
    Archived(RemoteProxyRecord),
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct QuarantinedSnapshot {
    quarantine_schema_version: u32,
    record: RemoteProxyRecord,
}

impl ArchiveSnapshot {
    pub(super) fn quarantined(record: RemoteProxyRecord) -> Self {
        Self::Quarantined(QuarantinedSnapshot {
            quarantine_schema_version: 1,
            record,
        })
    }

    pub(super) fn record(&self) -> &RemoteProxyRecord {
        match self {
            Self::Quarantined(snapshot) => &snapshot.record,
            Self::Archived(record) => record,
        }
    }

    pub(super) fn record_mut(&mut self) -> &mut RemoteProxyRecord {
        match self {
            Self::Quarantined(snapshot) => &mut snapshot.record,
            Self::Archived(record) => record,
        }
    }

    pub(super) fn is_quarantined(&self) -> bool {
        matches!(self, Self::Quarantined(_))
    }

    pub(super) fn into_record(self) -> RemoteProxyRecord {
        match self {
            Self::Quarantined(snapshot) => snapshot.record,
            Self::Archived(record) => record,
        }
    }

    pub(super) fn validate(&self) -> Result<()> {
        self.record().validate_stored()?;
        if serde_json::to_vec(self.record())?.len() > MAX_REMOTE_PROXY_FILE_BYTES {
            bail!("remote_proxy_byte_budget");
        }
        match self {
            Self::Quarantined(snapshot) if snapshot.quarantine_schema_version != 1 => {
                bail!("remote_proxy_quarantine_version_unsupported");
            }
            Self::Archived(record) if !record.pending.is_empty() => {
                bail!("remote_proxy_archive_pending");
            }
            _ => Ok(()),
        }
    }
}

/// Both domains remain permanently reserved, including requests whose outcome
/// is unknown and requests known never to have crossed the send barrier.
pub(super) fn sealed_request_ids(
    record: &RemoteProxyRecord,
) -> impl Iterator<Item = (&String, &String)> {
    record
        .retired_requests
        .iter()
        .map(|item| (&item.client_request_id, &item.task_id))
        .chain(
            record
                .pending
                .iter()
                .map(|item| (&item.client_request_id, &item.task_id)),
        )
}
