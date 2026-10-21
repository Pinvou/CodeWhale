"""Local installation records and bounded HTTP primitives; no engine execution."""
import hashlib
import http.client
import json
import os
from pathlib import Path, PurePosixPath
import re
import tempfile
import urllib.error
import urllib.parse
import urllib.request


class ToolError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code


def reply(summary, data=None, next_step=""):
    return {"summary": summary, "data": data, "next": next_step}


def page(items, args, prefix):
    offset, limit = args.get(prefix + "_offset", 0), args.get(prefix + "_limit", 200)
    if type(offset) is not int or offset < 0 or type(limit) is not int or not 1 <= limit <= 500:
        raise ToolError("INVALID_PAGE", "分页 offset 必须非负，limit 必须在 1 到 500 之间。")
    return {prefix: items[offset:offset + limit], prefix + "_total": len(items),
            prefix + "_offset": offset,
            prefix + "_next_offset": offset + limit if offset + limit < len(items) else None}


def root():
    home = Path(os.environ.get("PINVOU3_HOME") or Path.home() / ".pinvou3")
    return home / "megacube" / "installations"


def private_dir(path):
    path = Path(path)
    records = root()
    if path == records or records in path.parents:
        # Only the writer that creates a NEW root may attest its independent
        # origin. Never relabel an existing/empty legacy root: older local cmsd
        # observations used exactly that directory as their sole receipt.
        for parent in (records, *records.parents):
            if parent.is_symlink():
                raise ToolError("UNSAFE_PATH", "记录目录及其父目录不能是符号链接。")
        records.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        try:
            records.mkdir(mode=0o700)
        except FileExistsError:
            pass
        else:
            # An uncertain publication must keep the local guard closed. Its
            # pending entry is deliberately retained on any write/fsync error;
            # existing roots are never relabelled by a later generic writer.
            pending = records / ".origin-pending"
            fd = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            os.close(fd)
            marker = records / ".independent-records-v1"
            fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as stream:
                stream.write(b"pinvou-independent-installation-records-v1\n")
                stream.flush()
                os.fsync(stream.fileno())
            if os.name == "posix":
                fd = os.open(records, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(fd)
                finally:
                    os.close(fd)
            pending.unlink()
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if path.is_symlink() or not path.is_dir():
        raise ToolError("UNSAFE_PATH", "记录目录不能是符号链接或特殊文件。")
    return path


def write_json(path, value):
    path = Path(path)
    private_dir(path.parent)
    fd, name = tempfile.mkstemp(prefix=".write-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(value, f, ensure_ascii=False, indent=2)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def remember_local_device():
    path = root().parent / "local-device-observed-v1"
    for parent in (path.parent, *path.parent.parents):
        if parent.is_symlink():
            raise ToolError("DEVICE_RECORD_FAILED", "设备观测记录路径未核实，尚未发送管理请求。")
    write_json(path, {"source": "cmsd", "model_ownership": False})
    if os.name == "posix":
        fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


def read_json(path):
    path = Path(path)
    if path.is_symlink() or not path.is_file():
        raise ToolError("RECORD_NOT_FOUND", "没有找到普通文件形式的记录。")
    with path.open(encoding="utf-8") as f:
        return json.load(f)


def absolute_dir(value, create=False):
    if not isinstance(value, str) or not Path(value).expanduser().is_absolute():
        raise ToolError("INVALID_PATH", "目录必须使用绝对路径。")
    path = Path(value).expanduser()
    # Explicit destinations may live outside the store, but never follow links.
    for p in [path, *path.parents]:
        if p.is_symlink():
            raise ToolError("UNSAFE_PATH", "目标目录及其父目录不能是符号链接。")
    if create:
        path.mkdir(parents=True, exist_ok=True)
    if not path.is_dir():
        raise ToolError("DIRECTORY_NOT_FOUND", "目标目录不存在。")
    return path.resolve()


def relative_name(value):
    if not isinstance(value, str) or not value or "\\" in value or any(ord(c) < 32 for c in value):
        raise ToolError("INVALID_FILENAME", "文件名非法。")
    path = PurePosixPath(value)
    if path.is_absolute() or any(p in ("", ".", "..") for p in value.split("/")):
        raise ToolError("INVALID_FILENAME", "文件名必须是目录内的相对路径，不能包含路径穿越。")
    return value


def target_file(directory, name):
    path = Path(directory) / relative_name(name)
    for p in [path, *path.parents]:
        if p.is_symlink():
            raise ToolError("UNSAFE_PATH", "文件及其父目录不能是符号链接。")
    if path.exists() and not path.is_file():
        raise ToolError("UNSAFE_PATH", "目标不能是目录或特殊文件。")
    return path


def sha256_file(path, cancel=None):
    digest = hashlib.sha256()
    with Path(path).open("rb") as f:
        while True:
            if cancel:
                cancel()
            block = f.read(1024 * 1024)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def http_url(value):
    if not isinstance(value, str) or any(ord(c) < 32 for c in value):
        raise ToolError("INVALID_URL", "地址必须是 HTTP(S) URL。")
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password:
        raise ToolError("INVALID_URL", "只接受不含内嵌账号密码的 HTTP(S) 地址。")
    return value


def public_url(value):
    parsed = urllib.parse.urlsplit(http_url(value))
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", ""))


class RedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        http_url(newurl)
        if req.type == "https" and urllib.parse.urlsplit(newurl).scheme != "https":
            raise ToolError("INSECURE_REDIRECT", "拒绝 HTTPS 降级重定向。")
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if redirected and urllib.parse.urlsplit(req.full_url).netloc != urllib.parse.urlsplit(newurl).netloc:
            redirected.remove_header("Authorization")
        return redirected


def open_url(url, credential_env=None, headers=None, method=None, body=None):
    http_url(url)
    request_headers = {"User-Agent": "megacube-admin/2", **(headers or {})}
    if credential_env:
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", credential_env):
            raise ToolError("INVALID_CREDENTIAL_REF", "凭据引用必须是环境变量名，不能传 Token 本身。")
        token = os.environ.get(credential_env)
        if not token:
            raise ToolError("AUTH_REQUIRED", "凭据环境变量未设置；请使用安全凭据配置或已有终端工具。")
        if urllib.parse.urlsplit(url).scheme != "https":
            raise ToolError("INSECURE_AUTH", "凭据只允许通过 HTTPS 发送。")
        request_headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=body, headers=request_headers, method=method)
    return urllib.request.build_opener(RedirectHandler()).open(req, timeout=20)


def request_json(url, credential_env=None, method=None, value=None):
    body = None if value is None else json.dumps(value).encode("utf-8")
    headers = {} if value is None else {"Content-Type": "application/json"}
    with open_url(url, credential_env, headers, method, body) as response:
        raw = response.read(16 * 1024 * 1024 + 1)
    if len(raw) > 16 * 1024 * 1024:
        raise ToolError("METADATA_TOO_LARGE", "元数据响应超过 16 MiB；可改用专用 CLI。")
    try:
        return json.loads(raw)
    except (ValueError, UnicodeError):
        raise ToolError("INVALID_METADATA", "服务未返回有效 JSON。") from None


def error_reply(error):
    if isinstance(error, ToolError):
        code, message = error.code, str(error)
    elif isinstance(error, urllib.error.HTTPError):
        code, message = "HTTP_%s" % error.code, "远端服务返回 HTTP %s。" % error.code
    elif isinstance(error, (urllib.error.URLError, TimeoutError, http.client.HTTPException)):
        code, message = "NETWORK_ERROR", "网络连接失败或超时。"
    elif isinstance(error, OSError):
        code, message = "FILESYSTEM_ERROR", "文件操作失败（errno=%s）；检查空间、权限与目录。" % error.errno
    else:
        code, message = "INVALID_INPUT", "参数或元数据格式不正确。"
    return reply(message, {"error_code": code},
                 "按错误补充条件或换下载/安装工具；当前工具失败不代表模型无法安装。")
