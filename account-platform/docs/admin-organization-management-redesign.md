# 管理后台组织管理闭环设计

> 状态：已批准；APT-099～APT-103 已完成，APT-104 待分任务实施
>
> 日期：2026-09-01
>
> 关联需求：ACC-16、ACC-65～ACC-68、UI-33～UI-36

## 1. 目标与边界

组织管理从“列表加大抽屉”调整为可扩展的平台治理工作区，闭环覆盖组织目录、独立详情、成员权限、组织结构和终态删除。平台超级管理员仍受生命周期、最小权限、审计和跨域一致性约束，不获得绕过入口。

本轮不创建自定义角色、不恢复 DELETED 组织、不提供批量删除，也不把组织角色定义复制到组织详情。当前账号生命周期不采集或维护邮箱，组织管理界面不得展示、搜索或以邮箱作为成员标签。

## 2. 信息架构

### 2.1 组织目录

- URL 保留 `section=organizations`，并把 `organization_q`、`organization_status`、`organization_cursor` 写入查询参数。
- 工具栏包含搜索、状态筛选和“创建组织”；创建表单按需打开。
- 表格展示名称、可复制 ID、本地化状态、成员数、创建时间和操作。
- 组织名称/“查看”进入 `section=organizations&organization_id=<uuid>&organization_tab=overview`。
- ACTIVE 的“更多操作”只提供停用；DISABLED 提供启用和进入删除检查；不提供批量生命周期操作。

### 2.2 独立组织详情

详情占用主内容区并提供面包屑和返回目录，不使用模态抽屉。四个分区为：

1. 概览：名称、ID、状态、所有者、成员/节点数量、创建与更新时间；
2. 成员与权限：分页表格、远程添加成员和按需权限编辑；
3. 组织结构：树、创建、重命名、移动、删除；
4. 生命周期与风险：停用影响、删除就绪度、阻断项和终态删除。

返回目录必须恢复进入详情前的查询参数。详情刷新、浏览器前进/后退和复制链接均保持目标组织及当前分区。

## 3. 删除状态机与错误合同

```text
ACTIVE --停用--> DISABLED --只读就绪度检查--> READY | BLOCKED | UNAVAILABLE
READY --名称+原因确认并重新预检--> DELETED
```

- `GET /account/admin/v1/organizations/{id}/deletion-readiness` 只读聚合 Account、Billing、Token 当前事实；无删除权限时仍不得借读取接口推导敏感资源，响应按读取权限脱敏。
- DELETE 必须重新执行同样的强一致检查，不能信任前端缓存的 READY。
- `ORGANIZATION_LIFECYCLE_BLOCKED` 显示领域、稳定 code、资源类型和脱敏资源引用；`ORGANIZATION_LIFECYCLE_UNAVAILABLE` 显示可重试状态和 request_id。
- ACTIVE 不发送 DELETE。终态确认要求完整组织名称和 1～500 字原因；删除继续为 AUDIT_ONLY，但原因进入不可变审计。

## 4. 成员与权限

- `GET /organizations/{id}/members` 增加向后兼容的 `limit/cursor/q/role`，响应为 `items + next_cursor`。
- 稳定排序为 OWNER 优先，其后按规范成员标签和 `membership_id`；游标必须冻结排序字段。
- 主表只显示显示名称/用户名/用户编码/稳定 ID、状态、组织角色摘要、节点范围数量、加入时间和操作。
- “编辑权限”打开侧面板并只对当前成员渲染角色和节点选择；节点管理员仍是组织类角色目录中的节点作用域，不扁平提升为整个组织权限。
- 行级保存、移除、加载和错误相互隔离；最后所有者仍只能走所有权转让流程。

## 5. 最小权限

| 能力 | 权限 |
|---|---|
| 读取目录、详情、成员和结构 | `platform.organizations.read` |
| 创建顶层组织 | `platform.organizations.create` |
| 修改组织名称/资料 | `platform.organizations.profile.manage` |
| 启用、停用 | `platform.organizations.status.manage` |
| 添加/移除成员、修改成员角色 | `platform.organizations.members.manage` |
| 创建、重命名、移动、删除节点 | `platform.organizations.units.manage` |
| 转让所有权 | `platform.organizations.owner.transfer` |
| 终态删除 | `platform.organizations.delete` |

迁移必须把旧角色中由 `status.manage` 隐式获得的资料、成员和结构能力显式映射到新权限，避免升级后意外丢权；迁移完成后服务端不得继续用 `status.manage` 放行其他能力。

## 6. 独立任务与提交边界

| 任务 | 状态 | 独立交付 |
|---|---|---|
| APT-099 | 已完成 | 删除就绪度 API、结构化错误、终态确认和三语 Portal 闭环 |
| APT-100 | 已完成 | 独立详情 URL、历史恢复、四分区页面骨架 |
| APT-101 | 已完成 | 组织目录游标/状态筛选、远程所有者选择、创建与列表精简 |
| APT-102 | 已完成 | 成员游标 API、紧凑表格、远程添加和按需权限编辑 |
| APT-103 | 已完成 | 四类组织治理权限、角色迁移、服务端和前端权限门禁 |
| APT-104 | 待实施 | 管理端节点移动 API、CAS、树交互和冲突反馈 |

每个任务独立提交并在完成时更新任务状态、实现状态、OpenAPI、三语文案和对应自动化测试。文档基线提交只建立批准需求和 PENDING 任务，不提前宣称实现。

## 7. 验收重点

- ACTIVE 组织不能直接删除；DISABLED 的真实阻断和依赖不可用可区分。
- 组织和成员数量超过一页时无遗漏、重复或首屏候选限制。
- 详情 URL 可刷新、可分享、可前进后退，返回目录不丢状态。
- 主成员表不会产生“成员数 × 节点数”的复选框规模。
- 无邮箱文案或空邮箱占位；角色定义不在详情重复。
- 仅状态权限不能修改资料、成员或节点；管理端节点移动不绕过结构版本和对象边界。
