import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
LEGACY_CONFIG_PATH = ROOT / "deploy" / "nginx-pinvou-llmapi.conf"
SUBDOMAIN_CONFIG_PATH = (
    ROOT / "deploy" / "nginx-pinvou-llmapi-subdomain.conf"
)

PROXY_DIRECTIVES = (
    "proxy_pass http://127.0.0.1:13000;",
    "proxy_http_version 1.1;",
    "proxy_set_header Host $host;",
    "proxy_set_header X-Real-IP $remote_addr;",
    "proxy_set_header X-Forwarded-For $remote_addr;",
    "proxy_set_header X-Forwarded-Proto $scheme;",
    'proxy_set_header Connection "";',
    "proxy_buffering off;",
    "proxy_request_buffering off;",
    "proxy_cache off;",
    "proxy_read_timeout 600s;",
    "proxy_send_timeout 600s;",
)


def proxy_directives(config):
    return tuple(
        line.strip()
        for line in config.splitlines()
        if line.strip().startswith("proxy_")
    )


class LlmapiNginxConfigTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.config = LEGACY_CONFIG_PATH.read_text(encoding="utf-8")
        cls.subdomain_config = SUBDOMAIN_CONFIG_PATH.read_text(encoding="utf-8")

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

    def test_public_proxies_share_hardened_proxy_contract(self):
        for name, config in (
            ("legacy", self.config),
            ("subdomain", self.subdomain_config),
        ):
            with self.subTest(config=name):
                self.assertEqual(PROXY_DIRECTIVES, proxy_directives(config))
                self.assertNotIn("$proxy_add_x_forwarded_for", config)


if __name__ == "__main__":
    unittest.main()
