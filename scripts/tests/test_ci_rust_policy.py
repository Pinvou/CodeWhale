import importlib.util
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ci-rust-policy.py"
SPEC = importlib.util.spec_from_file_location("ci_rust_policy", SCRIPT)
POLICY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POLICY)


def pull_request(
    number=186,
    branch=None,
    repository="Pinvou/pinvou3",
    base_repository="Pinvou/pinvou3",
    base_ref="main",
    marker=None,
    labels=None,
):
    branch = branch or f"backflow/community-{number}"
    if marker is None:
        marker = f"<!-- community-backflow:Pinvou/pinvou-agent#{number} -->"
    return {
        "number": number,
        "baseRefName": base_ref,
        "baseRepository": {"nameWithOwner": base_repository},
        "headRefName": branch,
        "headRepository": {"nameWithOwner": repository},
        "body": marker,
        "labels": {
            "totalCount": len(labels or []),
            "nodes": [{"name": label} for label in (labels or [])],
            "pageInfo": {"hasNextPage": False, "endCursor": None},
        },
    }


def queue_page(*entries):
    return {
        "data": {
            "repository": {
                "mergeQueue": {
                    "entries": {
                        "nodes": list(entries),
                        "pageInfo": {"hasNextPage": False, "endCursor": None},
                    }
                }
            }
        }
    }


def change(path, before="", after="", patch_text=""):
    return POLICY.FileChange(
        path,
        "M",
        before.encode("utf-8"),
        after.encode("utf-8"),
        patch_text,
    )


def release_changes(old="0.9.0", new="0.9.1"):
    return [
        change("VERSION", f"{old}\n", f"{new}\n"),
        change(
            "pinvou3-app/src-tauri/tauri.conf.json",
            f'{{"version":"{old}","productName":"Pinvou"}}\n',
            f'{{"version":"{new}","productName":"Pinvou"}}\n',
        ),
        change(
            "pinvou3-app/src-tauri/Cargo.toml",
            f'[package]\nname = "pinvou3-tauri"\nversion = "{old}"\n\n[features]\ndefault = []\n',
            f'[package]\nname = "pinvou3-tauri"\nversion = "{new}"\n\n[features]\ndefault = []\n',
        ),
        change(
            "pinvou-knowledge/Cargo.toml",
            f'[package]\nname = "pinvou-knowledge"\nversion = "{old}"\n',
            f'[package]\nname = "pinvou-knowledge"\nversion = "{new}"\n',
        ),
        change(
            "pinvou-knowledge/Cargo.lock",
            f'[[package]]\nname = "pinvou-knowledge"\nversion = "{old}"\n',
            f'[[package]]\nname = "pinvou-knowledge"\nversion = "{new}"\n',
        ),
        change(
            "pinvou3-app/src-tauri/Cargo.lock",
            f'[[package]]\nname = "pinvou-knowledge"\nversion = "{old}"\n\n'
            f'[[package]]\nname = "pinvou3-tauri"\nversion = "{old}"\n',
            f'[[package]]\nname = "pinvou-knowledge"\nversion = "{new}"\n\n'
            f'[[package]]\nname = "pinvou3-tauri"\nversion = "{new}"\n',
        ),
        change(
            "pinvou3-app/package.json",
            f'{{"name":"pinvou3","version":"{old}"}}\n',
            f'{{"name":"pinvou3","version":"{new}"}}\n',
        ),
        change(
            "pinvou3-app/package-lock.json",
            f'{{"name":"pinvou3","version":"{old}","packages":{{"":{{"version":"{old}"}}}}}}\n',
            f'{{"name":"pinvou3","version":"{new}","packages":{{"":{{"version":"{new}"}}}}}}\n',
        ),
    ]


