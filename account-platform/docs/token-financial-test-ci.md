# Token 财务 PostgreSQL 验证

`account-platform-test` 在 Draft、Ready 和 Merge Queue 中，先保留所有 Go
模块的测试和 vet，再通过 `scripts/test-token-financial-postgres.py` 执行
Token 全包和 F06 组织状态 CAS、既有组织钱包、成员额度及追加预留的定向 PostgreSQL 验证。PostgreSQL
使用与已有 Hub 门禁相同的固定镜像，端口仅绑定 runner 的 `127.0.0.1`。无需部署凭据
或外部支付、供应商、issuer、NATS。

执行器只接受显式端口、无密码的 loopback `/postgres` 连接，先只读确认默认三个
数据库、唯一 bootstrap 用户、无用户对象且没有其他客户端连接，然后依次执行：

1. `owner-account`：运行组织状态投影版本 CAS 与原组织删除版本根。
   正式 Account fixture 自行创建和清理八个受限角色，不复用其他阶段的角色。
2. `owner-wallet`：单独运行原 `TestF06OrganizationWalletRequiresLifecycleEvidence`。
   该正式测试仅在 Billing 角色不存在时创建、使用并清理自己的角色，拒绝复用未知角色。
3. 核实数据库、角色、对象及连接全部还原后，按既有合同创建三个受限 Billing 角色，
   执行 `retained-token`：Token 全包和 20 个必跑根（原 18 根、组织准入临界根及追加预留投影根）。测试自行创建 UUID 数据库和
   Token 角色，安装原生产迁移并通过真实运行时仓储操作。
4. 再次核实当前阶段的角色、数据库、对象和连接集合后，执行 `retained-billing`：
   原 `TestMemberHoldIdentityMutationBoundaries`、`TestPersonalUsageHoldBoundaries`，
   加上 `TestHoldExtensionIdempotencePostgres`、`TestPersonalUsageHoldClockBoundaries` 与
   `TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved`，共五根。
   这些定向契约验证成员额度、个人钱包及追加幂等/时钟边界，不是 Billing 全包或完整支付验收。

四个阶段沿同一个公开入口和现有 CI 步骤执行，保留 `-race -count=1 -mod=readonly`
与既有 20 分钟 Go 超时。每阶段校验实际包、必跑根的 run/pass 和包成功终态；
Account、钱包和有子用例的四个定向 Billing 根还要求实际命中子用例；既有过期续租根是
单个叶测试，仍须有实际 run/pass 且不可跳过。任何失败、零用例、缺失根、
必跑子用例跳过或截断结果都会停止后续阶段。Token 其余需要外部服务的条件跳过仍
保留在真实输出中并单独披露。原 `f06fixture` 私有两子组诊断不代表此公开协调入口。

状态 CAS 只将内部读取版本传给同一事务的完成操作；仅已确认的目标状态和版本可无写重放。
真实 HTTP 与 PostgreSQL 回归在远端 ACK 和本地完成之间执行合法重命名，验证旧确认不能覆盖
新版本。远端依赖是受控 HTTP authority，此测试不声称跨域原子性，也不自动补偿已确认的远端状态。
组织准入根以真实 `BeginUsage`、有限分散用户/模型/路由和原完成操作，验证每分钟 599→600
允许/601 拒绝、并发 99→100 允许/101 拒绝，以及另一组织不受影响。用户与组织成员的并发
上限同为 5，成员计数是用户计数的子集，因此 4→5/5→6 验证二者合并边界，不冒称独立命中
成员谓词。拒绝及原键重放均核对无新增 usage/派发/资金变化；此路径尚未开始供应商派发。

结束时仅在数据库、角色、对象和连接恢复到预期、且本次角色没有外部依赖时，
事务性删除本次明确创建的三个 Billing 角色。未知遗留或外部依赖使门禁失败并保留现场；
不强制删除数据库、终止会话或修改任何迁移身份及 SQL。此验证不代表真实收款、
真实模型提供方或部署验收。

必跑套件包括缺引用截止恢复、派发所有者续租和不完整错误响应持久化。
也包括真实 Provisioner 的当前上下文资格与目录/后台 HTTP 接线，以及使用原迁移
验证 legacy 状态修复和可信拒绝响应的审计词表、幂等及资金事实。
派发竞争测试使用受控时钟与隔离库中的锁屏障，不冒真实等待两小时；
错误响应套件验证真实 Token 持久化，但其中 Billing 为受控依赖，不能冒资金余额验收。
追加预留套件覆盖真实 Billing HTTP 提交后 ACK 丢失、原操作 ID 重放及本地上限单调投影；
协议边界见 [追加操作幂等](hold-extension-idempotency.md)。

主动取消仍是既有个人资源边界：`personal.usage.cancel` 不能取消持久化的组织请求；
当前选择组织上下文也不改变已属于该用户的个人请求归属。
`TestF05PersonalHistoryAndCancellationPersistedOwnerScope` 使用真实 PostgreSQL 验证
组织请求 404 且无写入。F06 的个人 HTTP 测试遵守同一拒绝边界；另一个直接调用共享
`finishPreDispatchCancellation` 的测试只验证已授权内部恢复遇成员版本冲突仍保留
不确定证据、不会盲目释放资金，不表示向组织用户提供主动取消入口。

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
执行器清除继承的 `PINVOU_TEST_*`，仅向各阶段正式测试注入其所需的本次独占实例
数据库连接，避免其他本地或部署连接进入此门禁。
