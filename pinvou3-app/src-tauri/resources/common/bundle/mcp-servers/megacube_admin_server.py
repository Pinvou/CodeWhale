#!/usr/bin/env python3
"""megacube-admin — MegaCube 管理面（cmsd）与模型下载工具集（零第三方依赖，只用 stdlib）。

经本机 unix socket 调 cmsd 管理 API（默认 /run/megacube/cmsd.sock，CMSD_SOCKET
环境变量覆盖）。cmsd 不在时管理操作降级，独立下载和外部产物工具仍可用。

标准工具面（14 件，另追加 megacube_tools 的原子能力）：
  只读：cube_get_status / cube_list_models / cube_get_capacity / cube_get_logs /
        cube_plan_switch / cube_get_task / cube_list_store_models / cube_check_model_repo
  变更：cube_start_model / cube_stop_model / cube_set_default_model / cube_switch_model /
        cube_install_model / cube_cancel_task

模型下载（cube_install_model）两条路：
  - card_id：商店已收录的卡。逐脚本 https 下载 + 逐字节 sha256 校验后原子物化到
    卡片目录，再 POST cmsd install 下载权重。
  - repo_id：商店未收录、满足当前模板的通用 NVFP4 仓库，按元数据生成
    SGLang 容器卡。驭光仅接受对应模型的 H3C 官方商店卡，不从仓库生成原生卡。
    megacube_tools 另外提供独立下载、检查与登记。
外部口径：
  商店目录 = PINVOU_STORE_URL 或 <home>/model-store/config.json 的 store_url，
    默认 https://account.pinvou.com/model-store
  HF 端点  = PINVOU_HF_ENDPOINT 或 config.json 的 hf_endpoint，默认 https://hf-mirror.com
  卡片目录 = MEGACUBE_CARDS_DIR，默认 <PINVOU3_HOME||~/.pinvou3>/model-store/cards

返回统一 {summary, data, next}：summary 是给人和模型看的结论句（数字来自工具，
禁止凭记忆估算）；next 引导下一步该调什么。

协议：newline-delimited JSON-RPC 2.0 over stdio（对齐底座 stdio transport）。
"""
import hashlib
import io
import json
import os
import re
import shutil
import socket
import stat
import sys
import uuid
import time
import urllib.error
import urllib.parse
import urllib.request

# Also support importlib-based tests and the extracted standalone stdio entrypoint.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import megacube_tools
from megacube_tools.task_view import task_page, task_next, store_task_handle
from megacube_tools.state import remember_local_device as _remember_local_device
from megacube_tools.redact import redact_text as _redact_text

if sys.platform == "win32":
    sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8")
    sys.stdin = io.TextIOWrapper(sys.stdin.buffer, encoding="utf-8")

PROTOCOL_VERSION = "2024-11-05"
DEFAULT_SOCKET = "/run/megacube/cmsd.sock"


def _socket_path():
    explicit = os.environ.get("CMSD_SOCKET")
    if explicit:
        return explicit
    if os.path.exists(DEFAULT_SOCKET):
        return DEFAULT_SOCKET
    runtime = os.environ.get("XDG_RUNTIME_DIR")
    state = os.environ.get("XDG_STATE_HOME") or os.path.expanduser("~/.local/state")
    return os.path.join(runtime or state, "megacube/cmsd.sock")


_CMSD_MAX_RESPONSE_BYTES = 16 * 1024 * 1024


