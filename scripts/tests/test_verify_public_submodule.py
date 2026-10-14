import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
VERIFIER = ROOT / "scripts/verify-public-submodule.sh"
GITLINK = re.search(r'^EXPECTED_HEAD="([0-9a-f]{40})"$', (ROOT / "scripts/fork-guard.sh").read_text(encoding="utf-8"), re.MULTILINE).group(1)
OTHER_COMMIT = "2222222222222222222222222222222222222222"
TAG_OBJECT = "3333333333333333333333333333333333333333"
# 与 verify-public-submodule.sh 的常量保持一致：2026-10-02 起处于两次 rN 收口
# 之间的过渡期，TRANSITION_BASELINE 钉在 r3 收口 commit（与脚本同值）；默认发布
# 检查要求 gitlink 等于维护头且领先 tag，不支持登记快照豁免。下一次 rN 收口时，把此处的 tag 名/钉点与脚本
# 常量同步推进（新 tag、清空 TRANSITION_BASELINE），并恢复三方相等断言。
BASELINE = "61cb769be5b33abc64f64da4272f5b39a8b6c1fd"


BASH = "C:/Program Files/Git/bin/bash.exe" if os.name == "nt" and Path("C:/Program Files/Git/bin/bash.exe").exists() else shutil.which("bash")


