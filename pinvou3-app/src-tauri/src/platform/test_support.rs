//! Test helpers shared across features (every item is `#[cfg(test)]` and
//! never ships in release artifacts).
//!
//! 进程级 env（PINVOU3_HOME / DEEPSEEK_* 等）是 cargo test 并行执行下的隔离
//! 硬约束：所有改写 env 的测试必须先持有 `platform::paths::tests::ENV_LOCK`
//! （crate 唯一 env 锁——本模块不另立锁源，只复用它），并在退出（含 panic
//! 路径）时恢复原值。本模块把「PINVOU3_HOME 指向干净临时目录跑闭包」（
//! `with_temp_home`，叠加 keyring hermeticity 三件套）与「快照/恢复一组 env」（
//! `EnvRestore` / `locked_env`）两个高频脚手架收敛为单一实现，供 assistant /
//! connectors / marketplace / sessions 等各特性测试复用，避免逐文件复制出细微漂移。
//!
//! The module is declared in `platform/mod.rs`
//! (`#[cfg(test)] pub(crate) mod test_support;`) and never ships in release
//! artifacts.

#[cfg(test)]
mod filesystem;

#[cfg(all(test, unix))]
pub(crate) use filesystem::plant_fifo;

use std::ffi::OsString;

/// Runs the closure with PINVOU3_HOME pointed at a clean temp dir, then
/// restores and removes the dir. The caller supplies the dir-name prefix
/// (per feature/case, so different tests under the same pid do not delete
/// each other's dirs), plus the process id and `unique_suffix()` to avoid
/// collisions across parallel rounds. Serializes against other tests that
/// mutate PINVOU3_HOME via `platform::paths::tests::ENV_LOCK`.
#[cfg(test)]
pub(crate) fn with_temp_home(prefix: &str, f: impl FnOnce()) {
    let _g = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    let dir = std::env::temp_dir().join(format!(
        "{prefix}-{}-{}",
        std::process::id(),
        crate::platform::paths::tests::unique_suffix()
    ));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    // Keyring hermeticity rides along — the same trio the marketplace
    // harness installs. A test reaching a real credential write through
    // this harness otherwise lands in the developer's OS keychain, and on
    // macOS the OS probe from an ad-hoc-signed test binary can block
    // indefinitely on the ACL consent dialog. `PINVOU3_TEST_KEYRING_FILE_FALLBACK`
    // exists only in test builds (credential_store reads it before probing
    // the OS keyring); `CODEWHALE_HOME` rides to the temp dir so
    // CodeWhale-side state stays hermetic too. The trio is restored through
    // the shared `EnvRestore`.
    // Declared after `_g` so it drops BEFORE the lock is released (no other
    // test may observe the temporary values), and before the set_var calls
    // below so the snapshot sees the caller's values. Restore runs on Drop —
    // a failing assertion inside the closure unwinds, and a straight-line
    // restore would be skipped, leaking the env AND the temp dir into every
    // later test in this process — with the dir removal as the post_restore
    // tail so it happens after every env restore.
    let _env = EnvRestore::capture_with_post_restore(
        &[
            "PINVOU3_HOME",
            "CODEWHALE_HOME",
            "PINVOU3_TEST_KEYRING_FILE_FALLBACK",
        ],
        {
            let dir = dir.clone();
            move || {
                let _ = std::fs::remove_dir_all(dir);
            }
        },
    );
    // SAFETY: holding platform::paths::tests::ENV_LOCK; env writes serialized in-process.
    unsafe { std::env::set_var("PINVOU3_HOME", &dir) };
    // SAFETY: holding platform::paths::tests::ENV_LOCK; env writes serialized in-process.
    unsafe { std::env::set_var("CODEWHALE_HOME", &dir) };
    // SAFETY: holding platform::paths::tests::ENV_LOCK; env writes serialized in-process.
    unsafe { std::env::set_var("PINVOU3_TEST_KEYRING_FILE_FALLBACK", "1") };
    f();
}

