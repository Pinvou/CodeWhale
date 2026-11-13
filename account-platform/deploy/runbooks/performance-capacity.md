# 生产等价性能与容量验收

以 `account-platform/deploy/performance-policy.json` 为机器目标，在与生产相同 CPU、内存、网络、数据库参数、服务副本和依赖档位的隔离环境执行。冻结发布 SHA、数据集 SHA、负载脚本/Provider 模拟器版本和容量声明；稳态至少30分钟，普通指标每类至少10,000样本，流式至少1,000样本，负载不得超过声明容量70%。

报告必须分别记录平台、Provider、排队和客户端网络耗时，不得删除超时/错误样本。验证关键读写 P95/P99、登录/短信/支付外部依赖、首个非空模型内容事件、200并发流、100,000有效Hold、百万订单/三百万支付流水，以及每关键流百万未确认事件和十万积压在50%峰值新流量下15分钟排空。

容量测试同时验证幂等和守恒：重复请求不增加Hold、不重复履约/入账；达到硬容量时在创建Hold和调用Provider前公平拒绝；事件达到高水位会停止相关高风险新写入，不能丢弃或覆盖未消费事件。

Billing 管理分页使用至少 1,000,000 条支付订单的生产等价数据单独验收：在并发创建和状态更新持续发生时创建 15 分钟资源 ID 快照并完整翻页，记录首个快照查询及成员写入 P99，要求不超过 10 秒，且重复成员和遗漏成员均为 0。报告把 `payment_orders`、`pages`（200 条上限时至少 5,000 页）、`members_seen`、`concurrent_creates`、`concurrent_updates`、`first_snapshot_p99_ms`、`duplicate_members`、`omitted_members` 写入 `admin_pagination`，并附完整逐页轨迹与并发变更轨迹的 `page_trace_sha256`、`mutation_trace_sha256`；超时、截断结果、仅测首页或缺少并发变更轨迹均不得作为发布证据。

保存不含业务正文或凭据的聚合报告并执行：

```sh
node scripts/account-platform-performance-gate.mjs --require-evidence "$EVIDENCE_EXPORT_ROOT/performance-evidence.json" \
  --expected-release-sha256="$CANDIDATE_RELEASE_SHA256" \
  --trust-file="$CONTROLLED_TRUST_FILE" --artifact-root="$EVIDENCE_EXPORT_ROOT" \
  --deployment-id="$EXPECTED_DEPLOYMENT_ID" \
  --page-trace="$EVIDENCE_EXPORT_ROOT/admin-pagination-pages.ndjson" \
  --mutation-trace="$EVIDENCE_EXPORT_ROOT/admin-pagination-mutations.ndjson"
```

生产发布命令固定启用 `--require-evidence`，并由发布流水线把候选镜像或制品的可信 SHA-256 作为 `CANDIDATE_RELEASE_SHA256` 传入；证据中的 `release_sha256` 必须与其完全一致。命令会直接读取逐页轨迹和并发变更轨迹并重算摘要，任一文件缺失、摘要不符或未提供证据都必须失败。日常静态校验只运行 `check:performance-policy`，不得把策略文件本身冒充容量证据。

分页发布前仍必须取得真实事务分页、并发成员快照和完整遍历的现场证据。历史 `TestAdminPaginationPostgresIntegration` 测试名当前不存在；不能运行它或把现有只读单元测试冒充百万订单容量验收。本仓库当前没有该现场压测载体，未取得受控真实测量与独立审批前保持性能发布未验收。禁止指向含业务数据的共享数据库运行 fixture。

低于样本量、非生产等价资源、超过70%声明容量或缺少任一计时分段的结果均不能作为发布证据。

## PostgreSQL 连接容量采样

在授权的容量演练中，使用站点已核对的只读连接服务和预期数据库名，按固定间隔采集[连接样本](sql/postgres-connection-capacity-sample.sql)。从仓库根目录执行。连接服务负责 TLS 主机校验和凭据投递，采样只需要连接权限和默认统计视图读取权限。

```sh
psql -X --no-password --quiet --csv \
  --dbname="service=$APPROVED_CAPACITY_AUDIT_SERVICE connect_timeout=5" \
  --set=expected_database="$EXPECTED_CAPACITY_DATABASE" \
  --file=account-platform/deploy/runbooks/sql/postgres-connection-capacity-sample.sql
```

站点使用已核对的单一连接服务，服务名使用不含空格的配置项。采样调度器还应对整个 `psql` 进程设置截止时间，覆盖域名解析、连接与查询，并使截止时间小于采样间隔。将采样结果、退出码和缺失区间与候选制品、部署标识及测试时间绑定。每个样本报告数据库端的连接上限、两类预留槽、当前服务器及目标数据库的已连库后端数，排除采样会话。多库共享一个服务器时，服务器计数只能记一次。数据库重启或切换时分段记录。目标不符、超时或统计视图不可读时保留失败记录，不填零。

统计使用 `pg_stat_database.numbackends`。它包含连接到数据库的后端进程，涵盖客户端和已连库的后台工作进程。计算连接预算时另取客户端连接和应用池测量，以固定镜像的应用池配置、副本上界及迁移和运维预留核对。并发重连、代理或连接池和负载端也分别留证。现场容量结论仍须满足上述完整演练和独立复核。

## 工件验证限制

命令从仓库根目录执行；策略或字段格式通过、工件摘要绑定和真实演练/发布审批是不同结论。现有可信来源、全部原始工件和独立审批不可省略，详见 [运维证据](operational-evidence.md)。
