use std::fs;
use std::path::Path;

use agent_client_protocol::schema::v1::{
    ContentBlock, EmbeddedResource, EmbeddedResourceResource, ImageContent, PromptCapabilities,
    ResourceLink, TextContent, TextResourceContents,
};
use anyhow::{Context, Result, anyhow, bail};
use base64::Engine;
use serde::Serialize;

use super::workspace::WorkspacePromptReference;
use crate::features::files::file_ingest::{self, IngestResult};

const EMBED_FILE_MAX_TOKENS: u32 = 8_000;
const EMBED_TOTAL_MAX_TOKENS: u32 = 16_000;

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub(super) struct CodexDisplayAttachment {
    pub name: String,
    pub kind: String,
    pub size: u64,
}

pub(super) struct PreparedCodexPrompt {
    pub blocks: Vec<ContentBlock>,
    pub display_attachments: Vec<CodexDisplayAttachment>,
}

pub(super) fn prepare_codex_prompt(
    message: &str,
    attachments: &[IngestResult],
    workspace_references: &[WorkspacePromptReference],
    capabilities: &PromptCapabilities,
) -> Result<PreparedCodexPrompt> {
    let mut blocks = Vec::with_capacity(attachments.len() + workspace_references.len() + 1);
    if !message.trim().is_empty() {
        blocks.push(ContentBlock::Text(TextContent::new(message)));
    }

    let mut embedded_tokens = 0_u32;
    let mut display_attachments = Vec::with_capacity(attachments.len());
    for attachment in attachments {
        let path = if attachment.read_owner.is_some() {
            attachment.read_path().to_owned()
        } else {
            file_ingest::validate_path(&attachment.path)
                .map_err(anyhow::Error::msg)
                .with_context(|| format!("读取附件 {} 失败", attachment.basename))?
        };
        let format_path = if attachment.read_owner.is_some() {
            Path::new(&attachment.basename)
        } else {
            &path
        };
        if attachment.read_owner.is_some()
            && attachment.kind != "image"
            && attachment.markdown.is_none()
        {
            display_attachments.push(CodexDisplayAttachment {
                name: attachment.basename.clone(),
                kind: attachment.kind.clone(),
                size: attachment.byte_size,
            });
            // A resource link would let the agent bypass a rejected/failed
            // ingestion result by reopening the original bytes.
            blocks.push(ContentBlock::Text(TextContent::new(format!(
                "{}: {}",
                attachment.basename,
                attachment
                    .warning
                    .as_deref()
                    .unwrap_or("attachment_content_unavailable")
            ))));
            continue;
        }
        // The source fd is held only while this turn executes. URI strings
        // survive in ACP/model history, so use the independent named reference.
        let reference_path = if attachment.read_owner.is_some() {
            Path::new(&attachment.path)
        } else {
            &path
        };
        let extracted_reference = attachment.kind != "image"
            && attachment
                .read_owner
                .as_ref()
                .is_some_and(|owner| owner.read_path() != owner.path());
        let uri = tauri::Url::from_file_path(reference_path)
            .map_err(|_| anyhow::anyhow!("无法构造附件文件地址: {}", reference_path.display()))?
            .to_string();
        display_attachments.push(CodexDisplayAttachment {
            name: attachment.basename.clone(),
            kind: attachment.kind.clone(),
            size: attachment.byte_size,
        });

        if attachment.kind == "image" {
            if !capabilities.image {
                bail!(
                    "当前 Codex ACP Agent 未声明图片输入能力: {}",
                    attachment.basename
                );
            }
            let data =
                fs::read(&path).with_context(|| format!("读取图片附件失败: {}", path.display()))?;
            blocks.push(ContentBlock::Image(
                ImageContent::new(
                    base64::engine::general_purpose::STANDARD.encode(data),
                    codex_image_mime_type(format_path)?,
                )
                .uri(uri),
            ));
            continue;
        }

        let can_embed = capabilities.embedded_context
            && attachment.markdown.is_some()
            && attachment.token_estimate <= EMBED_FILE_MAX_TOKENS
            && embedded_tokens.saturating_add(attachment.token_estimate) <= EMBED_TOTAL_MAX_TOKENS;
        if can_embed {
            embedded_tokens = embedded_tokens.saturating_add(attachment.token_estimate);
            let resource =
                TextResourceContents::new(attachment.markdown.as_deref().unwrap_or_default(), uri)
                    .mime_type(text_mime_type(format_path));
            blocks.push(ContentBlock::Resource(EmbeddedResource::new(
                EmbeddedResourceResource::TextResourceContents(resource),
            )));
        } else {
            let (resource_path, resource_kind, size) = if extracted_reference {
                (
                    reference_path,
                    "text",
                    attachment.markdown.as_deref().unwrap_or_default().len() as u64,
                )
            } else {
                (format_path, attachment.kind.as_str(), attachment.byte_size)
            };
            let size = i64::try_from(size).unwrap_or(i64::MAX);
            let resource = ResourceLink::new(&attachment.basename, uri)
                .title(&attachment.basename)
                .size(size)
                .mime_type(resource_mime_type(resource_path, resource_kind));
            blocks.push(ContentBlock::ResourceLink(resource));
        }
    }

    for reference in workspace_references {
        let uri = tauri::Url::from_file_path(&reference.absolute_path)
            .map_err(|_| {
                anyhow::anyhow!(
                    "无法构造工作区文件地址: {}",
                    reference.absolute_path.display()
                )
            })?
            .to_string();
        let name = reference
            .absolute_path
            .file_name()
            .and_then(|value| value.to_str())
            .unwrap_or(&reference.relative_path);
        let size = i64::try_from(reference.size).unwrap_or(i64::MAX);
        blocks.push(ContentBlock::ResourceLink(
            ResourceLink::new(&reference.relative_path, uri)
                .title(&reference.relative_path)
                .size(size)
                .mime_type(resource_mime_type(&reference.absolute_path, "workspace")),
        ));
        display_attachments.push(CodexDisplayAttachment {
            name: name.to_string(),
            kind: "workspace".to_string(),
            size: reference.size,
        });
    }

    if blocks.is_empty() {
        bail!("消息和附件不能同时为空");
    }
    Ok(PreparedCodexPrompt {
        blocks,
        display_attachments,
    })
}

