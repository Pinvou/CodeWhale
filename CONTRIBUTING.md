# Contributing to Pinvou Agent

[English](CONTRIBUTING.md) | [简体中文](CONTRIBUTING.zh-CN.md)

Thank you for helping improve Pinvou Agent. Bug fixes, documentation, connectors, Skills, workflows, platform support, and focused product improvements are welcome.

This document covers the contribution workflow. Project-wide implementation and quality boundaries are defined in [AGENTS.md](AGENTS.md).

## Before you start

1. Search existing issues and pull requests to avoid duplicate work.
2. Discuss large features, architecture changes, and breaking changes in an issue first.
3. Follow [README.md](README.md) for development setup.
4. Start from the latest official `main`.

Maintainers may branch from `origin/main`. External contributors should configure the official repository once and branch from `upstream/main`:

```bash
git remote add upstream https://github.com/Pinvou/pinvou-agent.git
git fetch upstream
git switch -c feat/short-description upstream/main
git submodule update --init --recursive
```

Sync the latest official `main` before opening a pull request. During review, do not
repeatedly rebase only because `main` advances. When a pull request is ready, the
merge queue validates its combined tree against the latest `main`; manually rebase
again only to resolve a real conflict or when the queue reports an integration
failure that requires a branch change.

When resolving conflicts, preserve compatible functionality and user changes from
both sides. Do not choose between behaviorally different alternatives without
explaining the options and their impact to the user.

## DCO

Every human-authored commit must include a valid `Signed-off-by`:

```bash
git commit -s
```

