//! Full-fidelity session archive export (`tar.xz`).
//!
//! The interactive `/export` command renders a sanitized, lossy Markdown
//! transcript for humans. This module is the machine-facing counterpart: it
//! packs the durable session record into a compressed tar archive so a
//! complete session log — system prompt, every user and assistant message
//! including thinking blocks, tool calls and tool results, the branch
//! journal, approval receipts, and the session's artifacts — can be saved
//! with one command and restored later or attached to a bug report.
//!
//! Archive layout (format version 1):
//!
//! - `session.json` — the full [`SavedSession`] serialization, the same shape
//!   as the on-disk session record. Extract it and open it with `/load` in
//!   the TUI for a full-fidelity restore (system prompt included); note that
//!   `/resume <file>` imports the conversation transcript only.
//! - `container.json` — the portable [`SessionImportContainer`] for
//!   version-tolerant resume across schema changes.
//! - `artifacts/<...>` — the session-owned artifact directory, when present
//!   and not excluded. Only regular single-link files are archived:
//!   symlinked entries are skipped, a linked artifacts root is rejected at
//!   resolution time, and a hard-linked member fails the export, so a
//!   planted link can neither redirect the walk nor alias outside content
//!   in. An `output` inside the session store is rejected as well, so an
//!   export cannot clobber the records it reads. Residual: the walk itself
//!   is not race-confined (std has no `openat2` equivalent), so a writer
//!   inside the session store can still swap path components mid-export.
//! - `manifest.json` — archive format version, generator version, export
//!   timestamp, session metadata, and the index of the preceding members.
//!   Written last so its index covers everything above it.
//!
//! Contents are intentionally **not sanitized**: this is a full-fidelity log
//! for the session owner, unlike `/export`, which redacts secrets for
//! sharing. xz compression keeps verbose tool-heavy sessions small enough to
//! archive or attach.

use std::fs;
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};

use chrono::Utc;
use liblzma::write::XzEncoder;
use serde::Serialize;
use tar::Builder;

use crate::artifacts::ARTIFACTS_DIR_NAME;
use crate::session_manager::{SavedSession, SessionMetadata};
use crate::session_tree::SessionImportContainer;

/// Layout version of the exported archive. Bump when members change.
pub const SESSION_ARCHIVE_FORMAT_VERSION: u32 = 1;

/// Default xz compression preset (0–9), mirroring `xz -6`.
pub const DEFAULT_XZ_COMPRESSION_LEVEL: u32 = 6;

const SESSION_RECORD_MEMBER: &str = "session.json";
const SESSION_CONTAINER_MEMBER: &str = "container.json";
const ARCHIVE_MANIFEST_MEMBER: &str = "manifest.json";

/// Options for [`write_session_archive`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SessionArchiveOptions {
    /// Include the session-owned `artifacts/` directory when it exists.
    pub include_artifacts: bool,
    /// xz compression preset, 0 (fastest) through 9 (smallest).
    pub compression_level: u32,
}

impl Default for SessionArchiveOptions {
    fn default() -> Self {
        Self {
            include_artifacts: true,
            compression_level: DEFAULT_XZ_COMPRESSION_LEVEL,
        }
    }
}

/// One archive member reported in the manifest and [`SessionArchiveSummary`].
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct SessionArchiveMember {
    /// Member path inside the archive, `/`-separated.
    pub name: String,
    /// Uncompressed member size in bytes.
    pub bytes: u64,
}

/// Outcome of a successful [`write_session_archive`] call.
#[derive(Debug, Clone, Serialize)]
pub struct SessionArchiveSummary {
    /// Path the `.tar.xz` archive was written to.
    pub output: PathBuf,
    /// Exported session id.
    pub session_id: String,
    /// [`SESSION_ARCHIVE_FORMAT_VERSION`] used for this archive.
    pub archive_format_version: u32,
    /// xz preset the archive was written with.
    pub compression_level: u32,
    /// Whether `artifacts/` members were included.
    pub includes_artifacts: bool,
    /// Member index in write order. `manifest.json` itself is excluded: it
    /// is written last and indexes everything before it.
    pub members: Vec<SessionArchiveMember>,
}

