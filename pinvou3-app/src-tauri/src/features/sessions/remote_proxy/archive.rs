//! Permanent read-only archive bank and crash-recoverable seal journal.
//! The independent pending intent retires authority before SQLite indexing.

use std::path::PathBuf;

use crate::platform::filesystem;
use anyhow::{Result, bail};
use parking_lot::Mutex;
use rusqlite::{Connection, OptionalExtension, params};
use serde::{Deserialize, Serialize};

use super::schema::{MAX_REMOTE_PROXY_FILE_BYTES, RemoteProxyListItem, RemoteProxyRecord};

#[derive(Default)]
pub(super) struct ArchiveStore {
    path: PathBuf,
    owner: Mutex<()>,
}

#[cfg(test)]
mod tests {
    use super::super::schema::{
        AuthenticatedProjection, REMOTE_PROXY_SCHEMA_VERSION, RemoteUiMetadata,
        RetiredRemoteRequest, RetiredRequestDisposition,
    };
    use super::*;

    fn record(suffix: &str) -> RemoteProxyRecord {
        RemoteProxyRecord {
            schema_version: REMOTE_PROXY_SCHEMA_VERSION,
            local_session_id: format!("remote-{suffix}"),
            peer_id: "peer".into(),
            remote_session_ref: format!("ref-{suffix}"),
            controlled_session_id: None,
            ui: RemoteUiMetadata::default(),
            authenticated_cursor: None,
            retired_epochs: vec![],
            projection: AuthenticatedProjection::default(),
            pending: vec![],
            retired_requests: vec![RetiredRemoteRequest {
                client_request_id: format!("client-{suffix}"),
                task_id: format!("task-{suffix}"),
                request_fingerprint: "a".repeat(64),
                disposition: RetiredRequestDisposition::Terminal,
            }],
        }
    }

    fn bank(root: &std::path::Path) -> ArchiveStore {
        ArchiveStore::at(root.join("archives.sqlite3"))
    }

    #[test]
    fn sealed_history_and_two_id_domains_are_permanent() {
        let temp = tempfile::tempdir().unwrap();
        let bank = bank(temp.path());
        let record = record("one");
        bank.seal(&record).unwrap();
        assert_eq!(
            bank.read(&record.local_session_id).unwrap(),
            Some(record.clone())
        );
        assert_eq!(bank.list(None).unwrap().len(), 1);
        bank.hide(&record.local_session_id).unwrap();
        assert!(bank.list(None).unwrap().is_empty());
        assert!(bank.contains(&record.local_session_id).unwrap());
        let mut next = record.clone();
        next.local_session_id = "remote-two".into();
        next.remote_session_ref = "ref-two".into();
        assert!(bank.require_active_records(std::iter::once(&next)).is_err());
        next.retired_requests[0].client_request_id = "task-one".into();
        next.retired_requests[0].task_id = "client-one".into();
        bank.require_active_records(std::iter::once(&next)).unwrap();
        bank.seal(&next).unwrap();
        assert!(bank.read("remote-one").unwrap().unwrap().ui.hidden);
    }

    #[test]
    fn durable_intent_recovers_before_or_after_sql_commit() {
        for committed in [false, true] {
            let temp = tempfile::tempdir().unwrap();
            let bank = bank(temp.path());
            bank.with_connection(true, |_| Ok(())).unwrap();
            let mut anchor = bank.read_anchor().unwrap().unwrap();
            let record = record("crash");
            anchor.pending = Some(record.clone());
            bank.write_anchor(&anchor).unwrap();
            if committed {
                let mut db = Connection::open(&bank.path).unwrap();
                ArchiveStore::insert_seal(&mut db, &record, &anchor).unwrap();
            }
            let restarted = ArchiveStore::at(bank.path.clone());
            restarted.require_present().unwrap();
            assert!(restarted.contains(&record.local_session_id).unwrap());
            let complete = restarted.read_anchor().unwrap().unwrap();
            assert_eq!(complete.committed_count, 1);
            assert!(complete.pending.is_none());
            assert!(
                restarted
                    .require_active_records(std::iter::once(&record))
                    .is_err()
            );
        }
    }

