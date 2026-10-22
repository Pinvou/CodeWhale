"""Persistent HTTP transfer jobs. This worker never executes model code or shell."""
try:
    import fcntl
except ImportError:
    fcntl = None
import hashlib
import http.client
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import uuid

from .state import (ToolError, absolute_dir, error_reply, http_url, open_url,
                    private_dir, public_url, read_json, relative_name, reply,
                    root, sha256_file, target_file, write_json)

TERMINAL = ("succeeded", "failed", "cancelled", "interrupted")


def require_worker_platform():
    if sys.platform != "linux" or fcntl is None:
        raise ToolError("UNSUPPORTED_PLATFORM", "独立持久传输/校验进程当前仅支持 Linux；请使用当前平台已有下载 CLI 或终端工具。")


def job_dir(task_id):
    if not isinstance(task_id, str) or not re.fullmatch(r"fetch-[0-9a-f]{32}", task_id):
        raise ToolError("INVALID_TASK_ID", "下载任务 id 非法。")
    return root() / "tasks" / task_id


def process_identity(pid):
    try:
        raw = Path("/proc/%d/stat" % pid).read_text().rsplit(")", 1)[1].split()
        return None if raw[0] == "Z" else raw[19]
    except (OSError, IndexError):
        return None


def task_status(task_id):
    directory = job_dir(task_id)
    data = read_json(directory / "state.json")
    if data["status"] not in TERMINAL:
        pid = data.get("pid")
        if pid and process_identity(pid) != data.get("process_identity"):
            data.update(status="interrupted", error="下载进程已退出，部分文件保留，可恢复任务。")
        elif not pid and time.time() - data["created_at"] > 30:
            data.update(status="interrupted", error="下载进程未成功启动，可恢复任务。")
    data["cancel_requested"] = (directory / "cancel").exists()
    return data


def list_tasks():
    tasks = []
    directory = root() / "tasks"
    if directory.is_dir():
        for path in directory.glob("fetch-*"):
            try:
                data = task_status(path.name)
                tasks.append({k: v for k, v in data.items() if k not in ("artifacts", "events")})
            except (ToolError, OSError, ValueError):
                continue
    return sorted(tasks, key=lambda t: -t["created_at"])


def spawn(directory):
    # A transfer job outlives the MCP connection. stdout never contaminates stdio RPC.
    worker = Path(__file__).resolve().parent / "worker.py"
    subprocess.Popen([sys.executable, "-B", str(worker), str(directory)],
                     stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                     stderr=subprocess.DEVNULL, start_new_session=True, close_fds=True)