impl SessionArchiveSummary {
    /// Sum of uncompressed member sizes.
    pub fn total_member_bytes(&self) -> u64 {
        self.members.iter().map(|member| member.bytes).sum()
    }

    /// Compressed archive size in bytes, or 0 if the file is unreadable.
    pub fn compressed_bytes(&self) -> u64 {
        fs::metadata(&self.output).map_or(0, |meta| meta.len())
    }
}

#[derive(Serialize)]
struct ArchiveManifest {
    archive_format_version: u32,
    generator: &'static str,
    generator_version: &'static str,
    exported_at: String,
    session: SessionMetadata,
    members: Vec<SessionArchiveMember>,
}

/// Write one session as a full-fidelity `.tar.xz` archive.
///
/// `session` is typically loaded with
/// [`SessionManager::load_session_snapshot`](crate::session_manager::SessionManager::load_session_snapshot)
/// so the archive reflects the durable record without applying resume-time
/// repair. `sessions_dir` is the trusted store the session belongs to: when
/// given, the artifacts directory is resolved and confined through
/// [`session_artifacts_dir`], and an `output` inside the store is rejected —
/// records live at `<store>/<id>.json` and an export must never clobber
/// them. Pass `None` (or clear
/// [`SessionArchiveOptions::include_artifacts`]) to export the transcript
/// only, without store checks.
///
/// The archive is streamed to a sibling temporary file and renamed into
/// place, so a failed export never leaves a truncated archive at `output`.
/// An existing `output` is replaced.
pub fn write_session_archive(
    session: &SavedSession,
    sessions_dir: Option<&Path>,
    output: &Path,
    options: SessionArchiveOptions,
) -> io::Result<SessionArchiveSummary> {
    if options.compression_level > 9 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!(
                "xz compression level {} is out of range 0-9",
                options.compression_level
            ),
        ));
    }
    let session_json = session_json(session)?;
    let container_json = container_json(session)?;

    let parent = output
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
        .map_or_else(|| PathBuf::from("."), Path::to_path_buf);
    fs::create_dir_all(&parent)?;
    let root = sessions_dir
        .map(Path::canonicalize)
        .transpose()
        .map_err(|error| {
            io::Error::new(
                error.kind(),
                format!("cannot resolve the session store for export: {error}"),
            )
        })?;
    if let Some(root) = &root {
        let output_parent = parent.canonicalize().map_err(|error| {
            io::Error::new(
                error.kind(),
                format!(
                    "cannot resolve export output directory {}: {error}",
                    parent.display()
                ),
            )
        })?;
        if output_parent.starts_with(root) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "export output must be outside the session store",
            ));
        }
    }

    let artifact_files = match (options.include_artifacts, root.as_deref()) {
        (true, Some(root)) => match session_artifacts_dir(root, &session.metadata.id)? {
            Some(dir) => collect_artifact_files(&dir)?,
            None => Vec::new(),
        },
        _ => Vec::new(),
    };

    let temp = tempfile::Builder::new()
        .prefix(".codewhale-session-export-")
        .tempfile_in(&parent)?;
    let temp_path = temp.into_temp_path();

    let mut summary = SessionArchiveSummary {
        output: output.to_path_buf(),
        session_id: session.metadata.id.clone(),
        archive_format_version: SESSION_ARCHIVE_FORMAT_VERSION,
        compression_level: options.compression_level,
        includes_artifacts: !artifact_files.is_empty(),
        members: Vec::new(),
    };

    {
        let file = fs::File::create(&temp_path)?;
        let mut tar = Builder::new(XzEncoder::new(file, options.compression_level));
        append_member(
            &mut tar,
            SESSION_RECORD_MEMBER,
            MemberContents::Bytes(session_json.as_bytes()),
            &mut summary.members,
        )?;
        append_member(
            &mut tar,
            SESSION_CONTAINER_MEMBER,
            MemberContents::Bytes(container_json.as_bytes()),
            &mut summary.members,
        )?;
        for (name, path) in &artifact_files {
            append_member(
                &mut tar,
                name,
                MemberContents::File(path),
                &mut summary.members,
            )?;
        }
        let manifest_json = serde_json::to_string_pretty(&ArchiveManifest {
            archive_format_version: SESSION_ARCHIVE_FORMAT_VERSION,
            generator: env!("CARGO_PKG_NAME"),
            generator_version: env!("CARGO_PKG_VERSION"),
            exported_at: Utc::now().to_rfc3339(),
            session: session.metadata.clone(),
            members: summary.members.clone(),
        })
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))?;
        append_member(
            &mut tar,
            ARCHIVE_MANIFEST_MEMBER,
            MemberContents::Bytes(manifest_json.as_bytes()),
            &mut Vec::new(),
        )?;
        let encoder = tar.into_inner()?;
        let mut file = encoder.finish()?;
        file.flush()?;
        file.sync_all()?;
    }
    temp_path.persist(output).map_err(|error| error.error)?;

    Ok(summary)
}

