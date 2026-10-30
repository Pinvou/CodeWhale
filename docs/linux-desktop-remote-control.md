# Linux 双端远程控制

Linux 桌面端复用 Windows 桌面远控的协议、Relay、配对、Session 和执行链路，同时支持 A 主控端与 B 被控端。两端可以是 Windows 或 Linux；这是应用内的远程会话与任务控制，不新增系统屏幕共享或鼠标键盘接管能力。现有浏览器 WebUI v2 保持独立。

## 使用与发布开关

- 支持项目的 64 位 Linux 目标，包括 x86_64、aarch64、loongarch64；不依赖 X11 按键抓取，Wayland 不另设入口限制。macOS 和其他未适配目标仍返回 `desktop_remote_control_unsupported`。
- 构建与运行时共用 `PINVOU_DESKTOP_CONTROLLER_ENABLED` 的既有契约：debug 构建默认具备能力；release/custom 构建默认关闭。需要在构建时显式设置为 `1`，运行时才能使用；构建时关闭的产物不能通过运行时环境重新开启。运行时的 `0` 或非法值均关闭入口与命令。
- 正式构建仍通过项目入口，例如 `PINVOU_DESKTOP_CONTROLLER_ENABLED=1 npm --prefix pinvou3-app run build`。这不代替既有发布资格、Relay enrollment 或双机验收。
- 当前官方 Linux 发布流水线尚未传入上述构建开关，也没有 Linux 对应的远控构建证据任务，因此官方 Linux 安装包目前无法启用远控；须由独立发布工程变更接线后才具备该能力，本 PR 不修改发布流水线。
- 在设置的远程控制页选择主控端或被控端。B 手动开启、选择允许的工作区和远程任务权限后才接入 Relay；A 使用 B 的配对码连接。联网、模型请求及附件传输沿用现有用户操作与授权流程，不自动开启被控、上传文件或安装依赖。

Linux 复用统一 ChatView、`RemoteProxy`、模型来源与临时模型、技能选择、审批、附件上传/产物下载和断线恢复。B 始终持有权威 Session、工作区及权限，A 不接收 B 的本地路径或凭据；配对与消息格式不增加平台分支。

## 附件与平台边界

Linux 不以 `chmod 0600` 模拟 Windows 的文件共享锁。上传仍经私有目录句柄和匿名 staging、大小与 SHA-256 校验，再发布到封存的 `memfd`。内核同时禁止写入、扩容和截断；解析器与当轮图片内嵌读取使用同一 owner 的 `/proc/<owner-pid>/fd/<fd>`，逻辑文件名仅用于格式和会话范围验证。替换逻辑路径不能改变被解析的字节。

解析结果在进程内保留只读 owner，供主聊天的图片暂存与 ACP 的二次读取使用；该字段不参与 IPC、JSON 或持久化，不能由前端声明。传输回执撤销后，有活跃读者时延迟释放；最后一个 owner 退出后关闭句柄。清理只处理持有的对象和空目录，保留外来文件。进程崩溃遗留的 staging 目录沿用现有恢复边界，不保证自动清除。

提示词和 ACP 资源 URI 不保存 proc fd 地址：成功提取正文或识别为图片后，在会话 ledger 的 `attachment_references/<uuid>/` 生成独立引用，目录 `0700`、文件 `0600`。图片保留校验过的原始字节与文件名；非图片仅保存摄入后的 UTF-8 正文 `content.md`，不会通过原始压缩包等容器恢复已被成员摄入拒绝的内容。历史消息引用该独立文件，避免原 owner 释放后 fd 编号复用；ACP 链接的 MIME 和大小取该正文副本。被拒绝、转换失败或不支持的非图片不生成副本，ACP 仅发送告警，不给出可绕过摄入结果的资源链接；该拒绝路径收紧同时适用于 Windows 与 Linux。副本属于普通会话附件，不延续内核 seal；有相应会话文件权限的本地用户或工具可编辑它。删除文件后引用返回文件不存在，不会跟随普通 fd 复用。

