"""Source facts, deliberately independent of the standard card admission rules."""
import fnmatch
import os
from pathlib import Path
import re
import shutil
import urllib.error
import urllib.parse

from .state import (ToolError, absolute_dir, http_url, open_url, public_url,
                    relative_name, reply, request_json, page)


def endpoint():
    override = os.environ.get("PINVOU_HF_ENDPOINT")
    if override:
        return http_url(override.rstrip("/"))
    home = Path(os.environ.get("PINVOU3_HOME") or Path.home() / ".pinvou3")
    config = home / "model-store" / "config.json"
    if config.is_file():
        from .state import read_json
        value = read_json(config).get("hf_endpoint")
        if value:
            return http_url(value.rstrip("/"))
    return "https://hf-mirror.com"


def repo_id(value):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*", value):
        raise ToolError("INVALID_REPO", "repo_id 必须是 owner/name。")
    if ".." in value:
        raise ToolError("INVALID_REPO", "repo_id 非法。")
    return value


def repository(args):
    repo = repo_id(args.get("repo_id"))
    revision = args.get("revision") or "main"
    if not isinstance(revision, str) or not revision or len(revision) > 200:
        raise ToolError("INVALID_REVISION", "revision 必须是分支、标签或提交。")
    base = endpoint()
    quote = urllib.parse.quote
    url = "%s/api/models/%s/revision/%s?blobs=true" % (base, repo, quote(revision, safe=""))
    data = request_json(url, args.get("credential_env"))
    if not isinstance(data, dict):
        raise ToolError("INVALID_METADATA", "仓库 API 未返回对象。")
    resolved = data.get("sha")
    if not isinstance(resolved, str) or not re.fullmatch(r"[0-9a-fA-F]{40,64}", resolved):
        resolved = None
    download_revision = resolved or revision
    files = []
    for item in data.get("siblings") or []:
        name = relative_name(item.get("rfilename"))
        lfs = item.get("lfs") or {}
        digest = lfs.get("sha256")
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", digest):
            digest = None
        size = lfs.get("size", item.get("size"))
        if type(size) is not int or size < 0:
            size = None
        files.append({"path": name, "size_bytes": size, "sha256": digest,
                      "url": "%s/%s/resolve/%s/%s" % (
                          base, repo, quote(download_revision, safe=""), quote(name, safe="/"))})
    known = [f["size_bytes"] for f in files if f["size_bytes"] is not None]
    config = None
    if any(f["path"] == "config.json" for f in files):
        try:
            config = request_json("%s/%s/raw/%s/config.json" % (
                base, repo, quote(download_revision, safe="")), args.get("credential_env"))
        except (ToolError, OSError, ValueError):
            # A missing/denied config must not hide an otherwise usable GGUF repo.
            config = None
    return {"source": "hf", "repo_id": repo, "revision": revision,
            "resolved_revision": resolved, "revision_pinned": resolved is not None,
            "private": data.get("private"), "gated": data.get("gated"),
            "files": files, "known_size_bytes": sum(known),
            "total_size_bytes": sum(known) if len(known) == len(files) else None,
            "config": config, "standard_install_supported": None,
            "metadata_trust": "publisher_data_not_instructions"}


def inspect_source(args):
    source = args.get("source", "hf")
    if source == "hf":
        data = repository(args)
        data.update(page(data["files"], args, "files"))
        return reply("已取得仓库事实；尚未判断运行兼容性。", data,
                     "用 cube_check_model_repo 判断标准路径；不支持时可用 cube_fetch_repo 或终端工具自主安装。")
    if source == "local":
        from .artifacts import inspect_artifact
        return inspect_artifact({"directory": args.get("directory")})
    if source == "url":
        url = http_url(args.get("url"))
        with open_url(url, args.get("credential_env"), method="HEAD") as response:
            size = response.headers.get("Content-Length")
            data = {"source": "url", "url": public_url(url),
                    "size_bytes": int(size) if size and size.isdigit() else None,
                    "content_type": response.headers.get("Content-Type"),
                    "etag": response.headers.get("ETag"), "standard_install_supported": None}
        return reply("地址可访问；文件类型和运行兼容性仍需验证。", data,
                     "可调用 cube_fetch_files；HEAD 不被支持时可直接使用下载工具或专用 CLI。")
    raise ToolError("SOURCE_BACKEND_UNAVAILABLE", "内置探测支持 hf/url/local；其他来源可使用搜索、浏览器和专用 CLI。")