def start_download(args, source=None):
    require_worker_platform()
    raw_files = args.get("files")
    if not isinstance(raw_files, list) or not raw_files:
        raise ToolError("EMPTY_FILES", "没有选中可下载文件。")
    if len(raw_files) > 10000:
        raise ToolError("TOO_MANY_FILES", "单任务最多 10000 个文件；可分批或使用专用 CLI。")
    files, names = [], set()
    for item in raw_files:
        if not isinstance(item, dict):
            raise ToolError("INVALID_FILES", "files 每项必须是对象。")
        name = relative_name(item.get("path"))
        url = http_url(item.get("url"))
        if name in names:
            raise ToolError("DUPLICATE_PATH", "同一任务不能重复写入同一路径。")
        names.add(name)
        digest, size = item.get("sha256"), item.get("size_bytes")
        if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", digest)):
            raise ToolError("INVALID_HASH", "sha256 必须是 64 位十六进制摘要。")
        if size is not None and (type(size) is not int or size < 0):
            raise ToolError("INVALID_SIZE", "size_bytes 必须是非负整数或省略。")
        files.append({"path": name, "url": url, "sha256": digest.lower() if digest else None, "size_bytes": size})
    credential_env = args.get("credential_env")
    if credential_env is not None and (not isinstance(credential_env, str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", credential_env)):
        raise ToolError("INVALID_CREDENTIAL_REF", "credential_env 只能是环境变量名。")
    directory = absolute_dir(args.get("directory"), create=True)
    for item in files:
        target_file(directory, item["path"])
    task_id = "fetch-" + uuid.uuid4().hex
    task = private_dir(job_dir(task_id))
    write_json(task / "request.json", {"directory": str(directory), "files": files, "credential_env": credential_env})
    state = {"id": task_id, "task_id": task_id, "kind": "fetch", "status": "queued",
             "created_at": time.time(), "directory": str(directory), "source": source,
             "file_count": len(files), "files_completed": 0, "progress_done_bytes": 0,
             "progress_total_bytes": sum(f["size_bytes"] for f in files) if all(f["size_bytes"] is not None for f in files) else None,
             "events": [], "artifacts": [], "error": None}
    write_json(task / "state.json", state)
    try:
        spawn(task)
    except OSError:
        state.update(status="failed", error="无法启动下载进程。")
        write_json(task / "state.json", state)
        raise
    return reply("已创建独立下载任务；不依赖商店或 Cube Manage Software，也不会自动启动模型。",
                 {"task_id": task_id, "directory": str(directory)},
                 "用 cube_get_task 查询；cube_cancel_task 保留部分文件；失败或取消后 cube_resume_task 恢复。")


def cancel_task(task_id):
    data = task_status(task_id)
    if data["status"] in TERMINAL:
        return reply("任务已结束，无需取消；文件保持原状。", data, "需要恢复失败任务可使用 cube_resume_task。")
    if data["status"] not in TERMINAL:
        write_json(job_dir(task_id) / "cancel", {"requested_at": time.time()})
    return reply("已请求取消；正在进行的网络读取结束或超时后停止，保留下载成果。",
                 task_status(task_id), "用 cube_get_task 核实终态；不会删除文件或停止模型服务。")


def start_verification(directory, files, skipped):
    require_worker_platform()
    task_id = "fetch-" + uuid.uuid4().hex
    task = private_dir(job_dir(task_id))
    write_json(task / "request.json", {"operation": "verify", "directory": directory, "files": files})
    write_json(task / "state.json", {
        "id": task_id, "task_id": task_id, "kind": "verify", "status": "queued",
        "created_at": time.time(), "directory": directory, "file_count": len(files),
        "files_completed": 0, "progress_done_bytes": 0, "progress_total_bytes": None,
        "events": [], "artifacts": [], "skipped": skipped, "error": None})
    try:
        spawn(task)
    except OSError:
        state = read_json(task / "state.json")
        state.update(status="failed", error="无法启动校验进程。")
        write_json(task / "state.json", state)
        raise
    return reply("已创建后台文件校验任务；计算摘要不等于验证来源可信。",
                 {"task_id": task_id}, "用 cube_get_task 查询完整结果，支持取消和恢复。")


def resume_task(args):
    require_worker_platform()
    task_id = args.get("task_id")
    data = task_status(task_id)
    if data["status"] not in ("failed", "cancelled", "interrupted"):
        raise ToolError("TASK_NOT_RESUMABLE", "只可恢复失败、取消或已中断的下载任务。")
    directory = job_dir(task_id)
    # Serialize resume requests; worker takes the same lock for its full lifetime.
    with (directory / "worker.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ToolError("TASK_BUSY", "原下载进程尚未退出，请稍后重试。") from None
        current = task_status(task_id)
        if current["status"] not in ("failed", "cancelled", "interrupted"):
            raise ToolError("TASK_BUSY", "任务已被恢复。")
        data = current
        read_json(directory / "request.json")
        (directory / "cancel").unlink(missing_ok=True)
        data.update(status="queued", pid=None, created_at=time.time(), error=None)
        write_json(directory / "state.json", data)
    # Release worker.lock before spawn: a fast child must not exit on our lock.
    try:
        spawn(directory)
    except OSError:
        data.update(status="failed", error="无法恢复下载/校验进程。")
        write_json(directory / "state.json", data)
        raise
    return reply("下载任务已恢复；只在摘要或服务端版本标识允许时续传。", {"task_id": task_id}, "用 cube_get_task 查询。")


class Cancelled(Exception):
    pass


class Transfer:
    def __init__(self, directory, state):
        self.directory, self.state = directory, state
        self.last_saved = 0
        self.completed_bytes = 0
        self.previous_artifacts = {a["path"]: a for a in state.get("artifacts", [])}

    def prepare_resume(self):
        # Public artifacts describe this attempt only. Preserve the hashes of
        # earlier completed files separately before clearing that public view.
        path = self.directory / "resume-artifacts.json"
        retained = read_json(path) if path.exists() else {"version": 1, "artifacts": []}
        if (not isinstance(retained, dict) or retained.get("version") != 1
                or not isinstance(retained.get("artifacts"), list)
                or len(retained["artifacts"]) > 10000):
            raise ToolError("INVALID_RESUME_EVIDENCE", "下载恢复摘要记录损坏，请保留文件并核查原任务。")
        previous = {}
        for artifact in retained["artifacts"]:
            if (not isinstance(artifact, dict) or not isinstance(artifact.get("path"), str)
                    or not isinstance(artifact.get("sha256"), str)
                    or not re.fullmatch(r"[0-9a-f]{64}", artifact["sha256"])):
                raise ToolError("INVALID_RESUME_EVIDENCE", "下载恢复摘要记录损坏，请保留文件并核查原任务。")
            previous[artifact["path"]] = artifact
        # The most recent positive verification supersedes an older receipt.
        previous.update(self.previous_artifacts)
        self.previous_artifacts = previous
        self.save_resume()

    def save_resume(self):
        write_json(self.directory / "resume-artifacts.json", {"version": 1, "artifacts": [
            {"path": path, "sha256": artifact["sha256"]}
            for path, artifact in self.previous_artifacts.items()]})
        # Persist the receipt rename before the attempt may replace state.json
        # or publish a new target. A failed receipt write leaves old state intact.
        descriptor = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def remember_artifact(self, artifact):
        self.previous_artifacts[artifact["path"]] = artifact
        self.save_resume()

    def cancel(self):
        if (self.directory / "cancel").exists():
            raise Cancelled()

    def save(self, force=False):
        now = time.monotonic()
        if force or now - self.last_saved > 0.3:
            self.state["updated_at"] = time.time()
            write_json(self.directory / "state.json", self.state)
            self.last_saved = now

    def event(self, message):
        self.state["events"] = (self.state["events"] + [{"at": time.time(), "message": message}])[-100:]
        self.save(True)

    def progress(self, size):
        self.state["progress_done_bytes"] = self.completed_bytes + size
        self.save()

    def file(self, item, destination, credential_env):
        self.cancel()
        target = target_file(destination, item["path"])
        target.parent.mkdir(parents=True, exist_ok=True)
        target = target_file(destination, item["path"])
        lock_dir = private_dir(root() / "locks")
        lock_name = hashlib.sha256(str(target).encode()).hexdigest()
        with (lock_dir / lock_name).open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ToolError("DESTINATION_BUSY", "另一个任务正在写入同一目标文件。") from None
            return self.locked_file(item, destination, target, credential_env)

    def locked_file(self, item, destination, target, credential_env):
        if target.exists():
            digest = sha256_file(target, self.cancel)
            previous = self.previous_artifacts.get(str(target), {})
            expected = item["sha256"] or previous.get("sha256")
            if not expected or digest != expected or (item["size_bytes"] is not None and target.stat().st_size != item["size_bytes"]):
                raise ToolError("EXISTING_FILE_CONFLICT", "目标文件已存在但无法确认一致；保留原文件，请校验或选择其他位置。")
            artifact = {"path": str(target), "size_bytes": target.stat().st_size, "sha256": digest,
                        "verification": "expected_sha256" if item["sha256"] else "computed_only"}
            self.remember_artifact(artifact)
            return artifact
        key = hashlib.sha256((item["url"] + (item["sha256"] or "")).encode()).hexdigest()[:16]
        part_name = ".%s.megacube-%s.part" % (target.name, key)
        part = target_file(target.parent, part_name)
        meta_path = target_file(target.parent, part_name + ".json")
        for attempt in range(3):
            self.cancel()
            try:
                meta = read_json(meta_path) if meta_path.exists() else {}
                offset = part.stat().st_size if part.exists() else 0
                # Unknown sources without a stable validator are restarted, never concatenated blindly.
                etag = meta.get("etag")
                validator = etag if etag and not etag.startswith("W/") else meta.get("last_modified")
                if offset and not (item["sha256"] or validator):
                    offset = 0
                headers = {"Accept-Encoding": "identity"}
                if offset:
                    headers["Range"] = "bytes=%d-" % offset
                    if validator:
                        headers["If-Range"] = validator
                try:
                    response = open_url(item["url"], credential_env, headers)
                except urllib.error.HTTPError as exc:
                    if exc.code == 416 and offset:
                        if item["sha256"] and sha256_file(part, self.cancel) == item["sha256"] and (item["size_bytes"] is None or offset == item["size_bytes"]):
                            response = None
                        else:
                            part.unlink(missing_ok=True)
                            meta_path.unlink(missing_ok=True)
                            raise ToolError("RANGE_RESTART", "远端拒绝续传，下一次从头获取。") from None
                    else:
                        raise
                if response is not None:
                    with response:
                        status = response.status
                        if status not in (200, 206):
                            raise ToolError("INVALID_DOWNLOAD_RESPONSE", "下载只接受 HTTP 200/206。")
                        if status == 206:
                            match = re.fullmatch(r"bytes (\d+)-(\d+)/(\d+)", response.headers.get("Content-Range", ""))
                            if not match or int(match[1]) != offset or int(match[2]) < offset or int(match[3]) <= int(match[2]):
                                raise ToolError("INVALID_RANGE", "续传响应区间不匹配。")
                            total = int(match[3])
                            current_validator = response.headers.get("ETag") if validator == etag else response.headers.get("Last-Modified")
                            if validator and current_validator != validator:
                                raise ToolError("SOURCE_CHANGED", "远端文件版本改变，不能拼接续传。请重新检查来源。")
                        else:
                            offset = 0
                            length = response.headers.get("Content-Length")
                            total = int(length) if length and length.isdigit() else None
                        if item["size_bytes"] is not None and total is not None and total != item["size_bytes"]:
                            raise ToolError("SIZE_MISMATCH", "来源文件大小与预期不一致。")
                        remaining = (total - offset) if total is not None else 0
                        if remaining > shutil.disk_usage(target.parent).free:
                            raise ToolError("INSUFFICIENT_DISK", "目标磁盘空间不足，部分文件保留。")
                        write_json(meta_path, {"etag": response.headers.get("ETag"), "last_modified": response.headers.get("Last-Modified")})
                        flags = os.O_WRONLY | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
                        flags |= os.O_APPEND if offset else os.O_TRUNC
                        with os.fdopen(os.open(part, flags, 0o600), "ab" if offset else "wb") as output:
                            size = offset
                            while True:
                                self.cancel()
                                block = response.read1(1024 * 1024)
                                if not block:
                                    break
                                size += len(block)
                                expected = item["size_bytes"] if item["size_bytes"] is not None else total
                                if expected is not None and size > expected:
                                    raise ToolError("SIZE_MISMATCH", "下载超过预期大小。")
                                output.write(block)
                                self.progress(size)
                            output.flush()
                            os.fsync(output.fileno())
                        if total is not None and size != total:
                            raise ToolError("INCOMPLETE_DOWNLOAD", "连接提前结束，文件不完整。")
                size = part.stat().st_size
                if item["size_bytes"] is not None and size != item["size_bytes"]:
                    raise ToolError("SIZE_MISMATCH", "下载文件大小不匹配。")
                self.state["phase"] = "verify"
                self.save(True)
                digest = sha256_file(part, self.cancel)
                if item["sha256"] and digest != item["sha256"]:
                    # Retain evidence, but the next attempt must not resume corrupt bytes.
                    meta_path.unlink(missing_ok=True)
                    raise ToolError("HASH_MISMATCH", "文件摘要不匹配；不发布为完整产物。请检查来源或用新目录重新下载。")
                self.cancel()
                artifact = {"path": str(target), "size_bytes": size, "sha256": digest,
                            "verification": "expected_sha256" if item["sha256"] else "computed_only",
                            "source_url": public_url(item["url"])}
                # A receipt is not current completion. It only permits a fresh
                # hash comparison if publication or the state save is interrupted.
                self.remember_artifact(artifact)
                # Atomic publication without replacing a file created during download.
                os.link(part, target)
                part.unlink()
                meta_path.unlink(missing_ok=True)
                return artifact
            except (OSError, ToolError, http.client.HTTPException) as exc:
                code = getattr(exc, "code", None)
                retryable = (isinstance(exc, (TimeoutError, ConnectionError, http.client.HTTPException))
                             or isinstance(exc, urllib.error.URLError) and not isinstance(exc, urllib.error.HTTPError)
                             or isinstance(exc, urllib.error.HTTPError) and exc.code in (408, 429, 500, 502, 503, 504)
                             or code in ("INCOMPLETE_DOWNLOAD", "RANGE_RESTART"))
                if not retryable or attempt == 2:
                    raise
                self.event("网络或文件传输未完成，保留部分文件并重试。")
                for _ in range(10 * (attempt + 1)):
                    self.cancel()
                    time.sleep(0.1)
        raise ToolError("DOWNLOAD_FAILED", "下载失败。")


def run_worker(directory):
    directory = Path(directory)
    with (directory / "worker.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        state = read_json(directory / "state.json")
        transfer = Transfer(directory, state)
        try:
            request = read_json(directory / "request.json")
            destination = absolute_dir(request["directory"])
            if request.get("operation") != "verify":
                transfer.prepare_resume()
            state.update(status="running", pid=os.getpid(), process_identity=process_identity(os.getpid()),
                         phase="download", artifacts=[], files_completed=0, progress_done_bytes=0, error=None)
            state.pop("error_code", None)
            transfer.save(True)
            if request.get("operation") == "verify":
                from .artifacts import verify_files
                state["phase"] = "verify"
                state["artifacts"] = verify_files(destination, request["files"], transfer)
                transfer.cancel()
                state.update(status="succeeded", phase="complete")
                transfer.event("文件校验完成；未执行文件内容，也未验证推理。")
                return
            for item in request["files"]:
                state.update(current_file=item["path"], phase="download")
                transfer.event("正在获取：" + item["path"])
                artifact = transfer.file(item, destination, request.get("credential_env"))
                state["artifacts"].append(artifact)
                state["files_completed"] += 1
                transfer.completed_bytes += artifact["size_bytes"]
                transfer.progress(0)
            transfer.cancel()
            state.update(status="succeeded", phase="complete", progress_done_bytes=transfer.completed_bytes)
            transfer.event("文件获取完成；尚未安装引擎或验证推理。")
        except Cancelled:
            state.update(status="cancelled", phase="cancelled", error=None)
            transfer.event("任务已取消，完整文件与部分下载均保留。")
        except Exception as exc:
            result = error_reply(exc)
            state.update(status="failed", phase="failed", error=result["summary"], error_code=result["data"]["error_code"])
            transfer.event(result["summary"])
        finally:
            transfer.save(True)
