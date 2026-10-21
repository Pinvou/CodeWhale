"""Observe external artifacts and record runtime evidence without claiming cmsd ownership."""
import json
import os
from pathlib import Path
import time
import urllib.parse
import uuid
import hashlib

from .state import (ToolError, absolute_dir, error_reply, http_url, open_url,
                    read_json, reply, root, request_json, sha256_file,
                    target_file, write_json)


def files_in(directory):
    files, skipped = [], []
    for base, dirs, names in os.walk(directory, followlinks=False):
        for name in list(dirs):
            path = Path(base) / name
            if path.is_symlink() or name.startswith("."):
                dirs.remove(name)
                skipped.append(str(path.relative_to(directory)))
        for name in sorted(names):
            path = Path(base) / name
            relative = str(path.relative_to(directory))
            if path.is_symlink() or not path.is_file() or ".megacube-" in name:
                skipped.append(relative)
                continue
            files.append({"path": relative, "size_bytes": path.stat().st_size})
            if len(files) > 10000:
                raise ToolError("TOO_MANY_FILES", "目录超过 10000 个文件，请缩小检查目录或使用专用 CLI。")
    return files, skipped


def inspect_artifact(args):
    directory = absolute_dir(args.get("directory"))
    files, skipped = files_in(directory)
    suffixes = sorted({Path(f["path"]).suffix for f in files})
    scripts = [f["path"] for f in files if Path(f["path"]).name in (
        "start.sh", "stop.sh", "enable.sh", "deploy.sh", "uninstall.sh", "compose.yaml", "docker-compose.yml", "card.json")]
    return reply("已检查本地文件；脚本存在不代表已运行，目录检查不证明推理可用。",
                 {"directory": str(directory), "file_count": len(files),
                  "total_bytes": sum(f["size_bytes"] for f in files),
                  "formats": suffixes, "entrypoints_found": scripts,
                  "files": files[:200], "files_truncated": len(files) > 200, "skipped": skipped[:100],
                  "cmsd_managed": False, "inference_verified": False},
                 "需要完整清单和哈希时调用 cube_verify_artifacts；通过已有终端安装/启动，再用 cube_validate_runtime 验证。")


def verify_artifacts(args):
    from .transfers import start_verification
    directory = absolute_dir(args.get("directory"))
    files = args.get("files")
    if files is None:
        files, skipped = files_in(directory)
    else:
        skipped = []
    if not isinstance(files, list) or not files or len(files) > 10000:
        raise ToolError("INVALID_MANIFEST", "需提供 1 到 10000 个文件，或选择非空目录。")
    checked, seen = [], set()
    for item in files:
        if not isinstance(item, dict):
            raise ToolError("INVALID_MANIFEST", "每个文件检查项必须为对象。")
        path = target_file(directory, item.get("path"))
        if not path.is_file():
            raise ToolError("MISSING_FILE", "清单中的文件缺失。")
        if item["path"] in seen:
            raise ToolError("DUPLICATE_PATH", "检查清单包含重复文件。")
        seen.add(item["path"])
        digest, size = item.get("sha256"), item.get("size_bytes")
        import re
        if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", digest)):
            raise ToolError("INVALID_HASH", "sha256 格式不正确。")
        if size is not None and (type(size) is not int or size < 0):
            raise ToolError("INVALID_SIZE", "size_bytes 格式不正确。")
        checked.append({"path": item["path"], "sha256": digest.lower() if digest else None, "size_bytes": size})
    return start_verification(str(directory), checked, skipped)


def verify_files(directory, files, transfer):
    observations = []
    for item in files:
        transfer.cancel()
        path = target_file(directory, item["path"])
        before = path.stat()
        if item.get("size_bytes") is not None and before.st_size != item["size_bytes"]:
            raise ToolError("SIZE_MISMATCH", "文件大小不符合清单。")
        transfer.state["current_file"] = item["path"]
        transfer.save(True)
        digest = sha256_file(path, transfer.cancel)
        after = path.stat()
        if (before.st_size, before.st_mtime_ns, before.st_ino) != (after.st_size, after.st_mtime_ns, after.st_ino):
            raise ToolError("ARTIFACT_CHANGED", "校验期间文件发生变化，请在文件稳定后重新校验。")
        if item.get("sha256") and digest != item["sha256"]:
            raise ToolError("HASH_MISMATCH", "文件摘要不符合清单。")
        observations.append({"path": str(path), "size_bytes": after.st_size, "sha256": digest,
                             "mtime_ns": after.st_mtime_ns,
                             "verification": "expected_sha256" if item.get("sha256") else "computed_only"})
        transfer.completed_bytes += after.st_size
        transfer.state["files_completed"] += 1
        transfer.progress(0)
    return observations