@unittest.skipUnless(BASH, "shell verifier requires bash")
class PublicSubmoduleVerifierTests(unittest.TestCase):
    def _run(self, scenario, arguments=(), closed=False):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            bin_dir = temp / "bin"
            bin_dir.mkdir()
            state_file = temp / "ls-remote-attempts"
            calls_file = temp / "git-calls"

            fake_git = bin_dir / "git"
            fake_git.write_text(
                textwrap.dedent(
                    f"""\
                    #!/usr/bin/env bash
                    set -euo pipefail
                    printf '%s\\n' git >> "$PINVOU_FAKE_GIT_CALLS"

                    while [[ "${{1:-}}" == "-c" ]]; do shift 2; done

                    if [[ "${{1:-}}" == "-C" ]]; then
                      shift 2
                    fi

                    if [[ "$PINVOU_FAKE_GIT_SCENARIO" == "ambient_askpass" && ( "${{1:-}}" == "ls-remote" || "${{1:-}}" == "fetch" ) ]]; then
                      if [[ -n "${{GIT_ASKPASS:-}}" || -n "${{SSH_ASKPASS:-}}" ]]; then exit 128; fi
                    fi

                    case "${{1:-}}" in
                      init) exit 0 ;;
                      config)
                        key="${{5:-}}"
                        case "$key" in
                          submodule.CodeWhale.path) printf '%s\\n' 'CodeWhale' ;;
                          submodule.CodeWhale.url) printf '%s\\n' 'https://github.com/Pinvou/CodeWhale.git' ;;
                          submodule.CodeWhale.branch) exit 1 ;;
                          *) exit 2 ;;
                        esac
                        ;;
                      ls-files)
                        gitlink='{GITLINK}'
                        if [[ "$PINVOU_FAKE_GIT_SCENARIO" == "tag_caught_up" ]]; then
                          gitlink='{BASELINE}'
                        fi
                        printf '160000 %s 0\\tCodeWhale\\n' "$gitlink"
                        ;;
                      ls-remote)
                        attempt=0
                        if [[ -f "$PINVOU_FAKE_GIT_STATE" ]]; then
                          attempt="$(<"$PINVOU_FAKE_GIT_STATE")"
                        fi
                        attempt=$((attempt + 1))
                        printf '%s\\n' "$attempt" > "$PINVOU_FAKE_GIT_STATE"

                        case "$PINVOU_FAKE_GIT_SCENARIO" in
                          retry_then_aligned)
                            if [[ "$attempt" -lt 3 ]]; then exit 128; fi
                            ;;
                          transport_failure)
                            exit 128
                            ;;
                        esac

                        branch='{GITLINK}'
                        tag='{BASELINE}'
                        case "$PINVOU_FAKE_GIT_SCENARIO" in
                          branch_mismatch) branch='{OTHER_COMMIT}' ;;
                          maintenance_advanced|closed_advanced) branch='{OTHER_COMMIT}' ;;
                          closed_aligned) tag='{GITLINK}' ;;
                          annotated) tag='{TAG_OBJECT}' ;;
                          tag_drifted) tag='{OTHER_COMMIT}' ;;
                          tag_caught_up)
                            branch='{BASELINE}'
                            tag='{BASELINE}'
                            ;;
                        esac
                        if [[ "$PINVOU_FAKE_GIT_SCENARIO" == "closed_advanced" ]]; then tag='{GITLINK}'; fi
                        printf '%s\\trefs/heads/pinvou3-clean\\n' "$branch"
                        if [[ "$PINVOU_FAKE_GIT_SCENARIO" != "missing_tag" ]]; then
                          printf '%s\\trefs/tags/pinvou-v0.9.12-r3\\n' "$tag"
                        fi
                        if [[ "$PINVOU_FAKE_GIT_SCENARIO" == "annotated" ]]; then
                          printf '%s\\trefs/tags/pinvou-v0.9.12-r3^{{}}\\n' '{BASELINE}'
                        fi
                        ;;
                      *)
                        exit 2
                        ;;
                    esac
                    """
                ),
                encoding="utf-8",
            )
            fake_git.chmod(0o755)

            fake_sleep = bin_dir / "sleep"
            fake_sleep.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
            fake_sleep.chmod(0o755)

            env = os.environ.copy()
            verifier = VERIFIER
            if closed:
                scripts = temp / "scripts"
                scripts.mkdir()
                verifier = scripts / VERIFIER.name
                verifier.write_text(VERIFIER.read_text(encoding="utf-8").replace(f'TRANSITION_BASELINE="{BASELINE}"', 'TRANSITION_BASELINE=""'), encoding="utf-8")
            if os.name == "nt":
                env["PINVOU_TEST_BIN"] = bin_dir.as_posix()
                env["PINVOU_TEST_VERIFIER"] = verifier.as_posix()
                command = [BASH, "-c", 'export PATH="$(cygpath -u "$PINVOU_TEST_BIN"):$PATH"; exec bash "$PINVOU_TEST_VERIFIER" "$@"', "verifier", *arguments]
            else:
                env["PATH"] = f"{bin_dir}:{env['PATH']}"
                command = [BASH, str(verifier), *arguments]
            if scenario == "ambient_askpass":
                env["GIT_ASKPASS"] = "unused-test-credential-helper"
                env["SSH_ASKPASS"] = "unused-test-credential-helper"
            env["PINVOU_FAKE_GIT_SCENARIO"] = scenario
            env["PINVOU_FAKE_GIT_STATE"] = str(state_file)
            env["PINVOU_FAKE_GIT_CALLS"] = str(calls_file)
            result = subprocess.run(
                command,
                cwd=ROOT,
                env=env,
                text=True,
                encoding="utf-8",
                errors="replace",
                capture_output=True,
                check=False,
            )
            attempts = int(state_file.read_text(encoding="utf-8").strip()) if state_file.exists() else 0
            result.git_calls = len(calls_file.read_text(encoding="utf-8").splitlines()) if calls_file.exists() else 0
            return result, attempts

    def test_transition_gitlink_tracks_branch_and_aheads_of_pinned_tag(self):
        result, attempts = self._run("aligned")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, 1)
        self.assertIn("过渡期校验通过", result.stdout)
        self.assertIn(f"pinvou3-clean = gitlink = {GITLINK}", result.stdout)

    def test_annotated_tag_is_compared_after_peeling(self):
        result, attempts = self._run("annotated")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, 1)
        self.assertIn("过渡期校验通过", result.stdout)

    def test_mismatched_public_branch_fails_closed(self):
        result, attempts = self._run("branch_mismatch")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 1)
        self.assertIn("pinvou3-clean", result.stderr)

    def test_missing_immutable_tag_fails_closed(self):
        result, attempts = self._run("missing_tag")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 1)
        self.assertIn("pinvou-v0.9.12-r3", result.stderr)
        self.assertIn("过渡期内应钉在收口", result.stderr)
        self.assertIn("<不存在>", result.stderr)

    def test_tag_drifting_from_the_baseline_fails_closed(self):
        result, attempts = self._run("tag_drifted")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 1)
        self.assertIn("pinvou-v0.9.12-r3", result.stderr)
        self.assertIn("过渡期内应钉在收口", result.stderr)
        self.assertIn(OTHER_COMMIT, result.stderr)

    def test_tag_caught_up_by_the_gitlink_demands_a_new_closure_tag(self):
        result, attempts = self._run("tag_caught_up")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 1)
        self.assertIn("gitlink 已追平", result.stderr)
        self.assertIn("切除新 tag", result.stderr)

    def test_remote_transport_is_retried_with_a_finite_limit(self):
        result, attempts = self._run("retry_then_aligned")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, 3)

        result, attempts = self._run("transport_failure")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 3)
        self.assertIn("3 次尝试", result.stderr)

    def test_default_strict_check_rejects_advanced_maintenance_head(self):
        result, attempts = self._run("maintenance_advanced")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 1)
        self.assertIn("pinvou3-clean", result.stderr)
        self.assertIn(OTHER_COMMIT, result.stderr)
        self.assertIn(GITLINK, result.stderr)

    def test_public_operations_ignore_ambient_askpass_by_default(self):
        result, attempts = self._run("ambient_askpass")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, 1)
        self.assertIn("过渡期校验通过", result.stdout)

    def test_registered_snapshot_argument_is_rejected_before_any_git_call(self):
        result, attempts = self._run("aligned", ("--registered-snapshot",))
        self.assertEqual(result.returncode, 2)
        self.assertEqual(attempts, 0)
        self.assertEqual(result.git_calls, 0)
        self.assertIn("unknown argument: --registered-snapshot", result.stderr)

    def test_closed_release_keeps_default_three_way_equality(self):
        result, attempts = self._run("closed_aligned", closed=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, 1)
        self.assertIn(f"pinvou3-clean = pinvou-v0.9.12-r3 = {GITLINK}", result.stdout)
        result, attempts = self._run("closed_advanced", closed=True)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(attempts, 1)
        self.assertIn("pinvou3-clean", result.stderr)


if __name__ == "__main__":
    unittest.main()
