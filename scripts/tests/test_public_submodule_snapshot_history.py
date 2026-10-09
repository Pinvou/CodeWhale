"""Real Git object/ancestry checks against an isolated controlled bare remote.

Only fixture constants point to a file transport. The shipped verifier retains
its exact HTTPS URL; no global URL rewrite or pre-existing object store is used.
"""
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
BASH = "C:/Program Files/Git/bin/bash.exe" if os.name == "nt" and Path("C:/Program Files/Git/bin/bash.exe").exists() else shutil.which("bash")
GIT = shutil.which("git")
URL = "https://github.com/Pinvou/CodeWhale.git"
BASELINE = "61cb769be5b33abc64f64da4272f5b39a8b6c1fd"


@unittest.skipUnless(BASH and GIT, "real Git and bash required")
class PublicSnapshotHistoryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.env = os.environ.copy()
        for key in list(self.env):
            if key.startswith("GIT_") or key in ("SSH_ASKPASS",):
                self.env.pop(key)
        self.env.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")
        self.git(self.source, "init", "--quiet")
        self.git(self.source, "config", "user.name", "Controlled fixture")
        self.git(self.source, "config", "user.email", "fixture@example.invalid")
        self.base = self.commit("baseline")
        self.pin = self.commit("registered")
        self.advanced = self.commit("maintenance advanced")
        self.git(self.source, "branch", "pinvou3-clean", self.advanced)
        self.git(self.source, "tag", "pinvou-v0.9.12-r3", self.base)
        self.git(self.source, "checkout", "--quiet", "--detach", self.pin)
        self.candidate = self.commit("published candidate outside maintenance")
        self.git(self.source, "branch", "candidate", self.candidate)
        self.remote = self.root / "public.git"
        self.git(self.root, "clone", "--quiet", "--bare", str(self.source), str(self.remote))
        self.parent = self.root / "parent"
        (self.parent / "scripts").mkdir(parents=True)
        self.git(self.parent, "init", "--quiet")
        self.remote_url = self.remote.as_uri()
        (self.parent / ".gitmodules").write_text(
            '[submodule "CodeWhale"]\n path = CodeWhale\n url = ' + self.remote_url + '\n', encoding="utf-8")
        self.scratch = self.root / "scratch"
        self.scratch.mkdir()
        self.env["TMPDIR"] = self.scratch.as_posix()
        self.configure(self.pin)

    def git(self, cwd, *args):
        result = subprocess.run([GIT, "-C", str(cwd), *args], env=self.env,
                                text=True, encoding="utf-8", capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def commit(self, message):
        self.git(self.source, "commit", "--quiet", "--allow-empty", "-m", message)
        return self.git(self.source, "rev-parse", "HEAD")

    def configure(self, pin, registered=None, closed=False):
        self.git(self.parent, "update-index", "--add", "--cacheinfo", "160000", pin, "CodeWhale")
        (self.parent / "scripts/fork-guard.sh").write_text(
            f'EXPECTED_HEAD="{registered or pin}"\n', encoding="utf-8")
        script = (ROOT / "scripts/verify-public-submodule.sh").read_text(encoding="utf-8")
        script = script.replace(URL, self.remote_url)
        script = script.replace(f'TRANSITION_BASELINE="{BASELINE}"',
                                f'TRANSITION_BASELINE="{self.base if not closed else ""}"')
        (self.parent / "scripts/verify-public-submodule.sh").write_text(script, encoding="utf-8")

    def run_verifier(self, *arguments, ambient=None):
        env = self.env.copy()
        env.update(ambient or {})
        verifier = (self.parent / "scripts/verify-public-submodule.sh").as_posix()
        command = [BASH, verifier, *arguments]
        if "PINVOU_WRAPPER_BIN" in env:
            env["PINVOU_VERIFIER"] = verifier
            command = [BASH, "-c", 'if command -v cygpath >/dev/null; then p="$(cygpath -u "$PINVOU_WRAPPER_BIN")"; else p="$PINVOU_WRAPPER_BIN"; fi; export PATH="$p:$PATH"; exec bash "$PINVOU_VERIFIER" "$@"', "verifier", *arguments]
        result = subprocess.run(command,
                                cwd=self.parent, env=env, text=True, encoding="utf-8",
                                errors="replace", capture_output=True, timeout=60)
        self.assertEqual(list(self.scratch.iterdir()), [], "temporary bare repository is cleaned on success and failure")
        return result

    def test_snapshot_passes_real_advanced_maintenance_but_default_release_rejects(self):
        result = self.run_verifier("--registered-snapshot")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("冻结审核校验通过", result.stdout)
        self.assertNotEqual(self.run_verifier().returncode, 0)

    def test_remote_children_cannot_inherit_tls_disable_or_askpass(self):
        wrapper = self.root / "transport-bin"
        wrapper.mkdir()
        script = wrapper / "git"
        script.write_text(
            '#!/usr/bin/env bash\nset -euo pipefail\n'
            'for arg in "$@"; do\n'
            ' if [[ "$arg" == ls-remote || "$arg" == fetch ]]; then\n'
            '  [[ -z "${GIT_SSL_NO_VERIFY:-}" && -z "${GIT_ASKPASS:-}" && -z "${SSH_ASKPASS:-}" ]] || exit 128\n'
            ' fi\ndone\nexec "$PINVOU_REAL_GIT" "$@"\n', encoding="utf-8")
        script.chmod(0o755)
        result = self.run_verifier("--registered-snapshot", ambient={
            "PINVOU_WRAPPER_BIN": wrapper.as_posix(), "PINVOU_REAL_GIT": Path(GIT).as_posix(),
            "GIT_SSL_NO_VERIFY": "1", "GIT_ASKPASS": "invalid-helper", "SSH_ASKPASS": "invalid-helper",
        })
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_public_candidate_is_rejected_even_when_registered(self):
        self.configure(self.candidate)
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)

    def test_unregistered_index_pin_is_rejected(self):
        self.configure(self.pin, registered=self.advanced)
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)

    def test_registered_noncommit_is_rejected(self):
        tree = self.git(self.source, "rev-parse", f"{self.pin}^{{tree}}")
        self.configure(tree)
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)

    def test_moved_tag_is_rejected(self):
        self.git(self.remote, "update-ref", "refs/tags/pinvou-v0.9.12-r3", self.pin)
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)

    def test_annotated_tag_peels_to_fixed_commit(self):
        self.git(self.source, "tag", "-f", "-a", "pinvou-v0.9.12-r3", self.base, "-m", "fixed baseline")
        self.git(self.source, "push", "--quiet", "--force", str(self.remote), "refs/tags/pinvou-v0.9.12-r3")
        result = self.run_verifier("--registered-snapshot")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_pin_outside_fixed_tag_history_is_rejected(self):
        self.git(self.source, "checkout", "--quiet", "--orphan", "unrelated")
        unrelated = self.commit("unrelated root")
        self.git(self.source, "push", "--quiet", str(self.remote), "HEAD:refs/heads/unrelated")
        self.git(self.remote, "update-ref", "refs/heads/pinvou3-clean", unrelated)
        self.configure(unrelated)
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)

    def test_local_rewrites_and_ambient_credentials_objects_cannot_bypass_public_fetch(self):
        # A caller-local rewrite would make the valid remote disappear if it
        # leaked into ls-remote. Both remote calls must use the fresh bare repo.
        self.git(self.parent, "config", 'url.file:///absent-controlled-fixture/.insteadOf', self.remote_url)
        ambient = {
            "GIT_ASKPASS": "invalid-credential-helper", "SSH_ASKPASS": "invalid-credential-helper",
            "GIT_CONFIG_PARAMETERS": "invalid injected config", "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "url.file:///absent-controlled-fixture/.insteadOf",
            "GIT_CONFIG_VALUE_0": self.remote_url,
            "GIT_OBJECT_DIRECTORY": "/absent-controlled-object-store",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES": "/absent-controlled-alternate",
            "GIT_DIR": "/absent-controlled-repository",
            "GIT_INDEX_FILE": "/absent-controlled-index",
        }
        result = self.run_verifier("--registered-snapshot", ambient=ambient)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_missing_remote_cannot_use_local_objects(self):
        self.git(self.remote, "update-ref", "-d", "refs/heads/pinvou3-clean")
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)

    def test_closed_window_keeps_three_way_equality(self):
        self.configure(self.pin, closed=True)
        self.git(self.remote, "update-ref", "refs/tags/pinvou-v0.9.12-r3", self.pin)
        self.assertNotEqual(self.run_verifier("--registered-snapshot").returncode, 0)
        self.git(self.remote, "update-ref", "refs/heads/pinvou3-clean", self.pin)
        result = self.run_verifier("--registered-snapshot")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
