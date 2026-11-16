//! Shared production file-ingest core. Host format adapters remain in the parent module.
use super::{
    ATTACHMENT_ARCHIVE_EXPANDED_TOO_LARGE, ATTACHMENT_ARCHIVE_TOO_MANY_ENTRIES,
    ATTACHMENT_ARCHIVE_UNSAFE_ENTRY, ATTACHMENT_FILE_TOO_LARGE, AttachmentReadOwner,
    MAX_FILE_BYTES, ModelReferenceScope, PublishedAttachment,
};
use super::{
    ingest_archive, ingest_diagnostics, ingest_email, ingest_office, ingest_pdf, text_decode,
    visual_preview,
};
use serde::{Deserialize, Serialize};
use std::path::Path;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(in crate::features::files) enum AttachmentLimitViolation {
    FileTooLarge {
        byte_size: u64,
        max_bytes: u64,
    },
    ArchiveTooManyEntries {
        archive_byte_size: u64,
        entries: usize,
        max_entries: usize,
    },
    ArchiveExpandedTooLarge {
        archive_byte_size: u64,
        expanded_bytes: u64,
        max_bytes: u64,
    },
    ArchiveUnsafeEntry {
        archive_byte_size: u64,
    },
}

impl AttachmentLimitViolation {
    fn wire_code(self) -> &'static str {
        match self {
            Self::FileTooLarge { .. } => ATTACHMENT_FILE_TOO_LARGE,
            Self::ArchiveTooManyEntries { .. } => ATTACHMENT_ARCHIVE_TOO_MANY_ENTRIES,
            Self::ArchiveExpandedTooLarge { .. } => ATTACHMENT_ARCHIVE_EXPANDED_TOO_LARGE,
            Self::ArchiveUnsafeEntry { .. } => ATTACHMENT_ARCHIVE_UNSAFE_ENTRY,
        }
    }

    fn into_warning(self, path: &Path) -> IngestResult {
        let basename = path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("(unnamed)");
        match self {
            Self::FileTooLarge {
                byte_size,
                max_bytes,
            } => IngestResult::warning(
                "oversize",
                basename,
                path,
                byte_size,
                format!(
                    "文件 {:.1} MB 超过 {} MB 上限,请拆分或裁剪",
                    byte_size as f64 / 1024.0 / 1024.0,
                    max_bytes / 1024 / 1024
                ),
            ),
            Self::ArchiveTooManyEntries {
                archive_byte_size,
                entries,
                max_entries,
            } => IngestResult::warning(
                "archive",
                basename,
                path,
                archive_byte_size,
                format!("压缩包条目过多（{entries} > {max_entries}），拒绝展开"),
            ),
            Self::ArchiveExpandedTooLarge {
                archive_byte_size,
                expanded_bytes,
                max_bytes,
            } => IngestResult::warning(
                "archive",
                basename,
                path,
                archive_byte_size,
                format!(
                    "压缩包解压后约 {:.0} MB，超过 {} MB 上限（疑似压缩炸弹），拒绝展开",
                    expanded_bytes as f64 / 1024.0 / 1024.0,
                    max_bytes / 1024 / 1024
                ),
            ),
            Self::ArchiveUnsafeEntry { archive_byte_size } => IngestResult::warning(
                "archive",
                basename,
                path,
                archive_byte_size,
                "压缩包包含链接、重解析点或越界路径，已拒绝读取",
            ),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct IngestResult {
    /// 类型分类：text / pdf / docx / xlsx / image / binary
    pub kind: String,
    /// 文件名（不含路径）
    pub basename: String,
    /// 原始绝对路径（用于发送时构造 prompt 引用）
    pub path: String,
    /// 转换后的 markdown 内容（image/binary 为 None）
    pub markdown: Option<String>,
    /// token 估算值（粗算）。前端用来累加显示「已用 X / Y」
    pub token_estimate: u32,
    /// 原始字节数
    pub byte_size: u64,
    /// 警告或错误消息：超大、缺工具、不支持视觉等。前端 chip 上 ⚠️ 显示。
    pub warning: Option<String>,
    /// Process-only read authority, never accepted from IPC or persisted.
    /// Retained through format parsing, image staging and provider reads.
    #[serde(skip)]
    pub(crate) read_owner: Option<std::sync::Arc<dyn AttachmentReadOwner>>,
}

impl IngestResult {
    pub(crate) fn read_path(&self) -> &Path {
        self.read_owner
            .as_ref()
            .map_or_else(|| Path::new(&self.path), |owner| owner.read_path())
    }
    /// 缺工具/不支持时的占位结果（warning 文案，无 markdown）。
    ///
    /// 字段默认与既有字面量逐字段一致：`markdown: None`、`token_estimate: 0`、
    /// `warning: Some(warning)`，`path` 由 `path.to_string_lossy()` 还原（与各
    /// ingest 函数顶部预计算的 `path_str` 等价）。
    pub fn warning(
        kind: &str,
        basename: &str,
        path: &Path,
        byte_size: u64,
        warning: impl Into<String>,
    ) -> Self {
        IngestResult {
            read_owner: None,
            kind: kind.into(),
            basename: basename.into(),
            path: path.to_string_lossy().into(),
            markdown: None,
            token_estimate: 0,
            byte_size,
            warning: Some(warning.into()),
        }
    }

    /// 已生成 markdown 的正常结果。
    ///
    /// `token_estimate` 由 `estimate_tokens(&markdown)` 推导，`warning: None`。
    /// 仅适用于「成功提取正文且无任何告警」的结果；同时带有正文和告警（如编码转换、
    /// OCR 误差提示）的结果无法用本构造器表达，仍需保留字面量。
    pub fn with_markdown(
        kind: &str,
        basename: &str,
        path: &Path,
        byte_size: u64,
        markdown: String,
    ) -> Self {
        let token_estimate = estimate_tokens(&markdown);
        IngestResult {
            read_owner: None,
            kind: kind.into(),
            basename: basename.into(),
            path: path.to_string_lossy().into(),
            markdown: Some(markdown),
            token_estimate,
            byte_size,
            warning: None,
        }
    }

    /// 仅占位、无内容无警告（极少用）。`markdown: None`、`token_estimate: 0`、
    /// `warning: None`。典型用例：图片附件登记元数据（kind="image"）。
    pub fn placeholder(kind: &str, basename: &str, path: &Path, byte_size: u64) -> Self {
        IngestResult {
            read_owner: None,
            kind: kind.into(),
            basename: basename.into(),
            path: path.to_string_lossy().into(),
            markdown: None,
            token_estimate: 0,
            byte_size,
            warning: None,
        }
    }
}

/// 粗算 token：中英混合按 `chars / 1.6` —— 比较保守，偏向高估避免炸上下文。
/// 实测 cl100k_base 中文 1.0-1.5 char/token、英文 3-4 char/token。
pub(super) fn estimate_tokens(text: &str) -> u32 {
    let chars = text.chars().count();
    (chars as f64 / 1.6).ceil() as u32
}

/// Generic ingest keeps limit failures as warning placeholders for non-chat
/// callers such as knowledge indexing. Chat attachment entry points must use
/// [`ingest_attachment`] so a rejected file never becomes a sendable chip.
pub fn ingest(path: &Path) -> IngestResult {
    ingest_checked(path).unwrap_or_else(|violation| violation.into_warning(path))
}

/// Ingest a user-selected chat attachment and reject hard size/archive limits
/// with stable wire codes that the three UI languages can render locally.
pub fn ingest_attachment(path: &Path) -> Result<IngestResult, String> {
    ingest_checked(path).map_err(|violation| violation.wire_code().to_string())
}

/// Accept one B-local, already authenticated upload receipt for the existing
/// chat/ACP attachment pipeline. No remote-control type or A-supplied path is
/// admitted here: callers supply the authoritative session's private root and
/// the path returned by the B staging owner.
pub(crate) fn ingest_staged_attachment(
    session_root: &Path,
    attachment: &PublishedAttachment,
) -> Result<IngestResult, String> {
    use sha2::{Digest, Sha256};

    let staged_path = &attachment.path;
    let display_name = attachment.display_name.as_str();
    let expected_bytes = attachment.bytes;
    let expected_sha256 = attachment.sha256.as_str();

    let mut components = Path::new(display_name).components();
    if display_name.is_empty()
        || display_name.contains(['/', '\\'])
        || display_name.chars().any(char::is_control)
        || display_name.as_bytes().get(1) == Some(&b':')
        || staged_path.file_name() != Some(std::ffi::OsStr::new(display_name))
        || !matches!(components.next(), Some(std::path::Component::Normal(_)))
        || components.next().is_some()
        || expected_bytes > MAX_FILE_BYTES
        || expected_sha256.len() != 64
        || !expected_sha256
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err("task_attachment_invalid".to_owned());
    }
    let owner = attachment
        .read_owner
        .as_ref()
        .ok_or_else(|| "task_attachment_unavailable".to_owned())?;
    let canonical_session = std::fs::canonicalize(session_root)
        .map_err(|_| "task_attachment_unavailable".to_owned())?;
    let upload_root = canonical_session.join("remote_uploads");
    if !std::fs::symlink_metadata(&upload_root)
        .map_err(|_| "task_attachment_unavailable".to_owned())?
        .file_type()
        .is_dir()
    {
        return Err("task_attachment_invalid".to_owned());
    }
    let canonical_upload_root =
        std::fs::canonicalize(upload_root).map_err(|_| "task_attachment_unavailable".to_owned())?;
    if !canonical_upload_root.starts_with(&canonical_session) {
        return Err("task_attachment_invalid".to_owned());
    }
    if staged_path != owner.path() {
        return Err("task_attachment_invalid".to_owned());
    }
    owner
        .validate_scope(&canonical_upload_root)
        .map_err(attachment_source_error)?;
    let bytes = owner
        .read_bounded(expected_bytes)
        .map_err(attachment_source_error)?;
    if bytes.len() as u64 != expected_bytes {
        return Err("task_attachment_invalid".to_owned());
    }
    let actual_sha256 = Sha256::digest(&bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    if actual_sha256 != expected_sha256 {
        return Err("task_attachment_invalid".to_owned());
    }
    let reference_scope = if owner.read_path() != owner.path() {
        Some(
            ModelReferenceScope::open(&canonical_session)
                .map_err(|_| "task_attachment_unavailable".to_owned())?,
        )
    } else {
        None
    };
    // Reopen only the OS-owned immutable source. A Linux logical pathname is
    // scope metadata, never parser input; formats use the validated display name.
    let mut result =
        ingest_diagnostics::with_private(|| ingest_checked_named(owner.read_path(), display_name))
            .map_err(|_| "task_attachment_invalid".to_owned())?;
    if result.kind == "missing" {
        return Err("task_attachment_unavailable".to_owned());
    }
    result.basename = display_name.to_owned();
    result.path = staged_path.to_string_lossy().into_owned();
    if let Some(scope) = reference_scope {
        // Only parser operations may use ephemeral fd addresses. The model's
        // history can outlive this owner and an fd number can be reused by any
        // unrelated open, so give accepted content a named session copy. Text
        // references contain only the extracted body: an archive's original
        // bytes could otherwise restore members rejected during ingestion.
        let reference = if result.kind == "image" {
            Some((display_name, bytes.as_slice()))
        } else {
            result
                .markdown
                .as_deref()
                .map(|body| ("content.md", body.as_bytes()))
        };
        if let Some((name, content)) = reference {
            result.path = scope
                .persist(name, content)
                .map_err(|_| "task_attachment_unavailable".to_owned())?
                .to_string_lossy()
                .into_owned();
        }
    }
    result.read_owner = Some(owner.clone());
    Ok(result)
}

fn attachment_source_error(error: std::io::Error) -> String {
    match error.kind() {
        std::io::ErrorKind::InvalidInput | std::io::ErrorKind::InvalidData => {
            "task_attachment_invalid"
        }
        _ => "task_attachment_unavailable",
    }
    .to_owned()
}

pub(super) fn ingest_checked(path: &Path) -> Result<IngestResult, AttachmentLimitViolation> {
    let basename = path
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("(unnamed)")
        .to_string();
    ingest_checked_named(path, &basename)
}

fn ingest_checked_named(
    path: &Path,
    display_name: &str,
) -> Result<IngestResult, AttachmentLimitViolation> {
    let basename = display_name.to_owned();
    let path_str = path.to_string_lossy().to_string();
    let meta = match std::fs::metadata(path) {
        Ok(m) => m,
        Err(e) => {
            return Ok(IngestResult::warning(
                "missing",
                &basename,
                path,
                0,
                format!("文件不存在: {e}"),
            ));
        }
    };
    let byte_size = meta.len();
    if byte_size > MAX_FILE_BYTES {
        return Err(AttachmentLimitViolation::FileTooLarge {
            byte_size,
            max_bytes: MAX_FILE_BYTES,
        });
    }

    let ext = Path::new(display_name)
        .extension()
        .and_then(|e| e.to_str())
        .map(|s| s.to_ascii_lowercase())
        .unwrap_or_default();
    let kind = classify(&ext);

    let result = match kind {
        "text" => text_decode::ingest_text(path, basename, path_str, byte_size),
        "pdf" => ingest_pdf::ingest_pdf(path, basename, path_str, byte_size),
        // 文字文档：pandoc 原生支持 docx/odt。
        "doc_pandoc" => ingest_office::ingest_pandoc(path, basename, byte_size, &ext),
        // 文字文档：pandoc 吃不下，LibreOffice 转纯文本（doc/rtf/wps）。
        "doc_office" => ingest_office::ingest_office_text(path, basename, byte_size, &ext),
        // 演示：LibreOffice 转 PDF 再 pdftotext（pptx/ppt/odp/dps）。
        "presentation" => ingest_office::ingest_presentation(path, basename, byte_size, &ext),
        // 表格：LibreOffice 转 CSV（xlsx/ods/xls/et）—— pandoc 不支持表格输入。
        "spreadsheet" => {
            ingest_office::ingest_spreadsheet(path, basename, path_str, byte_size, &ext)
        }
        "image" => visual_preview::ingest_image(path, basename, path_str, byte_size),
        "archive" => return ingest_archive::ingest_archive(path, basename, path_str, byte_size),
        "email" => ingest_email::ingest_email(path, basename, path_str, byte_size, &ext),
        "media" => media_placeholder(basename, path_str, byte_size),
        // 私钥 / 密钥库:绝不读正文(防泄露给 LLM)。见 classify 的 "secret" 分支。
        "secret" => redacted_secret_placeholder(basename, path_str, byte_size),
        // 未知扩展名 / 无扩展名:不再一刀切 binary,先内容嗅探。是文本就当文本读
        // (让模型看到内容 —— 这是「接受任何文件」的根本解),真二进制才降级。
        _ => text_decode::sniff_text_or_binary(path, basename, path_str, byte_size),
    };
    Ok(result)
}

/// 按「文档类型」而非新旧分流：pandoc 只吃 docx/odt，pptx/ppt/xlsx/ods/xls 一律
/// 走 LibreOffice（演示→PDF→pdftotext，表格→CSV）。WPS 三件套按用途归类：
/// .wps→文字、.et→表格、.dps→演示。
pub(super) fn classify(ext: &str) -> &'static str {
    match ext {
        // 安全硬墙(优先级最高):私钥 / 密钥库类扩展名一律不读正文 —— 防止私钥被读进
        // markdown、内联进 prompt、进而发给(可能的云端)LLM。validate_path 只挡 5 个
        // 敏感目录,挡不住 ~/certs/server.key 这类文件,故在此补文件级拒绝。
        // The "secret" class goes to redacted_secret_placeholder (markdown
        // always None) and never reaches sniffing.
        // 注意:公钥证书(crt/cer/csr)本身可公开,仍归 text,方便用户问「证书 CN/到期日」。
        "key" | "pem" | "p12" | "pfx" | "keystore" | "jks" | "kdbx" | "gpg" | "pgp" => "secret",
        // 纯文本:文档 + 结构化数据 + 源码 + Web + 配置 + 公钥证书(均为 UTF-8/文本可读)。
        // 未列出的扩展名由 ingest 末尾 sniff_text_or_binary 内容嗅探兜底,故这里只列
        //「确定是文本」的常见格式,不追求穷举 —— 嗅探会接住遗漏(含用户自定义扩展名、
        // 以及无扩展名的 Makefile/Dockerfile/README —— 这些文件 Path::extension() 返回
        // None,根本不会进 classify,故不在此列「扩展名」)。
        "txt" | "md" | "markdown" | "mdx" | "rst" | "org" | "adoc" | "asciidoc" | "tex"
        | "latex" | "bib" | "text" | "log" | "json" | "jsonl" | "ndjson" | "geojson" | "csv"
        | "tsv" | "yaml" | "yml" | "toml" | "xml" | "proto" | "graphql" | "gql" | "sql" | "ini"
        | "conf" | "cfg" | "properties" | "props" | "env" | "editorconfig" | "gradle" | "cmake"
        | "bazel" | "bzl" | "mk" | "rs" | "py" | "pyi" | "js" | "mjs" | "cjs" | "ts" | "jsx"
        | "tsx" | "go" | "c" | "h" | "cpp" | "cc" | "cxx" | "hpp" | "hh" | "hxx" | "java"
        | "kt" | "kts" | "scala" | "groovy" | "clj" | "cljs" | "edn" | "el" | "lisp" | "scm"
        | "rkt" | "r" | "rb" | "php" | "pl" | "pm" | "lua" | "tcl" | "m" | "mm" | "swift"
        | "dart" | "hs" | "lhs" | "ml" | "mli" | "fs" | "fsx" | "fsi" | "cs" | "vb" | "pas"
        | "d" | "nim" | "zig" | "v" | "jl" | "ex" | "exs" | "erl" | "hrl" | "f" | "f90" | "f95"
        | "f03" | "asm" | "s" | "vhdl" | "sv" | "sh" | "bash" | "zsh" | "fish" | "ps1" | "bat"
        | "cmd" | "html" | "htm" | "xhtml" | "css" | "scss" | "sass" | "less" | "styl" | "vue"
        | "svelte" | "pug" | "hbs" | "ejs" | "twig" | "crt" | "cer" | "csr" | "wsgi" | "rake" => {
            "text"
        }
        "pdf" => "pdf",
        // 文字：pandoc 原生支持
        "docx" | "odt" => "doc_pandoc",
        // 文字：pandoc 不支持 → LibreOffice txt（含 WPS 文字 .wps）
        "doc" | "rtf" | "wps" => "doc_office",
        // 演示：LibreOffice 无 txt 导出 → 转 PDF 再 pdftotext（含 WPS 演示 .dps）
        "pptx" | "ppt" | "odp" | "dps" => "presentation",
        // 表格：pandoc 不支持 xlsx/ods → LibreOffice csv（含 WPS 表格 .et）
        "xlsx" | "ods" | "xls" | "et" => "spreadsheet",
        // 仅底座 image_analyze 支持的位图格式走视觉(vision/tools.rs detect_mime_type)。
        // svg(矢量)/tiff 不在支持列表 → 不归 image(避免被当图暂存后 image_analyze 报
        // "Unsupported image format")。它们经 `_ =>` 落到 sniff 兜底:SVG 是文本 XML,
        // 会被读成 text(模型能看懂结构,无害且有用);TIFF 头含 NUL(II*\0 / MM\0*),
        // sniff 判二进制 → binary_placeholder。
        "png" | "jpg" | "jpeg" | "gif" | "webp" | "bmp" => "image",
        // 压缩包：解压后递归识别（7z 统一处理 zip/rar/7z）
        "zip" | "rar" | "7z" => "archive",
        // 邮件：eml 走 python email 标准库；msg 按 OS 策略解析
        "eml" | "msg" => "email",
        // 音视频：本地语音转录(whisper)尚未部署，先优雅降级标「未处理」
        "mp4" | "avi" | "mov" | "mkv" | "webm" | "flv" | "wmv" | "m4v" | "mp3" | "wav" | "m4a"
        | "aac" | "flac" | "ogg" | "wma" => "media",
        _ => "binary",
    }
}

/// 音视频：本地语音转录（whisper 等）尚未部署到 GB10，先优雅降级，明确告知用户
/// 「未处理」而非臆测内容。真正转录留作未来独立能力（见 process.md）。
fn media_placeholder(basename: String, path_str: String, byte_size: u64) -> IngestResult {
    IngestResult::warning(
        "media",
        &basename,
        Path::new(&path_str),
        byte_size,
        "检测到音视频文件，当前暂不支持本地语音转录。\
             可改为提供文字稿，或口述其中要点。",
    )
}

pub(super) fn binary_placeholder(
    basename: String,
    path_str: String,
    byte_size: u64,
) -> IngestResult {
    IngestResult::warning(
        "binary",
        &basename,
        Path::new(&path_str),
        byte_size,
        "不支持的文件类型(二进制)",
    )
}

/// 私钥 / 密钥库文件的占位:与 binary_placeholder 同为 markdown=None,但文案明确说明
/// 「为防泄露而拒绝读取」,便于用户理解为什么内容没被读入。kind 用 "binary" 以复用
/// 前端既有分类展示(无需新增前端分支),靠 warning 区分原因。
///
/// Naming constraint: the name must carry a sanitized marker such as
/// "redact". The return value flows into the log/failure messages of many
/// test assertions, and CodeQL rust/cleartext-logging picks its taint
/// sources by "name contains secret without a redact/hash-style sanitizer
/// word", so `secret_placeholder` produced ~20 false "cleartext logging of
/// sensitive data" findings on those assertions.
pub(super) fn redacted_secret_placeholder(
    basename: String,
    path_str: String,
    byte_size: u64,
) -> IngestResult {
    IngestResult::warning(
        "binary",
        &basename,
        Path::new(&path_str),
        byte_size,
        "检测到密钥/私钥文件,为防止泄露给 LLM 已拒绝读取内容",
    )
}