/// Directory holding this session's artifacts, when it exists. `session_id`
/// is re-checked here because this helper is also the boundary for callers
/// that build the path from user-supplied ids; invalid ids are a loud
/// `InvalidInput` like upstream, not a silent transcript-only export.
pub fn session_artifacts_dir(sessions_dir: &Path, session_id: &str) -> io::Result<Option<PathBuf>> {
    if session_id.is_empty()
        || session_id == "."
        || session_id == ".."
        || session_id.contains(['/', '\\'])
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("invalid session id {session_id:?}"),
        ));
    }
    let session_dir = sessions_dir.join(session_id);
    let dir = session_dir.join(ARTIFACTS_DIR_NAME);
    // Detect a link at either level before following it: `is_dir` and
    // `read_dir` would happily walk the link target, and the member-level
    // symlink skip cannot see a redirected containing directory. Every
    // linked root is rejected — even one resolving inside the store — so
    // cross-session redirection fails too.
    for candidate in [&session_dir, &dir] {
        if fs::symlink_metadata(candidate)
            .map(|meta| meta.file_type().is_symlink())
            .unwrap_or(false)
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!(
                    "artifact root {} is a symlink; refusing to follow links under the session store {}",
                    candidate.display(),
                    sessions_dir.display()
                ),
            ));
        }
    }
    if !dir.is_dir() {
        return Ok(None);
    }
    // Backstop for link chains the per-component probe cannot see (Windows
    // reparse points on toolchains where they are not `is_symlink`, swaps
    // between the probes): resolve both ends and require the artifacts root
    // to stay inside the store.
    let canonical = dir.canonicalize().map_err(|error| {
        io::Error::new(
            error.kind(),
            format!(
                "cannot resolve artifact directory {}: {error}",
                dir.display()
            ),
        )
    })?;
    let root = sessions_dir.canonicalize().map_err(|error| {
        io::Error::new(
            error.kind(),
            format!(
                "cannot resolve session store {}: {error}",
                sessions_dir.display()
            ),
        )
    })?;
    if !canonical.starts_with(&root) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!(
                "artifact directory {} resolves outside the session store {}",
                dir.display(),
                root.display()
            ),
        ));
    }
    // Hand back the canonical path that was validated so the walk cannot
    // re-resolve a swapped prefix.
    Ok(Some(canonical))
}

/// Default archive file name for a session:
/// `codewhale-session-<short-id>.tar.xz`.
pub fn default_archive_file_name(metadata: &SessionMetadata) -> String {
    format!(
        "codewhale-session-{}.tar.xz",
        crate::session_manager::truncate_id(&metadata.id)
    )
}

