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
            ".venv/lib/module.py",
            "../.cache/file",
            "/.cache/file",
            "C:/vendor/.cache/file",
            "vendor/.cache/invalid\0file",
        ] {
            assert!(!is_runtime_cache(path), "{path}");
        }
    }
}
