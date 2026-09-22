use std::path::{Path, PathBuf};

fn main() {
    // Bundle system prompt and security hook changes must invalidate the
    // extracted immutable bundle on existing installations.
    let mut hashed = Vec::new();
    for file in [
        "resources/common/bundle/instructions-shared.md",
        "resources/common/bundle/instructions-work.md",
        "resources/common/bundle/instructions-work-bound.md",
        "resources/common/bundle/instructions-code.md",
        "resources/common/bundle/deny_sensitive_paths.sh",
    ] {
        println!("cargo:rerun-if-changed={file}");
        hashed.extend(std::fs::read(file).unwrap_or_else(|_| panic!("{file} must exist")));
    }
    let hash = fnv1a_64(&hashed);
    println!("cargo:rustc-env=BUNDLE_INSTRUCTIONS_HASH={hash:016x}");

    let target_os = std::env::var("CARGO_CFG_TARGET_OS")
        .unwrap_or_else(|_| panic!("target OS is set by Cargo"));
    let target_arch = std::env::var("CARGO_CFG_TARGET_ARCH")
        .unwrap_or_else(|_| panic!("target arch is set by Cargo"));
    let platform_bundle = PathBuf::from("resources/platforms")
        .join(target_os)
        .join(target_arch)
        .join("bundle");
    let h3c_cli_hash = hash_dir(Path::new("resources/common/bundle/eip"))
        ^ hash_dir(Path::new("resources/common/bundle/zhidao"))
        ^ hash_dir(&platform_bundle.join("eip"))
        ^ hash_dir(&platform_bundle.join("zhidao"));
    println!("cargo:rustc-env=BUNDLE_H3C_CLI_HASH={h3c_cli_hash:016x}");
    tauri_build::build();

    // ScreenCaptureKit.framework only ships on macOS 12.3+, but the app
    // declares minimumSystemVersion 11.0. The framework dependency must be
    // weak so dyld skips it on older systems instead of refusing to launch;
    // the capture path gates on the runtime OS version and falls back to
    // xcap/CGWindowList there (see computer_use/platform/screen_capture_kit.rs).
    // The bare `rustc-link-arg` instruction already applies to bins, tests
    // and cdylibs; a bins/tests loop on top would pass the flag twice.
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        println!("cargo:rustc-link-arg=-weak_framework");
        println!("cargo:rustc-link-arg=ScreenCaptureKit");
    }

    // tauri-build links OUT_DIR/resource.lib only into application binaries.
    // A library unit-test harness is not a Cargo `test` target, so
    // rustc-link-arg-tests does not reach it. Wrap the generated COFF resource
    // object in an archive that a cfg(test) link attribute can include without
    // adding a duplicate resource to the application binary.
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows")
        && std::env::var("CARGO_CFG_TARGET_ENV").as_deref() == Ok("msvc")
    {
        let out_dir = std::path::PathBuf::from(
            std::env::var_os("OUT_DIR")
                .unwrap_or_else(|| panic!("Cargo must provide OUT_DIR to build.rs")),
        );
        let generated_resource = out_dir.join("resource.lib");
        let test_resource_archive = out_dir.join("pinvou3_lib_test_resource.lib");
        let _ = std::fs::remove_file(&test_resource_archive);

        let target = std::env::var("TARGET")
            .unwrap_or_else(|_| panic!("Cargo must provide TARGET to build.rs"));
        let librarian = cc::windows_registry::find_tool(&target, "lib.exe").unwrap_or_else(|| {
            panic!("MSVC lib.exe is required to archive the Windows test manifest resource")
        });
        let machine = match std::env::var("CARGO_CFG_TARGET_ARCH").as_deref() {
            Ok("x86_64") => "X64",
            Ok("x86") => "X86",
            Ok("aarch64") => "ARM64",
            Ok("arm") => "ARM",
            other => panic!("Unsupported MSVC target architecture for test resources: {other:?}"),
        };
        let output = librarian
            .to_command()
            .arg("/NOLOGO")
            .arg(format!("/MACHINE:{machine}"))
            .arg(format!("/OUT:{}", test_resource_archive.display()))
            .arg(&generated_resource)
            .output()
            .unwrap_or_else(|error| {
                panic!("Failed to run MSVC lib.exe for the Windows test resource: {error}")
            });
        if !output.status.success() {
            panic!(
                "Failed to archive the Windows test manifest resource: {}{}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
        }
        println!("cargo:rustc-link-search=native={}", out_dir.display());
    }
}

fn hash_dir(dir: &Path) -> u64 {
    let mut files = Vec::new();
    collect_files(dir, &mut files);
    files.sort();
    let mut hash = 0xcbf2_9ce4_8422_2325_u64;
    for path in &files {
        println!("cargo:rerun-if-changed={}", path.display());
        hash = fnv1a_step(hash, path.to_string_lossy().as_bytes());
        if let Ok(bytes) = std::fs::read(path) {
            hash = fnv1a_step(hash, &bytes);
        }
    }
    hash
}

fn collect_files(dir: &Path, out: &mut Vec<PathBuf>) {
    let Ok(read_dir) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in read_dir.flatten() {
        let path = entry.path();
        let name = path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("");
        #[allow(clippy::case_sensitive_file_extension_comparisons)]
        if name.to_ascii_lowercase().ends_with(".pyc") || name == "__pycache__" {
            continue;
        }
        if path.is_dir() {
            collect_files(&path, out);
        } else {
            out.push(path);
        }
    }
}

fn fnv1a_64(bytes: &[u8]) -> u64 {
    fnv1a_step(0xcbf2_9ce4_8422_2325_u64, bytes)
}

fn fnv1a_step(mut hash: u64, bytes: &[u8]) -> u64 {
    for byte in bytes {
        hash ^= u64::from(*byte);
        hash = hash.wrapping_mul(0x100_0000_01b3);
    }
    hash
}
