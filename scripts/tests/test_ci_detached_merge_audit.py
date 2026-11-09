import os
from pathlib import Path
import re
import shutil
import subprocess
import textwrap
import unittest

from test_ci_gate_policy import _extract_job_block, _extract_job_level_if


ROOT = Path(__file__).resolve().parents[2]
BASH = shutil.which("bash")


class DetachedMergeAuditTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        cls.audit = _extract_job_block(cls.workflow, "merge-authored-audit")
        cls.fast = _extract_job_block(cls.workflow, "fast-gate")
        cls.gate = _extract_job_block(cls.workflow, "required-gate")

    def step(self, name):
        lines = self.audit.splitlines()
        starts = [i for i, line in enumerate(lines) if line == f"      - name: {name}"]
        self.assertEqual(len(starts), 1, f"one real audit step is required: {name}")
        start = starts[0]
        end = next((i for i in range(start + 1, len(lines)) if lines[i].startswith("      - ")), len(lines))
        return "\n".join(lines[start:end]).rstrip()

    @staticmethod
    def run_body(step):
        if "        run: |\n" in step:
            return textwrap.dedent(step.split("        run: |\n", 1)[1]).strip()
        return re.search(r"^        run: (.+)$", step, re.MULTILINE).group(1)

    def test_audit_has_independent_bounded_budget_and_same_event_head(self):
        self.assertEqual(self.workflow.count("\n  merge-authored-audit:\n"), 1)
        self.assertEqual(_extract_job_level_if(self.audit), _extract_job_level_if(self.fast))
        self.assertNotRegex(self.audit, r"(?m)^    needs:")
        self.assertNotIn("continue-on-error:", self.audit)
        audit_timeout = re.search(r"(?m)^    timeout-minutes: (\d+)$", self.audit).group(1)
        fast_timeout = re.search(r"(?m)^    timeout-minutes: (\d+)$", self.fast).group(1)
        self.assertGreater(int(audit_timeout), 0)
        self.assertEqual(audit_timeout, fast_timeout, "moving the audit must keep the existing finite budget")
        self.assertEqual(
            self.step("Checkout (含 submodule,fork 为 public 直接拉)"),
            "      - name: Checkout (含 submodule,fork 为 public 直接拉)\n"
            "        uses: actions/checkout@v7\n        with:\n"
            "          submodules: false\n          fetch-depth: 0",
        )
        self.assertEqual(
            re.search(r"(?m)^    runs-on: (.+)$", self.audit).group(1),
            re.search(r"(?m)^    runs-on: (.+)$", self.fast).group(1),
        )

    def test_original_public_preparation_and_audit_entrypoint_are_retained(self):
        self.assertEqual(self.run_body(self.step("公开底座 gitlink 可达性")), "./scripts/verify-public-submodule.sh")
        self.assertEqual(
            self.run_body(self.step("初始化公共底座 submodule")),
            'bash "${{ github.workspace }}/scripts/ci-prepare-codewhale.sh" --history',
        )
        self.assertEqual(self.run_body(self.step("fork-guard 指纹层 (秒级,不编译)")), "./scripts/fork-guard.sh --fast")
        self.assertLess(self.audit.index("公开底座 gitlink 可达性"), self.audit.index("初始化公共底座 submodule"))
        self.assertLess(self.audit.index("初始化公共底座 submodule"), self.audit.index("fork-guard 指纹层"))
        self.assertLess(self.audit.index("fork-guard 指纹层"), self.audit.index("- name: Merge authored audit"))
        self.assertEqual(
            self.run_body(self.step("Merge authored audit")),
            'base_ref="${{ github.event_name == \'merge_group\' && github.event.merge_group.base_ref || github.base_ref }}"\n'
            'base_ref="${base_ref#refs/heads/}"\n'
            'python3 scripts/audit-merge-authored.py "origin/$base_ref"',
        )
        self.assertNotIn("scripts/audit-merge-authored.py", self.fast)
        self.assertIn('bash "${{ github.workspace }}/scripts/ci-prepare-codewhale.sh" --history', self.fast)

    def test_required_gate_waits_for_and_receives_actual_audit_result(self):
        needs = self.gate.split("    needs:\n", 1)[1].split("    if:", 1)[0]
        self.assertEqual(needs.splitlines().count("      - merge-authored-audit"), 1)
        self.assertIn("MERGE_AUTHORED_AUDIT_RESULT: ${{ needs.merge-authored-audit.result }}", self.gate)

    @unittest.skipUnless(BASH, "actual Bash is required to execute the aggregate")
    def test_failed_cancelled_missing_or_skipped_audit_blocks_pr_and_queue(self):
        script = textwrap.dedent(self.gate.split("        run: |\n", 1)[1].split("\n      - ", 1)[0])
        result_names = re.findall(r"(?m)^          ([A-Z][A-Z0-9_]*_RESULT):", self.gate)
        for event in ("pull_request", "merge_group"):
            for result in ("success", "failure", "cancelled", "skipped", "timed_out", "", "queued"):
                with self.subTest(event=event, result=result):
                    env = {**os.environ, **dict.fromkeys(result_names, "success")}
                    env.update({
                        "GITHUB_EVENT_NAME": event,
                        "MERGE_AUTHORED_AUDIT_RESULT": result,
                        "WINDOWS_ENABLED": "false", "MACOS_ENABLED": "false",
                        "LOONGARCH64_CHECK_ENABLED": "false",
                        "WINDOWS_RUST_REQUIRED_BY_POLICY": "false",
                        "RELAY_REQUIRED_BY_POLICY": "false", "BACKFLOW_MODE": "ordinary",
                    })
                    if event == "merge_group":
                        env["COMMIT_MESSAGE_RESULT"] = "skipped"
                    actual = subprocess.run(
                        [BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script],
                        cwd=ROOT, env=env, capture_output=True, text=True, encoding="utf-8", timeout=15,
                    )
                    self.assertEqual(actual.returncode == 0, result == "success", actual.stderr)
                    if result != "success":
                        self.assertIn("merge-authored-audit=", actual.stderr)


if __name__ == "__main__":
    unittest.main()
