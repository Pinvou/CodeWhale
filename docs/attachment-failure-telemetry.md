# 附件失败诊断

桌面路径附件、桌面拖放和 Web 附件共用 `features/attachments/attachment-telemetry.js`
的有界错误分类。已知空文件、20 MiB 大小限制、压缩包限制及完整性拒绝仍为
`validation_failed`；权限、网络和超时分别使用既有类别；上传器缺失为
`configuration_missing`，无证据的其他异常为 `internal_error`。不会根据任意
错误文本里的 cancel 字样推定用户取消，仅保留明确取消操作及上传器的类型化取消。
旧原生字符串只按完整原因或明确错误模板解释：路径字段与 OS 原因分开，文件名
里的 timeout、permission denied 等字样不参与归因；不凭跨平台含义不同的 OS
错误数字猜测权限。桌面固定完整性拒绝与 Web 对应错误码均归验证失败，完整性
任务或读取异常仍按实际已知原因分类，无证据时保守归内部错误。

附件属于任务提交前的输入准备，沿用 v1 已注册的 `stage: submission` 和
`input_type: attachment`，不新增旧收集端无法接受的阶段枚举。固定 `step` 区分：

| step | 范围 |
| --- | --- |
| `attachment_validation` | File 元数据预检、上传器可用性检查 |
| `attachment_read` | 本地摘要和分块读取 |
| `attachment_upload` | 非最终分块发送 |
| `attachment_commit` | 最终分块发送、后端完整性校验、落盘与摄入；这些环节共享一个原生调用，不能据此细分根因 |
| `attachment_result` | 返回值校验及本地附件结果关联 |
| `attachment_ingest` | 路径入口的原生路径校验与摄入；不冒充已定位到具体解析器 |

步骤观察仅更新该附件尝试的本地元数据，不额外发送事件；可选观察回调异常不会
中断上传。每次尝试仍只发送一个终态，取消后的迟到完成不另报成功或失败。
自动事件只包含原有匿名 ID、类型/大小桶、固定阶段、步骤和错误类别；本地错误
文本只用于有界分类，不上传原文、文件名、路径、正文或错误文本哈希。

文件大小、路径安全、格式解析、上传清理和界面错误展示策略均保持原有行为。
`other` 仍是统计分类，并不代表文件不受支持。历史记录已丢失的具体原因无法
由本修复追溯恢复。

验证：`node --test tests/attachment_telemetry.test.mjs` 使用真实桌面 feature、
Web 实际函数和共享上传器，原生端口受控；覆盖分类、读取/分块/提交/结果故障、
成功及取消幂等，并验证事件可由真实 Relay v1 schema 接收。它不等于真实磁盘
故障、原生 IPC 或现场文件复现。
