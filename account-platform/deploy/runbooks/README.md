# Account Platform 运维手册

- [事件投递、死信与安全重放](event-delivery.md)
- [可观测性与告警响应](observability.md)
- [数据库备份、PITR 与恢复](backup-restore.md)
- [production-new 切换与回滚](cutover-rollback.md)
- [真实 New API 部署与初始化](newapi-deployment.md)
- [Keycloak 受控管理访问](keycloak-admin-access.md)
- [RPO/RTO 恢复演练记录](rpo-rto-exercise-template.md)

手册中的数据库与事件工具只允许作为短生命周期、最小权限的运维 Job 运行。
生产执行前必须记录 Issue/事故号、目标环境、镜像 digest、操作者和复核人。
