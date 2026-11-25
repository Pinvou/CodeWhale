//! A bundled artifact is a candidate only; discovery never installs or starts it.
use serde_json::{Value, json};
use std::path::Path;

pub fn bundled_package() -> Result<Value, String> {
    let Some(root) = crate::platform::paths::runtime_resource_dir() else {
        return Ok(json!({"available":false,"reason":"bundled_package_missing"}));
    };
    inspect_at(&root)
}

fn inspect_at(root: &Path) -> Result<Value, String> {
    let directory = root.join("runtime/cmsd");
    match std::fs::symlink_metadata(&directory) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(json!({"available":false,"reason":"bundled_package_missing"}));
        }
        Err(error) => return Err(error.to_string()),
        Ok(_) => {}
    }
    let mut value = super::inspect_package(directory.to_str().ok_or("invalid_package_path")?)?;
    value["available"] = json!(true);
    value["packagePath"] = json!(directory);
    value["source"] = json!("bundled-with-pinvou");
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_bundle_is_explicit_and_creates_no_resource_or_profile() {
        let root = tempfile::tempdir().unwrap();
        let result = inspect_at(root.path()).unwrap();
        assert_eq!(result["available"], false);
        assert_eq!(std::fs::read_dir(root.path()).unwrap().count(), 0);
    }

    #[test]
    fn existing_bad_bundle_is_an_error_not_a_missing_service() {
        let root = tempfile::tempdir().unwrap();
        let directory = root.path().join("runtime/cmsd");
        std::fs::create_dir_all(directory).unwrap();
        assert!(inspect_at(root.path()).is_err());
    }

    #[test]
    fn linked_bundle_cannot_bypass_package_verification() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir(root.path().join("runtime")).unwrap();
        let external = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(external.path(), root.path().join("runtime/cmsd")).unwrap();
        assert!(inspect_at(root.path()).is_err());
    }
}
