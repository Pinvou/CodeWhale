# 审批分页来源与配额

本说明对应 #1632 的单 Portal 拓扑分页来源项。Account 的已注册
`GET /admin/v2/approval-requests` 从 HTTP 入口限流器的已验证来源上下文
取得分页 IP 键。没有该上下文的旧装配仍按实际连接地址处理；领域 handler
不读取浏览器转发头。Token/主体认证、分页快照和过滤规则保持原合同。

## 有效配置与逐跳边界

Portal 从自己入口的已验证来源创建新的 Account 请求，并附带现有
`X-Pinvou-RateLimit-Source` 提示。Account 只接受其
`HTTP_TRUSTED_PROXY_IPS` 中明确列出的即时对端发来的单个有效地址。
该提示仅用于防滥用计费，不证明身份或授予权限。缺失、重复或非法提示，
以及未信任的对端，继续归入连接地址；`X-Forwarded-For` 不获得新权威。

生产 personal Compose 模板将 edge/Portal 对端地址作为必填变量传入
Account 的入口限流配置；这只证明仓库模板接线，不代表站点地址已经核实或已部署。
普通 Compose 使用 `ACCOUNT_SERVICE_HTTP_TRUSTED_PROXY_IPS`，Portal 使用
`TOKEN_PORTAL_HTTP_TRUSTED_PROXY_IPS`；Helm 使用
`security.httpTrustedProxyIPs.account-service` 和 `token-portal`。
后两种载体的默认空列表保留连接地址计费。站点必须填入实际、固定、会覆盖
该提示的即时代理地址；不使用 CIDR、整个私网或用户提供的任意转发链。
部署和网络配置未完成时，不把源码修复宣称为站点已按客户来源拆桶。

## 保留的容量和失败语义

原 PostgreSQL 分钟窗口继续跨 Account 实例共享：每主体 60 次读取、
每来源 IP 120 次读取。快照创建的主体/全局预算和容量不变，外层进程准入
继续生效。更换来源不能绕过主体预算，同一来源不能通过更换实例绕过 IP
预算。拒绝时事务回滚，保留原 `RATE_LIMITED`、`Retry-After` 和恢复方式；
数据库故障、无会话及无效分页输入继续使用原错误分类。

验证区分三层：实际 Portal BFF 的来源传递、已注册 Account 路由及真实入口
来源解析、公开迁移/真实运行角色下两仓库实例的分页与持久配额。前两层的
下游/身份验证受控，数据库层使用隔离 PostgreSQL；不将分层证据合称真实
站点、JWT 签发者、Redis 集群或生产容量验收。本修复不改变 schema 或额度。