这些副本与 upload receipt/reaper 分开，按会话删除及保留策略清理，不会让 memfd 常驻到会话结束。转换前固定已存在的会话目录句柄，结束后仅相对该句柄创建引用；慢转换不能重新创建已删除的会话目录。重复摄入、后续提交失败或取消也可能留下副本，最终由同一会话清理策略回收；因此除当轮内存外还需正常的附件磁盘空间。

需要 Linux 内核的 `memfd_create`/file seals 和可访问的 procfs；创建、封存或访问失败会拒绝该次附件，不回退到可变文件路径。每文件上限和传输并发预算沿用现有实现；活跃附件快照会额外占用内存。

Office、压缩包、MSG、扫描 PDF 的 OCR 和 PDF 预览统一使用独占的私有临时目录。Linux 即使使用 `umask 022`，转换目录仍为 `0700`；转换成功、失败和提前返回均由 RAII 释放目录，解析过程中不向其他本机用户开放明文产物。

- 纯文本、JSON、图片和 XLSX 使用现有解析/输入流程；格式识别与 MIME 取已校验的原始文件名。
- DOCX/ODT 仍调用 pandoc，显式传入输入格式；PDF、压缩包和邮件继续使用现有有界转换命令。
- LibreOffice 不直接接受这类匿名源。旧 Office、WPS、演示及表格回退改经本地 UNO `InputStream` 输入封存字节；使用独占临时 profile 和随机本地管道，禁用宏与链接更新，沿用进程树超时与输出限制。
- Linux 的该转换路径需要系统 `/usr/bin/python3`、`python3-uno` 与 LibreOffice。设置中的依赖检查提供中/英/日文说明和现有手动安装入口；Debian/Ubuntu 可安装 `libreoffice python3-uno`。不自动安装，不开放 TCP UNO 服务。缺桥接依赖返回 `attachment_office_stream_dependency_missing`，设置中的体检项明确提供安装入口；缺少 UNO 时不改走 CLI 文件路径，以维持封存输入契约。成功退出但没有实际转换产物仍判失败。
- UNO 冷启动预算为 60 秒，整棵进程树仍受外层 180 秒截止约束。正常退出先关闭文档和 UNO desktop；不另建可逃逸外层超时的子进程组。SIGKILL 无法运行 Python finally，可能遗留无正文内容的 `0700` 本地管道节点，此限制保留，不能宣称绝对无节点残留。
- 远程摄入的转换器与 IO 错误只输出安全诊断码，覆盖压缩包内成员转换告警；本地普通摄入保留原诊断，成功文档正文不会被路径过滤改写。源消失返回 unavailable，范围、名字或摘要不匹配返回 invalid。

## 验证

Linux Rust 构建的 PipeWire/SPA 绑定生成需要可加载的 libclang；Debian/Ubuntu 构建环境须安装 `libclang-dev`。PR 检查的应用与 CLI 编译/测试作业显式安装该构建依赖，避免依赖 runner 预装状态；这不增加应用运行时安装要求。

Relay 的 Linux 真实回滚回归使用 `ss` 检查监听地址；对应 CI 作业显式安装 `iproute2` 并确认命令可执行，保留监听检查失败时拒绝通过的行为。

```bash
python3 scripts/architecture-guard.py
node --test pinvou3-app/tests/*remote*.test.mjs pinvou3-app/tests/controlled_device*.test.mjs pinvou3-app/tests/connect_remote*.test.mjs pinvou3-app/tests/controller_session*.test.mjs
cargo test --locked --manifest-path pinvou3-app/src-tauri/tests/controller-crypto-harness/Cargo.toml -- --test-threads=1
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib -- --test-threads=1
# 显式调用实际本机 LibreOffice/UNO 转换，无联网或模型调用
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib sealed_rtf_converts -- --ignored --test-threads=1
```

测试覆盖平台/发布开关的组合、双端共享状态机和加密传输、真实 Linux 上传与延迟清理、封存源的子进程读取及篡改拒绝、会话范围拒绝、文件格式和只读 owner 的非序列化边界。前端 mock bridge smoke 与 Rust loopback 测试不代替已安装应用的双机验证。

