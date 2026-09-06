# APT-133-R1：隔离凭据会话完成前向整改

## 需求与根因

APT-138 的真实 PostgreSQL 总验收在 `CompleteCredentialSession` 完成事务中发现 SQLSTATE 42P02：会话更新语句含 `WHERE id=$1`，但 `Exec` 未传入 sessionID。该语句另以双反斜杠 bytea 字面量清理 PKCE；真实 PostgreSQL 验证其长度为 2，而非空字节串。两处均属既有 APT-133 领域实现，APT-138 暂停且保留未提交进度，使用本批独立修复、审核及提交，不混入激活功能。

## 设计与不变量

- 仅修正完成语句的 sessionID 参数，并以 `decode('','hex')` 生成明确的零长度 bytea；`key_id` 同时清空。无 schema、策略版本或公开 API 变更，不修改历史迁移。
- 会话必须仍为 READY，关联 launch 必须为 AUTHENTICATED；原浏览器/CSRF/actor/权限/版本及凭据验证要求不变。仅指定会话进入 COMPLETED，其他会话不得被修改。
- 凭据候选创建、旧凭据轮换、账户版本递增、session/launch/result 完成和审计仍处于原同一事务。任何后续写入失败全部回滚，不留下部分凭据或清空了一半的会话。
- 完成后的重复提交仍按原一次性语义拒绝，不改成重复 POST 成功。原安全结果可只读查询；重试/查询不得产生额外凭据、账户版本或审计，PKCE 不可回显。
- 测试 fixture 使用真实 PostgreSQL repository 与既有领域方法，不禁用生产触发器，不输出密钥内容。独立验证从 HEAD 加本批文件导出，不依赖未提交的 APT-138 reader、guard 或旧大测试修订。

## 独立任务与验收

- 状态：DONE / REMEDIATION_REVIEW_PASS；隔离候选真实 PostgreSQL 验证与 B 最终独立审核均通过，等待根代理独立签署提交。
- 文件所有权：本文；`internal/token/credential_isolation.go` 中唯一完成语句；新增专属 `internal/token/credential_session_completion_integration_test.go`。不修改 APT-138 或已发布迁移。
- 验收：真实 ENROLL/ROTATE 会话完成成功；指定 session 的 ciphertext 长度为 0 且 key_id 为空，其他 READY 会话原样保留；安全结果与凭据/版本/审计精确一致。完成后再次提交被拒、GET 原结果不变且持久证据无新增。通过测试限定的后续写失败证明整事务回滚，账户、凭据、会话密文与状态保持原事实。B 代码审核及隔离候选真实 PG 通过后独立签署提交，再恢复 APT-138。

## 独立审核记录

- 2026-09-06 A：复核实际 SQL 与原一次性生命周期，提交上述最小整改；尚未修改领域代码。
- 2026-09-06 B：正式文档审核 PASS，三文件边界与验收完整，允许按最小方案编码。
- 2026-09-06 A：已修正唯一完成语句参数和零长度清理，新增专属 ENROLL/ROTATE 真实 PG 回归；审计按实际领域动作精确计数，重放后六类持久事实整体不变，最终审计的测试限定失败验证所有前序写入原子回滚。尚未计入任何 APT-138 完成状态。
- 2026-09-06 根代理实测：仅含 HEAD+本批三文件的隔离候选 `2651afcf`，`TestCredentialSessionCompletionPostgresIntegration` 真实 PG 通过（主用例 5.98s；ENROLL 0.42s、ROTATE 0.11s、late_failure 0.10s），Token 包合计 14.204s、退出 0，无 SKIP。Migration manifest 268 项、architecture guard 与 diffcheck 均通过。
- 2026-09-06 B：APT-133-R1 最终独立审核 PASS；三文件范围、原一次性语义、真实零长度清理和同事务回滚证据均通过，可独立签署提交。提交后才能恢复 APT-138，不代表激活功能完成，未推送或部署。