/// Take the crate-unique env lock and snapshot a set of env vars in one step:
/// `let (_lock, _env) = locked_env(&["PINVOU3_HOME"]);`
/// Not reentrant — never call while already holding ENV_LOCK. The guard
/// releases the lock and restores the env when scope exits (including panic
/// unwind). (`bridge.rs`'s test module still keeps a private
/// `locked_env`/`EnvGuard` pair; its ~hundred call sites have not been
/// migrated here — new code always uses this implementation so the old pair
/// stops spreading.)
#[cfg(test)]
pub(crate) fn locked_env(
    vars: &[&'static str],
) -> (std::sync::MutexGuard<'static, ()>, EnvRestore) {
    let lock = crate::platform::paths::tests::ENV_LOCK
        .lock()
        .unwrap_or_else(|p| p.into_inner());
    (lock, EnvRestore::capture(vars))
}

/// Test entry point for the legacy connector-layout bin dir: returns
/// `managed_connector_bin_dir()`; if that is `None`, hard-asserts that the
/// current target platform is genuinely not covered by the lock table.
/// Consumer tests (legacy-layout migration, PATH ordering, etc.) soft-skip
/// based on this — a wrapper regression on a supported platform fails here
/// instead of being silently swallowed by each consumer's
/// `else { return }` (otherwise regressions in PATH injection, legacy
/// migration/residue cleanup, and spawn fallback resolution would all go
/// mute).
#[cfg(test)]
pub(crate) fn managed_connector_bin_dir_or_assert_unsupported() -> Option<std::path::PathBuf> {
    let dir = crate::platform::paths::managed_connector_bin_dir();
    if dir.is_none() {
        assert!(
            crate::platform::paths::connector_platform_dir(
                std::env::consts::OS,
                std::env::consts::ARCH
            )
            .is_none(),
            "managed_connector_bin_dir must be Some on a lock-covered platform (os={}, arch={})",
            std::env::consts::OS,
            std::env::consts::ARCH
        );
    }
    dir
}

/// RAII 快照/恢复一组环境变量：`capture` 记录现值，`Drop`（含 panic 路径）
/// 逐一恢复。调用方测试必须先持有 `platform::paths::tests::ENV_LOCK` 再
/// capture，保证 env 写全程在锁内串行。

#[cfg(test)]
pub(crate) struct EnvRestore {
    saved: Vec<(&'static str, Option<OsString>)>,
    /// One-shot tail action run after env restore completes (e.g. the
    /// multiagent regression test refreshes the personas cache after
    /// restoring PINVOU3_HOME); `None` for ordinary use.
    post_restore: Option<Box<dyn FnOnce() + Send>>,
}

#[cfg(test)]
impl EnvRestore {
    pub(crate) fn capture(names: &[&'static str]) -> Self {
        Self {
            saved: names
                .iter()
                .map(|name| (*name, std::env::var_os(name)))
                .collect(),
            post_restore: None,
        }
    }

    /// Same as [`EnvRestore::capture`], and runs `post_restore` once after
    /// all env vars are restored (restore first, tail action second, so the
    /// tail observes the already-restored environment).
    pub(crate) fn capture_with_post_restore(
        names: &[&'static str],
        post_restore: impl FnOnce() + Send + 'static,
    ) -> Self {
        let mut this = Self::capture(names);
        this.post_restore = Some(Box::new(post_restore));
        this
    }
}

#[cfg(test)]
impl Drop for EnvRestore {
    fn drop(&mut self) {
        for (name, value) in self.saved.drain(..) {
            match value {
                // SAFETY: the caller's test holds platform::paths::tests::ENV_LOCK throughout; env writes are serialized in-process.
                Some(value) => unsafe { std::env::set_var(name, value) },
                // SAFETY: the caller's test holds platform::paths::tests::ENV_LOCK throughout; env writes are serialized in-process.
                None => unsafe { std::env::remove_var(name) },
            }
        }
        if let Some(post_restore) = self.post_restore.take() {
            post_restore();
        }
    }
}

/// Test helper: drop the current user's read permission on `dir` (Unix chmod
/// 000) to simulate an unscannable installed directory (e.g. the marketplace
/// scope's DenyAll default enumeration degradation). Returns `None` when the
/// simulation cannot take effect — non-Unix has no POSIX mode bits, and root
/// is not constrained by 000 — so callers skip the assertions that depend on
/// EACCES semantics (covered on non-Unix-root environments, e.g. the ubuntu CI
/// runner). The returned guard restores 0755 on drop, including panic
/// unwinding, so the temp home stays cleanable even when an assertion fires
/// while the directory is unreadable.
pub(crate) fn make_dir_unreadable_for_test(dir: &std::path::Path) -> Option<UnreadableDirForTest> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        if std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o000)).is_err() {
            return None;
        }
        // Root is not constrained by 000: without an observable EACCES the
        // simulation is ineffective, so restore and report failure.
        if std::fs::read_dir(dir).is_ok() {
            let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755));
            return None;
        }
        Some(UnreadableDirForTest {
            dir: dir.to_path_buf(),
        })
    }
    #[cfg(not(unix))]
    {
        let _ = dir;
        None
    }
}

