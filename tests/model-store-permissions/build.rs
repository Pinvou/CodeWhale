use std::{env, fs, path::PathBuf};
fn section(text: &str, start: &str, end: &str) -> String {
    let (_, rest) = text
        .split_once(start)
        .expect("production start marker changed");
    let (body, _) = rest.split_once(end).expect("production end marker changed");
    format!("{start}{body}")
}
fn main() {
    let root = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap()).join("../..");
    let linux = root.join("pinvou3-app/src-tauri/src/features/model_store/platform/linux");
    for name in ["lifecycle.rs", "script_runner.rs", "card_assets.rs", "native_runtime.rs"] {
        println!("cargo:rerun-if-changed={}", linux.join(name).display());
    }
    // Compile production bodies verbatim, not a rewritten permission model.
    // Surrounding fixture adapter supplies only local asset data; no fetch/exec.
    let lifecycle = fs::read_to_string(linux.join("lifecycle.rs")).unwrap();
    let prepare = section(&lifecycle, "    fn prepare(", "    /// 单文件形态");
    let runner = fs::read_to_string(linux.join("script_runner.rs")).unwrap();
    let assets = section(
        &runner,
        "pub fn materialize_assets(",
        "/// 绝不能出现在提权进程",
    );
    let sanitize = section(&runner, "fn sanitize_relative_path(", "/// 回收子进程");
    // r36 P1-1：prepare 正文经 main 合并引入两个 super:: 接缝（card_assets /
    // native_runtime，均为纯文件系统逻辑）。两个函数体同样逐字提取作编译缝，
    // 仅做两处机械替换：
    //   card_assets 的 super::script_runner::sanitize_relative_path
    //     → crate 根 sanitize_relative_path（assets.rs 已含）；
    //   native_runtime 的 nix::libc:: → libc::（本 crate 依赖）。
    // prepare 正文的 super::card_assets / super::native_runtime 前缀改写为
    // crate::（各恰一处，构建期断言，生产标记漂移即炸）。
    let card_assets_src = fs::read_to_string(linux.join("card_assets.rs")).unwrap();
    let is_mutable = section(
        &card_assets_src,
        "pub(super) fn is_mutable_runtime_asset(",
        "\n#[cfg(test)]",
    )
    .replace(
        "super::script_runner::sanitize_relative_path",
        "sanitize_relative_path",
    );
    let native_src = fs::read_to_string(linux.join("native_runtime.rs")).unwrap();
    let checked = section(&native_src, "fn checked_directory(", "\npub(super) fn prepare(")
        .replace("nix::libc::", "libc::");
    let native_prepare = section(&native_src, "pub(super) fn prepare(", "\n#[cfg(test)]")
        .replace("nix::libc::", "libc::");
    let native_prepare = native_prepare.trim_end();
    let seams = format!(
        "mod card_assets {{\n    use super::script_runner::sanitize_relative_path;\n{is_mutable}\n}}\n\nmod native_runtime {{\n    use libc::{{O_DIRECTORY, O_NOFOLLOW}};\n    use std::fs::{{self, File, OpenOptions}};\n    use std::os::unix::fs::{{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt}};\n    use std::path::Path;\n{checked}\n{native_prepare}\n}}\n"
    );
    let out = PathBuf::from(env::var("OUT_DIR").unwrap());
    let prepare_merged = prepare
        .replace("super::card_assets::", "crate::card_assets::")
        .replace("super::native_runtime::", "crate::native_runtime::");
    assert!(prepare_merged.matches("crate::card_assets::").count() == 1);
    assert!(prepare_merged.matches("crate::native_runtime::").count() == 1);
    fs::write(
        out.join("prepare.rs"),
        format!("impl ScriptsSource {{\n{prepare_merged}\n}}"),
    )
    .unwrap();
    // sanitize 提为 pub(crate)：seams 的 card_assets 模块要跨模块引用它
    // （生产里它经 super::script_runner 访问；这里同 crate 直引）。
    let sanitize_pub = sanitize.replacen("fn sanitize_relative_path(", "pub(crate) fn sanitize_relative_path(", 1);
    fs::write(out.join("assets.rs"), format!("{assets}\n{sanitize_pub}")).unwrap();
    fs::write(out.join("seams.rs"), seams).unwrap();
}
