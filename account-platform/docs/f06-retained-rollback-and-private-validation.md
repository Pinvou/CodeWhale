# F06 保留迁移回滚与私有验证边界

000039 的原回滚 SQL 对返回 `void` 的范围版本函数使用 `DISTINCT`，PostgreSQL 在零行时也会因缺少 `void` 相等算子拒绝计划。迁移文件、名称及校验值保留；Account 正式回滚入口只对源文件和已安装账本均精确匹配的 39 使用执行适配：复用原 Up 的四列角色绑定去重查询，再逐绑定调用失效函数，其余 Down 语句保持原逻辑。合法的同源 LF/CRLF 历史校验值均可识别，未知身份在业务变更前拒绝。

权限撤销、授权版本更新、策略变更和 39 的账本移除仍在同一事务，失败全部回滚。已有审批引用第二版策略时，原外键仍拒绝回滚并保留历史；不能删除审批或强行撤销约束。其他保留数据迁移的回滚保护和高版本 forward-only 限制不变，不能据此直接将最新数据库降至 39，也不宣称全链可回退。需要回退时先核实实际最高版本、备份、审批引用和应用兼容性。

## 留存库只读核查与恢复验收

从仓库根目录使用已获授权的站点只读身份，对每个需要保留数据的 Account 数据库各执行一次[核查 SQL](../deploy/runbooks/sql/account-039-rollback-inventory.sql)。连接服务名和预期库名来自已复核的站点清单。站点服务定义负责验证 TLS、主机和连接身份，凭据由受控渠道提供。

```sh
psql -X --no-password --dbname="service=$APPROVED_ACCOUNT_AUDIT_SERVICE" \
  --set=expected_database="$EXPECTED_ACCOUNT_DATABASE" \
  --file=account-platform/deploy/runbooks/sql/account-039-rollback-inventory.sql
```

核查先读取 `public.schema_migrations` 的 `version`、`name`、`checksum`。最高版本至少为 018 时，还需要读取 `public.approval_requests` 的 `command_type`、`policy_version`。空账本及早于 018 的版本尚无审批关系，仍可报告迁移状态。目标库不符、预期审批关系缺失、缺权限、行级安全影响可见性、查询超时或脚本失败时，记录为核查失败并停止评估。正常输出包括最高迁移、039 安装身份和全部状态下对 039 所涉 v2 策略的引用数。把脱敏结果、站点与发布标识、核查时间和脚本提交保存在受控证据中。

| 核查结果 | 恢复路径 |
| --- | --- |
| `039_NOT_INSTALLED` | 039 不适用。核实计划回退的应用与实际 Schema 兼容。 |
| `EMPTY_MIGRATION_LEDGER`、`MISSING_039_ON_LATER_LEDGER` 或 `039_IDENTITY_MISMATCH` | 由 DBA 核实实际目标和迁移包来源，保留原账本。确认前停在现有安全状态。 |
| `LATER_MIGRATIONS_PRESENT` 或 `039_V2_POLICY_REFERENCED` | 先评估保留数据库的应用回退。应用与现有 Schema 不兼容时，走前向修复或隔离恢复。 |
| `039_RESTORE_REHEARSAL_REQUIRED` | 核对目标发布包的 039 Up/Down、备份和应用兼容性。只有确需执行 039 Down 时，才在站点恢复副本上通过正式 Account 迁移入口演练，验证原外键、权限撤销、历史保全和失败原子性。 |

核查是只读快照。审批写入或迁移状态变化后，应在执行窗口重新核对，正式回滚入口的事务和外键仍承担最终保护。关单需留存站点覆盖清单、实际演练及恢复结果，并由 DBA 和发布负责人确认采用的恢复路径。

## 已有隔离验证

真实 PostgreSQL 回归 `TestF10OrganizationGovernanceRollbackPostgres` 从原数值版本 001–039 安装独有数据库，不删高版本账本伪造历史阶段。它覆盖原始 SQL 的失败对照、空库与含角色绑定的正确撤权、重复权限关联只失效一次、撤销绑定和无关绑定不变、未知账本身份、后段失败原子性及历史审批外键保护。设置 `PINVOU_TEST_ACCOUNT_MIGRATE_BINARY` 为当前源码实际构建的 `cmd/migrate` 后，另经正式 CLI 验证；未设置时该 CLI 子例明确跳过，不能算 CLI 验证通过。本地验证不操作留存数据库，不等同部署回退验收。

私有 F06 资金验证入口执行现存的个人调用共享 Token/Billing 用例和组织成员额度资金用例，解析实际 `go test -json` 结果，要求两组父用例及叶场景真实通过；零命中、跳过、失败、错误名称或损坏证据均不能通过。个人 JSON/SSE/未知用量覆盖不冒称已验证完整组织身份、供应商、组织开通或分布式执行流程。

该入口没有显式 `PINVOU_TEST_F06_OWNER_DATABASE_URL` 时跳过，不借用普通 Billing 配置。显式启用后要求独占的本机测试运行时，并通过 `PINVOU_PRIVATE_OWNER_DATA_DIRECTORY`、`PINVOU_PRIVATE_OWNER_USER` 和连接配置核对目录、用户、端口及所有回退地址；不绑定私人机器路径或固定端口。配置错误按既有严格规则失败，不能隐藏为跳过。测试前须没有既有部署角色或其他客户端，测试后校验自己创建的数据库和角色清理，不操作留存业务库。

`PINVOU_PRIVATE_GO` 指向已准备的 Go 可执行文件，`PINVOU_PRIVATE_OUTPUT_DIR` 指向本次独立证据目录，两者使用有效绝对路径。证据文件采用独占创建，不覆盖既有记录。子进程只接收已验证测试运行时的 Token/Billing 数据库配置，原资金幂等、未知结果、案件留存及迁移身份规则保持。测试报告须分别披露实际用例、受控上游及身份输入、条件跳过和未执行的发布验收。