class CiRustPolicyTests(unittest.TestCase):
    def test_accepts_only_matching_same_repository_backflow(self):
        trusted, _ = POLICY.trusted_backflow(pull_request(), "Pinvou/pinvou3")
        self.assertTrue(trusted)

        for pr in (
            pull_request(repository="someone/pinvou3"),
            pull_request(base_repository="someone/pinvou3"),
            pull_request(base_ref="evil-feature"),
            pull_request(branch="feature/community-186"),
            pull_request(marker="<!-- community-backflow:Pinvou/pinvou-agent#185 -->"),
            pull_request(
                marker=(
                    "<!-- community-backflow:Pinvou/pinvou-agent#186 -->\n"
                    "<!-- community-backflow:Pinvou/pinvou-agent#186 -->"
                )
            ),
            pull_request(labels=["ci:full-rust"]),
        ):
            with self.subTest(pr=pr):
                trusted, _ = POLICY.trusted_backflow(pr, "Pinvou/pinvou3")
                self.assertFalse(trusted)

    def test_pull_request_payload_uses_the_same_trust_contract(self):
        event = {
            "pull_request": {
                "head": {
                    "ref": "backflow/community-186",
                    "repo": {"full_name": "Pinvou/pinvou3"},
                },
                "base": {
                    "ref": "main",
                    "repo": {"full_name": "Pinvou/pinvou3"},
                },
                "body": "<!-- community-backflow:Pinvou/pinvou-agent#186 -->",
                "labels": [],
            }
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request"}):
            trusted, _ = POLICY.resolve_event(event, "Pinvou/pinvou3")
        self.assertTrue(trusted)

    def test_merge_group_walks_and_validates_every_entry_in_the_commit_chain(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        entries = queue_page(
            {
                "baseCommit": {"oid": "base-main"},
                "headCommit": {"oid": "first-head"},
                "pullRequest": pull_request(number=186),
            },
            {
                "baseCommit": {"oid": "first-head"},
                "headCommit": {"oid": "queue-head"},
                "pullRequest": pull_request(number=187),
            },
            {
                "baseCommit": {"oid": "unrelated-base"},
                "headCommit": {"oid": "unrelated-head"},
                "pullRequest": pull_request(number=188),
            },
        )
        trusted, reason = POLICY.resolve_merge_group(
            event,
            "Pinvou/pinvou3",
            "token",
            fetch_page=lambda **_: entries,
        )
        self.assertTrue(trusted)
        self.assertIn("#186, #187", reason)

    def test_merge_group_rejects_a_mixed_provable_chain(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        response = queue_page(
            {
                "baseCommit": {"oid": "base-main"},
                "headCommit": {"oid": "first-head"},
                "pullRequest": pull_request(number=186),
            },
            {
                "baseCommit": {"oid": "first-head"},
                "headCommit": {"oid": "queue-head"},
                "pullRequest": pull_request(
                    number=480,
                    branch="feat/llmapi-production-subdomain",
                    marker="no marker",
                ),
            },
        )
        trusted, reason = POLICY.resolve_merge_group(
            event,
            "Pinvou/pinvou3",
            "token",
            fetch_page=lambda **_: response,
        )
        self.assertFalse(trusted)
        self.assertIn("#480", reason)
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda **_: response,
                inspect_diff=lambda *_: self.fail("mixed group used trusted diff routing"),
            )
        self.assertEqual(decision[:2], (False, "ordinary"))

    def test_unprovable_merge_group_fails_closed_to_full_mode(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda **_: queue_page(),
            )
        self.assertEqual(decision[:2], (False, "full"))
        self.assertIn("failed closed", decision.reason)

    def test_incomplete_merge_queue_pull_request_fails_closed_to_full_mode(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        incomplete_entries = []

        null_pr = {
            "baseCommit": {"oid": "base-main"},
            "headCommit": {"oid": "queue-head"},
            "pullRequest": None,
        }
        incomplete_entries.append(("null pull request", null_pr))

        partial_pr = pull_request()
        del partial_pr["headRepository"]
        incomplete_entries.append(
            (
                "missing critical field",
                {
                    "baseCommit": {"oid": "base-main"},
                    "headCommit": {"oid": "queue-head"},
                    "pullRequest": partial_pr,
                },
            )
        )

        for name, entry in incomplete_entries:
            with self.subTest(name=name), patch.dict(
                os.environ, {"GITHUB_EVENT_NAME": "merge_group"}
            ):
                decision = POLICY.resolve_routing_event(
                    event,
                    "Pinvou/pinvou3",
                    "token",
                    fetch_page=lambda **_: queue_page(entry),
                    inspect_diff=lambda *_: self.fail(
                        "incomplete queue entry used trusted diff routing"
                    ),
                )
            self.assertEqual(decision[:2], (False, "full"))
            self.assertIn("failed closed", decision.reason)

    def test_truncated_merge_queue_labels_fail_closed_to_full_mode(self):
        self.assertIn("totalCount", POLICY.QUEUE_QUERY)
        self.assertIn("pageInfo { hasNextPage endCursor }", POLICY.QUEUE_QUERY)
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        pr = pull_request(labels=[f"label-{index}" for index in range(100)])
        pr["labels"]["totalCount"] = 101
        pr["labels"]["pageInfo"] = {
            "hasNextPage": True,
            "endCursor": "opaque-cursor",
        }
        entry = {
            "baseCommit": {"oid": "base-main"},
            "headCommit": {"oid": "queue-head"},
            "pullRequest": pr,
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda **_: queue_page(entry),
                inspect_diff=lambda *_: self.fail(
                    "truncated queue labels used trusted diff routing"
                ),
            )
        self.assertEqual(decision[:2], (False, "full"))
        self.assertIn("labels were truncated", decision.reason)

    def test_visible_full_rust_queue_label_forces_full_mode(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        entry = {
            "baseCommit": {"oid": "base-main"},
            "headCommit": {"oid": "queue-head"},
            "pullRequest": pull_request(labels=["ci:full-rust"]),
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda **_: queue_page(entry),
                inspect_diff=lambda *_: self.fail(
                    "ci:full-rust queue entry used trusted diff routing"
                ),
            )
        self.assertEqual(decision[:2], (False, "full"))
        self.assertIn("ci:full-rust", decision.reason)

    def test_pagination_loops_fail_closed_to_full_mode(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        entry = {
            "baseCommit": {"oid": "base-main"},
            "headCommit": {"oid": "queue-head"},
            "pullRequest": pull_request(),
        }

        def page_with(has_next_page, end_cursor):
            return {
                "data": {
                    "repository": {
                        "mergeQueue": {
                            "entries": {
                                "nodes": [entry],
                                "pageInfo": {
                                    "hasNextPage": has_next_page,
                                    "endCursor": end_cursor,
                                },
                            }
                        }
                    }
                }
            }

        repeated = page_with(True, "same-cursor")
        repeated_pages = {None: repeated, "same-cursor": repeated}
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda cursor=None, **_: repeated_pages[cursor],
                inspect_diff=lambda *_: self.fail(
                    "cyclic pagination used trusted diff routing"
                ),
            )
        self.assertEqual(decision[:2], (False, "full"))
        self.assertIn("cursor repeated", decision.reason)

        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda **_: page_with(True, None),
                inspect_diff=lambda *_: self.fail(
                    "missing pagination cursor used trusted diff routing"
                ),
            )
        self.assertEqual(decision[:2], (False, "full"))
        self.assertIn("cursor is missing", decision.reason)

    def test_untraceable_merge_group_chains_fail_closed_to_full_mode(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }

        def entry(base, head, number=186):
            return {
                "baseCommit": {"oid": base},
                "headCommit": {"oid": head},
                "pullRequest": pull_request(number=number),
            }

        responses = (
            ("broken chain", queue_page(entry("orphan-base", "queue-head"))),
            (
                "duplicate head commit",
                queue_page(
                    entry("base-main", "queue-head", 186),
                    entry("base-main", "queue-head", 187),
                ),
            ),
            (
                "entry chain cycle",
                queue_page(
                    entry("second-head", "queue-head", 186),
                    entry("queue-head", "second-head", 187),
                ),
            ),
        )
        for name, response in responses:
            with self.subTest(name=name), patch.dict(
                os.environ, {"GITHUB_EVENT_NAME": "merge_group"}
            ):
                decision = POLICY.resolve_routing_event(
                    event,
                    "Pinvou/pinvou3",
                    "token",
                    fetch_page=lambda response=response, **_: response,
                    inspect_diff=lambda *_: self.fail(
                        "untraceable chain used trusted diff routing"
                    ),
                )
            self.assertEqual(decision[:2], (False, "full"))
            self.assertIn("failed closed", decision.reason)

    def test_malformed_queue_graphql_structure_fails_closed_without_attribute_error(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_sha": "base-main",
                "base_ref": "refs/heads/main",
            }
        }
        valid_entry = {
            "baseCommit": {"oid": "base-main"},
            "headCommit": {"oid": "queue-head"},
            "pullRequest": pull_request(),
        }
        malformed_responses = (
            ("scalar result", "not-an-object"),
            ("scalar data", {"data": "not-an-object"}),
            ("missing repository", {"data": {}}),
            ("scalar repository", {"data": {"repository": 7}}),
            (
                "missing merge queue",
                {"data": {"repository": {}}},
            ),
            (
                "scalar entries",
                {"data": {"repository": {"mergeQueue": {"entries": 7}}}},
            ),
            (
                "scalar nodes",
                {
                    "data": {
                        "repository": {
                            "mergeQueue": {
                                "entries": {
                                    "nodes": 7,
                                    "pageInfo": {
                                        "hasNextPage": False,
                                        "endCursor": None,
                                    },
                                }
                            }
                        }
                    }
                },
            ),
            (
                "null node",
                queue_page(None),
            ),
            (
                "scalar entry",
                queue_page("not-an-entry"),
            ),
            (
                "scalar head commit",
                queue_page({**valid_entry, "headCommit": "queue-head"}),
            ),
            (
                "missing base commit",
                queue_page(
                    {key: value for key, value in valid_entry.items() if key != "baseCommit"}
                ),
            ),
            (
                "missing page info",
                {
                    "data": {
                        "repository": {
                            "mergeQueue": {
                                "entries": {"nodes": [valid_entry]}
                            }
                        }
                    }
                },
            ),
            (
                "incomplete page info",
                {
                    "data": {
                        "repository": {
                            "mergeQueue": {
                                "entries": {
                                    "nodes": [valid_entry],
                                    "pageInfo": {"hasNextPage": False},
                                }
                            }
                        }
                    }
                },
            ),
        )
        for name, response in malformed_responses:
            with self.subTest(name=name), patch.dict(
                os.environ, {"GITHUB_EVENT_NAME": "merge_group"}
            ):
                decision = POLICY.resolve_routing_event(
                    event,
                    "Pinvou/pinvou3",
                    "token",
                    fetch_page=lambda response=response, **_: response,
                    inspect_diff=lambda *_: self.fail(
                        "malformed queue response used trusted diff routing"
                    ),
                )
            self.assertEqual(decision[:2], (False, "full"))
            self.assertIn("failed closed", decision.reason)

    def test_scalar_workflow_event_does_not_escape_policy_main(self):
        with tempfile.TemporaryDirectory() as directory:
            event_path = Path(directory) / "event.json"
            output_path = Path(directory) / "output"
            event_path.write_text("[]", encoding="utf-8")
            with patch.dict(
                os.environ,
                {
                    "GITHUB_EVENT_PATH": str(event_path),
                    "GITHUB_EVENT_NAME": "merge_group",
                    "GITHUB_REPOSITORY": "Pinvou/pinvou3",
                    "GITHUB_OUTPUT": str(output_path),
                },
                clear=False,
            ):
                self.assertEqual(POLICY.main(), 0)
            values = output_path.read_text(encoding="utf-8")
        self.assertIn("trusted_backflow=false", values)
        self.assertIn("backflow_mode=full", values)

    def test_pure_release_requires_every_mechanical_sync_version_field(self):
        pure, reason = POLICY.pure_release_change(release_changes())
        self.assertTrue(pure, reason)
        mode, _ = POLICY.classify_trusted_diff(release_changes())
        self.assertEqual(mode, "release")

        missing_derived = release_changes()[:-1]
        pure, _ = POLICY.pure_release_change(missing_derived)
        self.assertFalse(pure)
        self.assertEqual(
            POLICY.classify_trusted_diff(missing_derived)[0], "windows_full"
        )

    def test_release_content_anomaly_and_mixed_change_fail_closed(self):
        anomalous = release_changes()
        cargo = anomalous[2]
        anomalous[2] = cargo._replace(
            after=cargo.after + b'new-feature = ["dep:new"]\n'
        )
        pure, reason = POLICY.pure_release_change(anomalous)
        self.assertFalse(pure)
        self.assertIn("beyond managed version fields", reason)
        self.assertEqual(POLICY.classify_trusted_diff(anomalous)[0], "windows_full")

        mixed = release_changes() + [
            change(
                "pinvou3-app/src-tauri/src/features/chat/mod.rs",
                "fn old() {}\n",
                "fn new() {}\n",
                "-fn old() {}\n+fn new() {}\n",
            )
        ]
        self.assertEqual(POLICY.classify_trusted_diff(mixed)[0], "windows_full")

    def test_version_file_anomalies_reject_the_release_route(self):
        for name, before, after in (
            ("invalid semver", "0.9.0\n", "not-a-version\n"),
            ("leading blank line", "0.9.0\n", "\n0.9.1\n"),
            ("trailing note", "0.9.0\n", "0.9.1\n# release notes\n"),
        ):
            with self.subTest(name=name):
                anomalous = release_changes()
                anomalous[0] = change("VERSION", before, after)
                pure, _ = POLICY.pure_release_change(anomalous)
                self.assertFalse(pure)

    def test_release_version_targets_match_sync_version_targets(self):
        source = (ROOT / "scripts" / "sync-version.mjs").read_text(encoding="utf-8")
        managed = {
            re.sub(r"\([^)]*\)$", "", target)
            for target in re.findall(r"name:\s*'([^']+)'", source)
        }
        managed.add("VERSION")
        self.assertEqual(managed, set(POLICY.RELEASE_VERSION_TARGETS))

    def test_windows_high_risk_paths_and_changed_semantics_stay_full(self):
        cases = (
            [change("CodeWhale")],
            [change("pinvou3-app/src-tauri/src/platform/paths.rs")],
            [change("pinvou3-app/scripts/tauri/windows-installer.js")],
            [change(".github/workflows/pr-check.yml")],
            [change("scripts/ci-rust-policy.py")],
            [
                change(
                    "pinvou3-app/src-tauri/tauri.conf.json",
                    '{"version":"0.9.0"}\n',
                    '{"version":"0.9.0","bundle":{"active":true}}\n',
                )
            ],
            [
                change(
                    "pinvou3-app/src-tauri/src/features/chat/mod.rs",
                    after='let child = std::process::Command::new("tool").spawn();\n',
                    patch_text='+let child = std::process::Command::new("tool").spawn();\n',
                )
            ],
            [
                change(
                    "pinvou3-app/src-tauri/src/features/chat/launcher.rs",
                    before=(
                        "#[cfg(windows)]\n"
                        "fn launch(command: &mut Command) {\n"
                        "    command.creation_flags(OLD_FLAG);\n"
                        "}\n"
                    ),
                    after=(
                        "#[cfg(windows)]\n"
                        "fn launch(command: &mut Command) {\n"
                        "    command.creation_flags(CREATE_NO_WINDOW);\n"
                        "}\n"
                    ),
                    patch_text=(
                        "-    command.creation_flags(OLD_FLAG);\n"
                        "+    command.creation_flags(CREATE_NO_WINDOW);\n"
                    ),
                )
            ],
        )
        for changes in cases:
            with self.subTest(changes=changes):
                self.assertEqual(
                    POLICY.classify_trusted_diff(changes)[0], "windows_full"
                )

    def test_plain_cross_platform_rust_uses_portable_route(self):
        changes = [
            change(
                "pinvou3-app/src-tauri/src/features/feedback/store.rs",
                patch_text="-let score = 1;\n+let score = 2;\n",
            )
        ]
        self.assertEqual(POLICY.classify_trusted_diff(changes)[0], "portable_rust")

    def test_non_rust_backflow_routes_without_the_windows_leg(self):
        changes = [change("docs/community-backflow.md", "# title\n", "# changed\n")]
        mode, reason = POLICY.classify_trusted_diff(changes)
        self.assertEqual(mode, "non_rust")
        self.assertIn("non-Rust", reason)

    def test_empty_trusted_diff_fails_closed_to_full_mode(self):
        pull_event = {
            "pull_request": {
                **pull_request(),
                "base": {"sha": "a" * 40},
                "head": {"sha": "b" * 40},
            }
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request"}):
            decision = POLICY.resolve_routing_event(
                pull_event,
                "Pinvou/pinvou3",
                inspect_diff=lambda *_: [],
            )
        self.assertEqual(decision[:2], (False, "full"))
        self.assertIn("diff routing failed closed", decision.reason)

    def test_emit_fails_closed_on_unknown_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "github-output"
            with patch.dict(os.environ, {"GITHUB_OUTPUT": str(output)}):
                POLICY._emit(POLICY.RoutingDecision(True, "stealth", "unexpected"))
            values = output.read_text(encoding="utf-8")
        self.assertIn("trusted_backflow=false", values)
        self.assertIn("backflow_mode=full", values)
        self.assertIn("pure_release_backflow=false", values)
        self.assertIn("unknown mode", values)

    def test_routing_uses_event_base_head_and_emits_fail_closed_outputs(self):
        base_sha = "a" * 40
        head_sha = "b" * 40
        event = {
            "pull_request": {
                **pull_request(),
                "base": {"sha": base_sha},
                "head": {"sha": head_sha},
            }
        }
        observed = []
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request"}):
            decision = POLICY.resolve_routing_event(
                event,
                "Pinvou/pinvou3",
                inspect_diff=lambda base, head: observed.append((base, head))
                or release_changes(),
            )
        self.assertEqual(observed, [(base_sha, head_sha)])
        self.assertEqual(decision.mode, "release")

        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "github-output"
            with patch.dict(os.environ, {"GITHUB_OUTPUT": str(output)}):
                POLICY._emit(decision)
            values = output.read_text(encoding="utf-8")
        self.assertIn("trusted_backflow=true", values)
        self.assertIn("backflow_mode=release", values)
        self.assertIn("pure_release_backflow=true", values)

    def test_forged_backflow_never_reaches_semantic_diff_inspection(self):
        for pr in (
            pull_request(repository="attacker/pinvou3"),
            pull_request(base_repository="attacker/pinvou3"),
            pull_request(base_ref="evil-feature"),
        ):
            event = {
                "pull_request": {
                    **pr,
                    "base": {"sha": "a" * 40},
                    "head": {"sha": "b" * 40},
                }
            }
            with self.subTest(pr=pr), patch.dict(
                os.environ, {"GITHUB_EVENT_NAME": "pull_request"}
            ):
                decision = POLICY.resolve_routing_event(
                    event,
                    "Pinvou/pinvou3",
                    inspect_diff=lambda *_: self.fail("untrusted PR diff was inspected"),
                )
            self.assertFalse(decision.trusted)
            self.assertEqual(decision.mode, "ordinary")

    def test_api_and_diff_failures_return_the_ordinary_full_route(self):
        queue_event = {
            "merge_group": {
                "head_sha": "b" * 40,
                "base_sha": "a" * 40,
                "base_ref": "refs/heads/main",
            }
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "merge_group"}):
            api_failure = POLICY.resolve_routing_event(
                queue_event,
                "Pinvou/pinvou3",
                "token",
                fetch_page=lambda **_: (_ for _ in ()).throw(
                    RuntimeError("temporary API failure")
                ),
            )
        self.assertEqual(api_failure[:2], (False, "full"))
        self.assertIn("failed closed", api_failure.reason)

        pull_event = {
            "pull_request": {
                **pull_request(),
                "base": {"sha": "a" * 40},
                "head": {"sha": "b" * 40},
            }
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request"}):
            diff_failure = POLICY.resolve_routing_event(
                pull_event,
                "Pinvou/pinvou3",
                inspect_diff=lambda *_: (_ for _ in ()).throw(
                    RuntimeError("missing base object")
                ),
            )
        self.assertEqual(diff_failure[:2], (False, "full"))
        self.assertIn("failed closed", diff_failure.reason)

    def test_git_inspection_reads_the_exact_commit_range(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q"], cwd=root, check=True)
            subprocess.run(
                ["git", "config", "user.email", "ci-policy@example.invalid"],
                cwd=root,
                check=True,
            )
            subprocess.run(
                ["git", "config", "user.name", "CI Policy Test"],
                cwd=root,
                check=True,
            )
            source = root / "pinvou3-app/src-tauri/src/features/feedback/store.rs"
            source.parent.mkdir(parents=True)
            source.write_text("fn score() -> u8 { 1 }\n", encoding="utf-8")
            subprocess.run(["git", "add", "."], cwd=root, check=True)
            subprocess.run(["git", "commit", "-qm", "base"], cwd=root, check=True)
            base = subprocess.check_output(
                ["git", "rev-parse", "HEAD"], cwd=root, text=True
            ).strip()

            source.write_text("fn score() -> u8 { 2 }\n", encoding="utf-8")
            subprocess.run(["git", "commit", "-qam", "head"], cwd=root, check=True)
            head = subprocess.check_output(
                ["git", "rev-parse", "HEAD"], cwd=root, text=True
            ).strip()

            changes = POLICY.inspect_git_diff(base, head, root)
        self.assertEqual([item.path for item in changes], [source.relative_to(root).as_posix()])
        self.assertEqual(changes[0].before, b"fn score() -> u8 { 1 }\n")
        self.assertEqual(changes[0].after, b"fn score() -> u8 { 2 }\n")
        self.assertIn("+fn score() -> u8 { 2 }", changes[0].patch)
        self.assertEqual(POLICY.classify_trusted_diff(changes)[0], "portable_rust")


if __name__ == "__main__":
    unittest.main()
