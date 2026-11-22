"""Explicit rc.26 API acceptance with disposable Redis and the repo mock upstream."""

import argparse
import importlib.util
import json
from pathlib import Path
import re
import secrets
import time
import traceback
import urllib.parse

SPEC = importlib.util.spec_from_file_location("newapi_fresh_fixture", Path(__file__).with_name("account-platform-newapi-fixture.py"))
base = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(base)


def items(body):
    data = body.get("data")
    return data if isinstance(data, list) else data.get("items", []) if isinstance(data, dict) else []


def resource_id(value):
    base.require(type(value) is int and value > 0, "business_resource_id_invalid")
    return value


def successful(api, path, payload=None, headers=None, method=None):
    status, body = api.request(path, payload, headers, method)
    base.require(status == 200 and body.get("success") is True, "business_api_rejected")
    return body


def sse_usage(raw):
    chunks = [line[6:] for line in raw.decode().splitlines() if line.startswith("data: ")]
    base.require(chunks and chunks[-1] == "[DONE]", "stream_done_missing")
    events = [json.loads(chunk) for chunk in chunks[:-1]]
    usages = [event["usage"] for event in events if event.get("usage")]
    base.require(len(usages) == 1 and usages[0].get("prompt_tokens") == 12
                 and usages[0].get("completion_tokens") == 4, "stream_usage_mismatch")
    content = "".join(choice.get("delta", {}).get("content", "")
                      for event in events for choice in event.get("choices", []))
    base.require(content == "mock response", "stream_content_mismatch")
    return usages[0]


