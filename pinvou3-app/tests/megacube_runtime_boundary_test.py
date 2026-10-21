"""Offline urllib transport coverage for local runtime evidence boundaries."""
from email.message import Message
import io
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import unittest
from unittest.mock import patch
import urllib.request
from urllib.response import addinfourl

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers"))
from megacube_tools import artifacts, state


class Transport(urllib.request.BaseHandler):
    # Run after ProxyHandler so the test observes the actual selected host.
    handler_order = 499

    def __init__(self, redirects=None):
        self.redirects = redirects or {}
        self.requests = []

    def http_open(self, request):
        self.requests.append({"url": request.full_url, "host": request.host,
                              "authorization": request.get_header("Authorization")})
        location = self.redirects.get(request.full_url)
        if location:
            headers = Message()
            headers["Location"] = location
            response = addinfourl(io.BytesIO(b""), headers, request.full_url, 302)
            response.msg = "Found"
            return response
        streaming = request.data and json.loads(request.data).get("stream")
        if streaming:
            body = b'data: {"choices":[{"delta":{"content":"OK"}}]}\n\ndata: [DONE]\n\n'
        elif request.full_url.endswith("/models"):
            body = b'{"data":[{"id":"fixture-model"}]}'
        else:
            body = b'{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}'
        response = addinfourl(io.BytesIO(body), {}, request.full_url, 200)
        response.msg = "OK"
        return response

    https_open = http_open


class RuntimeBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name) / "model"
        self.directory.mkdir()
        self.environment = patch.dict(os.environ, {
            "PINVOU3_HOME": str(Path(self.temporary.name) / "profile"),
            "http_proxy": "http://proxy.invalid:3128",
            "https_proxy": "http://proxy.invalid:3128", "no_proxy": "",
        }, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        # A missed transport route must fail rather than create any network IO.
        self.socket = patch.object(socket, "socket", side_effect=AssertionError("network forbidden"))
        self.socket.start()
        self.addCleanup(self.socket.stop)

    def install_transport(self, transport):
        real_builder = urllib.request.build_opener
        builder = patch.object(urllib.request, "build_opener",
                               side_effect=lambda *handlers: real_builder(*handlers, transport))
        builder.start()
        self.addCleanup(builder.stop)

    def runtime(self, base="http://127.0.0.1:8000/v1", **extra):
        return artifacts.validate_runtime({"directory": str(self.directory),
            "base_url": base, "model": "fixture-model", **extra})["data"]

    def test_origin_uses_scheme_hostname_and_effective_port(self):
        self.assertEqual(state.http_origin("http://LOCALHOST/v1"), ("http", "localhost", 80))
        self.assertEqual(state.http_origin("http://localhost:80/other"), ("http", "localhost", 80))
        self.assertEqual(state.http_origin("https://[::1]/v1"), ("https", "::1", 443))
        self.assertEqual(state.http_origin("http://127.0.0.1:0/v1")[2], 0)
        with self.assertRaises(state.ToolError):
            state.http_origin("http://localhost:invalid/v1")

    def test_redirect_refuses_other_origin_before_request(self):
        source = "http://127.0.0.1:8000/v1/models"
        handler = state.RedirectHandler(state.http_origin(source))
        for target in ("https://example.invalid/v1/models", "http://127.0.0.1:8001/v1/models",
                       "https://127.0.0.1:8000/v1/models", "http://localhost:8000/v1/models"):
            for method in ("GET", "POST"):
                for code in (301, 302, 303, 307, 308):
                    with self.subTest(target=target, method=method, code=code):
                        request = urllib.request.Request(source, method=method)
                        with self.assertRaises(state.ToolError) as error:
                            handler.redirect_request(request, io.BytesIO(), code, "redirect", {}, target)
                        self.assertEqual(error.exception.code, "LOCAL_ENDPOINT_REQUIRED")

    def test_same_origin_paths_and_explicit_default_ports_are_allowed(self):
        for source, target in (("http://LOCALHOST:80/v1/models", "http://localhost/other/models"),
                               ("https://[::1]:443/v1/models", "https://[::1]/other/models")):
            with self.subTest(source=source):
                handler = state.RedirectHandler(state.http_origin(source))
                redirected = handler.redirect_request(urllib.request.Request(source), io.BytesIO(),
                                                      302, "redirect", {}, target)
                self.assertEqual(redirected.full_url, target)

    def test_initial_request_is_also_pinned_to_the_required_origin(self):
        with self.assertRaises(state.ToolError) as error:
            state.open_url("https://example.invalid/models",
                           required_origin=state.http_origin("http://127.0.0.1:8000/v1"))
        self.assertEqual(error.exception.code, "LOCAL_ENDPOINT_REQUIRED")

    def test_all_runtime_consumers_refuse_remote_redirects_and_record_failures(self):
        base = "http://127.0.0.1:8000/v1"
        transport = Transport({base + "/models": "https://example.invalid/v1/models",
                               base + "/chat/completions": "https://example.invalid/v1/chat/completions"})
        self.install_transport(transport)
        data = self.runtime(base)
        self.assertEqual(data["status"], "failed")
        self.assertFalse(data["passed"])
        self.assertEqual(len(data["checks"]), 3)
        self.assertTrue(all(check["error"] == "LOCAL_ENDPOINT_REQUIRED" for check in data["checks"]))
        self.assertEqual([request["host"] for request in transport.requests], ["127.0.0.1:8000"] * 3)
        self.assertEqual(artifacts.load_validation(data["id"]), data)

    def test_one_redirected_fact_prevents_a_positive_runtime_receipt(self):
        base = "http://127.0.0.1:8000/v1"
        transport = Transport({base + "/models": "http://127.0.0.1:8001/v1/models"})
        self.install_transport(transport)
        data = self.runtime(base)
        self.assertEqual(data["status"], "failed")
        self.assertEqual([check["passed"] for check in data["checks"]], [False, True, True])
        self.assertTrue(all(request["host"] == "127.0.0.1:8000" for request in transport.requests))

    def test_runtime_bypasses_environment_proxies_for_http_and_https(self):
        for base in ("http://localhost/v1", "https://[::1]:443/v1"):
            with self.subTest(base=base):
                transport = Transport()
                real_builder = urllib.request.build_opener
                with patch.object(urllib.request, "build_opener",
                                  side_effect=lambda *handlers: real_builder(*handlers, transport)):
                    data = self.runtime(base)
                self.assertTrue(data["passed"])
                expected = "localhost" if base.startswith("http:") else "[::1]:443"
                self.assertEqual([request["host"] for request in transport.requests], [expected] * 3)

    def test_runtime_keeps_same_origin_relative_path_redirects(self):
        base = "http://127.0.0.1:8000/v1"
        transport = Transport({base + "/models": "/actual/models"})
        self.install_transport(transport)
        data = self.runtime(base)
        self.assertTrue(data["passed"])
        self.assertEqual(transport.requests[1]["url"], "http://127.0.0.1:8000/actual/models")

    def test_general_downloads_keep_cross_origin_redirects_without_credentials(self):
        request = urllib.request.Request("https://source.invalid/file", headers={"Authorization": "Bearer fixture"})
        redirected = state.RedirectHandler().redirect_request(request, io.BytesIO(), 302, "redirect", {},
                                                               "https://cdn.invalid/file")
        self.assertEqual(redirected.full_url, "https://cdn.invalid/file")
        self.assertIsNone(redirected.get_header("Authorization"))
        with self.assertRaises(state.ToolError) as error:
            state.RedirectHandler().redirect_request(request, io.BytesIO(), 302, "redirect", {},
                                                      "http://cdn.invalid/file")
        self.assertEqual(error.exception.code, "INSECURE_REDIRECT")

    def test_general_downloads_keep_environment_proxy_support(self):
        transport = Transport()
        self.install_transport(transport)
        with state.open_url("http://source.invalid/file") as response:
            response.read()
        self.assertEqual(transport.requests[0]["host"], "proxy.invalid:3128")


if __name__ == "__main__":
    unittest.main()
