# 审批授权 Relay：隔离投递与禁止人工重放

> 来源：原完整候选 `828242031c42911099792ec78c1f746ed8620c49` 的成熟手册。以下保留原安全合同及历史证据；恢复文档不等于当前发布、部署或外部审批通过。当前任务不执行生产命令。

## 当前入口与历史边界

当前发布仍须验证专用数据库/NATS 身份、独立可信 public artifact 摘要与专项探针。普通服务 ready 或单元测试不能替代授权链就绪；原迁移身份及历史冻结事实不能修改。以下 GSA 代码签署、环境欠账与候选状态仅为原时点记录，实际现场仍需独立审批和现行源码/配置核对。

所需 Secret/受控身份仅由已有安全设施提供，不能写进 argv、仓库、工单或日志。当前缺载体、签署、可信来源或真实执行记录时保持未验收；不能以历史 DONE/PASS、JSON 格式或自造收据代替。恢复工件绑定与现场独立审批的区别见 [运维证据](operational-evidence.md)。

## 保留的原手册

状态：GSA-07 代码完成并通过 B 代码审核，DEPENDENCY_ENV_PENDING / NOT_DEPLOYED。本文是发布约束，不是发布成功记录；最终代码与环境状态以原 admin-governance-security-tasks.md 的历史任务记录（当前是否可执行另核真实源码与部署前置）为准。GSA-04 的 Linux 凭据生成、root-owned 投递及完整初始化链欠账继续传递，不以 Windows 隔离测试替代。

## 身份与进程边界

本项仅处理 Account subject `account.approval-command.authorized.v1` 及其自动死信 subject `account-platform.dead-letter.account.approval-command.authorized.v1`。Account 的同一个 event-relay 进程运行两个独立循环：普通循环在领取候选和统计时即排除授权事件；专用循环使用独立数据库池及 NATS 客户端。Billing/Token 保留原通用 Relay 行为。

| 输入 | 归属与约束 |
| --- | --- |
| `DATABASE_URL`、普通 NATS 身份 | Account 普通 Relay；不能作为授权 Relay 的备用身份 |
| `APPROVAL_RELAY_DATABASE_URL` | 仅 `account_approval_relay LOGIN NOINHERIT`；不能使用 service、writer、backfill、owner 或 migrator 身份 |
| `APPROVAL_RELAY_NATS_NKEY_SEED_FILE` | 专属 `account-approval-relay` NKey 文件，与下一项互斥 |
| `APPROVAL_RELAY_NATS_CREDENTIALS_FILE` | 专属 NATS credentials 文件，不能回退读取普通客户端凭据 |
| `APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_FILE`、`APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256` | 必填的 current-v2 public-only 制品及独立可信摘要；不得从同一不可信文件自算摘要后作为验证依据 |
| `APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_FILE`、`APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_SHA256` | 重叠期间成对配置唯一 previous-v1；缺一项、摘要不符、绑定不符或到期均拒绝专用就绪 |
| `NATS_URL`、`NATS_CA_FILE` | Broker 地址与信任配置可共享，但身份必须分离；生产保持 TLS 和服务端名称校验 |

专用数据库身份只有固定受控函数 EXECUTE，无 Outbox/sidecar/control 直接读写、DDL、owner 成员关系或角色委派。公开脱敏 inspect 由 `account_service` 执行，不向其开放 sidecar 原字节。凭据只从受控 Secret/文件投递，不能写进命令行、仓库、工单或普通日志。

专用依赖初始化失败时不回退，普通循环仍可运行。当前进程在启动时建立专用客户端：修正启动配置或补齐依赖后须受控重启该实例并重新验专用就绪，不能仅凭普通探针恢复判定授权链已恢复。

生产 Compose 要求 previous 的 FILE/SHA256 两变量都显式声明：无重叠期时都设为空字符串，有重叠期时都填入有效值；未设置或半配置均拒绝。模板使用 `${VAR?说明}` 而非带默认值的形式，不能依赖隐式空值启动。

