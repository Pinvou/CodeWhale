#!/usr/bin/env python3
"""megacube-admin MCP server 冒烟测试（纯 stdlib）。

起真 server 子进程走 stdio newline-delimited JSON-RPC；外部依赖全部用本地 fixture：
  - threading http.server 扮模型商店 + HF（env 覆盖 PINVOU_STORE_URL / PINVOU_HF_ENDPOINT）；
    HF 的 /api 先回 308 再落地，验证 server 的手工重定向跟随。
  - AF_UNIX socket 服务器（线程）扮 cmsd：/v1/models、POST install（记请求、回 202）、
    /v1/tasks/<id>（带 progress 字段）、POST cancel。CMSD_SOCKET 指过去。
  - MEGACUBE_CARDS_DIR 指向临时目录，PINVOU3_HOME 隔离到临时目录。

跑法：python3 tests/megacube_mcp_smoke.py（exit 0 = 全过，1 = 有失败）。
"""
import hashlib
import json
import os
import re
import select
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER = os.path.join(
    ROOT, "src-tauri", "resources", "common", "bundle", "mcp-servers",
    "megacube_admin_server.py")
# 母本卡（真机在役的 sglang 容器卡）：B1 快照 diff 的比对基准
MOTHER_CARD_DIR = os.path.join(
    ROOT, "src-tauri", "src", "features", "model_store", "data", "test-store",
    "qwen38-27b-sglang0519")

GOOD_REPO = "pinvou/Test1B-NVFP4-Tachyon"
NO_MANIFEST_REPO = "pinvou/Plain-BF16"       # 存在但没有 artifact-manifest → 落通用判定
MISSING_REPO = "pinvou/Does-Not-Exist"       # api/models 直接 404
STORE_CARD_ID = "testcard-1b-nvfp4-tachyon"
TAMPERED_CARD_ID = "tampered-card"
SGLANG_CARD_ID = "sglang-card"                    # 容器对话卡：W7 起放行（物化+下发）
DIFFUSERS_CARD_ID = "diffusers-card"              # 图像类：不是对话模型，必须拒装

ARTIFACT = "test1b_nvfp4_policy.tachyon"
ARTIFACT_BYTES = 12345678901
ARTIFACT_SHA256 = "a" * 64
ENGINE_PKG = "tachyon-serve-gb10-a1b2c3d4.tar.gz"
ENGINE_PKG_SHA256 = "b" * 64

# ---------------------------------------------------------------------------
# 通用对话仓库（第二形态）fixtures。口径对齐 D1 实测：hf-mirror 的
# /api/models 只有带 ?blobs=true 才回 siblings[].lfs（键名 sha256/size，没有
# oid）；GEN_OK_REPO 用母本仓库 unsloth/Qwen3.8-27B-NVFP4 的真实元数据（哈希/
# 体积与 test-store 母本卡逐字节一致，B1 快照 diff 靠它成立）。
# ---------------------------------------------------------------------------
GEN_ARCH = "Qwen3_5ForConditionalGeneration"
GEN_QUANT = {"quant_method": "compressed-tensors", "format": "mixed-precision",
             "config_groups": {"g0": {"format": "float-quantized"},
                               "g1": {"format": "nvfp4-pack-quantized"}}}
GEN_LFS_OK = [
    ("model.safetensors", 22568192096,
     "c473512c70eace07e2256fe9fd76596ac03e3295bee7d54cfb72676416afcc05"),
    ("model_mtp.safetensors", 849400392,
     "1d8268aa85ace093a561e3e7b63b9d390dac1cd55a90cd55b5ec509c3c9da9fe"),
    ("tokenizer.json", 19989325,
     "06b9509352d2af50381ab2247e083b80d32d5c0aba91c272ca9ff729b6a0e523"),
]
GEN_PLAIN_OK = [
    "README.md", ".gitattributes", "chat_template.jinja", "config.json",
    "generation_config.json", "model.safetensors.index.json",
    "preprocessor_config.json", "tokenizer_config.json",
    "video_preprocessor_config.json", "vocab.json",
]
GEN_CONFIG_OK = {"architectures": [GEN_ARCH], "model_type": "qwen3_5",
                 "quantization_config": GEN_QUANT}
GEN_TOTAL_BYTES = sum(size for _, size, _ in GEN_LFS_OK)  # 23437581813

GEN_OK_REPO = "unsloth/Qwen3.8-27B-NVFP4"            # A1/B1/C：母本仓库真实口径
GEN_NO_MTP_REPO = "unsloth/Qwen3.8-27B-NVFP4-Flat"   # A2/B2：无 MTP 文件
GEN_LLAMA_REPO = "meta-llama/Llama-4-8B"             # A3：架构族不放行
GEN_GGUF_REPO = "someone/Test-8B-GGUF"               # A4：GGUF 形态（无 safetensors）
GEN_FP8_REPO = "someone/Test-8B-FP8"                  # A5：量化非 NVFP4
GEN_GATED_REPO = "biglab/Gated-Test-8B"              # A6：gated 仓库
GEN_HF500_REPO = "someone/Server-Boom"               # A7：平台 5xx
GEN_HUGE_REPO = "someone/671B-NVFP4"                 # A8：超过单机 100GiB 上限
GEN_DRAFT_REPO = "someone/Test-8B-Draft"             # A9：独立 draft 仓库引用
GEN_BADNAME_REPO = "someone/Bad-File-Name"           # B4：LFS 文件名带空格


def _gen_api(lfs, plain, gated=False, private=False):
    siblings = [{"rfilename": n} for n in plain]
    siblings += [{"rfilename": n, "blobId": "f" * 40, "size": size,
                  "lfs": {"sha256": sha, "size": size, "pointerSize": 136}}
                 for n, size, sha in lfs]
    return {"siblings": siblings, "gated": gated, "private": private,
            "cardData": {"license": "apache-2.0"}}


GENERIC_HF = {
    GEN_OK_REPO: {"api": _gen_api(GEN_LFS_OK, GEN_PLAIN_OK), "config": GEN_CONFIG_OK},
    # 无 MTP：去掉 model_mtp.safetensors
    GEN_NO_MTP_REPO: {"api": _gen_api(GEN_LFS_OK[:1] + GEN_LFS_OK[2:], GEN_PLAIN_OK),
                      "config": GEN_CONFIG_OK},
    GEN_LLAMA_REPO: {"api": _gen_api(GEN_LFS_OK, GEN_PLAIN_OK),
                     "config": {"architectures": ["LlamaForCausalLM"],
                                "quantization_config": GEN_QUANT}},
    GEN_GGUF_REPO: {"api": _gen_api(
        [("model.gguf", 16000000000, "d" * 64)], ["config.json", "tokenizer.json"]),
        "config": {"architectures": [GEN_ARCH], "quantization_config": GEN_QUANT}},
    GEN_FP8_REPO: {"api": _gen_api(GEN_LFS_OK, GEN_PLAIN_OK),
                   "config": {"architectures": [GEN_ARCH],
                              "quantization_config": {"quant_method": "fp8"}}},
    GEN_GATED_REPO: {"api": _gen_api(GEN_LFS_OK, GEN_PLAIN_OK, gated="auto"),
                     "config": GEN_CONFIG_OK},
    GEN_HUGE_REPO: {"api": _gen_api(
        [("model-0000%d-of-00002.safetensors" % i, 60 * 1024 ** 3, "e" * 64)
         for i in (1, 2)], ["config.json", "tokenizer.json"]),
        "config": GEN_CONFIG_OK},
    GEN_DRAFT_REPO: {"api": _gen_api(GEN_LFS_OK, GEN_PLAIN_OK),
                     "config": {"architectures": [GEN_ARCH],
                                "quantization_config": GEN_QUANT,
                                "draft_model": "someone/eagle-head"}},
    GEN_BADNAME_REPO: {"api": _gen_api(
        [("bad name.safetensors", 1000, "f" * 64), ("model.safetensors", 1000, "a" * 64)],
        ["config.json", "tokenizer.json"]),
        "config": GEN_CONFIG_OK},
}

