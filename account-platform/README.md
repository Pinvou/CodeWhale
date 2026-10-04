# Pinvou Account Platform

账号平台是独立于当前生产 New API 的新服务集合。架构与实施基线见：

- [`../docs/账号组织Token与计费系统-架构设计.md`](../docs/账号组织Token与计费系统-架构设计.md)
- [`../docs/账号组织Token与计费系统-实施任务与验证计划.md`](../docs/账号组织Token与计费系统-实施任务与验证计划.md)
- [`docs/adr/`](docs/adr/)

## 服务

- `account-service`：用户、组织、成员、角色和活动上下文。
- `billing-service`：钱包、账本、支付、预占、退款和对账。
- `token-gateway`：Token 授权、路由、New API 代理和计费编排。
- `payment-simulator`：仅开发/测试使用的三渠道支付模拟器。

当前已实现账号/组织与会话授权、钱包与平衡账本、预占结算、三渠道支付模拟、退款与对账基础、Token 凭据加密轮换、确定性路由、OpenAI 兼容 JSON/SSE 代理、充值 BFF 和本地 Mock New API。桌面端、Web Portal、生产 Helm、真实支付与真实 New API 仍按实施计划的后续门禁推进。

## Token Gateway 安全默认值

- `TOKEN_LOCAL_KEK_BASE64` 必须解码为 32 字节，仅允许开发/测试环境使用。
- `production`/`production-new` 会拒绝本地密钥 Provider，必须接入 KMS/Vault 实现。
- `TOKEN_PROVIDER_ALLOWED_HOSTS` 是逗号分隔的精确主机白名单；生产环境同时强制 HTTPS。
- Gateway 不重试模型调用。上游结果不确定、流式断开或缺失 usage 时保留 Hold 并进入 `REVIEW`。
- Hold 到期进入 `REVIEW` 并保留预占；明确的用量结算或未消费释放才能结束该预占。
- `TOKEN_GATEWAY_MAX_INPUT_TOKENS` 是计算预占的输入规模假设，当前不执行 tokenizer 计数。省略输出上限时 Gateway 会向上游补入默认 `max_tokens`；若实际用量费用超过预占，结算进入 `REVIEW`，不会超额扣款。真实模型的输入规模仍需后续供应商验收确认。
- Keycloak API 凭据须为 `typ=Bearer` 的已验签 access token；ID/Refresh token 不可代替。共享 JWKS 获取最多等待 5 秒，失败后可重新获取。
- 支付回调地址由 Billing 的 `BILLING_PAYMENT_CALLBACK_URL` 固定配置，客户端不能提交回调 URL。
- 模拟支付重试保持通知 ID 和付款事实不变，仅刷新签名时间；Billing 保留 5 分钟验签窗口，重复通知只允许 `timestamp` 改变。

`mock-newapi` 只属于开发 Compose 后端网络，不发布宿主机端口；可用于验证正常、限流、断流和缺失 usage 场景。

## 本地命令

统一从仓库根执行：

```text
node scripts/account-platform.mjs verify
node scripts/account-platform.mjs dev up
node scripts/account-platform.mjs dev down
```

真实 New API 和真实支付测试需要明确 opt-in，普通测试不读取真实凭据或访问生产服务。

本地验证需要 Go 1.26.6 和 Node.js；契约测试检查生产 Go 类型序列化、注册路由请求校验及 Token eligible/错误响应。Token 仓储和上下文使用 fixture，这部分不执行付款或数据库端到端链路。设置 `PINVOU_ACCOUNT_TEST_DATABASE_URL` 可启用真实 PostgreSQL 回归，连接必须指向 loopback 测试实例；每个测试创建并清理独占随机 schema。未设置时这些数据库用例会明确跳过，不能将其算作数据库验证通过。CI 配置了该变量，实际执行这些用例。
