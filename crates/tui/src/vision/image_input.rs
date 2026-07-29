//! Shared workspace-image validation and request-time materialization.
//!
//! Sessions persist images as [`crate::models::ContentBlock::LocalImage`]
//! references (workspace-relative path + MIME + display name + byte size) —
//! never Base64. Right before a provider request is built, the engine clones
//! the message list and calls [`materialize_messages_local_images`] to turn
//! those references into `data:` URL image blocks. The materialized clone is
//! short-lived and must never be persisted, logged, or fed back into the op
//! mailbox.
//!
//! The `image_analyze` tool (`vision::tools`) reuses the same path-boundary
//! and file-validation helpers so both entry points enforce identical rules.

use std::path::{Component, Path, PathBuf};

use base64::{Engine as _, engine::general_purpose::STANDARD as BASE64};

use crate::models::{ContentBlock, ImageUrlContent, Message};

/// Unified upper bound for one image, checked before any Base64 encoding.
/// Aligned with the application-side attachment limit (`MAX_FILE_BYTES`).
pub const MAX_IMAGE_BYTES: u64 = 20 * 1024 * 1024;

/// Number of leading bytes needed to sniff every supported signature
/// (WEBP needs 12: `RIFF` + size + `WEBP`).
const SNIFF_HEADER_BYTES: usize = 16;

/// User-facing validation/materialization failure for local image input.
///
/// Messages are bilingual (Chinese first) so hosts can surface them verbatim;
/// they never contain Base64 or full request bodies.
#[derive(Debug)]
pub struct ImageInputError {
    message: String,
}

impl ImageInputError {
    fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
        }
    }
}

impl std::fmt::Display for ImageInputError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for ImageInputError {}

/// Resolve a workspace-relative image path, rejecting absolute paths, `..`
/// traversal, and symlink escapes. Shared by native image input and the
/// `image_analyze` tool.
pub fn resolve_workspace_image_path(
    workspace: &Path,
    relative_path: &Path,
) -> Result<PathBuf, ImageInputError> {
    if relative_path.components().any(|c| {
        matches!(
            c,
            Component::Prefix(_) | Component::RootDir | Component::ParentDir
        )
    }) {
        return Err(ImageInputError::new(
            "图片路径必须是工作区内的相对路径，不能越界 \
             (image path must be a relative path within the workspace and cannot escape it)",
        ));
    }

    let workspace = workspace.canonicalize().map_err(|e| {
        ImageInputError::new(format!(
            "无法解析工作区路径 (failed to resolve workspace path): {e}"
        ))
    })?;
    let candidate = workspace.join(relative_path);
    let resolved = candidate.canonicalize().map_err(|e| {
        if e.kind() == std::io::ErrorKind::NotFound {
            ImageInputError::new(format!(
                "原图片文件已不存在，无法发送 (the original image file no longer exists): {}",
                relative_path.display()
            ))
        } else {
            ImageInputError::new(format!(
                "无法解析图片文件 (failed to resolve image file): {e}"
            ))
        }
    })?;
    if !resolved.starts_with(&workspace) {
        return Err(ImageInputError::new(
            "图片路径越界：解析后的位置不在工作区内 \
             (image path must resolve within the workspace and cannot escape it)",
        ));
    }
    Ok(resolved)
}

/// Detect the real image MIME type from magic bytes. Returns `None` when the
/// header does not match any supported format (PNG/JPEG/GIF/WebP/BMP).
#[must_use]
pub fn sniff_image_mime(header: &[u8]) -> Option<&'static str> {
    if header.starts_with(b"\x89PNG\r\n\x1a\n") {
        return Some("image/png");
    }
    if header.starts_with(b"\xff\xd8\xff") {
        return Some("image/jpeg");
    }
    if header.starts_with(b"GIF87a") || header.starts_with(b"GIF89a") {
        return Some("image/gif");
    }
    if header.len() >= 12 && header.starts_with(b"RIFF") && &header[8..12] == b"WEBP" {
        return Some("image/webp");
    }
    if header.starts_with(b"BM") {
        return Some("image/bmp");
    }
    None
}

