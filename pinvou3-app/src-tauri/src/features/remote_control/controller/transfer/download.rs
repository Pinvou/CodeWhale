//! B→A artifact download extension slot (implemented by DEV-18).
//!
//! The index exposes opaque artifact IDs only: A never learns a B path, and a
//! download can only start from an ID that is currently indexed for the exact
//! session and lease. A missing or deleted artifact fails closed instead of
//! falling back to a basename or path lookup.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use parking_lot::Mutex;
use serde::Serialize;
use sha2::Digest as _;

use super::core::{
    CompletedTransfer, TransferContext, TransferDirection, TransferMetadata, TransferRegistry,
};
use super::limits::MAX_TRANSFER_CHUNK_BYTES;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::TaskLeaseBinding;

const MAX_ARTIFACTS: usize = 256;
const MAX_ARTIFACT_ID_CHARS: usize = 128;
const MAX_DISPLAY_CHARS: usize = 160;
const MAX_MIME_CHARS: usize = 120;
const MAX_SESSION_ID_CHARS: usize = 128;
const PREVIEW_MAX_BYTES: u64 = 64 * 1024;

pub(crate) trait DownloadSource: Send + Sync {
    fn total_bytes(&self) -> u64;

    /// Reads one chunk. `Ok(None)` means the source is shorter than advertised,
    /// which the caller must treat as a failed download.
    fn read_chunk(
        &self,
        chunk_index: u64,
        max_bytes: usize,
    ) -> std::result::Result<Option<Vec<u8>>, String>;
}

/// Number of chunks for a bounded payload.
pub(crate) fn chunk_count(total_bytes: u64, chunk_bytes: usize) -> u64 {
    let chunk_bytes = chunk_bytes.min(MAX_TRANSFER_CHUNK_BYTES).max(1) as u64;
    total_bytes.div_ceil(chunk_bytes)
}

/// Byte offset of one chunk index.
pub(crate) fn chunk_offset(chunk_index: u64, chunk_bytes: usize) -> u64 {
    let chunk_bytes = chunk_bytes.min(MAX_TRANSFER_CHUNK_BYTES).max(1) as u64;
    chunk_index.saturating_mul(chunk_bytes)
}

/// Fully in-memory source used by tests and by small generated artifacts.
pub(crate) struct MemoryArtifactSource {
    bytes: Vec<u8>,
}

impl MemoryArtifactSource {
    pub(crate) fn new(bytes: Vec<u8>) -> Self {
        Self { bytes }
    }
}

impl DownloadSource for MemoryArtifactSource {
    fn total_bytes(&self) -> u64 {
        self.bytes.len() as u64
    }

    fn read_chunk(
        &self,
        chunk_index: u64,
        max_bytes: usize,
    ) -> std::result::Result<Option<Vec<u8>>, String> {
        let max_bytes = max_bytes.min(MAX_TRANSFER_CHUNK_BYTES).max(1);
        let start = chunk_offset(chunk_index, max_bytes);
        if start >= self.bytes.len() as u64 {
            return Ok(None);
        }
        let end = (start as usize)
            .saturating_add(max_bytes)
            .min(self.bytes.len());
        Ok(Some(self.bytes[start as usize..end].to_vec()))
    }
}

/// File-backed source. The path lives only inside this struct and is never
/// projected to A; the index exposes the opaque ID instead.
pub(crate) struct FileArtifactSource {
    path: PathBuf,
}

impl FileArtifactSource {
    pub(crate) fn new(path: PathBuf) -> Self {
        Self { path }
    }
}

impl DownloadSource for FileArtifactSource {
    fn total_bytes(&self) -> u64 {
        std::fs::metadata(&self.path)
            .map(|meta| meta.len())
            .unwrap_or(0)
    }

