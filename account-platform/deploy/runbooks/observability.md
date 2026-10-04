# 可观测性与告警响应

HTTP 服务在独立指标监听器（默认 `9090`）的 `/metrics` 暴露低基数 Prometheus 指标，
应用入口不开放该路径，并通过 W3C
`traceparent` 传播 Trace。`OTEL_EXPORTER_OTLP_ENDPOINT` 在
`production-new` 必须为 HTTPS；开发环境可留空以关闭导出。日志使用 JSON，
每条 HTTP 完成日志包含 `request_id`、`trace_id`、路由模板、状态码和耗时。

指标只使用 `service/method/route/status/source` 标签。禁止把用户 ID、组织名、
钱包、订单、Token、Usage Request ID、二维码或任意凭据加入指标标签。
标准 HTTP 方法保留原指标值，任意扩展方法统一计为 `OTHER`；请求处理和日志仍
保留原方法。Relay 缺失告警按 `account/billing/token` 三个 `source` 去重，全部
无序列时补零；增加同域副本不能掩盖其它域缺失，原 `for: 5m` 和 P1 等级不变。
本套规则与 dashboard 沿用单部署的 scrape/query 作用域，指标没有
release/environment 标签。共享 Prometheus 同时采集多个部署时，必须先在外部
采集或查询配置中隔离作用域，并重新验证缺域/全部缺失告警；不能将其它部署的
同域序列算入本部署，也不能把本套规则与 dashboard 声明为已完成多部署监控验收。

## 关联查询

1. 从客户端错误信封或响应头取得 `X-Request-ID`、`X-Trace-ID`。
2. 在 Trace 后端按 Trace ID 定位入口 Span，再查看 Account/Billing/Token 的
   下游调用。
3. 在集中日志中按 `trace_id` 查询；支付和用量排障再使用受控字段
   `order_id` 或 `usage_request_id`，不得在无权限系统扩散这些标识。

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

## 规则回归

从实际 Helm 渲染结果准备新临时目录，然后用 Prometheus `promtool` 检查和执行
五组缺失 Relay 用例（零、一、二、三域和缺域多副本）。这里不会启动 Prometheus，
不会连接真实指标或发送告警；纯规则测试不能代替集群监控接线和首次安装验收。

```sh
helm template pinvou account-platform/deploy/helm/account-platform \
  -f account-platform/deploy/helm/account-platform/values-production-new.yaml > rendered.yaml
node account-platform/test/integration/prometheus-rule-fixtures.mjs \
  rendered.yaml /tmp/pinvou-new-rule-fixture
promtool check rules /tmp/pinvou-new-rule-fixture/event-relay-rules.yaml
promtool test rules /tmp/pinvou-new-rule-fixture/event-relay-missing.test.yaml
```
