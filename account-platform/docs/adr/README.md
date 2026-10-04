# Account Platform 架构决策记录

本目录中的 ADR 是账号平台实现的约束。状态为 `Accepted` 的决策不得在普通功能 PR 中静默改变；需要变更时新增 ADR，说明兼容、迁移和回滚影响。

| ADR | 决策 |
|---|---|
| [ADR-001](ADR-001-service-boundaries.md) | 服务边界和数据库所有权 |
| [ADR-002](ADR-002-identity-boundary.md) | Keycloak 与业务账号边界 |
| [ADR-003](ADR-003-active-context.md) | 活动组织按会话保存 |
| [ADR-004](ADR-004-ledger-units.md) | 积分单位和账本 |
| [ADR-005](ADR-005-usage-settlement.md) | Token 预占、结算和释放 |
| [ADR-006](ADR-006-payment-state.md) | 支付状态、迟到支付和退款 |
| [ADR-007](ADR-007-payment-simulator.md) | 模拟支付与真实支付隔离 |
| [ADR-008](ADR-008-newapi-cutover.md) | 新旧 New API 隔离与切换 |