class Scenario:
    def __init__(self, image, redis_image, binary):
        base.require(re.fullmatch(r"sha256:[a-f0-9]{64}", redis_image) is not None, "redis_requires_local_image_id")
        metadata = base.inspect("image", redis_image)
        base.require(metadata.get("Os") == "linux" and metadata.get("Architecture") == "amd64", "redis_platform_invalid")
        source = Path(binary)
        base.require(source.is_absolute() and not source.is_symlink() and source.is_file(), "mock_binary_invalid")
        self.binary = str(source.resolve(strict=True))
        base.require("," not in self.binary and "\n" not in self.binary, "mock_binary_path_invalid")
        self.image, self.redis_image = image, redis_image
        self.mock_key = secrets.token_hex(32)

    def prepare(self, network, resources, run_id):
        common = ["--pull=never", "--network", network, "--label", base.LABEL + "=" + run_id,
                  "--user", "65532:65532", "--read-only", "--cap-drop", "ALL",
                  "--security-opt", "no-new-privileges", "--log-driver", "none",
                  "--cpus", "1", "--pids-limit", "96",
                  "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m,mode=1777",
                  "--tmpfs", "/data:rw,nosuid,nodev,size=32m,uid=65532,gid=65532,mode=0700"]
        redis = base.docker("create", *common, "--name", "pinvou-newapi-fixture-" + run_id + "-redis",
                            "--network-alias", "fixture-redis", "--memory", "128m", self.redis_image,
                            "redis-server", "--save", "", "--appendonly", "no", "--protected-mode", "no")
        resources.append(("container", redis))
        base.docker("start", redis)
        base.eventually(lambda: base.docker("exec", redis, "redis-cli", "ping") == "PONG", "fixture_redis_not_ready")
        version = base.docker("exec", redis, "redis-server", "--version")
        base.require("v=8.2." in version, "redis_version_invalid")
        self.redis = redis
        mock = base.docker("create", *common, "--name", "pinvou-newapi-fixture-" + run_id + "-mock",
                           "--network-alias", "fixture-upstream", "--memory", "128m",
                           "--mount", "type=bind,src=" + self.binary + ",dst=/fixture/mock-newapi,readonly",
                           "--entrypoint", "/fixture/mock-newapi", "--env-file", "/dev/stdin", self.image,
                           payload="MOCK_NEWAPI_KEY=" + self.mock_key + "\nPORT=3000\nAPP_ENV=test\n")
        resources.append(("container", mock))
        base.docker("start", mock)
        self.mock_api = base.API(next(iter(base.inspect("container", mock)["NetworkSettings"]["Networks"].values()))["IPAddress"])
        base.eventually(lambda: self.mock_api.request("/v1/models", headers={"Authorization": "Bearer " + self.mock_key})[0] == 200,
                        "fixture_mock_not_ready")
        # Authentication is intentionally absent ONLY inside this throwaway network.
        # This is not the Redis TLS/ACL configuration for production.
        return {"REDIS_CONN_STRING": "redis://fixture-redis:6379/0"}

    def verify(self, api, sql, login):
        checks = ["redis_and_repo_mock"]
        data = login.get("data", {})
        access, user_id = data.get("access_token"), data.get("user", {}).get("id")
        base.require(isinstance(access, str) and len(access) > 16 and isinstance(user_id, int), "login_bundle_invalid")
        admin = {"Authorization": "Bearer " + access, "New-Api-User": str(user_id)}
        # Only change settings through the real admin API. No fabricated rows.
        for key, value in [("ModelRatio", {"pinvou-mock": 1, "pinvou-mock-rate-limit": 1}),
                           ("CompletionRatio", {"pinvou-mock": 1})]:
            successful(api, "/api/option/", {"key": key, "value": json.dumps(value)}, admin, "PUT")
        successful(api, "/api/channel/", {"mode": "single", "channel": {
            "type": 1, "name": "fixture-upstream", "key": self.mock_key, "status": 1,
            "base_url": "http://fixture-upstream:3000", "models": "pinvou-mock,pinvou-mock-rate-limit",
            "group": "default", "priority": 0, "weight": 1,
        }}, admin)
        channels = successful(api, "/api/channel/?p=1&page_size=10", headers=admin)
        base.require(len(items(channels)) == 1 and self.mock_key not in json.dumps(channels), "channel_metadata_not_redacted")
        channel_id = resource_id(items(channels)[0]["id"])
        # Even root must not read channel secrets without step-up verification.
        status, secret_response = api.request("/api/channel/" + str(channel_id) + "/key", {}, admin)
        base.require(status in (401, 403) and self.mock_key not in json.dumps(secret_response), "channel_stepup_not_enforced")
        checks.append("channel_creation_and_secret_guard")
        token_body = {"name": "fixture-relay", "expired_time": -1, "remain_quota": 1000000,
                      "unlimited_quota": False, "model_limits_enabled": True, "model_limits": "pinvou-mock",
                      "group": "default"}
        successful(api, "/api/token/", token_body, admin)
        listed = successful(api, "/api/token/?p=1&page_size=10", headers=admin)
        base.require(len(items(listed)) == 1, "token_list_invalid")
        token_id = resource_id(items(listed)[0]["id"])
        key_data = successful(api, "/api/token/" + str(token_id) + "/key", {}, admin)["data"]
        relay_key = key_data.get("key")
        base.require(isinstance(relay_key, str) and len(relay_key) > 16 and relay_key not in json.dumps(listed), "token_metadata_not_redacted")
        relay = {"Authorization": "Bearer " + relay_key}
        status, models = api.request("/v1/models", headers=relay)
        base.require(status == 200 and any(item.get("id") == "pinvou-mock" for item in models.get("data", [])), "model_discovery_failed")
        checks.append("scoped_token_and_model_discovery")
        # rc.26 enables in-process route caching when Redis is configured.
        # Channel creation does not invalidate that cache; its default refresh
        # period is 60 seconds. Wait without issuing potentially billable retries.
        deadline = time.monotonic() + 61
        while time.monotonic() < deadline:
            time.sleep(max(0, min(1, deadline - time.monotonic())))
        request_ids = []
        for streaming in [False, True]:
            status, headers, raw = api.raw_request("/v1/chat/completions", {
                "model": "pinvou-mock", "messages": [{"role": "user", "content": "fixture"}],
                "max_tokens": 16, "stream": streaming, **({"stream_options": {"include_usage": True}} if streaming else {}),
            }, relay)
            if status != 200:
                error = json.loads(raw).get("error", {})
                message = str(error.get("message", ""))
                print(json.dumps({"relay_status": status, "code": "suppressed",
                                  "categories": [word for word in ["channel", "SSRF", "ratio", "price", "quota", "tokenizer", "模型", "额度", "计费", "URL", "address", "refused"]
                                                 if word.lower() in message.lower()]}))
            base.require(status == 200, "relay_request_failed")
            request_id = next((value for key, value in headers.items() if key.lower() == "x-oneapi-request-id"), "")
            base.require(re.fullmatch(r"[A-Za-z0-9_-]{8,128}", request_id) is not None, "upstream_request_id_missing")
            request_ids.append(request_id)
            if streaming:
                sse_usage(raw)
            else:
                response = json.loads(raw)
                base.require(response.get("usage", {}).get("prompt_tokens") == 12
                             and response["usage"].get("completion_tokens") == 4, "json_usage_mismatch")
        base.require(len(set(request_ids)) == 2, "upstream_request_id_reused")
        checks.extend(["json_usage", "sse_usage_and_done"])
        pat = successful(api, "/api/user/token", headers=admin)["data"]
        base.require(isinstance(pat, str) and pat != relay_key and pat != access, "reconciliation_identity_not_separate")
        reconcile = {"Authorization": "Bearer " + pat, "New-Api-User": str(user_id)}
        def lookup(request_id):
            query = urllib.parse.urlencode({"p": 1, "page_size": 2, "type": 2, "request_id": request_id})
            body = successful(api, "/api/log/?" + query, headers=reconcile)
            rows = items(body)
            return rows[0] if len(rows) == 1 and rows[0].get("request_id") == request_id else None
        total_quota = 0
        for request_id in request_ids:
            base.eventually(lambda: lookup(request_id) is not None, "consume_log_not_found")
            row = lookup(request_id)
            base.require(row.get("prompt_tokens") == 12 and row.get("completion_tokens") == 4 and row.get("quota", 0) > 0,
                         "consume_log_usage_mismatch")
            total_quota += row["quota"]
            base.require(relay_key not in json.dumps(row) and self.mock_key not in json.dumps(row) and pat not in json.dumps(row), "consume_log_secret_leak")
        # Check the query is exact and does not silently ignore request_id.
        base.require(lookup("fixture-nonexistent-request") is None, "consume_log_filter_ignored")
        # Wait for asynchronous batched accounting before taking the baseline.
        base.eventually(lambda: int(sql("SELECT used_quota FROM tokens WHERE id=" + str(token_id) + ";")) == total_quota,
                        "quota_not_persisted")
        before = sql("SELECT used_quota FROM tokens WHERE id=" + str(token_id) + ";")
        for request_id in request_ids:
            lookup(request_id)
            lookup(request_id)
        base.require(sql("SELECT used_quota FROM tokens WHERE id=" + str(token_id) + ";") == before,
                     "readonly_reconciliation_changed_quota")
        checks.append("exact_log_reconciliation_no_duplicate_charge")
        base.require(int(base.docker("exec", self.redis, "redis-cli", "DBSIZE")) > 0, "newapi_redis_not_used")
        status, _ = api.request("/v1/chat/completions", {"model": "pinvou-mock-rate-limit", "messages": [{"role": "user", "content": "fixture"}]}, relay)
        base.require(status == 403, "token_model_limit_not_enforced")
        successful(api, "/api/token/" + str(token_id), headers=admin, method="DELETE")
        status, _ = api.request("/v1/models", headers=relay)
        base.require(status in (401, 403), "revoked_token_accepted")
        checks.append("model_limit_and_token_revocation")
        return checks


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--isolated-fixture", action="store_true", required=True)
    parser.add_argument("--newapi-image", required=True)
    parser.add_argument("--postgres-image", required=True)
    parser.add_argument("--redis-image", required=True)
    parser.add_argument("--mock-binary", required=True)
    args = parser.parse_args()
    try:
        scenario = Scenario(args.newapi_image, args.redis_image, args.mock_binary)
        base.run(args.newapi_image, args.postgres_image, scenario)
    except Exception as error:
        code = str(error) if isinstance(error, RuntimeError) and re.fullmatch(r"[a-z_]+", str(error)) else "unexpected_failure"
        print("NEWAPI_BUSINESS_FIXTURE_FAIL:" + code)
        print(json.dumps({"fixture_lines": [frame.lineno for frame in traceback.extract_tb(error.__traceback__)
                                             if frame.filename in (__file__, base.__file__)]}))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
