# 已交付财务权限的 Account 注册

注册赠送读取、管理及管理员免单三个已安装入口使用
`platform.registration-bonus.read`、`platform.registration-bonus.manage`、
`platform.billing.holds.release`。这些精确权限纳入 Account 运行时允许集合，
由既有单权限、复合权限查询及 Bootstrap、Me、授权上下文和角色目录共同消费。
未知权限仍默认拒绝；允许集合不从全部数据库种子或权限前缀自动生成。

当前角色边保持公开迁移结果：超级管理员拥有三项；Operations Admin
可读写赠送策略；Auditor 只读赠送策略；Billing Operator 可免单。
Billing Approver 不因此获得免单权，组织或单元绑定也不获得平台权限。
本次不修改迁移、角色授予或撤销规则，不新增 Portal 免单工作流。

有效身份、当前用户及绑定状态、作用域、授权版本、近期认证、readiness、
操作键、理由及原有资金执行规则继续由现有权威入口核验。
权限被登记不表示免除这些边界，也不代表已完成真实资金或发布验收。

真实角色及权限投影回归由隔离财务 PostgreSQL 门禁显式选择
`TestDeliveredPlatformPermissionsPostgres`，要求该根和子用例实际通过，
缺失、跳过或只有无子用例的根通过均不能核销此项验证。
