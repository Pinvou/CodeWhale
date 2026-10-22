# 来源获取与独立下载

内置持久下载/校验 worker 当前面向 Linux。其他平台返回 unsupported，应改用该平台已有 CLI/终端；不静默借用 Linux 进程判定。

以下独立下载能力用于模型文件及允许独立安装的依赖，不授权从外部安装驭光（Tachyon / tachyon-serve）。驭光必须使用模型商店中经过 H3C 官方适配的卡片；取得某个第三方权重不表示驭光能够加载它。

- `cube_search_models(query, sort, limit)` 搜索 HF；其他社区源使用已有联网工具或专用 CLI。不要猜仓库后立即下载，先核实身份和版本。
- `cube_inspect_source(source="hf", repo_id, revision)` 返回文件和配置事实，不按架构/量化过滤。`source="url"` 用 HEAD 探测，某些服务器不支持 HEAD，可直接下载；`source="local"` 检查已有目录。
- 下载前检查已有目录与宿主发现的权重候选；同名、同体积只算候选，匹配文件清单与发布方摘要后再复用。保留原件及其使用路径，优先引用或复制，不擅自移动/删除；重下需要具体原因，不用“更干净”代替验证。生成模板优先消费换行分隔的 LMM_IMPORT_CANDIDATES / LMM_IMPORT_DIRS，不复制缓存、凭据或无关文件。
- `cube_fetch_repo(repo_id, directory, revision, include, exclude)` 固定 API 返回的提交后下载快照。glob 由你按所需引擎选择；分片权重不要漏分片或 index、tokenizer、配置。未能固定提交时此工具不下载，可改用能固定版本的 CLI 或明确文件 URL。
- `cube_fetch_files(directory, files)` 下载任意 HTTP(S) 文件；每项包含相对 `path`、`url`，可选 `size_bytes`、`sha256`。支持子目录和非商店目录，没有架构、量化、100 GiB 限制。
- 两者返回独立 task_id，`cube_get_task` 查询、`cube_cancel_task` 取消、`cube_resume_task` 恢复。任务不依赖 MCP 连接、商店或 cmsd；机器关机后任务标为 interrupted，需要明确恢复。取消在当前网络读取结束/超时后生效，不承诺瞬时停止。
- `cube_get_task` 默认返回最新 8 条保留事件，`events_total/events_offset/events_next_offset` 用于分页（events_limit 最大 100）；这不是无上限完整日志。标准 cmsd 任务可 wait_seconds 有界等待，独立任务仍按真实状态查询。任务列表只作索引，单次任务成功后继续验证和登记，不把“文件进度100%”等同整个安装成功。
- 验证 Range 区间；远端不支持 Range 时从头获取。有预期摘要或稳定远端版本标识才续传，避免混合不同内容。签名 URL 失效时换新任务/CLI；已存在文件无法确认一致时保留原文件并返回冲突，由你校验或选择其他目标。
- 没有发布方摘要时仍允许下载，但结果 `computed_only` 只表示本地计算了摘要，不能称为来源认证。`expected_sha256` 表示匹配调用方提供的摘要，其可信度取决于摘要来源。
- 已完成文件的私有恢复摘要与本轮 `artifacts` 分开保存。恢复开始前先持久化原有 path/hash，再重置本轮成果；新文件也先持久化正向摘要，才无覆盖原子发布。排队、重新校验或发布/状态保存间中断不丢恢复依据，但旧回执本身不算本轮完成。
- 每轮使用已有目标均重新计算完整 SHA-256，与显式预期或原任务的恢复摘要匹配后才计入本轮成果；大小、修改时间或旧校验标记不能替代内容检查。无法确认一致、内容变化或恢复记录损坏时保留文件并报错；不会覆盖已有目标。
- `credential_env` 只接受已配置的环境变量名；工具目前不负责交互登录，也不直接读取系统密钥环。需要其他认证机制时复用已有 CLI 的安全认证流程。凭据不经跨域重定向转发。
- 下载请求保存在 `<PINVOU3_HOME>/megacube/installations/tasks/` 的私有记录中，可能含签名 URL，文件权限为 0600；展示结果隐藏 URL 查询参数。不要将这些请求文件或完整签名 URL 贴进聊天。
- 多下载工具是并列能力。内置工具无法获取的 Git LFS、ModelScope 或其他来源，使用现有终端的专用 CLI；仍应留下版本与落盘记录。