公开制品单独只读投递至 `/var/run/public/pinvou/nats-authorization`，不得与 `/var/run/secrets/pinvou/approval-nats` 私密身份目录混用。该目录只允许当前配置的 current-v2 及可选 previous-v1 文件；不得复制整个身份包、放入 seed/credentials、摘要附件或其他文件。撤除 previous 时也必须从该公开投影目录移走旧文件，原保管包仍保留。Compose 使用 `NATS_PUBLIC_AUTHORIZATION_DIRECTORY`；Helm 使用 `approvalRelay.natsPublicArtifactConfigMap`，通过逐文件 `subPath` 挂载提供 regular file，避免整卷 ConfigMap 的符号链接投影被校验器拒绝。Helm 的可信摘要分别来自 runtime Secret 的 `account-approval-relay-nats-current-public-keys-sha256` 与可选 `account-approval-relay-nats-previous-public-keys-sha256`；`approvalRelay.previousV1Enabled` 控制 previous 文件及配置是否成对启用。`subPath` 不自动更新，制品、摘要或 previous 配置变更后必须受控重建实例。

## 发布顺序与停步条件

以下步骤由 GSA-38 发布任务在取得明确部署授权后执行；本项不自动切换生产。

1. 核验备份、制品、迁移身份与专用数据库/NATS 凭据；先离线准备并验证 v2 身份包及唯一 previous-v1 绑定。不得覆盖或把新输出写入旧保管包目录。
2. 在唯一 control 仍为 `LEGACY / RUNNING / approval_epoch=1 / authorization_relay_epoch=1` 时执行 Account migration `000052`。迁移必须通过 sidecar 完整性及 owner/ACL 校验；缺失或畸形的冻结字节不能绕过预检。
3. 受控应用 v2 与唯一 previous-v1 的有界重叠配置。v1 独立合同仍为六身份；v2 为七身份，五个无关角色沿用原 key，仅轮换普通 Account key 并新增专用 key，去重后重叠为八个唯一 key。新普通 Account 与 operator 身份不得发布授权主 subject 或自动死信 subject。
4. 滚动部署新 binary，并投递专用数据库及 NATS 身份。核验普通服务就绪与授权专项就绪，分别检查两组积压；任何缺配置、权限漂移或专用探针失败均停止后续切换。
5. 在重叠截止前停止旧副本，确认旧 claim 和旧 handler 归零。移除 previous-v1，按环境已验证的方法应用 broker 配置并断开旧连接；实际证明旧 seed 无法重连/发布，而新身份仍可正常发布。
6. 只有旧身份撤销证据、旧副本/claim/handler 归零均成立，才能推进 QUIESCING；后续 FENCE、exclusion、reconciler 及切换工具属于 GSA-09/GSA-38，不由本手册提供直接 SQL 绕过。

缺少任何证据均停在当前安全状态，不通过删除 Outbox、重置 epoch、放宽权限或恢复旧身份来强行推进。重叠期限在生成时限定为 5 分钟至 7 天；渲染时截止前仍可接受有效 previous，截止时拒绝。静态 NKey 配置不会自行过期：操作人员必须在截止前完成移除与撤权验证，生成/渲染成功不能充当撤权证据。

本机隔离 NATS 证据采用受控 restart 断开旧连接，不证明无停机 hot reload 或生产撤权。正式环境若使用 reload，仍必须实际核验旧连接和重连均被拒绝。

## 探针、指标与恢复

普通 `/health/ready` 只反映普通循环依赖，不把授权依赖故障升级为整个实例驱逐。Account 另提供 `/health/ready/approval-authorization`；发布门禁必须显式检查该路径，不得只看容器普通健康状态。

专项就绪每次重新读取公开制品，核验独立可信摘要及 v2/previous-v1 绑定；仍配置 previous 且重叠截止已到时返回 503。完成 broker 撤权验证后，成对移除 previous 配置并重建实例，current-v2 保留历史截止字段不阻止恢复就绪。此检查验证的是本地受信发布制品及期限，不证明 broker 已应用对应 ACL，也不会自动撤销旧 key；不能通过提前移除 previous 配置替代第 5 步的实际撤权证据。