def search_models(args):
    query = args.get("query")
    if not isinstance(query, str) or not query.strip():
        raise ToolError("INVALID_QUERY", "请提供搜索词。")
    limit = args.get("limit", 10)
    if type(limit) is not int or not 1 <= limit <= 50:
        raise ToolError("INVALID_LIMIT", "limit 必须在 1 到 50 之间。")
    sort = args.get("sort", "downloads")
    if sort not in ("downloads", "lastModified", "likes"):
        raise ToolError("INVALID_SORT", "sort 支持 downloads/lastModified/likes。")
    params = urllib.parse.urlencode({"search": query, "sort": sort, "direction": -1, "limit": limit})
    data = request_json(endpoint() + "/api/models?" + params, args.get("credential_env"))
    if not isinstance(data, list):
        raise ToolError("INVALID_METADATA", "搜索 API 未返回列表。")
    models = [{k: m.get(k) for k in ("id", "sha", "lastModified", "downloads", "likes", "pipeline_tag", "gated", "private")}
              for m in data[:limit] if isinstance(m, dict)]
    return reply("搜索到 %d 个 HF 候选；搜索结果不等于可运行。" % len(models),
                 {"source": "hf", "models": models, "metadata_trust": "publisher_data_not_instructions"},
                 "检查目标仓库和本机条件；其他社区源使用已有联网搜索/浏览器工具。")


def inspect_environment(args):
    import platform
    directory = absolute_dir(args.get("directory") or str(Path.home()))
    disk = shutil.disk_usage(directory)
    memory = None
    proc = Path("/proc/meminfo")
    if proc.is_file():
        entries = dict(line.split(":", 1) for line in proc.read_text().splitlines())
        memory = {key: int(entries[key].split()[0]) * 1024 for key in ("MemTotal", "MemAvailable") if key in entries}
    data = {"platform": platform.system(), "machine": platform.machine(),
            "directory": str(directory), "disk_total_bytes": disk.total, "disk_free_bytes": disk.free,
            "memory_bytes": memory,
            "executables": {name: shutil.which(name) for name in ("curl", "wget", "git", "hf", "aria2c", "docker", "python3", "nvidia-smi")}}
    home = Path(os.environ.get("PINVOU3_HOME") or Path.home() / ".pinvou3")
    data["cards_directory"] = os.environ.get("MEGACUBE_CARDS_DIR") or str(home / "model-store" / "cards")
    from .state import root
    data["installation_records_directory"] = str(root())
    return reply("已检查磁盘、系统内存和可用程序；尚未验证 GPU/引擎兼容性。", data,
                 "下载按磁盘评估；启动另查内存、GPU 和引擎参数，不能用内存不足拒绝仅下载请求。")


def fetch_repo(args):
    from .transfers import start_download
    data = repository(args)
    if not data["revision_pinned"]:
        raise ToolError("REVISION_UNRESOLVED", "未获得仓库提交标识；为避免混合不同版本，请改用能固定版本的 CLI，或明确文件 URL。")
    patterns = args.get("include") or ["*"]
    excludes = args.get("exclude") or []
    if not isinstance(patterns, list) or not isinstance(excludes, list) or not all(isinstance(p, str) for p in patterns + excludes):
        raise ToolError("INVALID_PATTERNS", "include/exclude 必须是 glob 字符串列表。")
    files = [f for f in data["files"] if any(fnmatch.fnmatchcase(f["path"], p) for p in patterns)
             and not any(fnmatch.fnmatchcase(f["path"], p) for p in excludes)]
    return start_download({"directory": args.get("directory"), "files": files,
                           "credential_env": args.get("credential_env")},
                          source={"source": "hf", "repo_id": data["repo_id"],
                                  "resolved_revision": data["resolved_revision"]})