def _bounded_recv(sock, limit=_CMSD_MAX_RESPONSE_BYTES):
    """r18 #3：cmsd 管理通道有界读（与仓内其余通道 16MiB 口径对齐）。
    socket 超时只拦停顿的发送方，拦不住快速流——无界 recv 可被异常 cmsd
    或被指向的任意本地 socket 推爆整个 MCP 工具面的内存。"""
    chunks = []
    total = 0
    while True:
        chunk = sock.recv(65536)
        if not chunk:
            break
        total += len(chunk)
        if total > limit:
            raise megacube_tools.ToolError(
                "CMSD_RESPONSE_TOO_LARGE",
                "管理通道应答超过 %d MiB 上限，已中止读取。" % (limit // (1024 * 1024)),
            )
        chunks.append(chunk)
    return b"".join(chunks)

def _http_get(path):
    """unix socket 上的极简 GET。返回 (status, body_text)。"""
    sock_path = _socket_path()
    if not os.path.exists(sock_path):
        return None, None
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(5)
    try:
        sock.connect(sock_path)
        request = (
            "GET %s HTTP/1.1\r\nhost: localhost\r\nconnection: close\r\n\r\n" % path
        ).encode("utf-8")
        sock.sendall(request)
        raw = _bounded_recv(sock).decode("utf-8", "replace")
        head, _, body = raw.partition("\r\n\r\n")
        parts = head.split(" ", 2)
        status = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
        return status, body.strip()
    finally:
        sock.close()


def _unavailable():
    return {
        "summary": "本机无法连接 Cube Manage Software（管理 socket 不存在），这台设备上无法查询。",
        "data": None,
        "next": "管理操作需恢复 Cube Manage Software；下载、来源检查与外部安装记录可继续使用独立工具，框架外安装复用已有终端。",
    }


def _http_post(path, payload):
    """unix socket 上的极简 POST（JSON body）。返回 (status, body_text)。"""
    sock_path = _socket_path()
    if not os.path.exists(sock_path):
        return None, None
    body = json.dumps(payload or {}, ensure_ascii=False).encode("utf-8")
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(10)
    try:
        sock.connect(sock_path)
        # Persist device awareness before a request can grant lifecycle control.
        # This is not an ownership binding. Rust must not fall back to an old
        # script if the MCP accepted an adoption and the socket later vanished.
        try:
            _remember_local_device()
        except OSError as error:
            raise megacube_tools.ToolError("DEVICE_RECORD_FAILED", "不能持久记录设备连接，尚未发送管理请求。") from error
        head = (
            "POST %s HTTP/1.1\r\nhost: localhost\r\ncontent-type: application/json\r\n"
            "content-length: %d\r\nconnection: close\r\n\r\n" % (path, len(body))
        ).encode("utf-8")
        sock.sendall(head + body)
        raw = _bounded_recv(sock).decode("utf-8", "replace")
        head_text, _, resp_body = raw.partition("\r\n\r\n")
        parts = head_text.split(" ", 2)
        status = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
        return status, resp_body.strip()
    finally:
        sock.close()


_MODEL_STATUSES = {"running", "starting", "stopped", "degraded"}
_TASK_STATUSES = {"queued", "running", "cancelling", "succeeded", "failed", "cancelled", "interrupted"}


def _fact_text(value):
    return isinstance(value, str) and bool(value.strip())


def _model_observation(row):
    return (isinstance(row, dict) and _fact_text(row.get("card"))
            and _fact_text(row.get("status")) and isinstance(row.get("managed"), bool))


def _task_observation(row):
    return isinstance(row, dict) and _fact_text(row.get("id")) and _fact_text(row.get("status"))


def _observed_status(row, known):
    # Future states remain visible but cannot become positive readiness or a terminal result.
    return row if row["status"] in known else dict(row, status="unknown", reported_status=row["status"])


def _api_failure(code, message, status=None, unconfirmed=True):
    value = {"error": {"code": code, "message": message}, "error_code": code}
    if status is not None:
        value["http_status"] = status
    if unconfirmed:
        value["observation"] = "unconfirmed"
    return value


def _decode_api_response(status, body, task_snapshot=False):
    """Preserve transport failures without turning them into valid observations."""
    try:
        value = json.loads(body)
    except (ValueError, TypeError):
        return _api_failure("CMSD_INVALID_RESPONSE", "管理接口响应不完整或不是有效 JSON，结果尚未核实。", status)
    if not isinstance(value, dict):
        return _api_failure("CMSD_INVALID_RESPONSE", "管理接口未返回对象，结果尚未核实。", status)
    # A task's own execution failure is a successful observation, not an HTTP error.
    if task_snapshot and 200 <= status < 300 and _task_observation(value):
        return value
    error = value.get("error")
    if isinstance(error, dict):
        return _api_failure(str(error.get("code") or "CMSD_REQUEST_REJECTED"),
                            str(error.get("message") or error.get("code") or "管理请求未成功。"),
                            status, unconfirmed=False)
    if not 200 <= status < 300:
        return _api_failure("CMSD_HTTP_%s" % status, "管理接口返回 HTTP %s，结果尚未核实。" % status, status)
    if error is not None:
        return _api_failure("CMSD_INVALID_RESPONSE", "管理接口错误格式未核实。", status)
    return value


def _observation_reply(data):
    if data is None:
        return _unavailable()
    error = data.get("error") or {}
    return {"summary": "本次未取得管理事实：%s" % (error.get("message") or error.get("code") or "响应尚未核实"),
            "data": dict(data, observation="unconfirmed"),
            "next": "保留原模型和任务事实，按实际错误恢复管理或商店库存连接后重新查询；不要把未知当作空库存、未下载或已经完成，也不要重复提交管理动作。独立下载与外部安装记录仍可使用。"}


def _api_post(path, payload):
    try:
        status, body = _http_post(path, payload)
    except OSError:
        return _api_failure("MANAGEMENT_RESULT_UNCONFIRMED", "管理响应未取得，请先核实原任务；请求可能已经受理。")
    if status is None:
        return None
    return _decode_api_response(status, body)


def _api(path):
    """Return a validated GET observation, an explicit failure, or missing socket."""
    try:
        status, body = _http_get(path)
    except OSError:
        return _api_failure("CMSD_OBSERVATION_FAILED", "管理连接失败，本次未取得事实。")
    if status is None:
        return None
    route = path.split("?", 1)[0]
    value = _decode_api_response(status, body, task_snapshot=route.startswith("/v1/tasks/"))
    if "error_code" in value:
        return value
    valid = True
    if route == "/v1/status":
        valid = (value.get("service") == "cmsd" and _fact_text(value.get("version"))
                 and type(value.get("uptime_s")) is int and value["uptime_s"] >= 0
                 and "active_model" in value and (value["active_model"] is None or _fact_text(value["active_model"]))
                 and isinstance(value.get("models"), list) and all(_model_observation(row) for row in value["models"])
                 and isinstance(value.get("degraded"), list)
                 and all(isinstance(row, dict) and _fact_text(row.get("card")) and isinstance(row.get("reason"), str)
                         for row in value["degraded"]))
        if valid:
            value["models"] = [_observed_status(row, _MODEL_STATUSES) for row in value["models"]]
    elif route == "/v1/models":
        valid = (not value.get("selection_error") and isinstance(value.get("models"), list)
                 and all(_model_observation(row) for row in value["models"])
                 and "active_model" in value and (value["active_model"] is None or _fact_text(value["active_model"])))
        if valid:
            value["models"] = [_observed_status(row, _MODEL_STATUSES) for row in value["models"]]
    elif route == "/v1/capacity":
        valid = isinstance(value.get("memory"), dict)
    elif route == "/v1/tasks":
        valid = isinstance(value.get("tasks"), list) and all(_task_observation(row) for row in value["tasks"])
        if valid:
            value["tasks"] = [_observed_status(row, _TASK_STATUSES) for row in value["tasks"]]
    elif route.startswith("/v1/tasks/"):
        valid = _task_observation(value) and value["id"] == route.rsplit("/", 1)[-1]
        if valid:
            value = _observed_status(value, _TASK_STATUSES)
    elif route.startswith("/v1/models/") and route.endswith("/logs"):
        valid = isinstance(value.get("lines"), list) and all(isinstance(line, str) for line in value["lines"])
    if not valid:
        return _api_failure("CMSD_INVALID_RESPONSE", "管理接口缺少完整的事实字段，结果尚未核实。", status)
    if (route == "/v1/status" and value.get("service") == "cmsd") or route == "/v1/models":
        try:
            _remember_local_device()
        except OSError as error:
            raise megacube_tools.ToolError("DEVICE_RECORD_FAILED", "不能持久记录本机设备观测，请核对记录目录。") from error
    return value


def _fmt_gib(value):
    return "%.1f GiB" % value if isinstance(value, (int, float)) else "未知"


# ---------------------------------------------------------------------------
# 模型下载：商店 / HF 仓库 / 卡片物化
# ---------------------------------------------------------------------------
DEFAULT_STORE_BASE = "https://account.pinvou.com/model-store"
DEFAULT_HF_ENDPOINT = "https://hf-mirror.com"
HTTP_TIMEOUT_S = 30  # urllib 只有一个超时值（连接/读同档）；失败统一转中文应答
_GIB = 1024.0 ** 3
_NAME_RE = re.compile(r"^[A-Za-z0-9._-]+$")
_REPO_RE = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")
_SHA256_RE = re.compile(r"^[0-9a-fA-F]{64}$")


class _NetError(Exception):
    """网络/协议层失败（超时、拒连、DNS、重定向过多），message 已是中文。"""


class _CardError(Exception):
    """商店/仓库元数据不合规或内容被篡改，message 已是中文（拒绝安装的理由）。"""


# compose 型容器对话引擎（cmsd W6/W7 纳管口径）：这些引擎的容器卡出 OpenAI
# 对话接口，MCP 可代办安装；diffusers/comfyui 等图像类不在其列（不是对话模型）。
_DOCKER_CHAT_ENGINES = ("sglang", "vllm")

# ---------------------------------------------------------------------------
# 通用对话仓库（第二形态）准入常量（docs/megacube-第三方模型下载-方案与验收矩阵.md §1）
# ---------------------------------------------------------------------------
# 架构白名单以母本卡 qwen38-27b-sglang0519 的仓库 unsloth/Qwen3.8-27B-NVFP4 在
# HF 平台 API 实测上报的 architectures 为准：Qwen3.8 实际是
# Qwen3_5ForConditionalGeneration（混合线性注意力 + 视觉塔的 VLM），并非方案
# 初稿写的 Qwen3NextForCausalLM（D1 实测纠正）。启动参数组（mm-attention/mamba/
# linear-attn）与该族精确对应；换族必须新模板变体 + 真机标定，不允许远端元数据
# 生成运行参数。
_GENERIC_ARCH_WHITELIST = ("Qwen3_5ForConditionalGeneration",)
_GENERIC_MAX_WEIGHTS_BYTES = 100 * 1024 ** 3  # 仅标准模板的策略上限；独立下载不受此限


def _pinvou3_home():
    """对齐 app 侧 platform::paths::pinvou3_home：PINVOU3_HOME 环境变量优先。"""
    return os.environ.get("PINVOU3_HOME") or os.path.join(os.path.expanduser("~"), ".pinvou3")


def _cards_dir():
    return os.environ.get("MEGACUBE_CARDS_DIR") or os.path.join(
        _pinvou3_home(), "model-store", "cards")


def _store_config():
    """<home>/model-store/config.json（可能不存在）：store_url / hf_endpoint。"""
    path = os.path.join(_pinvou3_home(), "model-store", "config.json")
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
        return data if isinstance(data, dict) else {}
    except Exception:
        return {}


def _store_base():
    return (os.environ.get("PINVOU_STORE_URL") or _store_config().get("store_url")
            or DEFAULT_STORE_BASE).rstrip("/")


def _hf_endpoint():
    return (os.environ.get("PINVOU_HF_ENDPOINT") or _store_config().get("hf_endpoint")
            or DEFAULT_HF_ENDPOINT).rstrip("/")


def _valid_name(value):
    """卡片/文件名白名单：只允许 [A-Za-z0-9._-]，且不允许 .. 与点开头（防路径穿越）。

    fullmatch 而非 match+$：正则的 $ 在结尾换行前命中，"abc\\n" 这类值会
    混进请求行造成注入（与空格/CRLF 同类），必须整串匹配。
    """
    if not value or not _NAME_RE.fullmatch(value):
        return False
    return ".." not in value and not value.startswith(".")


def _valid_repo(value):
    """repo_id 形如 owner/name：只允许一个 /，两段各自过白名单。"""
    if not value or not _REPO_RE.match(value):
        return False
    owner, name = value.split("/", 1)
    return _valid_name(owner) and _valid_name(name)


_MAX_FETCH_BYTES = 16 * 1024 * 1024
_FETCH_ALLOWED_SCHEMES = ("http", "https")


class _FetchRedirectHandler(urllib.request.HTTPRedirectHandler):
    """r12 P2-1：每跳校验的重定向处理器。

    r11 的 _fetch_redirect_target 只在裸 urlopen 抛 HTTPError 时生效，
    但 urllib 默认 HTTPRedirectHandler 对 http↔http、https→http 降级与
    http→ftp 全部静默跟随不抛错——校验从未运行（评审实测复现）。改为
    build_opener 注入本处理器，每跳在 redirect_request 内校验，与
    megacube_tools.state.RedirectHandler 同口径。
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urljoin(req.full_url, newurl)
        scheme = urllib.parse.urlsplit(target).scheme
        if scheme not in _FETCH_ALLOWED_SCHEMES:
            raise urllib.error.URLError(
                "redirect target rejected: non-http(s) scheme %r" % (scheme or "empty"))
        if urllib.parse.urlsplit(req.full_url).scheme == "https" and scheme != "https":
            raise urllib.error.URLError("redirect rejected: https downgrade")
        # 不携带 Authorization（_fetch 本就不发送凭据；显式剥除以与 state 版一致）。
        redirected = super().redirect_request(req, fp, code, msg, headers, target)
        if redirected is not None:
            redirected.remove_header("Authorization")
        return redirected


def _fetch_opener():
    return urllib.request.build_opener(_FetchRedirectHandler())


def _fetch(url, timeout=HTTP_TIMEOUT_S):
    """极简 http(s) GET：返回 (status, body_bytes)。

    重定向经 _FetchRedirectHandler 每跳校验（仅 http(s)、不降级 https）。
    网络层失败抛 _NetError。
    """
    req = urllib.request.Request(url, headers={"User-Agent": "megacube-admin/1.0"})
    opener = _fetch_opener()
    try:
        with opener.open(req, timeout=timeout) as resp:
            # r7/r8 P2-3：与包内 16MiB 上限（state.py）同口径的有界读；超限
            # 显式报错而非静默截断（截断会让下游把超大响应当损坏内容误报）。
            raw = resp.read(_MAX_FETCH_BYTES + 1)
            if len(raw) > _MAX_FETCH_BYTES:
                raise _NetError("响应超过 16 MiB 上限，已拒绝读取；疑似异常或被劫持的源。")
            return getattr(resp, "status", 200) or 200, raw
    except urllib.error.HTTPError as e:
        if e.code in (301, 302, 303, 307, 308):
            # 合法重定向由 _FetchRedirectHandler 自动跟随；任何 3xx 走到
            # 这里都是被拒的跳（如 file:///，urllib 在 handler 前就以
            # "not allowed" 抛出）——显式报错而非返回 (302, b'')。
            raise _NetError("重定向被拒绝（%d -> %s）。" % (
                e.code, (e.headers.get("Location") or "未知目标")[:64]))
        try:
            # r8 P2-3：错误分支同样有界。
            return e.code, e.read(_MAX_FETCH_BYTES + 1)
        except Exception:
            return e.code, b""
    except urllib.error.URLError as e:
        # 含 _FetchRedirectHandler 的跳级拒绝（非 http(s) scheme / 降级）。
        raise _NetError("网络请求失败（含重定向拒绝）：%s" % (getattr(e, "reason", None) or e))
    except (socket.timeout, TimeoutError, OSError) as e:
        raise _NetError("网络请求失败：%s" % (e,))


def _fetch_json(url, timeout=HTTP_TIMEOUT_S):
    """GET + JSON 解析。返回 (status, obj)；非 2xx 或解析失败 obj 为 None。"""
    status, body = _fetch(url, timeout=timeout)
    if not (200 <= status < 300):
        return status, None
    try:
        return status, json.loads(body.decode("utf-8"))
    except Exception:
        return status, None


def _truncate(text, limit):
    text = (text or "").strip()
    if not text:
        return None
    return text if len(text) <= limit else text[:limit] + "…"


def _slug_card_id(name_segment):
    """repo 名段 → 卡片名：小写、去点（Qwen3.8 → qwen38）、其余非法字符折成 -。"""
    slug = name_segment.lower().replace(".", "")
    slug = re.sub(r"[^a-z0-9-]+", "-", slug)
    return re.sub(r"-{2,}", "-", slug).strip("-")


def _tachyon_store_required():
    return {"ok": False, "error_code": "TACHYON_STORE_REQUIRED",
            "reason": "驭光仅支持模型商店中该模型的 H3C 官方适配卡；不能从 HF 仓库或自制本地卡安装、替换引擎。"}


def _probe_repo(repo_id):
    """Probe only the open SGLang template. Native Tachyon is store-only."""
    hf = _hf_endpoint()
    status, api_data = _fetch_json(
        "%s/api/models/%s?blobs=true" % (hf, repo_id), timeout=15)
    if status == 404:
        return {"ok": False, "reason": "仓库不存在或未公开（HF 返回 404）"}
    if status != 200:
        return {"ok": False, "reason": "仓库探测失败（HF 返回 HTTP %s）" % status}
    status, manifest = _fetch_json(
        "%s/%s/resolve/main/artifact-manifest.json" % (hf, repo_id))
    artifact = manifest.get("artifact") if status == 200 and isinstance(manifest, dict) else None
    if (repo_id.lower().endswith("-tachyon") or isinstance(artifact, dict)
            and str(artifact.get("filename") or "").lower().endswith(".tachyon")):
        return _tachyon_store_required()
    return _probe_generic_repo(hf, repo_id, api_data if isinstance(api_data, dict) else {})


def _probe_generic_repo(hf, repo_id, api_data):
    """HF 通用对话仓库判定（第二形态）：准入规则表见方案 §1。

    数据源全部来自 HF 平台 API 的机器可读字段（?blobs=true 的
    siblings[].lfs.sha256/size + raw config.json），不采信仓库自述文本。
    LFS sha256 是内容寻址摘要（平台计算，发布方改不动），是下载校验的锚。
    """
    if api_data.get("private") is True or api_data.get("gated") not in (False, None):
        return {"ok": False, "reason": "这个仓库不公开（gated 或 private），标准无凭据路径无法下载；有访问权限时可使用凭据引用或专用 CLI。"}
    lfs_files, plain_files = [], []
    for sib in api_data.get("siblings") or []:
        if not isinstance(sib, dict):
            continue
        name = sib.get("rfilename")
        # 只认仓库根目录的平铺文件：子目录/隐藏文件不入卡（也天然杜绝穿越）
        if not isinstance(name, str) or not name or "/" in name or name.startswith("."):
            continue
        lfs = sib.get("lfs")
        if isinstance(lfs, dict) and isinstance(lfs.get("size"), int) \
                and _SHA256_RE.match(str(lfs.get("sha256") or "")):
            lfs_files.append(
                {"name": name, "sha256": str(lfs["sha256"]).lower(), "size": int(lfs["size"])})
        else:
            plain_files.append(name)
    if not any(f["name"].endswith(".safetensors") for f in lfs_files):
        return {
            "ok": False,
            "reason": "仓库里没有 safetensors 权重文件（比如 GGUF 形态），仅支持 safetensors 对话模型。",
        }
    status, cfg = _fetch_json("%s/%s/raw/main/config.json" % (hf, repo_id))
    if status != 200 or not isinstance(cfg, dict):
        return {"ok": False, "reason": "缺 config.json（HTTP %s），无法判定架构族。" % status}
    archs = [str(a) for a in cfg.get("architectures") or [] if isinstance(a, str)]
    arch = next((a for a in archs if a in _GENERIC_ARCH_WHITELIST), None)
    if arch is None:
        return {
            "ok": False,
            "reason": "架构族 %s 暂未放行当前标准模板（仅适配母本同族 %s），可评估其他引擎自主安装。" % ("、".join(archs) or "未知",
                                                      "、".join(_GENERIC_ARCH_WHITELIST)),
        }
    quant = cfg.get("quantization_config")
    quant_note = ""
    if isinstance(quant, dict):
        quant_note = str(quant.get("quant_method") or quant.get("format") or "未标注")
    if not isinstance(quant, dict) or "nvfp4" not in json.dumps(quant).lower():
        return {
            "ok": False,
            "reason": "量化形态不支持：仅放行 NVFP4（与母本卡同格式），检测到 %s。"
                      "可检查其他引擎对该格式的支持。" % (quant_note or "无量化配置"),
        }
    if isinstance(cfg.get("draft_model"), str) and cfg.get("draft_model"):
        return {"ok": False, "reason": "这张卡引用了独立的 draft 仓库（EAGLE head 单独发布的双仓库形态），暂不支持。"}
    total = sum(f["size"] for f in lfs_files)
    if total > _GENERIC_MAX_WEIGHTS_BYTES:
        return {
            "ok": False,
            "reason": "权重 %.0f GiB 超过标准模板 %.0f GiB 策略上限；单机、卸载或级联可行性需结合实际硬件另行评估。"
                      % (total / _GIB, _GENERIC_MAX_WEIGHTS_BYTES / _GIB),
        }
    known_files = {f["name"] for f in lfs_files} | set(plain_files)
    if not ({"tokenizer.json", "tokenizer_config.json"} & known_files):
        return {
            "ok": False,
            "reason": "仓库缺 tokenizer（tokenizer.json / tokenizer_config.json 都没有），引擎起不来。",
        }
    mtp = "none"
    if any("mtp" in f["name"].lower() for f in lfs_files) \
            or '"num_nextn_predict_layers"' in json.dumps(cfg):
        mtp = "eagle"
    # 小文件清单排除说明类文本（README/*.md/.gitattributes）：缺了不影响运行
    small_files = sorted(
        n for n in plain_files
        if not (n.lower().startswith("readme") or n.lower().endswith(".md")))
    name_segment = repo_id.split("/", 1)[1]
    card_data = api_data.get("cardData")
    return {
        "ok": True,
        "form": "generic",
        "repo_id": repo_id,
        "model_name": (card_data.get("name") if isinstance(card_data, dict) else None)
        or name_segment,
        "architecture": arch,
        "quantization": "NVFP4",
        "weights_bytes": total,
        "lfs_files": sorted(lfs_files, key=lambda f: f["name"]),
        "small_files": small_files,
        "mtp": mtp,
        "hf_endpoint": hf,
        "third_party": True,
    }


# Values interpolated into generated shell constants must exclude shell syntax.
_TEMPLATE_VALUE_RE = re.compile(r"^[A-Za-z0-9._:/~-]+$")

_IMPORT_ROOTS_SH = r"""
# Host discovery uses newline-separated directories (including spaces).
# Emit candidates first, then configured roots, then bounded legacy roots.
lmm_import_roots() {
    [ -z "${LMM_IMPORT_CANDIDATES:-}" ] || printf '%s\n' "$LMM_IMPORT_CANDIDATES"
    [ -z "${LMM_IMPORT_DIRS:-}" ] || printf '%s\n' "$LMM_IMPORT_DIRS"
    printf '%s\n' "$HOME/models" "$HOME/LMM/model" "$HOME/LMM" "$HOME/lmm-deploy" \
        "$HOME/Downloads" "$HOME/下载" "$HOME/桌面" "$HOME/Desktop" \
        /media/*/* /media/* /mnt/* /run/media/*/*
}

lmm_verify_import() {
    echo "验证本地候选：$1"
    ( cd "$1" && sha256sum -c "$LMM_WEIGHTS_MANIFEST" ) &
    lmm_heartbeat $! "正在校验本地候选摘要"
}

lmm_copy_import_file() {
    local source="$1" target="$2" temporary
    mkdir -p "$(dirname "$target")"
    if [ -e "$target" ] || [ -L "$target" ]; then
        if [ -f "$target" ] && [ ! -L "$target" ] && cmp -s -- "$source" "$target"; then
            return 0
        fi
        echo "ERROR: 导入目标已有不同文件，保留原文件：$target" >&2
        return 1
    fi
    # Interrupted copies must not leave a half-file at the final path. Publish
    # only a completed sibling file, and never overwrite a concurrent writer.
    temporary="$(mktemp "$target.lmm-import.XXXXXX")" || return 1
    if ! cp --reflink=auto -- "$source" "$temporary"; then
        rm -f -- "$temporary"
        return 1
    fi
    if ln -- "$temporary" "$target" 2>/dev/null; then
        rm -f -- "$temporary"
        return 0
    fi
    rm -f -- "$temporary"
    if [ -f "$target" ] && [ ! -L "$target" ] && cmp -s -- "$source" "$target"; then
        return 0
    fi
    echo "ERROR: 导入发布失败或目标已有不同文件，保留原文件：$target" >&2
    return 1
}
"""

# ---------------------------------------------------------------------------
# sglang 容器卡模板（第二形态）：以真机卡 qwen38-27b-sglang0519 为母本参数化，
# 引擎参数沿用母本；独立下载器额外处理宿主候选校验与非破坏性复用。
# 部署记录按字面数据读取，使用户目录中的 shell 特殊字符保持原值。
# 其余参数化差异全部来自「仓库相关量」：
#   1. 仓库标识：REPO_ID / MODEL_SUBDIR / SERVED_MODEL_NAME / CONTAINER_NAME /
#      VENDOR_SNAPSHOT_DIR（ModelScope 缓存目录名）；
#   2. 权重清单：TOTAL_BYTES / BIG/SMALL 文件清单 / weights.sha256 /
#      PROBE_FILENAME（本地搜权重的哨兵文件名，取最大 LFS 文件）；
#   3. MTP 开关：SPEC_ARGS_LINE（eagle/none 两档，EAGLE 参数组整组同进同退）。
# 钉死不参数化（防注入面 + 保真机标定）：引擎镜像 tag（lmsysorg/sglang:v0.5.19）、
# mem-fraction-static 0.60、并发/缓存参数组、--trust-remote-code——同族才放行，
# 参数复用母本 GB10 标定值；换族必须新模板变体 + 真机标定。
# 另一处与母本的行为差异：PORT 不写死 30001（多张通用卡并行常驻会撞端口），
# 从 30001 起找空闲端口，部署过之后优先复用 .deploy-env 的落档值。
# ---------------------------------------------------------------------------
_SGLANG_SPEC_ARGS_EAGLE = (
    "--speculative-algorithm EAGLE --speculative-adaptive --enable-linear-replayssm-spec")

_TS_COMMON_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 六个入口脚本共用的一段壳。
#
# 母本是真机在役的 qwen38-27b-sglang0519 卡：SGLang v0.5.19 容器 + NVFP4 权重，
# 启动参数含 GB10 真机标定值（mem-fraction 0.60 等），同族仓库直接复用。
# 可参数化的只有仓库相关的名字、体积与文件清单（全部来自 HF 平台 API 的机器
# 可读字段）；引擎镜像 tag 与调优参数组钉死在模板里，远端元数据生成不了运行
# 参数。
#
# vendor/ 下是母本厂商交付的 4 个脚本（deploy / start / stop / uninstall），
# 入口脚本把 LMM 的语义翻译成对它们的调用（拆法见 download_model.sh 头注）。

set -euo pipefail

CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
VENDOR_DIR="$CARD_DIR/vendor"
cd "$CARD_DIR"

[ -x "$VENDOR_DIR/deploy.sh" ] || {
    echo "ERROR: 卡片包不完整，缺少 vendor/deploy.sh" >&2
    exit 1
}

# 镜像名和容器名以厂商脚本里的常量为准，这里必须跟它们一致（vendor 里是写死的，
# 不接受环境变量覆盖），改了就对不上。容器名由卡片名派生，多张卡并行互不撞名。
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="__CONTAINER_NAME__"

# 对外端口不落死值：避开 30000（容器内部端口），从 30001 起用 ss -tln 找第一个
# 空闲端口——多张卡并行常驻是纳管语义支持的，写死会让后装的卡起不来。
# 已部署过的端口优先复用（.deploy-env 落档值），保证 deploy/enable/stop 口径
# 一致；用户显式 export PORT=xxxx 时尊重用户。
if [ -z "${PORT:-}" ]; then
    _deployed_port="$(sed -n 's/^PORT="\(.*\)"$/\1/p' "$VENDOR_DIR/.deploy-env" 2>/dev/null | tail -1 || true)"
    if [ -n "$_deployed_port" ]; then
        export PORT="$_deployed_port"
    else
        for _candidate in $(seq 30001 30200); do
            if ! ss -tln 2>/dev/null | awk '{print $4}' | grep -qE ":$_candidate\$"; then
                export PORT="$_candidate"
                break
            fi
        done
    fi
fi
: "${PORT:=30001}"

# 权重目录带来源后缀单独存一份（母本同款 name-owner 规则）：不同发布方的同名
# 模型不是同一份文件，落同一个目录会互相覆盖不掉、又都通不过完整性检查。
export MODEL_DIR="${LMM_MODELS_DIR:-$HOME/models}/__MODEL_SUBDIR__"

# 镜像拉取的国内镜像站链，跟 vendor/deploy.sh 里的 MIRROR_LIST 保持一致。
#
# 这是全卡唯一一处从 vendor 抄过来的常量：装引擎那步不能调 vendor/deploy.sh
# （见 download_model.sh 头注），但又必须用同一条回退链，否则同一台设备上
# 「装引擎」能拉通而「部署」拉不通（或者反过来）会非常费解。
MIRROR_LIST="${MIRROR_LIST:-m.daocloud.io/docker.io docker.1ms.run docker.m.daocloud.io DIRECT}"

PIP_MIRROR="${PIP_MIRROR:-https://pypi.tuna.tsinghua.edu.cn/simple}"

# 权重完整性清单（规范 3.3）。取自 __REPO_ID__ 在 HF 平台 API 公布的 LFS 摘要
# （sha256 内容寻址，发布方改不动）与字节数；非 LFS 小文件 HF 只给 git blob 的
# sha1，没有可比对的 sha256，由 lmm_model_complete 查存在（与真卡口径一致：
# LFS 大文件占绝对主体）。
#
# 分两层查：
#   便宜的一层（lmm_model_complete）——大文件核对**字节数**，小文件只查存在。
#     每次点操作都会走这层。核字节数而不是只查存在很关键：下载被中断时小文件
#     早就落地了，只有几十 GB 的大文件是半截的，只查存在会让「重新下载」永远
#     误判成「已经有了」，表现就是"怎么点下载都不管用"。
#   贵的一层（verify_weights，写在 download_model.sh 里）——按 weights.sha256
#     逐个算哈希，只在下载刚结束时做一次，做完留个记号，之后不再重复算。
LMM_BIG_FILES=(
__BIG_FILES_LINES__
)
LMM_SMALL_FILES=(
__SMALL_FILES_LINE__
)
LMM_WEIGHTS_MANIFEST="$CARD_DIR/weights.sha256"
LMM_VERIFIED_MARK="$MODEL_DIR/.lmm-verified"

# 便宜的一层：大小对不对、该有的文件在不在。
lmm_model_complete() {
    local dir="${1:-$MODEL_DIR}" entry name want actual f
    [ -d "$dir" ] || return 1
    for entry in "${LMM_BIG_FILES[@]}"; do
        name="${entry%%:*}"
        want="${entry##*:}"
        actual="$(stat -c %s "$dir/$name" 2>/dev/null || echo 0)"
        [ "$actual" = "$want" ] || return 1
    done
    for f in "${LMM_SMALL_FILES[@]}"; do
        [ -f "$dir/$f" ] || return 1
    done
    return 0
}

# docker 怎么调。设备出厂就把登录用户放进了 docker 组，正常情况下不需要 sudo；
# 退化到 sudo 时只用免密的那种，交互式密码框由 LMM 的提权通道负责，脚本自己不弹。
LMM_DOCKER=""
lmm_init_docker() {
    [ -n "$LMM_DOCKER" ] && return 0
    if docker info >/dev/null 2>&1; then
        LMM_DOCKER="docker"
    elif command -v sudo >/dev/null 2>&1 && sudo -n docker info >/dev/null 2>&1; then
        LMM_DOCKER="sudo -n docker"
    else
        echo "ERROR: 连不上 Docker 服务。请确认设备的容器运行环境正常，并且当前用户在 docker 组里" >&2
        echo "       （检查：docker info；加组：sudo usermod -aG docker \$USER，之后需要重新登录）" >&2
        return 1
    fi
    return 0
}
lmm_docker() { $LMM_DOCKER "$@"; }

# 删一个路径，**容器以 root 身份写进来的东西也要能删掉**。
#
# 引擎容器是以 root 跑的，它写进挂载目录的文件属主就是 root——SGLang 的
# flashinfer JIT 产物（vendor/.cache 下面那几百个编译出来的 kernel）是典型。
# 宿主上的普通用户删不掉这些：这不是文件本身权限的事，是**父目录属于 root 且
# 没给别人写位**，unlink 这个动作根本做不了，chmod 也改不动。
#
# 办法：先普通 rm；删不动就把父目录挂进引擎镜像里以 root 身份删——**容器写进来
# 的，让容器删回去**。不用 sudo，也不弹提权框。`--entrypoint rm` 是必须的，
# 镜像自带的 ENTRYPOINT 会把后面的参数当成它自己的参数吃掉。
#
# 返回 0 = 路径已经不在了；返回 1 = 还在。调用方自己决定是告警还是失败。
lmm_rm_rf_force() {
    local p="$1"
    [ -e "$p" ] || return 0
    rm -rf -- "$p" 2>/dev/null || true
    [ -e "$p" ] || return 0

    local parent name
    parent="$(cd -- "$(dirname -- "$p")" 2>/dev/null && pwd)" || return 1
    name="$(basename -- "$p")"
    lmm_init_docker || return 1
    lmm_docker run --rm --entrypoint rm \
        -v "$parent:/lmm-rm" "$IMAGE" -rf -- "/lmm-rm/$name" >/dev/null 2>&1 || true
    [ ! -e "$p" ]
}

# 长时间不出声会被 LMM 判定为卡死并强杀（规范 6.2：装引擎/部署/启用 30 分钟，
# 下载/停用/卸载 5 分钟）。在后台任务旁边打心跳。
# 用法：<命令> & lmm_heartbeat $! "正在做什么"
lmm_heartbeat() {
    local pid="$1" what="$2" start=$SECONDS
    while kill -0 "$pid" 2>/dev/null; do
        sleep 15
        kill -0 "$pid" 2>/dev/null || break
        echo "$what…已用时 $((SECONDS - start))s"
    done
    wait "$pid"
}
"""

_TS_INSTALL_ENGINE_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 装引擎 = 把官方 SGLang 镜像拉到本机 + 验一次容器能不能看见 GPU。不下权重。
#
# 刻意不调 vendor/deploy.sh：那个脚本会一路做到下载全部权重为止（原因见
# download_model.sh 头注）。这一步只做「拉镜像」，权重交给 download_model.sh。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# 只读探针：LMM 每次下载前先以普通用户身份跑一次，命中就整段跳过安装。
#
# 不打 LMM_NEED_ROOT：设备出厂就装好了 Docker 和 NVIDIA 容器运行时，登录用户也
# 已经在 docker 组里（真机实测 docker info 免 sudo 可用、runtime 列表里有 nvidia），
# 拉镜像不需要任何提权。真碰上环境不全，用 pkexec 也修不好（usermod 要重新登录
# 才生效），不如直接把话说清楚让用户处理。
#
# 顺带：用户如果是拿 U 盘 docker load 把镜像导进来的，这里同样命中，装引擎整步跳过。
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    command -v docker >/dev/null 2>&1 || {
        echo "ERROR: 没找到 docker。这张卡需要设备出厂自带的 NVIDIA 容器运行环境" >&2
        exit 1
    }
    if docker image inspect "$IMAGE" >/dev/null 2>&1; then
        echo "probe: 引擎镜像已就绪（$IMAGE）"
        exit 0
    fi
    echo "probe: 引擎镜像尚未准备（$IMAGE）" >&2
    exit 1
fi

lmm_init_docker

echo "检查硬件…"
command -v nvidia-smi >/dev/null 2>&1 || {
    echo "ERROR: 没找到 nvidia-smi。这张卡需要 NVIDIA 驱动 580 及以上（CUDA 13）" >&2
    exit 1
}
nvidia-smi -L || {
    echo "ERROR: nvidia-smi 执行失败，请检查 NVIDIA 驱动是否正常" >&2
    exit 1
}
[ "$(uname -m)" = "aarch64" ] || echo "注意：这套方案是按 GB10（aarch64）调的，当前是 $(uname -m)，继续执行"

# ---------------------------------------------------------------------------
# 拉镜像（约 15GB 压缩体积），逐个镜像站试，第一个通的算数
# ---------------------------------------------------------------------------
if lmm_docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "引擎镜像已在本机，跳过拉取：$IMAGE"
else
    echo "拉取引擎镜像 $IMAGE（约 15GB，视网络可能要十几分钟）…"
    pulled=""
    for prefix in $MIRROR_LIST; do
        if [ "$prefix" = "DIRECT" ]; then
            ref="$IMAGE"
            echo "尝试直连 Docker Hub…"
        else
            ref="$prefix/$IMAGE"
            echo "尝试镜像站 $prefix…"
        fi
        if lmm_docker pull "$ref"; then
            pulled="$ref"
            break
        fi
        echo "这个源没拉通，换下一个" >&2
    done
    [ -n "$pulled" ] || {
        echo "ERROR: 所有镜像源都没能拉下 $IMAGE" >&2
        echo "       请检查设备的网络（能不能上外网／镜像站是否可达）后重试；" >&2
        echo "       也可以在别的机器上拉好后用 docker save/load 拷进来，本步骤会自动跳过。" >&2
        exit 1
    }
    # 从镜像站拉的带着镜像站前缀，改回官方名字，后面的 vendor 脚本认的是官方名
    if [ "$pulled" != "$IMAGE" ]; then
        lmm_docker tag "$pulled" "$IMAGE"
        lmm_docker rmi "$pulled" >/dev/null 2>&1 || true
    fi
    echo "镜像就绪：$IMAGE"
fi

# ---------------------------------------------------------------------------
# 验 GPU 直通。容器看不见 GPU 的话，后面启用那一步会以很难懂的方式失败，
# 在这里就拦下来。
# ---------------------------------------------------------------------------
echo "验证容器能否访问 GPU…"
gpu_ok=""
for candidate in "--gpus all" "--device nvidia.com/gpu=all"; do
    # shellcheck disable=SC2086
    if lmm_docker run --rm $candidate "$IMAGE" nvidia-smi -L >/dev/null 2>&1; then
        gpu_ok="$candidate"
        echo "GPU 直通可用（$candidate）"
        break
    fi
done
[ -n "$gpu_ok" ] || {
    echo "ERROR: 容器里看不到 GPU。" >&2
    echo "       多半是 NVIDIA Container Toolkit 没注册进 Docker。可以让管理员执行：" >&2
    echo "         sudo nvidia-ctk runtime configure --runtime=docker && sudo systemctl restart docker" >&2
    exit 1
}

echo "LMM_ENGINE_IMAGE=$IMAGE"
echo "引擎准备完成。接下来可以下载权重。"
"""

_TS_DOWNLOAD_MODEL_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 下载权重（体积以 weights.sha256 与 TOTAL 字节清单为准）到 $MODEL_DIR。
#
# 断点续传、进度上报、完整性校验都在这里做——vendor/deploy.sh 里那段下载是一次性
# 的（下不完就整个失败重来，也没有进度可看），不适合 LMM 的下载步骤，所以这一步
# 自己实现，vendor/deploy.sh 到了「部署」那步会发现权重已完整而直接跳过下载。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

REPO_ID="${LMM_SOURCE_REPO_ID:-__REPO_ID__}"
TOTAL="${LMM_MODEL_SIZE_BYTES:-__TOTAL_BYTES__}"
VENV_DIR="$CARD_DIR/.venv"

__IMPORT_ROOTS__

mkdir -p "$MODEL_DIR"

# 逐个文件按 HF 官方 LFS sha256 校验（规范 3.3）。weights.sha256 只列走 LFS 的
# 文件——HF 只对这些公布内容寻址 sha256，正好是权重的绝对主体；其余是几 KB 的
# 配置文件，官方只给 git blob 的 sha1，由 lmm_model_complete 查存在。
#
# 缓存绑定清单摘要与实际权重的文件身份、大小和纳秒修改/变更时间。
# 清单不变但权重补回或同尺寸改写时必须重算；真正未改的完整权重才走快速路径。
lmm_weights_fingerprint() {
    local dir="$1" entry name
    (
        for entry in "${LMM_BIG_FILES[@]}"; do
            name="${entry%%:*}"
            stat -L -c '%d:%i:%s:%y:%z' -- "$dir/$name" || exit 1
        done
    ) | sha256sum | cut -d' ' -f1
}

verify_weights() {
    local dir="$1" manifest_hash before after want_mark
    [ -f "$LMM_WEIGHTS_MANIFEST" ] || {
        rm -f -- "$LMM_VERIFIED_MARK"
        echo "ERROR: 卡片包不完整，缺少权重清单 weights.sha256" >&2
        return 1
    }
    manifest_hash="$(sha256sum "$LMM_WEIGHTS_MANIFEST" | cut -d' ' -f1)" || {
        rm -f -- "$LMM_VERIFIED_MARK"; return 1
    }
    before="$(lmm_weights_fingerprint "$dir")" || {
        rm -f -- "$LMM_VERIFIED_MARK"; return 1
    }
    want_mark="v2:$manifest_hash:$before"
    if [ -f "$LMM_VERIFIED_MARK" ] && [ "$(cat "$LMM_VERIFIED_MARK" 2>/dev/null)" = "$want_mark" ]; then
        return 0
    fi
    # An old or changed receipt cannot survive a failed new verification.
    rm -f -- "$LMM_VERIFIED_MARK" || return 1
    echo "校验权重摘要（大文件需要几分钟）…"
    ( cd "$dir" && sha256sum -c "$LMM_WEIGHTS_MANIFEST" ) &
    lmm_heartbeat $! "正在校验权重摘要" || return 1
    after="$(lmm_weights_fingerprint "$dir")" || return 1
    [ "$before" = "$after" ] || {
        echo "ERROR: 权重在校验期间发生变化，请待文件稳定后重试。" >&2
        return 1
    }
    printf '%s' "$want_mark" > "$LMM_VERIFIED_MARK"
    echo "权重摘要校验通过。"
}

if lmm_model_complete "$MODEL_DIR"; then
    verify_weights "$MODEL_DIR" || {
        echo "ERROR: 本机已有的权重没通过摘要校验，文件可能损坏了。" >&2
        echo "       请删掉 $MODEL_DIR 后重新下载。" >&2
        exit 1
    }
    echo "权重已就绪，跳过下载：$MODEL_DIR"
    echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
    exit 0
fi

# Missing/truncated files require writes. Retire the old receipt before any
# import or downloader can replace bytes, including after an interrupted retry.
rm -f -- "$LMM_VERIFIED_MARK"

# ---------------------------------------------------------------------------
# 用户可能已经把权重拷到 U 盘或某个暂存目录里了，先找一圈，找到就不走网络。
# 单个目录和总耗时都设上限：某个挂载点很慢（网络共享）时不能把整个下载拖住。
# ---------------------------------------------------------------------------
found=""
search_start=$SECONDS
while IFS= read -r dir; do
    [ -d "$dir" ] || continue
    if [ $((SECONDS - search_start)) -ge 15 ]; then
        echo "本地查找超过 15 秒，不再继续找，改走网络下载"
        break
    fi
    while IFS= read -r candidate; do
        [ -n "$candidate" ] || continue
        candidate_dir="$(dirname "$candidate")"
        if lmm_model_complete "$candidate_dir" && lmm_verify_import "$candidate_dir"; then
            found="$candidate_dir"
            break
        fi
    done < <(timeout 3 find "$dir" -maxdepth 4 -type f -name "__PROBE_FILENAME__" 2>/dev/null)
    [ -n "$found" ] && break
done < <(lmm_import_roots)

if [ -n "$found" ]; then
    echo "在 $found 找到已校验权重，复制所需文件到 $MODEL_DIR（保留原件）…"
    (
        for item in "${LMM_BIG_FILES[@]}"; do
            name="${item%:*}"
            lmm_copy_import_file "$found/$name" "$MODEL_DIR/$name" || exit 1
        done
        for name in "${LMM_SMALL_FILES[@]}"; do
            lmm_copy_import_file "$found/$name" "$MODEL_DIR/$name" || exit 1
        done
    ) &
    lmm_heartbeat $! "正在复制已校验权重"
    lmm_model_complete "$MODEL_DIR" || {
        echo "ERROR: 转移完成后权重仍不完整，请检查 $MODEL_DIR" >&2
        exit 1
    }
    # 从 U 盘/暂存目录拷进来的东西来路不明，摘要必须真算一遍
    verify_weights "$MODEL_DIR" || {
        echo "ERROR: 转移过来的权重没通过摘要校验，文件可能不完整或不是这个版本。" >&2
        echo "       请删掉 $MODEL_DIR 后改走网络下载。" >&2
        exit 1
    }
    echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
    echo "权重就绪：$MODEL_DIR"
    exit 0
fi

# ---------------------------------------------------------------------------
# 下载工具：设备上没有 hf / modelscope（真机实测），装进卡片目录下的 venv，
# 不污染系统 python。
# ---------------------------------------------------------------------------
hf_bin() {
    if [ -x "$VENV_DIR/bin/hf" ]; then echo "$VENV_DIR/bin/hf"
    elif [ -x "$VENV_DIR/bin/huggingface-cli" ]; then echo "$VENV_DIR/bin/huggingface-cli"
    else echo ""
    fi
}

if [ -z "$(hf_bin)" ]; then
    echo "准备下载工具（首次需要几分钟，pip 源：$PIP_MIRROR）…"
    if [ ! -x "$VENV_DIR/bin/pip" ]; then
        python3 -m venv "$VENV_DIR" || {
            echo "ERROR: 创建 python 虚拟环境失败。请让管理员安装 python3-venv：" >&2
            echo "         sudo apt-get install -y python3-venv" >&2
            exit 1
        }
    fi
    "$VENV_DIR/bin/pip" install --upgrade "huggingface_hub[cli]" -i "$PIP_MIRROR" &
    lmm_heartbeat $! "正在安装下载工具"
    [ -n "$(hf_bin)" ] || {
        echo "ERROR: 下载工具装好了却找不到 hf 命令，请检查 $VENV_DIR/bin" >&2
        exit 1
    }
fi
HF="$(hf_bin)"

export HF_ENDPOINT="${LMM_HF_ENDPOINT:-https://hf-mirror.com}"
[ -n "${LMM_HF_TOKEN:-}" ] && export HF_TOKEN="$LMM_HF_TOKEN"
# 新版 huggingface_hub 会自动给大文件启用 Xet 分块通道，请求直接打到
# cas-server.xethub.hf.co——那条通道不走 HF_ENDPOINT 镜像，未授权会 401，实测
# 大文件下到八成多必然失败。关掉后退回标准 resolve 接口，走的就是上面的镜像。
export HF_HUB_DISABLE_XET=1

# 已下载字节 = 落地成品 + 还在传的临时块，并夹到总量以内。
#
# 不能直接 du 整个目录：huggingface_hub 把在传数据放在 .cache/huggingface/download/
# 下，传完再移到最终路径。同一个文件既有成品又残留临时块时（并发下载被中断就会
# 这样）du 会算两遍——真机上出现过「51.0GB / 30.6GB，进度 100% 还在跑」。
lmm_downloaded_bytes() {
    local done_bytes partial sum
    done_bytes="$(find "$MODEL_DIR" -type f -not -path "*/.cache/*" -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')"
    partial="$(find "$MODEL_DIR" -name '*.incomplete' -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')"
    sum=$((done_bytes + partial))
    if [ "$TOTAL" -gt 0 ] && [ "$sum" -gt "$TOTAL" ]; then sum="$TOTAL"; fi
    printf '%d' "$sum"
}

# 后台跑下载，前台按 LMM 约定的标记行报进度。总量用卡片元数据里的真实体积，
# 不自己估算。
run_with_progress() {
    "$@" &
    local pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        echo "LMM_PROGRESS_BYTES=$(lmm_downloaded_bytes)/$TOTAL"
        sleep 2
    done
    wait "$pid"
}

echo "开始下载 $REPO_ID → $MODEL_DIR"
echo "镜像地址：$HF_ENDPOINT"
# --max-workers 4 是实测出来的：8 并发在断线时会丢弃在途分片导致进度倒退，
# 1 并发只有 4 并发四分之一的速度。
# hf 自己支持续传和跳过已完整的文件，对着同一个 --local-dir 重跑即可，不用先清空。
ok=0
if run_with_progress "$HF" download "$REPO_ID" --local-dir "$MODEL_DIR" --max-workers 4; then
    ok=1
else
    echo "从 $HF_ENDPOINT 下载失败，改用 ModelScope 重试（已下好的部分会保留）…" >&2
    if [ ! -x "$VENV_DIR/bin/modelscope" ]; then
        "$VENV_DIR/bin/pip" install modelscope -i "$PIP_MIRROR" &
        lmm_heartbeat $! "正在安装 ModelScope 下载工具"
    fi
    if [ -x "$VENV_DIR/bin/modelscope" ] &&
        run_with_progress "$VENV_DIR/bin/modelscope" download --model "$REPO_ID" --local_dir "$MODEL_DIR"; then
        ok=1
    fi
fi

if [ "$ok" != "1" ]; then
    echo "ERROR: 权重没能下完。" >&2
    echo "       已经下好的部分保留在 $MODEL_DIR，再点一次下载会从断点接着传。" >&2
    echo "       如果反复失败，请检查设备网络，或在设置里换一个 HF 镜像地址。" >&2
    exit 1
fi

# 下载成功返回后还剩的 *.incomplete 按定义就是垃圾（所有文件都已下完并移走），
# 不清掉会一直白占磁盘——真机上一次攒过 23GB。
find "$MODEL_DIR" -name '*.incomplete' -delete 2>/dev/null || true

lmm_model_complete "$MODEL_DIR" || {
    echo "ERROR: 下载结束了，但权重不完整（文件缺失或大小不对）。" >&2
    echo "       目录：$MODEL_DIR。再点一次下载会补齐缺的部分。" >&2
    exit 1
}

# 大小对了不等于内容对，逐个文件比 HF 官方 sha256。校验不过绝不静默放行——带着一份
# 坏权重往下走，报出来的会是启用那一步某个看不懂的 CUDA 错误。
verify_weights "$MODEL_DIR" || {
    echo "ERROR: 权重摘要校验没通过，下载到的内容跟官方对不上。" >&2
    echo "       请删掉 $MODEL_DIR 后重新下载；反复出现的话多半是镜像源上的副本有问题，" >&2
    echo "       可以在设置里换一个 HF 镜像地址再试。" >&2
    exit 1
}

echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
echo "权重下载完成并校验通过：$MODEL_DIR"
"""

_TS_DEPLOY_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 部署 = 生成运行配置，不启动服务。
#
# 这一步才真正把 vendor/deploy.sh 跑一遍。前面两步已经把镜像和权重都备齐了，
# 所以它的前五步（查硬件／装 Docker／装容器工具／拉镜像）全部命中「已就绪」直接
# 跳过，第六步也因为权重完整而跳过下载，实际只剩下属于「部署」的两件事：
# 探出这台机器能用的 GPU 直通方式，并写出 start/stop/uninstall 都要读的 .deploy-env。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_init_docker

lmm_docker image inspect "$IMAGE" >/dev/null 2>&1 || {
    echo "ERROR: 引擎镜像不在本机（$IMAGE），请先完成「安装引擎」" >&2
    exit 1
}
lmm_model_complete "$MODEL_DIR" || {
    echo "ERROR: 权重不完整（$MODEL_DIR），请先完成「下载」" >&2
    exit 1
}

# 重复部署必须安全：已经在跑的实例先停掉，否则待会儿 start 起来的还是旧配置的容器。
if lmm_docker ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
    echo "检测到 $CONTAINER_NAME 正在运行，先停掉再重新部署…"
    "$VENDOR_DIR/stop.sh"
fi

# SKIP_DOCKER_INSTALL=1：装系统包是要 root 的事，这张卡的定位是「设备出厂就带好了
# 容器环境」。真缺的话在「安装引擎」那步就已经报过更清楚的错了，不该等到这里才
# 悄悄弹一个 apt 出来。
echo "生成运行配置（端口 $PORT，权重 $MODEL_DIR）…"
SKIP_DOCKER_INSTALL=1 MODEL_DIR="$MODEL_DIR" PORT="$PORT" "$VENDOR_DIR/deploy.sh"

[ -f "$VENDOR_DIR/.deploy-env" ] || {
    echo "ERROR: 部署跑完了却没有生成 .deploy-env，请把上面的输出反馈给我们" >&2
    exit 1
}
echo "部署完成，可以点「启用」把服务跑起来。"
"""

_TS_ENABLE_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 启用 = 把服务跑起来，并确认它真的能用之后才返回 0。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

[ -f "$VENDOR_DIR/.deploy-env" ] || {
    echo "ERROR: 还没有部署过（缺 .deploy-env），请先完成「部署」" >&2
    exit 1
}

# vendor/start.sh 自己会等到 /health 通过为止，但**等待期间一声不吭**（sleep 15
# 的循环里没有任何输出）。LMM 对「启用」的静默上限是 30 分钟，而它默认能等 3600
# 秒——真等久了就会先被 LMM 当成卡死强杀。所以两件事都要做：
#   1. 放后台跑，前台每 15 秒打一次心跳；
#   2. 把它自己的超时压到 25 分钟，让它的报错（会带上容器日志尾巴）来得及输出，
#      而不是被 LMM 一刀切掉。
# GB10 上实测约 5 分钟就绪（加载权重 + CUDA graph 捕获），25 分钟是很宽的余量。
echo "启动 $CONTAINER_NAME（首次启动约 5 分钟：加载权重 + CUDA graph 捕获）…"
STARTUP_TIMEOUT=1500 PORT="$PORT" "$VENDOR_DIR/start.sh" &
lmm_heartbeat $! "正在等待服务就绪"

# 容器 Up 不等于模型可用，再问一次 /v1/models 才算数。顺便把引擎真正认的模型名
# 拿到手——这个名字必须跟服务返回的 id 一字不差，网关转发时会把请求里的 model
# 换成它，差一个字符所有接入的应用（品悟等）第一句话就收到 404。所以问，不写死。
model_id=""
for _ in $(seq 1 10); do
    model_id="$(curl -fsS --max-time 10 "http://127.0.0.1:$PORT/v1/models" 2>/dev/null |
        python3 -c 'import json,sys;print(json.load(sys.stdin)["data"][0]["id"])' 2>/dev/null || true)"
    [ -n "$model_id" ] && break
    echo "服务已就绪，正在确认模型标识…"
    sleep 5
done
[ -n "$model_id" ] || {
    echo "ERROR: 服务起来了，但 /v1/models 问不到模型标识。" >&2
    echo "       可以看看容器日志：docker logs --tail 100 $CONTAINER_NAME" >&2
    exit 1
}

echo "LMM_ENDPOINT_URL=http://127.0.0.1:$PORT/v1/chat/completions"
echo "LMM_ENDPOINT_MODEL=$model_id"
echo "LMM_CONTAINER_NAME=$CONTAINER_NAME"
echo "服务已就绪：http://127.0.0.1:$PORT/v1（模型标识 $model_id）"
"""

_TS_STOP_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 停用 = 停掉服务，保留容器配置、权重、镜像和缓存。已经停了再停一次也返回 0。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# vendor/stop.sh 本身就是幂等的（没有这个容器时打一行「nothing to stop」后退 0），
# 而且它会多等一会儿让驱动把统一内存回收掉——GB10 上刚停完马上再启会因为内存
# 还没还回来而失败，这段等待是必要的。
"$VENDOR_DIR/stop.sh"
"""

_TS_UNINSTALL_SH = r"""#!/bin/bash
# 本卡由 megacube-admin 按 __REPO_ID__ 的 HF 仓库元数据生成（sglang 容器形态）。
# 卸载模型：停掉并移除容器、清掉部署配置和运行期缓存。**权重删不删由用户在界面
# 上选**，LMM 用 LMM_PURGE_WEIGHTS 把这个选择传进来（规范 4.6）：
#
#   LMM_PURGE_WEIGHTS=1  彻底删除：全部权重一起删，不可撤销
#   LMM_PURGE_WEIGHTS=0  仅卸载：权重留在盘上，重装时不用再下
#
# 变量没有时按 0（保留）算，跟规范 4.6 的默认值一致。
#
# **为什么不直接调 vendor/uninstall.sh**：它非交互跑（--yes）时会无条件把
# lmsysorg/sglang:v0.5.19 这个镜像也 rmi 掉，而它又没有「保留镜像」的开关。
# 在 LMM 里镜像属于「已安装引擎」，由那边统一管理和删除——同一个镜像可能还有
# 别的卡片/方案在用，卸载一个模型不该顺手把它清了。它剩下的动作（停容器、删
# 目录）就是几行 rm，与其去跟它的交互式确认较劲，不如在这里明确写出来。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

purge_weights=0
[ "${LMM_PURGE_WEIGHTS:-0}" = "1" ] && purge_weights=1

if lmm_init_docker; then
    if lmm_docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER_NAME"; then
        echo "停止并移除容器 $CONTAINER_NAME …"
        lmm_docker stop -t 60 "$CONTAINER_NAME" >/dev/null 2>&1 || true
        lmm_docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    fi
else
    # 连不上 docker 也要把该做的做完，不能因为这个就整个卸载失败
    echo "连不上 Docker，跳过容器清理，继续卸载" >&2
fi

# 权重目录从 .deploy-env 里读一遍：用户可能在设置里改过模型目录，那时候
# common.sh 算出来的和当初部署时用的会不一样，以实际部署过的为准。
# 保留权重时同样要读——这句要报给用户的路径必须是真正占着盘的那个。
target="$MODEL_DIR"
if [ -f "$VENDOR_DIR/.deploy-env" ]; then
    deployed="$(sed -n 's/^MODEL_DIR="\(.*\)"$/\1/p' "$VENDOR_DIR/.deploy-env" | tail -1)"
    [ -n "$deployed" ] && target="$deployed"
fi

if [ "$purge_weights" = "1" ]; then
    if [ -d "$target" ]; then
        echo "正在删除权重：$target"
        rm -rf -- "$target"
    fi
fi

# 卡片自己在设备上产生的运行期状态：SGLang 的 JIT 缓存、下载工具的 venv、
# 部署配置。都是可重建的，**两种选择下都清掉**——用户选的是"保留权重"，不是
# "保留这套部署"，而且这些目录留着反而会让下次重新部署读到过期配置。
#
# 走 lmm_rm_rf_force 而不是直接 rm：.cache 里是容器以 root 写进来的 JIT 产物，
# 普通用户删不掉（原因见 common.sh 里那个函数的注释）。
#
# **删不掉也不能让整个卸载失败。** 这里清的全是可重建的缓存；而此时
# 容器和权重都已经删完了。为这点东西把一次已经做完的卸载判成失败，LMM 就不会
# 更新数据库，卡片会停在「已部署」指着一个不存在的容器——真机上踩过这个。
# 所以失败只告警，并把没删掉的路径如实列出来，不替它把话圆过去。
leftover=()
for runtime_path in "$VENDOR_DIR/.cache" "$VENDOR_DIR/.venv" "$VENDOR_DIR/.deploy-env" "$CARD_DIR/.venv"; do
    lmm_rm_rf_force "$runtime_path" || leftover+=("$runtime_path")
done
if [ "${#leftover[@]}" -gt 0 ]; then
    echo "警告：以下运行期缓存没能删除，已跳过：${leftover[*]}" >&2
    echo "      它们是容器以 root 身份写入的可重建缓存，不影响本次卸载的结果；" >&2
    echo "      需要时可在设备上执行：sudo rm -rf ${leftover[*]}" >&2
fi

if [ "$purge_weights" = "1" ]; then
    echo "卸载完成，权重已删除。引擎镜像（$IMAGE）保留，可在「已安装引擎」里单独删除。"
elif [ -d "$target" ]; then
    echo "卸载完成。按你的选择保留了模型权重：$target（重新下载这张卡时可直接复用）。"
    echo "引擎镜像（$IMAGE）同样保留，可在「已安装引擎」里单独删除。"
else
    # 不能顺口说成"权重已保留"：那个目录根本不在。
    echo "卸载完成。按你的选择不删除权重，但没有找到权重目录：$target"
fi
"""

# .deploy-env is literal KEY="VALUE" data shared with the Rust inventory readers.
# Never source it: user-selected paths may contain shell metacharacters.
_DEPLOY_ENV_READ_SH = r"""
lmm_read_deploy_env() {
    local lmm_line lmm_key lmm_value
    [ -f "$1" ] || return 0
    while IFS= read -r lmm_line || [ -n "$lmm_line" ]; do
        case "$lmm_line" in ''|\#*) continue ;; esac
        case "$lmm_line" in
            IMAGE=\"*\"|CONTAINER_NAME=\"*\"|PORT=\"*\"|MODEL_DIR=\"*\"|GPU_FLAGS=\"*\")
                lmm_key="${lmm_line%%=*}"
                lmm_value="${lmm_line#*=}"
                lmm_value="${lmm_value#\"}"
                lmm_value="${lmm_value%\"}"
                printf -v "$lmm_key" '%s' "$lmm_value"
                ;;
            *) echo "ERROR: Invalid literal deployment record: $1" >&2; return 1 ;;
        esac
    done < "$1"
}
"""

_TS_PKG_DEPLOY_SH = r"""#!/usr/bin/env bash
# ============================================================================
# deploy.sh — One-shot deployment for the card's model on NVIDIA GB10 (DGX Spark)
#
# Generated by megacube-admin from the provenance card qwen38-27b-sglang0519
# (parameterized for __REPO_ID__; runtime tuning flags are pinned, not derived
# from remote metadata).
#
# What it does:
#   1. Verifies hardware / OS (aarch64 + NVIDIA GPU, Ubuntu 24.04 assumed)
#   2. Installs Docker CE + NVIDIA Container Toolkit if missing (China mirrors)
#   3. Pulls the official image lmsysorg/sglang:v0.5.19 (Docker Hub mirrors,
#      with fallback chain) and re-tags it to the canonical name
#   4. Verifies GPU access inside a container
#   5. Downloads the model from ModelScope (fallback: hf-mirror.com), or reuses
#      an already-downloaded copy found next to this script
#   6. Writes .deploy-env, which start.sh / stop.sh / uninstall.sh read
#
# Everything is idempotent: re-running skips work that is already done.
# Override knobs (env): MODEL_DIR, PORT, MIRROR_LIST, SKIP_DOCKER_INSTALL=1
# ============================================================================
set -euo pipefail

# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.deploy-env"

IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="__CONTAINER_NAME__"
PORT="${PORT:-30000}"
MODEL_ID="__REPO_ID__"
MODEL_DIR="${MODEL_DIR:-$SCRIPT_DIR/models/__MODEL_SUBDIR__}"
VENV_DIR="$SCRIPT_DIR/.venv"

# Docker Hub mirror chain (Mainland China). First reachable one wins.
# Override with MIRROR_LIST="prefix1 prefix2 ..." (space-separated).
MIRROR_LIST="${MIRROR_LIST:-m.daocloud.io/docker.io docker.1ms.run docker.m.daocloud.io DIRECT}"

PIP_MIRROR="https://pypi.tuna.tsinghua.edu.cn/simple"
APT_MIRROR="https://mirrors.aliyun.com/ubuntu-ports/"
DOCKER_CE_MIRROR="https://mirrors.aliyun.com/docker-ce"

# Model file verification (bytes, from the HF platform API LFS metadata)
declare -A MODEL_FILES=(
__VENDOR_BIG_FILES__
)
MODEL_EXTRA_FILES=(
__VENDOR_SMALL_FILES__
)

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------
log()  { printf '\033[1;32m[deploy]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[deploy]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[deploy] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

SUDO=""
need_sudo() {
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null || die "need root privileges but sudo is not installed"
    SUDO="sudo"
  fi
}

docker_ok() { docker info >/dev/null 2>&1; }

DOCK=""
init_dock() {
  if docker_ok; then
    DOCK="docker"
  elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
    DOCK="sudo docker"
  else
    die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
  fi
}
dock() { $DOCK "$@"; }

# --------------------------------------------------------------------------
# Step 1: hardware / OS sanity
# --------------------------------------------------------------------------
log "Step 1/6: checking hardware and OS"
[ "$(uname -m)" = "aarch64" ] || warn "this solution is tuned for aarch64 (GB10); '$(uname -m)' detected — continuing anyway"
command -v nvidia-smi >/dev/null || die "nvidia-smi not found — NVIDIA driver >= 580 (CUDA 13) is required"
nvidia-smi -L || die "nvidia-smi failed — check the NVIDIA driver installation"

# --------------------------------------------------------------------------
# Step 2: Docker CE
# --------------------------------------------------------------------------
log "Step 2/6: checking Docker"
if ! command -v docker >/dev/null; then
  [ "${SKIP_DOCKER_INSTALL:-0}" = "1" ] && die "docker not installed and SKIP_DOCKER_INSTALL=1"
  need_sudo
  log "installing Docker CE from Aliyun mirror ($DOCKER_CE_MIRROR)"
  $SUDO apt-get update
  $SUDO apt-get install -y ca-certificates curl gnupg
  $SUDO install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "$DOCKER_CE_MIRROR/linux/ubuntu/gpg" | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  $SUDO chmod a+r /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] $DOCKER_CE_MIRROR/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    | $SUDO tee /etc/apt/sources.list.d/docker.list >/dev/null
  $SUDO apt-get update
  $SUDO apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  $SUDO systemctl enable --now docker