/// 扩展名 → 图片 MIME 的唯一映射表：附件内嵌与工作区图片预览共用，取两处
/// 原有表格的超集。返回 None 表示表外扩展名，由调用方决定回退或拒绝。
pub(super) fn image_mime_type(path: &Path) -> Option<&'static str> {
    match path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase()
        .as_str()
    {
        "png" => Some("image/png"),
        "jpg" | "jpeg" => Some("image/jpeg"),
        "gif" => Some("image/gif"),
        "webp" => Some("image/webp"),
        "svg" => Some("image/svg+xml"),
        "bmp" => Some("image/bmp"),
        _ => None,
    }
}

/// Codex 图片附件仅接受栅格格式：svg/bmp 虽在共享 MIME 表内（工作区预览用），
/// 附件内嵌保持既有行为显式拒绝；其余表外扩展名同样拒绝。
fn codex_image_mime_type(path: &Path) -> Result<&'static str> {
    let extension = path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase();
    image_mime_type(path)
        .filter(|mime| *mime != "image/svg+xml" && *mime != "image/bmp")
        .ok_or_else(|| anyhow!("Codex 不支持该图片格式: .{extension}"))
}

fn text_mime_type(path: &Path) -> &'static str {
    match path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase()
        .as_str()
    {
        "md" | "markdown" => "text/markdown",
        "json" => "application/json",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        _ => "text/plain",
    }
}

