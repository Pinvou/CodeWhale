# 撤回审批的原操作确认

新浏览器 WITHDRAW 意图在原 `expected_version` 请求中明确发送
`command_protocol: 1`，并保留原 `X-Pinvou-Approval-Action-Control-Version`。
仅 WITHDRAW 使用该协议；SUBMIT、APPROVE、REJECT 及其他管理动作不附加此版本。
旧 pending 缺少版本时不能自动升级或派发。旧 Account 与 Portal 的严格请求
解码拒绝新字段；不存在降级为旧写入的 fallback。

Account 在原状态 CAS 同一事务的不可变 `approval_state_events` 中记录原 actor、
expected version、完整既有意图指纹、原控制版本和完成版本。只有这些证据全部
匹配才可确认该次撤回。旧 `{}` 事件和单独的当前 WITHDRAWN 状态不能证明原调用。
同控制版本重放原 POST 可返回同一成功事实，不追加冲突事件或再次执行审批。

响应丢失后，浏览器可只读访问版本中立 BFF：

```
GET /api/admin/approvals/requests/{id}/actions/withdraw-result?command_protocol=1&expected_version=N&origin_action_control_version=C
X-Pinvou-Approval-Action-Control-Version: 当前授权的控制版本
```

Portal 的冻结 Session wire 选择 `/admin/v1/` 或 `/admin/v2/` 的同名只读接口。
该查询保持原 N/C，不更新原 POST 的 CAS 或控制版本。Account 用当前 human、
visibility、requester、strong platform authorization 与 SAFE-close 控制门验证，
再读取精确原事件，只返回操作和事件身份、原/结果版本及时间等最小证明。它不恢复
旧审批推进能力、不产生授权事件、不调用财务执行器，也不添加原 WITHDRAW 没有的
推进 recent-auth 策略；SUBMIT/APPROVE 的原有近期认证要求不变。

跨控制版本只允许该只读确认，原 stale POST 仍拒绝。读取不可用、403、404、未找到
精确证据或会话/权限代际退休都不能证明原操作未执行：保留 unknown 和原事实，
不自动换键、改 CAS、重发旧写或标记保存成功。旧后端没有结果路由时同样保持未知。

测试入口：`test:account-platform-finance` 与 `test:account-platform-finance-ui`；
严格财务 PG runner 的 Account phase 强制选中
`TestApprovalWithdrawalOriginalReceiptPostgres`，必须运行真实根和子项，skip/零子项失败。
本修复不新增迁移、不回填旧事件，也不改变 APPROVE/REJECT 的未知恢复协议。
