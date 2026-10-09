# 开发支付模拟器

此提供商始终声明 `test_only=true`。付款、退款和账单只用于开发验证，不能据此开放正式收款。正式收款准入由 Billing 的真实提供商能力与准入门禁决定。

从 `account-platform/` 使用现有迁移命令安装该域，再启动服务：

```sh
go run ./cmd/migrate --dir migrations/payment-simulator
go run ./cmd/payment-simulator
```

由受控环境提供 `DATABASE_URL`、`PAYMENT_SIM_PUBLIC_URL`、`PAYMENT_SIM_PAY_TOKEN_SECRET` 和三个 `PAYMENT_SIM_HMAC_UNIONPAY`、`PAYMENT_SIM_HMAC_ALIPAY`、`PAYMENT_SIM_HMAC_WECHAT`。付款链接密钥和各渠道 HMAC 密钥至少 32 字符。内部接口使用现有 workload 身份配置；开发或测试可显式提供 `INTERNAL_SERVICE_TOKEN`。客户端支持 `WORKLOAD_IDENTITY_TOKEN_FILE` 的逐请求重读，不将令牌写入日志。服务通用日志、监听、可信时钟及可观测配置沿用已有宿主要求。

创建请求中的 `merchant_customer_key` 是冻结的真实客户 UUID；`mobile`、`external_sku` 与客户标识一并耐久存储。相同商户订单号重试只接受相同付款事实与回调目标。查询返回 `external_customer_key`、`external_sku`，不提供旧开发字段别名。客户映射仅在鉴权后的内部 JSON 中返回，公开付款页面不显示这些字段。

通知保持原 `notification_id` 和全部付款事实，重投时仅刷新签名覆盖的发送时间。模拟器继续保留故意错误金额、币种、商户和签名等既有失败场景。消费者须对同 ID 的付款事实做严格幂等核对；时间刷新不等于新付款。

账单参数为 `YYYY-MM-DD` 业务日期标签，范围为上海自然日的半开区间。成功付款保留原交易引用、金额和付款时刻；成功退款形成独立 `REFUND` 行，使用真实退款引用、退款金额和成功时刻。退款不会覆盖 `PAYMENT` 行，失败退款不会出现在成功账单中。该接口不执行或导入每日四方对账。

真实 PG 回归使用 `PINVOU_TEST_PAYMENT_SIM_DATABASE_URL`，未提供时可使用 `PINVOU_TEST_ACCOUNT_DATABASE_URL`。测试只创建随机独有 schema，通过生产迁移包装器安装并在结束时核对清理；没有配置时会明确跳过。受控 HTTP/PG 测试不代表真实供应商、部署或整套 E2E 验收。