    #[test]
    fn same_incarnation_old_backup_and_missing_bank_fail_closed() {
        let temp = tempfile::tempdir().unwrap();
        let bank = bank(temp.path());
        bank.with_connection(true, |_| Ok(())).unwrap();
        let old = temp.path().join("old.sqlite3");
        std::fs::copy(&bank.path, &old).unwrap();
        bank.seal(&record("committed")).unwrap();
        std::fs::copy(&old, &bank.path).unwrap();
        assert!(bank.require_present().is_err());
        assert!(bank.contains("remote-committed").is_err());
        std::fs::remove_file(&bank.path).unwrap();
        assert!(bank.with_connection(true, |_| Ok(())).is_err());
        assert!(!bank.path.exists());
    }

    #[test]
    fn foreign_bank_or_missing_anchor_never_reinterprets_history_as_empty() {
        let a = tempfile::tempdir().unwrap();
        let b = tempfile::tempdir().unwrap();
        let first = bank(a.path());
        let second = bank(b.path());
        first.seal(&record("first")).unwrap();
        second.seal(&record("second")).unwrap();
        std::fs::copy(&second.path, &first.path).unwrap();
        assert!(first.require_present().is_err());
        std::fs::remove_file(second.anchor_path()).unwrap();
        assert!(second.require_present().is_err());
        assert!(second.seal(&record("third")).is_err());
    }

    #[test]
    fn committed_intent_requires_exact_record_and_complete_tombstones() {
        let temp = tempfile::tempdir().unwrap();
        let bank = bank(temp.path());
        bank.with_connection(true, |_| Ok(())).unwrap();
        let record = record("incomplete");
        let mut anchor = bank.read_anchor().unwrap().unwrap();
        anchor.pending = Some(record.clone());
        bank.write_anchor(&anchor).unwrap();
        let mut db = Connection::open(&bank.path).unwrap();
        ArchiveStore::insert_seal(&mut db, &record, &anchor).unwrap();
        db.execute("DELETE FROM retired_ids", []).unwrap();
        drop(db);
        assert!(bank.require_present().is_err());
        assert!(bank.read_anchor().unwrap().unwrap().pending.is_some());
    }

    #[test]
    fn competing_bank_owner_cannot_overwrite_seal_intent() {
        let temp = tempfile::tempdir().unwrap();
        let bank = bank(temp.path());
        bank.with_connection(true, |_| Ok(())).unwrap();
        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(bank.path.with_extension("lock"))
            .unwrap();
        let mut lock = fd_lock::RwLock::new(file);
        let _owner = lock.try_write().unwrap();
        assert!(bank.seal(&record("competing")).is_err());
        assert_eq!(bank.read_anchor().unwrap().unwrap().committed_count, 0);
    }
}

/// Independent of the active JSON byte budget. A durable pending intent is an
/// irreversible seal, even when its SQLite index transaction is interrupted.
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ArchiveAnchor {
    id: String,
    committed_count: i64,
    pending: Option<RemoteProxyRecord>,
}

impl ArchiveStore {
    pub(super) fn require_present(&self) -> Result<()> {
        self.with_connection(false, |_| Ok(()))?;
        Ok(())
    }

    pub(super) fn at(path: PathBuf) -> Self {
        Self {
            path,
            owner: Mutex::new(()),
        }
    }

    fn anchor_path(&self) -> PathBuf {
        self.path.with_extension("anchor.json")
    }

    fn read_anchor(&self) -> Result<Option<ArchiveAnchor>> {
        use std::io::Read as _;
        let path = self.anchor_path();
        if !path.try_exists()? {
            return Ok(None);
        }
        let file = filesystem::pin_plain_database_file(&path)?;
        let mut bytes = Vec::new();
        (&file)
            .take((MAX_REMOTE_PROXY_FILE_BYTES + 65536 + 1) as u64)
            .read_to_end(&mut bytes)?;
        if bytes.len() > MAX_REMOTE_PROXY_FILE_BYTES + 65536
            || !filesystem::reserved_target_is_unchanged(&file, &path)
        {
            bail!("remote_proxy_archive_unavailable");
        }
        let anchor: ArchiveAnchor = serde_json::from_slice(&bytes)?;
        if uuid::Uuid::parse_str(&anchor.id).is_err() || anchor.committed_count < 0 {
            bail!("remote_proxy_archive_unavailable");
        }
        if let Some(record) = &anchor.pending {
            record.validate_stored()?;
            if !record.pending.is_empty() {
                bail!("remote_proxy_archive_unavailable");
            }
        }
        Ok(Some(anchor))
    }

    fn write_anchor(&self, anchor: &ArchiveAnchor) -> Result<()> {
        filesystem::atomic_write_private_anchored(
            &self.anchor_path(),
            &serde_json::to_vec(anchor)?,
        )?;
        Ok(())
    }

