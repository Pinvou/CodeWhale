"""Opt-in Linux acceptance through the production --model-store-host process.

Usage: python3 -B tests/megacube_local_authority_real.py --binary /absolute/pinvou3
  --evidence /absolute/short/new-directory
Only inert legacy connection metadata is created; no remote service is contacted.

Only fresh profiles, SQLite records, sentinel shell scripts and synthetic Unix
cmsd replies are used. No model, systemd unit, original card or inference service
is changed. This tests actual CLI/env/script boundaries, not real inference.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shlex
import socket
import socketserver
import sqlite3
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers"))
from megacube_tools.state import write_json


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.host)


def request(path, method, route, body=None):
    conn = UnixHTTP(str(path), timeout=15)
    try:
        conn.request(method, route, None if body is None else json.dumps(body))
        reply = conn.getresponse()
        return reply.status, json.loads(reply.read(2 * 1024 * 1024))
    finally:
        conn.close()


def wait_task(sock, run):
    end = time.monotonic() + 20
    while True:
        code, task = request(sock, "GET", "/v1/tasks/" + run)
        assert code == 200, task
        if task["status"] != "running":
            return task
        assert time.monotonic() < end, "bounded sentinel action"
        time.sleep(.05)


class SyntheticCmsd(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True

    def __init__(self, path, mode, card):
        self.mode, self.card, self.owned, self.requests = mode, card, False, []
        super().__init__(str(path), CmsdHandler)
        self.worker = threading.Thread(target=self.serve_forever, daemon=True)
        self.worker.start()


class CmsdHandler(socketserver.StreamRequestHandler):
    def handle(self):
        first = self.rfile.readline(8192)
        if not first:
            return  # receipt failure can close the connected stream before POST
        method, route, _ = first.decode().split()
        length = 0
        for _ in range(100):
            line = self.rfile.readline(8192)
            if line in (b"\r\n", b"\n", b""):
                break
            key, value = line.decode().split(":", 1)
            if key.lower() == "content-length":
                length = int(value)
        assert 0 <= length <= 1024 * 1024
        self.rfile.read(length)
        s = self.server
        s.requests.append({"method": method, "path": route})
        code, value = 200, {}
        if route == "/v1/models":
            if s.mode == "query-failed":
                code, value = 503, {"error": {"code": "fixture_busy"}}
            elif s.mode in ("observed", "adoptable", "receipt-failed"):
                value = {"models": [{"card": s.card, "managed": s.owned,
                         "can_adopt": s.mode != "observed", "can_reserve_uninstall": False}]}
            else:
                value = {"models": []}
        elif route == "/v1/device":
            value = {"device_id": "dev-synthetic", "capabilities": ["model.explicit_adoption"]}
        elif route == f"/v1/models/{s.card}/enable" and s.mode == "adoptable":
            s.owned = True
            value = {"task_id": "qa-adoption"}
        elif route == "/v1/tasks/qa-adoption":
            value = {"status": "succeeded", "events": []}
        elif route == "/v1/status":
            value = {"service": "cmsd", "gateway": "127.0.0.1:19999"}
        else:
            code, value = 404, {"error": {"code": "fixture_unexpected_route"}}
        raw = json.dumps(value).encode()
        self.wfile.write(f"HTTP/1.1 {code} QA\r\nContent-Length: {len(raw)}\r\nConnection: close\r\n\r\n".encode() + raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    assert sys.platform == "linux" and os.geteuid() != 0, "permission acceptance requires a normal Linux user"
    assert not Path("/run/megacube/cmsd.sock").exists(), "default system service must not participate in this fixture"
    root, binary = args.evidence.resolve(), args.binary.resolve()
    root.mkdir(mode=0o700)
    (root / "driver.py").write_bytes(Path(__file__).read_bytes())
    started = time.monotonic()
    report = {"scope": __doc__, "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "cases": [], "errors": []}
    cases = ["fresh", "remote", "remote-download", "remote-retry", "remote-retry-explicit", "explicit-missing", "explicit-unit",
             "legacy-empty", "legacy-with-download", "owned-offline", "local-receipt", "component-receipt",
             "records-file", "records-link", "metadata-link", "metadata-denied", "socket-denied", "socket-parent-file", "socket-parent-link", "origin-pending",
             "query-failed", "observed", "live-empty", "owned-missing", "adoptable", "receipt-failed"]
    try:
        for number, mode in enumerate(cases):
            work = root / str(number)
            profile = work / "p"
            profile.mkdir(parents=True, mode=0o700)
            (work / "runtime").mkdir(mode=0o700)
            store = profile / "model-store"
            store.mkdir(mode=0o700)
            (store / "config.json").write_text(json.dumps({"models_dir": str(work / "models"), "staging_dir": str(work / "staging"), "min_free_space_gb": 0}))
            sock = profile / "model-store/service.sock"
            assert len(str(sock).encode()) < 108, "use a shorter evidence directory"
            env = {k: v for k, v in os.environ.items() if not k.startswith(("CMSD_", "PINVOU3_CMSD_", "PINVOU3_MEGACUBE_"))}
            env.update(PINVOU3_HOME=str(profile), XDG_RUNTIME_DIR=str(work / "runtime"),
                       XDG_STATE_HOME=str(work / "state-home"), CODEWHALE_HOME=str(work / "credentials"),
                       CODEWHALE_SECRET_BACKEND="file", PINVOU3_SKIP_LINUX_ASR="1", PINVOU3_SKIP_KNOWLEDGE_HOST="1")
            cube, records = profile / "megacube", profile / "megacube/installations"
            card, sentinel = "qa-authority-sentinel", work / "sentinel"
            allowed = mode in ("fresh", "remote", "remote-download", "remote-retry", "live-empty")
            server, child, log = None, None, (work / "host.log").open("wb")
            restore = []
            item = {"case": mode, "expected_legacy_scripts": allowed, "actions": []}
            try:
                if mode.startswith("remote"):
                    remote = cube / "remote-connections"
                    remote.mkdir(parents=True, mode=0o700)
                    (remote / "legacy-readonly.json").write_text('{"legacy_connection":true}\n')
                if mode in ("legacy-empty", "legacy-with-download"):
                    records.mkdir(parents=True)
                if mode in ("remote-download", "remote-retry", "remote-retry-explicit", "legacy-with-download", "origin-pending"):
                    previous = os.environ.get("PINVOU3_HOME")
                    os.environ["PINVOU3_HOME"] = str(profile)
                    try:
                        write_json(records / "tasks/fixture/record.json", {"operation": "download", "status": "succeeded"})
                    finally:
                        if previous is None:
                            os.environ.pop("PINVOU3_HOME", None)
                        else:
                            os.environ["PINVOU3_HOME"] = previous
                    assert (records / ".independent-records-v1").exists() == (mode != "legacy-with-download")
                    if mode == "origin-pending":
                        (records / ".origin-pending").write_text("publication unconfirmed")
                if mode in ("owned-offline", "owned-missing"):
                    (records / "ownership").mkdir(parents=True)
                    (records / "ownership" / card).write_text("cmsd\n")
                if mode in ("local-receipt", "component-receipt"):
                    path = cube / ("local-device-observed-v1" if mode == "local-receipt" else "cmsd-component/service.json")
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text("old or corrupt local receipt")
                if mode == "records-file":
                    cube.mkdir(); records.write_text("not a directory")
                if mode == "records-link":
                    cube.mkdir(); records.symlink_to(work / "absent-records")
                if mode == "metadata-link":
                    cube.symlink_to(work / "absent-metadata")
                if mode == "metadata-denied":
                    cube.mkdir(); cube.chmod(0); restore.append(cube)
                    try:
                        (cube / "local-device-observed-v1").stat()
                        raise AssertionError("permission injection ineffective")
                    except PermissionError as error:
                        item["actual_metadata_errno"] = error.errno
                if mode in ("socket-denied", "socket-parent-file", "socket-parent-link"):
                    parent = work / "runtime/megacube"
                    if mode == "socket-denied":
                        parent.mkdir(); parent.chmod(0); restore.append(parent)
                    elif mode == "socket-parent-file":
                        parent.write_text("not a directory")
                    else:
                        parent.symlink_to(work / "absent-socket-parent")
                if mode in ("explicit-missing", "remote-retry-explicit"):
                    env["CMSD_SOCKET"] = str(work / "absent.sock")
                if mode == "explicit-unit":
                    env["PINVOU3_CMSD_UNIT"] = "cmsd-qa-absent.service"
                if mode in ("query-failed", "observed", "live-empty", "owned-missing", "adoptable", "receipt-failed"):
                    server = SyntheticCmsd(work / "cms.sock", mode, card)
                    env["CMSD_SOCKET"] = str(work / "cms.sock")
                    if mode == "receipt-failed":
                        (cube / "local-device-observed-v1").mkdir(parents=True)
                child = subprocess.Popen([str(binary), "--model-store-host"], env=env, stdout=log, stderr=log)
                deadline = time.monotonic() + 20
                while True:
                    assert child.poll() is None, "owned store host exited"
                    try:
                        if request(sock, "POST", "/v1/ui/health", {})[0] == 200:
                            break
                    except (OSError, http.client.HTTPException):
                        pass
                    assert time.monotonic() < deadline, "owned host start deadline"
                    time.sleep(.05)
                actual_env = dict(part.split(b"=", 1) for part in Path(f"/proc/{child.pid}/environ").read_bytes().split(b"\0") if b"=" in part)
                item["actual_child_environment"] = {key: actual_env.get(key.encode(), b"").decode() for key in ("CMSD_SOCKET", "PINVOU3_CMSD_UNIT", "PINVOU3_HOME", "XDG_RUNTIME_DIR")}
                for key in ("CMSD_SOCKET", "PINVOU3_CMSD_UNIT", "PINVOU3_HOME", "XDG_RUNTIME_DIR"):
                    assert actual_env.get(key.encode(), b"").decode() == env.get(key, "")
                scripts = {}
                for action in ("enable", "stop", "uninstall"):
                    text = f"#!/bin/bash\nprintf '%s\\n' {shlex.quote(action)} >> {shlex.quote(str(sentinel))}\n"
                    text += f"printf '%s\\n' \"${{CMSD_SOCKET-}}\" \"${{PINVOU3_CMSD_UNIT-}}\" >> {shlex.quote(str(work / 'script-authority-env'))}\n"
                    scripts[action] = {"content": text, "sha256": hashlib.sha256(text.encode()).hexdigest()}
                with sqlite3.connect(profile / "model-store/models.db") as db:
                    db.execute("INSERT INTO store_models (card_id,model_name,card_name,engine_name,status,downloaded_at,scripts_content_json,card_source) VALUES (?,?,?,?,?,?,?,?)",
                               (card, "QA shell only", "QA shell only", "qa-shell", "deployed", "2026-10-18T00:00:00Z", json.dumps(scripts), "local"))
                for action in (("enable",) if mode == "adoptable" else ("enable", "stop", "uninstall")):
                    code, accepted = request(sock, "POST", "/v1/ui/lifecycle", {"card_id": card, "action": action, "purge_weights": False})
                    assert code == 200, accepted
                    run = accepted["result"]["run_id"]
                    task = wait_task(sock, run)
                    item["actions"].append(task)
                    if allowed or mode == "adoptable":
                        assert task["status"] == "succeeded", task
                    else:
                        assert task["status"] == "failed", task
                        assert any(word in task.get("error", "") for word in ("cmsd_unavailable", "ownership_", "device_record_failed", "invalid_response")), task
                lines = sentinel.read_text().splitlines() if sentinel.exists() else []
                assert lines == (["enable", "stop", "uninstall"] if allowed else []), lines
                item["sentinel_lines"] = lines
                script_env = (work / "script-authority-env").read_text().splitlines() if allowed else []
                assert script_env == ([env.get("CMSD_SOCKET", ""), env.get("PINVOU3_CMSD_UNIT", "")] * 3 if allowed else [])
                item["actual_script_authority_environment"] = script_env
                if server:
                    item["synthetic_cmsd_requests"] = server.requests
                    posts = [v for v in server.requests if v["method"] == "POST"]
                    assert len(posts) == (1 if mode == "adoptable" else 0), posts
                if mode == "adoptable":
                    assert (records / "ownership" / card).read_text() == "cmsd\n"
                if mode in ("remote-retry", "remote-retry-explicit"):
                    retry_card = "qa-authority-retry"
                    package = store / "cards" / retry_card
                    package.mkdir(parents=True)
                    fail = work / "fail-download"
                    fail.write_text("own dependency failure")
                    (package / "card.json").write_text(json.dumps({"id": retry_card, "name": "QA retry shell only", "engine_name": "qa-shell", "size_bytes": 4, "source_repo_id": "QA/Fixture"}))
                    (package / "common.sh").write_text("# 本卡由 megacube-admin 按 QA/Fixture 的 artifact-manifest 生成\nMODEL_ID=qa-shell-only\n")
                    for action in ("install_engine", "deploy", "enable", "stop", "uninstall"):
                        (package / (action + ".sh")).write_text("#!/bin/sh\nexit 0\n")
                    # Fail before the model DB row is published. A deploy-only
                    # failure already has a library row and is intentionally not
                    # an incomplete-install retry candidate.
                    (package / "download_model.sh").write_text(f"#!/bin/sh\necho download-attempt >> {shlex.quote(str(work / 'retry-attempts'))}\nif [ -f {shlex.quote(str(fail))} ]; then echo '权限不足：own-injected-failure'; exit 23; fi\necho recovered\n")
                    code, first = request(sock, "POST", "/v1/install_local", {"card_id": retry_card})
                    assert code == 200, first
                    original = wait_task(sock, first["run_id"])
                    assert original["status"] == "failed" and "own-injected-failure" in "\n".join(original["lines"]), original
                    fail.unlink()
                    code, preview = request(sock, "POST", "/v1/ui/incomplete_preview", {"run_id": first["run_id"]})
                    assert code == 200, preview
                    code, accepted = request(sock, "POST", "/v1/ui/incomplete_retry", {"run_id": first["run_id"], "expected_sha256": preview["result"]["previewSha256"]})
                    if mode == "remote-retry":
                        assert code == 200, accepted
                        retried = wait_task(sock, accepted["result"]["run_id"])
                        assert retried["status"] == "succeeded", retried
                        assert len((work / "retry-attempts").read_text().splitlines()) == 2
                        assert not (cube / "local-device-observed-v1").exists()
                        item["independent_retry"] = {"original": original, "retried": retried}
                    else:
                        assert code == 409 and "cmsd_unavailable" in accepted["error"]["message"], accepted
                        assert len((work / "retry-attempts").read_text().splitlines()) == 1
                        item["independent_retry_blocked_by_explicit_local_configuration"] = accepted
                item["passed"] = True
            finally:
                if child:
                    if child.poll() is None:
                        try:
                            _, tasks = request(sock, "GET", "/v1/tasks")
                            for task in tasks.get("tasks", []):
                                if task["status"] == "running":
                                    request(sock, "POST", "/v1/tasks/" + task["run_id"] + "/cancel", {})
                        except (OSError, http.client.HTTPException):
                            pass
                    child.terminate()
                    try:
                        child.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        child.kill(); child.wait(timeout=5)
                if server:
                    server.shutdown(); server.server_close(); server.worker.join(timeout=3)
                for path in restore:
                    path.chmod(0o700)
                log.close()
                report["cases"].append(item)
                (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
            print(json.dumps({"case": mode, "passed": item.get("passed"), "legacy_scripts": len(item.get("sentinel_lines", []))}), flush=True)
    except Exception as error:
        report["errors"].append(repr(error))
    report.update(passed=not report["errors"] and len(report["cases"]) == len(cases), seconds=time.monotonic()-started,
                  all_owned_hosts_stopped=True, original_resources_modified=False)
    (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({k: report[k] for k in ("passed", "seconds", "errors")}), flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
