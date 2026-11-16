# Account Platform CI 并行执行

`account-platform-test` 使用 heavy 路由，但增加 CPU 本身不能缩短串行数据库测试。
2026-11-01 的成功运行提供了以下对照（不同提交、缓存状态，不能视作严格 A/B）：

| 运行 | Runner 档位 | 整个 Job | 财务 PostgreSQL 阶段 |
|---|---|---:|---:|
| [#2053 / 41277144752](https://github.com/Pinvou/pinvou3/actions/runs/41277144752/job/124761913263) | medium | 108 分 42 秒 | 79 分 12 秒 |
| [#2280 / 41257739457](https://github.com/Pinvou/pinvou3/actions/runs/41257739457/job/124704724397) | heavy | 113 分 08 秒 | 79 分 39 秒 |

第一份日志中，财务阶段依次执行所有者契约约 18 分钟、Token 约 10.5 分钟、
Billing 约 51 分钟。测试反复安装真实迁移，单个数据库的迁移和测试顺序具有意义。

## 执行方式

当前 CI 使用六个独立的 `account-financial-test` 矩阵作业，各自直接执行
`scripts/test-token-financial-postgres.py --group <组名>`，可与基础 Account 作业同时调度。
这六组此前已经由 `scripts/test-financial-parallel.py` 在同一 runner 内并行；
此次拆分让各组使用独立 runner，实际并发度取决于 runner 供给，不承诺固定提速。
矩阵关闭 fail-fast，完整组清单由校验器核对，所有适用组均必须通过：

- `owners`：保留原有 Account、钱包、Schema、供应商开通及生命周期协调阶段的顺序。
- `token`：执行完整原有 Token 包，仍只排除已经由 `owners` 执行的独占开通根测试。
- `billing-0` 至 `billing-3`：按 `scripts/account-financial-test-durations.json` 中的历史权重，
  将耗时最长的完整根依次分给当前负载最小的分片；同权重按根名排序，同负载按分片名选择。
  四组交集为空，并集始终等于全部必需 Billing 根测试；新根用默认权重参与，不会漏跑。

每组使用独立 PostgreSQL 服务、动态回环端口、匿名卷和系统标识。
服务仍启用默认 fsync、同步提交和真实事务；每个财务服务限制为 2 CPU、1 GiB 内存、无额外 swap。
既有部署、历史迁移、权限、回滚、闭户及数据库身份检查均保留。
所有分组继续启用 `-race`、`-count=1`，保留根测试及必需子用例验证。
retained Token 完整选中包的 Go 测试二进制预算为40分钟，retained Billing 为75分钟，
其它财务阶段为20分钟；编译、启动和身份检查在这些二进制预算之外，
基础 Account 作业和每个财务作业的外层上限均为180分钟。

分组之间不共享角色或数据库；组内也不拆分测试的锁竞争、故障注入或恢复子用例。
闭户契约由独立 `account-closure-test` job 执行，仅依赖 `changes` 路由，
使用自身的单个 PostgreSQL 服务及独立 ledger，不再等待财务分组或复用所有者服务；job 限45分钟。
基础 Account 作业保留两个原有 audit/broker 服务，每个财务作业及闭户作业各有一个服务。
这些作业使用 heavy 路由；全部同时执行需要八个可用 heavy runner。
容量不足时可以排队，排队或资源争用仍可能延长总耗时，不会因此省略必需组。
这只改变测试调度，不缓存已迁移数据库，不修改产品迁移或弱化持久性。

成功证明复用沿用完整输入校验：当前队列输入、配置及实际测试步骤必须匹配，
财务作业还必须具备全部六组的成功与退役证据，旧单体作业或部分组证据不能替代。
工作流、核验器或策略发生变化时，Merge Queue 仍须实际执行。
required-gate 只接受已核验复用的跳过；失败、取消或未经核验的跳过仍阻断。

## 诊断和验证

每组的测试输出与结果在对应作业的 `Run isolated financial group` 日志中。
`Financial group lifecycle receipts` 在成功或失败时尝试上传该组 ledger，
附件名称为 `account-financial-<组名>-resource-lifecycle-<运行次数>`，保留 7 天；
基础 Account 和闭户作业分别上传自身的资源回执。
上传属于尽力交付的辅助诊断：上传自身失败不阻断业务验收，附件可能缺失；
业务测试和服务收尾失败仍阻断 Job。上传失败时查看该步骤日志及财务测试步骤的标准输出。
这一设置不修复上传传输故障，也不能避免整个 Job 超时、取消或 Runner 丢失造成的失败。
任意组失败仍使财务汇总失败，其他组继续执行并完成各自清理。
每个 job 的收尾步骤先输出其自身 PG 的服务日志：基础 Account 两个、每个财务及闭户作业各一个；
然后自然停止这些确切服务，让 Docker AutoRemove 回收匿名卷。
停止前完整核对服务 ID、镜像、回环端口、匿名卷及 AutoRemove；不按名称或镜像前缀
选择容器，也不执行卷清扫。这样可避免 Runner 直接 force-rm 后留下临时数据库卷。
Runner 随后的标准清理可能对已自动移除的容器打印 logs/rm 警告；服务日志已提前输出，
不影响测试及收尾步骤的退出状态。
单组入口脚本不传 `--group` 时继续支持完整串行运行，可用于受控环境中同提交、同机器的对照。

静态及负向回归覆盖分组遗漏/重复、跳过、退出失败、角色残留、数据库身份串用、
服务配置和失败传播。真实性能必须以完整运行成功和实际计时为准；历史分组耗时只能用于预估。
