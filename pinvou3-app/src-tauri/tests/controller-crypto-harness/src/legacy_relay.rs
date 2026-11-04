// Use a real parent module file: an inline module's virtual directory need not
// exist in a clean POSIX checkout, so #[path] cannot safely traverse through it.
pub(crate) use crate::legacy_protocol as protocol;

#[path = "../../../src/features/remote_control/relay_client.rs"]
pub(crate) mod relay_client;

pub(crate) use crate::features::remote_control::controller;

pub(crate) mod manager {
    // Harness fixture: 镜像 remote_control/manager 的 TERMINAL_REGISTRATION_ERROR_CODES，
    // 让 relay_client 的终端注册对齐测试无需链接 manager 全量依赖即可编译。
    // 真实清单与 relay_client 终结臂的对齐由产品树内的
    // terminal_registration_code_lists_stay_aligned 钉住。
    pub(crate) const TERMINAL_REGISTRATION_ERROR_CODES: &[&str] = &[
        "endpoint_revoked",
        "invalid_access_token",
        "invalid_desktop_secret",
        "bad_desktop_endpoint_register",
        "unsupported_protocol_version",
    ];
}