专用 NATS 客户端是仅发布客户端，不取得 stream INFO/创建/消费权限；其 Ping 只验证连接，不证明资源规格正确。固定资源仍须由独立受控 operator 校验，不能为了让专用客户端自查而额外授予管理或元数据 API 权限。

普通 `pinvou_outbox_*` 指标不包含授权分区。授权分区分别查看：

- `pinvou_authorization_outbox_pending_events`：未终结授权事件。
- `pinvou_authorization_outbox_active_claims`：租约有效的 claim。
- `pinvou_authorization_outbox_reclaimable_claims`：租约已过期但仍待恢复的 claim。
- `pinvou_authorization_outbox_unclassified_dead_letters`：待后续治理处理的授权终态死信。
- `pinvou_authorization_relay_active_handlers`：本实例当前正在执行主 subject 或自动 DLQ Broker.Publish 的数量；成功、确定失败、结果未知或取消返回后均归零，不包含仅领取但尚未调用 Broker 的任务。

指标值必须与探针和采集时间一起判断，采集失败时不能把旧值或零值当作无积压证明。

FENCE/停流后的排空检查必须逐个 Relay 副本确认进程内 `active_handlers=0`，并同时核对数据库 active/reclaimable claim 为零，不能用其中一项替代另一项。handler 指标只是当前进程观测值，进程重启即重置；它不是持久安全依据，不能替代数据库锁、epoch、租约或撤权证明。正常发布中的非零计数不单独导致专项就绪返回 503。

Claim 和发布事务都遵循 control 共享锁先于事件行锁；发布时重新核验 token、relay epoch、租约及冻结字节绑定，并在有界 Broker 调用和 finish 全程保持事务锁。租约回收产生新 token，迟到 worker 不得继续发布。主消息沿用原 event ID 为 Msg-Id，自动死信用稳定派生 Msg-Id，两者均发送原 sidecar 字节，不从 payload 重新序列化。

Broker 或 COMMIT 结果不确定时只能查询持久 outcome，不能推测成功、计作确定失败或立即投死信；未终结事件通过原 ID/原字节自动恢复。自动租约恢复不是人工 replay。SAFE_DRAIN 仅在 `SAFE_ROLLBACK + RUNNING` 且事件为前一 approval epoch 时成立，不能由调用者参数临时开启。

## 永久人工 replay 禁令与回滚

授权事件的 `event-replay --inspect` 只返回脱敏元数据、摘要及 `replayable=false`。任何 confirm/workload 人工 replay 均返回 `AUTHORIZATION_EVENT_REPLAY_FORBIDDEN`，即使已有同 key 的历史幂等记录也不能绕过。数据库同时拒绝授权 replay audit INSERT、终态清除、attempt reset 和重新排期；Portal/HTTP 不新增重放入口。普通事件仍遵循当前 `account-platform/cmd/event-replay` 的受控流程：先 `--inspect --event-id <UUID>` 读脱敏元数据，只有普通事件可在原 workload 身份、明确 `--confirm`、inspect 原 `--expected-digest`、既有 `--idempotency-key` 和 `--reason` 合同齐全时执行；授权事件仍永久禁止人工重放（原 event-delivery.md 当前未恢复）的受控流程。

`000052` down 只允许尚无专用 claim 审计事实、两个 epoch 均为 1 的初始兼容状态。任何专用 claim，即使已经 published/retry，均保留 token/epoch 事实并阻止 down；两个 epoch 中任意一个曾推进，返回原 mode/state 也不能恢复 down 资格。安全 down 必须恢复 51 的权限/函数并验证旧 writer/backfill，不能只检查 schema 版本号。

越过不可逆点后保留 52 及全部事实，使用已验证兼容的应用回滚策略，不恢复无分区、无专用失败关闭能力的旧实现。不得把授权死信改回 pending。GSA-08 consumer 持久尝试、GSA-09 治理恢复和 GSA-38～40 正式切换验收仍是独立任务。