    fn bank_state(db: &Connection) -> Result<(String, i64)> {
        let id: String =
            db.query_row("SELECT id FROM archive_meta WHERE singleton=1", [], |row| {
                row.get(0)
            })?;
        let (count, maximum): (i64, i64) = db.query_row(
            "SELECT count(*),coalesce(max(sequence),0) FROM archives",
            [],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )?;
        if count != maximum {
            bail!("remote_proxy_archive_unavailable");
        }
        Ok((id, count))
    }

    fn insert_seal(
        db: &mut Connection,
        record: &RemoteProxyRecord,
        anchor: &ArchiveAnchor,
    ) -> Result<()> {
        let tx = db.transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        if Self::bank_state(&tx)? != (anchor.id.clone(), anchor.committed_count) {
            bail!("remote_proxy_archive_unavailable");
        }
        tx.execute(
            "INSERT INTO archives(local_id,remote_ref,record,hidden) VALUES(?1,?2,?3,?4)",
            params![
                record.local_session_id,
                record.remote_session_ref,
                serde_json::to_vec(record)?,
                false
            ],
        )?;
        for retired in &record.retired_requests {
            tx.execute(
                "INSERT INTO retired_ids(client_id,task_id,local_id) VALUES(?1,?2,?3)",
                params![
                    retired.client_request_id,
                    retired.task_id,
                    record.local_session_id
                ],
            )?;
        }
        tx.commit()?;
        Ok(())
    }

