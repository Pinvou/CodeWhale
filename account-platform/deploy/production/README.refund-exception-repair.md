# 退款异常财务修复交付索引

F13 交付 `REFUND_EXCEPTION_REPAIR`：处理 Provider 已退款、但本地冲销缺失且已按既有规则升级财务修复的异常。运行、资格、审批和激活边界以 [受控激活手册](../runbooks/refund-exception-repair-activation.md) 为准；不新增退款方式、不再次调用 Provider，也不在本波注册后继 Chargeback 修复。

## 复用统一部署

继续使用 [财务部署说明](README.finance.md) 中的 `compose.personal.yaml`、`compose.personal-workload.yaml`、`compose.finance.yaml` 组合及当前部署已审核的模型等叠加配置，保留既有 `ACCOUNT_PLATFORM_PROFILE` 与各模式设施要求。Refund 已由统一 Billing 服务按 `approvalconsumer.BillingCommands()` 注册，不需要另建 `compose.refund-exception-repair.yaml`、独立服务或命令专属启用开关。

Account、Billing、Token、Portal、迁移作业、relay 与 approval-resources 必须来自同一个已固定并独立审核的 F13 源码，包含由项目标准入口构建的当前 Portal 产物。沿用统一配置中的 TLS/NKey、公开身份清单、manifest、receipt 及各自 SHA 校验、只读挂载、独立私钥目录和最小 workload 权限。私密值只通过既有受控环境文件投递，不输出展开的 Compose 私密配置。缺少有效 receipt 或出现资源漂移时必须继续拒绝启动，不能由运行服务自动生成并信任资源。

## 迁移和当前闭合集合

使用 [数据库运行说明](../postgres/README.md) 的既有备份、独立迁移和运行角色流程，安装当前完整 Account/Billing 迁移集合，保留全部前序 SQL 身份与校验和。本波新增 Account `000074`、Billing `000067`、`000068`、`900018`；不能只摘录本波文件执行，不能改写冻结迁移或 migration ledger。

当前源码 registry 包含八条独立 Billing descriptor 与三条 Token descriptor，Refund 的专属 durable 为 `billing-refund-exception-repair-executor-p1-v1`，policy v1 / snapshot v1；历史 shared Billing durable 仍只承接原三项映射。资源应由同一当前源码按 [NATS 资源手册](../runbooks/nats-resources.md) 的既有流程渲染、核验和投放，不能沿用早期六 descriptor 或后继命令的资源说明。各运行模式仍须遵守既有 live 操作与设施门禁，描述符清单不等于 broker 已安装或消费者已经就绪。

新增命令改变全局策略集合后，必须按原 SAFE、quiescence、fence、资源 readback 和独立签名发行流程，使用当前 `PolicySetDigest` 重新签发并验证治理快照及收据。旧集合 ACTIVE 摘要不能复用；初始化、切换和激活均由已授权 operator 显式执行，不由启动或 readiness 代替。

## 激活、回退和验收

默认 `INACTIVE` 是关闭执行的安全状态，不是生产就绪或可发布的结论。适用 GSA-38～40 的切换/回滚、生产等价集成、性能、N/N-1、故障演练、额度与生产证据归档门禁必须通过，并取得本命令明确的生产激活授权后，才能执行 [受控激活手册](../runbooks/refund-exception-repair-activation.md) 的操作。

Billing `900018` 提供固定 Refund v1/s1 的私有 owner 锁能力：新执行在同一财务事务内核对 ACTIVE、activation 版本和冻结事实；已提交的同事实回执仍先只读返回。运行身份不能修改激活表或继承 schema owner。Down 显式拒绝；回退通过受控停止本命令 consumer、置为 INACTIVE 并核验门禁，保留已执行 case、journal、应收、审计及全部历史回执，不删除财务事实。

本索引只补齐已交付统一部署入口，不执行数据库迁移、broker 资源发行、部署或激活。本地隔离 PostgreSQL、受控页面、编译和配置检查不能替代真实 Provider、issuer/NATS、现场切换或生产性能验收；正式合入与发布门禁继续保留。
