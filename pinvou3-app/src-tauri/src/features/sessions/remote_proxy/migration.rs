use std::collections::HashSet;
use std::fs;
use std::io::ErrorKind;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use serde::Deserialize;

use super::RemoteProxyState;
use super::schema::{
    AuthenticatedCursor, AuthenticatedProjection, MAX_REMOTE_PROXY_FILE_BYTES,
    MAX_REMOTE_PROXY_RECORDS, PendingRemoteRequest, REMOTE_PROXY_SCHEMA_VERSION, RemoteProxyFile,
    RemoteProxyRecord, RemoteUiMetadata,
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyRemoteProxyFile {
    schema_version: u32,
    records: Vec<LegacyRemoteProxyRecord>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyRemoteProxyRecord {
    local_session_id: String,
    peer_id: String,
    remote_session_ref: String,
    controlled_session_id: Option<String>,
    title: String,
}

/// The pre-projection v1 wire shape is frozen. A v2 record with fields
/// removed cannot masquerade as v1 while its file/record versions remain 2.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyV1File {
    schema_version: u32,
    records: Vec<LegacyV1Record>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyV1Record {
    schema_version: u32,
    local_session_id: String,
    peer_id: String,
    remote_session_ref: String,
    controlled_session_id: Option<String>,
    ui: RemoteUiMetadata,
    authenticated_cursor: Option<AuthenticatedCursor>,
    pending: Vec<PendingRemoteRequest>,
}

impl RemoteProxyState {
    pub(crate) fn load(path: PathBuf, sessions_dir: &Path) -> Result<Self> {
        let bytes = match fs::read(&path) {
            Ok(bytes) => bytes,
            Err(error) if error.kind() == ErrorKind::NotFound => {
                return Ok(Self {
                    path,
                    ..Self::default()
                });
            }
            Err(error) => return Err(error).with_context(|| format!("read {}", path.display())),
        };
        if bytes.len() > MAX_REMOTE_PROXY_FILE_BYTES {
            bail!("RemoteProxy file exceeds byte budget");
        }
        let raw: serde_json::Value =
            serde_json::from_slice(&bytes).with_context(|| format!("parse {}", path.display()))?;
        let version = raw
            .get("schema_version")
            .and_then(serde_json::Value::as_u64)
            .context("RemoteProxy file has no valid schema_version")?;
        let records = match version {
            0 => {
                let legacy: LegacyRemoteProxyFile = serde_json::from_value(raw)?;
                if legacy.schema_version != 0 {
                    bail!("invalid RemoteProxy v0 file");
                }
                legacy
                    .records
                    .into_iter()
                    .map(|record| RemoteProxyRecord {
                        schema_version: REMOTE_PROXY_SCHEMA_VERSION,
                        local_session_id: record.local_session_id,
                        peer_id: record.peer_id,
                        remote_session_ref: record.remote_session_ref,
                        controlled_session_id: record.controlled_session_id,
                        ui: RemoteUiMetadata {
                            title: record.title,
                            ..RemoteUiMetadata::default()
                        },
                        authenticated_cursor: None,
                        retired_epochs: Vec::new(),
                        projection: AuthenticatedProjection::default(),
                        pending: Vec::new(),
                    })
                    .collect()
            }
            1 => {
                let legacy: LegacyV1File = serde_json::from_value(raw)?;
                if legacy.schema_version != 1 {
                    bail!("invalid RemoteProxy v1 file");
                }
                legacy
                    .records
                    .into_iter()
                    .map(|record| {
                        if record.schema_version != 1 {
                            bail!("invalid RemoteProxy v1 record");
                        }
                        // V1's cursor had no authenticated content slot. Drop
                        // that watermark; preserve UI and non-authoritative pending.
                        let _old_cursor = record.authenticated_cursor;
                        Ok(RemoteProxyRecord {
                            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
                            local_session_id: record.local_session_id,
                            peer_id: record.peer_id,
                            remote_session_ref: record.remote_session_ref,
                            controlled_session_id: record.controlled_session_id,
                            ui: record.ui,
                            authenticated_cursor: None,
                            retired_epochs: Vec::new(),
                            projection: AuthenticatedProjection::default(),
                            pending: record.pending,
                        })
                    })
                    .collect::<Result<Vec<_>>>()?
            }
            2 => {
                let file: RemoteProxyFile = serde_json::from_value(raw)?;
                file.records
            }
            _ => bail!("unsupported RemoteProxy schema version {version}"),
        };
        let mut state = Self {
            path,
            ..Self::default()
        };
        let mut recovered_pending = false;
        let mut references = HashSet::new();
        if records.len() > MAX_REMOTE_PROXY_RECORDS {
            bail!("RemoteProxy record capacity exceeded");
        }
        for record in records {
            record.validate_stored()?;
            if sessions_dir
                .join(format!("{}.json", record.local_session_id))
                .exists()
            {
                bail!("RemoteProxy id collides with a local Session");
            }
            if !references.insert(record.remote_session_ref.clone())
                || state
                    .records
                    .insert(record.local_session_id.clone(), record)
                    .is_some()
            {
                bail!("duplicate RemoteProxy identity");
            }
        }
        for record in state.records.values_mut() {
            for pending in &mut record.pending {
                if !pending.outcome_unknown {
                    pending.outcome_unknown = true;
                    recovered_pending = true;
                }
            }
        }
        if version < 2 || recovered_pending {
            state.persist()?;
        }
        Ok(state)
    }

    pub(super) fn persist(&self) -> Result<()> {
        if self.records.len() > MAX_REMOTE_PROXY_RECORDS {
            bail!("RemoteProxy record capacity exceeded");
        }
        let mut records: Vec<_> = self.records.values().cloned().collect();
        let mut references = HashSet::new();
        for record in &records {
            record.validate_stored()?;
            if self.records.get(&record.local_session_id) != Some(record)
                || !references.insert(&record.remote_session_ref)
            {
                bail!("duplicate or mismatched RemoteProxy identity");
            }
        }
        records.sort_by(|left, right| left.local_session_id.cmp(&right.local_session_id));
        let bytes = serde_json::to_vec_pretty(&RemoteProxyFile {
            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
            records,
        })?;
        if bytes.len() > MAX_REMOTE_PROXY_FILE_BYTES {
            bail!("RemoteProxy file exceeds byte budget");
        }
        deepseek_tui::utils::write_atomic(&self.path, &bytes)
            .with_context(|| format!("persist RemoteProxy records to {}", self.path.display()))
    }
}

pub(super) fn persist_changed(
    state: &mut RemoteProxyState,
    id: &str,
    record: RemoteProxyRecord,
) -> Result<()> {
    let previous = state.records.insert(id.to_owned(), record);
    if let Err(error) = state.persist() {
        match previous {
            Some(previous) => {
                state.records.insert(id.to_owned(), previous);
            }
            None => {
                state.records.remove(id);
            }
        }
        return Err(error);
    }
    Ok(())
}
