//! Strict current-schema loading and atomic RemoteProxy persistence.

use std::collections::HashSet;
use std::fs;
use std::io::ErrorKind;
use std::path::{Path, PathBuf};

use super::RemoteProxyState;
use super::schema::{
    MAX_REMOTE_PROXY_FILE_BYTES, MAX_REMOTE_PROXY_RECORDS, REMOTE_PROXY_SCHEMA_VERSION,
    RemoteProxyFile, RemoteProxyRecord,
};
use anyhow::{Context, Result, bail};

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
        let file: RemoteProxyFile =
            serde_json::from_slice(&bytes).with_context(|| format!("parse {}", path.display()))?;
        if file.schema_version != REMOTE_PROXY_SCHEMA_VERSION {
            bail!(
                "unsupported RemoteProxy schema version {}",
                file.schema_version
            );
        }
        let records = file.records;
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
