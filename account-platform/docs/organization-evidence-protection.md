# 组织命令证据保护

Account 的补充迁移 `900015_organization_evidence_truncate_guards` 为组织创建、单元命令、成员治理的三个原始证据表补齐语句级保护，复用已有拒绝修改函数。原有行级 UPDATE/DELETE 保护、业务写入、幂等回执及历史迁移保持原样。

部署仍使用公开的 Account 迁移入口和现有角色；新建和存量数据库均执行该前向迁移。Up、Verify、Down 校验该迁移的已发布身份及双向 SQL；Down 明确拒绝撤去保护，返回 SQLSTATE `55000`。修正后续问题应追加前向迁移。迁移清单只追加新的一对文件。

这项保护拒绝正常运行角色直接清空证据的语句。Account 现有 `account_service` 仍是表及数据库所有者，拥有 DDL 管理权；本变更不建立独立所有者边界，也不承诺抵御所有者主动撤销保护。调整 Account 所有权、部署角色及升级权限需要单独交付，不能据此将 #1599 整体关闭。

Billing 的原始 `030` 两个闭合授予证据表已受 `900003_runtime_ownership_hardening` 的全局保护：运行角色无 TRUNCATE/所有者权限，所有者执行清空也由语句级触发器拒绝。本次增加真实部署角色验证，未追加重复 Billing DDL。

`scripts/test-token-financial-postgres.py` 在 owner-account 阶段要求新建和存量升级、三表保护、拒绝后的合法回执重放及公开 Down 拒绝；在 retained-billing 阶段要求两个原始关系的运行角色权限与所有者保护。断言要求实际 PostgreSQL、原始公开迁移及非超级用户角色；跳过、缺少任一必需子场景均不能通过门禁。Account 证据由真实命令写方生成，身份激活使用现有夹具，不代表身份供应或钱包的完整端到端验收。