/// Normalize a declared MIME type for comparison (lowercase, trimmed, common
/// alias folding).
fn normalize_mime(mime: &str) -> String {
    let lowered = mime.trim().to_ascii_lowercase();
    match lowered.as_str() {
        "image/jpg" | "image/pjpeg" => "image/jpeg".to_string(),
        other => other.to_string(),
    }
}

fn format_size_limit() -> String {
    format!("{} MB", MAX_IMAGE_BYTES / (1024 * 1024))
}

fn check_image_bytes(declared_mime: &str, bytes: &[u8]) -> Result<(), ImageInputError> {
    let sniffed = sniff_image_mime(bytes).ok_or_else(|| {
        ImageInputError::new(
            "无法识别的图片格式：文件头不是支持的图片类型，已拒绝发送 \
             (unsupported image format: unrecognized file signature)",
        )
    })?;
    if normalize_mime(declared_mime) != sniffed {
        return Err(ImageInputError::new(format!(
            "图片真实格式（{sniffed}）与声明类型（{declared_mime}）不符，已拒绝发送 \
             (declared MIME type does not match the real file signature)"
        )));
    }
    Ok(())
}

/// Read an already-resolved image file with the size cap enforced both before
/// and after the read, detecting the real MIME type from magic bytes.
pub async fn read_image_sniffed(
    resolved_path: &Path,
) -> Result<(Vec<u8>, &'static str), ImageInputError> {
    let metadata = tokio::fs::metadata(resolved_path).await.map_err(|e| {
        if e.kind() == std::io::ErrorKind::NotFound {
            ImageInputError::new(format!(
                "原图片文件已不存在，无法发送 (the original image file no longer exists): {}",
                resolved_path.display()
            ))
        } else {
            ImageInputError::new(format!(
                "无法读取图片文件信息 (failed to stat image file): {e}"
            ))
        }
    })?;
    if metadata.len() > MAX_IMAGE_BYTES {
        return Err(ImageInputError::new(format!(
            "图片超过大小上限：{} 字节，单张图片最大允许 {} (image exceeds the maximum allowed size)",
            metadata.len(),
            format_size_limit(),
        )));
    }

    let bytes = tokio::fs::read(resolved_path).await.map_err(|e| {
        ImageInputError::new(format!(
            "无法读取图片文件 (failed to read image file): {e}"
        ))
    })?;
    // Re-check after the read: the file may have grown between stat and read.
    if bytes.len() as u64 > MAX_IMAGE_BYTES {
        return Err(ImageInputError::new(format!(
            "图片超过大小上限：{} 字节，单张图片最大允许 {} (image exceeds the maximum allowed size)",
            bytes.len(),
            format_size_limit(),
        )));
    }

    let sniffed = sniff_image_mime(&bytes).ok_or_else(|| {
        ImageInputError::new(
            "无法识别的图片格式：文件头不是支持的图片类型，已拒绝发送 \
             (unsupported image format: unrecognized file signature)",
        )
    })?;
    Ok((bytes, sniffed))
}

/// Read an already-resolved image file with the size cap enforced, then
/// verify the real signature against `declared_mime`. Returns the bytes and
/// the canonical sniffed MIME type.
pub async fn read_validated_image(
    resolved_path: &Path,
    declared_mime: &str,
) -> Result<(Vec<u8>, &'static str), ImageInputError> {
    let (bytes, sniffed) = read_image_sniffed(resolved_path).await?;
    if normalize_mime(declared_mime) != sniffed {
        return Err(ImageInputError::new(format!(
            "图片真实格式（{sniffed}）与声明类型（{declared_mime}）不符，已拒绝发送 \
             (declared MIME type does not match the real file signature)"
        )));
    }
    Ok((bytes, sniffed))
}