Use `--signoff` when amending or rebasing existing commits. See the [Developer Certificate of Origin](https://developercertificate.org/). CI rejects unsigned human commits; trusted Dependabot and GitHub Actions bot commits and merge commits (more than one parent) are exempt.

The fixed historical squash-commit registry documented in [the commit convention](docs/Git%20Commit%20信息规范文档.md#10-固定历史-squash-提交的签署登记) preserves the recorded original signers of 19 existing integration commits. It is not an identity alias or an exemption for new commits; titles and the registered signoffs remain checked.

## Where changes belong

Pinvou Agent uses [CodeWhale](https://github.com/Pinvou/CodeWhale) as its agent engine. Do not reimplement engine capabilities in the desktop layer. The extension-boundary table is defined once in [AGENTS.md](AGENTS.md) §2; CodeWhale changes must follow it and [`docs/fork-policy.md`](docs/fork-policy.md), including the required same-PR documentation, fingerprints, and tests.

## Commit messages

Use:

```text
<type>: <中文描述>
<type>(<scope>)!: <中文描述>
```

`scope` and `!` are optional. Allowed types are `feat`, `fix`, `refactor`, `perf`, `docs`, `style`, `test`, `build`, `ci`, `chore`, and `revert`. Descriptions must contain Chinese, be specific, be at most 50 characters, and not end with punctuation.

Issues and pull requests may use English or Chinese. See [`docs/Git Commit 信息规范文档.md`](docs/Git%20Commit%20信息规范文档.md) for full commit rules.

## Local checks

Run checks that match the affected area. A common baseline is:

```bash
./scripts/fork-guard.sh --fast
python3 scripts/architecture-guard.py
npm --prefix pinvou3-app run lint:ui
npm --prefix pinvou3-app run build:ui
npm --prefix pinvou3-app test
cargo fmt --manifest-path pinvou3-app/src-tauri/Cargo.toml -- --check
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib -- --test-threads=1
```

Run additional frontend, Relay, Rust, CodeWhale, or platform checks when relevant. The workflows in [`.github/workflows/`](.github/workflows/) are the source of truth for automated gates. Disclose checks that could not be run locally.

When a change regenerates a `package-lock.json`, regenerate against the official
registry (`https://registry.npmjs.org/`): a mirror configured in a local npmrc is
copied verbatim into the lock's `resolved` URLs, and
`scripts/check-lock-registry.py` (fast-gate) fails any committed mirror URL. Use
npm >= 11.11 so the `libc` fields on the `@img/*` platform packages survive the
rewrite — the libc serializer fix landed in npm 11.11.0, so every npm from 10
up to 11.10 silently strips them and the same guard fails the strip.

Tests requiring a live model, network service, credential, or large model asset must be ignored by default and provide an explicit opt-in command.

## CI and merge queue

Pull requests use a staged, path-aware gate. Draft pull requests run fast feedback
(lint, build, deterministic logic tests, and Rust formatting where applicable).
Ready pull requests add browser smokes selected from the actual diff, platform
runtime contracts, and other affected checks. Ready Rust pull requests run fast
formatting, lint, dependency-policy, and compile feedback by default. All unknown
Rust paths fail closed to full regression; only documented isolated leaf-feature
boundaries use the lightweight route. Dependency, CodeWhale gitlink, app-command,
platform, permission, credential, session, remote-control, and other high-blast-
radius paths automatically add full Linux and Windows Rust regression before
queueing. Maintainers can apply `ci:full-rust` to force the same full regression
for another ready Rust pull request; the label has no effect on a draft.
The current lightweight Rust boundaries are internal-only changes under `feedback`,
`personas`, and `pet`. Changing their registration, app commands, shared platform
surface, Cargo metadata, or any unclassified Rust path still runs full regression.
Trusted same-repository `backflow/community-*` pull requests add a content-aware
decision over the actual base-to-head diff. A pure release is accepted only when
`VERSION` and every field managed by `scripts/sync-version.mjs` change mechanically
to one consistent SemVer; it runs the release contract without product compilation.
Other cross-platform Rust backflows keep lint, portability, affected tests, and
cumulative `main` coverage, while Windows/platform/path/process/install/permission/
native changes, Cargo dependency or feature changes, `build.rs`, the toolchain, the
CodeWhale gitlink, non-version `tauri.conf.json` fields, and edits to the gate
itself (`pr-check.yml`, `ci-rust-policy.py`) retain the complete Windows job. A
mismatched source marker,
fork, base repository, or non-`main` base uses the unchanged ordinary PR routing.
An incomplete Merge Group member chain, API/diff failure, malformed version field,
mixed release change, or unknown policy state fails closed to the complete Windows
job; `required-gate` does not accept a skipped Windows result in that state.
An entry explicitly reported as `QUEUED` with both `baseCommit` and `headCommit`
present and null has not been built and is excluded from the member-chain index.
Missing fields, partial commit pairs, and unknown states still fail closed; the
event's complete base-to-head chain and every actual member remain mandatory.
`ci:full-rust` continues to force the ordinary full route.
Release-chain changes run lightweight contract tests only; full deb, dmg, and nsis
packages are built only after a
`VERSION` change reaches `main`, or through an explicit `workflow_dispatch`.

The Linux Rust jobs prepare disk with `scripts/ci-rust-disk.py`, which removes
only the unused preinstalled SDK directories (Android, .NET, Haskell/ghcup,
Julia by default; optionally the hostedtoolcache and browser bundles with
`--aggressive`) on disposable GitHub-hosted Linux runners; deletion refuses
self-hosted runners and redirected paths. On self-hosted Linux, workflows use
`--check-only`, which checks the same capacity floor without scanning or deleting
SDKs. Both modes refuse an undersized disk
(`--min-free-gib`, default 12). The pr-check rust-test job calls it before the
swap file and caches land with a 24 GiB floor, again before the cache
restores, and again after them (12 GiB floor); rust-lint uses the same
24 GiB pre-swap floor, while cli-test and the loongarch64 cross-check run the
default floor. The release build jobs run the same preparation once with a
24 GiB floor. Hosted Linux
runners use a single disk (`/` and `/mnt` share one ext4 filesystem), so the
memory expansion step (`scripts/ci-memory-setup.sh`) provisions memory in
layers: zram (on by default, sized to 2x RAM with a zstd compressor, pool
capped at 70% of RAM), a mandatory 8 GiB `/mnt/swapfile` as the unbounded
overflow (on the shared single disk this costs 8 GiB of build disk; it is
skipped only when /mnt is RAM-backed or has too little free space), then
zswap and the image swap as fallbacks. Only image-provided zram modules are
loaded; if the module is missing or cannot load, the disk-swap target rises
to 16 GiB (subject to the same free-space cap). Memory setup never installs
kernel packages: its hard timeout must not interrupt dpkg and break later
required system-dependency installation.
Self-hosted runners retain their administrator-provisioned memory and swap;
the memory setup script leaves them unchanged, and apt cleanup is hosted-only.

Repository administrators can set `PINVOU_LINUX_RUNNER` to a JSON label array
(for example `["self-hosted","Linux","pinvou3"]`) to route x64 Linux jobs to
a dedicated Ubuntu 22.04 runner. Without that variable they use `ubuntu-22.04`.
The runner needs passwordless sudo for dependency setup, Docker with Compose and
Buildx, GitHub CLI, and access to the required package registries. It must be
reserved for trusted repository code. Self-hosted jobs retain local npm caches
and disable setup-node's remote cache upload. Set `PINVOU_WINDOWS_MAC_ENABLED`
to the literal `false` to disable Windows/macOS jobs and their corresponding
required results; Linux checks remain required. Unset or any other value keeps
the existing three-platform policy. ARM release jobs keep their hosted ARM label.
Optional `PINVOU_LINUX_HEAVY_RUNNER`, `PINVOU_LINUX_MEDIUM_RUNNER`, and
`PINVOU_LINUX_LIGHT_RUNNER` arrays override that default for compilation,
application/integration tests, and fast policy checks respectively. Each falls
back to `PINVOU_LINUX_RUNNER`, then to the hosted 22.04 image. Distinct pool labels
keep large Rust compilations away from small policy runners.

Hosts use Ubuntu 26.04 while runner containers use the Ubuntu 22.04 image in
`scripts/ci/runner-image/`. This retains the existing release glibc compatibility
baseline. Each runner has independent writable state and resource limits.
CI restores CodeWhale to the indexed gitlink and cleans its disposable workspace
with `scripts/ci-prepare-codewhale.sh`. The fingerprint gate also requests full
history with `--history`; compilation-only jobs do not download missing history.
The helper requires the parent metadata at `.git` and CodeWhale metadata at
`CodeWhale/.git` or `.git/modules/CodeWhale` in that Actions workspace; redirected
or shared writable repository metadata and mutable Git path overrides are refused.
Read-only object alternates are permitted. It does not change the gitlink or
replace the public-baseline and fork-guard checks.

PostgreSQL allocates dynamic CI ports; release-console container tests use a
job-specific Compose project, port, and temporary public directory instead of
the host production directory. The image preinstalls Chrome and the verified
PipeWire prefix. `PINVOU_PIPEWIRE_SOURCE_ARCHIVE` supplies a cached source archive
with the same upstream SHA256 check; it does not bypass integrity verification.
All Linux jobs pin the release runner image (`ubuntu-22.04` /
`ubuntu-22.04-arm`): release binaries link the build machine's glibc, so
tests must run on the same system. The image is upgraded repo-wide in one
coordinated change only (enforced by the image guard test) — never per-job
and never via rolling tags like `ubuntu-latest`. Both the main library tests
and the separate behavior-telemetry tests remain required. If runner images
change, inspect the preparation step's free-space report instead of bypassing
the capacity check or skipping either test harness.

The merge queue runs the applicable product gates against the actual combined tree
of the queued pull request and the latest `main`. Rust changes run formatting,
Clippy, compile, and dependency-policy checks there. High-blast-radius Rust changes
also run the full Linux behavior regression on the combined tree; Windows coverage
already ran on the ready PR and is not repeated unless the fail-closed policy
above requires it. Frontend changes run browser smokes
selected from the merge group's actual base/head diff; shared, unknown, or test-
infrastructure paths fall back to the complete smoke set. Every retained `main` push
runs cumulative Linux compile verification (full Linux behavior execution stays on the
merge-queue leg, which already validates the same combined tree), native Windows
checks, and — since 2026-10 — the native macOS legs folded into `macos-rust-check`
(full lib tests on every push; the release-fast/bundle smokes only on pushes whose
diff touches the Rust, knowledge, or packaging surfaces), and continues to warm the
shared caches. The Windows required job runs its
all-target metadata check and linked native regression as two parallel matrix legs;
both legs use the same high-risk/main routing and both must pass. See
[`docs/windows-rust-ci.md`](docs/windows-rust-ci.md) for coverage, cache, timing, and
rollback details. A red main regression is a stop signal for further queueing until
it is fixed or reverted. During review, maintainers should inspect only required checks:

```bash
gh pr checks <number> --required
```

Do not wait for non-required post-merge platform or release builds when `main` is
green. Queue independent ready pull requests without routine rebases; resolve actual
conflicts, and let the queue validate freshness. Maintainers may queue at most two
low-risk, independent pull requests in one merge group. Dependency-lock, CI, release,
permission, session, CodeWhale gitlink, and other high-risk changes enter alone.

## Pull requests

Review the actual diff against the target branch and complete the quality self-check required by [AGENTS.md](AGENTS.md) before submission.

A pull request should explain:

- what changed and why;
- affected features, platforms, compatibility, and known risks;
- tests actually run;
- unverified scenarios or environment limitations.

Keep changes focused. Update documentation and regression tests with behavior changes. Resolve conflicts against the latest official `main` before merge. The project uses CI-gated pull requests and squash merge by default.

By participating, you agree to follow [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Support expectations are documented in [SUPPORT.md](SUPPORT.md).

## Security

Never commit credentials, tokens, passwords, customer or private data, or internal-only addresses. Report unpatched vulnerabilities privately through [SECURITY.md](SECURITY.md) or `security@pinvou.com`.