发布前仍须用实际安装包完成 Linux→Linux、Linux→Windows、Windows→Linux：配对、chat/code 两种模式、工作区授权、两种模型来源、审批允许/拒绝、图片/文本/Office 附件、产物下载、断网恢复和 B 手动关闭。不同架构的系统包、Wayland 实际宿主与 Windows 原生回归分别保留验证记录。

## 初版本机验证记录（2026-10-23）

执行环境为 aarch64 Linux。以下记录针对初版 `de43bd12e`，不代表安装包、Windows 原生或真实双机发布验收。

本机未执行真实模型/ACP 工具回合，也未运行缺少 `msgconvert` 的 MSG 实际转换；这些场景保留到安装包环境验收。

| 验证 | 结果 |
| --- | --- |
| Rust lib 测试二进制，串行完整执行 | 4,810 passed，0 failed，20 ignored；已同步 main 并完成权限修复 |
| 文件模块权限修复后定向回归（完整 lib 的子集） | 83 passed，0 failed，8 ignored；含独立 `umask 022` 与失败清理回归 |
| controller crypto harness，串行完整执行 | 509 passed，0 failed |
| 显式启用 `sealed_rtf_converts` 实际转换/摄入 | 1 passed；不以默认 ignored 计作覆盖 |
| 前端远控/双端面板相关回归 | 360 passed，0 failed |
| 发布开关、文档与三语言契约 | 11 passed，0 failed |
| 附件 drop / ACP 客户端回归 | 通过 |
| UI 构建与 remote archive 浏览器 smoke | 通过；smoke 使用 mock bridge，不覆盖原生 IPC |
| 架构守卫、fork guard fast、Rust fmt、diff whitespace | 通过 |
| ESLint 与 Clippy 默认仓库门禁 | 通过；warning 输出非零 |
| DOCX/pandoc、ZIP、RTF/PDF、演示流及表格流实测 | 通过；使用本机工具与合成数据，无模型回合 |

Linux 新启用路径的本地附件夹具放在用户目录中，继续遵守上传路径策略。远程模型接线测试比较运行时最终生效的模型名，因此本机已启动单模型服务时的既有 served-name 解析不会造成错误的配置名断言；产品模型路由未因测试调整而改变。

2026-10-30 复审后，该模型路由断言调整已从本 PR 净差异中撤回，测试与 main 完全一致，保留对夹具预期模型名的客观断言；以上历史验证不作为新 head 的通过结论。

## 评审修复后验证（2026-10-26）

以下结果针对修复后的同一源码快照，执行环境仍为 aarch64 Linux。Rust 测试使用编译后独立复制的二进制，避免共享构建目录被其他任务更新。

| 验证 | 结果 |
| --- | --- |
| 完整 Rust lib，串行 | 4,823 passed，0 failed，23 ignored |
| 文件模块、ACP 附件、聊天附件定向（完整 lib 子集） | 分别 90、6、9 passed；默认忽略项分别 10、1、0 |
| 显式实际工具验证（不计入默认 ignored 覆盖） | ZIP 拒绝成员、嵌套转换诊断、UNO 启动预算/缺依赖、sealed RTF 各 1 passed |
| controller crypto harness | 509 passed，0 failed |
| 前端远控与三语言相关回归 | 361 passed，0 failed；其中完整 bridge 能力正反向四组测试 45 passed |
| UI 构建、ESLint、Clippy | 通过；warning 输出非零 |
| 架构守卫、fork guard fast、Rust fmt、diff whitespace | 通过 |

新增回归实际复用已释放的 fd 编号，确认旧 proc 路径能读到合成的无关字节，而历史引用仍读取独立正文；同时覆盖私有权限、会话隔离及删除回收、删除后慢转换不能重建 ledger、被拒附件不生成副本或资源链接，以及压缩包链接只包含提取正文。前端测试实际加载完整 bridge 并调用 Linux 的主控与被控状态命令。

Windows 原生与已安装应用双机演练、真实模型/ACP 工具回合、MSG 实际转换等初版列出的未测项仍保留；PR 新 head 的跨平台 CI 另行验证，不能用旧 head 的绿灯替代。
