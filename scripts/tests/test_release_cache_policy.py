import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
RELEASE_WORKFLOW = REPO_ROOT / ".github/workflows/release-packages.yml"
PR_CHECK_WORKFLOW = REPO_ROOT / ".github/workflows/pr-check.yml"
MAIN_ONLY_SAVE = "save-if: ${{ github.ref == 'refs/heads/main' }}"


def _without_yaml_comments(block):
    # 与 test_ci_gate_policy 同款辅助:被注释掉的步骤或 save-if 行
    # 不得满足钉定活配置的断言。
    return "\n".join(
        line for line in block.splitlines() if not line.lstrip().startswith("#")
    )


class ReleaseCachePolicyTests(unittest.TestCase):
    def test_release_jobs_write_no_caches(self):
        # Release caches are 1-2 GB each. While they were main-save (the
        # pre-2026-10 policy), every VERSION push landed 4-6 GB of cache at
        # once and the 10 GB LRU flushed the PR-gate caches (windows/macos/
        # rust-test/lint) in the same stroke — the eviction dynamic that left
        # mac-build permanently cold. Releases are rare and have passed cold
        # throughout, so with #643 (landed 2026-10-09) the save side was
        # disabled; on 2026-10-15 the (by then guaranteed-miss) restore-only
        # rust-cache steps were removed entirely, and the npm side was held
        # to the same no-write policy: setup-node cannot be told not to
        # save, so keeping cache: 'npm' re-mints a main-scope
        # single-lockfile entry whenever the previous one is gone —
        # evicted, or displaced by a lockfile hash change (scopes are
        # refs: release jobs run in main scope alongside pr-check's push
        # legs; an exact hit in scope skips the re-save, and the 0.10.11
        # macOS release leg genuinely restored 294MB from exactly such an
        # entry). What the removal costs is seconds of npm ci on a rare
        # job (cold npm ci measured 10-25s in the release runs); what it
        # saves is ~0.2-0.3GB of per-release churn against the quota.
        # Re-adding any cache write to a release job needs a fresh look at
        # the quota policy (the PR-gate namespaces must stay resident).
        # Case-folded: GitHub resolves action repo refs case-insensitively,
        # so `uses: Swatinem/Rust-Cache@v2` writes real caches too —
        # casing must not be an evasion path for the absence pins.
        workflow = _without_yaml_comments(
            RELEASE_WORKFLOW.read_text(encoding="utf-8")
        ).lower()

        # Fork-generic pin: any *rust-cache action (Swatinem or a fork,
        # under any version) must trip — a re-added cache under a forked
        # action name must not pass an identity pin vacuously. Identity
        # bans cannot cover unknown names: a differently-named future
        # cache mechanism needs its own pin added here.
        self.assertNotIn(
            "rust-cache",
            workflow,
            "release jobs must not run rust-cache: they never save (quota "
            "policy) and the restore side was a guaranteed miss — zero "
            "release-* entries existed live when the steps were removed",
        )
        # The save-capable actions/cache spellings are banned; the
        # restore-only subaction is not a write and stays allowed (the
        # loongarch64 release job restores the toolchain/sysroot keys that
        # pr-check's push legs write — restore consumes no quota churn).
        self.assertNotIn(
            "actions/cache@",
            workflow,
            "release jobs must not run the save-capable actions/cache "
            "action (restore + post-job save); restore-only consumption "
            "via actions/cache/restore@ is allowed",
        )
        self.assertNotIn(
            "actions/cache/save@",
            workflow,
            "release jobs must not run the save-only subaction either — "
            "restore + explicit save would reassemble the banned "
            "save-capable flow",
        )
        # The common Rust caching sidecar by name; a renamed successor is
        # the documented residual gap above.
        self.assertNotIn(
            "sccache",
            workflow,
            "release jobs must not run sccache either — the no-write "
            "policy covers every caching mechanism",
        )
        # Spelling-proof key absence: a value with a trailing comment
        # (cache: 'npm' # warm) or a folded scalar (cache: >-) parses as a
        # live npm cache while per-line value matchers stay green. Banning
        # the cache: key itself is immune to every YAML spelling and fails
        # closed. (The ban is substring-wide on stripped text: an innocent
        # *_cache: env key would trip it too — acceptable here, release
        # jobs must hold no cache config at all.)
        self.assertNotIn(
            "cache:",
            workflow,
            "release jobs must not declare any cache: key at all — any "
            "setup-node npm cache spelling (quoted, commented, folded) is "
            "banned with it",
        )
        self.assertNotIn(
            "cache-dependency-path",
            workflow,
            "a cache-dependency-path without cache: 'npm' is dead config; "
            "release legs hold no npm cache at all",
        )

    def test_rust_test_allows_main_to_rebuild_cold_cache(self):
        workflow = PR_CHECK_WORKFLOW.read_text(encoding="utf-8")
        rust_test_job = workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]

        self.assertIn("timeout-minutes: 120", rust_test_job)
        self.assertIn("shared-key: rust-test-v2", rust_test_job)
        self.assertIn(MAIN_ONLY_SAVE, rust_test_job)

    def test_rust_test_restores_lint_cache_before_its_own_cache(self):
        workflow = PR_CHECK_WORKFLOW.read_text(encoding="utf-8")
        rust_test_job = workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]
        lint_cache = rust_test_job.index("shared-key: rust-lint")
        test_cache = rust_test_job.index("shared-key: rust-test")
        warm_cache_step = rust_test_job.split(
            "- name: 恢复 Rust lint 暖缓存", maxsplit=1
        )[1].split("- name: Cargo test cache", maxsplit=1)[0]

        self.assertLess(lint_cache, test_cache)
        self.assertIn("save-if: false", warm_cache_step)

    def test_linux_release_jobs_use_hosted_runner_profile(self):
        workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        linux_jobs = {
            "x64": workflow.split("\n  build-linux-x64:", maxsplit=1)[1].split(
                "\n  build-linux-arm64:", maxsplit=1
            )[0],
            "arm64": workflow.split(
                "\n  build-linux-arm64:", maxsplit=1
            )[1].split("\n  build-windows-x64:", maxsplit=1)[0],
        }

        for architecture, job in linux_jobs.items():
            with self.subTest(architecture=architecture):
                job_env = job.split("\n    steps:", maxsplit=1)[0]
                self.assertIn(
                    'CARGO_PROFILE_RELEASE_LTO: "false"', job_env
                )
                self.assertIn(
                    'CARGO_PROFILE_RELEASE_CODEGEN_UNITS: "16"', job_env
                )
                # size-policy flags(与 docs/binary-size-optimization.md 一致):
                # lld(BFD OOM + 无 ICF)、--icf=safe(lld 同码折叠,保守层)、
                # remap-path-prefix(制品内嵌路径规一化,顶层 rustc flag)。
                self.assertIn(
                    'RUSTFLAGS: "-C link-arg=-fuse-ld=lld '
                    '-C link-arg=-Wl,--icf=safe '
                    '--remap-path-prefix=${{ github.workspace }}=/"',
                    job_env,
                )
                self.assertIn("build-essential pkg-config cmake lld", job)

    def test_windows_artifact_release_uses_unsigned_build_entry(self):
        workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        windows_job = workflow.split(
            "\n  build-windows-x64:", maxsplit=1
        )[1].split("\n  build-macos-universal:", maxsplit=1)[0]

        self.assertIn("run: npm run build:nsis:local", windows_job)
        self.assertNotIn("run: npm run build:nsis\n", windows_job)
        self.assertNotIn("signing.wosign.conf.json", windows_job)
        self.assertNotIn(".builtin-secrets.env", windows_job)


if __name__ == "__main__":
    unittest.main()