fi
init_dock
if [ "$DOCK" != "docker" ]; then
  warn "docker daemon accessed via sudo. To drop sudo: sudo usermod -aG docker \$USER (then re-login)"
fi

# --------------------------------------------------------------------------
# Step 3: NVIDIA Container Toolkit
# --------------------------------------------------------------------------
log "Step 3/6: checking NVIDIA Container Toolkit"
if ! command -v nvidia-ctk >/dev/null; then
  need_sudo
  log "installing nvidia-container-toolkit"
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | $SUDO gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    | $SUDO tee /etc/apt/sources.list.d/nvidia-container-toolkit.list >/dev/null
  $SUDO apt-get update
  $SUDO apt-get install -y nvidia-container-toolkit
  # Generate the CDI spec so both --gpus and --device nvidia.com/gpu=all work
  $SUDO nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml || true
  $SUDO systemctl restart docker
fi

# --------------------------------------------------------------------------
# Step 4: pull the official SGLang image (mirror fallback chain)
# --------------------------------------------------------------------------
log "Step 4/6: pulling $IMAGE"
if dock image inspect "$IMAGE" >/dev/null 2>&1; then
  log "image already present: $IMAGE"
else
  pulled=""
  for prefix in $MIRROR_LIST; do
    if [ "$prefix" = "DIRECT" ]; then
      ref="$IMAGE"
    else
      ref="$prefix/$IMAGE"
    fi
    log "trying: docker pull $ref"
    if dock pull "$ref"; then
      pulled="$ref"
      break
    fi
    warn "pull failed via $prefix — trying next mirror"
  done
  [ -n "$pulled" ] || die "could not pull $IMAGE from any mirror. Check the network, or set MIRROR_LIST to a reachable registry mirror."
  if [ "$pulled" != "$IMAGE" ]; then
    dock tag "$pulled" "$IMAGE"
    dock rmi "$pulled" >/dev/null 2>&1 || true
  fi
  log "image ready: $IMAGE"
fi

# --------------------------------------------------------------------------
# Step 5: verify GPU access inside the container
# --------------------------------------------------------------------------
log "Step 5/6: verifying GPU access inside a container"
GPU_FLAGS=""
for candidate in "--gpus all" "--device nvidia.com/gpu=all"; do
  # shellcheck disable=SC2086
  if dock run --rm $candidate "$IMAGE" nvidia-smi -L >/dev/null 2>&1; then
    GPU_FLAGS="$candidate"
    log "GPU access works with: $candidate"
    break
  fi
done
[ -n "$GPU_FLAGS" ] || die "container cannot see the GPU. Check nvidia-container-toolkit (see DEPLOYMENT.md → Troubleshooting)."

# --------------------------------------------------------------------------
# Step 6: model weights
# --------------------------------------------------------------------------
log "Step 6/6: checking model weights"

model_complete() {
  local dir="$1" f size actual
  [ -d "$dir" ] || return 1
  for f in "${!MODEL_FILES[@]}"; do
    [ -f "$dir/$f" ] || return 1
    size="${MODEL_FILES[$f]}"
    actual=$(stat -c %s "$dir/$f" 2>/dev/null || echo 0)
    [ "$actual" = "$size" ] || return 1
  done
  for f in "${MODEL_EXTRA_FILES[@]}"; do
    [ -f "$dir/$f" ] || return 1
  done
  return 0
}

FOUND_MODEL=""
# Candidate locations, in priority order: explicit MODEL_DIR, default dir,
# ModelScope cache layout (as shipped with this project), flat dir.
for cand in \
  "$MODEL_DIR" \
  "$SCRIPT_DIR/models/__MODEL_SUBDIR__" \
  "$SCRIPT_DIR/__VENDOR_SNAPSHOT_DIR__/snapshots/master" \
  "$SCRIPT_DIR/__VENDOR_SNAPSHOT_DIR__" \
  "$SCRIPT_DIR/__SERVED_MODEL_NAME__"; do
  if model_complete "$cand"; then FOUND_MODEL="$cand"; break; fi
done

if [ -n "$FOUND_MODEL" ]; then
  log "found complete model at: $FOUND_MODEL"
  MODEL_DIR="$FOUND_MODEL"
else
  log "downloading $MODEL_ID from ModelScope"
  mkdir -p "$MODEL_DIR"
  # ModelScope CLI in a local venv (no system-wide pip pollution)
  if [ ! -x "$VENV_DIR/bin/modelscope" ]; then
    log "creating download venv at $VENV_DIR (pip mirror: $PIP_MIRROR)"
    python3 -m venv "$VENV_DIR" || {
      need_sudo; $SUDO apt-get update && $SUDO apt-get install -y python3-venv
      python3 -m venv "$VENV_DIR"
    }
    "$VENV_DIR/bin/pip" install -q --upgrade pip -i "$PIP_MIRROR"
    "$VENV_DIR/bin/pip" install -q modelscope -i "$PIP_MIRROR"
  fi
  if ! "$VENV_DIR/bin/modelscope" download --model "$MODEL_ID" --local_dir "$MODEL_DIR"; then
    warn "ModelScope download failed — falling back to hf-mirror.com"
    "$VENV_DIR/bin/pip" install -q "huggingface_hub[cli,hf_transfer]" -i "$PIP_MIRROR"
    HF_ENDPOINT=https://hf-mirror.com "$VENV_DIR/bin/hf" download "$MODEL_ID" --local-dir "$MODEL_DIR"
  fi
  model_complete "$MODEL_DIR" || die "download finished but the model directory is incomplete: $MODEL_DIR"
  log "model ready: $MODEL_DIR"
fi

# --------------------------------------------------------------------------
# Write .deploy-env for the other scripts
# --------------------------------------------------------------------------
cat > "$ENV_FILE" <<EOF
# Written by deploy.sh on $(date -Is). Literal data; never source or eval.
IMAGE="$IMAGE"
CONTAINER_NAME="$CONTAINER_NAME"
PORT="$PORT"
MODEL_DIR="$MODEL_DIR"
GPU_FLAGS="$GPU_FLAGS"
EOF
log "wrote $ENV_FILE"

