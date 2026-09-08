"""Offline response validators; not a substitute for real rc.26 acceptance."""

import importlib.util
import json
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("business", Path(__file__).resolve().parents[1] / "account-platform-newapi-business-fixture.py")
business = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(business)


def stream(usage=None, done=True):
    events = [{"choices": [{"delta": {"content": "mock response"}}]},
              {"choices": [], "usage": usage or {"prompt_tokens": 12, "completion_tokens": 4}}]
    return ("".join("data: " + json.dumps(event) + "\n\n" for event in events) + ("data: [DONE]\n\n" if done else "")).encode()


class BusinessFixtureTests(unittest.TestCase):
    def test_sse_usage(self):
        self.assertEqual(business.sse_usage(stream())["completion_tokens"], 4)
        with self.assertRaisesRegex(RuntimeError, "stream_done_missing"):
            business.sse_usage(stream(done=False))
        with self.assertRaisesRegex(RuntimeError, "stream_usage_mismatch"):
            business.sse_usage(stream({"prompt_tokens": -1, "completion_tokens": 4}))
        with self.assertRaisesRegex(RuntimeError, "stream_usage_mismatch"):
            business.sse_usage(stream().replace(b"data: [DONE]", b'data: {"usage":{"prompt_tokens":12,"completion_tokens":4}}\n\ndata: [DONE]'))

    def test_resource_ids_are_not_sql_fragments(self):
        self.assertEqual(business.resource_id(1), 1)
        for value in [True, "1", "1; SELECT 1", 0, -1, None]:
            with self.assertRaisesRegex(RuntimeError, "business_resource_id_invalid"):
                business.resource_id(value)

    def test_pagination_response(self):
        self.assertEqual(business.items({"data": {"items": [{"id": 1}]}}), [{"id": 1}])
        self.assertEqual(business.items({"data": [{"id": 1}]}), [{"id": 1}])
        self.assertEqual(business.items({"error": "unavailable"}), [])


if __name__ == "__main__":
    unittest.main()
