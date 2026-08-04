import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
CONFIG_PATH = ROOT / "deploy" / "nginx-pinvou-llmapi.conf"


class LlmapiNginxConfigTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.config = CONFIG_PATH.read_text(encoding="utf-8")

    def test_documents_snippet_install_target(self):
        self.assertIn(
            "Install this file as /etc/nginx/snippets/pinvou-llmapi.conf",
            self.config,
        )

    def test_only_legacy_api_routes_are_proxied(self):
        compatibility_pattern = r"^/llmapi/(?:api|v1)(?:/|$)"
        catch_all_pattern = r"^/llmapi/(.*)$"
        self.assertIn(f"location ~ {compatibility_pattern}", self.config)
        self.assertIn(f"location ~ {catch_all_pattern}", self.config)
        self.assertLess(
            self.config.index(f"location ~ {compatibility_pattern}"),
            self.config.index(f"location ~ {catch_all_pattern}"),
        )
        self.assertNotIn("location ^~ /llmapi/", self.config)
        self.assertEqual(1, self.config.count("proxy_pass http://127.0.0.1:13000;"))

        compatibility = re.compile(compatibility_pattern)
        for path in (
            "/llmapi/api",
            "/llmapi/api/status",
            "/llmapi/v1",
            "/llmapi/v1/chat/completions",
        ):
            self.assertIsNotNone(compatibility.match(path), path)
        for path in (
            "/llmapi/dashboard",
            "/llmapi/login",
            "/llmapi/v10/models",
        ):
            self.assertIsNone(compatibility.match(path), path)

        self.assertIn("rewrite ^/llmapi(/.*)$ $1 break;", self.config)
        self.assertIn(
            "return 302 https://llmapi.pinvou.com/$1$is_args$args;",
            self.config,
        )
        self.assertEqual(
            2,
            self.config.count(
                "return 302 https://llmapi.pinvou.com/$is_args$args;"
            ),
        )
        redirect_match = re.compile(catch_all_pattern).match(
            "/llmapi/dashboard/settings"
        )
        self.assertIsNotNone(redirect_match)
        self.assertEqual(
            "https://llmapi.pinvou.com/dashboard/settings?tab=models",
            f"https://llmapi.pinvou.com/{redirect_match.group(1)}?tab=models",
        )

    def test_public_proxy_discards_client_forwarding_chain(self):
        self.assertIn("proxy_set_header X-Forwarded-For $remote_addr;", self.config)
        self.assertNotIn("$proxy_add_x_forwarded_for", self.config)


if __name__ == "__main__":
    unittest.main()