MANIFEST = {
    "artifact": {
        "filename": ARTIFACT, "bytes": ARTIFACT_BYTES, "sha256": ARTIFACT_SHA256,
        "architecture": "TestArchForCausalLM", "name": "Test1B-NVFP4",
        "components": ["mtp", "vision"],
    },
    "source": {"base": {"license": "apache-2.0"}},
    "runtime": {"minimum_revision": "r1", "dflash2_draft_tokens": {"recommended": 7}},
}
SHA256SUMS = "%s  %s\n%s  %s\n" % (
    ARTIFACT_SHA256, ARTIFACT, ENGINE_PKG_SHA256, ENGINE_PKG)

# 商店假卡：脚本内容是 stub shell，详情带正确 sha256；tampered 卡故意报错哈希
CARD_SCRIPTS = {
    "deploy": "#!/bin/bash\necho stub deploy\n",
    "download_model": "#!/bin/bash\necho stub download\n",
    "enable": "#!/bin/bash\necho stub enable\n",
    "install_engine": "#!/bin/bash\necho stub install_engine\n",
    "stop": "#!/bin/bash\necho stub stop\n",
    "uninstall": "#!/bin/bash\necho stub uninstall\n",
}
CARD_ASSETS = {
    "common.sh": "#!/bin/bash\necho stub common\n",
    "weights.sha256": "%s  %s\n" % (ARTIFACT_SHA256, ARTIFACT),
}

FAILURES = []


def check(label, cond, detail=""):
    if cond:
        print("PASS %s" % label)
    else:
        print("FAIL %s  %s" % (label, detail))
        FAILURES.append(label)


# ---------------------------------------------------------------------------
# fixture：商店 + HF（同一个 http.server）
# ---------------------------------------------------------------------------
class FixtureState:
    def __init__(self):
        self.by_hash = {}  # sha256 -> bytes，商店脚本/资产走 by-hash URL


def _register(state, content):
    raw = content.encode("utf-8")
    sha = hashlib.sha256(raw).hexdigest()
    state.by_hash[sha] = raw
    return sha


def _store_detail(state, base, card_id, tamper=False, kind="house"):
    scripts, script_sha = {}, {}
    for name, content in sorted(CARD_SCRIPTS.items()):
        sha = _register(state, content)
        scripts[name] = "%s/store/by-hash/%s" % (base, sha)
        # tamper：声明的哈希与真实内容对不上，server 必须拒装
        script_sha[name] = ("0" * 64) if tamper and name == "deploy" else sha
    if kind == "docker":
        # compose 型容器对话卡（真机 mtp 卡形状）：无 assets、engine_is_house=False、
        # 六个脚本带真哈希——W7 起 MCP 放行（cmsd 已纳管容器引擎）
        return {
            "id": card_id, "name": "测试 sglang 卡", "description": "fixture 容器对话卡",
            "size_bytes": ARTIFACT_BYTES, "quantization": "NVFP4",
            "engine_name": "sglang", "engine_version": "v0.5.16-cu130",
            "engine_is_house": False,
            "script_sha256": script_sha, "asset_sha256": {},
            "scripts": scripts, "assets": {},
        }
    if kind == "image":
        # 图像类引擎（diffusers）：不是对话模型，server 必须拒装并引导商店界面
        return {
            "id": card_id, "name": "测试图像卡", "description": "fixture 文生图卡",
            "size_bytes": ARTIFACT_BYTES, "quantization": "NVFP4",
            "engine_name": "diffusers", "engine_version": "0.40.0",
            "engine_is_house": False,
            "script_sha256": {}, "asset_sha256": {}, "scripts": {}, "assets": {},
        }
    assets, asset_sha = {}, {}
    for fname, content in sorted(CARD_ASSETS.items()):
        sha = _register(state, content)
        assets[fname] = "%s/store/by-hash/%s" % (base, sha)
        asset_sha[fname] = sha
    return {
        "id": card_id, "name": "测试卡 1B", "description": "fixture 假卡",
        "size_bytes": ARTIFACT_BYTES, "quantization": "NVFP4",
        "engine_name": "驭光（H3C自研推理引擎）", "engine_version": "gb10-a1b2c3d4",
        "engine_is_house": True,
        "script_sha256": script_sha, "asset_sha256": asset_sha,
        "scripts": scripts, "assets": assets,
    }


class FixtureHandler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _send(self, code, body=b"", ctype="application/json", extra=None):
        if isinstance(body, (dict, list)):
            body = json.dumps(body, ensure_ascii=False)
        if isinstance(body, str):
            body = body.encode("utf-8")
        self.send_response(code)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        state = self.server.state
        base = "http://127.0.0.1:%d" % self.server.server_port
        path, _, query = self.path.partition("?")
        if path == "/store/api/v1/cards":
            self._send(200, json.dumps([{
                "id": STORE_CARD_ID, "name": "测试卡 1B",
                "description": "fixture 假卡", "size_bytes": ARTIFACT_BYTES,
                "quantization": "NVFP4", "engine_name": "tachyon-serve",
                "engine_version": "gb10-a1b2c3d4",
            }]))
            return
        m = re.match(r"^/store/api/v1/cards/([A-Za-z0-9._-]+)$", path)
        if m:
            cid = m.group(1)
            if cid in (STORE_CARD_ID, TAMPERED_CARD_ID):
                self._send(200, json.dumps(_store_detail(
                    state, base, cid, tamper=(cid == TAMPERED_CARD_ID))))
            elif cid == SGLANG_CARD_ID:
                self._send(200, json.dumps(_store_detail(state, base, cid, kind="docker")))
            elif cid == DIFFUSERS_CARD_ID:
                self._send(200, json.dumps(_store_detail(state, base, cid, kind="image")))
            else:
                self._send(404, {"error": "no such card"})
            return
        m = re.match(r"^/store/by-hash/([0-9a-f]{64})$", path)
        if m and m.group(1) in state.by_hash:
            self._send(200, state.by_hash[m.group(1)], ctype="application/octet-stream")
            return
        # HF：/api 先 308 跳 /hf2（仿 hf-mirror 跳 huggingface.co），验证重定向跟随；
        # query（?blobs=true）原样带过去——不带它平台就不回 LFS 摘要（D1 实测）
        m = re.match(r"^/hf/api/models/(.+)$", path)
        if m:
            self._send(308, extra={"Location": "/hf2/api/models/%s%s" % (
                m.group(1), ("?" + query) if query else "")})
            return
        m = re.match(r"^/hf2/api/models/(.+)$", path)
        if m:
            repo = m.group(1)
            if repo == MISSING_REPO:
                self._send(404, {"error": "not found"})
            elif repo == GEN_HF500_REPO:
                self._send(500, {"error": "boom"})
            elif repo in GENERIC_HF:
                self._send(200, json.dumps(GENERIC_HF[repo]["api"]))
            else:
                self._send(200, {"id": repo})
            return
        # HF：raw config.json（通用形态判族与量化用）
        m = re.match(r"^/hf/(.+)/raw/main/config\.json$", path)
        if m:
            spec = GENERIC_HF.get(m.group(1))
            if spec and "config" in spec:
                self._send(200, json.dumps(spec["config"]))
            else:
                self._send(404, "not found", ctype="text/plain")
            return
        m = re.match(r"^/hf/(.+)/resolve/main/artifact-manifest\.json$", path)
        if m:
            if m.group(1) in (GOOD_REPO, "thirdparty/Unofficial-Tachyon"):
                self._send(200, json.dumps(MANIFEST))
            else:
                self._send(404, "not found", ctype="text/plain")
            return
        m = re.match(r"^/hf/(.+)/resolve/main/SHA256SUMS$", path)
        if m:
            if m.group(1) in (GOOD_REPO, "thirdparty/Unofficial-Tachyon"):
                self._send(200, SHA256SUMS, ctype="text/plain")
            else:
                self._send(404, "not found", ctype="text/plain")
            return
        self._send(404, {"error": "no route", "path": path})


