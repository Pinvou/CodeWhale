//! Runtime caches are mutable state, never an offline card's source files.
pub(super) fn is_runtime_cache(path: &str) -> bool {
    // Leave malformed paths to the existing path guard instead of hiding them.
    if path.chars().any(char::is_control) {
        return false;
    }
    let Ok(path) = super::script_runner::sanitize_relative_path(path) else {
        return false;
    };
    path.components().any(|part| {
        matches!(part,
        std::path::Component::Normal(name) if name == ".cache" || name == "__pycache__")
    })
}

/// Preserve live state while replaying an old offline recipe. Explicitly
/// bundled source (including venv code) still belongs to the recipe.
pub(super) fn is_mutable_runtime_asset(path: &str) -> bool {
    // Malformed paths must still reach the materialization path guard.
    if path.chars().any(char::is_control) {
        return false;
    }
    let Ok(path) = super::script_runner::sanitize_relative_path(path) else {
        return false;
    };
    let generated_tree = path.components().any(|part| {
        matches!(part, std::path::Component::Normal(name)
            if matches!(name.to_str(), Some(".runtime" | ".cache" | "__pycache__"))
                || name.to_string_lossy().starts_with(".tmp-"))
    });
    // State lives at the card root (and the known legacy vendor environment),
    // not in arbitrary source subdirectories named logs or containing .env.
    let known_state = path == std::path::Path::new(".env")
        || path == std::path::Path::new(".deploy-env")
        || path == std::path::Path::new("vendor/.env")
        || path == std::path::Path::new("vendor/.deploy-env")
        || path.starts_with("logs");
    let state_file_parent = path
        .parent()
        .is_some_and(|p| p.as_os_str().is_empty() || p == std::path::Path::new("vendor"));
    generated_tree
        || known_state
        || path.extension().and_then(|e| e.to_str()).is_some_and(|e| {
            matches!(e, "pyc" | "pyo")
                || (state_file_parent && matches!(e, "part" | "incomplete" | "log"))
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn only_generated_cache_components_are_excluded() {
        for path in [
            ".cache/log",
            "vendor/.cache/jit.log",
            "pkg/__pycache__/module.pyc",
            "vendor\\.cache\\log",
            "vendor//.cache/file",
            "./vendor/.cache/file",
        ] {
            assert!(is_runtime_cache(path), "{path}");
        }
        for path in [
            "vendor/cache.py",
            "vendor/.cache-tools/helper.py",
            "vendor/logs/helper.py",
            "vendor/pkg/.env",
            "vendor/pkg/fixture.log",
            ".venv/lib/module.py",
            "../.cache/file",
            "/.cache/file",
            "C:/vendor/.cache/file",
            "vendor/.cache/invalid\0file",
        ] {
            assert!(!is_runtime_cache(path), "{path}");
        }
    }

    #[test]
    fn snapshot_replay_preserves_state_without_dropping_declared_source() {
        for path in [
            ".runtime/engine.json",
            ".deploy-env",
            "vendor/.env",
            "vendor/.cache/jit.json",
            "vendor\\__pycache__\\module.pyc",
            "logs/engine.txt",
            ".tmp-download/manifest.json",
            "vendor/download.part",
            "vendor/download.incomplete",
            "vendor/engine.log",
            "vendor/module.pyo",
        ] {
            assert!(is_mutable_runtime_asset(path), "{path}");
        }
        for path in [
            ".venv/lib/module.py",
            "venv/bin/activate",
            "vendor/logs/helper.py",
            "vendor/pkg/.env",
            "vendor/pkg/fixture.log",
            "node_modules/package/index.js",
            "vendor/.cache-tools/helper.py",
            "vendor/cache.py",
            "../.runtime/engine.json",
            "/.deploy-env",
            "C:/vendor/.cache/file",
            "vendor/.cache/invalid\0file",
        ] {
            assert!(!is_mutable_runtime_asset(path), "{path}");
        }
    }
}
