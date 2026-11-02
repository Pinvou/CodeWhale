# 账户平台动作路径边界

Gin 将路径段内的冒号也解释为参数起点。账户平台现有五类 POST 动作以 `RequirePathSuffix` 保留合同中的完整动作拼写；相似但不同的动作不能进入对应处理器。

| 服务 | 已登记动作 | 保留的权限边界 |
| --- | --- | --- |
| Token | `/v1/organizations/:organization_id/quota-grants:batch` | 原组织权限与处理器校验 |
| Token | `/v1/organizations/:organization_id/account-closed-grants:reclaim-batch` | 原组织回收权限与处理器校验 |
| Billing | `/internal/v1/organizations/:organization_id/quota-grants:batch` | 原工作负载权限与人类授权证明 |
| Account | `/internal/v1/ledger-repair/authorization-proofs:batch` | 原内部工作负载权限 |
| Token | `/admin/v1/commands:precheck` | 原管理权限与命令治理开关 |

合法路径的方法、参数、认证授权、幂等和资金事实处理均不改变。拒绝针对解析后的 URL path，query 不能伪装动作后缀；标准 URL 转义后的等价路径继续按既有 Gin/HTTP 解码规则处理。该守卫不替代身份、权限或资金证明。

定向 Go 回归通过真实公共路由配置验证合法/别名、参数与处理器调用次数；同类注册和授权覆盖另行核对。该测试不是工作负载身份、真实财务操作或现场端到端验收。

此独立修复从 #2251、#2253、#2255 的无关增量中分离；三个原 PR 分别保留原因码、兼容层文档、注销审计主题。其余审核问题与关联 Issue 的剩余验收不因本次范围分离而自动关闭。
