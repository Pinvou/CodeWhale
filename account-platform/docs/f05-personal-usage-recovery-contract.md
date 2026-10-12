# F05 个人用量与取消回执

个人用量列表和详情按已持久化的用户身份及个人资源范围授权：目标 `user_id` 必须匹配，`organization_id` 必须为空。历史详情不绑定创建调用时的登录会话，因此重新登录后仍可查看自己在列表中看到的个人记录。原调用的冻结 session、供应商及幂等重放行为保持不变；此处不扩大组织调用能力。

`GET /v1/me/usage-requests`、`GET /v1/me/usage-turns` 和 `GET /v1/usage-requests/{usage_request_id}` 继续要求个人上下文及 `personal.usage.read`。详情原有状态、金额、序列及时间字段保持兼容。另一用户或组织资源返回 404。

`POST /v1/usage-requests/{usage_request_id}/cancel` 使用 Bearer 用户身份、`personal.usage.cancel` 和稳定的取消 `Idempotency-Key`（16–128 字符），无请求体。当前切换到组织上下文不会阻断停止既有个人调用；仓储在与派发共用的行锁下再次验证目标用户及空组织范围。派发后的取消仅停止投递，返回 `UNSUPPORTED` 等真实状态，不能推断供应商执行停止或释放预约资金。未派发取消仍复用持久取消、可信预约恢复及释放确认流程。200 是当前持久回执；202 表示释放回执未知，应继续查询并沿原命令恢复。

## 丢失 usage header 的只读恢复

成功接受调用时，模型接口提供 `X-Pinvou-Usage-Request-ID`。丢失该响应头时，客户端可使用原调用身份查询：

- `GET /v1/usage-requests/by-idempotency-key`
- `Authorization: Bearer <用户身份>`
- `Idempotency-Key: <原模型调用键>`，16–128 字符，服务端去除两端空白后计算原 SHA-256 身份。
- 权限为 `personal.usage.cancel`；当前上下文选择不影响个人目标的回执恢复。

200 仅返回 `usage_request_id`、`execution_status`、`delivery_status`、`cancel_status`、`settlement_status`，不返回模型请求内容、凭据或密钥。目标必须属于当前用户，且组织为空。此 GET 不写取消命令、不执行模型调用、不释放资金；响应为 `Cache-Control: no-store`。

404 `TOKEN_NOT_FOUND` 只表示当前无法找到属于该用户的个人调用事实，不能证明原请求没有效果。客户端须保留原调用及取消身份，继续查询或提示未知状态；不得通过重发模型 POST、创建新键或猜测 usage ID 恢复。权限不足、无效键及暂时错误也不提供无效果保证。

## 管理员集合与预约到期

管理员分页关闭时的用量集合使用与分页集合相同的安全摘要，保留用量 ID、模型、执行/投递/取消/结算状态、金额和必要时间；不输出用户会话、钱包、凭据、供应商请求、请求指纹或原始证据。详情与证据继续使用分别授权的入口。

到期时间不证明未派发。旧 `ExpireHolds` 内部入口显式返回 `ErrHoldExpiryProofRequired`，不释放资金；真实后台仅使用带可信 Token 证明的恢复 worker。没有可信未派发证明的过期预约保留 ACTIVE 状态及预约余额，并登记独立 MANUAL_REVIEW 恢复记录；可信释放已提交但回执丢失时，原持久恢复记录继续确认该 RELEASED 预约，不重复放款。已过期预约续租返回 409 `BILLING_CONFLICT`，数据库时钟围栏拒绝时，金额分配和租约事件一并回滚。只有已持久阻断派发、可信释放及本地确认闭环才能完成恢复。

流式逐 chunk 查询的性能观察跟踪于 Issue #1595；本次保留既有取消投递检查。
