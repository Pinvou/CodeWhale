# APT-134-R2：计划发布的草稿与目录基线修复

## 需求与根因

APT-138 真实生命周期集成验收发现，合法的已发现模型保存独立草稿后，计划发布首次提交固定返回冲突。`ScheduleModelPublish` 要求 `model_catalog_entries.lifecycle_status='DRAFT'`，但已发布 Token 000012 的目录约束只允许 `PUBLISHED / DEPRECATED / REMOVED`。实际草稿由 `model_catalog_drafts` 独立保存；将目录伪造成 DRAFT 会违反数据库约束，不能用非法 fixture 绕过此问题。

状态 `DONE / REMEDIATION_REVIEW_PASS`。本批是 APT-134 领域前置整改，不夹入未完成的 APT-138，不修改已发布迁移或目录生命周期枚举。

## 最小设计

- 在现有目录/模型锁下，仅把错误的“目录必须 DRAFT”条件改为“期望目录版本中确有该模型的合法目录基线”。未知模型或缺失目录条目仍冲突，不凭空创建目录模型。
- 草稿是否存在、草稿 base_catalog_version 是否等于当前目录、精确 draft_version/fingerprint、协议及路由/Provisioning 集合仍由现有同事务 `loadCommandPrecheckSnapshot` 与冻结 expected facts 重验；不可只检查目录条目后绕过草稿校验。Billing 价格仍由既有跨域权威预检提供冻结证据，不宣称 Token 事务内原子重读 Billing。所有 stale snapshot、依赖缺口与活动计划冲突仍失败关闭。
- 计划记录的 `current_lifecycle='DRAFT' → target_lifecycle='PUBLISHED'` 表示独立草稿的发布动作，保持现有不可变计划 schema 与 worker 语义；它不是声称目录已处于 DRAFT。计划提交只持久化未来执行计划、通用结果、审计和 Outbox，不提前发布目录、不删除草稿。
- 与既有即时草稿发布保持一致：目录三种合法状态不替代独立草稿事实。只纠正计划发布错误地要求目录 DRAFT 的条件，不新增生命周期转换规则、不修改 worker、审批、策略、凭据或 APT-138 guard。
- 保留 existing-first 幂等及同事务计划/result/audit/Outbox；重放原提交不因后续可变草稿或目录变化生成第二条计划或重复事件。

## 单任务与验收

- 三文件边界：本文、`internal/token/lifecycle_plans.go` 中 `ScheduleModelPublish` 唯一目录基线检查、独立 `internal/token/scheduled_publication_baseline_integration_test.go`。同文件的 APT-138 guard 不属于本整改提交。
- 文档经 B 审核通过后编码，代码再经 B 复审；根代理从 HEAD 加本批三文件导出隔离候选，以真实 PostgreSQL 和正式 HTTP handler 验收，再独立签署提交。
- 专属 fixture 自建合法已发现目录、独立草稿、正式协议、路由与 READY Provisioning，不复用仍未提交的旧 `seedLifecycleModels` 修订；局部包正式 CorrelationHandler，显式 SCHEDULED 命令，报价为明确固定证据 fixture，不声称 Billing 集成。
- 覆盖合法目录基线加精确草稿的计划创建、计划 DRAFT→PUBLISHED 语义、目录/草稿未提前变更、通用审计与 Outbox 精确一次；重复提交及后续草稿漂移后的原键重放保持完整持久事实不变。
- 覆盖缺失草稿、过期草稿版本/内容、目录基线漂移或条目缺失、路由/Provisioning 不满足及活动计划冲突的拒绝，比较失败前后的完整计划、命令、审计与事件事实，禁止以宽泛非 nil 错误掩盖 SQL 失败。
- 具名 `TestScheduledPublicationBaselinePostgresIntegration` 不把 SKIP 计通过。到期 worker 的完整生命周期大回归由 APT-138 整合候选另行运行，不混入本批已有旧 fixture 改动或提前宣称整体通过。

## 独立审核记录

- 2026-09-06 A：结合根代理真实 409 与数据库约束证据，核对即时 `publishCatalogDraftEffect`、计划预检与独立草稿结构，完成最小方案供 B 审核；尚未修改领域生产代码。
- 2026-09-06 B：文档审核 PASS；明确 Token 同事务仅重验本地事实，Billing 保持既有跨域权威预检，不扩大原子性声明。
- 2026-09-06 A：唯一目录基线条件与专属真实 PG/HTTP 测试已编写并 gofmt/diffcheck，提交 B 代码审核；尚未宣称隔离候选运行通过。
- 2026-09-06 B：三文件代码静态审核 PASS；唯一 EXISTS 检查及后续本地精确重验保持原边界，合法 fixture 与正负矩阵符合已审文档。
- 2026-09-06 根代理：独立候选 `18a24ccc3eedeb4e0e3d9178591999d31a3aabec` 为 HEAD 加本批精确三文件，不含 APT-138 guard；`TestScheduledPublicationBaselinePostgresIntegration` 真实 PostgreSQL 与 HTTP 验收 2.69 秒，10 个子场景全部 PASS，exit 0、无 SKIP。
- 2026-09-06 B：结合隔离候选实测给出最终独立审核 PASS。本整改完成并交根代理独立签署；尚待整合的大生命周期 PG 和 APT-138 总验收不计入本结论。
