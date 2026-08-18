use std::path::{Path, PathBuf};

fn main() {
    // Bundle system prompt and security hook changes must invalidate the
    // extracted immutable bundle on existing installations.
    let mut hashed = Vec::new();
    for file in [
        "resources/common/bundle/instructions-shared.md",
        "resources/common/bundle/instructions-work.md",
        "resources/common/bundle/instructions-code.md",
        "resources/common/bundle/deny_sensitive_paths.sh",
    ] {
        println!("cargo:rerun-if-changed={file}");
        hashed.extend(std::fs::read(file).unwrap_or_else(|_| panic!("{file} must exist")));
    }
    let hash = fnv1a_64(&hashed);
    println!("cargo:rustc-env=BUNDLE_INSTRUCTIONS_HASH={hash:016x}");

    let target_os = std::env::var("CARGO_CFG_TARGET_OS").expect("target OS is set by Cargo");
    let target_arch = std::env::var("CARGO_CFG_TARGET_ARCH").expect("target arch is set by Cargo");
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