/// Restore pairing for [`make_dir_unreadable_for_test`]: restores 0755 (test
/// directories are created by `create_dir_all` with default permissions) on
/// drop, in the RAII guard house pattern. Only ever constructed on Unix.
pub(crate) struct UnreadableDirForTest {
    dir: std::path::PathBuf,
}

impl Drop for UnreadableDirForTest {
    fn drop(&mut self) {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let _ = std::fs::set_permissions(&self.dir, std::fs::Permissions::from_mode(0o755));
        }
        #[cfg(not(unix))]
        {
            let _ = &self.dir;
        }
    }
}

/// Root-skip probes for the fail-closed permission fixtures:
/// chmod the target to the denial mode, check the denial actually took
/// effect, and report `false` — restoring the default mode and eprintln-ing
/// the loud, per-test greppable `ROOT-SKIP[<test>]` marker — when it did not
/// (root is not constrained by mode bits), so the caller skips the
/// assertions that depend on EACCES instead of passing vacuously:
/// `if !file_unreadable_or_skip(&path, "test_name") { return; }`. Unix-only:
/// every caller is a `#[cfg(unix)]` test. Deny/restore modes match the
/// fixtures this replaces — files deny with 000 and restore 644, directories
/// deny with 0555 and restore 755.
#[cfg(all(test, unix))]
pub(crate) fn file_unreadable_or_skip(file: &std::path::Path, test: &str) -> bool {
    use std::os::unix::fs::PermissionsExt as _;
    let _ = std::fs::set_permissions(file, std::fs::Permissions::from_mode(0o000));
    // Root is not constrained by 000: with the file still readable the
    // unreadable-read branch would never run — restore and skip loudly.
    if std::fs::read(file).is_ok() {
        let _ = std::fs::set_permissions(file, std::fs::Permissions::from_mode(0o644));
        eprintln!(
            "ROOT-SKIP[{test}]: running as root - the unreadable-file fixture stays readable; NOT exercised"
        );
        return false;
    }
    true
}

/// [`file_unreadable_or_skip`] for a read-only directory: chmod 0555 and
/// probe by creating a file inside it. On `true` the directory stays
/// read-only for the caller's denial assertions; on `false` the fixture was
/// ineffective (root) — restored to 755 and skipped loudly.
#[cfg(all(test, unix))]
pub(crate) fn dir_read_only_or_skip(dir: &std::path::Path, test: &str) -> bool {
    use std::os::unix::fs::PermissionsExt as _;
    let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o555));
    // Root is not constrained by 0555: with the directory still writable the
    // failure branch would never run — restore and skip loudly.
    let probe = dir.join(".root-probe");
    if std::fs::write(&probe, b"").is_ok() {
        let _ = std::fs::remove_file(&probe);
        let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755));
        eprintln!(
            "ROOT-SKIP[{test}]: running as root - the read-only-dir fixture stays writable; NOT exercised"
        );
        return false;
    }
    true
}

/// POSIX mode bits of `path`, or `None` where the platform has none.
///
/// Permission assertions belong to the platform layer: a feature test that
/// writes its own `#[cfg(unix)]` block puts an OS conditional outside an
/// adapter (architecture guard `rust_target_cfg_outside_adapter`), and every
/// such copy drifts on its own. Callers assert `Some(0o600)` and skip on
/// `None`.
#[cfg(test)]
pub(crate) use filesystem::permission_bits;

#[cfg(test)]
pub(crate) use filesystem::with_read_permission_denied;

/// Loosens `path` to 0644 the way a pre-hardening build's umask left it, so a
/// test can prove the next write tightens it. No-op (and reports `false`)
/// where the platform has no mode bits.
#[cfg(test)]
pub(crate) use filesystem::loosen_to_world_readable_for_test;

