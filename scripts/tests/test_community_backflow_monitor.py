import importlib.util
import io
import sys
import unittest
from contextlib import redirect_stdout
from pathlib import Path


SCRIPT_PATH = (
    Path(__file__).resolve().parents[1] / "community-backflow-monitor.py"
)
SPEC = importlib.util.spec_from_file_location("community_backflow_monitor", SCRIPT_PATH)
assert SPEC and SPEC.loader
monitor = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = monitor
SPEC.loader.exec_module(monitor)


class CommunityBackflowMonitorTests(unittest.TestCase):
    def test_schedule_covers_weekday_utc8_business_hours(self):
        workflow = (
            Path(__file__).resolve().parents[2]
            / ".github/workflows/community-backflow-monitor.yml"
        ).read_text(encoding="utf-8")
        self.assertIn('cron: "0 1-10 * * 1-5"', workflow)
        self.assertIn("UTC+8", workflow)
        self.assertIn("issues: write", workflow)

    def test_classify_private_overlap_as_semantic_merge(self):
        recommendation, reason = monitor.classify_paths(
            ["pinvou3-app/src-tauri/src/features/updater/mod.rs"]
        )
        self.assertEqual(recommendation, "需要语义合并")
        self.assertIn("Official", reason)

    def test_classify_shared_code_as_needing_review(self):
        recommendation, _ = monitor.classify_paths(
            ["pinvou3-app/src/features/chat/ChatView.jsx"]
        )
        self.assertEqual(recommendation, "需要评估")

    def test_classify_community_governance_without_auto_skip(self):
        recommendation, reason = monitor.classify_paths(
            ["LICENSE", ".github/ISSUE_TEMPLATE/bug_report.yml"]
        )
        self.assertEqual(recommendation, "可能仅 Community")
        self.assertIn("人工确认", reason)

    def test_release_pull_is_detected_from_version_diff(self):
        self.assertTrue(monitor.is_release_pull(["VERSION", "README.md"]))
        self.assertFalse(
            monitor.is_release_pull(["docs/release-notes.md", "README.md"])
        )

    def test_issue_body_is_traceable_and_limits_path_output(self):
        pull = {
            "number": 42,
            "html_url": "https://github.com/Pinvou/pinvou-agent/pull/42",
            "title": "fix: 修复示例问题",
            "merged_at": "2026-07-27T08:00:00Z",
            "user": {"login": "contributor"},
        }
        files = [
            {
                "filename": f"pinvou3-app/src/file-{index}.js",
                "additions": 1,
                "deletions": 2,
            }
            for index in range(65)
        ]

        body = monitor.render_issue_body("Pinvou/pinvou-agent", pull, files)

        self.assertIn(
            "<!-- community-backflow:Pinvou/pinvou-agent#42 -->", body
        )
        self.assertIn("https://github.com/Pinvou/pinvou-agent/pull/42", body)
        self.assertIn("65 个文件，`+65 / -130`", body)
        self.assertIn("Release 回流：**否**", body)
        self.assertIn("其余 5 个文件", body)
        self.assertNotIn("file-64.js", body)
        self.assertIn("均是不可信输入", body)

    def test_state_labels_cover_unattended_processing(self):
        self.assertIn("backflow:processing", monitor.LABELS)
        self.assertIn("backflow:blocked", monitor.LABELS)

    def test_release_issue_requests_official_firmware_build(self):
        pull = {
            "number": 106,
            "html_url": "https://github.com/Pinvou/pinvou-agent/pull/106",
            "title": "chore(release): 版本升级至 0.7.2",
            "merged_at": "2026-07-30T09:06:55Z",
            "user": {"login": "maintainer"},
        }
        files = [
            {"filename": "VERSION", "additions": 1, "deletions": 1},
            {
                "filename": "pinvou3-app/package.json",
                "additions": 1,
                "deletions": 1,
            },
        ]

        body = monitor.render_issue_body("Pinvou/pinvou-agent", pull, files)

        self.assertIn("Release 回流：**是", body)
        self.assertIn("`release-packages.yml` 固件构建", body)

    def test_timestamp_requires_timezone(self):
        with self.assertRaisesRegex(ValueError, "必须包含时区"):
            monitor.parse_utc_timestamp("2026-07-27T09:00:00")

    def test_public_pr_text_cannot_create_private_mentions(self):
        pull = {
            "number": 43,
            "html_url": "https://github.com/Pinvou/pinvou-agent/pull/43",
            "title": "fix: notify @Pinvou/owners\nsecond line",
            "merged_at": "2026-07-27T08:00:00Z",
            "user": {"login": "user@name"},
        }
        files = [
            {"filename": "docs/`odd`\nname.md", "additions": 1, "deletions": 0}
        ]

        body = monitor.render_issue_body("Pinvou/pinvou-agent", pull, files)

        self.assertNotIn("@Pinvou", body)
        self.assertNotIn("@name", body)
        self.assertIn("@\u200bPinvou/owners second line", body)
        self.assertIn("`docs/ˋoddˋ name.md`", body)

    def test_existing_marker_prevents_duplicate_issue(self):
        pulls = [
            {
                "number": number,
                "html_url": f"https://github.com/Pinvou/pinvou-agent/pull/{number}",
                "title": f"fix: 示例 {number}",
                "merged_at": "2026-07-27T08:00:00Z",
                "user": {"login": "contributor"},
            }
            for number in (44, 45)
        ]

        class FakeApi:
            def __init__(self):
                self.created = []

            def paginated(self, path):
                if path.startswith("/repos/Pinvou/pinvou3/issues?"):
                    return [
                        {
                            "body": (
                                "<!-- community-backflow:"
                                "Pinvou/pinvou-agent#44 -->"
                            )
                        }
                    ]
                if path.startswith("/repos/Pinvou/pinvou-agent/pulls?"):
                    return pulls
                if path.endswith("/pulls/45/files"):
                    return [
                        {
                            "filename": "pinvou3-app/src/file.js",
                            "additions": 1,
                            "deletions": 0,
                        }
                    ]
                raise AssertionError(f"unexpected path: {path}")

            def get_optional(self, _path):
                return {"name": "exists"}

            def request(self, method, path, body=None):
                self.created.append((method, path, body))
                return {"html_url": "https://github.com/Pinvou/pinvou3/issues/300"}

        api = FakeApi()
        with redirect_stdout(io.StringIO()):
            created = monitor.create_backflow_issues(
                api,
                "Pinvou/pinvou-agent",
                "Pinvou/pinvou3",
                monitor.parse_utc_timestamp("2026-07-27T07:12:46Z"),
            )

        self.assertEqual(created, ["https://github.com/Pinvou/pinvou3/issues/300"])
        self.assertEqual(len(api.created), 1)
        self.assertIn("pinvou-agent#45", api.created[0][2]["title"])


if __name__ == "__main__":
    unittest.main()
