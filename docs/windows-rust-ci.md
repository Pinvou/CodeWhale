# Windows Rust CI

## Coverage contract

`windows-rust-test` runs only on retained `main` pushes and ready high-risk or
explicit `ci:full-rust` pull requests. It remains skipped for Merge Queue events,
drafts, ordinary non-Rust pull requests, and documented low-risk Rust changes. The
single required job is a two-leg matrix; `fail-fast` is disabled and both legs must
pass:

- `all-targets-check` runs `cargo check --all-targets --features dev-tools`.
- `regression` links the `pinvou3_lib` unit-test executable, embeds the
  Common-Controls v6 manifest, runs the PE import diagnostic, executes all six
  existing Windows regression filters serially, and runs the CodeWhale PowerShell
  regression filters (`forkguard_powershell`, `forkguard_windows_shell_text`) from
  the dependency crate itself — parent application jobs do not execute a dependency
  crate's lib tests. The step unsets `SHELL` so the Windows fallback to `pwsh.exe`
  is deterministic, and each filter must match at least one test so a rename cannot
  silently pass.

The matrix changes scheduling, not the set or failure semantics of checks. The
Common-Controls manifest and import diagnostic remain attached to the exact test
executable later used by the six regression filters.

## Why the legs are independent

The default feature graph includes `local-embed`. `dev-tools` adds no dependency,
but enables the `dump_system_prompt` binary and gives the package a distinct feature
fingerprint. More importantly, `cargo check` produces metadata-only artifacts for
the library, binaries, integration tests, and unit-test target; `cargo test --lib
--no-run` must still generate and link executable code for the default-feature
library test harness. Running check first therefore does not turn the following
test link into a metadata-only incremental operation, and the old job placed both
compile graphs on one 47–78 minute critical path.

CodeWhale initialization remains in both legs because the application has direct
path dependencies on its crates. The old Node/npm setup originated when this job
also ran JavaScript Tauri layout contracts. Those contracts now run elsewhere, and
neither Rust build scripts nor the retained six filters consume the application
`node_modules`, so the matrix does not install it.

## Cache and diagnostics

The single cache entry covers both target directories — the application workspace
and the `CodeWhale` path dependency, whose required lib tests need their own target
directory — under the existing `windows-rust-test` namespace, restored into isolated
runner workspaces. Only the `regression` leg on `main` may save it, so the split adds
neither a new namespace nor a second writer; the first `main` run after adding the
`CodeWhale` workspace compiles it cold and adds roughly 0.8–2 GB compressed, while
Cargo registry/target data remains under the existing 10 GB cache budget and
eviction behavior. This keeps the experiment
independent of an uncontrolled new cache while allowing both legs to reuse the same
known snapshot. Restore misses and metadata time remain visible rollback signals.

Each run emits only aggregate, non-sensitive diagnostics:

- `WINDOWS_RUST_CACHE` records matrix phase, target presence, fingerprint-crate count,
  and direct dependency-artifact count without recursively scanning the large tree.
- `WINDOWS_RUST_TIMING` records phase, fixed step/filter identifier, elapsed seconds,
  and exit status for CodeWhale initialization, Cargo work, the six-filter suite,
  and the CodeWhale PowerShell regressions.

No environment values, paths outside the workspace, command output credentials, or
cache keys derived from secrets are printed.

## Expected result and rollback

Previously the critical path was approximately `setup + check + link + tests`.
After the split it is `max(setup + check, setup + link + tests)`. Based on the
observed 47–78 minute baseline, the initial CI hypothesis is a 25–35% wall-clock
reduction, approximately 31–59 minutes, without reducing coverage. This estimate is
not a local benchmark: hosted-runner queueing, aws-lc-sys compilation, and cache
availability can change it.

Compare at least three high-risk PR/main runs using the emitted phase timings. If
the legs do not overlap or the check leg becomes the new slower path, revert
the matrix scheduling while retaining the metrics. Do not recover time by removing
a regression filter, skipping the manifest/import contract, changing failures to
warnings, or enabling an additional large target cache beyond the single
two-workspace entry documented above.
