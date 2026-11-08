"""Offline response validators; not a substitute for real rc.26 acceptance."""

import importlib.util
import json
import io
from contextlib import redirect_stdout
from types import SimpleNamespace
from unittest.mock import patch
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

    def test_relay_failure_diagnostic_never_prints_upstream_code(self):
        # Execute the actual Scenario.verify error path with controlled HTTP only.
        reflected = "synthetic_sensitive_token_0123456789"
        scenario = object.__new__(business.Scenario)
        scenario.mock_key = "synthetic_mock_key"
        def success(api, path, payload=None, headers=None, method=None):
            if path.startswith("/api/channel/?"):
                return {"data": [{"id": 1}]}
            if path.startswith("/api/token/?"):
                return {"data": [{"id": 2}]}
            if path.endswith("/key"):
                return {"data": {"key": "synthetic_relay_key_0123456789"}}
            return {"data": {}}
        def request(path, *args, **kwargs):
            if path == "/v1/models":
                return 200, {"data": [{"id": "pinvou-mock"}]}
            return 403, {"error": "denied"}
        api = SimpleNamespace(request=request, raw_request=lambda *args, **kwargs:
                              (400, {}, json.dumps({"error": {"code": reflected,
                                                           "message": "quota refused"}}).encode()))
        output = io.StringIO()
        with patch.object(business, "successful", side_effect=success), \
                patch.object(business.time, "monotonic", side_effect=[0, 62]), redirect_stdout(output):
            with self.assertRaisesRegex(RuntimeError, "^relay_request_failed$"):
                scenario.verify(api, None, {"data": {"access_token": "synthetic_access_0123456789", "user": {"id": 1}}})
        self.assertNotIn(reflected, output.getvalue())
        self.assertEqual(json.loads(output.getvalue()), {"relay_status": 400, "code": "suppressed", "categories": ["quota", "refused"]})

    def test_pagination_response(self):
        self.assertEqual(business.items({"data": {"items": [{"id": 1}]}}), [{"id": 1}])
        self.assertEqual(business.items({"data": [{"id": 1}]}), [{"id": 1}])
        self.assertEqual(business.items({"error": "unavailable"}), [])


if __name__ == "__main__":
    unittest.main()
