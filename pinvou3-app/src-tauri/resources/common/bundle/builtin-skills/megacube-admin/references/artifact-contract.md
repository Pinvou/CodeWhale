# 产物检查、验证与接入

最终结果按实际能力验收，安装过程使用符合引擎来源限制的合适工具。驭光（Tachyon / tachyon-serve）需要 H3C 官方逐模型适配，只能通过模型商店官方适配卡安装；本文件的终端安装与生成卡说明不能用于绕过该限制。

1. `cube_validate_artifact(directory)` 观察文件、体积、格式和可能的入口，不执行脚本，不要求模型满足六脚本或 OpenAI 接口契约。符号链接和临时下载被跳过并报告，必要时使用其他工具补查。
2. `cube_verify_artifacts(directory, files?)` 后台计算/比较摘要。明确清单可检查缺失文件、预期大小和摘要；省略清单只检查当前普通文件，不能证明上游应有的文件全部齐全。结果由 `cube_get_task` 获取，可取消/恢复。
3. 通过已有终端安装引擎、启动目标模型。`cube_validate_runtime(directory, base_url, model, max_tokens?)` 验证回环 OpenAI 兼容端点的模型身份、聊天和可选流式；它不会启动服务。验证直连指定回环端点，不使用环境 HTTP 代理，仅允许同协议、主机与有效端口的路径重定向；其他端点的响应不作为本机证据。输出预算默认128，允许32—512；预算耗尽且无正文时返回 `status=inconclusive`，不要直接判模型不可用，可增加预算重验。工具调用、视觉、取消、质量和负载没有测试就必须标为未测。其他协议或远端服务用已有工具补测。
4. 在产物目录写 `INSTALLATION.md`，包含来源与固定版本、位置、引擎/依赖、启动/停止命令、端点、验证证据、清理方法、已改环境和未知项。不记录 Token、密码、私有 URL 或含密钥的启动命令。
5. 推理首次成功就用 `cube_register_artifact(directory, name?, source_url?, instructions_file?, verification_task_id?, runtime_validation_id?)` 保存外部成果，再尝试可选接入。`instructions_file` 是目录内相对路径，不会被执行。证据只能引用真实工具产生的 id；重复登记未传的来源、说明和证据沿用已有值。`file_evidence`、`runtime_evidence` 是带时间的历史观察，最近证据引用保存在 `evidence_history`，不证明当前健康、配置未变或进程归属。

外部安装记录在 `cube_list_models.data.external_artifacts` 可见，当前运行状态始终 unknown，检查工具可继续使用；没有管理权时不能宣称可通过 cmsd 启停、自启、切换或自动回滚。

若要执行完整标准卡安装，用 `cube_inspect_environment` 返回的 `cards_directory` 定位卡片根目录，准备 `cards/<唯一 card_id>/`。六入口为 install_engine/download_model/deploy/enable/stop/uninstall.sh，另需现有 common.sh 或 compose 布局、card.json 等引擎契约。`cube_install_local_card(card_id)` 会重新执行安装、下载/复用、部署，可能触发提权；安装成功仍需确认真实运行状态和管理权。它不是服务登记或安全沙箱。

新生成的卡在 card.json 的 `package_files` 中列出交付文件（相对文件路径）；六入口单独收集，card.json 自动保留。只打包静态脚本、必要配置与校验清单，运行缓存、日志、.venv、权重均不放入交付包。旧卡没有清单时使用兼容扫描，跳过已知运行目录与符号链接。

已有服务可用时，先交付和登记；只有核实标准卡与实际配置匹配、授权覆盖管理接入时，才用 `cube_start_model` 显式启用/接管该卡（会修改管理权和自启）。不要为了“纳管”再次调用 install_local。其他服务没有接入 API 时继续独立使用，不虚构通用接管能力。管理权未知或失联先恢复连接，不绕过已有托管权。

既有原生 Tachyon 卡也包含完整六入口；该技术契约不表示任意自制卡已经获得 H3C 官方适配。新增/修复驭光安装仍走模型商店官方适配卡，不把其他卡的引擎复制到实验卡。卸载清理前先由授权管理路径停用；卸载脚本发现存活 PID、精确权重引用或占用端口时拒绝清理，不直接杀进程。默认保留权重，显式 LMM_PURGE_WEIGHTS=1 仅删除本卡声明文件与校验/部署记录；共享引擎和其他文件保留。

校验/冒烟结果是某一时刻的观察。修改权重、镜像、配置、上下文或并发后重验，不借用旧结果证明新配置。验证失败可以登记事实和剩余工作，不能把失败或仅下载完成的模型报告为可用。
