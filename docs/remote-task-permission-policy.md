# 远程任务的 B 端权限模式

远程任务不会继承 B 的普通聊天或代码页面默认权限。B 必须在本地 `settings.json` 明确配置每条远程任务通道；没有配置的通道拒绝创建和执行新任务。A 的请求不包含可升级权限的配置字段。

关闭 B 应用后，保留已有设置，在顶层加入下面的对象，然后重新启动 B：

```json
"remote_task_permissions": {
  "chat": "plan",
  "code": "plan"
}
```

可选值仅为 `plan` 和 `yolo`。省略对象、省略通道或使用 `null` 均不授权该通道；未知值只禁用对应远程通道，不回退到自动执行或重置其他本地偏好。配置失败会通过现有三语远程任务错误界面显示；诊断码 `task_permission_mode_required` 表示该通道未明确授权，`task_permission_mode_changed` 表示当前会话模式与 B 配置不一致，`task_permission_mode_unsupported` 表示 adapter 能力、模式配置或返回值不符合契约。原始本地路径和 provider 错误不发送给 A。

- chat 的 `plan`/`yolo` 复用 CodeWhale 的 Plan/Agent 模式和既有工具权限规则。
- code 仅支持可识别的 `@agentclientprotocol/codex-acp` 1.6.2 包及其明确报告的模式选项。`plan` 映射到 `read-only`（readOnly 沙箱、关闭网络、on-request 审批）；`yolo` 映射到受限 `agent`（workspaceWrite 沙箱、关闭网络、on-request 审批），**不映射到** `agent-full-access`（never 审批、dangerFullAccess）。这些语义来自该版本的 AgentMode 契约，不能推用于其他 ACP provider、任意替代二进制或未知版本。
- code 的配置只作用于当前精确会话，不更新 B 的普通 ACP 全局默认。provider 不支持指定选项、配置失败或返回值不匹配时，不发送 prompt。
- provider 发出的权限请求继续由已有远程权限 owner 和 A 的批准/拒绝链处理；B 的显式模式不能替代 A 对这些请求的决定。on-request 并不表示每个读取或沙箱内操作都必须产生独立审批。

创建任务时，明确模式严格持久化到该会话。执行前必须仍与 B 配置一致，不能把旧会话静默提升到新模式。调整配置会拒绝后续不匹配的执行；要使用新模式，应创建新任务。已经进入执行的合法回调、取消收尾和历史记录继续按原 owner 处理，不清除或重新派发旧代际任务。

此设置不授予新工作区访问权，不扩大 A/B 身份授权、能力清单或工具范围。修改其他权限或撤销设备仍遵循各自原有流程。
