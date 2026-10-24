# Token Account 归属联合权威

创建及恢复复用 ADM-61/APT-123-R2 的 `POST /internal/v1/token-account-authority/snapshot`，要求已验证且映射为 `token-gateway` 的 workload 与精确权限 `internal.account.token-owner-authority.check`。主体匹配沿用配置规则，Compose 与 Kubernetes 可各用自己的主体；生产模板与 Helm/受控 Compose 使用同一权限。

四种 mode、固定有序双平台权限、17 字段闭合传输及 15 字段 JCS 仍由共享 `tokenauthority` 合同验证。Account 只在单一 RR 只读事务观察本域身份、角色、生命周期与版本，不查询 Token reservation。OPERATOR_AUTHORIZE 不读取 owner。

最小候选目录支持 USER、ORGANIZATION、UNIT；仅 UNIT 可携带真实父组织的 `organization_id` 筛选，USER 加组织筛选仍拒绝。候选公开 item 保持 id/type/display_label/status 四字段，保留原分页、高水位、授权绑定、限流及签名游标。UNIT 的有效状态由本行和实际父组织共同决定；父组织非 ACTIVE 时不能获得 ACTIVE 权威。版本仍是 unit.version，摘要绑定实际父组织，不编造组合版本。

Token 创建保留 `validOwner` 的 USER/ORG/ORG_UNIT 与 `validShareScope` 的既有验证，移除额外 USER-only 裁剪；不新增分享范围或成员管理写入口。EXPLICIT_MEMBERS 的既有读取与空成员行为保持，本修复不宣称补齐成员管理功能。

规范化状态和 tombstone 不变：存在但非 ACTIVE 的行归 INACTIVE，版本化 DELETED 归 NOT_FOUND，物理缺行作为不可用失败关闭。组织正常停用/启用在同一 Account 事务推进其 raw ACTIVE UNIT 的持久 version 与 updated_at，保留 raw 状态与组织结构版本；精确状态重放不重复推进，非 ACTIVE、tombstone 和其他组织 UNIT 不受影响。Token 因正常停用取得更高版 DENY 后终结 FAILED 并释放 reservation 容量；共享 owner wallet 保留。只有模拟旧存量或异常绕过领域写入的同版 DENY 才继续保持 PENDING，不能用放宽 Token DENY 校验修复版本缺口，也不据此宣称所有 Saga 终态问题已解决。

T1 仅写合法 reservation/result/audit/outbox，不调用钱包。worker 按已有合同先取得 Billing receipt 再 T3；后置失败不能宣称零 wallet，且不得危险删除共享 owner wallet。只有最终 Token 权威及事务围栏通过才生成可读账户。

验证入口为 `go test ./internal/account -run '^TestOwnerCombined' -count=1 -v`，必须显式提供独占 PostgreSQL 的 `PINVOU_TEST_ACCOUNT_DATABASE_URL`，具名测试无 SKIP 才算执行。它使用实际 Token HTTP client、Account handler、签名 JWT/JWKS、公开迁移与运行身份；内部子进程另建独占 Token fixture，验证三 owner 创建、父组织停用与人工恢复。受控钱包供应方不等于真实 Billing、生产身份设施、TLS 终止或部署验收。
