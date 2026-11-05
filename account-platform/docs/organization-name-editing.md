# 组织与节点名称编辑

管理员详情默认只读显示组织和各节点名称；具备原 `platform.organizations.profile.manage` / `platform.organizations.units.manage` 权限时，可分别主动进入名称编辑，提交或取消。取消只丢弃该对象尚未提交的草稿；成功确认后该对象退出编辑，其他对象的未提交草稿在详情刷新中独立保留。

草稿仅保存在当前页面内存，并随客户端、身份、授权、选中组织或相关管理权限退休。写入仍使用原 OrganizationAdminClient、权限、名称校验与幂等调用；busy 或 UNKNOWN 时不能用取消创建新调用。UNKNOWN 仅以原事实和原 key 显式重试，语言切换不重发。编辑成功或取消不会改变对象 ID、父子关系或审计合同。

交互回归接入现有 `npm run test:account-platform-organization-ui` 中的组织边界 smoke，使用真实 React 组件、AdminPlatformClient 和 finance hook；受控 HTTP 响应不代表真实 Account/IdP 或生产部署验收。
