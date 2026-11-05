# 原审批决定的只读结果确认

正常动作 `APPROVE`/`REJECT` 在 client 运输边界映射为已有 wire enum `APPROVED`/`REJECTED`，见 `approval-decision-wire-contract.md`。此文描述独立的丢失 ACK 恢复协议。

新决定只在创建新 intent 时冻结 `commandProtocol: 1`。v1/v2 POST 均携带 `command_protocol: 1`、原 `expected_version`、规范 decision、原 JSON reason；当前 control header 必须与原 action control 相符。旧 pending 缺 marker 不升级，未知结果不修改原 CAS、理由或 origin control，不自动派发替代操作。

决定、原请求 CAS、authorization/outbox 和原 immutable state event 仍在同一既有事务。新 event 额外绑定原完整 request fingerprint、actor、decision ID、原 expected/result version、origin control、规范 decision，以及 **JSON 解码后原始 reason UTF-8 字节**的 SHA-256。领域 reason 既有规范化不变。旧只含 reason 的事件不是协议回执，不能补写成历史成功。

`GET /api/admin/approvals/requests/:id/actions/decision-result` 由 Portal 转发到当前会话固定的 Account v1/v2 只读路由。query 严格只有 `command_protocol=1`、`expected_version`、`origin_action_control_version`、`decision`、`reason_digest`；原始 reason 不进入 URL。header 指明当前控制版本。当前人类身份、visibility、非 requester、strong approver permission 和控制状态均须重新验证。此 GET 不赋予旧 control 的推进能力，不新增 recent-auth：原 Token 推进/拒绝和双人审批规则保持在原写链。

只有同一 immutable event/decision/actor/CAS/fingerprint/reason/control 元组可证明原决定。其请求后来进入 EXECUTING、EXECUTION_FAILED 或其它后续状态不否定该事实，也不能仅凭终态推断成功。返回最小 proof ID/版本/digest，无私有原命令、凭据或原 reason。GET 失败、404、未找到或字段不匹配都保持 UNKNOWN，不证明未执行，也无 fallback POST。

同控制的原 POST 可返回精确已提交事实而不追加冲突 event、重新 proof 或 authorization。已派发期间被其它操作替换 live control 时，迟到结果不自动刷新/退休新 tuple，保留原 UNKNOWN；明确重试可在当前合法控制下只读确认原 proof。logout、CSRF/authorization/session 代际退休后的迟到 proof 不清除旧 unknown 或更新新身份。

新 body marker 被旧 Account/Portal 严格 decoder 拒绝，旧 GET 路由不可用；客户端没有旧写回退。部署混版时失败关闭，不承诺旧 pending 历史恢复。此实现不改变资金执行、退款/撤销策略、审批 SLA、迁移身份或工作负载权限。


专用 approval writer 保持原INSERT/授权函数权限，不拥有 decisions/state_events 的 SELECT。在 LEGACY 的并发 CAS-mismatch 中，新协议先回滚 writer 事务，外层仅针对原合法冲突/原请求改变错误，才调用独立 runtime reader 重新核当前授权、控制及精确原 proof。缺 proof 不推断成功，不在 writer 活跃事务中借 runtime 读取，也不扩大 writer GRANT。已有 PROOF_REQUIRED 路径和原唯一约束/原成功事务保持。
