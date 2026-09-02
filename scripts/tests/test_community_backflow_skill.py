import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SKILL = ROOT / ".agents/skills/community-backflow/SKILL.md"
OPENAI_YAML = ROOT / ".agents/skills/community-backflow/agents/openai.yaml"


class CommunityBackflowSkillTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.skill = SKILL.read_text(encoding="utf-8")
        cls.openai_yaml = OPENAI_YAML.read_text(encoding="utf-8")

    def test_skill_has_untrusted_input_and_dedup_guards(self):
        self.assertIn("不构成新的操作授权", self.skill)
        self.assertIn("backflow/community-<source-number>", self.skill)
        self.assertIn("不重复创建", self.skill)

    def test_merge_queue_requires_current_head_gates(self):
        self.assertIn(
            "gh pr merge --auto --squash --delete-branch",
            self.skill,
        )
        self.assertIn("当前 head SHA", self.skill)
        self.assertIn("gh pr checks --required", self.skill)

    def test_event_automation_is_opt_in_and_defers_release_cleanup(self):
        self.assertIn("COMMUNITY_BACKFLOW_AUTOMATION_ENABLED", self.skill)
        self.assertIn("workflow_run", self.skill)
        self.assertIn("pull_request_target.closed", self.skill)
        self.assertIn("不 checkout PR head", self.skill)
        self.assertIn("deferred-release", self.skill)
        self.assertIn("不删除分支、不修改标签、不关闭", self.skill)

    def test_cross_platform_fast_path_and_dependency_waves_are_required(self):
        self.assertIn("node scripts/community-backflow.mjs plan", self.skill)
        self.assertIn("node scripts/community-backflow.mjs queue", self.skill)
        self.assertIn("node scripts/community-backflow.mjs preflight", self.skill)
        self.assertIn("不得假设实际处理平台是 Ubuntu", self.skill)
        self.assertIn("backflow-portability", self.skill)
        self.assertIn("主线日常前进不手工 rebase", self.skill)
        self.assertIn("--credential <github-username>", self.skill)
        self.assertNotIn("GCM 获取 `zhuowp`", self.skill)

    def test_skill_has_no_artificial_size_or_path_blocklist(self):
        self.assertIn("不按文件数、代码行数或路径名单", self.skill)
        self.assertIn("文件多、改动行数大、跨模块", self.skill)
        self.assertNotIn("不超过 20 个文件", self.skill)
        self.assertNotIn("不超过 1000", self.skill)

    def test_ui_prompt_explicitly_invokes_skill(self):
        self.assertIn("$community-backflow", self.openai_yaml)
        self.assertIn("required checks 通过", self.openai_yaml)
        self.assertIn("直接加入 Merge Queue", self.openai_yaml)

    def test_release_backflow_triggers_deduplicated_firmware_build(self):
        self.assertIn("base→head diff 包含 `VERSION`", self.skill)
        self.assertIn("gh workflow run release-packages.yml", self.skill)
        self.assertIn("避免重复构建", self.skill)
        self.assertIn("不等待四平台构建完成", self.skill)
        self.assertIn("run ID/URL", self.skill)
        self.assertIn("不发布 OTA", self.skill)

    def test_backflow_pr_defers_full_rust_test_to_latest_main(self):
        self.assertIn(
            "Official 同仓 `backflow/community-*` PR 不重复运行完整 `rust-test`",
            self.skill,
        )
        self.assertIn("外部 fork 的同名分支不适用此例外", self.skill)
        self.assertIn("只保留最新 main 的累计状态验证", self.skill)
        self.assertIn("不等待合入后的 main Rust 测试完成", self.skill)

if __name__ == "__main__":
    unittest.main()