fn resource_mime_type(path: &Path, kind: &str) -> &'static str {
    match path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase()
        .as_str()
    {
        "pdf" => "application/pdf",
        "docx" => "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "xlsx" => "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        "pptx" => "application/vnd.openxmlformats-officedocument.presentationml.presentation",
        "zip" => "application/zip",
        _ if kind == "text" => text_mime_type(path),
        _ => "application/octet-stream",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::path::PathBuf;
    use std::time::{SystemTime, UNIX_EPOCH};

    struct TestDir(PathBuf);

    impl TestDir {
        fn new(label: &str) -> Self {
            let nonce = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            // `prepare_codex_prompt` forces attachments to live under `$HOME`
            // via `validate_path` (validate_upload_location). The fixture must
            // follow suit, or the guard rejects the legitimate fixture when the
            // tests run from a checkout outside $HOME (e.g. a /tmp worktree).
            let path = crate::platform::os::user_home_dir().join(format!(
                ".pinvou3-codex-attachment-test-{label}-{}-{nonce}",
                std::process::id()
            ));
            fs::create_dir_all(&path).unwrap();
            Self(path)
        }

        fn path(&self) -> &Path {
            &self.0
        }
    }

    impl Drop for TestDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn attachment(path: &Path, kind: &str, markdown: Option<&str>, tokens: u32) -> IngestResult {
        IngestResult {
            read_owner: None,
            kind: kind.to_string(),
            basename: path.file_name().unwrap().to_string_lossy().into_owned(),
            path: path.to_string_lossy().into_owned(),
            markdown: markdown.map(str::to_string),
            token_estimate: tokens,
            byte_size: fs::metadata(path).unwrap().len(),
            warning: None,
        }
    }

    #[test]
    fn image_uses_native_acp_content_block() {
        let dir = TestDir::new("image");
        let path = dir.path().join("image.png");
        fs::write(&path, b"png").unwrap();
        let capabilities = PromptCapabilities::new().image(true);
        let prepared = prepare_codex_prompt(
            "看图",
            &[attachment(&path, "image", None, 0)],
            &[],
            &capabilities,
        )
        .unwrap();
        assert!(matches!(prepared.blocks[0], ContentBlock::Text(_)));
        assert!(matches!(prepared.blocks[1], ContentBlock::Image(_)));
        assert_eq!(prepared.display_attachments[0].name, "image.png");
    }

    #[test]
    fn published_image_owner_survives_ingest_and_native_acp_second_read() {
        let dir = tempfile::tempdir().unwrap();
        let folder = dir.path().join("remote_uploads").join("ctr-up-image");
        let directory = crate::platform::filesystem::open_private_file_directory(&folder).unwrap();
        let bytes = base64::engine::general_purpose::STANDARD.decode(
            "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6xYkAAAAASUVORK5CYII=").unwrap();
        let publication = crate::platform::filesystem::publish_private_read_path(
            directory,
            std::ffi::OsStr::new("image.png"),
            &bytes,
        );
        if !crate::platform::filesystem::supports_private_read_path_publication() {
            assert_eq!(
                publication.unwrap_err().kind(),
                std::io::ErrorKind::Unsupported
            );
            return;
        }
        use sha2::{Digest, Sha256};
        let owner = std::sync::Arc::new(publication.unwrap());
        let descriptor = crate::features::files::file_ingest::PublishedAttachment {
            handle_id: "ctr-up-image".into(),
            path: owner.path().to_owned(),
            display_name: "image.png".into(),
            mime_type: "image/png".into(),
            bytes: bytes.len() as u64,
            sha256: Sha256::digest(&bytes)
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect(),
            read_owner: Some(owner.clone()),
        };
        let ingested = file_ingest::ingest_staged_attachment(dir.path(), &descriptor).unwrap();
        assert!(std::fs::write(owner.read_path(), b"replacement").is_err());
        let prepared =
            prepare_codex_prompt("", &[ingested], &[], &PromptCapabilities::new().image(true))
                .unwrap();
        let ContentBlock::Image(image) = &prepared.blocks[0] else {
            panic!("native image block required");
        };
        assert_eq!(
            image.data,
            base64::engine::general_purpose::STANDARD.encode(&bytes)
        );
        let image_reference = tauri::Url::parse(image.uri.as_deref().unwrap())
            .unwrap()
            .to_file_path()
            .unwrap();
        let independent_reference = owner.read_path() != owner.path();
        assert!(
            !serde_json::to_string(&prepared.blocks)
                .unwrap()
                .contains("/proc/")
        );
        drop(descriptor);
        owner.request_cleanup();
        drop(owner);
        assert!(!folder.exists());
        if independent_reference {
            assert_eq!(std::fs::read(image_reference).unwrap(), bytes);
        }
    }

    #[test]
    fn published_large_text_links_keep_named_references_after_source_cleanup() {
        if !crate::platform::filesystem::supports_private_read_path_publication() {
            return;
        }
        use sha2::{Digest as _, Sha256};
        let root = tempfile::tempdir().unwrap();
        let body = "verified large text\n".repeat(10_000).into_bytes();
        let owner = std::sync::Arc::new(
            crate::platform::filesystem::publish_private_read_path(
                crate::platform::filesystem::open_private_file_directory(
                    &root.path().join("remote_uploads/ctr-up-large"),
                )
                .unwrap(),
                std::ffi::OsStr::new("large.txt"),
                &body,
            )
            .unwrap(),
        );
        let independent_reference = owner.read_path() != owner.path();
        let descriptor = crate::features::files::file_ingest::PublishedAttachment {
            handle_id: "ctr-up-large".into(),
            path: owner.path().to_owned(),
            display_name: "large.txt".into(),
            mime_type: "text/plain".into(),
            bytes: body.len() as u64,
            sha256: crate::platform::encoding::hex_lower(&Sha256::digest(&body)),
            read_owner: Some(owner.clone()),
        };
        let ingested = file_ingest::ingest_staged_attachment(root.path(), &descriptor).unwrap();
        let reference = std::path::PathBuf::from(&ingested.path);
        let prepared = prepare_codex_prompt(
            "",
            &[ingested],
            &[],
            &PromptCapabilities::new().embedded_context(true),
        )
        .unwrap();
        let ContentBlock::ResourceLink(link) = &prepared.blocks[0] else {
            panic!("large text must use a resource link");
        };
        assert_eq!(
            tauri::Url::parse(&link.uri)
                .unwrap()
                .to_file_path()
                .unwrap(),
            reference
        );
        assert!(
            !serde_json::to_string(&prepared.blocks)
                .unwrap()
                .contains("/proc/")
        );
        drop(descriptor);
        owner.request_cleanup();
        drop(owner);
        if independent_reference {
            assert_eq!(std::fs::read(reference).unwrap(), body);
        }
    }

    #[test]
    fn published_redacted_attachment_stays_warning_only_without_resource_uri() {
        if !crate::platform::filesystem::supports_private_read_path_publication() {
            return;
        }
        use sha2::{Digest as _, Sha256};
        let root = tempfile::tempdir().unwrap();
        let body = b"synthetic rejected fixture bytes";
        let owner = std::sync::Arc::new(
            crate::platform::filesystem::publish_private_read_path(
                crate::platform::filesystem::open_private_file_directory(
                    &root.path().join("remote_uploads/ctr-up-redacted"),
                )
                .unwrap(),
                std::ffi::OsStr::new("redacted.key"),
                body,
            )
            .unwrap(),
        );
        owner.request_cleanup();
        let descriptor = crate::features::files::file_ingest::PublishedAttachment {
            handle_id: "ctr-up-redacted".into(),
            path: owner.path().to_owned(),
            display_name: "redacted.key".into(),
            mime_type: "application/octet-stream".into(),
            bytes: body.len() as u64,
            sha256: crate::platform::encoding::hex_lower(&Sha256::digest(body)),
            read_owner: Some(owner.clone()),
        };
        let ingested = file_ingest::ingest_staged_attachment(root.path(), &descriptor).unwrap();
        let warning = ingested.warning.clone().unwrap();
        let prepared = prepare_codex_prompt(
            "",
            &[ingested],
            &[],
            &PromptCapabilities::new().embedded_context(true),
        )
        .unwrap();
        assert_eq!(prepared.blocks.len(), 1);
        let ContentBlock::Text(text) = &prepared.blocks[0] else {
            panic!("rejected input must only send a warning");
        };
        assert!(text.text.contains(&warning));
        let serialized = serde_json::to_string(&prepared.blocks).unwrap();
        assert!(!serialized.contains("file:"));
        assert!(!serialized.contains("/proc/"));
        assert!(!serialized.contains(std::str::from_utf8(body).unwrap()));
        assert!(!root.path().join("attachment_references").exists());
        assert_eq!(prepared.display_attachments[0].name, "redacted.key");
    }

    #[test]
    #[ignore = "requires local 7z; no network or model"]
    fn published_archive_links_only_the_extracted_body_without_rejected_member_bytes() {
        if !crate::platform::filesystem::supports_private_read_path_publication() {
            return;
        }
        use sha2::{Digest as _, Sha256};
        let root = tempfile::tempdir().unwrap();
        let mut zip = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
        zip.start_file("notes.txt", zip::write::SimpleFileOptions::default())
            .unwrap();
        zip.write_all(b"accepted archive notes").unwrap();
        zip.start_file("redacted.key", zip::write::SimpleFileOptions::default())
            .unwrap();
        zip.write_all(b"synthetic rejected member bytes").unwrap();
        let bytes = zip.finish().unwrap().into_inner();
        let owner = std::sync::Arc::new(
            crate::platform::filesystem::publish_private_read_path(
                crate::platform::filesystem::open_private_file_directory(
                    &root.path().join("remote_uploads/ctr-up-archive"),
                )
                .unwrap(),
                std::ffi::OsStr::new("archive.zip"),
                &bytes,
            )
            .unwrap(),
        );
        owner.request_cleanup();
        // Named extracted references are the Linux adapter behavior. Windows
        // retains its existing original-file sharing lease.
        if owner.read_path() == owner.path() {
            return;
        }
        let descriptor = crate::features::files::file_ingest::PublishedAttachment {
            handle_id: "ctr-up-archive".into(),
            path: owner.path().to_owned(),
            display_name: "archive.zip".into(),
            mime_type: "application/zip".into(),
            bytes: bytes.len() as u64,
            sha256: crate::platform::encoding::hex_lower(&Sha256::digest(&bytes)),
            read_owner: Some(owner.clone()),
        };
        let ingested = file_ingest::ingest_staged_attachment(root.path(), &descriptor).unwrap();
        let markdown = ingested.markdown.clone().unwrap();
        assert!(markdown.contains("accepted archive notes"));
        assert!(markdown.contains("redacted.key"));
        assert!(!markdown.contains("synthetic rejected member bytes"));
        let prepared =
            prepare_codex_prompt("", &[ingested], &[], &PromptCapabilities::new()).unwrap();
        let ContentBlock::ResourceLink(link) = &prepared.blocks[0] else {
            panic!("disabled embedding must use the extracted reference");
        };
        let reference = tauri::Url::parse(&link.uri)
            .unwrap()
            .to_file_path()
            .unwrap();
        assert_eq!(reference.file_name().unwrap(), "content.md");
        assert_eq!(fs::read_to_string(reference).unwrap(), markdown);
        assert_eq!(link.mime_type.as_deref(), Some("text/markdown"));
        assert_eq!(link.size, Some(markdown.len() as i64));
        assert_eq!(prepared.display_attachments[0].name, "archive.zip");
    }

    #[test]
    fn small_text_uses_embedded_resource_and_large_text_uses_link() {
        let dir = TestDir::new("text");
        let small = dir.path().join("small.md");
        let large = dir.path().join("large.md");
        fs::File::create(&small)
            .unwrap()
            .write_all(b"small")
            .unwrap();
        fs::File::create(&large)
            .unwrap()
            .write_all(b"large")
            .unwrap();
        let capabilities = PromptCapabilities::new().embedded_context(true);
        let prepared = prepare_codex_prompt(
            "",
            &[
                attachment(&small, "text", Some("# small"), 10),
                attachment(&large, "text", Some("# large"), 8_001),
            ],
            &[],
            &capabilities,
        )
        .unwrap();
        assert!(matches!(prepared.blocks[0], ContentBlock::Resource(_)));
        assert!(matches!(prepared.blocks[1], ContentBlock::ResourceLink(_)));
    }

    #[test]
    fn image_requires_advertised_agent_capability() {
        let dir = TestDir::new("capability");
        let path = dir.path().join("image.png");
        fs::write(&path, b"png").unwrap();
        let error = prepare_codex_prompt(
            "",
            &[attachment(&path, "image", None, 0)],
            &[],
            &PromptCapabilities::default(),
        )
        .err()
        .expect("image capability must be enforced");
        assert!(error.to_string().contains("未声明图片输入能力"));
    }
}
