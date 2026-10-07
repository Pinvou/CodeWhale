//! Directional transfer accounting shared by upload and download (DEV-14).
//!
//! The core only tracks opaque handles, byte budgets, chunk ordering and the
//! rolling digest. It never touches the file system and never learns a B host
//! path: DEV-17/DEV-18 stage or read the real bytes behind their own port.

use std::collections::HashMap;
use std::fmt;

use parking_lot::Mutex;
use serde::Serialize;
use sha2::{Digest, Sha256};

use super::limits::{
    MAX_FILE_NAME_CHARS, MAX_MIME_CHARS, MAX_TRANSFER_CHUNKS, MAX_TRANSFER_ID_CHARS, TransferLimits,
};
use crate::features::remote_control::controller::protocol::LeaseContext;

/// Controller handles are namespaced so a Web handle can never be replayed
/// into the controller core (and vice versa).
pub(crate) const CONTROLLER_HANDLE_PREFIX: &str = "ctr";
const UPLOAD_PREFIX: &str = "ctr-up-";
const DOWNLOAD_PREFIX: &str = "ctr-dn-";

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TransferError {
    pub(crate) code: &'static str,
}

impl TransferError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl fmt::Display for TransferError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for TransferError {}

type Result<T> = std::result::Result<T, TransferError>;

fn error(code: &'static str) -> TransferError {
    TransferError::new(code)
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum TransferDirection {
    Upload,
    Download,
}

impl TransferDirection {
    fn prefix(self) -> &'static str {
        match self {
            Self::Upload => UPLOAD_PREFIX,
            Self::Download => DOWNLOAD_PREFIX,
        }
    }

    pub(crate) fn wire(self) -> &'static str {
        match self {
            Self::Upload => "upload",
            Self::Download => "download",
        }
    }
}

/// Immutable binding of one transfer to a live authenticated context.
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TransferBinding {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) relay_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) session_id: String,
    pub(crate) task_id: String,
}

impl TransferBinding {
    pub(crate) fn from_context(
        context: &LeaseContext,
        session_id: &str,
        task_id: &str,
    ) -> Result<Self> {
        let binding = Self {
            connection_id: context.connection_id.clone(),
            controller_lease_id: context.controller_lease_id.clone(),
            lease_epoch: context.lease_epoch.clone(),
            relay_epoch: context.relay_epoch.clone(),
            remote_control_generation: context.remote_control_generation.clone(),
            session_id: session_id.to_owned(),
            task_id: task_id.to_owned(),
        };
        binding.validate()?;
        Ok(binding)
    }