fn session_json(session: &SavedSession) -> io::Result<String> {
    serde_json::to_string_pretty(session)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
}

fn container_json(session: &SavedSession) -> io::Result<String> {
    let container: SessionImportContainer = session.export_container("session-archive");
    serde_json::to_string_pretty(&container)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
}

/// Collect regular artifact files as `(archive member name, source path)`,
/// sorted by member name for deterministic archives. Symlinked and other
/// non-regular entries are skipped; containing-directory confinement is
/// enforced by [`session_artifacts_dir`].
fn collect_artifact_files(artifacts_dir: &Path) -> io::Result<Vec<(String, PathBuf)>> {
    let mut files = Vec::new();
    collect_artifact_files_recursive(artifacts_dir, "", &mut files)?;
    files.sort_by(|left, right| left.0.cmp(&right.0));
    Ok(files)
}

/// Hard links are indistinguishable from regular files to a directory walk
/// but alias content from anywhere on the filesystem into it; the link count
/// is the only tell.
#[cfg(unix)]
fn has_single_link(metadata: &fs::Metadata) -> bool {
    use std::os::unix::fs::MetadataExt;
    metadata.nlink() == 1
}

/// NTFS hard links expose the count through the Windows metadata extension.
#[cfg(windows)]
fn has_single_link(metadata: &fs::Metadata) -> bool {
    use std::os::windows::fs::MetadataExt;
    metadata.number_of_links() == 1
}

#[cfg(not(any(unix, windows)))]
fn has_single_link(_: &fs::Metadata) -> bool {
    // No portable link-count API on this target; the root-level probes and
    // the member symlink skip still confine links there.
    true
}

fn collect_artifact_files_recursive(
    dir: &Path,
    prefix: &str,
    files: &mut Vec<(String, PathBuf)>,
) -> io::Result<()> {
    for entry in fs::read_dir(dir)? {
        let entry = entry?;
        let file_type = entry.file_type()?;
        if file_type.is_symlink() {
            continue;
        }
        let child = entry.file_name().to_string_lossy().into_owned();
        let member = if prefix.is_empty() {
            format!("{ARTIFACTS_DIR_NAME}/{child}")
        } else {
            format!("{prefix}/{child}")
        };
        let path = dir.join(&child);
        if file_type.is_dir() {
            collect_artifact_files_recursive(&path, &member, files)?;
        } else if file_type.is_file() {
            if !has_single_link(&entry.metadata()?) {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!(
                        "artifact member {member} is a hard link; exports only archive single-link files"
                    ),
                ));
            }
            files.push((member, path));
        }
    }
    Ok(())
}

fn archive_header(size: u64) -> tar::Header {
    let mut header = tar::Header::new_gnu();
    header.set_size(size);
    header.set_mode(0o644);
    header.set_mtime(Utc::now().timestamp().max(0) as u64);
    header.set_cksum();
    header
}

/// Payload for one archive member: in-memory bytes or a filesystem file
/// streamed straight into the tar.
enum MemberContents<'a> {
    Bytes(&'a [u8]),
    File(&'a Path),
}

/// Append exactly `expected` bytes of `reader` under `name`. The bounded read
/// keeps the tar header and the member payload consistent when the source
/// file changes size mid-export: growth is capped at the snapshot size
/// (valid archive, prefix content), while shrinkage — which would otherwise
/// silently shift every following header and corrupt the archive — fails the
/// export instead.
fn append_sized<W: Write, R: Read>(
    tar: &mut Builder<W>,
    header: &mut tar::Header,
    name: &str,
    reader: R,
    expected: u64,
) -> io::Result<()> {
    let mut limited = reader.take(expected);
    tar.append_data(header, name, &mut limited)?;
    if limited.limit() != 0 {
        return Err(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            format!(
                "member {name} ended before its recorded size of {expected} bytes; the source changed during export"
            ),
        ));
    }
    Ok(())
}