    fn verify_pending(db: &Connection, anchor: &ArchiveAnchor) -> Result<()> {
        let record = anchor
            .pending
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("missing seal intent"))?;
        let (sequence, bytes): (i64, Vec<u8>) = db.query_row(
            "SELECT sequence,record FROM archives WHERE local_id=?1 AND remote_ref=?2",
            params![record.local_session_id, record.remote_session_ref],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )?;
        if sequence
            != anchor
                .committed_count
                .checked_add(1)
                .ok_or_else(|| anyhow::anyhow!("archive capacity"))?
            || bytes != serde_json::to_vec(record)?
        {
            bail!("remote_proxy_archive_unavailable");
        }
        let count: i64 = db.query_row(
            "SELECT count(*) FROM retired_ids WHERE local_id=?1",
            [&record.local_session_id],
            |row| row.get(0),
        )?;
        if count != record.retired_requests.len() as i64 {
            bail!("remote_proxy_archive_unavailable");
        }
        for retired in &record.retired_requests {
            let present = db
                .query_row(
                    "SELECT 1 FROM retired_ids WHERE client_id=?1 AND task_id=?2 AND local_id=?3",
                    params![
                        retired.client_request_id,
                        retired.task_id,
                        record.local_session_id
                    ],
                    |_| Ok(()),
                )
                .optional()?
                .is_some();
            if !present {
                bail!("remote_proxy_archive_unavailable");
            }
        }
        Ok(())
    }

    fn finish_pending(&self, db: &mut Connection, anchor: &mut ArchiveAnchor) -> Result<()> {
        let (id, count) = Self::bank_state(db)?;
        if id != anchor.id {
            bail!("remote_proxy_archive_unavailable");
        }
        if let Some(pending) = anchor.pending.as_ref() {
            if count == anchor.committed_count {
                Self::insert_seal(db, pending, anchor)?;
            } else if Some(count) != anchor.committed_count.checked_add(1) {
                bail!("remote_proxy_archive_unavailable");
            }
            Self::verify_pending(db, anchor)?;
            // Verify the CURRENT named DB before advancing the independent
            // watermark. A commit to a removed inode is never reported done.
            let current = Connection::open_with_flags(
                &self.path,
                rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
            )?;
            if Self::bank_state(&current)? != (anchor.id.clone(), anchor.committed_count + 1) {
                bail!("remote_proxy_archive_unavailable");
            }
            Self::verify_pending(&current, anchor)?;
            anchor.committed_count += 1;
            anchor.pending = None;
            self.write_anchor(anchor)?;
        } else if count != anchor.committed_count {
            bail!("remote_proxy_archive_unavailable");
        }
        let current =
            Connection::open_with_flags(&self.path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)?;
        if Self::bank_state(&current)? != (anchor.id.clone(), anchor.committed_count) {
            bail!("remote_proxy_archive_unavailable");
        }
        Ok(())
    }

    fn with_connection<T>(
        &self,
        create: bool,
        action: impl FnOnce(&mut Connection) -> Result<T>,
    ) -> Result<Option<T>> {
        let _owner = self.owner.lock();
        if self.path.as_os_str().is_empty() {
            bail!("remote_proxy_archive_unavailable");
        }
        // Cooperating processes serialize anchor+DB changes, not merely
        // SQLite writes. Never unlink or replace this ownership file.
        let lock_path = self.path.with_extension("lock");
        let lock_file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(&lock_path)?;
        let mut ownership = fd_lock::RwLock::new(lock_file);
        let _process_owner = ownership.try_write()?;
        let mut anchor = self.read_anchor()?;
        let exists = self.path.try_exists()?;
        if !exists && anchor.is_some() {
            bail!("remote_proxy_archive_unavailable");
        }
        if !exists && !create {
            return Ok(None);
        }
        if exists
            && std::fs::symlink_metadata(&self.path)?
                .file_type()
                .is_symlink()
        {
            bail!("remote_proxy_archive_unavailable");
        }
        if !exists {
            filesystem::create_private_file_once(&self.path, b"")?;
        }
        // Pin BEFORE opening SQLite. Pinning after open could validate a new
        // pathname while the SQL connection still uses an old replaced inode.
        let pinned = filesystem::pin_plain_database_file(&self.path)?;
        let mut connection = Connection::open(&self.path)?;
        connection.busy_timeout(std::time::Duration::from_secs(2))?;
        connection.execute_batch("PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;")?;
        let initial_version: u32 =
            connection.query_row("PRAGMA user_version", [], |row| row.get(0))?;
        let table_count: i64 = connection.query_row(
            "SELECT count(*) FROM sqlite_master WHERE type='table'",
            [],
            |row| row.get(0),
        )?;
        if initial_version == 0 && table_count == 0 && anchor.is_none() {
            let tx = connection.transaction()?;
            tx.execute_batch(
                    "CREATE TABLE archives (
                    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
                    local_id TEXT NOT NULL UNIQUE, remote_ref TEXT NOT NULL UNIQUE,
                    record BLOB NOT NULL, hidden INTEGER NOT NULL DEFAULT 0);
                    CREATE TABLE retired_ids (
                    client_id TEXT PRIMARY KEY NOT NULL, task_id TEXT NOT NULL UNIQUE,
                    local_id TEXT NOT NULL REFERENCES archives(local_id));
                    CREATE TABLE archive_meta(singleton INTEGER PRIMARY KEY CHECK(singleton=1),id TEXT NOT NULL);
                    PRAGMA user_version=2;",
                )?;
            tx.execute(
                "INSERT INTO archive_meta(singleton,id) VALUES(1,?1)",
                [uuid::Uuid::new_v4().to_string()],
            )?;
            tx.commit()?;
        }
        let version: u32 = connection.query_row("PRAGMA user_version", [], |row| row.get(0))?;
        if version != 2 {
            bail!("remote_proxy_archive_unavailable");
        }
        let (id, count) = Self::bank_state(&connection)?;
        if anchor.is_none() {
            let tombstones: i64 =
                connection.query_row("SELECT count(*) FROM retired_ids", [], |row| row.get(0))?;
            if count != 0 || tombstones != 0 {
                bail!("remote_proxy_archive_unavailable");
            }
            if create {
                let initial = ArchiveAnchor {
                    id,
                    committed_count: 0,
                    pending: None,
                };
                filesystem::create_private_file_once(
                    &self.anchor_path(),
                    &serde_json::to_vec(&initial)?,
                )?;
                anchor = Some(initial);
            }
        }
        if let Some(anchor) = &mut anchor {
            self.finish_pending(&mut connection, anchor)?;
        }
        let result = action(&mut connection);
        if !filesystem::reserved_target_is_unchanged(&pinned, &self.path) {
            bail!("remote_proxy_archive_unavailable");
        }
        // Includes recovery of a newly written seal intent. This completes
        // before any caller can use the normal active-record authority.
        if let Some(mut anchor) = self.read_anchor()? {
            self.finish_pending(&mut connection, &mut anchor)?;
        }
        result.map(Some)
    }

    pub(super) fn contains(&self, id: &str) -> Result<bool> {
        Ok(self
            .with_connection(false, |db| {
                Ok(db
                    .query_row("SELECT 1 FROM archives WHERE local_id=?1", [id], |_| Ok(()))
                    .optional()?
                    .is_some())
            })?
            .unwrap_or(false))
    }

    pub(super) fn read(&self, id: &str) -> Result<Option<RemoteProxyRecord>> {
        Ok(self
            .with_connection(false, |db| {
                let bytes: Option<(Vec<u8>, bool)> = db.query_row(
                "SELECT record,hidden FROM archives WHERE local_id=?1 AND length(record)<=?2",
                params![id, MAX_REMOTE_PROXY_FILE_BYTES as i64], |row| Ok((row.get(0)?, row.get(1)?))
            ).optional()?;
                let Some((bytes, hidden)) = bytes else {
                    return Ok(None);
                };
                let mut record: RemoteProxyRecord = serde_json::from_slice(&bytes)?;
                record.validate_stored()?;
                if record.local_session_id != id || !record.pending.is_empty() {
                    bail!("invalid remote archive");
                }
                record.ui.hidden = hidden;
                Ok(Some(record))
            })?
            .flatten())
    }

    pub(super) fn seal(&self, record: &RemoteProxyRecord) -> Result<()> {
        record.validate_stored()?;
        if !record.pending.is_empty() {
            bail!("remote_proxy_archive_pending");
        }
        let bytes = serde_json::to_vec(record)?;
        if bytes.len() > MAX_REMOTE_PROXY_FILE_BYTES {
            bail!("remote_proxy_byte_budget");
        }
        self.with_connection(true, |_| {
            let mut anchor = self
                .read_anchor()?
                .ok_or_else(|| anyhow::anyhow!("missing archive anchor"))?;
            anchor.pending = Some(record.clone());
            self.write_anchor(&anchor)?;
            Ok(())
        })?;
        Ok(())
    }

    pub(super) fn require_active_records<'a>(
        &self,
        records: impl Iterator<Item = &'a RemoteProxyRecord>,
    ) -> Result<()> {
        self.with_connection(false, |db| {
            let mut identities = db.prepare_cached(
                "SELECT 1 FROM archives WHERE local_id=?1 OR remote_ref=?2 LIMIT 1",
            )?;
            let mut request_ids = db.prepare_cached(
                "SELECT 1 FROM retired_ids WHERE client_id=?1 OR task_id=?2 LIMIT 1",
            )?;
            for record in records {
                if identities
                    .query_row(
                        params![record.local_session_id, record.remote_session_ref],
                        |_| Ok(()),
                    )
                    .optional()?
                    .is_some()
                {
                    bail!("remote_proxy_archived");
                }
                for (client, task) in record
                    .pending
                    .iter()
                    .map(|r| (&r.client_request_id, &r.task_id))
                    .chain(
                        record
                            .retired_requests
                            .iter()
                            .map(|r| (&r.client_request_id, &r.task_id)),
                    )
                {
                    if request_ids
                        .query_row(params![client, task], |_| Ok(()))
                        .optional()?
                        .is_some()
                    {
                        bail!("retired remote request identifier cannot be reused");
                    }
                }
            }
            Ok(())
        })?;
        Ok(())
    }

    pub(super) fn hide(&self, id: &str) -> Result<()> {
        let changed = self
            .with_connection(false, |db| {
                Ok(db.execute("UPDATE archives SET hidden=1 WHERE local_id=?1", [id])?)
            })?
            .unwrap_or(0);
        if changed == 0 {
            bail!("remote_proxy_not_found");
        }
        Ok(())
    }

    /// Only one bounded page is materialized, never archive transcript bodies.
    pub(super) fn list(&self, before: Option<i64>) -> Result<Vec<(i64, RemoteProxyListItem)>> {
        Ok(self
            .with_connection(false, |db| {
                let mut statement = db.prepare(
                    "SELECT sequence,local_id,
                json_extract(CAST(record AS TEXT),'$.ui'),
                json_array_length(CAST(record AS TEXT),'$.projection.entries')
                FROM archives WHERE hidden=0 AND sequence<?1 ORDER BY sequence DESC LIMIT 256",
                )?;
                let rows = statement.query_map([before.unwrap_or(i64::MAX)], |row| {
                    Ok((
                        row.get::<_, i64>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, String>(2)?,
                        row.get::<_, u32>(3)?,
                    ))
                })?;
                let mut list = Vec::new();
                for row in rows {
                    let (cursor, id, ui, count) = row?;
                    list.push((
                        cursor,
                        RemoteProxyListItem {
                            local_session_id: id,
                            ui: serde_json::from_str(&ui)?,
                            projected_entry_count: count as usize,
                        },
                    ));
                }
                Ok(list)
            })?
            .unwrap_or_default())
    }
}
