"""Bound model-facing task observations without changing execution or task state."""
import re
from .state import ToolError


def store_task_handle(run_id):
    if (isinstance(run_id, str)
            and re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]{0,159}", run_id)
            and ".." not in run_id):
        return "store:" + run_id
    return None


def event_view(event):
    if isinstance(event, dict):
        event = dict(event)
        if isinstance(event.get("message"), str) and len(event["message"]) > 1000:
            event["message"] = event["message"][:1000]
            event["message_truncated"] = True
    return event


def task_page(data, args):
    events = data.get("events") or []
    if not isinstance(events, list):
        events = []
    legacy_lines = data.get("lines")
    from_store_lines = not events and isinstance(legacy_lines, list)
    if from_store_lines:
        events = [{"message": line} for line in legacy_lines if isinstance(line, str)]
    limit = args.get("events_limit", 8)
    offset = args.get("events_offset", max(0, len(events) - limit) if type(limit) is int else 0)
    if type(offset) is not int or offset < 0 or type(limit) is not int or not 1 <= limit <= 100:
        raise ToolError("INVALID_PAGE", "events_offset 必须非负，events_limit 必须在 1 到 100 之间。")
    selected = [event_view(event) for event in events[offset:offset + limit]]
    result = dict(data, events=selected, events_total=len(events), events_offset=offset,
                events_next_offset=offset + limit if offset + limit < len(events) else None,
                events_truncated=offset > 0 or offset + limit < len(events),
                events_basis="retained_task_events_not_unbounded_log")
    # Older v1 cmsd proxies expose store details as lines rather than events.
    # Leaving lines alongside the page would leak the entire retained log into
    # every model request and make offsets appear to have no effect.
    result.pop("lines", None)
    if from_store_lines:
        result["events_basis"] = "retained_store_lines"
    handle = store_task_handle(data.get("store_run_id"))
    if handle:
        result["store_task_id"] = handle
    if "last_event" in result:
        result["last_event"] = event_view(result["last_event"])
    return result


def task_next(status, kind):
    if status not in ("queued", "running", "cancelling", "succeeded", "failed", "cancelled", "interrupted"):
        return "任务状态尚未核实，先查原任务与管理连接；不要按未知状态重发操作、恢复安装或判断任务已结束。"
    if status in ("queued", "running", "cancelling"):
        return "任务尚未终结；用 cube_get_task(wait_seconds=10) 有界等待并报告进度，events_offset/events_limit 可读保留日志。不要要求用户重复发消息才继续。"
    if status == "succeeded":
        if kind in ("install", "fetch", "download", "verify"):
            return "本阶段已完成；按用户目标继续启动或实际用途验证，先登记已完成成果。下载完成不证明推理可用，不为可选接入重复安装。"
        return "本阶段已完成；核对实际服务、健康和默认模型状态并记录结果。换胎后按持久任务结果恢复，不重复发起切换。"
    if kind in ("fetch", "verify"):
        return "保留已完成文件和证据；用 cube_resume_task 恢复失败、取消或中断的独立任务，来源失效则更换下载方式。不要删除已有成果。"
    return "保留已完成文件和验证证据，先核对原服务与默认模型，再按错误诊断或恢复。管理权未知或 Cube Manage Software 失联时不能回退执行托管模型旧脚本。"
