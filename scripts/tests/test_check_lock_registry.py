"""Behavioral regressions for scripts/check-lock-registry.py (round-12).

The guard itself had CI wiring pins in test_ci_gate_policy.py but no
committed behavior tests: the mirror blacklist and the @img/* libc mapping
logic changes were only ever exercised against the real tree. These tests
run the guard's main() against synthetic trees (guard REPO_ROOT pointed at
a temp dir), so every failure class below is pinned without network and
without the real lockfiles:

- mirror blacklist: npmmirror (host AND bare-domain tarball form), taobao,
  tencent, huawei, cnpmjs, aliyun, ustc, 163, tuna — case- and port-insensitive;
- official registry passes clean;
- libc: strip and value-swap fail, exact mapping passes; a linux @img
  package the table does not list fails structurally when it lacks libc
  (round-21) and as "unexpected" when it carries one;
- lockfileVersion != 3 and malformed JSON fail closed;
- a tree with zero locks and a missing hub lock fail closed.
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "check-lock-registry.py"
SPEC = importlib.util.spec_from_file_location("check_lock_registry", SCRIPT)
assert SPEC and SPEC.loader
guard = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(guard)


def _lock(registry="https://registry.npmjs.org/", with_libc=True):
    # The @img/* packages are generated from the guard's own constant so the
    # clean-case fixture always matches EXPECTED_TEAM_RELAY_HUB_LIBC; a
    # legitimate constant change updates the fixture automatically.
    packages = {
        "": {"name": "hub", "version": "1.0.0", "dependencies": {}},
        "node_modules/left-pad": {
            "resolved": registry + "left-pad/-/left-pad-1.3.0.tgz"
        },
    }
    for suffix, libcs in guard.EXPECTED_TEAM_RELAY_HUB_LIBC.items():
        name = suffix[len("@img/") :]
        entry = {
            "resolved": registry + f"@img/{name}/-/{name}-0.35.5.tgz"
        }
        if with_libc:
            entry["libc"] = list(libcs)
        packages["node_modules/" + suffix] = entry
    return {"name": "hub", "lockfileVersion": 3, "packages": packages}


class LockRegistryGuardTests(unittest.TestCase):
    def _run(self, files):
        """Write the given tree (repo-relative paths), run guard main()
        with REPO_ROOT patched, return (exit_code, stderr)."""
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            for rel, text in files.items():
                path = root / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text, encoding="utf-8")
            stderr = io.StringIO()
            with patch.object(guard, "REPO_ROOT", str(root)), contextlib.redirect_stderr(
                stderr
            ):
                code = guard.main()
            return code, stderr.getvalue()

    def test_official_registry_is_clean(self):
        code, err = self._run(
            {
                "team-relay-hub/package-lock.json": json.dumps(_lock()),
                "other/package-lock.json": json.dumps(_lock()),
            }
        )
        self.assertEqual(code, 0, err)

    def test_npmmirror_host_and_bare_domain_forms_are_caught(self):
        for url in (
            "https://registry.npmmirror.com/left-pad/-/left-pad-1.3.0.tgz",
            "https://REGISTRY.NPMMIRROR.COM:443/left-pad/-/left-pad-1.3.0.tgz",
            # round-12: the bare-domain tarball form some tools write
            "https://npmmirror.com/mirrors/left-pad/-/left-pad-1.3.0.tgz",
        ):
            with self.subTest(url=url):
                code, err = self._run(
                    {
                        "team-relay-hub/package-lock.json": json.dumps(
                            _lock(registry="https://registry.npmjs.org/")
                        ),
                        "other/package-lock.json": json.dumps(_lock(registry=url)),
                    }
                )
                self.assertEqual(code, 1)
                self.assertIn("registry mirror", err)

    def test_other_cn_mirrors_are_caught(self):
        for url in (
            "https://mirrors.aliyun.com/npm/left-pad/-/left-pad-1.3.0.tgz",
            "https://mirrors.ustc.edu.cn/npm/left-pad/-/left-pad-1.3.0.tgz",
            "https://mirrors.163.com/npm/left-pad/-/left-pad-1.3.0.tgz",
            "https://mirrors.tuna.tsinghua.edu.cn/npm/left-pad/-/left-pad-1.3.0.tgz",
            "https://npm.taobao.org/left-pad/-/left-pad-1.3.0.tgz",
            "https://mirrors.cloud.tencent.com/npm/left-pad/-/left-pad-1.3.0.tgz",
            # round-22 review: the non-cloud tencent host is a separate
            # marker — deleting it survived this suite (only the -cloud
            # URL was pinned); each marker needs its own URL here.
            "https://mirrors.tencent.com/npm/left-pad/-/left-pad-1.3.0.tgz",
            "https://mirrors.huaweicloud.com/repository/npm/left-pad-1.3.0.tgz",
            "https://registry.cnpmjs.org/left-pad/-/left-pad-1.3.0.tgz",
        ):
            with self.subTest(url=url):
                code, err = self._run(
                    {
                        "team-relay-hub/package-lock.json": json.dumps(_lock()),
                        "other/package-lock.json": json.dumps(_lock(registry=url)),
                    }
                )
                self.assertEqual(code, 1)
                self.assertIn("registry mirror", err)

    def test_libc_strip_fails(self):
        code, err = self._run(
            {
                "team-relay-hub/package-lock.json": json.dumps(
                    _lock(with_libc=False)
                ),
            }
        )
        self.assertEqual(code, 1)
        self.assertIn("libc field stripped", err)
        self.assertIn("11.11", err)

    def test_libc_value_swap_fails(self):
        swapped = _lock()
        swapped["packages"]["node_modules/@img/sharp-linux-x64"]["libc"] = ["musl"]
        code, err = self._run(
            {"team-relay-hub/package-lock.json": json.dumps(swapped)}
        )
        self.assertEqual(code, 1)
        self.assertIn("libc value drift", err)

    def test_new_linux_img_package_without_libc_fails(self):
        # Round-21 review: the frozen table cannot see a platform package it
        # does not list, so a sharp bump that ADDS a linux @img entry and a
        # rewrite that strips its libc would slip past the set difference.
        # The structural rule (os=["linux"] without libc) must catch it.
        bumped = _lock()
        bumped["packages"]["node_modules/@img/sharp-linux-loong64"] = {
            "resolved": "https://registry.npmjs.org/@img/sharp-linux-loong64/-/sharp-linux-loong64-0.36.0.tgz",
            "os": ["linux"],
        }
        code, err = self._run(
            {"team-relay-hub/package-lock.json": json.dumps(bumped)}
        )
        self.assertEqual(code, 1)
        self.assertIn("no libc field", err)
        self.assertIn("sharp-linux-loong64", err)

    def test_new_linux_img_package_with_libc_is_reported_as_unexpected(self):
        # Same bump, healthy lock: not the strip signature — it stays on the
        # "unexpected package" path that asks for a table update instead.
        bumped = _lock()
        bumped["packages"]["node_modules/@img/sharp-linux-loong64"] = {
            "resolved": "https://registry.npmjs.org/@img/sharp-linux-loong64/-/sharp-linux-loong64-0.36.0.tgz",
            "os": ["linux"],
            "libc": ["glibc"],
        }
        code, err = self._run(
            {"team-relay-hub/package-lock.json": json.dumps(bumped)}
        )
        self.assertEqual(code, 1)
        self.assertIn("unexpected @img/* libc package", err)
        self.assertNotIn("no libc field", err)

    def test_lockfile_version_downgrade_fails(self):
        lock = _lock()
        lock["lockfileVersion"] = 2
        code, err = self._run(
            {
                "team-relay-hub/package-lock.json": json.dumps(lock),
            }
        )
        self.assertEqual(code, 1)
        self.assertIn("unsupported lockfileVersion 2", err)

    def test_malformed_lock_fails_closed(self):
        code, err = self._run(
            {
                "team-relay-hub/package-lock.json": "{ not json",
            }
        )
        self.assertEqual(code, 1)
        self.assertIn("unreadable lockfile", err)

    def test_zero_locks_fail_closed(self):
        code, err = self._run({"README.md": "no locks here"})
        self.assertEqual(code, 1)
        self.assertIn("no package-lock.json found", err)

    def test_missing_hub_lock_fails_closed(self):
        code, err = self._run(
            {"other/package-lock.json": json.dumps(_lock())}
        )
        self.assertEqual(code, 1)
        self.assertIn("team-relay-hub/package-lock.json not found", err)

    def test_unreadable_hub_lock_reports_parse_failure_not_not_found(self):
        # Round-13 review: an unreadable hub lock already reports the real
        # error, but the hub-not-parsed state then ALSO appended the
        # misleading "not found" message, sending the reader hunting for a
        # file that exists.
        code, err = self._run(
            {"team-relay-hub/package-lock.json": "{ not json"}
        )
        self.assertEqual(code, 1)
        self.assertIn("unreadable lockfile", err)
        self.assertNotIn("not found", err)
        self.assertIn("could not be parsed", err)

    def test_v3_lock_with_empty_packages_map_fails(self):
        # Round-13 review: a v3 lock whose `packages` map is missing or
        # empty made the mirror scan pass silently over nothing (no real
        # npm produces that — even a dependency-free lock carries the ""
        # root entry). Both the hub leg and a plain side lock must fail.
        empty = {"name": "x", "lockfileVersion": 3, "packages": {}}
        code, err = self._run(
            {
                "team-relay-hub/package-lock.json": json.dumps(_lock()),
                "other/package-lock.json": json.dumps(empty),
            }
        )
        self.assertEqual(code, 1)
        self.assertIn("packages` map is missing or empty", err)

        code, err = self._run(
            {
                "team-relay-hub/package-lock.json": json.dumps(empty),
            }
        )
        self.assertEqual(code, 1)
        self.assertIn("packages` map is missing or empty", err)


if __name__ == "__main__":
    unittest.main()