cat <<EOF

============================================================================
 Deployment complete.

   Image : $IMAGE
   Model : $MODEL_DIR
   GPU   : via $GPU_FLAGS
   Port  : $PORT

 Next steps:
   ./start.sh     # start the endpoint (~5 min to ready on GB10)
   ./stop.sh      # stop the endpoint
   ./uninstall.sh # remove container / image / (optionally) model
============================================================================
EOF
"""

_TS_PKG_START_SH = r"""#!/usr/bin/env bash
# ============================================================================
# start.sh — Start the model's OpenAI-compatible endpoint (SGLang)
#
# Generated by megacube-admin from the provenance card qwen38-27b-sglang0519
# (parameterized for __REPO_ID__; runtime tuning flags are pinned, not derived
# from remote metadata).
#
# Reads .deploy-env written by deploy.sh (falls back to built-in defaults).
# Any extra CLI arguments are appended to the sglang launch command and
# override the built-in ones, e.g.:
#   ./start.sh --max-running-requests 8
# Env overrides: PORT, MEM_FRACTION_STATIC, MAX_RUNNING_REQUESTS,
#                STARTUP_TIMEOUT (seconds, default 3600)
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults (overridden by .deploy-env when present; env vars win over both)
_ENV_PORT="${PORT:-}"
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="__CONTAINER_NAME__"
PORT="30000"
MODEL_DIR="$SCRIPT_DIR/models/__MODEL_SUBDIR__"
GPU_FLAGS="--gpus all"
__DEPLOY_ENV_READ__
lmm_read_deploy_env "$SCRIPT_DIR/.deploy-env"
PORT="${_ENV_PORT:-$PORT}"

MEM_FRACTION_STATIC="${MEM_FRACTION_STATIC:-0.60}"
MAX_RUNNING_REQUESTS="${MAX_RUNNING_REQUESTS:-16}"
STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-3600}"
CACHE_DIR="$SCRIPT_DIR/.cache/sglang"

# Speculative decoding group (EAGLE MTP): filled at card generation time from
# the repo's MTP presence — eagle/none are the two template variants. The
# group moves as a whole: EAGLE + adaptive + linear-replayssm go together,
# and a repo without MTP must launch with none of them.
SPEC_ARGS=(__SPEC_ARGS_LINE__)

log()  { printf '\033[1;32m[start]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[start] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DOCK=""
if docker info >/dev/null 2>&1; then
  DOCK="docker"
elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
  DOCK="sudo docker"
else
  die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
fi
dock() { $DOCK "$@"; }

# --------------------------------------------------------------------------
# Preconditions
# --------------------------------------------------------------------------
[ -f "$MODEL_DIR/config.json" ] || die "model not found at $MODEL_DIR — run ./deploy.sh first"
dock image inspect "$IMAGE" >/dev/null 2>&1 || die "image $IMAGE not present — run ./deploy.sh first"

if dock ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  log "container '$CONTAINER_NAME' is already running"
  log "endpoint: http://127.0.0.1:$PORT/v1   (logs: docker logs -f $CONTAINER_NAME)"
  exit 0
fi
# Remove a leftover stopped container with the same name
dock rm "$CONTAINER_NAME" >/dev/null 2>&1 || true

mkdir -p "$CACHE_DIR"

# --------------------------------------------------------------------------
# Launch
# Tuning notes (validated on GB10 on the provenance card; do not derive these
# from remote metadata — a new architecture family needs its own calibrated
# template variant):
#   mem-fraction-static 0.60  : 0.75+ exhausts the 128 GB unified memory
#   EAGLE MTP (SPEC_ARGS)     : speculative decoding w/ adaptive steps
#   bfloat16 SSM + flashinfer linear-attn kernels: ~2x SSM state capacity
#   MAX_JOBS=2                : prevents parallel nvcc from OOM-freezing the
#                               machine during first-boot JIT compilation
# --------------------------------------------------------------------------
log "starting container '$CONTAINER_NAME' (image: $IMAGE)"
# shellcheck disable=SC2086
dock run -d \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  $GPU_FLAGS \
  --ipc=host \
  --shm-size 32g \
  -p "$PORT:30000" \
  -v "$MODEL_DIR:/model:ro" \
  -v "$CACHE_DIR:/root/.cache/sglang" \
  -e MAX_JOBS=2 \
  "$IMAGE" \
  python3 -m sglang.launch_server \
    --model-path /model \
    --served-model-name __SERVED_MODEL_NAME__ \
    --host 0.0.0.0 \
    --port 30000 \
    --trust-remote-code \
    --mem-fraction-static "$MEM_FRACTION_STATIC" \
    --chunked-prefill-size 2048 \
    "${SPEC_ARGS[@]}" \
    --mamba-radix-cache-strategy extra_buffer_lazy \
    --mamba-ssm-dtype bfloat16 \
    --max-mamba-cache-size 64 \
    --linear-attn-decode-backend flashinfer \
    --linear-attn-verify-backend flashinfer \
    --linear-attn-prefill-backend flashinfer \
    --max-running-requests "$MAX_RUNNING_REQUESTS" \
    --cuda-graph-max-bs-decode "$MAX_RUNNING_REQUESTS" \
    --stream-interval 8 \
    --mm-attention-backend fa4 \
    --reasoning-parser qwen3 \
    --tool-call-parser qwen3_coder \
    "$@"

# --------------------------------------------------------------------------
# Wait for readiness. Startup = weight load (~2.5 min) + kernel warmup/JIT +
# CUDA graph capture ≈ 5 min total on GB10 (measured). The JIT cache persists
# in .cache/sglang, so later starts are at least as fast.
# Note on --mm-attention-backend: fa4 (FlashAttention 4, bundled in the image)
# is the fast ViT path on SM121. Do NOT use flashinfer_cudnn here — the
# image's cuDNN 9.20 is too old (< 9.26) and crashes the vision tower at
# warmup (cudnnGraphNotSupportedError). Fallback if fa4 ever misbehaves:
#   ./start.sh --mm-attention-backend triton_attn
# --------------------------------------------------------------------------
log "waiting for the server to become ready (timeout ${STARTUP_TIMEOUT}s)"
log "follow progress with: docker logs -f $CONTAINER_NAME"
deadline=$(( $(date +%s) + STARTUP_TIMEOUT ))
while true; do
  if ! dock ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
    echo
    dock logs --tail 50 "$CONTAINER_NAME" >&2 || true
    die "container exited during startup — see logs above"
  fi
  if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || {
    dock logs --tail 50 "$CONTAINER_NAME" >&2 || true
    die "server did not become ready within ${STARTUP_TIMEOUT}s — see logs above"
  }
  sleep 15
done

cat <<EOF

============================================================================
 Endpoint is ready.

   OpenAI-compatible API : http://127.0.0.1:$PORT/v1
   Model name            : __SERVED_MODEL_NAME__
   Container             : $CONTAINER_NAME

 Quick test:
   curl http://127.0.0.1:$PORT/v1/chat/completions \\
     -H 'Content-Type: application/json' \\
     -d '{"model":"__SERVED_MODEL_NAME__","messages":[{"role":"user","content":"Hello!"}]}'

 Manage:
   docker logs -f $CONTAINER_NAME   # logs
   ./stop.sh                        # stop
============================================================================
EOF
"""

_TS_PKG_STOP_SH = r"""#!/usr/bin/env bash
# ============================================================================
# stop.sh — Stop the model's endpoint and free the unified memory
#
# Generated by megacube-admin from the provenance card qwen38-27b-sglang0519.
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTAINER_NAME="__CONTAINER_NAME__"
__DEPLOY_ENV_READ__
lmm_read_deploy_env "$SCRIPT_DIR/.deploy-env"

log()  { printf '\033[1;32m[stop]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[stop]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[stop] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DOCK=""
if docker info >/dev/null 2>&1; then
  DOCK="docker"
elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
  DOCK="sudo docker"
else
  die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
fi
dock() { $DOCK "$@"; }

if ! dock ps -a --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  log "no container named '$CONTAINER_NAME' — nothing to stop"
  exit 0
fi

if dock ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  log "stopping '$CONTAINER_NAME' (graceful, up to 120s)"
  dock stop -t 120 "$CONTAINER_NAME" >/dev/null
fi

# --restart unless-stopped keeps the stopped container around; remove it so
# start.sh always begins from a clean state.
dock rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
log "container stopped and removed"

# Unified memory is reclaimed asynchronously by the driver; give it a moment.
log "waiting for memory to be reclaimed..."
for _ in $(seq 1 12); do
  avail=$(awk '/MemAvailable/ {print int($2/1024/1024)}' /proc/meminfo)
  [ "$avail" -ge 100 ] && break
  sleep 5
done
avail=$(awk '/MemAvailable/ {print int($2/1024/1024)}' /proc/meminfo)
log "done. MemAvailable: ${avail} GB"
[ "$avail" -ge 100 ] || warn "memory still below 100 GB available — wait ~20s before restarting"
"""

_TS_PKG_UNINSTALL_SH = r"""#!/usr/bin/env bash
# ============================================================================
# uninstall.sh — Remove the model's deployment
#
# Generated by megacube-admin from the provenance card qwen38-27b-sglang0519.
#
# Default (interactive): stops and removes the container, asks about the
# docker image, model weights, and local caches.
# Flags:
#   --all          remove everything, including the model weights
#   --keep-model   keep the model weights, remove everything else
#   --yes / -y     do not ask; use defaults (remove container+image+caches,
#                  keep the model unless --all was given)
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="__CONTAINER_NAME__"
MODEL_DIR="$SCRIPT_DIR/models/__MODEL_SUBDIR__"
__DEPLOY_ENV_READ__
lmm_read_deploy_env "$SCRIPT_DIR/.deploy-env"

REMOVE_ALL=0 ASSUME_YES=0 KEEP_MODEL=0
for arg in "$@"; do
  case "$arg" in
    --all) REMOVE_ALL=1 ;;
    --keep-model) KEEP_MODEL=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

