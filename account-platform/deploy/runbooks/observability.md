# 可观测性与告警响应

HTTP 服务在独立受限的 `:9090/metrics` 暴露低基数 Prometheus 指标，并通过
W3C `traceparent` 传播 Trace。Outbox 固化 Trace ID，Relay 发布和 NATS Consumer
恢复父 Trace，后台任务建立任务 Span。`OTEL_EXPORTER_OTLP_ENDPOINT` 在
`production-new` 必须为 HTTPS；开发环境可留空以关闭导出。

日志使用固定 JSON schema：`time/level/service/environment/version/event/result`，
请求或任务按需附加 `request_id/trace_id`。事件和字段均走白名单；未知动态消息折叠，
原始错误、密码、验证码、Token、Cookie、请求/响应正文、事件 Payload 与模型正文
不会序列化。每次发布先运行：

```text
npm --prefix account-platform run check:observability-release
```

指标只使用 `service/method/route/status/source/outcome` 标签。禁止把用户 ID、组织名、
钱包、订单、Token、Usage Request ID、二维码或任意凭据加入指标标签。
Trace ID 只能出现在 Exemplar。

## 关联查询

1. 从客户端错误信封或响应头取得 `X-Request-ID`、`X-Trace-ID`。
2. 在 Trace 后端按 Trace ID 定位入口 Span，再查看 Account/Billing/Token 的
   下游调用。
3. 在集中日志中按 `trace_id` 查询；支付和用量资源定位转到受权限控制的领域证据或
   审计查询，不把订单、用户或 Usage Request ID 扩散到通用日志。

## 健康分层

- `/health/live` 只反映进程是否可运行，依赖断开不应使 liveness 失败。
- `/health/ready` 验证安全处理请求所需的关键依赖；Identity 验证 Keycloak，三个
  Relay 同时验证各自 PostgreSQL 与 NATS。失败必须返回503。
- 普通 readiness 不调用支付、模型计费或 TOK-37 客户端切换检查。

## P1 响应

- 5xx 比例或 p95 告警：按 `service` 和路由模板定位，确认依赖 Readiness、
  数据库连接、上游超时和最近部署。
- Outbox 告警：执行 [事件投递 Runbook](event-delivery.md)，不得删除事件或
  直接改业务表。
- 死信告警：在修复根因和双人复核前不得重放资金/用量事件。

Grafana 可导入 `deploy/dashboards/account-platform-overview.json`。Helm 在
启用 `observability.prometheusRulesEnabled` 时创建 P1 规则；集群必须先安装
Prometheus Operator CRD，并确保监控命名空间/Pod 标签与 NetworkPolicy
selectors 一致。
