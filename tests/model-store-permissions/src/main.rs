#![allow(dead_code)]
extern crate self as codewhale_secrets;
// Compilation seam only; directory checks never access the system keyring.
pub struct DefaultKeyringStore;
pub trait KeyringStore {
    fn get(&self, _: &str) -> std::io::Result<Option<String>> {
        panic!("keyring not in scope")
    }
    fn set(&self, _: &str, _: &str) -> std::io::Result<()> {
        panic!("keyring not in scope")
    }
    fn delete(&self, _: &str) -> std::io::Result<()> {
        panic!("keyring not in scope")
    }
}
impl DefaultKeyringStore {
    pub fn new(_: &str) -> Self {
        Self
    }
}
impl KeyringStore for DefaultKeyringStore {}

#[path = "../../../pinvou3-app/src-tauri/src/platform/filesystem.rs"]
mod actual_filesystem;
#[path = "../../../pinvou3-app/src-tauri/src/platform/paths.rs"]
mod actual_paths;
mod platform {
    pub(crate) use crate::actual_filesystem as filesystem;
    pub(crate) use crate::actual_paths as paths;
    pub mod os {
        pub fn user_home_dir() -> std::path::PathBuf {
            std::env::var_os("HOME").unwrap().into()
        }
        pub fn platform_compat_path(path: &str) -> std::path::PathBuf {
            path.into()
        }
        pub fn python_command() -> String {
            panic!("not in scope")
        }
    }
}
#[path = "../../../pinvou3-app/src-tauri/src/features/model_store/platform/linux/config.rs"]
mod config;

use std::collections::BTreeMap;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
#[derive(Debug)]
struct ModelManagerError(String);
impl ModelManagerError {
    fn new(s: impl Into<String>) -> Self {
        Self(s.into())
    }
}
mod script_runner {
    use super::{ModelManagerError as ScriptError, Path, PathBuf};
    use std::os::unix::fs::PermissionsExt;
    include!(concat!(env!("OUT_DIR"), "/assets.rs"));
}
struct Blob {
    content: String,
}
enum ScriptsSource {
    Local {
        blobs: BTreeMap<String, Blob>,
        assets: BTreeMap<String, String>,
    },
    Unavailable,
}
struct PreparedScript {
    entry: PathBuf,
    cwd: Option<PathBuf>,
}
impl ScriptsSource {
    fn has_assets(&self) -> bool {
        true
    }
    fn fetch_policy(&self, _: &str) {}
    fn materialize_entry(&self, _: &str, _: ()) -> Result<PathBuf, ModelManagerError> {
        panic!("single-file path is not this test")
    }
    fn asset_contents(&self, _: ()) -> Result<BTreeMap<String, String>, ModelManagerError> {
        match self {
            Self::Local { assets, .. } => Ok(assets.clone()),
            _ => panic!("no network"),
        }
    }
    fn entry_content(&self, kind: &str, _: ()) -> Result<String, ModelManagerError> {
        match self {
            Self::Local { blobs, .. } => Ok(blobs[kind].content.clone()),
            _ => panic!("no network"),
        }
    }
}
include!(concat!(env!("OUT_DIR"), "/prepare.rs"));

fn mode(path: &Path) -> u32 {
    path.symlink_metadata().unwrap().permissions().mode() & 0o777
}
fn source() -> ScriptsSource {
    ScriptsSource::Local {
        blobs: [(
            "deploy".into(),
            Blob {
                content: "#!/bin/bash\nexit 0\n".into(),
            },
        )]
        .into(),
        assets: [
            ("common.sh".into(), "# fixture data; never executed".into()),
            ("nested/a/data.json".into(), "{}".into()),
        ]
        .into(),
    }
}
fn main() {
    let mask = u32::from_str_radix(&std::env::args().nth(1).unwrap(), 8).unwrap();
    let temp = tempfile::Builder::new()
        .permissions(std::fs::Permissions::from_mode(0o700))
        .tempdir()
        .unwrap();
    unsafe {
        std::env::set_var("HOME", temp.path());
        std::env::remove_var("PINVOU3_HOME");
    }
    let settings = config::load_settings();
    let root = config::data_dir();
    let staging = settings.staging_path();
    let models = settings.models_path();
    for p in [
        temp.path().join(".pinvou3"),
        root.clone(),
        staging.clone(),
        models.clone(),
    ] {
        assert_eq!(mode(&p), 0o700, "{}", p.display());
    }
    let generic = source().prepare("generic", "deploy").unwrap().cwd.unwrap();
    assert!(!generic.join(".runtime").exists());
    assert_eq!(mode(&generic), 0o700);
    assert_eq!(mode(&root.join("cards")), 0o700);
    let mut native = source();
    if let ScriptsSource::Local { assets, .. } = &mut native {
        assets.insert(
            "native_engine_process.py".into(),
            "# fixture; never executed".into(),
        );
    }
    let card = native.prepare("native", "deploy").unwrap().cwd.unwrap();
    let runtime = card.join(".runtime");
    assert_eq!(mode(&runtime), 0o700);
    assert_eq!(std::fs::read_dir(&runtime).unwrap().count(), 0);
    println!(
        "PASS actual config/prepare fresh700; native runtime700; generic no runtime mask={mask:04o}"
    );

    for historical in [0o755, 0o775] {
        for p in [&root, &staging, &models, &root.join("cards"), &runtime] {
            std::fs::set_permissions(p, std::fs::Permissions::from_mode(historical)).unwrap();
        }
        std::fs::write(runtime.join("receipt"), "UNTRUSTED KEEP").unwrap();
        assert_eq!(config::data_dir(), root);
        settings.staging_path();
        settings.models_path();
        native.prepare("native", "deploy").unwrap();
        for p in [&root, &staging, &models, &root.join("cards"), &runtime] {
            assert_eq!(mode(p), historical, "initializer changed {}", p.display());
        }
        assert_eq!(
            std::fs::read_to_string(runtime.join("receipt")).unwrap(),
            "UNTRUSTED KEEP"
        );
        println!(
            "PASS existing{historical:o} config/parents/runtime unchanged; no receipt adoption"
        );
    }
    // Original asset writer behavior is intentionally NOT changed by this patch.
    std::fs::set_permissions(&card, std::fs::Permissions::from_mode(0o775)).unwrap();
    native.prepare("native", "deploy").unwrap();
    assert_eq!(
        mode(&card),
        0o700,
        "existing shared writer still chmods card directory"
    );
    assert_eq!(mode(&runtime), 0o775, "runtime is not an asset directory");
    println!("PASS original shared writer chmod behavior explicitly retained");
    let probe = temp.path().join("umask-probe");
    std::fs::create_dir(&probe).unwrap();
    assert_eq!(mode(&probe), 0o777 & !mask);
    println!("PASS global umask unchanged");
}
