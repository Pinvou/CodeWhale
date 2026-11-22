"""LLM access to the same durable, reviewed retry path as the store UI."""
import http.client
import json
import os
from pathlib import Path
import re
import socket
import struct
import sys

from .state import ToolError, error_reply, page, reply
from .redact import redact_line, redact_text

_LIMIT = 16 * 1024 * 1024
_RUN = re.compile(r"[A-Za-z0-9_-][A-Za-z0-9_.-]{0,159}\Z")


def failure(error):
    result = error_reply(error)
    result["next"] = "先查原任务、未完成索引与管理连接；不能据此重调完整安装或改动受保护卡。更新或恢复后台后重新预览。框架限制不代表模型不可安装，已授权的独立实验路径仍可用。"
    return result


def _run(value):
    if not isinstance(value, str) or not _RUN.fullmatch(value) or ".." in value:
        raise ToolError("INVALID_RUN_ID", "使用工具返回的原始商店 run_id，不传路径。")
    return value


class _Connection(http.client.HTTPConnection):
    def connect(self):
        if sys.platform != "linux":
            raise ToolError("UNSUPPORTED_PLATFORM", "本机商店恢复仅支持 Linux。")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(35)
        home = Path(os.environ.get("PINVOU3_HOME") or Path.home() / ".pinvou3")
        self.sock.connect(str(home / "model-store" / "service.sock"))
        _, uid, _ = struct.unpack("3i", self.sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        if uid != os.geteuid():
            raise ToolError("STORE_PEER_MISMATCH", "商店服务不属于当前用户，未发送请求。")


def _rpc(method, args):
    connection = _Connection("store", timeout=35)
    try:
        connection.request("POST", "/v1/ui/" + method, json.dumps(args), {"Content-Type": "application/json"})
        response = connection.getresponse()
        raw = response.read(_LIMIT + 1)
        if len(raw) > _LIMIT:
            raise ToolError("STORE_RESPONSE_LIMIT", "商店响应超过读取上限，请在模型商店检查任务。")
        value = json.loads(raw)
        if response.status != 200 or "error" in value:
            error = value.get("error") or {}
            # r20 P2-2：商店拒绝 message 可能内嵌上游 JSON 回显（admission
            # 类错误整体内嵌上游应答），与 task()/list_incomplete 同口径脱敏。
            raise ToolError("STORE_REQUEST_REJECTED", redact_text(str(error.get("message") or "商店拒绝请求。")))
        if "result" not in value:
            raise ToolError("STORE_PROTOCOL_ERROR", "商店未返回完整结果。")
        return value["result"]
    except (OSError, http.client.HTTPException, ValueError) as error:
        # A lost response can follow durable acceptance. Never automatically
        # resend or suggest the old install endpoint in response to this error.
        raise ToolError("STORE_OBSERVATION_FAILED", "无法核实商店响应；先查现有任务和未完成安装，勿重复发起。独立安装工具仍可用。") from error
    finally:
        connection.close()


def _check():
    health = _rpc("health", {})
    if "model.install_retry.v1" not in health.get("capabilities", []):
        raise ToolError("STORE_RETRY_CAPABILITY_REQUIRED", "当前商店后台缺少恢复能力，请更新后台；不能改调完整安装来替代重试。")


def _arguments(args, allowed):
    if set(args) - set(allowed):
        raise ToolError("INVALID_INPUT", "只传工具声明的参数，不传路径、owner 或 confirmed。")


def list_incomplete(args):
    _arguments(args, ("installations_offset", "installations_limit"))
    _check()
    value = _rpc("library", {})
    result = page(value.get("incompleteInstallations", []), {"installations_limit": 8, **args}, "installations")
    for item in result["installations"]:
        # r18 #4：先脱敏再截断——与 cmsd 侧镜像/直通边界同口径，防止截断
        # 把凭据劈成两半漏出前缀。
        if isinstance(item.get("error"), str):
            redacted = redact_text(item["error"])
            if len(redacted) > 1000:
                item["error"] = redacted[:1000]
                item["error_truncated"] = True
            else:
                item["error"] = redacted
    result["observation_error"] = value.get("incompleteError")
    return reply("未完成安装索引；不代表已安装或已取得管理权。", result,
                 "先查原任务和错误；可恢复项调用 cube_preview_install_retry。取消保留部分文件，不自动清理。")


def preview(args):
    _arguments(args, ("run_id",))
    run_id = _run(args.get("run_id"))
    _check()
    value = _rpc("incomplete_preview", {"run_id": run_id})
    return reply("已只读预览当前包和原任务语义，尚未安装。", value,
                 "核对来源、目录及 download/install_local 的实际影响；在用户已有授权内用 cube_retry_install 传回 previewSha256。包变化须重新预览。")


def retry(args):
    _arguments(args, ("run_id", "expected_preview_sha256"))
    run_id = _run(args.get("run_id"))
    digest = args.get("expected_preview_sha256")
    if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise ToolError("INVALID_PREVIEW_DIGEST", "传入预览返回的 previewSha256 原值。")
    _check()
    value = _rpc("incomplete_retry", {"run_id": run_id, "expected_sha256": digest})
    accepted_run = _run(value.get("run_id"))
    value = {**value, "task_id": "store:" + accepted_run, "original_run_id": run_id,
             "management_status": "not_confirmed_by_installation"}
    return reply("重试已受理；保留原任务类型，尚未证明安装完成。", value,
                 "用 cube_get_task 查询 task_id；取消用 cube_cancel_task。受理后暂时没有子日志属于转交等待，不重复安装；不会自动启用。")


def task(args):
    run_id = _run(args["task_id"][len("store:"):])
    offset = args.get("events_offset")
    limit = args.get("events_limit", 8)
    wait = args.get("wait_seconds", 3)
    if (offset is not None and (type(offset) is not int or offset < 0)
            or type(limit) is not int or not 1 <= limit <= 100
            or type(wait) is not int or not 0 <= wait <= 15):
        raise ToolError("INVALID_PAGE", "事件 offset 非负，limit 为1—100，等待为0—15秒。")
    _check()
    request = {"run_id": run_id, "from": offset or 0, "wait": wait,
               "limit": limit, "preview_chars": 1000}
    if offset is None:
        request["tail"] = limit
    value = _rpc("log", request)
    lines = value.get("lines", [])
    start = 0 if "total" in value or offset is not None else max(0, len(lines) - limit)
    absolute_start = value.get("from", offset or 0) + start
    total = value.get("total", value.get("next"))
    truncated = set(value.get("truncated_lines", []))
    selected = lines[start:start + limit]
    status = "unknown" if value.get("unconfirmed") else "running"
    if value.get("done"):
        status = "cancelled" if value.get("cancelled") else "succeeded" if value.get("ok") else "failed"
    raw_error = value.get("error")
    data = {"id": args["task_id"], "store_run_id": run_id, "status": status,
            "backend_pending": value.get("backend_pending", False),
            "error": redact_text(raw_error) if isinstance(raw_error, str) else raw_error,
            "result_available": value.get("result") is not None or value.get("result_truncated", False),
            "result_truncated": value.get("result_truncated", False),
            "error_truncated": value.get("error_truncated", False),
            "events": [{"message": redact_line(line)[:1000], "truncated": len(line) > 1000 or absolute_start + index in truncated}
                       for index, line in enumerate(selected)],
            "events_total": total, "events_offset": absolute_start,
            "events_next_offset": absolute_start + len(selected) if total is not None and absolute_start + len(selected) < total else None}
    return reply("商店任务 %s：%s。" % (run_id, status), data,
                 "unknown/等待转交不是安装失败；继续查原任务。完成后核实成果和能力，不以安装成功代替启用或推理验证。")


def cancel(args):
    run_id = _run(args["task_id"][len("store:"):])
    _check()
    value = _rpc("cancel", {"run_id": run_id})
    return reply("取消请求已发送，最终结果仍需查询。", {**value, "task_id": args["task_id"]},
                 "继续用 cube_get_task 核实终态；取消保留部分文件，不能当作清理完成。")