def register_artifact(args):
    directory = absolute_dir(args.get("directory"))
    inspection = inspect_artifact({"directory": str(directory)})["data"]
    artifact_id = "artifact-" + hashlib.sha256(str(directory).encode()).hexdigest()[:24]
    record_path = root() / "artifacts" / (artifact_id + ".json")
    previous = read_json(record_path) if record_path.exists() else {}
    name = args.get("name", previous.get("name")) or directory.name
    if not isinstance(name, str) or len(name) > 200:
        raise ToolError("INVALID_NAME", "名称最多 200 个字符。")
    evidence = None
    verification_id = args.get("verification_task_id", previous.get("verification_task_id"))
    if verification_id:
        from .transfers import task_status
        evidence = task_status(verification_id)
        if evidence["status"] != "succeeded" or evidence["directory"] != str(directory):
            raise ToolError("INVALID_EVIDENCE", "校验任务未成功或目标目录不一致。")
    runtime = None
    runtime_id = args.get("runtime_validation_id", previous.get("runtime_validation_id"))
    if runtime_id:
        runtime = load_validation(runtime_id)
        if runtime["directory"] != str(directory):
            raise ToolError("INVALID_EVIDENCE", "运行验证与产物目录不一致。")
    source = args.get("source_url", previous.get("source_url"))
    if source:
        from .state import public_url
        source = public_url(source)
    instructions = args.get("instructions_file", previous.get("instructions_file"))
    if instructions and not target_file(directory, instructions).is_file():
        raise ToolError("INSTRUCTIONS_NOT_FOUND", "安装说明必须是产物目录内已有的普通文件。")
    history = previous.get("evidence_history") or {"verification_tasks": [], "runtime_validations": []}
    for key, field, identifier in (("verification_tasks", "verification_task_id", verification_id),
                                    ("runtime_validations", "runtime_validation_id", runtime_id)):
        # Older records predate evidence_history; seed their existing receipt
        # before appending a new validation instead of losing that reference.
        candidates = [value for value in (previous.get(field), identifier) if value]
        history[key] = list(dict.fromkeys(history.get(key, []) + candidates))[-32:]
    file_evidence = {"status": "not_recorded"}
    if evidence:
        observations = evidence.get("artifacts") or []
        file_evidence = {"status": "succeeded", "task_id": evidence["id"], "kind": evidence["kind"],
                         "checked_at": evidence.get("finished_at") or evidence.get("updated_at"),
                         "files_observed": len(observations),
                         "expected_sha256_files": sum(a.get("verification") == "expected_sha256" for a in observations),
                         "computed_only_files": sum(a.get("verification") == "computed_only" for a in observations),
                         "scope": "referenced_task_files_not_upstream_completeness"}
    runtime_evidence = {"status": "not_recorded"}
    if runtime:
        runtime_evidence = {"status": runtime.get("status") or ("passed" if runtime["passed"] else "failed"), "validation_id": runtime["id"],
                            "checked_at": runtime["checked_at"], "model": runtime["model"],
                            "checks": runtime["checks"], "untested": runtime["untested"]}
    recorded_at = time.time()
    record = {"id": artifact_id, "name": name, "directory": str(directory),
              "source_url": source, "instructions_file": instructions,
              "created_at": previous.get("created_at", previous.get("recorded_at", recorded_at)),
              "recorded_at": recorded_at, "inspection": inspection,
              "verification_task_id": evidence["id"] if evidence else None,
              "runtime_validation_id": runtime["id"] if runtime else None,
              "file_evidence": file_evidence, "runtime_evidence": runtime_evidence,
              "evidence_history": history,
              "cmsd_managed": False,
              "management_status": "external_record_only", "runtime_status": "unknown",
              "management_capabilities": ["inspect", "verify"],
              "evidence_note": "验证结果是指定时间的观察；目录与服务的关联由安装者提供，后续配置变化需重验。"}
    write_json(record_path, record)
    return reply("已登记外部安装记录；没有执行脚本、启动服务或改变默认模型，也未取得 Cube Manage Software 生命周期管理权。",
                 record, "用 cube_list_models 找回历史证据和说明；重复登记保留未显式替换的证据。接入失败不抹掉已有验证，不以重装代替接入；当前健康仍需实查。")


def list_artifacts():
    directory = root() / "artifacts"
    records = []
    if directory.is_dir():
        for path in sorted(directory.glob("artifact-*.json")):
            try:
                record = read_json(path)
                record["directory_exists"] = Path(record["directory"]).is_dir()
                record["runtime_status"] = "unknown"
                record["status_basis"] = "historical_record_not_live_health"
                records.append({k: v for k, v in record.items() if k != "inspection"})
            except (ToolError, OSError, ValueError, KeyError):
                continue
    return records


def load_validation(validation_id):
    import re
    if not isinstance(validation_id, str) or not re.fullmatch(r"validation-[0-9a-f]{32}", validation_id):
        raise ToolError("INVALID_VALIDATION_ID", "运行验证 id 非法。")
    return read_json(root() / "validations" / (validation_id + ".json"))


