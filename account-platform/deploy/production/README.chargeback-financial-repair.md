# 拒付财务修复交付索引

F14 交付 `CHARGEBACK_FINANCIAL_REPAIR`，首版仅处理已确认 Provider 拒付的 BASE 短缺补偿。完整资格、冻结证据、审批与激活边界见 [受控激活手册](../runbooks/chargeback-financial-repair-activation.md)；不扩展 BONUS 缺口、部分撤销或退款策略，也不在本波注册后继 Reconciliation 命令。

## 复用统一部署

继续使用 [财务部署说明](README.finance.md) 中的 `compose.personal.yaml`、`compose.personal-workload.yaml`、`compose.finance.yaml` 组合和当前已审部署的其他叠加配置，保留既有 `ACCOUNT_PLATFORM_PROFILE` 与对应设施门禁。统一 Billing 服务已经按 `approvalconsumer.BillingCommands()` 注册 Chargeback，不需要新增独立 compose、服务或 `BILLING_CHARGEBACK_FINANCIAL_REPAIR_CONSUMER_ENABLED` 开关。

Account、Billing、Token、Portal、迁移作业、relay 和 approval-resources 必须来自同一个固定并独立审核的 F14 源码，包含标准入口构建的当前 Portal 静态资源。沿用统一配置的 NATS/TLS/NKey、公开身份、manifest、receipt 及 SHA 校验、独立私钥目录、只读挂载和最小 workload 权限。私密值只经既有受控环境文件投递，不能将展开的私密 Compose 配置打印到日志；资源缺失、摘要不符或读取失败继续拒绝启动。

## 完整迁移和资源集合

按 [数据库运行说明](../postgres/README.md) 先满足原有备份、独立迁移身份和运行角色要求，安装当前完整 Account/Billing 迁移集合，保留全部前序 SQL 身份和校验和。本波新增 Account `000075`、Billing `000070`、`000071`、`900019`，不能只摘录本波文件执行或重写 migration ledger。

当前源码包含九条独立 Billing descriptor 与三条 Token descriptor；本命令专属 durable 为 `billing-chargeback-financial-repair-executor-p1-v1`，policy v1 / snapshot v1。历史 shared Billing durable 仍只承接原三项映射。由同一当前源码按 [NATS 资源手册](../runbooks/nats-resources.md) 渲染并独立核验资源、ACL 和收据，不沿用旧命令集合或后继波次的资源说明。各运行模式的 live 操作和设施要求保持原样，注册清单不代表 broker 已安装或消费已就绪。

新命令改变全局策略集合后，按既有 SAFE、quiescence、fence、资源 readback 和独立签名发行流程，以当前 `PolicySetDigest` 重签并核验治理快照；旧 ACTIVE 摘要与收据不能复用。初始化、切换和激活均要求已授权 operator 显式执行，不由启动或 readiness 自动完成。

## 激活、回退和验收

默认 `INACTIVE` 只表示执行关闭，不代表生产就绪或可发布。适用 GSA-38～40 的生产等价集成、性能、N/N-1、切换/回滚、故障演练、额度和生产证据归档门禁，以及本命令明确激活授权必须全部具备，才可按 [受控激活手册](../runbooks/chargeback-financial-repair-activation.md) 操作。原 #1719 未定位的历史跨域就绪风险及其他既有验收限制不因本索引关闭。

Billing `900019` 提供固定 Chargeback v1/s1 的私有 owner 锁接口；新财务事务在不可变成功回执只读核对后检查 ACTIVE、冻结 activation 版本与时间。运行身份不能修改激活表或继承 owner。Down 显式拒绝；回退通过受控停止 consumer 和切换 INACTIVE，保留已执行 case、journal、receivable、审计和全部回执，不删除财务事实。

本索引不执行迁移、broker 资源发行、部署或激活。本地隔离 PostgreSQL、受控页面、编译与静态配置检查不能代替真实 Provider、issuer/NATS、现场切换和生产容量验收，正式合入与发布门禁继续保留。
