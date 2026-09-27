use std::collections::HashSet;
use std::fs;
use std::io::ErrorKind;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use serde::Deserialize;

use super::RemoteProxyState;
use super::schema::{
    AuthenticatedCursor, AuthenticatedProjection, MAX_REMOTE_PROXY_FILE_BYTES,
    MAX_REMOTE_PROXY_RECORDS, PendingDispatchState, PendingRemoteRequest,
    REMOTE_PROXY_SCHEMA_VERSION, RemoteProxyFile, RemoteProxyRecord, RemoteUiMetadata,
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

/// Legacy shapes are exact: a v3 record cannot masquerade as v1 or v2 by
/// erasing the new dispatch and retired-ID security fields.
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
    pending: Vec<LegacyPendingRemoteRequest>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyV2File {
    schema_version: u32,
    records: Vec<LegacyV2Record>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyV2Record {
    schema_version: u32,
    local_session_id: String,
    peer_id: String,
    remote_session_ref: String,
    controlled_session_id: Option<String>,
    ui: RemoteUiMetadata,
    authenticated_cursor: Option<AuthenticatedCursor>,
    retired_epochs: Vec<String>,
    projection: AuthenticatedProjection,
    pending: Vec<LegacyPendingRemoteRequest>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyPendingRemoteRequest {
    client_request_id: String,
    task_id: String,
    request_fingerprint: String,
    outcome_unknown: bool,
}

fn recover_legacy_pending(items: Vec<LegacyPendingRemoteRequest>) -> Vec<PendingRemoteRequest> {
    items
        .into_iter()
        .map(|item| {
            // Neither the old false nor true flag proves the send boundary was
            // never crossed. Migration must deny dispatch and abandonment.
            let _old_unknown = item.outcome_unknown;
            PendingRemoteRequest {
                client_request_id: item.client_request_id,
                task_id: item.task_id,
                request_fingerprint: item.request_fingerprint,
                dispatch_state: PendingDispatchState::AttemptMayHaveOccurred,
            }
        })
        .collect()
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
                        retired_requests: Vec::new(),
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
                            pending: recover_legacy_pending(record.pending),
                            retired_requests: Vec::new(),
                        })
                    })
                    .collect::<Result<Vec<_>>>()?
            }
            2 => {
                let file: LegacyV2File = serde_json::from_value(raw)?;
                if file.schema_version != 2 {
                    bail!("invalid RemoteProxy v2 file");
                }
                file.records
                    .into_iter()
                    .map(|record| {
                        if record.schema_version != 2 {
                            bail!("invalid RemoteProxy v2 record");
                        }
                        Ok(RemoteProxyRecord {
                            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
                            local_session_id: record.local_session_id,
                            peer_id: record.peer_id,
                            remote_session_ref: record.remote_session_ref,
                            controlled_session_id: record.controlled_session_id,
                            ui: record.ui,
                            authenticated_cursor: record.authenticated_cursor,
                            retired_epochs: record.retired_epochs,
                            projection: record.projection,
                            pending: recover_legacy_pending(record.pending),
                            retired_requests: Vec::new(),
                        })
                    })
                    .collect::<Result<Vec<_>>>()?
            }
            3 => {
                let file: RemoteProxyFile = serde_json::from_value(raw)?;
                if file.schema_version != 3 {
                    bail!("invalid RemoteProxy v3 file");
                }
                file.records
            }
            _ => bail!("unsupported RemoteProxy schema version {version}"),
        };
        let mut state = Self {
            path,
            ..Self::default()
        };
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
        state.validate_global_request_ids()?;
        if version < 3 {
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
        self.validate_global_request_ids()?;
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

    fn validate_global_request_ids(&self) -> Result<()> {
        let mut client_ids = HashSet::new();
        let mut task_ids = HashSet::new();
        for record in self.records.values() {
            for pending in &record.pending {
                if !client_ids.insert(&pending.client_request_id)
                    || !task_ids.insert(&pending.task_id)
                {
                    bail!("remote request identifier reused across RemoteProxy records");
                }
            }
            for retired in &record.retired_requests {
                if !client_ids.insert(&retired.client_request_id)
                    || !task_ids.insert(&retired.task_id)
                {
                    bail!("remote request identifier reused across RemoteProxy records");
                }
            }
        }
        Ok(())
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
