// Only the capability projection is a host seam. Ledger file operations and
// private anchored directories use the actual product OS implementation.
pub(crate) fn desktop_control_supported() -> bool {
    crate::features::remote_control::desktop_control_supported()
}

#[cfg(test)]
#[path = "../../../src/platform/filesystem.rs"]
pub(crate) mod filesystem;

// Link the same filesystem-only test primitives as the full application.
// This is not a copied refusal algorithm or a fake permission/FIFO result.
// 挂载保持在 platform 模块的真实目录层级（虚拟目录内 #[path] 不能穿越，
// 见 legacy_relay.rs 顶部注释），由下方包装模块 re-export 维持
// `crate::platform::test_support::*` 的既有平面路径。
#[cfg(test)]
#[path = "../../../src/platform/test_support/filesystem.rs"]
pub(crate) mod test_support_filesystem;

#[cfg(test)]
pub(crate) mod test_support {
    pub(crate) use super::test_support_filesystem::*;

    // 回流形态适配（community #876）：relay_client 的载荷钉经
    // `crate::platform::test_support::comment_filtered` 消费共享注释过滤
    // 原语；产品实现在 src/platform/test_support.rs（依赖面大，无法整文件
    // 挂载），此处按产品实现逐字镜像这两个自包含字符串函数。漂移由产品
    // 树内的 stripper 钉（web/relay 套件与 relay.test.js 的嵌套深度钉）
    // 看护，与本文件 TERMINAL_REGISTRATION_ERROR_CODES 桩同一纪律。
    pub(crate) fn comment_filtered(source: &str) -> String {
        let without_line_comments = source
            .lines()
            .filter(|line| {
                let trimmed = line.trim_start();
                !(trimmed.starts_with("//") || trimmed.starts_with("///"))
            })
            .collect::<Vec<_>>()
            .join("\n");
        strip_closed_block_comments(&without_line_comments)
    }

    fn strip_closed_block_comments(src: &str) -> String {
        let mut out = String::with_capacity(src.len());
        let mut rest = src;
        while let Some(open) = rest.find("/*") {
            let after_open = &rest[open + 2..];
            let mut depth = 1usize;
            let mut scan = 0usize;
            let mut close_end = None;
            while depth > 0 {
                let next_open = after_open[scan..].find("/*").map(|rel| scan + rel);
                let next_close = after_open[scan..].find("*/").map(|rel| scan + rel);
                match (next_open, next_close) {
                    (Some(open_at), Some(close_at)) if open_at < close_at => {
                        depth += 1;
                        scan = open_at + 2;
                    }
                    (_, Some(close_at)) => {
                        depth -= 1;
                        if depth == 0 {
                            close_end = Some(close_at + 2);
                        } else {
                            scan = close_at + 2;
                        }
                    }
                    (Some(open_at), None) => {
                        depth += 1;
                        scan = open_at + 2;
                    }
                    (None, None) => break,
                }
            }
            match close_end {
                Some(end) => {
                    out.push_str(&rest[..open]);
                    let inner = &after_open[..end - 2];
                    for _ in 0..inner.matches('\n').count() {
                        out.push('\n');
                    }
                    rest = &after_open[end..];
                }
                None => {
                    // Unterminated block: keep it verbatim, scan past the marker.
                    out.push_str(&rest[..open + 2]);
                    rest = after_open;
                }
            }
        }
        out.push_str(rest);
        out
    }
}

#[cfg(test)]
pub(crate) mod paths {
    pub(crate) mod tests {
        #[cfg(unix)]
        pub(crate) use crate::platform::test_support::plant_fifo;
    }
}

pub(crate) use crate::features::remote_control::HarnessWorkspaceIdentity as WorkspaceIdentity;
pub(crate) use crate::features::remote_control::{test_workspace_identity, workspace_identity};
#[path = "../../../src/platform/encoding.rs"]
pub(crate) mod encoding;

// Host seam used only when actual Windows filesystem tests invoke PowerShell:
// convert canonical Windows prefixes to shell paths, not a production OS-policy test.
pub(crate) mod os {
    pub(crate) fn platform_compat_path(value: &str) -> std::path::PathBuf {
        if let Some(rest) = value.strip_prefix(r"\\?\UNC\") {
            return std::path::PathBuf::from(format!(r"\\{rest}"));
        }
        std::path::PathBuf::from(value.strip_prefix(r"\\?\").unwrap_or(value))
    }
}
