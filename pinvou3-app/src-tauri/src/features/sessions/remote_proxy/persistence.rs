//! Strict current-schema loading and atomic RemoteProxy persistence.

use std::collections::HashSet;
use std::fs;
use std::io::{ErrorKind, Read};
use std::path::{Path, PathBuf};

use super::RemoteProxyState;
use super::schema::{
    MAX_PROJECTION_BYTES, MAX_REMOTE_PROXY_FILE_BYTES, MAX_REMOTE_PROXY_RECORDS,
    REMOTE_PROXY_SCHEMA_VERSION, RemoteProxyFile, RemoteProxyRecord,
};
use anyhow::{Context, Result, bail};

impl RemoteProxyState {
    /// Remote cache failure must not prevent local SessionStore startup. The
    /// strict loader remains the schema authority; a failure grants no remote
    /// identity and cannot be healed by overwriting the failed file.
    pub(crate) fn load_for_startup(path: PathBuf, sessions_dir: &Path) -> Self {
        match Self::load(path.clone(), sessions_dir) {
            Ok(state) => state,
            Err(_) => {
                eprintln!("[sessions] RemoteProxy cache unavailable; remote writes disabled");
                Self {
                    path,
                    load_failed: true,
                    ..Self::default()
                }
            }
        }
    }

    pub(super) fn require_available(&self) -> Result<()> {
        if self.load_failed {
            bail!("RemoteProxy cache is unavailable; original file preserved");
        }
        self.archives.require_present()?;
        Ok(())
    }

    pub(crate) fn load(path: PathBuf, sessions_dir: &Path) -> Result<Self> {
        let file = match fs::File::open(&path) {
            Ok(file) => file,
            Err(error) if error.kind() == ErrorKind::NotFound => {
                let state = Self {
                    archives: super::archive::ArchiveStore::at(
                        path.with_extension("archives.sqlite3"),
                    ),
                    path,
                    ..Self::default()
                };
                state.require_available()?;
                return Ok(state);
            }
            Err(error) => return Err(error).with_context(|| format!("read {}", path.display())),
        };
        let mut bytes = Vec::new();
        file.take(MAX_REMOTE_PROXY_FILE_BYTES as u64 + 1)
            .read_to_end(&mut bytes)
            .with_context(|| format!("read {}", path.display()))?;
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
            archives: super::archive::ArchiveStore::at(path.with_extension("archives.sqlite3")),
            path,
            ..Self::default()
        };
        let mut references = HashSet::new();
        state.require_available()?;
        if records.len() > MAX_REMOTE_PROXY_RECORDS {
            bail!("RemoteProxy record capacity exceeded");
        }
        for record in records {
            record.validate_stored()?;
            if state.archives.contains(&record.local_session_id)? {
                let archived = state
                    .archives
                    .read(&record.local_session_id)?
                    .ok_or_else(|| anyhow::anyhow!("remote archive unavailable"))?;
                // A committed seal may survive interrupted JSON cleanup. Only
                // an identical immutable record permits dropping that residue.
                let mut comparable = archived;
                comparable.ui.hidden = record.ui.hidden;
                if comparable != record {
                    bail!("remote archive conflicts with active residue");
                }
                continue;
            }
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
        state
            .archives
            .require_active_records(state.records.values())?;
        let active_bytes = serde_json::to_vec(&RemoteProxyFile {
            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
            records: state.records.values().cloned().collect(),
        })?;
        // A valid legacy file is not corrupt merely because new admissions
        // would need more headroom. Keep settlement/abandon/archive usable.
        state
            .persisted_required
            .set(Some(state.required_budget(&active_bytes)?));
        Ok(state)
    }

    pub(super) fn persist(&self) -> Result<()> {
        self.require_available()?;
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
        self.archives
            .require_active_records(self.records.values())?;
        records.sort_by(|left, right| left.local_session_id.cmp(&right.local_session_id));
        let bytes = serde_json::to_vec(&RemoteProxyFile {
            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
            records,
        })?;
        let required = self.required_budget(&bytes)?;
        if bytes.len() > MAX_REMOTE_PROXY_FILE_BYTES
            || (required > MAX_REMOTE_PROXY_FILE_BYTES
                && self
                    .persisted_required
                    .get()
                    .is_none_or(|old| required > old))
        {
            bail!("remote_proxy_byte_budget");
        }
        deepseek_tui::utils::write_atomic(&self.path, &bytes)
            .with_context(|| format!("persist RemoteProxy records to {}", self.path.display()))?;
        self.persisted_required.set(Some(required));
        Ok(())
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

    /// Admission reserves growth for pending settlement and a complete bounded
    /// authenticated snapshot. Tombstones are never discarded to fit the file.
    /// Persistence is compact JSON, exactly like the projection validator.
    /// Epochs allow control characters: each input byte may escape to six JSON
    /// bytes. 64*(128*6+3)+2 bounds the whole epoch array; 1KiB bounds the cursor.
    /// Subtract current serialized use so growth consumes, not duplicates,
    /// the reservation. Each pending handle has 1KiB settlement headroom.
    pub(super) fn required_budget(&self, bytes: &[u8]) -> Result<usize> {
        let mut required = bytes.len();
        for record in self
            .records
            .values()
            .filter(|record| !record.pending.is_empty())
        {
            let projection_bytes = serde_json::to_vec(&record.projection)?.len();
            let epoch_bytes = serde_json::to_vec(&record.retired_epochs)?.len();
            let cursor_bytes = serde_json::to_vec(&record.authenticated_cursor)?.len();
            required = required
                .saturating_add(MAX_PROJECTION_BYTES.saturating_sub(projection_bytes))
                .saturating_add((64 * (128 * 6 + 3) + 2_usize).saturating_sub(epoch_bytes))
                .saturating_add(1024_usize.saturating_sub(cursor_bytes));
            for pending in &record.pending {
                // Prepared→attempted and reservation fingerprint growth must
                // consume already reserved space, even at exact admission cap.
                required = required
                    .saturating_add(1024_usize.saturating_sub(serde_json::to_vec(pending)?.len()));
            }
        }
        Ok(required)
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