fn append_member(
    tar: &mut Builder<XzEncoder<fs::File>>,
    name: &str,
    contents: MemberContents<'_>,
    members: &mut Vec<SessionArchiveMember>,
) -> io::Result<()> {
    match contents {
        MemberContents::Bytes(bytes) => {
            let mut header = archive_header(bytes.len() as u64);
            tar.append_data(&mut header, name, bytes)?;
            members.push(SessionArchiveMember {
                name: name.to_string(),
                bytes: bytes.len() as u64,
            });
        }
        MemberContents::File(path) => {
            let mut file = fs::File::open(path)?;
            let size = file.metadata()?.len();
            let mut header = archive_header(size);
            append_sized(tar, &mut header, name, &mut file, size)?;
            members.push(SessionArchiveMember {
                name: name.to_string(),
                bytes: size,
            });
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::{ContentBlock, Message, Role};
    use crate::session_manager::create_saved_session;
    use crate::session_tree::SessionJournal;
    use std::io::Read;

    fn fixture_session() -> SavedSession {
        let messages = vec![
            Message {
                role: Role::User,
                content: vec![ContentBlock::Text {
                    text: "list the files in src".to_string(),
                    cache_control: None,
                }],
            },
            Message {
                role: Role::Assistant,
                content: vec![
                    ContentBlock::thinking("I should run ls first"),
                    ContentBlock::ToolUse {
                        id: "toolu_01".to_string(),
                        name: "shell".to_string(),
                        input: serde_json::json!({ "command": "ls src" }),
                        caller: None,
                        thought_signature: None,
                    },
                ],
            },
            Message {
                role: Role::User,
                content: vec![ContentBlock::ToolResult {
                    tool_use_id: "toolu_01".to_string(),
                    content: "main.rs\nlib.rs".to_string(),
                    is_error: None,
                    content_blocks: None,
                }],
            },
        ];
        let mut session = create_saved_session(
            &messages,
            "test-model",
            Path::new("/tmp/archive-fixture"),
            128,
            None,
        );
        session.system_prompt = Some("You are a careful coding agent.".to_string());
        session.journal = Some(SessionJournal::from_messages(messages, 0));
        session
    }

    fn read_archive_members(path: &Path) -> Vec<(String, Vec<u8>)> {
        let file = fs::File::open(path).expect("archive opens");
        let mut archive = tar::Archive::new(liblzma::read::XzDecoder::new(file));
        archive
            .entries()
            .expect("archive entries")
            .map(|entry| {
                let mut entry = entry.expect("entry");
                let name = entry.path().expect("path").to_string_lossy().into_owned();
                let mut bytes = Vec::new();
                entry.read_to_end(&mut bytes).expect("member bytes");
                (name, bytes)
            })
            .collect()
    }

    #[test]
    fn forkguard_session_archive_export_roundtrips_full_context() {
        let session = fixture_session();
        let dir = tempfile::tempdir().expect("tempdir");
        let output = dir.path().join("session.tar.xz");

        let summary =
            write_session_archive(&session, None, &output, SessionArchiveOptions::default())
                .expect("archive export");

        assert_eq!(summary.session_id, session.metadata.id);
        let mut members = read_archive_members(&output);
        members.sort_by(|left, right| left.0.cmp(&right.0));
        let names: Vec<&str> = members.iter().map(|(name, _)| name.as_str()).collect();
        assert_eq!(
            names,
            vec!["container.json", "manifest.json", "session.json"]
        );

        // The raw session record restores the complete context: system
        // prompt, thinking, tool call, and tool result.
        let (_, session_bytes) = members
            .iter()
            .find(|(name, _)| name == SESSION_RECORD_MEMBER)
            .expect("session.json member");
        let restored: SavedSession =
            serde_json::from_slice(session_bytes).expect("session.json parses");
        assert_eq!(
            restored.system_prompt.as_deref(),
            Some("You are a careful coding agent.")
        );
        let mut saw_tool_use = false;
        let mut saw_tool_result = false;
        let mut saw_thinking = false;
        for message in &restored.messages {
            for block in &message.content {
                match block {
                    ContentBlock::ToolUse { id, .. } => {
                        saw_tool_use = true;
                        assert_eq!(id, "toolu_01");
                    }
                    ContentBlock::ToolResult { tool_use_id, .. } => {
                        saw_tool_result = true;
                        assert_eq!(tool_use_id, "toolu_01");
                    }
                    ContentBlock::Thinking { .. } => saw_thinking = true,
                    _ => {}
                }
            }
        }
        assert!(saw_tool_use && saw_tool_result && saw_thinking);

        // The portable container feeds the exact import path `/resume` uses
        // for exported session JSON.
        let (_, container_bytes) = members
            .iter()
            .find(|(name, _)| name == SESSION_CONTAINER_MEMBER)
            .expect("container.json member");
        let container = SessionImportContainer::from_json(
            std::str::from_utf8(container_bytes).expect("container utf8"),
        )
        .expect("container parses");
        let imported = SavedSession::import_foreign(
            container,
            PathBuf::from("/tmp/archive-fixture"),
            "test-model".to_string(),
        )
        .expect("container imports");
        assert!(
            imported
                .messages
                .iter()
                .any(|message| message.content.iter().any(
                    |block| matches!(block, ContentBlock::ToolUse { id, .. } if id == "toolu_01")
                )),
            "imported session keeps the tool call"
        );
        let manifest: serde_json::Value = serde_json::from_slice(
            &members
                .iter()
                .find(|(name, _)| name == ARCHIVE_MANIFEST_MEMBER)
                .expect("manifest member")
                .1,
        )
        .expect("manifest parses");
        assert_eq!(
            manifest["archive_format_version"],
            SESSION_ARCHIVE_FORMAT_VERSION
        );
        assert_eq!(manifest["session"]["id"], session.metadata.id);
    }

    #[test]
    fn forkguard_session_archive_includes_artifacts_and_respects_skip() {
        let session = fixture_session();
        let dir = tempfile::tempdir().expect("tempdir");
        let sessions_dir = dir.path().join("sessions");
        let artifacts_dir = sessions_dir
            .join(&session.metadata.id)
            .join(ARTIFACTS_DIR_NAME);
        fs::create_dir_all(&artifacts_dir).expect("artifacts dir");
        fs::write(artifacts_dir.join("art_call-1.txt"), b"artifact body").expect("artifact");
        let resolved = session_artifacts_dir(&sessions_dir, &session.metadata.id)
            .expect("valid root resolves")
            .expect("artifacts dir is discovered for a valid session id");
        // The resolver hands back the canonical path it validated, so the
        // walk cannot re-resolve a different (swapped) prefix.
        assert_eq!(
            resolved,
            fs::canonicalize(&artifacts_dir).expect("canonicalize artifacts"),
            "resolver returns the canonical path it validated"
        );
        let error = session_artifacts_dir(&sessions_dir, "../escape")
            .expect_err("traversal ids are rejected, not silently skipped");
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        assert!(
            error.to_string().contains("invalid session id"),
            "error must name the invalid id: {error}"
        );

        let with_artifacts = write_session_archive(
            &session,
            Some(&sessions_dir),
            &dir.path().join("full.tar.xz"),
            SessionArchiveOptions::default(),
        )
        .expect("archive with artifacts");
        assert!(with_artifacts.includes_artifacts);
        let names: Vec<&str> = with_artifacts
            .members
            .iter()
            .map(|member| member.name.as_str())
            .collect();
        assert_eq!(
            names,
            vec!["session.json", "container.json", "artifacts/art_call-1.txt"]
        );
        let members = read_archive_members(&dir.path().join("full.tar.xz"));
        let (_, artifact_bytes) = members
            .iter()
            .find(|(name, _)| name == "artifacts/art_call-1.txt")
            .expect("artifact member");
        assert_eq!(artifact_bytes, b"artifact body");

        let transcript_only = write_session_archive(
            &session,
            Some(&sessions_dir),
            &dir.path().join("lean.tar.xz"),
            SessionArchiveOptions {
                include_artifacts: false,
                ..SessionArchiveOptions::default()
            },
        )
        .expect("archive without artifacts");
        assert!(!transcript_only.includes_artifacts);
        assert!(
            !transcript_only
                .members
                .iter()
                .any(|member| member.name.starts_with(ARTIFACTS_DIR_NAME))
        );
    }

    #[cfg(unix)]
    #[test]
    fn forkguard_session_archive_rejects_artifact_root_symlink() {
        // A symlinked artifacts root would redirect the whole archive walk
        // outside the session store; the confinement check must reject it
        // loudly instead of silently archiving someone else's directory.
        use std::os::unix::fs::symlink;

        let dir = tempfile::tempdir().expect("tempdir");
        let sessions_dir = dir.path().join("sessions");
        let outside = dir.path().join("outside");
        fs::create_dir_all(&outside).expect("outside dir");
        fs::write(outside.join("secret.txt"), b"not session data").expect("outside file");
        let session = fixture_session();
        fs::create_dir_all(sessions_dir.join(&session.metadata.id)).expect("session dir");
        symlink(
            &outside,
            sessions_dir
                .join(&session.metadata.id)
                .join(ARTIFACTS_DIR_NAME),
        )
        .expect("link the artifacts root");

        let error = session_artifacts_dir(&sessions_dir, &session.metadata.id)
            .expect_err("a symlinked artifacts root must be rejected");
        assert!(
            error.to_string().contains("is a symlink"),
            "error must name the linked root: {error}"
        );

        // A symlinked session directory (one level up) is the same escape.
        let second = fixture_session();
        fs::create_dir_all(&sessions_dir).expect("sessions root");
        symlink(&outside, sessions_dir.join(&second.metadata.id)).expect("link the session dir");
        let error = session_artifacts_dir(&sessions_dir, &second.metadata.id)
            .expect_err("a symlinked session directory must be rejected");
        assert!(
            error.to_string().contains("is a symlink"),
            "error must name the linked root: {error}"
        );
    }

    #[cfg(unix)]
    #[test]
    fn forkguard_session_archive_rejects_artifact_hard_link() {
        // A hard link aliases outside content into the walk with no symlink
        // anywhere for the root probe to see; the walker must fail the export
        // instead of archiving another inode's bytes under a benign member
        // name (upstream rejects multi-link files for exactly this reason).
        let dir = tempfile::tempdir().expect("tempdir");
        let sessions_dir = dir.path().join("sessions");
        let outside = dir.path().join("outside");
        fs::create_dir_all(&outside).expect("outside dir");
        fs::write(outside.join("secret.txt"), b"not session data").expect("outside file");
        let session = fixture_session();
        let artifacts_dir = sessions_dir
            .join(&session.metadata.id)
            .join(ARTIFACTS_DIR_NAME);
        fs::create_dir_all(&artifacts_dir).expect("artifacts dir");
        fs::write(artifacts_dir.join("plain.txt"), b"session data").expect("plain file");
        fs::hard_link(
            outside.join("secret.txt"),
            artifacts_dir.join("innocent.txt"),
        )
        .expect("hard link");

        let error = write_session_archive(
            &session,
            Some(&sessions_dir),
            &dir.path().join("out.tar.xz"),
            SessionArchiveOptions::default(),
        )
        .expect_err("a hard-linked member must fail the export");
        assert!(
            error.to_string().contains("hard link"),
            "error must name the hard link: {error}"
        );

        // Control: the same tree without the link exports cleanly.
        fs::remove_file(artifacts_dir.join("innocent.txt")).expect("remove the link");
        let summary = write_session_archive(
            &session,
            Some(&sessions_dir),
            &dir.path().join("out.tar.xz"),
            SessionArchiveOptions::default(),
        )
        .expect("a clean tree exports");
        assert!(summary.includes_artifacts);
    }

    #[test]
    fn forkguard_session_archive_rejects_output_inside_store() {
        // Session records live at <store>/<id>.json, so an export written
        // inside the store (e.g. a mistyped --output with --force) would
        // clobber a live record; the writer rejects in-store outputs the way
        // upstream does instead of trusting each caller to check.
        let dir = tempfile::tempdir().expect("tempdir");
        let sessions_dir = dir.path().join("sessions");
        let session = fixture_session();
        let artifacts_dir = sessions_dir
            .join(&session.metadata.id)
            .join(ARTIFACTS_DIR_NAME);
        fs::create_dir_all(&artifacts_dir).expect("artifacts dir");
        fs::write(artifacts_dir.join("art.txt"), b"artifact body").expect("artifact");

        let error = write_session_archive(
            &session,
            Some(&sessions_dir),
            &sessions_dir.join("inside.tar.xz"),
            SessionArchiveOptions::default(),
        )
        .expect_err("an output inside the store must be rejected");
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        assert!(
            error.to_string().contains("outside the session store"),
            "error must name the confinement failure: {error}"
        );
        assert!(
            !sessions_dir.join("inside.tar.xz").exists(),
            "a rejected export must not write anything"
        );

        let summary = write_session_archive(
            &session,
            Some(&sessions_dir),
            &dir.path().join("outside.tar.xz"),
            SessionArchiveOptions::default(),
        )
        .expect("an output outside the store is fine");
        assert!(summary.includes_artifacts);
    }

    #[test]
    fn forkguard_session_archive_rejects_artifact_shorter_than_recorded_size() {
        // A member whose source shrank between stat and copy must fail the
        // export instead of being zero-padded into a silently shifted
        // archive (the growth direction is capped to the snapshot size).
        let mut tar = Builder::new(Vec::new());
        let mut header = archive_header(16);
        let error = append_sized(
            &mut tar,
            &mut header,
            "artifacts/shrinking.bin",
            &b"only-nine"[..],
            16,
        )
        .expect_err("short member must fail the export");
        assert_eq!(error.kind(), io::ErrorKind::UnexpectedEof);

        let mut tar = Builder::new(Vec::new());
        let mut header = archive_header(9);
        append_sized(
            &mut tar,
            &mut header,
            "artifacts/stable.bin",
            &b"only-nine"[..],
            9,
        )
        .expect("exact-size member succeeds");
    }

    #[test]
    fn session_archive_rejects_out_of_range_compression_level() {
        let session = fixture_session();
        let dir = tempfile::tempdir().expect("tempdir");
        let error = write_session_archive(
            &session,
            None,
            &dir.path().join("out.tar.xz"),
            SessionArchiveOptions {
                compression_level: 10,
                ..SessionArchiveOptions::default()
            },
        )
        .expect_err("level 10 must be rejected");
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
    }

    #[test]
    fn session_archive_replaces_existing_output_atomically() {
        let session = fixture_session();
        let dir = tempfile::tempdir().expect("tempdir");
        let output = dir.path().join("out.tar.xz");
        write_session_archive(&session, None, &output, SessionArchiveOptions::default())
            .expect("first export");
        write_session_archive(&session, None, &output, SessionArchiveOptions::default())
            .expect("second export replaces");
        let members = read_archive_members(&output);
        assert_eq!(members.len(), 3);
        assert!(default_archive_file_name(&session.metadata).ends_with(".tar.xz"));
    }
}
