//! Shared attachment ingestion budget, including remote upload admission.
/// 单文件硬上限 50 MB。原始体积与上下文占用解耦:进 prompt 的部分由内联预算
/// (`assistant::attachments::ATTACH_INLINE_MAX_TOKENS`)按**解析后文本**封顶,
/// 超限附件走「落盘路径 + 预览 + `read` 分页」,故该上限只需覆盖传输与子进程
/// 转换(LibreOffice/pandoc/pdftotext,各有 kill-tree 超时)的资源预算。
/// 典型图片型 PPT/PPTX 体积来自嵌入图片,常超旧 20 MB 上限而文本量很小
/// (community PR #894 / issue #842),故提到 50 MB;解压类风险仍由压缩包独立上限约束。
/// `pub` 供 remote_control 上传链路对齐硬上限(PR #213 审查 #5:mobile upload cap 必须
/// ≤ 此值,否则超限文件会被 mobile 接受但 file_ingest 静默降级为 oversize 兜底)。
pub const MAX_FILE_BYTES: u64 = 50 * 1024 * 1024;