    fn validate(&self) -> Result<()> {
        for value in [
            &self.connection_id,
            &self.controller_lease_id,
            &self.lease_epoch,
            &self.relay_epoch,
            &self.remote_control_generation,
            &self.session_id,
            &self.task_id,
        ] {
            if value.is_empty()
                || value.len() > MAX_TRANSFER_ID_CHARS
                || !value.bytes().all(|byte| {
                    byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b':')
                })
            {
                return Err(error("transfer_binding_invalid"));
            }
        }
        Ok(())
    }

    fn matches(&self, live: &LeaseContext) -> bool {
        self.connection_id == live.connection_id
            && self.controller_lease_id == live.controller_lease_id
            && self.lease_epoch == live.lease_epoch
            && self.relay_epoch == live.relay_epoch
            && self.remote_control_generation == live.remote_control_generation
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TransferContext {
    pub(crate) binding: TransferBinding,
    pub(crate) direction: TransferDirection,
}

impl TransferContext {
    pub(crate) fn new(
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        direction: TransferDirection,
    ) -> Result<Self> {
        Ok(Self {
            binding: TransferBinding::from_context(lease, session_id, task_id)?,
            direction,
        })
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TransferMetadata {
    pub(crate) file_name: String,
    pub(crate) mime_type: String,
    pub(crate) total_bytes: u64,
    pub(crate) sha256: String,
}

impl TransferMetadata {
    pub(crate) fn validate(&self, limits: &TransferLimits) -> Result<()> {
        let name = self.file_name.trim();
        if name.is_empty()
            || name.chars().count() > MAX_FILE_NAME_CHARS
            || name.contains(['/', '\\'])
            || name == "."
            || name == ".."
        {
            return Err(error("transfer_file_name_invalid"));
        }
        if self.mime_type.len() > MAX_MIME_CHARS
            || !self
                .mime_type
                .bytes()
                .all(|byte| byte.is_ascii_graphic() || byte == b' ')
        {
            return Err(error("transfer_mime_invalid"));
        }
        if self.total_bytes == 0 || self.total_bytes > limits.max_transfer_bytes {
            return Err(error("transfer_size_invalid"));
        }
        // Narrowed chunk limits must not admit a file that cannot finish even
        // when the sender uses the largest advertised chunks throughout.
        if limits.chunk_bytes == 0
            || self.total_bytes.div_ceil(limits.chunk_bytes as u64) > MAX_TRANSFER_CHUNKS as u64
        {
            return Err(error("transfer_chunk_budget"));
        }
        if self.sha256.len() != 64 || !is_lower_hex(&self.sha256) {
            return Err(error("transfer_digest_invalid"));
        }
        Ok(())
    }
}

/// Opaque, direction-carrying handle. It is the only capability needed to keep
/// feeding one transfer, and it is dropped when the transfer reaches a terminal
/// state.
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TransferHandle {
    id: String,
    direction: TransferDirection,
}

impl TransferHandle {
    pub(crate) fn id(&self) -> &str {
        &self.id
    }

    pub(crate) fn direction(&self) -> TransferDirection {
        self.direction
    }

    /// Rebuilds a handle from its opaque ID (DEV-17/DEV-18 contract addition).
    /// Only controller-prefixed IDs are accepted and the direction is derived
    /// from the prefix, so a caller can never relabel an upload as a download.
    pub(crate) fn from_id(value: &str) -> Option<Self> {
        if value.starts_with(UPLOAD_PREFIX) {
            Some(Self {
                id: value.to_owned(),
                direction: TransferDirection::Upload,
            })
        } else if value.starts_with(DOWNLOAD_PREFIX) {
            Some(Self {
                id: value.to_owned(),
                direction: TransferDirection::Download,
            })
        } else {
            None
        }
    }
}

pub(crate) fn is_controller_handle_id(value: &str) -> bool {
    value.starts_with(UPLOAD_PREFIX) || value.starts_with(DOWNLOAD_PREFIX)
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct CompletedTransfer {
    pub(crate) handle_id: String,
    pub(crate) context: TransferContext,
    pub(crate) metadata: TransferMetadata,
    pub(crate) sha256: String,
    pub(crate) bytes: u64,
}

/// Revocation evidence handed to the lifecycle owner before file cleanup.
/// The exact typed handle/context must survive even if staging abort fails.
#[derive(Debug, Clone)]
pub(crate) struct RevokedTransfer {
    pub(crate) handle: TransferHandle,
    pub(crate) context: TransferContext,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ChunkOutcome {
    pub(crate) received_bytes: u64,
    pub(crate) next_chunk: u64,
    /// A byte-identical retry of an already accepted chunk.
    pub(crate) duplicate: bool,
    pub(crate) completed: Option<CompletedTransfer>,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct TransferProgress {
    pub(crate) direction: TransferDirection,
    pub(crate) received_bytes: u64,
    pub(crate) total_bytes: u64,
    pub(crate) next_chunk: u64,
}

struct TransferState {
    context: TransferContext,
    metadata: TransferMetadata,
    received_bytes: u64,
    next_chunk: u64,
    // Accepted indices are contiguous: no map keys or per-digest allocations
    // are needed. At most 4096 * 32 bytes of retry evidence per transfer.
    chunk_digests: Vec<[u8; 32]>,
    hasher: Sha256,
    created_at_ms: u64,
    updated_at_ms: u64,
}

#[derive(Default)]
struct RegistryInner {
    transfers: HashMap<String, TransferState>,
    connection_bytes: HashMap<String, u64>,
    task_transfers: HashMap<String, usize>,
}

#[derive(Default)]
pub(crate) struct TransferRegistry {
    limits: TransferLimits,
    inner: Mutex<RegistryInner>,
}

impl TransferRegistry {
    pub(crate) fn new(limits: TransferLimits) -> Result<Self> {
        let limits = limits
            .narrow()
            .ok_or_else(|| error("transfer_limits_invalid"))?;
        Ok(Self {
            limits,
            inner: Mutex::new(RegistryInner::default()),
        })
    }

    pub(crate) fn limits(&self) -> TransferLimits {
        self.limits
    }

    pub(crate) fn begin(
        &self,
        context: TransferContext,
        metadata: TransferMetadata,
        now_b: u64,
    ) -> Result<(TransferHandle, TransferProgress)> {
        self.begin_with_id_source(context, metadata, now_b, uuid::Uuid::new_v4)
    }

    fn begin_with_id_source(
        &self,
        context: TransferContext,
        metadata: TransferMetadata,
        now_b: u64,
        mut next_id: impl FnMut() -> uuid::Uuid,
    ) -> Result<(TransferHandle, TransferProgress)> {
        metadata.validate(&self.limits)?;
        let mut inner = self.inner.lock();
        if inner.transfers.len() >= self.limits.max_in_flight {
            return Err(error("transfer_capacity"));
        }
        let connection = context.binding.connection_id.clone();
        let reserved = inner
            .connection_bytes
            .get(&connection)
            .copied()
            .unwrap_or(0);
        let next_total = reserved
            .checked_add(metadata.total_bytes)
            .ok_or_else(|| error("transfer_budget_overflow"))?;
        if next_total > self.limits.max_connection_bytes {
            return Err(error("transfer_connection_budget"));
        }
        let task = context.binding.task_id.clone();
        let task_count = inner.task_transfers.get(&task).copied().unwrap_or(0);
        if task_count >= self.limits.max_transfers_per_task {
            return Err(error("transfer_task_capacity"));
        }
        // A handle is a bearer capability, not a guessable process counter.
        // Keep collision admission under the same lock, bounded and before
        // any budget mutation, so an existing transfer can never be replaced.
        let handle = (0..4)
            .find_map(|_| {
                let id = format!("{}{}", context.direction.prefix(), next_id());
                (!inner.transfers.contains_key(&id)).then_some(TransferHandle {
                    id,
                    direction: context.direction,
                })
            })
            .ok_or_else(|| error("transfer_capacity"))?;
        let state = TransferState {
            context: context.clone(),
            metadata: metadata.clone(),
            received_bytes: 0,
            next_chunk: 0,
            chunk_digests: Vec::new(),
            hasher: Sha256::new(),
            created_at_ms: now_b,
            updated_at_ms: now_b,
        };
        let progress = TransferProgress {
            direction: context.direction,
            received_bytes: 0,
            total_bytes: metadata.total_bytes,
            next_chunk: 0,
        };
        inner.transfers.insert(handle.id.clone(), state);
        inner.connection_bytes.insert(connection, next_total);
        inner.task_transfers.insert(task, task_count + 1);
        Ok((handle, progress))
    }

    pub(crate) fn accept_chunk(
        &self,
        handle: &TransferHandle,
        expected_direction: TransferDirection,
        chunk_index: u64,
        bytes: &[u8],
        now_b: u64,
    ) -> Result<ChunkOutcome> {
        if bytes.is_empty() || bytes.len() > self.limits.chunk_bytes {
            return Err(error("transfer_chunk_size_invalid"));
        }
        let mut inner = self.inner.lock();
        let state = inner
            .transfers
            .get_mut(&handle.id)
            .ok_or_else(|| error("transfer_unknown"))?;
        if state.context.direction != expected_direction || handle.direction != expected_direction {
            return Err(error("transfer_direction_mismatch"));
        }
        if chunk_index < state.next_chunk {
            let chunk_digest: [u8; 32] = Sha256::digest(bytes).into();
            return match usize::try_from(chunk_index)
                .ok()
                .and_then(|index| state.chunk_digests.get(index))
            {
                Some(existing) if existing == &chunk_digest => Ok(ChunkOutcome {
                    received_bytes: state.received_bytes,
                    next_chunk: state.next_chunk,
                    duplicate: true,
                    completed: None,
                }),
                Some(_) => Err(error("transfer_chunk_conflict")),
                None => Err(error("transfer_chunk_out_of_order")),
            };
        }
        if chunk_index > state.next_chunk {
            return Err(error("transfer_chunk_out_of_order"));
        }
        // Reject before hashing, allocating or changing progress. The caller
        // owns terminal cleanup, as for other rejected chunks; retries of any
        // accepted chunk remain verifiable even at the limit.
        if state.chunk_digests.len() >= MAX_TRANSFER_CHUNKS {
            return Err(error("transfer_chunk_budget"));
        }
        let next_received = state
            .received_bytes
            .checked_add(bytes.len() as u64)
            .ok_or_else(|| error("transfer_overrun"))?;
        if next_received > state.metadata.total_bytes {
            return Err(error("transfer_overrun"));
        }
        state.hasher.update(bytes);
        state.chunk_digests.push(Sha256::digest(bytes).into());
        state.received_bytes = next_received;
        state.next_chunk = chunk_index + 1;
        state.updated_at_ms = now_b;

        if state.received_bytes < state.metadata.total_bytes {
            return Ok(ChunkOutcome {
                received_bytes: state.received_bytes,
                next_chunk: state.next_chunk,
                duplicate: false,
                completed: None,
            });
        }

        let digest = hex_lower(&state.hasher.clone().finalize());
        if digest != state.metadata.sha256 {
            let context = state.context.clone();
            let total = state.metadata.total_bytes;
            let connection = context.binding.connection_id.clone();
            let task = context.binding.task_id.clone();
            inner.transfers.remove(&handle.id);
            release_budget(&mut inner, &connection, total, &task);
            return Err(error("transfer_digest_mismatch"));
        }
        let context = state.context.clone();
        let total = state.metadata.total_bytes;
        let completed = CompletedTransfer {
            handle_id: handle.id.clone(),
            context,
            metadata: state.metadata.clone(),
            sha256: digest,
            bytes: total,
        };
        let connection = completed.context.binding.connection_id.clone();
        let task = completed.context.binding.task_id.clone();
        inner.transfers.remove(&handle.id);
        release_budget(&mut inner, &connection, total, &task);
        Ok(ChunkOutcome {
            received_bytes: total,
            next_chunk: chunk_index + 1,
            duplicate: false,
            completed: Some(completed),
        })
    }

    /// Releases one transfer. Idempotent: the second call reports `false`.
    pub(crate) fn abort(&self, handle: &TransferHandle) -> bool {
        let mut inner = self.inner.lock();
        let Some(state) = inner.transfers.remove(&handle.id) else {
            return false;
        };
        let connection = state.context.binding.connection_id.clone();
        let task = state.context.binding.task_id.clone();
        release_budget(&mut inner, &connection, state.metadata.total_bytes, &task);
        true
    }

    pub(crate) fn progress(&self, handle: &TransferHandle) -> Result<TransferProgress> {
        let inner = self.inner.lock();
        let state = inner
            .transfers
            .get(&handle.id)
            .ok_or_else(|| error("transfer_unknown"))?;
        Ok(TransferProgress {
            direction: state.context.direction,
            received_bytes: state.received_bytes,
            total_bytes: state.metadata.total_bytes,
            next_chunk: state.next_chunk,
        })
    }

    /// Rejects any handle that was created for another connection, lease epoch
    /// or generation.
    pub(crate) fn verify_binding(
        &self,
        handle: &TransferHandle,
        live: &LeaseContext,
    ) -> Result<()> {
        let inner = self.inner.lock();
        let state = inner
            .transfers
            .get(&handle.id)
            .ok_or_else(|| error("transfer_unknown"))?;
        if !state.context.binding.matches(live) {
            return Err(error("transfer_binding_stale"));
        }
        Ok(())
    }

    /// A handle is not authority by itself. The B action receiver must bind
    /// it to the authenticated lease, B session and task before chunk/abort.
    pub(crate) fn require_upload_binding(
        &self,
        handle: &TransferHandle,
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
    ) -> Result<()> {
        let inner = self.inner.lock();
        let state = inner
            .transfers
            .get(&handle.id)
            .ok_or_else(|| error("transfer_unknown"))?;
        if handle.direction != TransferDirection::Upload
            || state.context.direction != TransferDirection::Upload
            || !state.context.binding.matches(live)
            || state.context.binding.session_id != session_id
            || state.context.binding.task_id != task_id
        {
            return Err(error("transfer_binding_stale"));
        }
        Ok(())
    }

    pub(crate) fn verify_session(&self, handle: &TransferHandle, session_id: &str) -> Result<()> {
        let inner = self.inner.lock();
        let state = inner
            .transfers
            .get(handle.id())
            .ok_or_else(|| error("transfer_unknown"))?;
        if state.context.binding.session_id != session_id {
            return Err(error("transfer_binding_stale"));
        }
        Ok(())
    }

    pub(crate) fn take_expired(&self, now_b: u64) -> Vec<RevokedTransfer> {
        let ttl = self.limits.ttl.as_millis() as u64;
        self.take_where(|state| now_b.saturating_sub(state.updated_at_ms) >= ttl)
    }

    pub(crate) fn take_expired_downloads(&self, now_b: u64) -> Vec<RevokedTransfer> {
        let ttl = self.limits.ttl.as_millis() as u64;
        self.take_where(|state| {
            state.context.direction == TransferDirection::Download
                && now_b.saturating_sub(state.updated_at_ms) >= ttl
        })
    }

    pub(crate) fn take_for_task(&self, task_id: &str) -> Vec<RevokedTransfer> {
        self.take_where(|state| state.context.binding.task_id == task_id)
    }

    pub(crate) fn take_for_connection(&self, connection_id: &str) -> Vec<RevokedTransfer> {
        self.take_where(|state| state.context.binding.connection_id == connection_id)
    }

    pub(crate) fn take_for_generation(&self, generation: &str) -> Vec<RevokedTransfer> {
        self.take_where(|state| state.context.binding.remote_control_generation == generation)
    }

    pub(crate) fn take_downloads_for_task(&self, task_id: &str) -> Vec<RevokedTransfer> {
        self.take_where(|state| {
            state.context.direction == TransferDirection::Download
                && state.context.binding.task_id == task_id
        })
    }

    pub(crate) fn take_downloads_for_connection(
        &self,
        connection_id: &str,
    ) -> Vec<RevokedTransfer> {
        self.take_where(|state| {
            state.context.direction == TransferDirection::Download
                && state.context.binding.connection_id == connection_id
        })
    }

    /// Drops every transfer that exceeded its TTL.
    pub(crate) fn expire(&self, now_b: u64) -> Vec<String> {
        let ttl = self.limits.ttl.as_millis() as u64;
        let mut inner = self.inner.lock();
        let expired: Vec<String> = inner
            .transfers
            .iter()
            .filter(|(_, state)| now_b.saturating_sub(state.updated_at_ms) >= ttl)
            .map(|(id, _)| id.clone())
            .collect();
        for id in &expired {
            if let Some(state) = inner.transfers.remove(id) {
                let connection = state.context.binding.connection_id.clone();
                let task = state.context.binding.task_id.clone();
                release_budget(&mut inner, &connection, state.metadata.total_bytes, &task);
            }
        }
        expired
    }

    pub(crate) fn cleanup_for_task(&self, task_id: &str) -> usize {
        self.cleanup_where(|state| state.context.binding.task_id == task_id)
    }

    pub(crate) fn cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.cleanup_where(|state| state.context.binding.connection_id == connection_id)
    }

    pub(crate) fn cleanup_for_session(&self, session_id: &str) -> usize {
        self.cleanup_where(|state| state.context.binding.session_id == session_id)
    }

    pub(crate) fn active_count(&self) -> usize {
        self.inner.lock().transfers.len()
    }

    pub(crate) fn connection_bytes(&self, connection_id: &str) -> u64 {
        self.inner
            .lock()
            .connection_bytes
            .get(connection_id)
            .copied()
            .unwrap_or(0)
    }

    fn cleanup_where(&self, predicate: impl Fn(&TransferState) -> bool) -> usize {
        self.take_where(predicate).len()
    }

    fn take_where(&self, predicate: impl Fn(&TransferState) -> bool) -> Vec<RevokedTransfer> {
        let mut inner = self.inner.lock();
        let ids: Vec<String> = inner
            .transfers
            .iter()
            .filter(|(_, state)| predicate(state))
            .map(|(id, _)| id.clone())
            .collect();
        let mut revoked = Vec::with_capacity(ids.len());
        for id in &ids {
            if let Some(state) = inner.transfers.remove(id) {
                let connection = state.context.binding.connection_id.clone();
                let task = state.context.binding.task_id.clone();
                release_budget(&mut inner, &connection, state.metadata.total_bytes, &task);
                revoked.push(RevokedTransfer {
                    handle: TransferHandle {
                        id: id.clone(),
                        direction: state.context.direction,
                    },
                    context: state.context,
                });
            }
        }
        revoked
    }
}

fn release_budget(inner: &mut RegistryInner, connection_id: &str, bytes: u64, task_id: &str) {
    if let Some(reserved) = inner.connection_bytes.get_mut(connection_id) {
        *reserved = reserved.saturating_sub(bytes);
        if *reserved == 0 {
            inner.connection_bytes.remove(connection_id);
        }
    }
    if let Some(count) = inner.task_transfers.get_mut(task_id) {
        *count = count.saturating_sub(1);
        if *count == 0 {
            inner.task_transfers.remove(task_id);
        }
    }
}

fn is_lower_hex(value: &str) -> bool {
    value
        .bytes()
        .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn hex_lower(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(HEX[(byte >> 4) as usize] as char);
        out.push(HEX[(byte & 0x0f) as usize] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn context(generation: &str) -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: generation.into(),
        }
    }

    fn metadata(name: &str, bytes: &[u8]) -> TransferMetadata {
        TransferMetadata {
            file_name: name.into(),
            mime_type: "text/plain".into(),
            total_bytes: bytes.len() as u64,
            sha256: hex_lower(&Sha256::digest(bytes)),
        }
    }

    fn upload(
        registry: &TransferRegistry,
        task_id: &str,
        session_id: &str,
        data: &[u8],
        now_b: u64,
    ) -> TransferHandle {
        let context = TransferContext::new(
            &context("generation-0001"),
            session_id,
            task_id,
            TransferDirection::Upload,
        )
        .unwrap();
        registry
            .begin(context, metadata("notes.txt", data), now_b)
            .unwrap()
            .0
    }

    #[test]
    fn default_registry_has_valid_budgets_and_an_empty_ledger() {
        let defaults = TransferLimits::default();
        assert_eq!(defaults.narrow(), Some(defaults));
        let registry = TransferRegistry::default();
        let checked = TransferRegistry::new(defaults).unwrap();
        assert_eq!(registry.limits(), checked.limits());
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
        let inner = registry.inner.lock();
        assert!(inner.transfers.is_empty());
        assert!(inner.connection_bytes.is_empty());
        assert!(inner.task_transfers.is_empty());

        for invalid in [
            TransferLimits {
                chunk_bytes: 0,
                ..defaults
            },
            TransferLimits {
                max_transfer_bytes: defaults.max_transfer_bytes + 1,
                ..defaults
            },
        ] {
            assert_eq!(
                TransferRegistry::new(invalid).err().unwrap().code,
                "transfer_limits_invalid"
            );
        }
    }

    #[test]
    fn upload_and_download_share_one_connection_budget() {
        let registry = TransferRegistry::default();
        let lease = context("generation-0001");
        let upload_context = TransferContext::new(
            &lease,
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let download_context = TransferContext::new(
            &lease,
            "session-0001",
            "task-0001",
            TransferDirection::Download,
        )
        .unwrap();
        let big = vec![0u8; 32 * 1024 * 1024];
        registry
            .begin(upload_context.clone(), metadata("big.bin", &big), 1_000)
            .unwrap();
        let (download_handle, _) = registry
            .begin(download_context.clone(), metadata("other.bin", &big), 1_100)
            .unwrap();
        assert_eq!(
            registry.connection_bytes("connection-0001"),
            64 * 1024 * 1024
        );
        // The third transfer cannot fit into the shared connection budget even
        // though each individual file stays under the per-file limit.
        let small = vec![0u8; 1024];
        assert_eq!(
            registry
                .begin(
                    download_context.clone(),
                    metadata("small.bin", &small),
                    1_200
                )
                .unwrap_err()
                .code,
            "transfer_connection_budget"
        );
        // Aborting one direction frees room for the other direction.
        assert!(registry.abort(&download_handle));
        registry
            .begin(
                upload_context.clone(),
                metadata("second.bin", &small),
                1_400,
            )
            .unwrap();
    }

    #[test]
    fn per_file_limit_is_not_widened_by_the_connection_budget() {
        let registry = TransferRegistry::default();
        let lease = context("generation-0001");
        let context = TransferContext::new(
            &lease,
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let oversized = vec![0u8; 33 * 1024 * 1024];
        assert_eq!(
            registry
                .begin(context, metadata("huge.bin", &oversized), 1_000)
                .unwrap_err()
                .code,
            "transfer_size_invalid"
        );
        let small = vec![0u8; 1024];
        let context = TransferContext::new(
            &lease,
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        registry
            .begin(context, metadata("small.bin", &small), 1_100)
            .unwrap();
    }

    #[test]
    fn handle_collision_retries_are_bounded_and_preserve_existing_state_and_budgets() {
        let registry = TransferRegistry::default();
        let data = [7u8; 8];
        let context = TransferContext::new(
            &context("generation-0001"),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let collision_id = uuid::Uuid::from_u128(1);
        let fresh_id = uuid::Uuid::from_u128(2);
        let (existing, _) = registry
            .begin_with_id_source(context.clone(), metadata("notes.txt", &data), 1_000, || {
                collision_id
            })
            .unwrap();
        registry
            .accept_chunk(&existing, TransferDirection::Upload, 0, &[7], 1_100)
            .unwrap();
        let snapshot = || {
            let inner = registry.inner.lock();
            let state = &inner.transfers[existing.id()];
            (
                state.context.clone(),
                state.metadata.clone(),
                state.received_bytes,
                state.next_chunk,
                state.chunk_digests.clone(),
                state.hasher.clone().finalize(),
                state.created_at_ms,
                state.updated_at_ms,
            )
        };
        let before = snapshot();
        let (connection_budget, task_budget) = {
            let inner = registry.inner.lock();
            (inner.connection_bytes.clone(), inner.task_transfers.clone())
        };
        let mut attempts = 0;
        assert_eq!(
            registry
                .begin_with_id_source(context.clone(), metadata("other.txt", &data), 2_000, || {
                    attempts += 1;
                    collision_id
                })
                .unwrap_err()
                .code,
            "transfer_capacity"
        );
        assert_eq!(attempts, 4);
        assert_eq!(snapshot(), before);
        assert_eq!(registry.active_count(), 1);
        {
            let inner = registry.inner.lock();
            assert_eq!(inner.connection_bytes, connection_budget);
            assert_eq!(inner.task_transfers, task_budget);
        }
        attempts = 0;
        let (second, _) = registry
            .begin_with_id_source(context, metadata("other.txt", &data), 2_100, || {
                attempts += 1;
                if attempts < 4 { collision_id } else { fresh_id }
            })
            .unwrap();
        assert_eq!(attempts, 4);
        assert_ne!(existing.id(), second.id());
        assert_eq!(snapshot(), before);
        assert_eq!(registry.active_count(), 2);
        assert_eq!(registry.connection_bytes("connection-0001"), 16);
        assert_eq!(
            registry.inner.lock().task_transfers.get("task-0001"),
            Some(&2)
        );
        assert!(registry.abort(&existing));
        assert!(registry.abort(&second));
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
    }

    #[test]
    fn handles_use_random_v4_ids_across_registry_restarts_and_directions() {
        let mut seen = std::collections::HashSet::new();
        let data = [1u8; 8];
        for _ in 0..32 {
            let registry = TransferRegistry::default();
            for direction in [TransferDirection::Upload, TransferDirection::Download] {
                let context = TransferContext::new(
                    &context("generation-0001"),
                    "session-0001",
                    "task-0001",
                    direction,
                )
                .unwrap();
                let (handle, _) = registry
                    .begin(context, metadata("notes.txt", &data), 1_000)
                    .unwrap();
                let id = handle.id().strip_prefix(direction.prefix()).unwrap();
                let random_id = uuid::Uuid::parse_str(id).unwrap();
                assert_eq!(random_id.get_version(), Some(uuid::Version::Random));
                assert_eq!(random_id.get_variant(), uuid::Variant::RFC4122);
                assert!(seen.insert(handle.id().to_owned()));
                assert!(registry.abort(&handle));
            }
        }
    }

    #[test]
    fn handles_are_directional_and_namespaced() {
        let registry = TransferRegistry::default();
        let data = vec![7u8; 512];
        let upload_handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        assert_eq!(upload_handle.direction(), TransferDirection::Upload);
        assert!(is_controller_handle_id(upload_handle.id()));
        assert!(!upload_handle.id().starts_with("webup_"));
        assert_eq!(
            registry
                .accept_chunk(&upload_handle, TransferDirection::Download, 0, &data, 1_100)
                .unwrap_err()
                .code,
            "transfer_direction_mismatch"
        );
        // A Web-style handle can never address a controller transfer.
        let web_handle = TransferHandle {
            id: "webup_00000001".into(),
            direction: TransferDirection::Upload,
        };
        assert_eq!(
            registry
                .accept_chunk(&web_handle, TransferDirection::Upload, 0, &data, 1_200)
                .unwrap_err()
                .code,
            "transfer_unknown"
        );
    }

    #[test]
    fn narrowed_chunks_reject_impossible_transfers_before_reserving_resources() {
        let registry = TransferRegistry::new(TransferLimits {
            chunk_bytes: 1,
            ..TransferLimits::default()
        })
        .unwrap();
        let data = vec![7u8; MAX_TRANSFER_CHUNKS + 1];
        let context = TransferContext::new(
            &context("generation-0001"),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        assert_eq!(
            registry
                .begin(context, metadata("notes.txt", &data), 1_000)
                .unwrap_err()
                .code,
            "transfer_chunk_budget"
        );
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
        assert!(registry.inner.lock().task_transfers.is_empty());
        let handle = upload(
            &registry,
            "task-0001",
            "session-0001",
            &data[..MAX_TRANSFER_CHUNKS],
            1_100,
        );
        assert!(registry.abort(&handle));
    }

    #[test]
    fn one_byte_chunks_have_bounded_retry_evidence_and_rejection_preserves_state() {
        let registry = TransferRegistry::default();
        let data = vec![7u8; MAX_TRANSFER_CHUNKS + 1];
        let handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        for index in 0..MAX_TRANSFER_CHUNKS {
            registry
                .accept_chunk(
                    &handle,
                    TransferDirection::Upload,
                    index as u64,
                    &[7],
                    1_100,
                )
                .unwrap();
        }
        let progress = registry.progress(&handle).unwrap();
        let digest_before = {
            let inner = registry.inner.lock();
            let state = &inner.transfers[handle.id()];
            assert_eq!(state.chunk_digests.len(), MAX_TRANSFER_CHUNKS);
            assert!(state.chunk_digests.capacity() <= MAX_TRANSFER_CHUNKS);
            assert_eq!(std::mem::size_of_val(&state.chunk_digests[0]), 32);
            state.hasher.clone().finalize()
        };
        assert_eq!(
            registry
                .accept_chunk(
                    &handle,
                    TransferDirection::Upload,
                    MAX_TRANSFER_CHUNKS as u64,
                    &[7],
                    1_900,
                )
                .unwrap_err()
                .code,
            "transfer_chunk_budget"
        );
        assert_eq!(registry.progress(&handle).unwrap(), progress);
        assert_eq!(
            registry.connection_bytes("connection-0001"),
            data.len() as u64
        );
        {
            let inner = registry.inner.lock();
            let state = &inner.transfers[handle.id()];
            assert_eq!(state.chunk_digests.len(), MAX_TRANSFER_CHUNKS);
            assert_eq!(state.hasher.clone().finalize(), digest_before);
            assert_eq!(state.updated_at_ms, 1_100);
        }
        // The whole accepted prefix stays verifiable at capacity, not just
        // the most recent retry, and retries never extend the TTL.
        for index in [0, (MAX_TRANSFER_CHUNKS - 1) as u64] {
            assert!(
                registry
                    .accept_chunk(&handle, TransferDirection::Upload, index, &[7], 2_000)
                    .unwrap()
                    .duplicate
            );
            assert_eq!(
                registry
                    .accept_chunk(&handle, TransferDirection::Upload, index, &[8], 2_100)
                    .unwrap_err()
                    .code,
                "transfer_chunk_conflict"
            );
        }
        assert!(registry.abort(&handle));
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
        assert!(registry.inner.lock().task_transfers.is_empty());
    }

    #[test]
    fn final_chunk_at_the_chunk_budget_completes_and_releases_resources() {
        let registry = TransferRegistry::default();
        let data = vec![3u8; MAX_TRANSFER_CHUNKS];
        let handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        for index in 0..MAX_TRANSFER_CHUNKS {
            let outcome = registry
                .accept_chunk(
                    &handle,
                    TransferDirection::Upload,
                    index as u64,
                    &[3],
                    1_100,
                )
                .unwrap();
            assert_eq!(
                outcome.completed.is_some(),
                index + 1 == MAX_TRANSFER_CHUNKS
            );
        }
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
        assert!(registry.inner.lock().task_transfers.is_empty());
    }

    #[test]
    fn largest_file_completes_with_advertised_chunk_size_in_both_directions() {
        let registry = TransferRegistry::default();
        let data = vec![5u8; registry.limits().max_transfer_bytes as usize];
        let chunk_bytes = registry.limits().chunk_bytes;
        assert!(data.len().div_ceil(chunk_bytes) <= MAX_TRANSFER_CHUNKS);
        for direction in [TransferDirection::Upload, TransferDirection::Download] {
            let context = TransferContext::new(
                &context("generation-0001"),
                "session-0001",
                "task-0001",
                direction,
            )
            .unwrap();
            let (handle, _) = registry
                .begin(context, metadata("large.bin", &data), 1_000)
                .unwrap();
            for (index, chunk) in data.chunks(chunk_bytes).enumerate() {
                let outcome = registry
                    .accept_chunk(&handle, direction, index as u64, chunk, 1_100)
                    .unwrap();
                if let Some(completed) = outcome.completed {
                    assert_eq!(completed.bytes, data.len() as u64);
                    assert_eq!(completed.sha256, metadata("large.bin", &data).sha256);
                }
            }
            assert_eq!(registry.active_count(), 0);
            assert_eq!(registry.connection_bytes("connection-0001"), 0);
        }
    }

    #[test]
    fn chunks_are_ordered_deduplicated_and_digest_verified() {
        let registry = TransferRegistry::default();
        let data: Vec<u8> = (0..300u32).map(|value| value as u8).collect();
        let handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        let first = &data[..128];
        let second = &data[128..256];
        let third = &data[256..];

        assert_eq!(
            registry
                .accept_chunk(&handle, TransferDirection::Upload, 1, first, 1_100)
                .unwrap_err()
                .code,
            "transfer_chunk_out_of_order"
        );
        registry
            .accept_chunk(&handle, TransferDirection::Upload, 0, first, 1_200)
            .unwrap();
        let duplicate = registry
            .accept_chunk(&handle, TransferDirection::Upload, 0, first, 1_300)
            .unwrap();
        assert!(duplicate.duplicate);
        assert_eq!(duplicate.received_bytes, 128);
        assert_eq!(
            registry
                .accept_chunk(&handle, TransferDirection::Upload, 0, second, 1_400)
                .unwrap_err()
                .code,
            "transfer_chunk_conflict"
        );
        registry
            .accept_chunk(&handle, TransferDirection::Upload, 1, second, 1_500)
            .unwrap();
        let completed = registry
            .accept_chunk(&handle, TransferDirection::Upload, 2, third, 1_600)
            .unwrap()
            .completed
            .expect("final chunk completes the transfer");
        assert_eq!(completed.bytes, data.len() as u64);
        assert_eq!(completed.sha256, metadata("notes.txt", &data).sha256);
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
        // The completed handle is gone, so reusing it fails closed.
        assert_eq!(
            registry
                .accept_chunk(&handle, TransferDirection::Upload, 3, first, 1_700)
                .unwrap_err()
                .code,
            "transfer_unknown"
        );
    }

    #[test]
    fn digest_mismatch_fails_closed_and_releases_the_budget() {
        let registry = TransferRegistry::default();
        let data = vec![3u8; 256];
        let context = TransferContext::new(
            &context("generation-0001"),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let mut meta = metadata("notes.txt", &data);
        meta.sha256 = "0".repeat(64);
        let (handle, _) = registry.begin(context, meta, 1_000).unwrap();
        assert_eq!(
            registry
                .accept_chunk(&handle, TransferDirection::Upload, 0, &data, 1_100)
                .unwrap_err()
                .code,
            "transfer_digest_mismatch"
        );
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
    }

    #[test]
    fn cross_lease_handles_are_rejected_and_expiry_cleans_up() {
        let registry = TransferRegistry::new(TransferLimits {
            ttl: std::time::Duration::from_millis(500),
            ..TransferLimits::default()
        })
        .unwrap();
        let data = vec![1u8; 128];
        let handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        assert_eq!(
            registry
                .verify_binding(&handle, &context("generation-other"))
                .unwrap_err()
                .code,
            "transfer_binding_stale"
        );
        assert!(
            registry
                .verify_binding(&handle, &context("generation-0001"))
                .is_ok()
        );
        assert!(registry.expire(1_400).is_empty());
        assert_eq!(registry.expire(1_600), vec![handle.id().to_owned()]);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
    }

    #[test]
    fn cleanup_is_idempotent_and_scoped() {
        let registry = TransferRegistry::default();
        let data = vec![2u8; 128];
        let _first = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        let _second = upload(&registry, "task-0002", "session-0001", &data, 1_000);
        let _third = upload(&registry, "task-0003", "session-0002", &data, 1_000);
        assert_eq!(registry.cleanup_for_task("task-0001"), 1);
        assert_eq!(registry.cleanup_for_task("task-0001"), 0);
        assert_eq!(registry.active_count(), 2);
        assert_eq!(registry.cleanup_for_session("session-0001"), 1);
        assert_eq!(registry.cleanup_for_connection("connection-0001"), 1);
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.cleanup_for_connection("connection-0001"), 0);
    }

    #[test]
    fn abort_is_idempotent_and_frees_the_budget() {
        let registry = TransferRegistry::default();
        let data = vec![9u8; 4096];
        let handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        assert!(registry.abort(&handle));
        assert!(!registry.abort(&handle));
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
    }

    #[test]
    fn capacity_and_metadata_validation_fail_closed() {
        let registry = TransferRegistry::new(TransferLimits {
            max_in_flight: 1,
            ..TransferLimits::default()
        })
        .unwrap();
        let data = vec![1u8; 128];
        let _first = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        let context = TransferContext::new(
            &context("generation-0001"),
            "session-0001",
            "task-0002",
            TransferDirection::Upload,
        )
        .unwrap();
        assert_eq!(
            registry
                .begin(context.clone(), metadata("notes.txt", &data), 1_100)
                .unwrap_err()
                .code,
            "transfer_capacity"
        );

        let bad_name = TransferMetadata {
            file_name: "../escape.txt".into(),
            ..metadata("notes.txt", &data)
        };
        assert_eq!(
            registry.begin(context, bad_name, 1_200).unwrap_err().code,
            "transfer_file_name_invalid"
        );
    }

    #[test]
    fn oversized_chunks_and_overruns_are_rejected() {
        let registry = TransferRegistry::new(TransferLimits {
            chunk_bytes: 64,
            ..TransferLimits::default()
        })
        .unwrap();
        let data = vec![1u8; 128];
        let handle = upload(&registry, "task-0001", "session-0001", &data, 1_000);
        assert_eq!(
            registry
                .accept_chunk(&handle, TransferDirection::Upload, 0, &data, 1_100)
                .unwrap_err()
                .code,
            "transfer_chunk_size_invalid"
        );
        registry
            .accept_chunk(&handle, TransferDirection::Upload, 0, &data[..64], 1_200)
            .unwrap();
        registry
            .accept_chunk(&handle, TransferDirection::Upload, 1, &data[64..], 1_300)
            .unwrap();
    }
}