/// Preflight validation for a workspace-local image reference. Resolves the
/// path inside `workspace`, enforces the size cap, and verifies the real file
/// signature against `declared_mime` without keeping the file contents.
/// Returns the actual byte size for persistence metadata.
pub async fn validate_local_image_reference(
    workspace: &Path,
    relative_path: &Path,
    declared_mime: &str,
) -> Result<u64, ImageInputError> {
    let resolved = resolve_workspace_image_path(workspace, relative_path)?;
    let metadata = tokio::fs::metadata(&resolved)
        .await
        .map_err(|e| ImageInputError::new(format!("无法读取图片文件信息 (failed to stat image file): {e}")))?;
    if metadata.len() > MAX_IMAGE_BYTES {
        return Err(ImageInputError::new(format!(
            "图片超过大小上限：{} 字节，单张图片最大允许 {} (image exceeds the maximum allowed size)",
            metadata.len(),
            format_size_limit(),
        )));
    }

    let mut file = tokio::fs::File::open(&resolved)
        .await
        .map_err(|e| ImageInputError::new(format!("无法读取图片文件 (failed to open image file): {e}")))?;
    let mut header = [0u8; SNIFF_HEADER_BYTES];
    let read = {
        use tokio::io::AsyncReadExt as _;
        file.read(&mut header)
            .await
            .map_err(|e| ImageInputError::new(format!("无法读取图片文件 (failed to read image file): {e}")))?
    };
    check_image_bytes(declared_mime, &header[..read])?;
    Ok(metadata.len())
}

/// Build the OpenAI-compatible `data:` URL for validated image bytes.
#[must_use]
pub fn data_url(mime_type: &str, bytes: &[u8]) -> String {
    format!("data:{mime_type};base64,{}", BASE64.encode(bytes))
}

/// Split a `data:<mime>;base64,<data>` URL into its parts, borrowing from the
/// input so callers never clone the payload. Returns `None` for any other
/// URL shape (e.g. remote `https:` URLs).
#[must_use]
pub fn split_data_url(url: &str) -> Option<(&str, &str)> {
    let rest = url.strip_prefix("data:")?;
    let (mime, data) = rest.split_once(";base64,")?;
    if mime.is_empty() || data.is_empty() {
        return None;
    }
    Some((mime, data))
}

/// Materialize one local image reference into an inline `ImageUrl` block by
/// reading and validating the file fresh from disk.
pub async fn materialize_local_image(
    workspace: &Path,
    relative_path: &Path,
    declared_mime: &str,
) -> Result<ContentBlock, ImageInputError> {
    let resolved = resolve_workspace_image_path(workspace, relative_path)?;
    // The data URL carries the canonical sniffed type, not the host's
    // declaration (which may be an alias like `image/jpg`).
    let (bytes, sniffed_mime) = read_validated_image(&resolved, declared_mime).await?;
    Ok(ContentBlock::ImageUrl {
        image_url: ImageUrlContent {
            url: data_url(sniffed_mime, &bytes),
        },
    })
}

/// Replace every [`ContentBlock::LocalImage`] in a cloned request message
/// list with its materialized inline image. Operates in place; on failure the
/// caller must abort the request — never fall back to dropping the image.
///
/// Re-runs on every request build, which is what makes retries and resumed
/// sessions re-read the file from its relative path and report a clear error
/// when the original image is gone.
pub async fn materialize_messages_local_images(
    messages: &mut [Message],
    workspace: &Path,
) -> Result<(), ImageInputError> {
    for message in messages.iter_mut() {
        if !message
            .content
            .iter()
            .any(|block| matches!(block, ContentBlock::LocalImage { .. }))
        {
            continue;
        }
        for block in message.content.iter_mut() {
            if let ContentBlock::LocalImage {
                relative_path,
                mime_type,
                display_name,
                ..
            } = block
            {
                let materialized = materialize_local_image(workspace, relative_path, mime_type)
                    .await
                    .map_err(|e| ImageInputError::new(format!("{e}（图片：{display_name}）")))?;
                *block = materialized;
            }
        }
    }
    Ok(())
}

/// Placeholder for transcript/debug rendering of an image reference, e.g.
/// `[image:image/jpeg,106138 bytes]`. Never includes URLs or Base64.
#[must_use]
pub fn image_placeholder(mime_type: &str, byte_size: u64) -> String {
    format!("[image:{mime_type},{byte_size} bytes]")
}

