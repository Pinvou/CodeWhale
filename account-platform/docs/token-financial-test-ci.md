# Token 财务 PostgreSQL 验证

`account-platform-test` 在 Draft、Ready 和 Merge Queue 中，先保留所有 Go
模块的测试和 vet，再通过 `scripts/test-token-financial-postgres.py` 执行
Token 全包及定向 Billing 追加预留契约的实际 PostgreSQL 验证。PostgreSQL 使用与已有 Hub 门禁相同的固定镜像，
端口仅绑定 runner 的 `127.0.0.1`。无需部署凭据或外部支付、供应商、issuer、NATS。

执行器只接受显式端口、无密码的 loopback `/postgres` 连接，并先只读确认实例仅有
默认数据库、唯一 bootstrap 用户和无用户对象。它按既有部署合同创建三个 Billing
角色；正式测试自行创建 UUID 数据库和 Token 角色，安装原生产迁移并执行运行时操作。
执行后仅在数据库及角色集恢复到预期时删除本次 Billing 角色；存在未知遗留时拒绝清理，
不强制删除数据库，不修改任何迁移身份或 SQL。

全包使用 `-race -count=1 -mod=readonly`，明确要求 19 个资金、取消、幂等、归属和
内容留存核心套件实际通过，再以相同模式执行 4 个 Billing 顶层套件：追加操作幂等、
个人 hold 边界、锁后时钟和过期续租。两阶段之间及最终必须恢复数据库、角色和客户端边界。
结果按实际包名核验，不能以其他包的同名用例代替。零用例、缺少套件、核心子用例跳过或任何测试失败均使门禁
失败。Go 的真实输出保留；其他需要外部服务的测试如有条件跳过，仍按跳过披露。
此验证不代表真实收款、真实模型提供方或部署验收。

必跑套件包括缺引用截止恢复、派发所有者续租和不完整错误响应持久化。
也包括真实 Provisioner 的当前上下文资格与目录/后台 HTTP 接线，以及使用原迁移
验证 legacy 状态修复和可信拒绝响应的审计词表、幂等及资金事实。
派发竞争测试使用受控时钟与隔离库中的锁屏障，不冒真实等待两小时；
错误响应套件验证真实 Token 持久化，但其中 Billing 为受控依赖，不能冒资金余额验收。
追加预留套件覆盖真实 Billing HTTP 提交后 ACK 丢失、原操作 ID 重放及本地上限单调投影；
协议边界见 [追加操作幂等](hold-extension-idempotency.md)。

本地重现需要仓库指定 Go、Python 3、`psql` 及一个独占的一次性 PostgreSQL 16+
实例。下面的固定容器和端口专用于测试；同名容器已存在时先确认归属，勿复用未知实例。

```bash
docker run --rm -d --name pinvou-token-financial-ci \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -p 127.0.0.1:5433:5432 \
  mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
# 等待 pg_isready -U postgres -d postgres 通过，再执行：
python3 -m unittest discover -s scripts/tests -p test_token_financial_postgres.py
PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable' \
  python3 scripts/test-token-financial-postgres.py
# 仅停止上面本次创建的容器：
docker stop pinvou-token-financial-ci
```

非 PATH 工具可通过 `PINVOU_ACCOUNT_TEST_GO`、`PINVOU_ACCOUNT_TEST_PSQL` 指定。
执行器清除继承的 `PINVOU_TEST_*`，仅向正式 Token 测试注入本次独占实例的两个
数据库连接，并向定向 Billing 阶段提供同一隔离实例，避免其他本地或部署连接进入此门禁。
