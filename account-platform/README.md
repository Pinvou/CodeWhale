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
- 支付回调地址由 Billing 的 `BILLING_PAYMENT_CALLBACK_URL` 固定配置，客户端不能提交回调 URL。

`mock-newapi` 只属于开发 Compose 后端网络，不发布宿主机端口；可用于验证正常、限流、断流和缺失 usage 场景。

## 本地命令

统一从仓库根执行：

```text
node scripts/account-platform.mjs verify
node scripts/account-platform.mjs dev up
node scripts/account-platform.mjs dev down
```

真实 New API 和真实支付测试需要明确 opt-in，普通测试不读取真实凭据或访问生产服务。