log()  { printf '\033[1;32m[uninstall]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[uninstall] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DOCK=""
if docker info >/dev/null 2>&1; then
  DOCK="docker"
elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
  DOCK="sudo docker"
else
  die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
fi
dock() { $DOCK "$@"; }

confirm() { # confirm <question> <default:y|n>
  local q="$1" def="${2:-n}" ans
  [ "$ASSUME_YES" = "1" ] && { [ "$def" = "y" ]; return; }
  read -r -p "$q [$def] " ans
  ans="${ans:-$def}"
  [ "$ans" = "y" ] || [ "$ans" = "Y" ]
}

# 1. container ----------------------------------------------------------------
if dock ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER_NAME"; then
  log "stopping and removing container '$CONTAINER_NAME'"
  dock stop -t 60 "$CONTAINER_NAME" >/dev/null 2>&1 || true
  dock rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

# 2. image --------------------------------------------------------------------
if dock image inspect "$IMAGE" >/dev/null 2>&1; then
  if [ "$ASSUME_YES" = "1" ] || confirm "Remove docker image $IMAGE (~15 GB)?" "y"; then
    dock rmi "$IMAGE" && log "image removed"
  fi
fi

# 3. model weights -------------------------------------------------------------
if [ "$REMOVE_ALL" = "1" ] && [ "$KEEP_MODEL" = "0" ]; then
  log "removing model weights: $MODEL_DIR"
  rm -rf "$MODEL_DIR"
  # also remove the pre-shipped ModelScope cache layout if present
  rm -rf "$SCRIPT_DIR/__VENDOR_SNAPSHOT_DIR__"
elif [ -d "$MODEL_DIR" ] && [ "$KEEP_MODEL" = "0" ]; then
  if confirm "Remove model weights at $MODEL_DIR?" "n"; then
    rm -rf "$MODEL_DIR"
    log "model removed"
  else
    log "model kept: $MODEL_DIR"
  fi
fi

# 4. local state (caches, venv, env file) --------------------------------------
log "removing local state: .cache .venv .deploy-env"
rm -rf "$SCRIPT_DIR/.cache" "$SCRIPT_DIR/.venv" "$SCRIPT_DIR/.deploy-env"

cat <<EOF

============================================================================
 Uninstall complete.

 Not touched (remove manually if desired):
   - Docker CE and nvidia-container-toolkit system packages
   - apt/pip mirror configuration on the host
$( [ "$KEEP_MODEL" = "1" ] || [ "$REMOVE_ALL" = "0" ] && echo "   - model weights (if you chose to keep them)" )
============================================================================
EOF
"""

_SGLANG_CARD_TEMPLATES = {
    "common.sh": _TS_COMMON_SH,
    "install_engine.sh": _TS_INSTALL_ENGINE_SH,
    "download_model.sh": _TS_DOWNLOAD_MODEL_SH,
    "deploy.sh": _TS_DEPLOY_SH,
    "enable.sh": _TS_ENABLE_SH,
    "stop.sh": _TS_STOP_SH,
    "uninstall.sh": _TS_UNINSTALL_SH,
    "vendor/deploy.sh": _TS_PKG_DEPLOY_SH,
    "vendor/start.sh": _TS_PKG_START_SH,
    "vendor/stop.sh": _TS_PKG_STOP_SH,
    "vendor/uninstall.sh": _TS_PKG_UNINSTALL_SH,
}


def _render_sglang_card_files(probe, card_id):
    """按通用仓库探测结果渲染整张 sglang 容器卡。

    产物满足 install_local 的放行条件：六个入口脚本（含 uninstall.sh）齐全、
    common.sh 布局可辨、来源标记行（本卡由 megacube-admin 按 …）在场。
    单行模板值过 _TEMPLATE_VALUE_RE，清单值逐文件过 _valid_name（B4：防
    路径穿越/注入）；SPEC_ARGS_LINE 是 eagle/none 枚举展开，不来自远端
    元数据；未替换占位符抛 _CardError。
    """
    repo_id = probe["repo_id"]
    owner, name_segment = repo_id.split("/", 1)
    lfs_files = sorted(probe["lfs_files"], key=lambda f: f["name"])
    small_files = sorted(probe["small_files"])
    # 输入先于派生值过白名单（B4：恶意文件名在进任何模板值之前就拒）
    for fname in [f["name"] for f in lfs_files] + small_files:
        if not _valid_name(fname):
            raise _CardError("仓库文件名 %r 不在卡片白名单内，拒绝渲染。" % fname)
    for f in lfs_files:
        if not _SHA256_RE.match(f["sha256"]):
            raise _CardError("LFS 摘要不合法（%s），拒绝渲染卡片。" % f["name"])
    # 权重子目录用 name-owner（母本同款）：不同发布方的同名模型不共用目录
    values = {
        "REPO_ID": repo_id,
        "MODEL_SUBDIR": "%s-%s" % (name_segment, _slug_card_id(owner)),
        "SERVED_MODEL_NAME": name_segment,
        "CONTAINER_NAME": "sglang-%s" % card_id,
        "TOTAL_BYTES": str(probe["weights_bytes"]),
        "PROBE_FILENAME": max(lfs_files, key=lambda f: f["size"])["name"],
        "VENDOR_SNAPSHOT_DIR": "%s--%s" % (owner, name_segment),
        "SPEC_ARGS_LINE": _SGLANG_SPEC_ARGS_EAGLE if probe.get("mtp") == "eagle" else "",
    }
    for key, val in values.items():
        if val and not _TEMPLATE_VALUE_RE.match(val) \
                and not (key == "SPEC_ARGS_LINE"
                         and all(_TEMPLATE_VALUE_RE.match(t) for t in val.split())):
            # SPEC_ARGS_LINE 是 eagle/none 枚举展开（模板常量，非远端元数据），
            # 值含空格分隔的参数组，逐 token 过白名单
            raise _CardError("仓库元数据里 %s 含非法字符（%r），拒绝渲染卡片。" % (key, val))
    blocks = {
        "BIG_FILES_LINES": "\n".join(
            '    "%s:%d"' % (f["name"], f["size"]) for f in lfs_files),
        "SMALL_FILES_LINE": " ".join(small_files),
        "VENDOR_BIG_FILES": "\n".join(
            "  [%s]=%d" % (f["name"], f["size"]) for f in lfs_files),
        "VENDOR_SMALL_FILES": " ".join(small_files),
    }
    files = {}
    for fname, tpl in _SGLANG_CARD_TEMPLATES.items():
        text = tpl.replace("__IMPORT_ROOTS__", _IMPORT_ROOTS_SH)
        text = text.replace("__DEPLOY_ENV_READ__", _DEPLOY_ENV_READ_SH)
        for key, val in values.items():
            text = text.replace("__%s__" % key, val)
        for key, val in blocks.items():
            text = text.replace("__%s__" % key, val)
        if re.search(r"__[A-Z0-9_]+__", text):
            raise _CardError("模板渲染失败：%s 有未替换的占位符。" % fname)
        files[fname] = text.encode("utf-8")
    files["weights.sha256"] = "".join(
        "%s  %s\n" % (f["sha256"], f["name"]) for f in lfs_files).encode("utf-8")
    meta = {
        "id": card_id,
        "name": probe.get("model_name"),
        "size_bytes": probe["weights_bytes"],
        "engine_name": "sglang",
        "quantization": probe.get("quantization"),
        "source_repo_id": repo_id,
        "third_party": True,
        "package_files": sorted(files),
    }
    files["card.json"] = json.dumps(meta, ensure_ascii=False, indent=1).encode("utf-8")
    return files


def _card_path_stat(path):
    """Follow supported aliases, but only treat a provably missing path as absent."""
    try:
        return os.stat(path)
    except FileNotFoundError:
        current = absolute = os.path.abspath(path)
        while True:
            try:
                os.lstat(current)
            except FileNotFoundError:
                parent = os.path.dirname(current)
                if parent == current:
                    raise _CardError("卡片路径无法核实，不改动：%s" % path)
                current = parent
                continue
            except OSError as error:
                raise _CardError("卡片路径无法核实，不改动：%s" % path) from error
            try:
                metadata = os.stat(current)
            except OSError as error:
                # A dangling/looping alias is present, not a missing directory.
                raise _CardError("卡片路径无法核实，不改动：%s" % path) from error
            if current != absolute and not stat.S_ISDIR(metadata.st_mode):
                raise _CardError("卡片父路径不是目录，不改动：%s" % path)
            return metadata if current == absolute else None
    except OSError as error:
        raise _CardError("卡片路径无法核实，不改动：%s" % path) from error


def _write_card_file(base_dir, fname, content):
    """Atomically create one missing card file without replacing existing entries."""
    path = os.path.join(base_dir, fname)
    if _card_path_stat(path) is not None:
        raise _CardError("卡片文件已存在，不覆写：%s" % fname)
    parent = os.path.dirname(path)
    metadata = _card_path_stat(parent)
    if metadata is not None and not stat.S_ISDIR(metadata.st_mode):
        raise _CardError("卡片父路径不是目录，不改动：%s" % parent)
    os.makedirs(parent, exist_ok=True)
    temporary = os.path.join(parent, ".tmp-" + uuid.uuid4().hex)
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o666)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(content)
            if fname.endswith(".sh"):
                os.chmod(temporary, 0o755)
        # link publishes complete bytes atomically and refuses every existing leaf.
        os.link(temporary, path)
    finally:
        os.unlink(temporary)


def _materialize_card(card_id, files):
    """Publish a new card, or complete only genuinely missing delivery files.

    Existing byte-identical regular files (including verified file aliases) are
    retained. Unknown paths and different content are never replaced. Runtime
    output such as .deploy-env/ and .runtime/ is outside this comparison.
    """
    base = _cards_dir()
    target = os.path.join(base, card_id)
    metadata = _card_path_stat(target)
    if metadata is not None:
        if not stat.S_ISDIR(metadata.st_mode):
            raise _CardError("卡片路径已存在且不是目录，不覆写：%s" % target)
        completed = False
        for fname, content in files.items():
            path = os.path.join(target, fname)
            metadata = _card_path_stat(path)
            if metadata is None:
                _write_card_file(target, fname, content)
                completed = True
                continue
            if not stat.S_ISREG(metadata.st_mode):
                raise _CardError("现有卡片文件不是普通文件，不覆写：%s" % fname)
            # Preserve live file aliases; pin the read to a nonblocking regular FD.
            with os.fdopen(os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0)), "rb") as stream:
                if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                    raise _CardError("现有卡片文件不是普通文件，不覆写：%s" % fname)
                if stream.read(len(content) + 1) != content:
                    raise _CardError(
                        "卡片目录已存在且 %s 与现有卡片内容不一致，不覆写。" % fname)
        return "ok_completed" if completed else "ok_same"
    metadata = _card_path_stat(base)
    if metadata is not None and not stat.S_ISDIR(metadata.st_mode):
        raise _CardError("卡片父路径不是目录，不改动：%s" % base)
    os.makedirs(base, exist_ok=True)
    temporary = os.path.join(base, ".tmp-" + uuid.uuid4().hex)
    os.mkdir(temporary)
    try:
        for fname, content in files.items():
            _write_card_file(temporary, fname, content)
        if _card_path_stat(target) is not None:
            raise _CardError("卡片路径已存在，不覆写：%s" % target)
        os.rename(temporary, target)
    except Exception:
        shutil.rmtree(temporary, ignore_errors=True)
        raise
    return "ok_new"


def _store_card_files(detail, require_common_sh=True):
    """商店卡详情 → 逐个 https 下载脚本/资产并逐字节 sha256 校验。

    文件名映射：scripts 的 name → <name>.sh，assets 用原文件名。
    缺哈希、下载失败、校验不过都抛 _CardError（拒绝安装）。
    require_common_sh：驭光原生卡契约（六脚本都 source common.sh，缺了物化必坏）；
    compose 型容器卡没有 common.sh（容器信息在 install_engine.sh 与 vendor .env），
    传 False 放行。
    """
    scripts = detail.get("scripts") or {}
    assets = detail.get("assets") or {}
    script_sha = detail.get("script_sha256") or {}
    asset_sha = detail.get("asset_sha256") or {}
    wanted = []
    for name, url in sorted(scripts.items()):
        wanted.append(("%s.sh" % name, url, script_sha.get(name), "script_sha256"))
    for fname, url in sorted(assets.items()):
        wanted.append((fname, url, asset_sha.get(fname), "asset_sha256"))
    if not wanted:
        raise _CardError("商店卡片详情里没有可下载的脚本/资产，拒绝安装。")
    files = {}
    for fname, url, expect, sha_table in wanted:
        if not _valid_name(fname):
            raise _CardError("商店卡片含非法文件名 %s，拒绝安装。" % fname)
        if not url or not str(url).startswith(("https://", "http://")):
            raise _CardError("商店卡片里 %s 的下载地址缺失或不是 http(s)，拒绝安装。" % fname)
        if not expect or not _SHA256_RE.match(str(expect)):
            raise _CardError(
                "商店卡片的 %s 表缺 %s 的 sha256，为安全起见拒绝安装。" % (sha_table, fname))
        try:
            status, body = _fetch(str(url))
        except _NetError as e:
            raise _CardError("下载 %s 失败：%s" % (fname, e))
        if status != 200:
            raise _CardError("下载 %s 失败（HTTP %d）。" % (fname, status))
        actual = hashlib.sha256(body).hexdigest()
        if actual != str(expect).lower():
            raise _CardError(
                "%s 的 sha256 对不上（期望 %s，实算 %s），商店内容可能被篡改，拒绝安装。"
                % (fname, expect, actual))
        files[fname] = body
    if require_common_sh and "common.sh" not in files:
        raise _CardError("这张卡不是驭光原生卡（缺 common.sh 资产），请去模型商店界面下载。")
    # card.json：把商店元数据里 cmsd 需要的部分随卡物化（compose 一代卡目录
    # 本身不带体积口径，cmsd 的进度分母/预算兜底都靠它）。
    meta = {
        "id": detail.get("id"),
        "name": detail.get("name"),
        "size_bytes": detail.get("size_bytes"),
        "engine_name": detail.get("engine_name"),
        "engine_version": detail.get("engine_version"),
        "quantization": detail.get("quantization"),
        "source_repo_id": detail.get("source_repo_id"),
    }
    files["card.json"] = json.dumps(meta, ensure_ascii=False, indent=1).encode("utf-8")
    return files



def _tool_status():
    data = _api("/v1/status")
    if data is None:
        result = _unavailable()
        result["data"] = {"cmsd_available": False, "external_artifacts": megacube_tools.artifacts.list_artifacts(),
                          "independent_downloads_available": True}
        return result
    if data.get("error"):
        result = _observation_reply(data)
        result["data"]["external_artifacts"] = megacube_tools.artifacts.list_artifacts()
        result["data"]["independent_downloads_available"] = True
        return result
    data = dict(data, cmsd_available=True, external_artifacts=megacube_tools.artifacts.list_artifacts(),
                independent_downloads_available=True)
    models = data.get("models") or []
    running = [m for m in models if m.get("status") == "running"]
    degraded = data.get("degraded") or []
    summary = "管理面在线：Cube Manage Software %s，运行 %s 秒；活跃模型 %s；运行中 %d 个，降级 %d 个。" % (
        data.get("version", "?"),
        data.get("uptime_s", "?"),
        data.get("active_model") or "无",
        len(running),
        len(degraded),
    )
    unknown = sum(model.get("status") == "unknown" for model in models)
    if unknown:
        summary += "运行状态尚未核实 %d 个。" % unknown
    if degraded:
        summary += "降级明细：" + "；".join(
            "%s（%s）" % (d.get("card"), d.get("reason")) for d in degraded
        )
    return {
        "summary": summary,
        "data": data,
        "next": "要看每张卡细节调 cube_list_models；排障先调 cube_get_logs。",
    }


def _tool_models():
    data = _api("/v1/models")
    external = megacube_tools.artifacts.list_artifacts()
    if data is None:
        return {"summary": "Cube Manage Software 不可用；找到 %d 个外部安装记录，实时运行状态未知。" % len(external),
                "data": {"external_artifacts": external, "cmsd_available": False, "observation": "unconfirmed"},
                "next": "可检查记录目录并继续安装或验证；不能把历史记录当成正在运行。"}
    if data.get("error"):
        result = _observation_reply(data)
        result["data"]["external_artifacts"] = external
        return result
    data = dict(data, external_artifacts=external, cmsd_available=True)
    models = data.get("models") or []
    lines = []
    for m in models:
        state = "尚未核实（%s）" % m.get("reported_status", "unknown") if m.get("status") == "unknown" else m.get("status")
        parts = ["%s：%s" % (m.get("card"), state)]
        if m.get("managed") is True:
            parts.append("已托管")
        elif m.get("can_adopt") is True:
            parts.append("仅发现，可按显式启用请求接管")
        else:
            parts.append("只读观察，未授予启停权")
        if m.get("healthy") is True:
            parts.append("健康")
        if m.get("restart_count"):
            parts.append("重启过 %d 次" % m["restart_count"])
        if m.get("port"):
            parts.append("端口 %d" % m["port"])
        if m.get("measured_footprint_gib"):
            parts.append("常驻 %.1f GiB" % m["measured_footprint_gib"])
        lines.append("、".join(parts))
    owned_count = sum(model.get("managed") is True for model in models)
    summary = "共 %d 个模型：%d 个已托管、%d 个仅发现。%s" % (
        len(models),
        owned_count,
        len(models) - owned_count,
        "；".join(lines) if lines else "",
    )
    if external:
        summary += "另有 %d 个外部安装记录（实时状态未知）。" % len(external)
    return {
        "summary": summary,
        "data": data,
        "next": "managed=true 才有停用/默认管理权；can_adopt=true 的标准卡可按用户明确启用目标用 cube_start_model 接管，发现本身不授权。can_reserve_uninstall 仅允许商店清理预留，不授启停权。external_artifacts 仅为外部记录，使用已有终端核实实际服务。",
    }


def _tool_capacity():
    data = _api("/v1/capacity")
    if data is None:
        return _unavailable()
    if data.get("error"):
        return _observation_reply(data)
    mem = data.get("memory") or {}
    resident = data.get("resident_models") or []
    summary = "内存：总量 %s，当前可用 %s，安全边际 %s。" % (
        _fmt_gib(mem.get("total_gib")),
        _fmt_gib(mem.get("available_gib")),
        _fmt_gib(data.get("safety_margin_gib")),
    )
    if resident:
        summary += " 常驻模型：" + "、".join(
            "%s %s" % (r.get("card"), _fmt_gib(r.get("measured_footprint_gib")))
            for r in resident
        )
    return {
        "summary": summary,
        "data": data,
        "next": "这是运行内存，不是下载磁盘预算。下载用 cube_inspect_environment 查目标盘；未知引擎不能只用权重体积推断启动占用。",
    }


def _tool_logs(args):
    card = (args.get("card") or "").strip()
    if not _valid_name(card):
        return {
            "summary": "缺少或非法参数 card（卡片名，如 ornith15-35b-a3b-nvfp4）。",
            "data": None,
            "next": "先调 cube_list_models 拿卡片名列表。",
        }
    tail = args.get("tail")
    tail = int(tail) if isinstance(tail, (int, float)) else 100
    tail = min(max(tail, 1), 1000)
    data = _api("/v1/models/%s/logs?tail=%d" % (card, tail))
    if data is None:
        return _unavailable()
    if data.get("error"):
        return _observation_reply(data)
    lines = data.get("lines") or []
    summary = "%s 最近 %d 行日志（已脱敏）。" % (card, len(lines))
    # 日志原文走 data；summary 附最后两行帮助模型快速定位
    if lines:
        summary += " 最后两行：%s" % " ⏎ ".join(lines[-2:])
    return {
        "summary": summary,
        "data": data,
        "next": "对照已知错误模式判断；反复重启看 cube_get_status 的 restart_count。",
    }


def _selected_model_inventory(data):
    """Resolve only the daemon's current inventory; never infer version aliases."""
    def invalid():
        raise megacube_tools.ToolError("MODEL_SELECTION_UNCONFIRMED", "当前部署选择未核实，请恢复 Cube Manage Software 与商店的库存连接；不把未知状态当作未下载。")
    if not isinstance(data, dict) or data.get("error") or data.get("selection_error"):
        invalid()
    rows, bindings = data.get("models"), data.get("store_bindings", {})
    if (not isinstance(rows, list) or len(rows) > 512 or not isinstance(bindings, dict)
            or len(bindings) > 512
            or data.get("selection_policy") == "latest-installed-v1" and "store_bindings" not in data):
        invalid()
    by_runtime = {}
    def safe(value):
        return (isinstance(value, str) and len(value) <= 128
                and _NAME_RE.fullmatch(value) is not None and _valid_name(value))
    for row in rows:
        if not isinstance(row, dict) or not safe(row.get("card")) or row["card"] in by_runtime:
            invalid()
        by_runtime[row["card"]] = row
    logical_by_runtime = {}
    for logical, runtime in bindings.items():
        if (not safe(logical) or not safe(runtime) or runtime not in by_runtime
                or by_runtime[runtime].get("managed") is not True
                or runtime in logical_by_runtime or logical != runtime and logical in by_runtime):
            invalid()
        logical_by_runtime[runtime] = logical
    # store_bindings proves existing management and is validated above. Current
    # selection metadata also exists on unowned rows; use it only to associate
    # observations and resolve a preview, never to change the row's permissions.
    current_targets = dict(bindings)
    for runtime, row in by_runtime.items():
        logical = row.get("logical_card")
        if logical is None:
            continue
        if (not safe(logical)
                or logical != runtime and logical in by_runtime
                or logical in current_targets and current_targets[logical] != runtime
                or runtime in logical_by_runtime and logical_by_runtime[runtime] != logical):
            invalid()
        current_targets[logical] = runtime
        logical_by_runtime[runtime] = logical
    return by_runtime, current_targets, logical_by_runtime


def _tool_plan_switch(args):
    """换胎计划书（只读，不执行）：停谁启谁、中断窗口、内存预算（换胎口径）。"""
    card = (args.get("card") or "").strip()
    if not _valid_name(card):
        return {"summary": "缺少或非法参数 card（目标卡片名）。", "data": None, "next": "先调 cube_list_models 拿卡片名。"}
    status = _api("/v1/status")
    models = _api("/v1/models")
    if status is None or models is None:
        return _unavailable()
    for observation in (status, models):
        if observation.get("error"):
            return _observation_reply(observation)
    try:
        by_runtime, current_targets, _ = _selected_model_inventory(models)
    except megacube_tools.ToolError as error:
        return megacube_tools.error_reply(error)
    active = status.get("active_model")
    runtime = current_targets.get(card, card)
    capacity = _api("/v1/capacity?card=%s" % runtime)
    if capacity is None:
        return _unavailable()
    if capacity.get("error"):
        return _observation_reply(capacity)
    target = by_runtime.get(runtime)
    active_entry = by_runtime.get(active)
    if target is None:
        return {
            "summary": "管理面不认识卡片 %s。" % card,
            "data": None,
            "next": "先调 cube_list_models 看登记卡片清单。",
        }
    state, healthy = target.get("status"), target.get("healthy")
    ready = state == "running" and healthy is True
    if target.get("managed") is not True and (ready or target.get("can_adopt") is not True):
        can_adopt = target.get("can_adopt")
        adoptable = target.get("managed") is False and can_adopt is True
        read_only = target.get("managed") is False and can_adopt is False
        return {
            "summary": "%s%s，但尚未取得模型管理权；当前不能直接%s。" % (
                card, "已在运行且健康" if ready else "仅被发现", "设为默认" if ready else "启用或切换"),
            "data": {"ready": ready, "default_selected": active == runtime,
                     "managed": target.get("managed"),
                     "can_adopt": can_adopt if isinstance(can_adopt, bool) else None,
                     "selection_permitted": False,
                     "management_status": "adoption_required" if adoptable else "read_only" if read_only else "unconfirmed"},
            "next": ("先说明接管会授予生命周期管理权并登记恢复意图；用户明确授权后用 cube_start_model 显式接管，再重新查询状态和切换计划。不要重装或执行旧脚本。" if adoptable else
                     "此资源仅可观察，不能直接停止或设为默认；请在模型商店核实或修复标准部署，保留现有服务。" if read_only else
                     "接管能力尚未核实；先重新查询模型与管理连接，不试探性提交启停、默认选择或安装。"),
        }
    if active == runtime and ready:
        return {
            "summary": "%s 已是默认模型，且正在运行并通过健康检查，无需切换。" % card,
            "data": {"already_active": True},
            "next": "无需切换；按用户目标继续使用或验证。",
        }
    # Only positive readiness permits a selection-only plan. A default pointer
    # survives some crashes; neither it nor a missing health result proves use.
    if ready:
        return {
            "summary": "%s 已在运行且健康（非默认）：这是纯选择切换场景，不产生任何启停，无需预算评估。" % card,
            "data": {"already_running": True, "budget_ok": True},
            "next": "直接调 cube_set_default_model（仅挪默认，不动任何进程）；想换掉当前模型才需要 cube_switch_model。",
        }
    if active == runtime or state != "stopped":
        if state in ("stopped", "starting", "degraded"):
            readiness = state
            detail = {"stopped": "服务已停止", "starting": "服务仍在启动中", "degraded": "服务已降级"}[state]
        elif state == "running" and healthy is False:
            readiness, detail = "unhealthy", "健康检查未通过"
        else:
            readiness, detail = "unknown", "运行或健康状态尚未核实"
        return {
            "summary": "%s%s，%s，尚未确认可用。" % (
                "当前默认指针指向 " if active == runtime else "", card, detail),
            "data": {"default_selected": active == runtime, "ready": False,
                     "readiness": readiness, "target_status": state, "target_healthy": healthy},
            "next": "先用 cube_get_status、cube_list_models、cube_get_logs 核实任务和故障原因，按原因恢复后重新预览；暂不切默认，默认指针不证明服务可用。",
        }
    est = capacity.get("estimate") or {}
    required = est.get("required_gib")
    margin = est.get("safety_margin_gib") or 0.0
    available = est.get("available_gib")
    if not isinstance(available, (int, float)):
        available = (capacity.get("memory") or {}).get("available_gib") or 0.0
    # 换胎语义（执行侧就是先停旧再启新）：预算是「目标需要 ≤ 当前可用 +
    # 活跃模型将释放的常驻」，不是并行加开口径——按并行算会把能换的胎
    # 误报成预算不足（真机实测：73 GiB 容器在跑时三款目标全被误拒）。
    freed = 0.0
    if active_entry is not None and active_entry.get("status") == "running":
        freed = active_entry.get("measured_footprint_gib") or 0.0
    effective = available + freed
    ok = isinstance(required, (int, float)) and (required + margin) <= effective + 1e-9
    if isinstance(required, (int, float)):
        if freed > 0:
            verdict = "需要 %.1f + 安全边际 %.1f = %.1f GiB；切换释放 %s（常驻 %.1f GiB）后可用 %.1f GiB" % (
                required, margin, required + margin, active, freed, effective)
        else:
            verdict = "需要 %.1f + 安全边际 %.1f = %.1f GiB，当前可用 %.1f GiB" % (
                required, margin, required + margin, available)
        if not ok:
            verdict += "；还差 %.1f GiB" % (required + margin - effective)
        else:
            verdict += "——预算可行"
    else:
        verdict = est.get("verdict") or ""
    plan = {
        "stop": active or "（当前无活跃模型）",
        "start": card,
        "downtime_estimate": "停旧约 3s + 启新约 15-25s（权重热缓存）/ 冷启动分钟级",
        "budget_ok": ok,
        "budget_verdict": verdict,
        "budget_required_gib": required,
        "budget_effective_available_gib": round(effective, 1),
        "budget_freed_by_switch_gib": round(freed, 1),
    }
    summary = "切换计划：停 %s → 启 %s；预计中断 %s；预算：%s" % (
        plan["stop"], plan["start"], plan["downtime_estimate"], verdict)
    if not ok:
        summary += "（预算不足，不建议执行）"
    return {
        "summary": summary,
        "data": plan,
        "next": "展示停启、内存与中断影响；在已有授权范围内 cube_switch_model 执行，新增影响需确认。标准预算不足时另评估可行配置，勿重复强启。",
    }


def _tool_task(args):
    task_id = (args.get("task_id") or "").strip()
    if task_id.startswith("store:"):
        try:
            return megacube_tools.store_recovery.task(args)
        except Exception as error:
            return megacube_tools.store_recovery.failure(error)
    if task_id.startswith("fetch-"):
        try:
            data = megacube_tools.transfers.task_status(task_id)
            data.update(megacube_tools.state.page(data.get("artifacts", []), args, "artifacts"))
            data = task_page(data, args)
            return {"summary": "独立任务 %s：%s（%s）。" % (task_id, data["status"], data["kind"]),
                    "data": data, "next": task_next(data["status"], data["kind"])}
        except Exception as error:
            return megacube_tools.error_reply(error)
    if not task_id:
        # 排障入口（「模型下载失败了，帮忙处理一下」）：不带 id 时列最近任务，
        # 失败/取消的排前面，帮模型快速定位该处理哪一个。
        data = _api("/v1/tasks")
        cmsd_tasks = (data or {}).get("tasks")
        observed = isinstance(cmsd_tasks, list)
        observation_error = data.get("error") if isinstance(data, dict) else None
        store_query = data.get("store_query") if isinstance(data, dict) else None
        store_observed = isinstance(store_query, dict) and store_query.get("available") is True
        observation = {"cmsd_task_observation": "available" if observed else "unconfirmed",
                       "observation_error": observation_error,
                       "store_task_observation": "available" if store_observed else "unconfirmed",
                       "store_query": store_query}
        tasks = (cmsd_tasks if observed else []) + megacube_tools.transfers.list_tasks()
        recent = sorted(
            tasks,
            key=lambda t: (
                0 if t.get("status") in ("failed", "cancelled", "interrupted") else 1,
                -(t["created_at"] if isinstance(t.get("created_at"), (int, float)) else 0),
            ),
        )[:8]
        # Task listing is an index, not a replay of eight complete event logs.
        recent = [{k: v for k, v in t.items() if k not in ("events", "artifacts", "result")}
                  for t in recent]
        # cmsd aggregates parent and store receiver tasks. Give model callers
        # the explicit store: handle for bounded store logs. Legacy v1 proxies
        # also accept a bare receiver ID but return a different lines shape.
        for task in recent:
            handle = store_task_handle(task.get("store_run_id"))
            if handle:
                task["store_task_id"] = handle
                if task.get("source") == "store":
                    task["id"] = handle
        if not recent:
            summary = "当前无法核实设备任务，独立任务索引为空。"
            if observed:
                summary = ("管理面没有任务记录。" if store_observed else
                           "设备任务已取得，商店任务尚未核实；当前未取得任务记录。")
            return {
                "summary": summary,
                "data": {"tasks": [], **observation},
                "next": "用 cube_list_incomplete_installs 检查尚未进入模型库的商店失败；同时核实管理连接。没有查到记录不能证明没有任务，不让用户先手工摘日志。",
            }
        lines = []
        for t in recent:
            status_cn = {"running": "进行中", "succeeded": "已成功", "failed": "失败",
                         "cancelled": "已取消", "unknown": "尚未核实"}.get(t.get("status"), t.get("status", "?"))
            line = "%s（%s on %s）：%s" % (t.get("id"), t.get("kind"), t.get("card"), status_cn)
            if t.get("error"):
                line += "——%s" % str(t["error"])[:80]
            lines.append(line)
        return {
            "summary": "最近 %d 个任务（失败/取消在前）：%s" % (len(recent), "；".join(lines)) + (" 商店任务尚未核实，列表可能不完整。" if not store_observed else ""),
            "data": {"tasks": recent, **observation},
            "next": "挑中要处理的任务，带它的 id 再调 cube_get_task 看事件流与错误细节。",
        }
    # 任务 id 会拼进 /v1/tasks/<id> 请求行；store:/fetch- 前缀分支在上面已
    # 各自处理，走到这里的裸 id 与 _tool_cancel_task 同口径过白名单。
    if not _valid_name(task_id):
        return {"summary": "参数 task_id 非法。", "data": None,
                "next": "用长操作返回的 task_id 原值。"}
    data = _api("/v1/tasks/%s" % task_id)
    # Bounded server-side waiting avoids a tight LLM/tool loop or a guessed
    # minute-long sleep after a fast model has already become ready.
    wait = args.get("wait_seconds", 3)
    wait = min(15, max(0, wait)) if isinstance(wait, int) else 3
    deadline = time.monotonic() + wait
    while isinstance(data, dict) and data.get("status") == "running" and time.monotonic() < deadline:
        time.sleep(min(.5, max(0, deadline-time.monotonic())))
        data = _api("/v1/tasks/%s" % task_id)
    if data is None:
        return _unavailable()
    if not isinstance(data, dict) or data.get("_parse_error") or (
            isinstance(data.get("error"), dict) and not isinstance(data.get("id"), str)):
        observation = {"id": task_id, "status": "unknown", "error_code": "TASK_LOOKUP_FAILED"}
        # A failed device lookup must not become a fabricated task snapshot.
        # Offer a canonical handle for the known receiver naming convention;
        # do not silently resend this request to another backend.
        handle = store_task_handle(task_id) if task_id.startswith("cmsd-") else None
        if handle:
            observation["store_task_id"] = handle
        if isinstance(data, dict) and isinstance(data.get("error"), dict):
            observation["observation_error_code"] = str(data["error"].get("code", "unknown"))[:80]
            observation["observation_error"] = data["error"]
            if "http_status" in data:
                observation["http_status"] = data["http_status"]
        return {"summary": "本次未取得设备任务详情，任务状态尚未核实。", "data": observation,
                "next": "重新列出任务并使用返回的 id；商店日志用 store_task_id。单次查询失败不能证明后台离线或任务已结束。"}
    try:
        data = task_page(data, args)
    except megacube_tools.ToolError as error:
        return megacube_tools.error_reply(error)
    status = data.get("status", "?")
    summary = "任务 %s（%s on %s）：%s。" % (
        task_id,
        data.get("kind", "?"),
        data.get("card", "?"),
        {"running": "进行中", "succeeded": "已成功", "failed": "失败",
         "cancelled": "已取消", "unknown": "尚未核实"}.get(status, status),
    )
    # 下载进度：字节数 → GiB + 百分比，让用户对剩余时间有感觉
    done = data.get("progress_done_bytes")
    total = data.get("progress_total_bytes")
    if isinstance(done, (int, float)) and isinstance(total, (int, float)) and total > 0:
        label = "下载中" if status == "running" else "文件进度"
        summary += " %s %.1f/%.1f GiB（%d%%）。" % (
            label,
            done / _GIB, total / _GIB, round(done * 100.0 / total))
    if data.get("error"):
        summary += " 错误：%s" % data["error"]
    return {"summary": summary, "data": data, "next": task_next(status, data.get("kind"))}


def _task_reply(data, doing):
    """变更类统一应答：任务句柄 + 引导轮询。"""
    if data is None:
        return _unavailable()
    if data.get("observation") == "unconfirmed":
        result = _observation_reply(data)
        result["summary"] = "管理结果尚未核实；不能确认本次请求是否受理。"
        result["next"] = "先用 cube_get_task 查询已有任务并核实模型状态，保留已完成成果，不自动重发管理动作；安装可同时查 cube_list_incomplete_installs。"
        return result
    if data.get("error") is not None:
        err = data["error"]
        return {
            "summary": "被拒：%s" % (err.get("message") or err.get("code")),
            "data": data,
            "next": "说明实际错误与影响；核对目标名称、请求和管理连接后诊断。unknown_card 不证明引擎或架构不受支持；预算不足另评估配置。管理权未知或失联时不能回退托管模型旧脚本。",
        }
    task_id = data.get("task_id")
    if not isinstance(task_id, str) or not re.fullmatch(r"[A-Za-z0-9_.-]{1,160}", task_id):
        return _task_reply(_api_failure("MANAGEMENT_RESULT_UNCONFIRMED", "响应中未取得任务句柄。"), doing)
    if data.get("status") == "cancelled":
        return {
            "summary": "任务 %s 已取消。已完成和部分文件保留，能否续传取决于下载器与实际校验。" % task_id,
            "data": data,
            "next": "先查原任务；安装类用 cube_list_incomplete_installs 找回原运行，预览当前包后重试，不能用完整安装替代恢复。其他操作按实际目标核实。",
        }
    return {
        "summary": "%s已下发，任务 %s 进行中。" % (doing, task_id),
        "data": data,
        "next": "用 cube_get_task 查询任务 %s 直到终结；本阶段成功后继续用户目标，失败保留已有成果并诊断。" % task_id,
    }


def _tool_start(args):
    card = (args.get("card") or "").strip()
    if not _valid_name(card):
        return {"summary": "缺少参数 card（卡片名）。", "data": None, "next": "先调 cube_list_models 拿卡片名。"}
    return _task_reply(_api_post("/v1/models/%s/enable" % card, None), "启动 %s（不动当前默认模型）" % card)


def _tool_stop(args):
    card = (args.get("card") or "").strip()
    if not _valid_name(card):
        return {"summary": "缺少参数 card（卡片名）。", "data": None, "next": "先调 cube_list_models 拿卡片名。"}
    return _task_reply(_api_post("/v1/models/%s/disable" % card, None), "停止 %s " % card)


def _tool_set_default(args):
    card = (args.get("card") or "").strip()
    if not _valid_name(card):
        return {"summary": "缺少参数 card（卡片名）。", "data": None, "next": "先调 cube_list_models 拿卡片名。"}
    return _task_reply(
        _api_post("/v1/models/%s/set-default" % card, None),
        "默认模型切换为 %s（仅挪默认，不动任何进程）" % card,
    )


def _tool_switch(args):
    card = (args.get("card") or "").strip()
    # r11 P2-2：card 进 JSON body（POST 非 URL 路径），但仍与 start/stop/
    # set_default 同口径过白名单——保持"统一 fullmatch"声明成立。
    if not _valid_name(card):
        return {"summary": "缺少或非法参数 card（目标卡片名）。", "data": None, "next": "先调 cube_list_models 拿卡片名。"}
    return _task_reply(_api_post("/v1/switch", {"target": card}), "切换到 %s " % card)


def _local_state(model, active_card):
    """cmsd 登记卡 → active|running|downloaded|none。
    /v1/models 的实测口径是 weights_complete(bool)；weights 字段从宽解释兜底。"""
    if not isinstance(model, dict):
        return "none"
    status = str(model.get("status") or "").lower()
    if status not in _MODEL_STATUSES and status != "downloaded":
        return "unknown"
    if status == "running":
        return "active" if active_card and model.get("card") == active_card else "running"
    w = model.get("weights")
    ready = (
        model.get("weights_complete") is True
        or w is True
        or (isinstance(w, dict) and (w.get("complete") or w.get("ready")))
        or (isinstance(w, str) and w.lower() in ("complete", "ready", "downloaded", "ok"))
    )
    if ready or status == "downloaded":
        return "downloaded"
    if model.get("weights_complete") is False or w is False:
        return "none"
    return "unknown"


def _tool_store_models():
    """商店目录 ∪ cmsd /v1/models 本地状态；商店不可达时诚实降级只给本地。"""
    local = _api("/v1/models")
    status_data = _api("/v1/status")
    active = status_data.get("active_model") if isinstance(status_data, dict) else None
    local_by_card = {}
    local_known = False
    try:
        by_runtime, _, logical_by_runtime = _selected_model_inventory(local)
        local_by_card = {logical_by_runtime.get(runtime, runtime): row for runtime, row in by_runtime.items()}
        local_known = True
    except megacube_tools.ToolError:
        pass
    store_cards = None
    store_err = None
    try:
        code, body = _fetch(_store_base() + "/api/v1/cards", timeout=15)
        if code == 200:
            try:
                parsed = json.loads(body.decode("utf-8"))
                if isinstance(parsed, list):
                    store_cards = parsed
                else:
                    store_err = "目录响应不是数组"
            except Exception:
                store_err = "目录响应不是合法 JSON"
        else:
            store_err = "HTTP %d" % code
    except _NetError as e:
        store_err = str(e)

    entries = []
    seen = set()
    for c in store_cards or []:
        if not isinstance(c, dict) or not c.get("id"):
            continue
        cid = c["id"]
        seen.add(cid)
        engine = " ".join(x for x in [c.get("engine_name"), c.get("engine_version")] if x)
        size = c.get("size_bytes")
        entries.append({
            "card_id": cid,
            "name": c.get("name"),
            "description": _truncate(c.get("description"), 80),
            "size_gib": round(size / _GIB, 1) if isinstance(size, (int, float)) else None,
            "quantization": c.get("quantization"),
            "engine": engine or None,
            "local_state": _local_state(local_by_card.get(cid), active) if local_known else "unknown",
            "runtime_card": local_by_card.get(cid, {}).get("card"),
            "store_listed": True,
        })
    for cid in sorted(local_by_card):
        if cid in seen:
            continue
        entries.append({
            "card_id": cid, "name": None, "description": None, "size_gib": None,
            "quantization": None, "engine": None,
            "local_state": _local_state(local_by_card[cid], active),
            "store_listed": False,
        })

    data = {
        "store_reachable": store_cards is not None,
        "store_error": store_err,
        "local_reachable": local is not None,
        "local_observation": "available" if local_known else "unconfirmed",
        "local_error": local.get("error") if isinstance(local, dict) else None,
        "status_error": status_data.get("error") if isinstance(status_data, dict) else None,
        "models": entries,
    }
    counts = {}
    for e in entries:
        counts[e["local_state"]] = counts.get(e["local_state"], 0) + 1
    state_text = "、".join(
        "%s %d" % ({"active": "使用中", "running": "运行中",
                    "downloaded": "已下载", "none": "未下载", "unknown": "尚未核实"}.get(k, k), v)
        for k, v in sorted(counts.items())) or "无"
    if store_cards is None:
        summary = ("商店目录暂时不可达（%s），以下只含本机管理面登记的 %d 个模型：%s。"
                   % (store_err, len(entries), state_text))
    else:
        overview = "；".join(
            "%s（%s·%s，本地：%s）" % (
                e["card_id"],
                ("%.1f GiB" % e["size_gib"]) if e["size_gib"] is not None else "体积未知",
                e["quantization"] or "量化未知",
                {"active": "使用中", "running": "运行中",
                 "downloaded": "已下载", "none": "未下载", "unknown": "尚未核实"}[e["local_state"]])
            for e in entries[:3])
        summary = "商店共 %d 款模型，本地状态：%s。%s%s" % (
            len(entries), state_text,
            "热门：" + overview if overview else "",
            "（其余见 data）" if len(entries) > 3 else "")
        if not local_known:
            summary += " 注意本机当前部署尚未核实，本地状态未知，不能据此重新安装。"
    return {
        "summary": summary,
        "data": data,
        "next": ("先恢复 Cube Manage Software 与商店库存连接，重新核实本地模型；不要因 unknown 重新安装。" if not local_known else
                 "local_state=unknown 的卡片尚未核实，不能据此重新安装；先核实其现有部署与任务，其他已确认候选仍可按用户授权继续。" if counts.get("unknown") else
                 "把候选款的体积和本地状态摆给用户选；选定后在已授权范围内调 cube_install_model(card_id=...)，未知体积或时间如实说明；商店没有的型号先用 cube_check_model_repo 探测。"),
    }


def _tool_check_repo(args):
    repo_id = (args.get("repo_id") or "").strip()
    if not _valid_repo(repo_id):
        return {
            "summary": "参数 repo_id 非法（形如 unsloth/Qwen3.8-27B-NVFP4，只允许字母数字 . _ - 和一个 /）。",
            "data": None,
            "next": "核对拼写后重试。",
        }
    try:
        probe = _probe_repo(repo_id)
    except _NetError as e:
        return {
            "summary": "暂时连不上模型仓库：%s" % e,
            "data": {"repo_id": repo_id, "card_ready": False, "reachable": False},
            "next": "检查网络后重试；持续失败换个网络环境再试。",
        }
    if not probe["ok"]:
        if probe.get("error_code") == "TACHYON_STORE_REQUIRED":
            return {"summary": probe["reason"], "data": {"repo_id": repo_id,
                    "card_ready": False, "standard_install_supported": False,
                    "error_code": "TACHYON_STORE_REQUIRED"},
                    "next": "用 cube_list_store_models 找对应模型的 H3C 官方适配卡，再用 cube_install_model(card_id=...)；没有官方适配时评估其他引擎。"}
        return {
            "summary": "这个仓库没有可代办安装的模型包（当前标准路径）：%s。" % probe["reason"],
            "data": {"repo_id": repo_id, "card_ready": False, "standard_install_supported": False,
                     "alternative_execution_possible": "unknown", "reason": probe["reason"]},
            "next": "这仅表示标准路径不支持。用 cube_inspect_source 检查事实；说明其他路径的具体风险并取得用户确认后，使用独立下载和已有终端继续安装。",
        }
    if probe.get("form") == "generic":
        # 通用对话仓库（第三方 sglang 容器卡）：第三方来源必须明示
        gib = round(probe["weights_bytes"] / _GIB, 1)
        data = {
            "repo_id": repo_id,
            "form": "generic",
            "model_name": probe["model_name"],
            "architecture": probe["architecture"],
            "quantization": probe["quantization"],
            "weights_gib": gib,
            "mtp": probe["mtp"],
            "lfs_file_count": len(probe["lfs_files"]),
            "third_party": True,
            "card_ready": True,
        }
        summary = (
            "仓库 %s 是可代办的第三方对话模型（sglang 容器形态）：%s（架构 %s，量化 %s%s），"
            "权重 %.1f GiB，共 %d 个 LFS 权重文件。"
            % (repo_id, probe["model_name"] or "未命名", probe["architecture"],
               probe["quantization"], "，带 MTP" if probe["mtp"] == "eagle" else "",
               gib, len(probe["lfs_files"])))
        return {
            "summary": summary,
            "data": data,
            "next": "此标准模板路径未经官方验证；模型发布方身份需单独核实。把体积（%.1f GiB）、量化、耗时是否已知和"
                    "「未经官方验证」如实告诉用户，在已取得的来源/风险授权范围内调 cube_install_model（repo_id=%s）。"
                    % (gib, repo_id),
        }


def _install_from_store(card_id):
    try:
        status, detail = _fetch_json(
            "%s/api/v1/cards/%s" % (_store_base(), card_id), timeout=15)
    except _NetError as e:
        return {"summary": "暂时连不上模型商店：%s" % e, "data": None,
                "next": "检查网络后重试。"}
    if status == 404:
        return {"summary": "商店里没有卡片 %s。" % card_id, "data": None,
                "next": "调 cube_list_store_models 拿在售卡片清单。"}
    if not isinstance(detail, dict):
        return {"summary": "商店卡片详情拉取失败（HTTP %s）。" % status, "data": None,
                "next": "稍后重试；持续失败说明商店服务异常。"}
    # 可代办判型（与 cmsd 纳管口径同步）：驭光原生卡（engine_is_house=True 且带
    # common.sh 资产）或 compose 型容器对话引擎（sglang/vllm——cmsd W6/W7 已
    # 纳管启停/切换/自启）。engine_name 是展示标签不能判原生型，但容器系只
    # 有这两个引擎出 OpenAI 对话接口；diffusers/comfyui 等图像类不是对话模型，
    # 接了品悟也用不了，明确拒并引导商店界面。
    engine_name = str(detail.get("engine_name") or "")
    is_house_native = detail.get("engine_is_house") is True \
        and isinstance(detail.get("assets"), dict) and "common.sh" in detail["assets"]
    is_docker_chat = engine_name.strip().lower() in _DOCKER_CHAT_ENGINES
    if not is_house_native and not is_docker_chat:
        return {
            "summary": "这张卡（引擎 %s）不是管理面能代办的对话引擎形态（支持驭光原生卡与 sglang/vllm 容器卡），请去模型商店界面下载。"
                       % (engine_name or "未知"),
            "data": {"card_id": card_id, "engine_name": engine_name},
            "next": "标准对话卡路径不支持此引擎；可使用商店界面，也可检查来源，在用户确认风险后通过独立下载和已有终端安装。",
        }
    try:
        files = _store_card_files(detail, require_common_sh=is_house_native)
        state = _materialize_card(card_id, files)
    except _CardError as e:
        return {"summary": "拒绝安装：%s" % e, "data": {"card_id": card_id},
                "next": "把原因原样转告用户；怀疑商店内容被篡改时不要沉默重试，先反馈。"}
    except OSError as e:
        return {"summary": "卡片落盘失败：%s" % e, "data": {"card_id": card_id},
                "next": "检查磁盘空间和目录权限后重试。"}
    doing = "安装 %s（卡片已%s，开始下载权重）" % (
        card_id, "就绪" if state == "ok_same" else "物化")
    return _task_reply(_api_post("/v1/models/%s/install" % card_id, None), doing)


def _install_from_repo(repo_id):
    try:
        probe = _probe_repo(repo_id)
    except _NetError as e:
        return {"summary": "暂时连不上模型仓库：%s" % e, "data": None,
                "next": "检查网络后重试。"}
    if not probe["ok"]:
        if probe.get("error_code") == "TACHYON_STORE_REQUIRED":
            return {"summary": probe["reason"], "data": {"repo_id": repo_id,
                    "card_ready": False, "standard_install_supported": False,
                    "error_code": "TACHYON_STORE_REQUIRED"},
                    "next": "用 cube_list_store_models 找对应模型的 H3C 官方适配卡，再用 cube_install_model(card_id=...)；没有官方适配时评估其他引擎。"}
        return {
            "summary": "这个仓库没有可代办安装的模型包（当前标准路径）：%s。" % probe["reason"],
            "data": {"repo_id": repo_id, "card_ready": False, "standard_install_supported": False,
                     "alternative_execution_possible": "unknown", "reason": probe["reason"]},
            "next": "用 cube_inspect_source 继续评估；用户确认实验路径风险后可 cube_fetch_repo/cube_fetch_files 下载并用已有终端安装，不必放弃用户指定模型。",
        }
    name_segment = repo_id.split("/", 1)[1]
    card_id = _slug_card_id(name_segment)
    if not card_id:
        return {"summary": "repo 名段 %s 无法转成合法卡片名。" % name_segment,
                "data": {"repo_id": repo_id}, "next": "换一个命名规范的仓库。"}
    try:
        files = _render_sglang_card_files(probe, card_id)
        state = _materialize_card(card_id, files)
    except _CardError as e:
        return {"summary": "拒绝安装：%s" % e, "data": {"repo_id": repo_id, "card_id": card_id},
                "next": "把原因原样转告用户；目录内容不一致时不要强行覆写。"}
    except OSError as e:
        return {"summary": "卡片落盘失败：%s" % e, "data": {"repo_id": repo_id},
                "next": "检查磁盘空间和目录权限后重试。"}
    doing = "安装 %s（已按 %s 的 HF 元数据生成 sglang 容器卡 %s，开始下载权重）" % (
        probe["model_name"] or name_segment, repo_id, card_id)
    weights_bytes = probe["weights_bytes"]
    data = _api_post("/v1/models/%s/install" % card_id, None)
    reply = _task_reply(data, doing)
    if isinstance(reply.get("data"), dict) and "error" not in reply["data"]:
        reply["data"] = dict(reply["data"], card_id=card_id, weights_gib=round(
            weights_bytes / _GIB, 1))
    return reply


def _tool_install_model(args):
    card_id = (args.get("card_id") or "").strip()
    repo_id = (args.get("repo_id") or "").strip()
    if bool(card_id) == bool(repo_id):
        return {
            "summary": "参数二选一：card_id（商店已收录的卡）或 repo_id（商店未收录的 HF 仓库），只传一个。",
            "data": None,
            "next": "商店卡先 cube_list_store_models 拿 card_id；未收录仓库先 cube_check_model_repo 探测。",
        }
    if card_id:
        if not _valid_name(card_id):
            return {"summary": "参数 card_id 非法（只允许字母数字 . _ -）。",
                    "data": None, "next": "用 cube_list_store_models 返回的 card_id 原值。"}
        return _install_from_store(card_id)
    if not _valid_repo(repo_id):
        return {"summary": "参数 repo_id 非法（形如 unsloth/<模型名>）。",
                "data": None, "next": "核对拼写后重试。"}
    return _install_from_repo(repo_id)


def _tool_install_local_card(args):
    """Opt-in execution of a prepared local card through the existing store runner."""
    card_id = args.get("card_id")
    if not isinstance(card_id, str) or not _valid_name(card_id):
        return {"summary": "本地 card_id 非法。", "data": {"error_code": "INVALID_CARD_ID"}, "next": "使用唯一的目录名，不传路径。"}
    try:
        directory = megacube_tools.state.absolute_dir(os.path.join(_cards_dir(), card_id))
        for name in ("install_engine", "download_model", "deploy", "enable", "stop", "uninstall"):
            if not megacube_tools.state.target_file(directory, name + ".sh").is_file():
                raise megacube_tools.ToolError("INCOMPLETE_CARD", "本地标准卡缺少六脚本入口；可以继续独立安装并登记外部记录。")
        metadata = directory / "card.json"
        if metadata.exists():
            value = megacube_tools.state.read_json(metadata)
            engine = str(value.get("engine_name") or "") if isinstance(value, dict) else ""
            if re.search(r"tachyon|驭光", engine, re.IGNORECASE):
                raise megacube_tools.ToolError("TACHYON_STORE_REQUIRED", _tachyon_store_required()["reason"])
        # Also recognize old generated cards and cards without metadata. This
        # is a product admission rule, not attestation of arbitrary shell code.
        for name in ("install_engine.sh", "download_model.sh", "deploy.sh", "enable.sh", "stop.sh", "uninstall.sh", "common.sh"):
            path = megacube_tools.state.target_file(directory, name)
            if path.is_file() and re.search(r"tachyon|驭光", path.read_text(encoding="utf-8"), re.IGNORECASE):
                raise megacube_tools.ToolError("TACHYON_STORE_REQUIRED", _tachyon_store_required()["reason"])
    except Exception as error:
        result = megacube_tools.error_reply(error)
        if isinstance(error, megacube_tools.ToolError) and error.code == "TACHYON_STORE_REQUIRED":
            result["next"] = "用 cube_list_store_models 找对应模型的 H3C 官方适配卡，再用 cube_install_model(card_id=...)；没有适配时评估其他引擎。"
        return result
    result = _task_reply(_api_post("/v1/models/%s/install" % card_id, None), "执行本地卡 %s " % card_id)
    if isinstance(result.get("data"), dict) and "error" not in result["data"]:
        result["data"]["card_id"] = card_id
        result["data"]["execution_effects"] = ["install_engine", "download_or_reuse_weights", "deploy"]
        result["data"]["management_status"] = "not_confirmed_by_installation"
        result["next"] += " 这是安装任务；已有服务接入应核实标准卡及管理权后显式启用，不以重装代替；无法接入仍登记独立成果。"
    return result


def _tool_cancel_task(args):
    task_id = (args.get("task_id") or "").strip()
    if task_id.startswith("store:"):
        try:
            return megacube_tools.store_recovery.cancel(args)
        except Exception as error:
            return megacube_tools.store_recovery.failure(error)
    if task_id.startswith("fetch-"):
        try:
            return megacube_tools.transfers.cancel_task(task_id)
        except Exception as error:
            return megacube_tools.error_reply(error)
    if not task_id:
        return {"summary": "缺少参数 task_id。", "data": None,
                "next": "任务 id 由 cube_install_model / 启用停用等长操作返回。"}
    if not _NAME_RE.fullmatch(task_id):
        return {"summary": "参数 task_id 非法。", "data": None,
                "next": "用长操作返回的 task_id 原值。"}
    return _task_reply(
        _api_post("/v1/tasks/%s/cancel" % task_id, None), "取消任务 %s " % task_id)


TOOLS = [
    {
        "name": "cube_get_status",
        "description": "查本机 MegaCube 管理面（cmsd）状态：版本/在线/活跃模型/降级列表。排障第一步。",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "cube_list_models",
        "description": "列出本机 MegaCube 已登记模型：状态/健康/端口/重启计数/实测常驻内存。",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "cube_get_capacity",
        "description": "查本机 MegaCube 内存容量：总量/可用/安全边际/常驻模型实测占用。装新模型前先查它。",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "cube_get_logs",
        "description": "读本机某个模型的引擎日志尾部（已脱敏）。排障用。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "card": {"type": "string", "description": "卡片名（cube_list_models 返回的 card 字段）。"},
                "tail": {"type": "number", "description": "尾部行数，默认 100，上限 1000。"},
            },
            "required": ["card"],
        },
    },
    {
        "name": "cube_start_model",
        "description": "按用户明确启用目标启动本机已部署的 MegaCube 模型，不动当前默认。can_adopt=true 的标准卡会在此次显式启用时交由 cmsd 管理，发现本身不授权；外部安装记录不获控制权。幂等：在跑且健康直接成功。先检查真实容量，新增中断或资源影响时确认。",
        "inputSchema": {
            "type": "object",
            "properties": {"card": {"type": "string", "description": "卡片名（cube_list_models 的 card 字段）。"}},
            "required": ["card"],
        },
    },
    {
        "name": "cube_stop_model",
        "description": "停止本机已托管的 MegaCube 模型并撤销 cmsd 恢复意图。若停的是当前默认模型，默认指针会清空。仅发现的资源没有停止权限。告知下线影响并按用户授权执行；不能擅自停用户未授权的服务。",
        "inputSchema": {
            "type": "object",
            "properties": {"card": {"type": "string", "description": "卡片名。"}},
            "required": ["card"],
        },
    },
    {
        "name": "cube_set_default_model",
        "description": "仅把本机默认模型挪到一个已在跑且健康的本机模型（两个都在跑时的纯选择切换，不动任何进程）。目标没在跑会被拒绝——那种情况先 cube_start_model 或 cube_switch_model。",
        "inputSchema": {
            "type": "object",
            "properties": {"card": {"type": "string", "description": "目标卡片名。"}},
            "required": ["card"],
        },
    },
    {
        "name": "cube_plan_switch",
        "description": "生成本机换胎计划书（只读，不执行）：停谁启谁/中断窗口/内存预算。切换前先调它核实影响，已有授权覆盖该影响时无需重复确认。",
        "inputSchema": {
            "type": "object",
            "properties": {"card": {"type": "string", "description": "目标卡片名。"}},
            "required": ["card"],
        },
    },
    {
        "name": "cube_switch_model",
        "description": "切换本机活跃模型（串行换胎：停旧的→启新的→失败自动回滚）。任何时刻只有一个模型常驻。先核实停启、中断与预算，按用户授权范围执行；新增影响需确认。",
        "inputSchema": {
            "type": "object",
            "properties": {"card": {"type": "string", "description": "目标卡片名。"}},
            "required": ["card"],
        },
    },
    {
        "name": "cube_get_task",
        "description": "查本机任务：带 task_id 查长任务状态、日志事件与进度；不带参数列最近任务。设备任务用 id，商店任务用 store_task_id（store: 前缀），不能直接把 store_run_id 当设备任务 id。默认仅末尾8条；根因可能在较早日志，可用 events_offset=0/events_limit 分页读取。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "task_id": {"type": "string", "description": "设备任务的 id，或商店任务的 store_task_id（store:run_id）。"},
            },
            "required": ["task_id"],
        },
    },
    {
        "name": "cube_list_store_models",
        "description": "列出模型商店目录并合并本机状态（active/running/downloaded/none/unknown）：每款带体积/量化/引擎/简介。帮用户选模型第一步。商店不可达时诚实降级只给本地部分。",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "cube_check_model_repo",
        "description": "只读判断 HF 仓库能否使用当前 SGLang 标准模板（母本同族 NVFP4）；驭光只接受官方商店卡。不支持仅代表标准路径受限；继续用 cube_inspect_source、独立下载和已有终端评估自主安装。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "repo_id": {"type": "string", "description": "HF 仓库 id，形如 unsloth/Qwen3.8-27B-NVFP4。驭光需使用官方商店卡。"},
            },
            "required": ["repo_id"],
        },
    },
    {
        "name": "cube_install_model",
        "description": "在本机下载安装模型（变更）：二选一——card_id（商店已收录的卡）或 repo_id（商店未收录的 HF 仓库，应先过 cube_check_model_repo；只支持放行的通用对话仓库；驭光仅使用官方商店 card_id）。成功后返回 task_id，用 cube_get_task 轮询进度。按用户授权范围执行，来源未经 Pinvou 验证时需说明；体积和耗时未知则明确未知。标准路径不支持时用独立下载与已有终端继续评估。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "card_id": {"type": "string", "description": "商店卡片 id（cube_list_store_models 的 card_id 字段）。与 repo_id 二选一。"},
                "repo_id": {"type": "string", "description": "商店未收录的 HF 通用对话仓库 id；驭光仅使用官方商店卡。与 card_id 二选一。"},
            },
        },
    },
    {
        "name": "cube_cancel_task",
        "description": "请求取消进行中的长任务；继续查询最终状态。保留已完成和部分文件，能否续传取决于下载器与校验。标准商店恢复先查未完成索引并预览原任务，不自动完整重装。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "task_id": {"type": "string", "description": "任务 id。"},
            },
            "required": ["task_id"],
        },
    },
]