/// Placeholder for an already-materialized inline image block, deriving MIME
/// and approximate byte size from the data URL without copying the payload.
#[must_use]
pub fn data_url_placeholder(url: &str) -> String {
    match split_data_url(url) {
        Some((mime, data)) => {
            let approx_bytes = (data.len() / 4) * 3;
            image_placeholder(mime, approx_bytes as u64)
        }
        None => "[image:remote-url]".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PNG_HEADER: [u8; 8] = [0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A];

    #[test]
    fn sniff_detects_supported_signatures() {
        assert_eq!(sniff_image_mime(&PNG_HEADER), Some("image/png"));
        assert_eq!(sniff_image_mime(b"\xff\xd8\xff\xe0rest"), Some("image/jpeg"));
        assert_eq!(sniff_image_mime(b"GIF87a...."), Some("image/gif"));
        assert_eq!(sniff_image_mime(b"GIF89a...."), Some("image/gif"));
        assert_eq!(
            sniff_image_mime(b"RIFF\x04\x00\x00\x00WEBPvp8 "),
            Some("image/webp")
        );
        assert_eq!(sniff_image_mime(b"BM6\x00"), Some("image/bmp"));
    }

    #[test]
    fn sniff_rejects_unknown_and_truncated_headers() {
        assert_eq!(sniff_image_mime(b"plain text"), None);
        assert_eq!(sniff_image_mime(b""), None);
        // WEBP requires the full 12-byte signature.
        assert_eq!(sniff_image_mime(b"RIFF\x04\x00\x00\x00"), None);
        assert_eq!(sniff_image_mime(b"RIFF\x04\x00\x00\x00WAVE"), None);
    }

    #[test]
    fn split_data_url_parses_canonical_shape() {
        let (mime, data) =
            split_data_url("data:image/png;base64,iVBORw0K").expect("valid data url");
        assert_eq!(mime, "image/png");
        assert_eq!(data, "iVBORw0K");
        assert!(split_data_url("https://example.com/x.png").is_none());
        assert!(split_data_url("data:;base64,AAAA").is_none());
        assert!(split_data_url("data:image/png;base64,").is_none());
    }

    #[test]
    fn placeholders_never_contain_payload() {
        assert_eq!(image_placeholder("image/jpeg", 106_138), "[image:image/jpeg,106138 bytes]");
        assert_eq!(
            data_url_placeholder("data:image/png;base64,AAAAAAAA"),
            "[image:image/png,6 bytes]"
        );
        assert_eq!(
            data_url_placeholder("https://example.com/x.png"),
            "[image:remote-url]"
        );
    }

    #[tokio::test]
    async fn materialize_uses_canonical_sniffed_mime_for_aliases() {
        let tmp = tempfile::tempdir().expect("tempdir");
        let attachments = tmp.path().join("attachments");
        std::fs::create_dir_all(&attachments).expect("mkdir");
        std::fs::write(attachments.join("photo.jpg"), [0xFF, 0xD8, 0xFF, 0xE0, 0x01])
            .expect("write jpeg bytes");

        let block = materialize_local_image(
            tmp.path(),
            Path::new("attachments/photo.jpg"),
            "image/jpg", // alias declaration
        )
        .await
        .expect("alias declaration matches sniffed jpeg");

        let ContentBlock::ImageUrl { image_url } = block else {
            panic!("expected materialized ImageUrl block");
        };
        assert!(
            image_url.url.starts_with("data:image/jpeg;base64,"),
            "canonical sniffed MIME must win over the alias; got {}",
            image_url.url
        );
    }

    #[tokio::test]
    async fn materialize_rejects_workspace_escape_and_missing_file() {
        let tmp = tempfile::tempdir().expect("tempdir");

        let err = materialize_local_image(tmp.path(), Path::new("../escape.png"), "image/png")
            .await
            .expect_err("parent-dir escape must reject");
        assert!(err.to_string().contains("越界"), "{err}");

        let err = materialize_local_image(tmp.path(), Path::new("gone.png"), "image/png")
            .await
            .expect_err("missing file must reject");
        assert!(err.to_string().contains("已不存在"), "{err}");
    }
}
