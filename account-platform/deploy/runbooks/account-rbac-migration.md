# Account RBAC 升级映射

本流程只用于从旧 `platform-admin` Realm Role 升级到 Account 业务 RBAC。OIDC/Keycloak 继续证明登录主体，但 Realm Role 不再授予任何业务权限；不得把所有旧管理员自动升级为超级管理员。

## 前置条件

1. 已完成 Account `000010_authorization_rbac` 迁移，应用尚未开放后台流量。
2. 待映射用户已经至少登录并完成一次 Account bootstrap，因此 `identities` 中存在稳定的 `issuer + subject → user_id` 映射。
3. 由变更审批明确列出每个主体、目标平台角色、复核人和回滚责任人；至少一名用户映射为 `PLATFORM_SUPER_ADMIN`。

## 受控映射

在受控 `psql` 会话中设置变量后执行；变量值和执行记录不得写入仓库。每次只处理一个经复核的主体：

```sql
\set issuer 'https://identity.example/realms/pinvou'
\set subject 'provider-subject'
\set role_code 'PLATFORM_SUPER_ADMIN'

BEGIN;

SELECT i.user_id, u.status
FROM identities i
JOIN users u ON u.id = i.user_id
WHERE i.issuer = :'issuer' AND i.subject = :'subject'
FOR UPDATE;

INSERT INTO role_assignments(
    id, user_id, role_code, organization_id, unit_id,
    created_by, source, created_at
)
SELECT gen_random_uuid(), i.user_id, :'role_code', NULL, NULL,
       i.user_id, 'CONTROLLED_MIGRATION', now()
FROM identities i
JOIN users u ON u.id = i.user_id AND u.status = 'ACTIVE'
JOIN roles r ON r.code = :'role_code' AND r.scope = 'PLATFORM'
WHERE i.issuer = :'issuer' AND i.subject = :'subject'
ON CONFLICT DO NOTHING;

COMMIT;
```

若主体不存在、用户不是 `ACTIVE`、目标角色不是平台角色，`INSERT` 必须写入零行并停止升级。不得临时恢复 Realm Role 授权绕过失败。

## 核验与回滚

映射完成后，在开放后台流量前核验：

```sql
SELECT u.id, i.issuer, i.subject, ra.role_code, av.version
FROM role_assignments ra
JOIN users u ON u.id = ra.user_id
JOIN identities i ON i.user_id = u.id
LEFT JOIN authorization_versions av
  ON av.user_id = u.id
 AND av.scope = 'PLATFORM'
 AND av.scope_id = '00000000-0000-0000-0000-000000000000'::uuid
WHERE ra.organization_id IS NULL
ORDER BY u.id, ra.role_code;
```

- 至少存在一个经审批的 `PLATFORM_SUPER_ADMIN`，且不存在未列入映射清单的主体。
- 用各职责账号调用对应后台接口，验证允许和拒绝矩阵；尤其验证超级管理员不能隐式读取或修改 Token Account、路由和供应商凭据。
- 回滚单条错误映射时，只删除 `source = 'CONTROLLED_MIGRATION'` 且主体、角色完全匹配的记录；删除前确认不会移除最后一个有效超级管理员。最后管理员自动保护与 Break-glass 由后续专门任务实现，在此之前必须由双人运维复核保证。