# ---------------------------------------------------------------------------
# fixture：cmsd（AF_UNIX 上的极简 HTTP/1.1）
# ---------------------------------------------------------------------------
class FakeCmsd(threading.Thread):
    def __init__(self, sock_path):
        super().__init__(daemon=True)
        self.sock_path = sock_path
        self.models = []
        self.store_bindings = {}
        self.capacity = None
        self.capacity_requests = []
        self.active_model = None
        self.installs = []   # 记录收到的 install 卡片名
        self.model_actions = []
        self.cancels = []    # 记录收到的 cancel 任务 id
        self.tasks = {}
        self._seq = 0
        self._stopped = False

    def run(self):
        if os.path.exists(self.sock_path):
            os.unlink(self.sock_path)
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(self.sock_path)
        srv.listen(8)
        srv.settimeout(0.2)
        while not self._stopped:
            try:
                conn, _ = srv.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with conn:
                self._handle(conn)
        srv.close()

    def stop(self):
        self._stopped = True

    def _handle(self, conn):
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = conn.recv(65536)
            if not chunk:
                return
            data += chunk
        head, _, body = data.partition(b"\r\n\r\n")
        m = re.search(r"content-length: (\d+)", head.decode("latin-1"), re.I)
        need = int(m.group(1)) if m else 0
        while len(body) < need:
            chunk = conn.recv(65536)
            if not chunk:
                break
            body += chunk
        parts = head.split(b"\r\n")[0].decode("latin-1").split(" ")
        method, path = parts[0], parts[1]
        status, payload = self._route(method, path)
        raw = json.dumps(payload).encode("utf-8")
        conn.sendall(
            b"HTTP/1.1 %d X\r\ncontent-type: application/json\r\n"
            b"content-length: %d\r\nconnection: close\r\n\r\n%s"
            % (status, len(raw), raw))

    def _route(self, method, path):
        if method == "POST" and re.fullmatch(r"/v1/models/testcard-1b-nvfp4-tachyon/(enable|disable|set-default)", path):
            self.model_actions.append(path)
            return 202, {"task_id": "task-model-action", "status": "running"}
        if method == "GET" and path == "/v1/status":
            return 200, {"service": "cmsd", "version": "0.0-test", "uptime_s": 42,
                         "active_model": self.active_model, "degraded": [],
                         "models": [dict({"managed": False}, **row) for row in self.models]}
        if method == "GET" and path == "/v1/models":
            return 200, {"models": [dict({"managed": False}, **row) for row in self.models],
                         "active_model": self.active_model, "store_bindings": self.store_bindings,
                         "selection_policy": "latest-installed-v1"}
        if method == "GET" and path.startswith("/v1/capacity"):
            self.capacity_requests.append(path)
            return 200, self.capacity or {"estimate": {}, "memory": {}}
        m = re.match(r"^/v1/models/[^/]+/install$", path)
        if method == "POST" and m:
            card = path.split("/")[3]
            self.installs.append(card)
            self._seq += 1
            tid = "task-install-%d" % self._seq
            self.tasks[tid] = {
                "id": tid, "kind": "install", "card": card, "status": "running",
                "created_at": 1791891041,
                "last_event": {"message": "下载权重中"},
                "result": None, "error": None,
                "progress_done_bytes": 11811160064,
                "progress_total_bytes": 21944459776,
            }
            return 202, {"task_id": tid, "kind": "install", "card": card,
                         "status": "running"}
        if method == "GET" and re.match(r"^/v1/tasks/[^/]+$", path):
            tid = path.split("/")[3]
            if tid in self.tasks:
                return 200, self.tasks[tid]
            return 404, {"error": {"code": "not_found", "message": "任务不存在"}}
        if method == "GET" and path == "/v1/tasks":
            return 200, {"tasks": list(self.tasks.values())}
        if method == "POST" and re.match(r"^/v1/tasks/[^/]+/cancel$", path):
            tid = path.split("/")[3]
            self.cancels.append(tid)
            if tid in self.tasks:
                self.tasks[tid]["status"] = "cancelled"
                return 202, {"task_id": tid, "kind": self.tasks[tid]["kind"],
                             "card": self.tasks[tid]["card"], "status": "cancelled"}
            return 404, {"error": {"code": "not_found", "message": "任务不存在"}}
        return 404, {"error": {"code": "not_found", "message": "no route %s" % path}}


# ---------------------------------------------------------------------------
# MCP stdio 客户端
# ---------------------------------------------------------------------------
class McpClient:
    def __init__(self, env):
        self.proc = subprocess.Popen(
            [sys.executable, SERVER],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, encoding="utf-8", bufsize=1, env=env)
        self._id = 0

    def call(self, method, params=None, timeout=30):
        self._id += 1
        rid = self._id
        self.proc.stdin.write(json.dumps(
            {"jsonrpc": "2.0", "id": rid, "method": method,
             "params": params or {}}, ensure_ascii=False) + "\n")
        self.proc.stdin.flush()
        deadline = time.time() + timeout
        while time.time() < deadline:
            ready, _, _ = select.select(
                [self.proc.stdout], [], [], max(0.05, deadline - time.time()))
            if not ready:
                break
            line = self.proc.stdout.readline()
            if not line:
                raise RuntimeError("server 进程退出了。stderr：%s" % self.stderr_tail())
            msg = json.loads(line)
            if msg.get("id") == rid:
                return msg
        raise TimeoutError("等 %s 响应超时" % method)

    def tool(self, name, args=None):
        msg = self.call("tools/call", {"name": name, "arguments": args or {}})
        if "error" in msg:
            raise RuntimeError("tools/call %s 协议层报错：%s" % (name, msg["error"]))
        return json.loads(msg["result"]["content"][0]["text"])

    def stderr_tail(self):
        try:
            ready, _, _ = select.select([self.proc.stderr], [], [], 0.1)
            return self.proc.stderr.read() if ready else ""
        except Exception:
            return ""

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.terminate()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


def make_env(tmp, cmsd, http_port):
    env = dict(os.environ)
    env["PINVOU3_HOME"] = os.path.join(tmp, "home")
    env["MEGACUBE_CARDS_DIR"] = os.path.join(tmp, "cards")
    env["CMSD_SOCKET"] = cmsd.sock_path
    env["PINVOU_STORE_URL"] = "http://127.0.0.1:%d/store" % http_port
    env["PINVOU_HF_ENDPOINT"] = "http://127.0.0.1:%d/hf" % http_port
    return env


