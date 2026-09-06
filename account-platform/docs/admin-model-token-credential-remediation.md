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

## APT-133-R2：凭据校验租约时间参数整改

- 状态：DONE / REMEDIATION_REVIEW_PASS；七文件独立候选真实 PG 与 B 最终独立审核均通过。
- 根因：后续真实 PG 回归在 `claimCredentialValidationProbe` 领取租约时返回 SQLSTATE 42P08；同一 `$2` 同时用于 timestamp 列赋值与未声明类型的 interval 加法，导致参数类型推断冲突，校验未到达 provider。
- 初始范围为显式 `timestamptz` 的唯一租约 UPDATE；随后经 B 审核补齐同一超时恢复链的 Go 与数据库固定码约束。最终范围以下述七文件合同为准，保留原 30 秒租约、UTC 时钟、策略、事务隔离级别、探测次数与激活状态语义。
- 独立任务最终包含本文、唯一租约 UPDATE、专属 PG/负例测试、固定码 validator 的唯一分支、新 Token 000030 up/down 与其 manifest 登记。APT-138 的 epoch guard 仍由 APT-138 负责，本修复从 HEAD 加本批七文件独立验收和提交。
- 验收：真实 PG 证明首次领取成功、时间精确等于固定时钟且到期差为 30 秒；未到期重复领取不再次取得密钥；非原 actor 被拒且不占租约；合法完成只产生一次校验事实、通用结果、审计及 Outbox。已完成重放不新增效果；原租约超时仅将原命令终结为 `CREDENTIAL_PROBE_OUTCOME_UNKNOWN`，不得重新领取密钥或发起 provider 校验。
- 回归不得改用 simple protocol 来绕过生产参数推断，也不得放宽权限、版本、幂等和一次探测校验。测试 seed 自己使用显式时间类型，测试密钥只在内存中使用且不输出。
- 2026-09-06 B：上述需求、设计及任务审核 PASS，允许最小编码。首次 PG 已验证租约领取不再报 42P08；合法完成的测试 fingerprint 改为既有要求的 64 位十六进制，未放宽生产校验。

### R2 补充：原租约超时结果的固定错误码

- 已经 B 审核：同一恢复闭环存在既有合同冲突，超时路径使用固定 `CREDENTIAL_PROBE_OUTCOME_UNKNOWN`，通用 stable error 校验却拒绝任何含 `CREDENTIAL` 的值，导致原命令无法终结。
- 最小方案：增加 `command_governance.go` 中 `validStableTokenErrorCode` 的单个精确常量豁免；不允许前缀/后缀拼接，不允许其他任意 credential/token/key/secret/password 文本，不改名已有错误码，不接收 provider 原始错误。其余敏感字段和结果投影校验保持不变。
- 此阶段范围增至四文件（最终另追加下面三份数据库文件），新增源码范围仅该 validator 的单个固定常量分支；专属测试同时验证常量精确接受、大小写/空白/前后缀/任意凭据码及原敏感码仍拒绝，并通过真实 PG 验证到期原命令失败终态和审计只写一次。APT-138 在同文件的所有其他未提交修改不进入本批候选及提交。
- 2026-09-06 B：固定错误码补充设计 PASS。第二轮真实 PG 的首次领取、合法完成及终态重放均已通过，超时最终持久化进一步暴露 `000024` 的同名数据库 CHECK 也拒绝该固定码（SQLSTATE 23514）。

### R2 补充：数据库前向兼容约束

- 已经 B 审核：为避免改写历史迁移或重命名既有公开码，本批唯一取得 Token `000030_credential_probe_unknown_code` 的 up/down 及 manifest 登记。尚未提交的 APT-138 activation 迁移及其具名引用顺延 `000031`，由 APT-138 所有者同步；不替换任何其他 30 秒时长或历史序号。
- up 在原表上事务内重建同名 `token_command_results_stable_error_code_check`，保留原 NULL、长度、字符范围与敏感词限制，只额外允许精确常量 `CREDENTIAL_PROBE_OUTCOME_UNKNOWN`。不更新、删除或重写任一命令结果，不变更权限或表所有者。
- down 同一事务先锁表并检查引用；任何结果已持久化该常量则以 SQLSTATE 55000 拒绝并保留事实。无引用时才恢复原约束。应用回滚可保留本兼容约束，不宣称能删除历史结果。
- 独立 PG 增加数据库约束拒绝前后缀及敏感码、无引用 down/up、已有引用 down 拒绝且结果/审计不变的证明。本批至多七文件：上述四文件、两份前向迁移与 manifest，仅本批登记进入独立候选；不夹带 APT-138 activation 迁移及其他源码。
- 2026-09-06 根代理：隔离候选 `ace70024`，`TestCredentialProbeLeasePostgresIntegration` 真实 PG 通过（1.74s），同时固定码精确边界与原敏感码回归通过；Token 包 3.606s、退出 0、无 SKIP。包含精确 30 秒、唯一完成/重放、到期 UNKNOWN 零 provider 调用、原 SQL CHECK 负例及有引用 down 55000 保留历史。manifest 仅前向新增本批两文件，共 270 项；APT-138 代码与 000031 均未进入候选。
- 2026-09-06 B：APT-133-R2 最终独立审核 PASS；租约、固定码双层精确约束、历史回退保护、七文件边界和真实 PG 证据均通过。仅本批独立签署提交，不包含 APT-138 的同文件改动或 000031，不代表总装完成、推送或部署。
