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
    for name in ["lifecycle.rs", "script_runner.rs"] {
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
    let out = PathBuf::from(env::var("OUT_DIR").unwrap());
    fs::write(
        out.join("prepare.rs"),
        format!("impl ScriptsSource {{\n{prepare}\n}}"),
    )
    .unwrap();
    fs::write(out.join("assets.rs"), format!("{assets}\n{sanitize}")).unwrap();
}
