"""Atomic model acquisition and external-installation observation tools."""
from . import artifacts, sources, store_recovery, transfers
from .state import ToolError, error_reply


def tool(name, description, properties, required=()):
    return {"name": name, "description": description,
            "inputSchema": {"type": "object", "properties": properties,
                            "required": list(required), "additionalProperties": False}}


TEXT = {"type": "string"}
DIRECTORY = {"type": "string", "description": "用户指定的本机绝对目录；允许商店目录之外的位置。"}
CREDENTIAL = {"type": "string", "description": "已有凭据环境变量名；只传引用，禁止传 Token 值。"}
FILES = {"type": "array", "minItems": 1, "maxItems": 10000, "items": {
    "type": "object", "properties": {
        "path": {"type": "string", "description": "相对目录内的文件路径，支持子目录。"},
        "url": {"type": "string", "description": "HTTP(S) 文件地址。"},
        "sha256": TEXT, "size_bytes": {"type": "integer", "minimum": 0}},
    "required": ["path", "url"], "additionalProperties": False}}
REPO_PROPERTIES = {"repo_id": TEXT, "revision": {"type": "string", "description": "分支、标签或提交；下载时解析并固定提交。"}, "credential_env": CREDENTIAL}

TOOLS = [
    tool("cube_list_incomplete_installs", "只读列出商店未完成安装；包含尚未进入模型库的失败/取消任务，不授予管理权。",
         {"installations_offset": {"type": "integer", "minimum": 0}, "installations_limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 8}}),
    tool("cube_preview_install_retry", "只读预览原任务的当前安装包摘要、固定目录和 download/install_local 语义；不重装、不启用。",
         {"run_id": TEXT}, ["run_id"]),
    tool("cube_retry_install", "按已审阅摘要重试失败或取消的商店安装，复用界面后台及 Cube Manage Software 准入。保留原下载/部署类型和部分文件，不自动启用；连接或能力失败不可回退完整安装。按用户既有授权执行。",
         {"run_id": TEXT, "expected_preview_sha256": {"type": "string", "description": "64 位十六进制预览摘要；格式在执行前 fullmatch 校验。"}}, ["run_id", "expected_preview_sha256"]),
    tool("cube_search_models", "搜索 HF 实时候选，不按架构/量化/体积过滤；其他来源可用已有搜索或浏览器工具。",
         {"query": TEXT, "limit": {"type": "integer", "minimum": 1, "maximum": 50},
          "sort": {"type": "string", "enum": ["downloads", "lastModified", "likes"]}, "credential_env": CREDENTIAL}, ["query"]),
    tool("cube_inspect_source", "只读检查 HF 仓库、文件 URL 或本地目录；返回来源事实，不把标准框架限制当作不可安装。",
         {"source": {"type": "string", "enum": ["hf", "url", "local"]},
          **REPO_PROPERTIES, "url": TEXT, "directory": DIRECTORY,
          "files_offset": {"type": "integer", "minimum": 0},
          "files_limit": {"type": "integer", "minimum": 1, "maximum": 500}}, ["source"]),
    tool("cube_inspect_environment", "只读检查本机架构、磁盘、系统内存及下载/引擎程序；不自动启动进程或查询 GPU。",
         {"directory": DIRECTORY}),
    tool("cube_fetch_files", "独立批量下载 HTTP(S) 文件，不依赖商店或 Cube Manage Software，不限制模型架构/格式/总大小；返回可查询、取消、恢复的任务。不会覆盖无法确认一致的已有文件。按用户授权范围执行。",
         {"directory": DIRECTORY, "files": FILES, "credential_env": CREDENTIAL}, ["directory", "files"]),
    tool("cube_fetch_repo", "独立获取 HF 仓库快照，先固定提交，支持 glob 文件筛选、续传和摘要校验。非 HF 来源使用已有终端及专用下载 CLI。仅下载，不启动模型。",
         {**REPO_PROPERTIES, "directory": DIRECTORY,
          "include": {"type": "array", "items": TEXT}, "exclude": {"type": "array", "items": TEXT}}, ["repo_id", "directory"]),
    tool("cube_resume_task", "恢复独立下载或文件校验任务；只接受失败、取消或中断状态。来源已失效时改用新下载任务。",
         {"task_id": TEXT}, ["task_id"]),
    tool("cube_validate_artifact", "只读检查任意本地安装目录、文件格式与入口文件；不运行脚本，不要求六脚本齐全，不把未纳管当失败。",
         {"directory": DIRECTORY}, ["directory"]),
    tool("cube_verify_artifacts", "后台计算/比对文件大小和 SHA256；省略 files 时检查目录普通文件。返回任务 id；computed_only 摘要不证明发布方可信。",
         {"directory": DIRECTORY, "files": {"type": "array", "items": {
             "type": "object", "properties": {"path": TEXT, "sha256": TEXT, "size_bytes": {"type": "integer", "minimum": 0}},
             "required": ["path"], "additionalProperties": False}}}, ["directory"]),
    tool("cube_validate_runtime", "向显式指定的本机回环 OpenAI 兼容服务发送小规模真实推理，检查模型名、聊天和可选流式。会占用推理资源，不启动/停止服务，不改变路由；其他接口和能力通过已有终端补测。",
         {"directory": DIRECTORY, "base_url": TEXT, "model": TEXT,
          "max_tokens": {"type": "integer", "minimum": 32, "maximum": 512, "default": 128,
                         "description": "单次冒烟输出预算；耗尽且无正文时报告inconclusive，不直接判模型失败。"},
          "test_stream": {"type": "boolean"}, "credential_env": CREDENTIAL}, ["directory", "base_url", "model"]),
    tool("cube_register_artifact", "登记框架外安装与真实验证证据；首次推理成功后先登记，再处理可选接入。重复登记保留未显式替换的来源、说明和证据，返回历史检查能力与时间。只记录，不执行脚本或取得 Cube Manage Software 管理权，当前健康始终未知；cube_list_models 可查。",
         {"directory": DIRECTORY, "name": TEXT, "source_url": TEXT,
          "instructions_file": {"type": "string", "description": "产物目录内安装说明的相对路径；只登记，不执行。"},
          "verification_task_id": TEXT, "runtime_validation_id": TEXT}, ["directory"]),
]

_HANDLERS = {
    "cube_list_incomplete_installs": store_recovery.list_incomplete,
    "cube_preview_install_retry": store_recovery.preview,
    "cube_retry_install": store_recovery.retry,
    "cube_search_models": sources.search_models,
    "cube_inspect_source": sources.inspect_source,
    "cube_inspect_environment": sources.inspect_environment,
    "cube_fetch_files": transfers.start_download,
    "cube_fetch_repo": sources.fetch_repo,
    "cube_resume_task": transfers.resume_task,
    "cube_validate_artifact": artifacts.inspect_artifact,
    "cube_verify_artifacts": artifacts.verify_artifacts,
    "cube_validate_runtime": artifacts.validate_runtime,
    "cube_register_artifact": artifacts.register_artifact,
}


def call(name, args):
    try:
        if not isinstance(args, dict):
            raise ToolError("INVALID_INPUT", "参数必须是对象。")
        value = _HANDLERS[name](args)
        return value
    except Exception as error:
        return error_reply(error)


DISPATCH = {name: (lambda args, name=name: call(name, args)) for name in _HANDLERS}