# ---------------------------------------------------------------------------
# B 组（进程内纯函数单测）：探测 → 渲染 → 与母本卡逐行 diff + 防注入。
# 直接 import server 模块（探测走 fixture http server，与 stdio 子进程同一份数据）。
# ---------------------------------------------------------------------------
_B1_ALLOWED = [
    re.compile(r"^[+-]\s*#"),                            # 头注/说明注释差异
    re.compile(r"^[+-]\s*$"),                            # 空行
    re.compile(r"^[+-]CONTAINER_NAME="),
    re.compile(r"^[+-](export )?MODEL_DIR="),
    re.compile(r"^[+-]TOTAL="),
    re.compile(r"^[+-]\s+\"[A-Za-z0-9_.]+:\d+\""),        # LMM_BIG_FILES 数组项
    re.compile(r"^[+-]\s+\[[A-Za-z0-9_.]+\]=\d+"),        # vendor MODEL_FILES 项
    re.compile(r"^[+-]\s*[A-Za-z0-9_. ]*\.(json|jinja|gitattributes)[A-Za-z0-9_. ]*$"),
    re.compile(r"^[+-].*PORT"),                           # 动态端口块（含注释）
    re.compile(r"^[+-].*(_deployed_port|_candidate|ss -tln|seq 30001)"),
    re.compile(r"^[+-]\s*(else|fi|done|break|then)$"),
    re.compile(r"^[+-].*SPEC_ARGS"),
    re.compile(r"^[+-]\s+--(speculative-algorithm|speculative-adaptive|enable-linear-replayssm-spec)"),
    re.compile(r"^[+-].*find \"\$dir\".*-name"),          # 哨兵文件名参数化
    re.compile(r"^[+-]\s*echo \"(校验权重摘要|开始下载)"),
    re.compile(r"^[+-]\[ -f \"\$MODEL_DIR/"),             # start.sh 前置条件（分片仓库口径）
    re.compile(r"^[+-]\s*log \"downloading"),
    re.compile(r"^[+-]\s*if confirm \"Remove model weights"),
    re.compile(r"^[+-].*(Qwen3\.8-27B-NVFP4|qwen38-sglang|sglang-qwen38|unsloth)"),
]


def pure_render_checks(http_port):
    """B 组：渲染与防注入（纯函数）。probe 真走 fixture，渲染结果与母本卡 diff。"""
    import importlib.util
    saved = {k: os.environ.get(k) for k in
             ("PINVOU_HF_ENDPOINT", "MEGACUBE_CARDS_DIR", "PINVOU3_HOME")}
    cards_dir = tempfile.mkdtemp(prefix="megacube-b-test-")
    os.environ["PINVOU_HF_ENDPOINT"] = "http://127.0.0.1:%d/hf" % http_port
    os.environ["MEGACUBE_CARDS_DIR"] = cards_dir
    os.environ.pop("PINVOU3_HOME", None)
    try:
        spec = importlib.util.spec_from_file_location("megacube_admin_under_test", SERVER)
        m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(m)

        probe = m._probe_repo(GEN_OK_REPO)
        check("B1 probe 通用形态 ok + 体积=LFS 和",
              probe.get("ok") is True and probe.get("form") == "generic"
              and probe["weights_bytes"] == GEN_TOTAL_BYTES,
              json.dumps({k: probe.get(k) for k in ("ok", "form", "weights_bytes", "mtp")},
                         ensure_ascii=False))
        files = m._render_sglang_card_files(probe, "qwen38-27b-nvfp4")
        bad_lines = []
        for name in m._SGLANG_CARD_TEMPLATES:
            # The independent downloader now consumes host import candidates
            # and preserves source files. Exercise it in the workflow suite;
            # runtime/vendor parameters must still match the calibrated card.
            if name == "download_model.sh":
                continue
            generated = files[name].decode()
            if name in ("vendor/start.sh", "vendor/stop.sh", "vendor/uninstall.sh"):
                # Only this declared data-reader repair differs from the
                # calibrated runtime. Its actual behavior has a separate CI
                # lifecycle suite; all other vendor parameters stay compared.
                reader = m._DEPLOY_ENV_READ_SH + '\nlmm_read_deploy_env "$SCRIPT_DIR/.deploy-env"'
                check("B1 " + name + " 使用唯一字面部署读取器", generated.count(reader) == 1, "")
                generated = generated.replace(reader,
                    '[ -f "$SCRIPT_DIR/.deploy-env" ] && . "$SCRIPT_DIR/.deploy-env"', 1)
            gen = generated.splitlines()
            # 母本源目录里 vendor 脚本放在 package/ 下（商店物化时才改名 vendor/）
            mother_name = name.replace("vendor/", "package/", 1)
            with open(os.path.join(MOTHER_CARD_DIR, mother_name), encoding="utf-8") as f:
                mother = f.read().splitlines()
            import difflib
            for line in difflib.unified_diff(mother, gen, lineterm="", n=0):
                if line[:1] not in "+-" or line[:3] in ("---", "+++"):
                    continue
                if not any(p.match(line) for p in _B1_ALLOWED):
                    bad_lines.append("%s: %s" % (name, line))
        check("B1 除已验证下载和字面读取外，运行参数与母本一致", not bad_lines,
              "\n".join(bad_lines[:12]))

        # B2：MTP 两档快照——EAGLE 参数组整组出现/消失
        probe_none = m._probe_repo(GEN_NO_MTP_REPO)
        check("B2 无 MTP 仓库探测 mtp=none", probe_none.get("mtp") == "none", "")
        files_eagle = files["vendor/start.sh"].decode()
        files_none = m._render_sglang_card_files(
            probe_none, "qwen38-27b-nvfp4-flat")["vendor/start.sh"].decode()
        check("B2 eagle 档含 EAGLE 参数组、none 档整组消失",
              "--speculative-algorithm EAGLE" in files_eagle
              and "--enable-linear-replayssm-spec" in files_eagle
              and "SPEC_ARGS=(--speculative-algorithm" in files_eagle
              and "--speculative-algorithm" not in files_none
              and "--speculative-adaptive" not in files_none
              and "--enable-linear-replayssm-spec" not in files_none
              and "SPEC_ARGS=()" in files_none, "")

        # B3（渲染层）：恶意 repo_id / 恶意值在模板校验处被拒（绕过 _valid_repo 前置）
        evil = dict(probe, repo_id="evil/x;rm -rf /")
        try:
            m._render_sglang_card_files(evil, "evil")
            check("B3 渲染层拒绝注入型 repo_id", False, "没有抛 _CardError")
        except m._CardError as e:
            check("B3 渲染层拒绝注入型 repo_id", "非法字符" in str(e), str(e))

        # B5：模板漏替换占位符 → _CardError
        saved_tpl = m._SGLANG_CARD_TEMPLATES["common.sh"]
        m._SGLANG_CARD_TEMPLATES["common.sh"] = saved_tpl + "\n# __LEFTOVER__\n"
        try:
            m._render_sglang_card_files(probe, "leftover")
            check("B5 漏替换占位符抛 _CardError", False, "没有抛")
        except m._CardError as e:
            check("B5 漏替换占位符抛 _CardError", "未替换的占位符" in str(e), str(e))
        finally:
            m._SGLANG_CARD_TEMPLATES["common.sh"] = saved_tpl
    finally:
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        shutil.rmtree(cards_dir, ignore_errors=True)