/// Bounded-refusal assertion for an operation that must fail fast on a
/// planted non-regular target (FIFO/device) instead of hanging: runs `f` on
/// a worker thread and requires it to report `true` within 5 seconds. On
/// timeout the test panics while the abandoned worker (holding no lock) is
/// left to finish or die on its own, so the red does not cascade; a `false`
/// result within the bound is a plain refusal-assertion failure. The repo
/// convention for the hardened private-data primitives.
#[cfg(test)]
pub(crate) use filesystem::assert_refused_without_hanging;

/// Bounded-run containment for a worker that may hold a cross-process lock
/// (mcp.lock, a scope/transaction lock, the landing lease): spawns `worker`
/// on its own thread and returns its value, but only if it finishes within
/// 5 seconds. On a timeout or a channel disconnect (worker panic) the
/// helper eprints the abort line and kills the process with
/// `std::process::abort()`: a plain test panic would unwind the assertion
/// thread while the abandoned worker stays blocked HOLDING the lock, and
/// that leaked holder cascade-hangs every later test in the serial lane —
/// the abort variant of [`assert_refused_without_hanging`], whose workers
/// hold no lock and may safely be left to finish or die on their own. The
/// abort line reads `FAIL: {label} — aborting the test process to contain
/// the leaked lock holder`; `label` carries the per-site context (what
/// still blocks, on what). Per-site refusal/value predicates stay at the
/// call sites — this helper owns only the spawn/5s-bound/eprintln+abort/
/// join discipline.
#[cfg(test)]
pub(crate) fn run_bounded_worker_or_abort<R: Send + 'static>(
    label: &str,
    worker: impl FnOnce() -> R + Send + 'static,
) -> R {
    let (tx, rx) = std::sync::mpsc::channel();
    let handle = std::thread::spawn(move || {
        let value = worker();
        let _ = tx.send(value);
    });
    let value = rx
        .recv_timeout(std::time::Duration::from_secs(5))
        .unwrap_or_else(|_| {
            eprintln!(
                "FAIL: {label} — aborting the test process to contain the leaked lock holder"
            );
            std::process::abort();
        });
    handle.join().unwrap();
    value
}

/// Shared zip-fixture builder for the plugin-package import tests: writes
/// `entries` (archive path → bytes, in order) into a uniquely named temp zip
/// (`{prefix}-{pid}-{suffix}.zip`) and returns its path. Zip bytes and temp
/// names match the per-test inline builders this replaces.
#[cfg(test)]
pub(crate) fn write_test_zip(prefix: &str, entries: &[(&str, Vec<u8>)]) -> std::path::PathBuf {
    let mut zip_buf = std::io::Cursor::new(Vec::new());
    {
        use std::io::Write;
        let mut zw = zip::ZipWriter::new(&mut zip_buf);
        let opts = zip::write::SimpleFileOptions::default();
        for (name, bytes) in entries {
            zw.start_file((*name).to_string(), opts).unwrap();
            zw.write_all(bytes).unwrap();
        }
        zw.finish().unwrap();
    }
    let tmp = std::env::temp_dir().join(format!(
        "{prefix}-{}-{}.zip",
        std::process::id(),
        crate::platform::paths::tests::unique_suffix()
    ));
    std::fs::write(&tmp, zip_buf.into_inner()).unwrap();
    tmp
}

/// 测试把文件权限改成指定 mode(unix);非 unix 平台恒返 false(调用方按
/// 「平台不支持即跳过」处理)。与 [`crate::platform::filesystem::permission_bits`]
/// 读侧配对使用。
#[cfg(test)]
pub(crate) fn force_private_file_mode(path: &std::path::Path, mode: u32) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).is_ok()
    }
    #[cfg(not(unix))]
    {
        let _ = (path, mode);
        false
    }
}

