import importlib.util
import os
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
    branch="backflow/community-186",
    repository="Pinvou/pinvou3",
    marker="<!-- community-backflow:Pinvou/pinvou-agent#186 -->",
    labels=None,
):
    return {
        "number": number,
        "headRefName": branch,
        "headRepository": {"nameWithOwner": repository},
        "body": marker,
        "labels": {"nodes": [{"name": label} for label in (labels or [])]},
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


class CiRustPolicyTests(unittest.TestCase):
    def test_accepts_only_matching_same_repository_backflow(self):
        trusted, _ = POLICY.trusted_backflow(pull_request(), "Pinvou/pinvou3")
        self.assertTrue(trusted)

        for pr in (
            pull_request(repository="someone/pinvou3"),
            pull_request(branch="feature/community-186"),
            pull_request(marker="<!-- community-backflow:Pinvou/pinvou-agent#185 -->"),
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
                "body": "<!-- community-backflow:Pinvou/pinvou-agent#186 -->",
                "labels": [],
            }
        }
        with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request"}):
            trusted, _ = POLICY.resolve_event(event, "Pinvou/pinvou3")
        self.assertTrue(trusted)

    def test_merge_group_resolves_the_active_queue_entry_by_head_sha(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_ref": "refs/heads/main",
            }
        }
        entries = queue_page(
            {
                "headCommit": {"oid": "queue-head"},
                "pullRequest": pull_request(),
            },
            {
                "headCommit": None,
                "pullRequest": pull_request(number=187),
            },
        )
        trusted, _ = POLICY.resolve_merge_group(
            event,
            "Pinvou/pinvou3",
            "token",
            fetch_page=lambda **_: entries,
        )
        self.assertTrue(trusted)

    def test_merge_group_fails_closed_for_mixed_or_unresolved_entries(self):
        event = {
            "merge_group": {
                "head_sha": "queue-head",
                "base_ref": "refs/heads/main",
            }
        }
        cases = (
            queue_page(),
            queue_page(
                {
                    "headCommit": {"oid": "queue-head"},
                    "pullRequest": pull_request(),
                },
                {
                    "headCommit": {"oid": "queue-head"},
                    "pullRequest": pull_request(
                        number=480,
                        branch="feat/llmapi-production-subdomain",
                        marker="",
                    ),
                },
            ),
            queue_page(
                {
                    "headCommit": {"oid": "queue-head"},
                    "pullRequest": pull_request(labels=["ci:full-rust"]),
                }
            ),
        )
        for response in cases:
            with self.subTest(response=response):
                trusted, _ = POLICY.resolve_merge_group(
                    event,
                    "Pinvou/pinvou3",
                    "token",
                    fetch_page=lambda response=response, **_: response,
                )
                self.assertFalse(trusted)


if __name__ == "__main__":
    unittest.main()