def main():
    tmp = tempfile.mkdtemp(prefix="megacube-mcp-test-")
    state = FixtureState()
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler)
    httpd.state = state
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    cmsd = FakeCmsd(os.path.join(tmp, "cmsd.sock"))
    cmsd.start()
    client = None
    try:
        pure_render_checks(httpd.server_port)
        client = McpClient(make_env(tmp, cmsd, httpd.server_port))

        init = client.call("initialize")
        check("initialize", init["result"]["serverInfo"]["name"] == "megacube-admin")

        tools = client.call("tools/list")["result"]["tools"]
        names = sorted(t["name"] for t in tools)
        check("tools/list 无重复工具名", len(names) == len(set(names)), names)
        for want in ("cube_list_store_models", "cube_check_model_repo",
                     "cube_install_model", "cube_cancel_task"):
            check("tools/list 含 %s" % want, want in names)

        # --- cube_list_store_models：商店 ∪ 本地（先空后有） ---
        r = client.tool("cube_list_store_models")
        entry = next((e for e in r["data"]["models"] if e["card_id"] == STORE_CARD_ID), None)
        check("list_store_models 含商店卡", entry is not None, r["summary"])
        check("list_store_models 本地状态 none（cmsd 不认识）",
              entry and entry["local_state"] == "none", entry)
        check("list_store_models size_gib 换算",
              entry and entry["size_gib"] == round(ARTIFACT_BYTES / 1024 ** 3, 1), entry)
        cmsd.models = [{"card": STORE_CARD_ID, "status": "running", "healthy": True,
                        "port": 18080, "weights": {"complete": True}}]
        cmsd.active_model = STORE_CARD_ID
        r = client.tool("cube_list_store_models")
        entry = next(e for e in r["data"]["models"] if e["card_id"] == STORE_CARD_ID)
        check("list_store_models 合并本地状态 active",
              entry["local_state"] == "active", entry)

        runtime = STORE_CARD_ID + "--v2"
        cmsd.models = [{"card": runtime, "status": "running", "healthy": True,
                        "managed": True, "weights_complete": True}]
        cmsd.active_model = runtime
        cmsd.store_bindings = {STORE_CARD_ID: runtime}
        r = client.tool("cube_list_store_models")
        entry = next(e for e in r["data"]["models"] if e["card_id"] == STORE_CARD_ID)
        check("商店逻辑卡合并当前runtime且不重复列出",
              entry["local_state"] == "active" and entry["runtime_card"] == runtime
              and not any(e["card_id"] == runtime for e in r["data"]["models"]), r)
        r = client.tool("cube_plan_switch", {"card": STORE_CARD_ID})
        check("换胎预览按当前runtime比较默认指针并请求准确容量", (r.get("data") or {}).get("already_active") is True
              and cmsd.capacity_requests[-1] == "/v1/capacity?card=" + runtime, r)
        r = client.tool("cube_plan_switch", {"card": STORE_CARD_ID + "--v1"})
        check("已归档runtime不作普通换胎候选", r["data"] is None and "不认识" in r["summary"], r)
        # A complete manual store rollback changes the authoritative mapping.
        cmsd.models[0]["card"] = STORE_CARD_ID + "--v1"
        cmsd.active_model = cmsd.models[0]["card"]
        cmsd.store_bindings = {STORE_CARD_ID: cmsd.active_model}
        r = client.tool("cube_list_store_models")
        entry = next(e for e in r["data"]["models"] if e["card_id"] == STORE_CARD_ID)
        check("商店手动完整回退后跟随重新发布的当前版本", entry["local_state"] == "active"
              and entry["runtime_card"] == cmsd.active_model, r)
        for invalid in (None, [], {"bad\n": cmsd.active_model}, {STORE_CARD_ID: "absent-runtime"},
                        {STORE_CARD_ID: cmsd.active_model, "duplicate-alias": cmsd.active_model}):
            cmsd.store_bindings = invalid
            r = client.tool("cube_list_store_models")
            entry = next(e for e in r["data"]["models"] if e["card_id"] == STORE_CARD_ID)
            check("库存关系未核实不能伪报未下载或重复安装", entry["local_state"] == "unknown"
                  and r["data"]["local_observation"] == "unconfirmed" and "不要" in r["next"], r)
            r = client.tool("cube_plan_switch", {"card": STORE_CARD_ID})
            check("未核实选择不能生成确定换胎预览", r["data"].get("error_code") == "MODEL_SELECTION_UNCONFIRMED", r)
        cmsd.store_bindings = {}
        cmsd.models = [{"card": STORE_CARD_ID, "status": "running", "healthy": True,
                        "port": 18080, "weights": {"complete": True}}]
        cmsd.active_model = STORE_CARD_ID

        # --- cube_check_model_repo：合规 / 不合规 / 不存在 ---
        r = client.tool("cube_check_model_repo", {"repo_id": GOOD_REPO})
        d = r["data"]
        check("check_model_repo 驭光仓库只指向官方商店适配卡",
              d.get("card_ready") is False and d.get("error_code") == "TACHYON_STORE_REQUIRED"
              and "H3C" in r["summary"] and "card_id" in r["next"], r)
        r = client.tool("cube_check_model_repo", {"repo_id": NO_MANIFEST_REPO})
        check("check_model_repo 不合规 card_ready=false + 中文说明",
              r["data"].get("card_ready") is False
              and "没有可代办安装的模型包" in r["summary"], r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": MISSING_REPO})
        check("check_model_repo 404 → 不存在",
              r["data"].get("card_ready") is False and "不存在" in r["summary"], r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": "../etc"})
        check("check_model_repo 非法 repo_id 拒绝",
              "非法" in r["summary"], r["summary"])

        # --- cube_install_model 商店路径 ---
        cards_dir = os.path.join(tmp, "cards")
        r = client.tool("cube_install_model", {"card_id": STORE_CARD_ID})
        check("install_model 商店路返回 task_id",
              bool((r.get("data") or {}).get("task_id"), ), r["summary"])
        check("install_model 商店路向 cmsd 发了 install",
              STORE_CARD_ID in cmsd.installs, str(cmsd.installs))
        card_dir = os.path.join(cards_dir, STORE_CARD_ID)
        expect_files = {"%s.sh" % n for n in CARD_SCRIPTS} | set(CARD_ASSETS) | {"card.json"}
        check("install_model 商店路物化全部文件",
              os.path.isdir(card_dir)
              and set(os.listdir(card_dir)) == expect_files,
              os.listdir(card_dir) if os.path.isdir(card_dir) else "目录不存在")
        ok_bytes = all(
            open(os.path.join(card_dir, fname), "rb").read() == content.encode()
            for fname, content in list(CARD_ASSETS.items())
            + [("%s.sh" % n, c) for n, c in CARD_SCRIPTS.items()])
        check("install_model 商店路内容逐字节一致（sha256 校验过）", ok_bytes)
        # A fully prepared local card need not exist in the remote catalog.
        local_id = "llm-prepared-local-card"
        shutil.copytree(card_dir, os.path.join(cards_dir, local_id))
        with open(os.path.join(cards_dir, local_id, "card.json"), "w", encoding="utf-8") as stream:
            json.dump({"engine_name": "sglang"}, stream)
        r = client.tool("cube_install_local_card", {"card_id": local_id})
        check("install_local_card 不查询商店目录即可下发本地卡",
              bool((r.get("data") or {}).get("task_id")) and local_id in cmsd.installs, r)
        r = client.tool("cube_install_local_card", {"card_id": "../outside"})
        check("install_local_card 路径穿越拒绝", r["data"].get("error_code") == "INVALID_CARD_ID", r)
        os.remove(os.path.join(cards_dir, local_id, "uninstall.sh"))
        r = client.tool("cube_install_local_card", {"card_id": local_id})
        check("install_local_card 缺脚本不执行", r["data"].get("error_code") == "INCOMPLETE_CARD", r)
        r = client.tool("cube_install_model", {"card_id": STORE_CARD_ID})
        check("install_model 商店路幂等重装仍下发任务",
              bool((r.get("data") or {}).get("task_id")), r["summary"])

        # --- 缺文件就地补齐（真机实测：品悟商店链路物化的卡缺 uninstall.sh，
        # 它只进商店数据库不落盘；补齐 ≠ 覆写现存内容） ---
        victim = os.path.join(card_dir, "uninstall.sh")
        os.remove(victim)
        r = client.tool("cube_install_model", {"card_id": STORE_CARD_ID})
        check("install_model 缺文件的卡就地补齐并下发任务",
              bool((r.get("data") or {}).get("task_id")) and os.path.isfile(victim)
              and open(victim, encoding="utf-8").read() == CARD_SCRIPTS["uninstall"],
              r["summary"])

        # --- cube_install_model 商店路径：sha256 篡改被拒 ---
        installs_pre_tamper = list(cmsd.installs)
        r = client.tool("cube_install_model", {"card_id": TAMPERED_CARD_ID})
        check("install_model 篡改卡被拒且说明 sha256",
              "拒绝安装" in r["summary"] and "sha256" in r["summary"], r["summary"])
        check("install_model 篡改卡未落盘未下发",
              not os.path.exists(os.path.join(cards_dir, TAMPERED_CARD_ID))
              and cmsd.installs == installs_pre_tamper,
              str(cmsd.installs))

        # --- cube_install_model 商店路径：compose 型容器对话卡（sglang）放行 ---
        r = client.tool("cube_install_model", {"card_id": SGLANG_CARD_ID})
        check("install_model sglang 容器对话卡放行并下发任务",
              bool((r.get("data") or {}).get("task_id")), r["summary"])
        sglang_dir = os.path.join(cards_dir, SGLANG_CARD_ID)
        check("install_model sglang 卡物化六脚本（无 common.sh 资产）",
              os.path.isdir(sglang_dir)
              and set(os.listdir(sglang_dir)) == {"%s.sh" % n for n in CARD_SCRIPTS} | {"card.json"},
              os.listdir(sglang_dir) if os.path.isdir(sglang_dir) else "目录不存在")
        check("install_model 物化的 card.json 带 size_bytes（进度/预算口径）",
              isinstance(json.load(open(os.path.join(sglang_dir, "card.json"), encoding="utf-8")).get("size_bytes"), int)
              and json.load(open(os.path.join(sglang_dir, "card.json"), encoding="utf-8"))["size_bytes"] == ARTIFACT_BYTES)

        # --- cube_install_model 商店路径：图像类引擎（非对话模型）拒装 ---
        installs_pre_image = list(cmsd.installs)
        r = client.tool("cube_install_model", {"card_id": DIFFUSERS_CARD_ID})
        check("install_model 图像类卡拒装并说明对话引擎口径",
              "不是管理面能代办" in r["summary"] and "模型商店" in r["summary"], r["summary"])
        check("install_model 图像类卡未下发任务",
              cmsd.installs == installs_pre_image, str(cmsd.installs))

        # --- cube_plan_switch：换胎预算口径（可用 + 当前模型将释放） ---
        # 场景钉自真机事故：73.4 GiB 模型在跑、可用 35.2、目标需要 94.9——
        # 并行加开口径必拒，换胎口径可行
        cmsd.models = [
            {"card": "old-model", "status": "running", "healthy": True,
             "measured_footprint_gib": 73.4, "port": 18080},
            {"card": "target-card", "status": "stopped", "port": 18081},
        ]
        cmsd.active_model = "old-model"
        cmsd.capacity = {
            "estimate": {"card": "target-card", "required_gib": 85.2,
                         "safety_margin_gib": 9.7, "available_gib": 35.2, "ok": False,
                         "verdict": "内存不够，还差 59.7 GiB：需要 静态预留 85.2 + 安全边际 9.7 = 94.9 GiB，当前可用 35.2 GiB"},
            "memory": {"total_gib": 121.7, "available_gib": 35.2},
            "resident_models": [],
        }
        r = client.tool("cube_plan_switch", {"card": "target-card"})
        plan = r.get("data") or {}
        check("plan_switch 换胎口径计入当前模型释放（73.4+35.2 ≥ 94.9 判可行）",
              plan.get("budget_ok") is True
              and "切换释放" in r["summary"] and "预算可行" in r["summary"]
              and "不建议执行" not in r["summary"],
              r["summary"])
        check("plan_switch 计划含释放量与有效可用字段",
              plan.get("budget_freed_by_switch_gib") == 73.4
              and plan.get("budget_effective_available_gib") == 108.6,
              json.dumps(plan, ensure_ascii=False)[:160])
        # 释放后仍不够（需要 130+9.7）→ 保持拒绝并给缺口
        cmsd.capacity["estimate"]["required_gib"] = 130.0
        r = client.tool("cube_plan_switch", {"card": "target-card"})
        check("plan_switch 释放后仍不足时保持拒绝",
              (r.get("data") or {}).get("budget_ok") is False
              and "不建议执行" in r["summary"] and "还差" in r["summary"],
              r["summary"])
        # 目标已在跑（非活跃）→ 引导 cube_set_default_model 而非换胎
        cmsd.models.append({"card": "running-card", "status": "running", "healthy": True, "port": 18082})
        r = client.tool("cube_plan_switch", {"card": "running-card"})
        check("plan_switch 目标已在跑引导仅挪默认",
              (r.get("data") or {}).get("already_running") is True
              and "cube_set_default_model" in (r.get("next") or ""),
              r["summary"])
        cmsd.capacity = None
        cmsd.models = []
        cmsd.active_model = None


        # Native engine packages are admitted only through official store cards.
        before = list(cmsd.installs)
        for repo, generated in ((GOOD_REPO, "test1b-nvfp4-tachyon"),
                                ("thirdparty/Unofficial-Tachyon", "unofficial-tachyon")):
            r = client.tool("cube_install_model", {"repo_id": repo})
            check("install_model 任意owner的驭光仓库均拒绝且不物化不下发",
                  (r.get("data") or {}).get("error_code") == "TACHYON_STORE_REQUIRED"
                  and cmsd.installs == before
                  and not os.path.exists(os.path.join(cards_dir, generated)), r)
        r = client.tool("cube_install_model", {"repo_id": NO_MANIFEST_REPO})
        check("install_model repo 不合规直拒",
              "没有可代办安装的模型包" in r["summary"], r["summary"])
        r = client.tool("cube_install_model",
                        {"card_id": STORE_CARD_ID, "repo_id": GOOD_REPO})
        check("install_model 两个参数都传被拒", "二选一" in r["summary"], r["summary"])
        r = client.tool("cube_install_model", {"card_id": "../evil"})
        check("install_model 路径穿越被拒", "非法" in r["summary"], r["summary"])

        # ===================================================================
        # A 组：通用仓库探测与准入（双形态第二跳）
        # ===================================================================
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_OK_REPO})
        d = r["data"]
        check("A1 合规通用仓库 card_ready + generic 形态",
              d.get("card_ready") is True and d.get("form") == "generic", r["summary"])
        check("A1 体积=ΣLFS / MTP=eagle / 第三方标记",
              d.get("weights_gib") == round(GEN_TOTAL_BYTES / 1024 ** 3, 1)
              and d.get("mtp") == "eagle" and d.get("third_party") is True
              and d.get("quantization") == "NVFP4"
              and d.get("architecture") == GEN_ARCH,
              json.dumps(d, ensure_ascii=False))
        check("A1 应答引导含「未经官方验证」知情话术",
              "未经官方验证" in (r.get("next") or ""), r.get("next"))
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_NO_MTP_REPO})
        check("A2 同族无 MTP 仓库 MTP=none",
              r["data"].get("mtp") == "none" and r["data"].get("card_ready") is True,
              r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_LLAMA_REPO})
        check("A3 Llama 标准模板受限但允许另评估安装路径",
              r["data"].get("card_ready") is False
              and r["data"].get("standard_install_supported") is False
              and "cube_inspect_source" in r["next"], r)
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_GGUF_REPO})
        check("A4 GGUF 形态拒绝",
              r["data"].get("card_ready") is False and "safetensors" in r["summary"],
              r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_FP8_REPO})
        check("A5 量化非 NVFP4 拒绝并建议商店同款",
              r["data"].get("card_ready") is False and "NVFP4" in r["summary"],
              r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_GATED_REPO})
        check("A6 gated 仓库拒绝（不公开）",
              r["data"].get("card_ready") is False and "不公开" in r["summary"],
              r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_HF500_REPO})
        check("A7 平台 5xx 与 404 三态区分",
              "HTTP 500" in r["summary"]
              and r["data"].get("card_ready") is False, r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_HUGE_REPO})
        check("A8 超 100GiB 单机上限拒绝并说明级联",
              r["data"].get("card_ready") is False
              and "级联" in r["summary"] and "上限" in r["summary"], r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": GEN_DRAFT_REPO})
        check("A9 独立 draft 双仓库形态拒绝",
              r["data"].get("card_ready") is False and "draft" in r["summary"],
              r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": "evil/x;rm -rf"})
        check("B3 注入型 repo_id 被白名单拒绝", "非法" in r["summary"], r["summary"])
        r = client.tool("cube_check_model_repo", {"repo_id": "evil/名字"})
        check("B3 unicode repo_id 被白名单拒绝", "非法" in r["summary"], r["summary"])

        # ===================================================================
        # C 组（MCP 侧）：通用渲染卡安装下发 + 物化布局
        # ===================================================================
        installs_pre_gen = list(cmsd.installs)
        r = client.tool("cube_install_model", {"repo_id": GEN_BADNAME_REPO})
        check("B4 恶意 LFS 文件名拒绝渲染且不落盘",
              "拒绝安装" in r["summary"] and "白名单" in r["summary"]
              and not os.path.exists(os.path.join(cards_dir, "bad-file-name"))
              and cmsd.installs == installs_pre_gen, r["summary"])

        r = client.tool("cube_install_model", {"repo_id": GEN_OK_REPO})
        gen_card_id = "qwen38-27b-nvfp4"
        check("C1 通用卡安装返回 task_id 并下发 install_local",
              bool((r.get("data") or {}).get("task_id"))
              and gen_card_id in cmsd.installs
              and (r.get("data") or {}).get("weights_gib") == round(
                  GEN_TOTAL_BYTES / 1024 ** 3, 1),
              r["summary"])
        gdir = os.path.join(cards_dir, gen_card_id)
        expect_layout = {
            "common.sh", "install_engine.sh", "download_model.sh", "deploy.sh",
            "enable.sh", "stop.sh", "uninstall.sh", "vendor", "weights.sha256",
            "card.json"}
        check("C1 物化布局：七脚本 + vendor/ + 清单 + card.json",
              os.path.isdir(gdir) and set(os.listdir(gdir)) == expect_layout
              and sorted(os.listdir(os.path.join(gdir, "vendor"))) == [
                  "deploy.sh", "start.sh", "stop.sh", "uninstall.sh"],
              os.listdir(gdir) if os.path.isdir(gdir) else "目录不存在")
        check("C1 vendor 脚本落子目录且可执行",
              os.access(os.path.join(gdir, "vendor", "start.sh"), os.X_OK)
              and os.access(os.path.join(gdir, "deploy.sh"), os.X_OK))
        meta = json.load(open(os.path.join(gdir, "card.json"), encoding="utf-8"))
        check("B6 card.json：engine_name=sglang + size_bytes=LFS 精确和 + third_party",
              meta.get("engine_name") == "sglang"
              and meta.get("size_bytes") == GEN_TOTAL_BYTES
              and meta.get("source_repo_id") == GEN_OK_REPO
              and meta.get("third_party") is True, json.dumps(meta))
        gen_ws = open(os.path.join(gdir, "weights.sha256"), encoding="utf-8").read()
        mother_ws = open(os.path.join(MOTHER_CARD_DIR, "weights.sha256"),
                         encoding="utf-8").read()
        check("B1 weights.sha256 与母本卡逐字节一致", gen_ws == mother_ws, gen_ws)
        gen_common = open(os.path.join(gdir, "common.sh"), encoding="utf-8").read()
        check("C1 common.sh 钉死镜像 tag + 来源标记行 + 动态端口",
              'IMAGE="lmsysorg/sglang:v0.5.19"' in gen_common
              and "本卡由 megacube-admin 按 %s" % GEN_OK_REPO in gen_common
              and 'CONTAINER_NAME="sglang-%s"' % gen_card_id in gen_common
              and 'export MODEL_DIR="${LMM_MODELS_DIR:-$HOME/models}/Qwen3.8-27B-NVFP4-unsloth"' in gen_common
              and 'seq 30001 30200' in gen_common
              and '"model.safetensors:22568192096"' in gen_common
              and '"model_mtp.safetensors:849400392"' in gen_common
              and '"tokenizer.json:19989325"' in gen_common,
              gen_common[:200])
        r = client.tool("cube_install_model", {"repo_id": GEN_OK_REPO})
        check("B7 幂等重装（内容一致跳过）仍下发任务",
              bool((r.get("data") or {}).get("task_id")), r["summary"])
        victim = os.path.join(gdir, "vendor", "start.sh")
        os.remove(victim)
        r = client.tool("cube_install_model", {"repo_id": GEN_OK_REPO})
        check("B7 缺文件就地补齐（子目录 vendor/）",
              os.path.isfile(victim) and bool((r.get("data") or {}).get("task_id")),
              r["summary"])
        with open(os.path.join(gdir, "common.sh"), "a", encoding="utf-8") as f:
            f.write("# tampered\n")
        installs_pre_tamper = list(cmsd.installs)
        r = client.tool("cube_install_model", {"repo_id": GEN_OK_REPO})
        check("B7 内容不一致拒覆写",
              "不一致" in r["summary"] and "拒绝安装" in r["summary"]
              and cmsd.installs == installs_pre_tamper, r["summary"])
        # 恢复现场：删掉被污染的卡目录，后续用例不受影响
        shutil.rmtree(gdir, ignore_errors=True)

        # A7 网络断：HF 端点指向关门端口 → 稍后再试口径
        dead_hf = socket.socket()
        dead_hf.bind(("127.0.0.1", 0))
        dead_hf_port = dead_hf.getsockname()[1]
        dead_hf.close()
        env3 = make_env(tmp, cmsd, httpd.server_port)
        env3["PINVOU_HF_ENDPOINT"] = "http://127.0.0.1:%d/hf" % dead_hf_port
        client3 = McpClient(env3)
        try:
            client3.call("initialize")
            r = client3.tool("cube_check_model_repo", {"repo_id": GEN_OK_REPO})
            check("A7 网络断三态：暂时连不上",
                  "暂时连不上" in r["summary"]
                  and r["data"].get("reachable") is False, r["summary"])
        finally:
            client3.close()

        # Real stdio -> Unix HTTP path regression: do not accept literal %s.
        for tool, action in (("cube_start_model", "enable"), ("cube_stop_model", "disable"),
                             ("cube_set_default_model", "set-default")):
            r = client.tool(tool, {"card": STORE_CARD_ID})
            check(tool + " sends the exact card path",
                  (r.get("data") or {}).get("task_id") == "task-model-action"
                  and cmsd.model_actions[-1:] == ["/v1/models/%s/%s" % (STORE_CARD_ID, action)], r)
            before = list(cmsd.model_actions)
            r = client.tool(tool, {"card": "../wrong?card"})
            check(tool + " refuses path input", r.get("data") is None and cmsd.model_actions == before, r)

        # --- cube_get_task 进度 + cancelled；cube_cancel_task 透传 ---
        tid = "task-install-1"
        r = client.tool("cube_get_task", {"task_id": tid})
        check("get_task 进度格式化",
              "下载中 11.0/20.4 GiB（54%）" in r["summary"], r["summary"])
        r = client.tool("cube_cancel_task", {"task_id": tid})
        check("cancel_task 透传 cmsd 且应答已取消",
              tid in cmsd.cancels and "已取消" in r["summary"], r["summary"])
        r = client.tool("cube_get_task", {"task_id": tid})
        check("get_task cancelled 翻成已取消", "已取消" in r["summary"], r["summary"])

        # --- cube_get_task 不带 id：列最近任务（失败/取消在前），排障入口 ---
        cmsd.tasks["task-old-fail"] = {
            "id": "task-old-fail", "kind": "install", "card": "fail-card",
            "status": "failed", "created_at": 1791900000,
            "error": "磁盘空间不足", "result": None, "events": [],
        }
        r = client.tool("cube_get_task", {})
        check("get_task 无 id 列任务且失败在前",
              r["summary"].index("task-old-fail") < r["summary"].index(tid)
              and "磁盘空间不足" in r["summary"],
              r["summary"][:160])
        check("get_task 无 id 引导带 id 再查细节",
              "cube_get_task" in (r.get("next") or "") and "id" in (r.get("next") or ""),
              r.get("next"))
        del cmsd.tasks["task-old-fail"]

        cmsd.tasks["cmsd-receiver-run"] = {
            "id": "cmsd-receiver-run", "store_run_id": "cmsd-receiver-run", "source": "store",
            "kind": "install_local", "card": "receiver-card", "status": "failed",
            "created_at": 1791900001, "events": [],
        }
        r = client.tool("cube_get_task", {})
        receiver = next(item for item in r["data"]["tasks"] if item.get("card") == "receiver-card")
        check("store task index exposes callable store handle",
              receiver["id"] == "store:cmsd-receiver-run"
              and receiver["store_task_id"] == "store:cmsd-receiver-run"
              and receiver["store_run_id"] == "cmsd-receiver-run", receiver)
        check("cmsd parent task handle is unchanged",
              any(item["id"] == tid for item in r["data"]["tasks"]), r["data"]["tasks"])
        del cmsd.tasks["cmsd-receiver-run"]

        cmsd.tasks[tid]["store_run_id"] = "cmsd-parent-receiver"
        r = client.tool("cube_get_task", {"task_id": tid})
        check("parent detail links its callable store log handle",
              r["data"]["id"] == tid
              and r["data"]["store_task_id"] == "store:cmsd-parent-receiver"
              and r["data"]["store_run_id"] == "cmsd-parent-receiver", r["data"])
        cmsd.tasks[tid]["store_run_id"] = "../invalid"
        r = client.tool("cube_get_task", {"task_id": tid})
        check("invalid store run does not become a callable handle",
              "store_task_id" not in r["data"], r["data"])
        del cmsd.tasks[tid]["store_run_id"]

        r = client.tool("cube_get_task", {"task_id": "cmsd-receiver-run", "wait_seconds": 0})
        check("bare store run lookup reports unknown and suggests canonical handle",
              r["data"]["status"] == "unknown"
              and r["data"]["error_code"] == "TASK_LOOKUP_FAILED"
              and r["data"]["store_task_id"] == "store:cmsd-receiver-run"
              and "socket 不存在" not in r["summary"], r)
        r = client.tool("cube_get_task", {"task_id": "missing-device-task", "wait_seconds": 0})
        check("unknown device task does not fabricate task or store routing",
              r["data"]["status"] == "unknown"
              and "store_task_id" not in r["data"]
              and r["data"]["observation_error_code"] == "not_found", r)
        cmsd.tasks["malformed-task"] = ["unexpected response"]
        r = client.tool("cube_get_task", {"task_id": "malformed-task", "wait_seconds": 0})
        check("malformed task response remains an unconfirmed observation",
              r["data"]["status"] == "unknown" and r["data"]["error_code"] == "TASK_LOOKUP_FAILED", r)
        del cmsd.tasks["malformed-task"]

        cmsd.tasks["cmsd-legacy-lines"] = {
            "id": "cmsd-legacy-lines", "source": "store", "status": "failed",
            "kind": "install_local", "card": "legacy-card", "store_run_id": "cmsd-legacy-lines",
            "lines": ["first error: " + "x" * 2000] + ["cleanup " + str(i) for i in range(29)],
        }
        r = client.tool("cube_get_task", {"task_id": "cmsd-legacy-lines", "wait_seconds": 0})
        check("legacy v1 store lines are bounded without duplicate raw log",
              r["data"]["events_total"] == 30 and len(r["data"]["events"]) == 8
              and r["data"]["events_offset"] == 22 and "lines" not in r["data"]
              and r["data"]["events_basis"] == "retained_store_lines", r["data"])
        r = client.tool("cube_get_task", {"task_id": "cmsd-legacy-lines", "wait_seconds": 0,
                                         "events_offset": 0, "events_limit": 3})
        check("legacy log root cause can be paged back with explicit clipping",
              r["data"]["events"][0]["message"].startswith("first error:")
              and r["data"]["events"][0]["message_truncated"] is True
              and r["data"]["events_next_offset"] == 3
              and r["data"]["store_task_id"] == "store:cmsd-legacy-lines", r["data"])
        del cmsd.tasks["cmsd-legacy-lines"]

        cmsd.tasks["task-wait"] = {"id": "task-wait", "kind": "enable", "card": STORE_CARD_ID,
                                   "status": "running", "events": []}
        timer = threading.Timer(.15, lambda: cmsd.tasks["task-wait"].update(status="succeeded"))
        timer.start()
        started = time.monotonic()
        r = client.tool("cube_get_task", {"task_id": "task-wait", "wait_seconds": 2})
        timer.join()
        check("get_task bounded waiting observes the terminal state",
              r["data"]["status"] == "succeeded" and .1 <= time.monotonic()-started < 2, r)
        del cmsd.tasks["task-wait"]

        # --- 商店不可达：诚实降级（另起 server 指向关门端口） ---
        dead = socket.socket()
        dead.bind(("127.0.0.1", 0))
        dead_port = dead.getsockname()[1]
        dead.close()
        env2 = make_env(tmp, cmsd, httpd.server_port)
        env2["PINVOU_STORE_URL"] = "http://127.0.0.1:%d/store" % dead_port
        client2 = McpClient(env2)
        try:
            client2.call("initialize")
            r = client2.tool("cube_list_store_models")
            check("商店不可达诚实降级",
                  r["data"]["store_reachable"] is False
                  and "不可达" in r["summary"]
                  and all(e["card_id"] == STORE_CARD_ID for e in r["data"]["models"]),
                  r["summary"])
        finally:
            client2.close()
    finally:
        if client:
            client.close()
        cmsd.stop()
        httpd.shutdown()
        shutil.rmtree(tmp, ignore_errors=True)

    if FAILURES:
        print("\n%d 项失败：%s" % (len(FAILURES), "、".join(FAILURES)))
        return 1
    print("\n全部通过。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