/// Shared source-pin primitive ("drop pure comment lines first", round-27
/// review m14): every file's pins used to inline the same filter closure
/// (mod.rs/persistence.rs/memory.rs/sessions.rs/memory tests/tasks.rs/
/// executor.rs/relay_client.rs/remote_control.rs) and the calibers could
/// only drift by eye. Whole-line `//`/`///` comments are dropped, then
/// closed `/* … */` spans are removed (review JensenChen28 4510500034 /
/// 4521238202): the line pass runs first, so a `//`-commented-out `/*`
/// marker cannot make the block pass eat live code, and the block pass
/// removes closed spans (tracking nesting depth, review JensenChen28
/// 4525154638), so a compiling mutation that block-comments an assignment
/// cannot keep the needle's substring alive while the executable behavior
/// is gone (an unterminated `/*` is kept verbatim).
/// Residual exposure, unchanged and handled per pin's disclosed residuals:
/// a needle mentioned in a trailing `//` comment on an executable line,
/// or inside a string literal, still satisfies a `contains` pin. The filter
/// is string-literal-blind in the other direction too: a `/* … */` inside a
/// string literal is stripped like a comment, so over-stripping can only
/// shorten a pin window and fail it red (fail-closed) — never keep a dead
/// needle alive.
#[cfg(test)]
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

/// Newline-preserving removal of closed `/* … */` spans, tracking nesting
/// depth: Rust block comments nest, so a span's balancing close is the
/// `*/` that returns the depth to zero, not the first `*/` seen (review
/// JensenChen28 4525154638). Runs after the whole-line-comment pass; an
/// unterminated `/*` is kept verbatim (scan past the marker) rather than
/// eating the rest of the source.
#[cfg(test)]
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

#[cfg(test)]
mod comment_filtered_tests {
    use super::comment_filtered;

    #[test]
    fn block_commented_text_no_longer_satisfies_a_pin_needle() {
        // Review JensenChen28 4510500034/4521238202: the line pass alone
        // left a block-commented assignment's substring alive, so a
        // compiling mutation (wrap the executable line in `/* … */`) kept
        // every adopted pin green while the behavior was gone. The block
        // pass must remove the needle with the span.
        let mutated = "fn revoke() {\n    /*\n    endpoint.config.allow_host_workspace = true;\n    self.emit_status();\n    */\n}";
        let filtered = comment_filtered(mutated);
        assert!(
            !filtered.contains("endpoint.config.allow_host_workspace = true;"),
            "block-commented executable text must not satisfy a pin needle"
        );
        assert!(
            !filtered.contains("self.emit_status();"),
            "every needle inside the block-commented span must be removed"
        );
    }

    #[test]
    fn line_commented_block_markers_do_not_eat_live_code() {
        // Ordering discipline (mirrors forwarder.rs's forwarder_body): the
        // whole-line pass runs first, so a `//`-commented-out `/*` marker
        // cannot make the block pass eat the live lines after it.
        let live = "fn keep() {\n    // /*\n    let keep_me = true;\n    // */\n    let also_live = keep_me;\n}";
        let filtered = comment_filtered(live);
        assert!(
            filtered.contains("let keep_me = true;"),
            "a line-commented block marker must not eat live code"
        );
        assert!(
            filtered.contains("let also_live = keep_me;"),
            "code after line-commented markers must survive"
        );
    }

    #[test]
    fn nested_block_comments_strip_through_the_balancing_close() {
        // Review JensenChen28 4525154638: Rust block comments nest, so the
        // first `*/` does not close the outer span. A compiling mutation
        // that wraps the pin target in an outer comment holding an inner
        // `/* … */` used to leave the needle's substring alive past the
        // inner close, so the pin still passed with the behavior commented
        // out. The block pass must track depth and stop at the balancing
        // close.
        let mutated = "fn revoke() {\n    /* outer\n    /* inner */\n    endpoint.config.allow_host_workspace = true;\n    */\n}";
        let filtered = comment_filtered(mutated);
        assert!(
            !filtered.contains("endpoint.config.allow_host_workspace = true;"),
            "a needle behind a nested block-comment span must be removed"
        );
        assert!(
            !filtered.contains("*/"),
            "the balancing close must be consumed with the span"
        );
    }

    #[test]
    fn disclosed_residual_trailing_comment_mention_survives() {
        // Unchanged residual: whole-line filtering does not strip trailing
        // `//` comments, so a needle repeated in one still matches. Each
        // pin discloses this exposure itself.
        assert!(
            comment_filtered("let x = 1; // let x = 1;").contains("let x = 1;"),
            "trailing-comment mentions remain a disclosed residual"
        );
    }
}