    fn read_chunk(
        &self,
        chunk_index: u64,
        max_bytes: usize,
    ) -> std::result::Result<Option<Vec<u8>>, String> {
        use std::io::{Read, Seek, SeekFrom};
        let max_bytes = max_bytes.min(MAX_TRANSFER_CHUNK_BYTES).max(1);
        let mut file =
            std::fs::File::open(&self.path).map_err(|_| "artifact_unavailable".to_owned())?;
        let offset = chunk_offset(chunk_index, max_bytes);
        let total = file
            .metadata()
            .map_err(|_| "artifact_unavailable".to_owned())?
            .len();
        if offset >= total {
            return Ok(None);
        }
        file.seek(SeekFrom::Start(offset))
            .map_err(|_| "artifact_unavailable".to_owned())?;
        let expected = (total - offset).min(max_bytes as u64) as usize;
        let mut buffer = vec![0u8; expected];
        file.read_exact(&mut buffer)
            .map_err(|_| "artifact_unavailable".to_owned())?;
        Ok(Some(buffer))
    }
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum PreviewKind {
    /// Small textual artifact that fits inline.
    Text,
    /// Supported but too large; A must download a copy.
    DownloadOnly,
    /// No inline preview is supported for this type.
    Unsupported,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ArtifactDescriptor {
    pub(crate) artifact_id: String,
    pub(crate) task_id: String,
    pub(crate) display_name: String,
    pub(crate) mime_type: String,
    pub(crate) total_bytes: u64,
    pub(crate) sha256: String,
    pub(crate) preview_kind: PreviewKind,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ArtifactPreview {
    pub(crate) artifact_id: String,
    pub(crate) preview_kind: PreviewKind,
    pub(crate) truncated: bool,
    pub(crate) text: Option<String>,
}

struct IndexedArtifact {
    task_sequence: u64,
    session_id: String,
    lease: TaskLeaseBinding,
    descriptor: ArtifactDescriptor,
    source: Arc<dyn DownloadSource>,
    published: bool,
}

/// Session-scoped artifact index. IDs are opaque handles minted by B; the
/// registry never exposes a path or a basename-derived lookup.
#[derive(Default)]
pub(crate) struct ArtifactIndex {
    entries: Mutex<HashMap<String, IndexedArtifact>>,
}

impl ArtifactIndex {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn register_owned(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        display_name: &str,
        mime_type: &str,
        sha256: &str,
        source: Arc<dyn DownloadSource>,
        sequence: u64,
        published: bool,
    ) -> Result<ArtifactDescriptor, String> {
        self.register_with_visibility(
            lease,
            session_id,
            task_id,
            artifact_id,
            display_name,
            mime_type,
            sha256,
            source,
            published,
            sequence,
        )
    }

    pub(crate) fn require_admission(&self, artifact_id: &str, sequence: u64) -> Result<(), String> {
        let entries = self.entries.lock();
        if entries
            .get(artifact_id)
            .is_some_and(|entry| entry.task_sequence == sequence)
        {
            Ok(())
        } else {
            Err("artifact_binding_stale".into())
        }
    }

    #[cfg(test)]
    pub(crate) fn register(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        display_name: &str,
        mime_type: &str,
        sha256: &str,
        source: Arc<dyn DownloadSource>,
    ) -> std::result::Result<ArtifactDescriptor, String> {
        self.register_with_visibility(
            lease,
            session_id,
            task_id,
            artifact_id,
            display_name,
            mime_type,
            sha256,
            source,
            true,
            0,
        )
    }

    /// Reserve a validated index entry without exposing it to list, preview,
    /// or download operations. The task terminal CAS later publishes every
    /// staged entry for the exact binding atomically.
    #[cfg(test)]
    pub(crate) fn register_pending(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        display_name: &str,
        mime_type: &str,
        sha256: &str,
        source: Arc<dyn DownloadSource>,
    ) -> std::result::Result<ArtifactDescriptor, String> {
        self.register_with_visibility(
            lease,
            session_id,
            task_id,
            artifact_id,
            display_name,
            mime_type,
            sha256,
            source,
            false,
            0,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn register_with_visibility(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        display_name: &str,
        mime_type: &str,
        sha256: &str,
        source: Arc<dyn DownloadSource>,
        published: bool,
        task_sequence: u64,
    ) -> std::result::Result<ArtifactDescriptor, String> {
        if session_id.is_empty()
            || session_id.len() > MAX_SESSION_ID_CHARS
            || !session_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err("artifact_session_invalid".to_owned());
        }
        if task_id.is_empty()
            || task_id.len() > MAX_ARTIFACT_ID_CHARS
            || !task_id.bytes().all(|byte| {
                byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b':')
            })
        {
            return Err("artifact_task_invalid".to_owned());
        }
        if artifact_id.is_empty()
            || artifact_id.len() > MAX_ARTIFACT_ID_CHARS
            || !artifact_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err("artifact_id_invalid".to_owned());
        }
        let display_name: String = display_name
            .chars()
            .filter(|ch| !matches!(ch, '/' | '\\') && !ch.is_control())
            .take(MAX_DISPLAY_CHARS)
            .collect();
        if display_name.trim().is_empty() || matches!(display_name.trim(), "." | "..") {
            return Err("artifact_name_invalid".to_owned());
        }
        if mime_type.len() > MAX_MIME_CHARS
            || !mime_type
                .bytes()
                .all(|byte| byte.is_ascii_graphic() || byte == b' ')
        {
            return Err("artifact_mime_invalid".to_owned());
        }
        if sha256.len() != 64 || !sha256.bytes().all(|byte| byte.is_ascii_hexdigit()) {
            return Err("artifact_digest_invalid".to_owned());
        }
        let total_bytes = source.total_bytes();
        if total_bytes == 0 {
            return Err("artifact_unavailable".to_owned());
        }
        let mut entries = self.entries.lock();
        if entries.contains_key(artifact_id) {
            return Err("artifact_id_conflict".to_owned());
        }
        if entries.len() >= MAX_ARTIFACTS {
            return Err("artifact_capacity".to_owned());
        }
        let preview_kind = preview_kind_for(mime_type, total_bytes);
        let descriptor = ArtifactDescriptor {
            artifact_id: artifact_id.to_owned(),
            task_id: task_id.to_owned(),
            display_name,
            mime_type: mime_type.to_owned(),
            total_bytes,
            sha256: sha256.to_ascii_lowercase(),
            preview_kind,
        };
        entries.insert(
            artifact_id.to_owned(),
            IndexedArtifact {
                task_sequence,
                session_id: session_id.to_owned(),
                lease: TaskLeaseBinding::from_context(lease),
                descriptor: descriptor.clone(),
                source,
                published,
            },
        );
        Ok(descriptor)
    }

    /// Publish all staged entries for one successful task as a single index
    /// transition. Returning descriptors is idempotent only for the first
    /// transition; callers use that property to emit artifact journal events
    /// exactly once.
    #[cfg(test)]
    pub(crate) fn publish_task(
        &self,
        session_id: &str,
        task_id: &str,
        lease: &LeaseContext,
    ) -> Vec<ArtifactDescriptor> {
        self.publish_admission(session_id, task_id, lease, 0)
    }

    pub(crate) fn publish_admission(
        &self,
        session_id: &str,
        task_id: &str,
        lease: &LeaseContext,
        sequence: u64,
    ) -> Vec<ArtifactDescriptor> {
        let binding = TaskLeaseBinding::from_context(lease);
        let mut entries = self.entries.lock();
        let mut published = Vec::new();
        for entry in entries.values_mut().filter(|entry| {
            entry.session_id == session_id
                && entry.task_sequence == sequence
                && entry.descriptor.task_id == task_id
                && entry.lease == binding
                && !entry.published
        }) {
            entry.published = true;
            published.push(entry.descriptor.clone());
        }
        published.sort_by(|left, right| left.artifact_id.cmp(&right.artifact_id));
        published
    }

    pub(crate) fn list_for_binding(
        &self,
        session_id: &str,
        lease: &LeaseContext,
    ) -> Vec<ArtifactDescriptor> {
        let binding = TaskLeaseBinding::from_context(lease);
        let entries = self.entries.lock();
        let mut descriptors: Vec<_> = entries
            .values()
            .filter(|entry| {
                entry.session_id == session_id
                    && entry.lease == binding
                    && entry.published
                    && entry.source.total_bytes() == entry.descriptor.total_bytes
            })
            .map(|entry| entry.descriptor.clone())
            .collect();
        descriptors.sort_by(|left, right| left.artifact_id.cmp(&right.artifact_id));
        descriptors
    }

    pub(crate) fn require_artifact_task(
        &self,
        session_id: &str,
        artifact_id: &str,
        task_id: &str,
        lease: &LeaseContext,
    ) -> Result<(), String> {
        self.resolve_for_task(session_id, artifact_id, task_id, lease)
            .map(|_| ())
    }

    pub(crate) fn descriptor_for_binding(
        &self,
        session_id: &str,
        artifact_id: &str,
        lease: &LeaseContext,
    ) -> Result<ArtifactDescriptor, String> {
        let entries = self.entries.lock();
        entries
            .get(artifact_id)
            .filter(|entry| {
                entry.session_id == session_id
                    && entry.lease == TaskLeaseBinding::from_context(lease)
                    && entry.published
            })
            .map(|entry| entry.descriptor.clone())
            .ok_or_else(|| "artifact_not_found".to_owned())
    }

    pub(crate) fn preview_for_binding(
        &self,
        session_id: &str,
        artifact_id: &str,
        lease: &LeaseContext,
    ) -> Result<ArtifactPreview, String> {
        self.preview_checked(session_id, artifact_id, Some(lease))
    }

    pub(crate) fn preview_for_task_binding(
        &self,
        session_id: &str,
        artifact_id: &str,
        task_id: &str,
        lease: &LeaseContext,
    ) -> Result<ArtifactPreview, String> {
        let entries = self.entries.lock();
        let entry = entries
            .get(artifact_id)
            .filter(|entry| {
                entry.session_id == session_id
                    && entry.descriptor.task_id == task_id
                    && entry.lease == TaskLeaseBinding::from_context(lease)
                    && entry.published
            })
            .ok_or_else(|| "artifact_not_found".to_owned())?;
        preview_entry(artifact_id, entry)
    }

    pub(crate) fn list(&self, session_id: &str) -> Vec<ArtifactDescriptor> {
        let entries = self.entries.lock();
        let mut descriptors: Vec<ArtifactDescriptor> = entries
            .values()
            .filter(|entry| entry.session_id == session_id && entry.published)
            .map(|entry| entry.descriptor.clone())
            .collect();
        descriptors.sort_by(|left, right| left.artifact_id.cmp(&right.artifact_id));
        descriptors
    }

    pub(crate) fn descriptor(
        &self,
        session_id: &str,
        artifact_id: &str,
    ) -> Result<ArtifactDescriptor, String> {
        let entries = self.entries.lock();
        let entry = entries
            .get(artifact_id)
            .filter(|entry| entry.session_id == session_id && entry.published)
            .ok_or_else(|| "artifact_not_found".to_owned())?;
        Ok(entry.descriptor.clone())
    }

    pub(crate) fn source(
        &self,
        session_id: &str,
        artifact_id: &str,
    ) -> Result<Arc<dyn DownloadSource>, String> {
        let entries = self.entries.lock();
        entries
            .get(artifact_id)
            .filter(|entry| entry.session_id == session_id && entry.published)
            .map(|entry| Arc::clone(&entry.source))
            .ok_or_else(|| "artifact_not_found".to_owned())
    }

    pub(crate) fn preview(
        &self,
        session_id: &str,
        artifact_id: &str,
    ) -> Result<ArtifactPreview, String> {
        self.preview_checked(session_id, artifact_id, None)
    }

    fn preview_checked(
        &self,
        session_id: &str,
        artifact_id: &str,
        lease: Option<&LeaseContext>,
    ) -> Result<ArtifactPreview, String> {
        let entries = self.entries.lock();
        let entry = entries
            .get(artifact_id)
            .filter(|entry| {
                entry.session_id == session_id
                    && entry.published
                    && lease
                        .is_none_or(|lease| entry.lease == TaskLeaseBinding::from_context(lease))
            })
            .ok_or_else(|| "artifact_not_found".to_owned())?;
        preview_entry(artifact_id, entry)
    }

    pub(crate) fn remove(&self, artifact_id: &str) -> bool {
        self.entries.lock().remove(artifact_id).is_some()
    }

    pub(crate) fn clear_session(&self, session_id: &str) -> usize {
        let mut entries = self.entries.lock();
        let ids: Vec<String> = entries
            .iter()
            .filter(|(_, entry)| entry.session_id == session_id)
            .map(|(id, _)| id.clone())
            .collect();
        for id in &ids {
            entries.remove(id);
        }
        ids.len()
    }

    pub(crate) fn clear_connection(&self, connection_id: &str) -> usize {
        let mut entries = self.entries.lock();
        let before = entries.len();
        entries.retain(|_, entry| entry.lease.connection_id != connection_id);
        before - entries.len()
    }

    pub(crate) fn clear_admission(&self, sequence: u64) -> usize {
        let mut entries = self.entries.lock();
        let before = entries.len();
        entries.retain(|_, entry| entry.task_sequence != sequence);
        before - entries.len()
    }

    #[cfg(test)]
    pub(crate) fn clear_task(
        &self,
        session_id: &str,
        task_id: &str,
        lease: &LeaseContext,
    ) -> usize {
        let mut entries = self.entries.lock();
        let before = entries.len();
        let binding = TaskLeaseBinding::from_context(lease);
        entries.retain(|_, entry| {
            entry.session_id != session_id
                || entry.descriptor.task_id != task_id
                || entry.lease != binding
        });
        before - entries.len()
    }

    pub(crate) fn len(&self) -> usize {
        self.entries.lock().len()
    }

    fn resolve_for_task(
        &self,
        session_id: &str,
        artifact_id: &str,
        task_id: &str,
        lease: &LeaseContext,
    ) -> Result<(ArtifactDescriptor, Arc<dyn DownloadSource>), String> {
        let entries = self.entries.lock();
        let entry = entries
            .get(artifact_id)
            .filter(|entry| {
                entry.session_id == session_id
                    && entry.descriptor.task_id == task_id
                    && entry.lease == TaskLeaseBinding::from_context(lease)
                    && entry.published
            })
            .ok_or_else(|| "artifact_not_found".to_owned())?;
        Ok((entry.descriptor.clone(), Arc::clone(&entry.source)))
    }
}

fn preview_entry(artifact_id: &str, entry: &IndexedArtifact) -> Result<ArtifactPreview, String> {
    match entry.descriptor.preview_kind {
        PreviewKind::Text => {
            let chunk = entry
                .source
                .read_chunk(0, PREVIEW_MAX_BYTES as usize)
                .map_err(|_| "artifact_unavailable".to_owned())?
                .ok_or_else(|| "artifact_unavailable".to_owned())?;
            if chunk.len() as u64 != entry.descriptor.total_bytes
                || crate::platform::encoding::hex_lower(&sha2::Sha256::digest(&chunk))
                    != entry.descriptor.sha256
            {
                return Err("artifact_unavailable".to_owned());
            }
            Ok(ArtifactPreview {
                artifact_id: artifact_id.to_owned(),
                preview_kind: PreviewKind::Text,
                truncated: false,
                text: Some(String::from_utf8_lossy(&chunk).into_owned()),
            })
        }
        kind => Ok(ArtifactPreview {
            artifact_id: artifact_id.to_owned(),
            preview_kind: kind,
            truncated: false,
            text: None,
        }),
    }
}

fn preview_kind_for(mime_type: &str, total_bytes: u64) -> PreviewKind {
    let textual = mime_type.starts_with("text/")
        || matches!(
            mime_type,
            "application/json"
                | "application/xml"
                | "application/x-yaml"
                | "application/yaml"
                | "application/toml"
        );
    if !textual {
        return PreviewKind::Unsupported;
    }
    if total_bytes > PREVIEW_MAX_BYTES {
        PreviewKind::DownloadOnly
    } else {
        PreviewKind::Text
    }
}

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ArtifactDownloadBegin {
    pub(crate) handle_id: String,
    pub(crate) chunk_bytes: usize,
    pub(crate) total_bytes: u64,
}

/// Owner for B→A artifact downloads: index plus the shared transfer core.
pub(crate) struct ArtifactDownloads {
    registry: Arc<TransferRegistry>,
    index: Arc<ArtifactIndex>,
    active: Mutex<HashMap<String, DownloadBinding>>,
}

#[derive(Clone)]
struct DownloadBinding {
    task_sequence: u64,
    lease: TaskLeaseBinding,
    session_id: String,
    task_id: String,
    artifact_id: String,
}

impl ArtifactDownloads {
    pub(crate) fn new(registry: Arc<TransferRegistry>, index: Arc<ArtifactIndex>) -> Self {
        Self {
            registry,
            index,
            active: Mutex::new(HashMap::new()),
        }
    }

    pub(crate) fn index(&self) -> &Arc<ArtifactIndex> {
        &self.index
    }

    pub(crate) fn begin(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        now_b: u64,
    ) -> Result<ArtifactDownloadBegin, String> {
        let (descriptor, source) =
            self.index
                .resolve_for_task(session_id, artifact_id, task_id, lease)?;
        let task_sequence = self
            .index
            .entries
            .lock()
            .get(artifact_id)
            .map(|entry| entry.task_sequence)
            .ok_or("artifact_unavailable")?;
        // A deleted/raced artifact must fail closed rather than serve a stale
        // length or a truncated file.
        if source.total_bytes() != descriptor.total_bytes {
            self.index.remove(artifact_id);
            return Err("artifact_unavailable".to_owned());
        }
        let context = TransferContext::new(lease, session_id, task_id, TransferDirection::Download)
            .map_err(|error| error.code.to_owned())?;
        let (handle, progress) = self
            .registry
            .begin(
                context,
                TransferMetadata {
                    file_name: descriptor.display_name.clone(),
                    mime_type: descriptor.mime_type.clone(),
                    total_bytes: descriptor.total_bytes,
                    sha256: descriptor.sha256.clone(),
                },
                now_b,
            )
            .map_err(|error| error.code.to_owned())?;
        self.active.lock().insert(
            handle.id().to_owned(),
            DownloadBinding {
                task_sequence,
                lease: TaskLeaseBinding::from_context(lease),
                session_id: session_id.to_owned(),
                task_id: task_id.to_owned(),
                artifact_id: artifact_id.to_owned(),
            },
        );
        Ok(ArtifactDownloadBegin {
            handle_id: handle.id().to_owned(),
            chunk_bytes: self.registry.limits().chunk_bytes,
            total_bytes: progress.total_bytes,
        })
    }

    pub(crate) fn next_chunk_bound(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        handle_id: &str,
        chunk_index: u64,
        now_b: u64,
    ) -> Result<Option<DownloadChunk>, String> {
        let binding = self
            .active
            .lock()
            .get(handle_id)
            .cloned()
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        if binding.lease != TaskLeaseBinding::from_context(lease)
            || binding.session_id != session_id
            || binding.task_id != task_id
            || binding.artifact_id != artifact_id
        {
            return Err("transfer_binding_stale".to_owned());
        }
        self.index
            .require_admission(artifact_id, binding.task_sequence)?;
        let source = match self
            .index
            .resolve_for_task(session_id, artifact_id, task_id, lease)
        {
            Ok((_, source)) => source,
            Err(error) => {
                self.active.lock().remove(handle_id);
                self.abort(handle_id);
                return Err(error);
            }
        };
        let result = self.next_chunk_from_source(source, handle_id, chunk_index, now_b);
        if result.as_ref().is_err()
            || result
                .as_ref()
                .ok()
                .and_then(|chunk| chunk.as_ref())
                .is_some_and(|chunk| chunk.completed.is_some())
        {
            self.active.lock().remove(handle_id);
        }
        result
    }

    pub(crate) fn abort_bound(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        handle_id: &str,
    ) -> Result<bool, String> {
        let mut active = self.active.lock();
        let binding = active
            .get(handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        if binding.lease != TaskLeaseBinding::from_context(lease)
            || binding.session_id != session_id
            || binding.task_id != task_id
            || binding.artifact_id != artifact_id
        {
            return Err("transfer_binding_stale".to_owned());
        }
        active.remove(handle_id);
        Ok(self.abort(handle_id))
    }

    pub(crate) fn next_chunk(
        &self,
        session_id: &str,
        artifact_id: &str,
        handle_id: &str,
        chunk_index: u64,
        now_b: u64,
    ) -> Result<Option<DownloadChunk>, String> {
        let source = self.index.source(session_id, artifact_id)?;
        self.next_chunk_from_source(source, handle_id, chunk_index, now_b)
    }

    fn next_chunk_from_source(
        &self,
        source: Arc<dyn DownloadSource>,
        handle_id: &str,
        chunk_index: u64,
        now_b: u64,
    ) -> Result<Option<DownloadChunk>, String> {
        let handle = super::core::TransferHandle::from_id(handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        if handle.direction() != TransferDirection::Download {
            return Err("transfer_direction_mismatch".into());
        }
        let chunk_bytes = self.registry.limits().chunk_bytes;
        let bytes = match source.read_chunk(chunk_index, chunk_bytes) {
            Ok(Some(bytes)) => bytes,
            Ok(None) | Err(_) => {
                self.registry.abort(&handle);
                return Err("artifact_unavailable".to_owned());
            }
        };
        match self.registry.accept_chunk(
            &handle,
            TransferDirection::Download,
            chunk_index,
            &bytes,
            now_b,
        ) {
            Ok(outcome) => Ok(Some(DownloadChunk {
                chunk_index,
                bytes,
                received_bytes: outcome.received_bytes,
                next_chunk: outcome.next_chunk,
                completed: outcome.completed,
            })),
            Err(error) => {
                self.registry.abort(&handle);
                Err(error.code.to_owned())
            }
        }
    }

    pub(crate) fn abort(&self, handle_id: &str) -> bool {
        let Ok(handle) = super::core::TransferHandle::from_id(handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())
        else {
            return false;
        };
        handle.direction() == TransferDirection::Download && self.registry.abort(&handle)
    }

    pub(crate) fn verify_binding(
        &self,
        handle_id: &str,
        live: &LeaseContext,
    ) -> Result<(), String> {
        let handle = super::core::TransferHandle::from_id(handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        if handle.direction() != TransferDirection::Download {
            return Err("transfer_direction_mismatch".into());
        }
        self.registry
            .verify_binding(&handle, live)
            .map_err(|error| error.code.to_owned())
    }

    pub(crate) fn cleanup_for_task(&self, task_id: &str) -> usize {
        self.active
            .lock()
            .retain(|_, binding| binding.task_id != task_id);
        self.registry.take_downloads_for_task(task_id).len()
    }

    pub(crate) fn cleanup_for_admission(&self, sequence: u64) -> usize {
        let ids: Vec<_> = self
            .active
            .lock()
            .iter()
            .filter(|(_, binding)| binding.task_sequence == sequence)
            .map(|(id, _)| id.clone())
            .collect();
        let count = ids.len();
        for id in ids {
            self.active.lock().remove(&id);
            self.abort(&id);
        }
        count
    }

    pub(crate) fn cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.active
            .lock()
            .retain(|_, binding| binding.lease.connection_id != connection_id);
        let cleaned = self
            .registry
            .take_downloads_for_connection(connection_id)
            .len();
        self.index.clear_connection(connection_id);
        cleaned
    }

    /// DEV-18 审核修复：未完成下载同样受 TTL 约束（RC-F-003 的下载侧）。
    pub(crate) fn expire(&self, now_b: u64) -> usize {
        let expired = self.registry.take_expired_downloads(now_b);
        self.active.lock().retain(|id, _| {
            super::core::TransferHandle::from_id(id)
                .is_some_and(|handle| self.registry.progress(&handle).is_ok())
        });
        expired.len()
    }
}

#[derive(Debug, Clone, PartialEq)]
pub(crate) struct DownloadChunk {
    pub(crate) chunk_index: u64,
    pub(crate) bytes: Vec<u8>,
    pub(crate) received_bytes: u64,
    pub(crate) next_chunk: u64,
    pub(crate) completed: Option<CompletedTransfer>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use sha2::{Digest, Sha256};

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn digest(bytes: &[u8]) -> String {
        Sha256::digest(bytes)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }

    fn downloads() -> (ArtifactDownloads, Arc<ArtifactIndex>) {
        let registry = Arc::new(TransferRegistry::default());
        let index = Arc::new(ArtifactIndex::default());
        (ArtifactDownloads::new(registry, Arc::clone(&index)), index)
    }

    fn register(index: &ArtifactIndex, id: &str, name: &str, mime: &str, bytes: &[u8]) {
        index
            .register(
                &lease(),
                "session-0001",
                "task-0001",
                id,
                name,
                mime,
                &digest(bytes),
                Arc::new(MemoryArtifactSource::new(bytes.to_vec())),
            )
            .unwrap();
    }

    #[test]
    fn chunk_budget_rejection_releases_the_download_handle_and_budget() {
        // A source may return less than max_bytes. Model repeated short reads
        // so the advertised byte budget alone cannot bound retry evidence.
        struct ShortChunkSource {
            total_bytes: u64,
        }
        impl DownloadSource for ShortChunkSource {
            fn total_bytes(&self) -> u64 {
                self.total_bytes
            }

            fn read_chunk(
                &self,
                chunk_index: u64,
                _max_bytes: usize,
            ) -> Result<Option<Vec<u8>>, String> {
                Ok((chunk_index < self.total_bytes).then(|| vec![1]))
            }
        }

        let (downloads, index) = downloads();
        let chunk_count = super::super::limits::MAX_TRANSFER_CHUNKS;
        let data = vec![1_u8; chunk_count + 1];
        index
            .register(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-budget",
                "report.bin",
                "application/octet-stream",
                &digest(&data),
                Arc::new(ShortChunkSource {
                    total_bytes: data.len() as u64,
                }),
            )
            .unwrap();
        let begin = downloads
            .begin(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-budget",
                1_000,
            )
            .unwrap();
        for chunk_index in 0..chunk_count {
            let chunk = downloads
                .next_chunk_bound(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-budget",
                    &begin.handle_id,
                    chunk_index as u64,
                    1_100,
                )
                .unwrap()
                .unwrap();
            assert_eq!(chunk.bytes, [1]);
            assert_eq!(chunk.received_bytes, (chunk_index + 1) as u64);
            assert_eq!(chunk.next_chunk, (chunk_index + 1) as u64);
            assert!(chunk.completed.is_none());
        }
        assert_eq!(downloads.registry.active_count(), 1);
        assert_eq!(downloads.active.lock().len(), 1);
        assert_eq!(
            downloads.registry.connection_bytes("connection-0001"),
            data.len() as u64
        );
        assert_eq!(
            downloads
                .next_chunk_bound(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-budget",
                    &begin.handle_id,
                    chunk_count as u64,
                    1_200,
                )
                .unwrap_err(),
            "transfer_chunk_budget"
        );
        assert_eq!(downloads.registry.active_count(), 0);
        assert_eq!(downloads.registry.connection_bytes("connection-0001"), 0);
        assert!(downloads.active.lock().is_empty());
        assert_eq!(
            downloads
                .next_chunk_bound(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-budget",
                    &begin.handle_id,
                    0,
                    1_300
                )
                .unwrap_err(),
            "transfer_unknown"
        );
        assert!(!downloads.abort(&begin.handle_id));
        // The failed transfer owns accounting, not the session's artifact.
        assert!(index.descriptor("session-0001", "artifact-budget").is_ok());
    }

    #[test]
    fn old_admission_download_cleanup_preserves_new_same_task_instance() {
        let (downloads, index) = downloads();
        for (id, sequence) in [("artifact-old", 1), ("artifact-new", 257)] {
            index
                .register_owned(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    id,
                    "report.md",
                    "text/markdown",
                    &digest(b"report"),
                    Arc::new(MemoryArtifactSource::new(b"report".to_vec())),
                    sequence,
                    true,
                )
                .unwrap();
        }
        assert_eq!(
            index.require_admission("artifact-old", 257).unwrap_err(),
            "artifact_binding_stale"
        );
        let old = downloads
            .begin(&lease(), "session-0001", "task-0001", "artifact-old", 1)
            .unwrap();
        let new = downloads
            .begin(&lease(), "session-0001", "task-0001", "artifact-new", 2)
            .unwrap();
        assert_eq!(downloads.cleanup_for_admission(1), 1);
        assert!(!downloads.active.lock().contains_key(&old.handle_id));
        assert!(downloads.active.lock().contains_key(&new.handle_id));
        let chunk = downloads
            .next_chunk_bound(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-new",
                &new.handle_id,
                0,
                3,
            )
            .unwrap()
            .unwrap();
        assert_eq!(chunk.bytes, b"report");
        assert!(chunk.completed.is_some());
        assert_eq!(downloads.cleanup_for_admission(1), 0);
        assert!(downloads.active.lock().is_empty());
    }

    #[test]
    fn impossible_chunk_budget_is_refused_without_a_download_allocation() {
        let registry = Arc::new(
            TransferRegistry::new(super::super::TransferLimits {
                chunk_bytes: 1,
                ..super::super::TransferLimits::default()
            })
            .unwrap(),
        );
        let index = Arc::new(ArtifactIndex::default());
        let downloads = ArtifactDownloads::new(registry.clone(), index.clone());
        let data = vec![1_u8; super::super::limits::MAX_TRANSFER_CHUNKS + 1];
        register(
            &index,
            "artifact-budget",
            "report.bin",
            "application/octet-stream",
            &data,
        );
        assert_eq!(
            downloads
                .begin(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-budget",
                    1_000
                )
                .unwrap_err(),
            "transfer_chunk_budget"
        );
        assert_eq!(registry.active_count(), 0);
        assert_eq!(registry.connection_bytes("connection-0001"), 0);
        assert!(downloads.active.lock().is_empty());
        assert!(index.descriptor("session-0001", "artifact-budget").is_ok());
    }

    #[test]
    fn chunk_arithmetic_is_bounded_and_never_widens_the_protocol_limit() {
        assert_eq!(chunk_count(0, 1024), 0);
        assert_eq!(chunk_count(1024, 1024), 1);
        assert_eq!(chunk_count(1025, 1024), 2);
        assert_eq!(
            chunk_count(MAX_TRANSFER_CHUNK_BYTES as u64 * 2, usize::MAX),
            2
        );
        assert_eq!(
            chunk_offset(3, usize::MAX),
            MAX_TRANSFER_CHUNK_BYTES as u64 * 3
        );
    }

    #[test]
    fn index_projection_never_contains_a_host_path() {
        let (downloads, index) = downloads();
        let bytes = b"report body".to_vec();
        // The file lives in a private directory; only the opaque ID is exposed.
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("report.md");
        std::fs::write(&path, &bytes).unwrap();
        index
            .register(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                "report.md",
                "text/markdown",
                &digest(&bytes),
                Arc::new(FileArtifactSource::new(path.clone())),
            )
            .unwrap();
        let descriptor = index.descriptor("session-0001", "artifact-0001").unwrap();
        let encoded = serde_json::to_string(&descriptor).unwrap();
        assert!(!encoded.contains(directory.path().to_string_lossy().as_ref()));
        assert!(encoded.contains("artifact-0001"));
        let listed = serde_json::to_string(&downloads.index().list("session-0001")).unwrap();
        assert!(!listed.contains(directory.path().to_string_lossy().as_ref()));
        // A file-backed source really serves the bytes behind the opaque ID.
        let preview = index.preview("session-0001", "artifact-0001").unwrap();
        assert_eq!(preview.text.as_deref(), Some("report body"));
    }

    #[test]
    fn preview_is_text_only_for_small_textual_artifacts() {
        let (_downloads, index) = downloads();
        register(&index, "artifact-text", "notes.txt", "text/plain", b"hello");
        let preview = index.preview("session-0001", "artifact-text").unwrap();
        assert_eq!(preview.preview_kind, PreviewKind::Text);
        assert_eq!(preview.text.as_deref(), Some("hello"));
        assert!(!preview.truncated);

        register(
            &index,
            "artifact-big",
            "big.txt",
            "text/plain",
            &vec![b'a'; (PREVIEW_MAX_BYTES + 1) as usize],
        );
        let preview = index.preview("session-0001", "artifact-big").unwrap();
        assert_eq!(preview.preview_kind, PreviewKind::DownloadOnly);
        assert!(preview.text.is_none());

        register(
            &index,
            "artifact-image",
            "photo.png",
            "image/png",
            b"\x89PNG",
        );
        let preview = index.preview("session-0001", "artifact-image").unwrap();
        assert_eq!(preview.preview_kind, PreviewKind::Unsupported);
        assert!(preview.text.is_none());
    }

    #[test]
    fn download_walks_the_artifact_in_order_and_verifies_the_digest() {
        let (_unused, index) = downloads();
        let bytes: Vec<u8> = (0..5000u32).map(|value| value as u8).collect();
        // Force multi-chunk transfers with a narrow chunk budget.
        let registry = Arc::new(
            TransferRegistry::new(super::super::TransferLimits {
                chunk_bytes: 1024,
                ..super::super::TransferLimits::default()
            })
            .unwrap(),
        );
        let downloads = ArtifactDownloads::new(registry, Arc::clone(&index));
        register(
            &index,
            "artifact-0001",
            "data.bin",
            "application/octet-stream",
            &bytes,
        );
        let begin = downloads
            .begin(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                1_000,
            )
            .unwrap();
        assert_eq!(begin.chunk_bytes, 1024);
        let mut collected = Vec::new();
        let mut completed = None;
        for index in 0..chunk_count(bytes.len() as u64, begin.chunk_bytes) {
            let chunk = downloads
                .next_chunk(
                    "session-0001",
                    "artifact-0001",
                    &begin.handle_id,
                    index,
                    1_100 + index,
                )
                .unwrap()
                .unwrap();
            collected.extend_from_slice(&chunk.bytes);
            completed = chunk.completed;
        }
        assert_eq!(collected, bytes);
        assert_eq!(completed.unwrap().sha256, digest(&bytes));
        // The handle is consumed once the transfer completed.
        assert_eq!(
            downloads
                .next_chunk("session-0001", "artifact-0001", &begin.handle_id, 0, 2_000)
                .unwrap_err(),
            "transfer_unknown"
        );
    }

    #[test]
    fn cross_session_ids_and_removed_artifacts_fail_closed() {
        let (downloads, index) = downloads();
        let bytes = b"artifact".to_vec();
        register(&index, "artifact-0001", "notes.txt", "text/plain", &bytes);
        assert_eq!(
            downloads
                .begin(
                    &lease(),
                    "session-0002",
                    "task-0001",
                    "artifact-0001",
                    1_000
                )
                .unwrap_err(),
            "artifact_not_found"
        );
        assert_eq!(
            downloads
                .begin(
                    &lease(),
                    "session-0001",
                    "task-0002",
                    "artifact-0001",
                    1_000
                )
                .unwrap_err(),
            "artifact_not_found"
        );
        let mut stale_lease = lease();
        stale_lease.lease_epoch = "2".into();
        assert_eq!(
            downloads
                .begin(
                    &stale_lease,
                    "session-0001",
                    "task-0001",
                    "artifact-0001",
                    1_000
                )
                .unwrap_err(),
            "artifact_not_found"
        );
        assert_eq!(
            downloads
                .begin(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-9999",
                    1_000
                )
                .unwrap_err(),
            "artifact_not_found"
        );
        assert!(index.remove("artifact-0001"));
        assert_eq!(
            downloads
                .begin(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-0001",
                    1_100
                )
                .unwrap_err(),
            "artifact_not_found"
        );
        assert!(!index.remove("artifact-0001"));
    }

    #[test]
    fn active_download_rejects_cross_task_chunk_and_abort() {
        let (downloads, index) = downloads();
        register(
            &index,
            "artifact-0001",
            "notes.txt",
            "text/plain",
            b"artifact",
        );
        let begun = downloads
            .begin(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                1_000,
            )
            .unwrap();
        assert_eq!(
            downloads
                .next_chunk_bound(
                    &lease(),
                    "session-0001",
                    "task-0002",
                    "artifact-0001",
                    &begun.handle_id,
                    0,
                    1_100,
                )
                .unwrap_err(),
            "transfer_binding_stale"
        );
        assert_eq!(
            downloads
                .abort_bound(
                    &lease(),
                    "session-0001",
                    "task-0002",
                    "artifact-0001",
                    &begun.handle_id,
                )
                .unwrap_err(),
            "transfer_binding_stale"
        );
        assert!(
            downloads
                .abort_bound(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-0001",
                    &begun.handle_id,
                )
                .unwrap()
        );
    }

    #[test]
    fn same_length_file_replacement_fails_preview_digest_check() {
        let (_downloads, index) = downloads();
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("report.txt");
        std::fs::write(&path, b"hello").unwrap();
        index
            .register(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                "report.txt",
                "text/plain",
                &digest(b"hello"),
                Arc::new(FileArtifactSource::new(path.clone())),
            )
            .unwrap();

        std::fs::write(path, b"world").unwrap();
        assert_eq!(
            index
                .preview_for_task_binding("session-0001", "artifact-0001", "task-0001", &lease(),)
                .unwrap_err(),
            "artifact_unavailable"
        );
    }

    #[test]
    fn a_shrinking_source_is_detected_before_the_transfer_starts() {
        use std::sync::atomic::{AtomicU64, Ordering};

        struct ResizableSource {
            len: AtomicU64,
        }

        impl DownloadSource for ResizableSource {
            fn total_bytes(&self) -> u64 {
                self.len.load(Ordering::SeqCst)
            }

            fn read_chunk(
                &self,
                _chunk_index: u64,
                _max_bytes: usize,
            ) -> std::result::Result<Option<Vec<u8>>, String> {
                Ok(None)
            }
        }

        let (downloads, index) = downloads();
        let source = Arc::new(ResizableSource {
            len: AtomicU64::new(10),
        });
        index
            .register(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                "notes.txt",
                "text/plain",
                &digest(b"0123456789"),
                Arc::clone(&source) as Arc<dyn DownloadSource>,
            )
            .unwrap();
        // Simulate the file being replaced between indexing and download.
        source.len.store(4, Ordering::SeqCst);
        assert_eq!(
            downloads
                .begin(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-0001",
                    1_000
                )
                .unwrap_err(),
            "artifact_unavailable"
        );
        // The stale entry was dropped instead of being served truncated.
        assert_eq!(index.len(), 0);
    }

    #[test]
    fn cleanup_is_scoped_and_idempotent() {
        let (downloads, index) = downloads();
        let bytes = b"artifact".to_vec();
        register(&index, "artifact-0001", "notes.txt", "text/plain", &bytes);
        downloads
            .begin(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                1_000,
            )
            .unwrap();
        assert_eq!(downloads.cleanup_for_task("task-0001"), 1);
        assert_eq!(downloads.cleanup_for_task("task-0001"), 0);
        assert_eq!(index.clear_session("session-0001"), 1);
        assert_eq!(index.clear_session("session-0001"), 0);
        assert_eq!(index.len(), 0);
    }

    #[test]
    fn connection_cleanup_removes_downloads_and_index_entries() {
        let (downloads, index) = downloads();
        register(
            &index,
            "artifact-0001",
            "notes.txt",
            "text/plain",
            b"artifact",
        );
        downloads
            .begin(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                1_000,
            )
            .unwrap();
        assert_eq!(downloads.cleanup_for_connection("connection-0001"), 1);
        assert_eq!(index.len(), 0);
        assert_eq!(downloads.cleanup_for_connection("connection-0001"), 0);
    }

    #[test]
    fn index_rejects_unbounded_or_path_bearing_metadata() {
        let (_downloads, index) = downloads();
        let source = Arc::new(MemoryArtifactSource::new(b"data".to_vec()));
        assert_eq!(
            index
                .register(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-0001",
                    "../escape.txt",
                    "text/plain",
                    &digest(b"data"),
                    source.clone(),
                )
                .unwrap()
                .display_name,
            "..escape.txt"
        );
        assert_eq!(
            index
                .register(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "../artifact",
                    "notes.txt",
                    "text/plain",
                    &digest(b"data"),
                    source.clone(),
                )
                .unwrap_err(),
            "artifact_id_invalid"
        );
        assert_eq!(
            index
                .register(
                    &lease(),
                    "session-0001",
                    "task-0001",
                    "artifact-0002",
                    "notes.txt",
                    "text/plain",
                    "not-a-digest",
                    source,
                )
                .unwrap_err(),
            "artifact_digest_invalid"
        );
    }

    #[test]
    fn pending_artifacts_are_invisible_until_atomic_task_publish() {
        let (_downloads, index) = downloads();
        let bytes = b"staged artifact".to_vec();
        let descriptor = index
            .register_pending(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-pending",
                "result.txt",
                "text/plain",
                &digest(&bytes),
                Arc::new(MemoryArtifactSource::new(bytes)),
            )
            .unwrap();
        assert!(index.list_for_binding("session-0001", &lease()).is_empty());
        assert_eq!(
            index
                .descriptor_for_binding("session-0001", &descriptor.artifact_id, &lease())
                .unwrap_err(),
            "artifact_not_found"
        );

        assert_eq!(
            index.publish_task("session-0001", "task-0001", &lease()),
            vec![descriptor.clone()]
        );
        assert_eq!(
            index.list_for_binding("session-0001", &lease()),
            vec![descriptor]
        );
        assert!(
            index
                .publish_task("session-0001", "task-0001", &lease())
                .is_empty()
        );
    }
}