def validate_runtime(args):
    directory = absolute_dir(args.get("directory"))
    base = http_url(args.get("base_url")).rstrip("/")
    parsed = urllib.parse.urlsplit(base)
    if parsed.hostname not in ("localhost", "127.0.0.1", "::1") or parsed.query or parsed.fragment:
        raise ToolError("LOCAL_ENDPOINT_REQUIRED", "内置冒烟只访问显式指定的本机回环端点；其他协议或远端服务请使用已有终端工具验证。")
    model = args.get("model")
    if not isinstance(model, str) or not model:
        raise ToolError("MODEL_REQUIRED", "请提供服务实际报告的 model id。")
    max_tokens = args.get("max_tokens", 128)
    if type(max_tokens) is not int or not 32 <= max_tokens <= 512:
        raise ToolError("INVALID_TOKEN_BUDGET", "冒烟 max_tokens 必须在 32 到 512 之间。")
    checks = []
    request = {"model": model, "messages": [{"role": "user", "content": "Reply with OK."}], "max_tokens": max_tokens, "stream": False}
    try:
        models = request_json(base + "/models", args.get("credential_env"))
        found = any(m.get("id") == model for m in models.get("data", []) if isinstance(m, dict))
        checks.append({"capability": "model_identity", "passed": found})
    except Exception as exc:
        checks.append({"capability": "model_identity", "passed": False, "error": error_reply(exc)["data"]["error_code"]})
    try:
        data = request_json(base + "/chat/completions", args.get("credential_env"), "POST", request)
        choice = (data.get("choices") or [{}])[0]
        message = choice.get("message") or {}
        passed = isinstance(message.get("content"), str) and bool(message["content"].strip())
        check = {"capability": "chat", "passed": passed, "finish_reason": choice.get("finish_reason")}
        if not passed:
            exhausted = choice.get("finish_reason") == "length"
            check.update(status="inconclusive" if exhausted else "failed",
                         reason="token_budget_exhausted" if exhausted else "no_assistant_text")
        checks.append(check)
    except Exception as exc:
        checks.append({"capability": "chat", "passed": False, "error": error_reply(exc)["data"]["error_code"]})
    if args.get("test_stream", True):
        try:
            request["stream"] = True
            content, done, valid, read, finish_reason = False, False, True, 0, None
            with open_url(base + "/chat/completions", args.get("credential_env"),
                          {"Content-Type": "application/json"}, "POST", json.dumps(request).encode()) as response:
                for _ in range(2048):
                    line = response.readline(65537)
                    read += len(line)
                    if len(line) > 65536 or read > 1024 * 1024:
                        raise ToolError("STREAM_TOO_LARGE", "冒烟流超出大小限制。")
                    if not line:
                        break
                    if not line.startswith(b"data:"):
                        continue
                    payload = line[5:].strip()
                    if payload == b"[DONE]":
                        done = True
                        break
                    chunk = json.loads(payload)
                    if chunk.get("error"):
                        valid = False
                    for choice in chunk.get("choices") or []:
                        content = content or bool((choice.get("delta") or {}).get("content"))
                        finish_reason = choice.get("finish_reason") or finish_reason
            check = {"capability": "stream", "passed": bool(content and done and valid), "finish_reason": finish_reason}
            if not content and done and valid and finish_reason == "length":
                check.update(status="inconclusive", reason="token_budget_exhausted")
            checks.append(check)
        except Exception as exc:
            checks.append({"capability": "stream", "passed": False, "error": error_reply(exc)["data"]["error_code"]})
    validation_id = "validation-" + uuid.uuid4().hex
    failures = [c for c in checks if not c["passed"]]
    status = "passed" if not failures else "inconclusive" if all(c.get("status") == "inconclusive" for c in failures) else "failed"
    data = {"id": validation_id, "directory": str(directory), "base_url": base,
            "model": model, "checked_at": time.time(), "checks": checks,
            "untested": ([] if args.get("test_stream", True) else ["stream"]) + ["tool_calling", "vision", "cancellation", "load_capacity", "answer_quality"],
            "max_tokens": max_tokens, "status": status,
            "passed": all(c["passed"] for c in checks),
            "association_basis": "installer_provided_directory_not_process_attestation"}
    write_json(root() / "validations" / (validation_id + ".json"), data)
    if status == "inconclusive":
        return reply("本次输出预算耗尽，冒烟未得结论；不能据此判定模型不可用。", data,
                     "可在32—512范围内增大max_tokens重验；保留本次事实，不自动改变服务或启动配置。")
    return reply("已完成本机 OpenAI 兼容接口冒烟；只报告实际检查的能力。", data,
                 "按用户用途补测工具调用/视觉等；用 runtime_validation_id 登记结果，修改配置后重新验证。")