_DISPATCH = {
    "cube_get_status": lambda args: _tool_status(),
    "cube_list_models": lambda args: _tool_models(),
    "cube_get_capacity": lambda args: _tool_capacity(),
    "cube_get_logs": _tool_logs,
    "cube_get_task": _tool_task,
    "cube_start_model": _tool_start,
    "cube_stop_model": _tool_stop,
    "cube_switch_model": _tool_switch,
    "cube_set_default_model": _tool_set_default,
    "cube_plan_switch": _tool_plan_switch,
    "cube_list_store_models": lambda args: _tool_store_models(),
    "cube_check_model_repo": _tool_check_repo,
    "cube_install_model": _tool_install_model,
    "cube_cancel_task": _tool_cancel_task,
    "cube_install_local_card": _tool_install_local_card,
}

TOOLS.append({
    "name": "cube_install_local_card",
    "description": "执行 cards/<card_id>/ 的安装、下载/复用、部署脚本，可能触发既有提权；这是安装入口，不能用于把已运行服务直接纳管，也不证明已经取得管理权。已有标准卡接入先核实卡片/配置/管理权，再按授权显式启用；其他服务可登记外部成果。不从商店重新取脚本。",
    "inputSchema": {"type": "object", "properties": {"card_id": {"type": "string"}},
                    "required": ["card_id"], "additionalProperties": False},
})
TOOLS.extend(megacube_tools.TOOLS)
_DISPATCH.update(megacube_tools.DISPATCH)
# Task listing is intentionally callable without an id (including after MCP restart).
next(t for t in TOOLS if t["name"] == "cube_get_task")["inputSchema"]["required"] = []
next(t for t in TOOLS if t["name"] == "cube_get_task")["inputSchema"]["properties"].update({
    "events_offset": {"type": "integer", "minimum": 0,
                      "description": "保留事件列表的起点；不传默认返回最新8条。"},
    "events_limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 8},
    "wait_seconds": {"type": "integer", "minimum": 0, "maximum": 15, "default": 3,
                     "description": "Wait briefly for a cmsd task to finish; bounded to 15 seconds. No effect on external fetch tasks."},
    "artifacts_offset": {"type": "integer", "minimum": 0},
    "artifacts_limit": {"type": "integer", "minimum": 1, "maximum": 500},
})


def _send(msg):
    sys.stdout.write(json.dumps(msg, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def _result(req_id, result):
    _send({"jsonrpc": "2.0", "id": req_id, "result": result})


def _error(req_id, code, message):
    _send({"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}})


def _text_content(payload, is_error=False):
    return {
        "content": [{"type": "text", "text": json.dumps(payload, ensure_ascii=False)}],
        "isError": is_error,
    }


def _handle(msg):
    method = msg.get("method")
    req_id = msg.get("id")
    if req_id is None:
        return
    if method == "initialize":
        _result(req_id, {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "megacube-admin", "version": "1.6.0"},
        })
    elif method == "tools/list":
        _result(req_id, {"tools": TOOLS})
    elif method == "tools/call":
        params = msg.get("params") or {}
        name = params.get("name")
        handler = _DISPATCH.get(name)
        if handler is None:
            _error(req_id, -32601, "unknown tool: %s" % name)
            return
        try:
            response = handler(params.get("arguments") or {})
            data = response.get("data") if isinstance(response, dict) else None
            failed = isinstance(data, dict) and "error_code" in data and ("status" not in data or data.get("observation") == "unconfirmed")
            _result(req_id, _text_content(response, is_error=failed))
        except megacube_tools.ToolError as error:
            response = megacube_tools.error_reply(error)
            response["next"] = "先核对本机设备连接和记录目录，修复后重新核实原任务；不要因查询失败重复提交管理动作。独立下载工具仍可使用。"
            _result(req_id, _text_content(response, is_error=True))
        except Exception as e:
            # r20 P2-2：异常串可内嵌响应片段（如 JSONDecodeError 引用原文），
            # 出站前脱敏。
            _result(req_id, _text_content({
                "summary": "查询失败：%s" % _redact_text(str(e)),
                "data": None,
                "next": "稍后重试；持续失败说明管理面异常。",
            }, is_error=True))
    else:
        _error(req_id, -32601, "method not found: %s" % method)


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except Exception:
            continue
        try:
            _handle(msg)
        except Exception as e:
            rid = msg.get("id") if isinstance(msg, dict) else None
            if rid is not None:
                _error(rid, -32603, "internal error: %s" % e)


if __name__ == "__main__":
    main()
