# Token 普通 outbox 发布

本项修复 #1632 的 Token manual-review 事件承载。领域写方和事件格式不变；新增独立 Token 发布身份、可选部署及发布确认。默认 Compose/Helm 装配继续只运行 Account、Billing relay。这里不启用资金执行、不安装新的业务消费者，也不改变历史保留或清理政策。

## 前置条件与迁移

先使用已有独立运维身份执行 [Token 数据库准备](token-database-bootstrap.md)，再通过正式 `migrate` 的 Token 入口应用完整受钉定迁移包。已安装数据库直接向前升级，不重跑 bootstrap。新增 `900011_outbox_publication_generation` 给既有事件补充默认值 0，保留原事件、重试和租约字段。Down 明确拒绝；恢复采用向前修复，不能删除发布围栏。

数据库运行连接使用 Token 域既有 `token_gateway` 角色；独立保管 relay 的 DSN/pgpass 文件，不能给它迁移凭据或其他域 DSN。relay 固定每个连接的 `search_path=pg_catalog,public`，启动核验真实 outbox 的 `token_schema_owner` 所有权及 BIGINT 非空发布代次。缺少迁移或域身份错误时拒绝启动。

## Broker 授权

使用已有 v2 公共身份清单及独立可信 SHA256。只从原托管系统向此工作负载签发清单中保留的 `token-outbox-relay` NKey，禁止复制 Account、Billing 或 Token command executor 私钥。公共清单与私钥分目录；SHA256 必须经独立核验，不能把现场自算摘要当批准。

`approval-resources` 新增三个离线命令，环境变量和输出文件合同沿用原 render 入口，且要求 `ACCOUNT_PLATFORM_PROFILE=models`：

| 原已批准载体 | 对应命令 |
| --- | --- |
| fresh | `render-token-events` |
| existing-broker | `render-existing-broker-token-events` |
| refund-upgrade | `render-refund-upgrade-token-events` |

选择与现存部署一致的载体。它保留所有已有身份权限和原资源 manifest，只增加 Token 发布者：发布 `token.>`、`account-platform.dead-letter.token.>`，查询指定 stream，订阅自己的 reply inbox。没有其他域发布、审批授权、consumer 管理或 ACK 权限。原 render 命令继续不签发 Token 发布者。

按已有受控流程审核并安装生成的 broker 配置。资源仍由独立 operator 预置，relay 不会自动建 stream、consumer 或降级到其他凭据。不要用 fresh 配置覆盖 existing-broker/refund-upgrade 的消费权限。

## 可选部署

当前模块化生产 Compose 的 `models` 配方追加 `token-events` overlay（`compose.token-events.yaml`）。旧的 staging/legacy Compose 装配仍不支持这个 relay。

- `TOKEN_RELAY_PRIVATE_ENV_FILE` 只含 `DATABASE_URL`；连接为 Token 运行角色，TLS `verify-full`。
- `TOKEN_RELAY_DATABASE_DIRECTORY` 只含该连接的 pgpass；`TOKEN_RELAY_NATS_DIRECTORY` 只含 `nkey.seed`，运行用户 65532 可读。
- `APPROVAL_PUBLIC_IDENTITY_DIRECTORY/current.json` 为批准的 v2 公共清单，`APPROVAL_PUBLIC_KEYS_SHA256` 为独立可信摘要。
- 使用原配方中的固定镜像、基础 CA 和 TLS NATS 地址。启动依赖 `token-migrate` 成功，私有目录只读挂载。生产 preflight 会校验独立文件、域 DSN、公共身份摘要和装配来源。

Helm 显式设置 `tokenEventRelay.enabled=true`。需在现有 `global.runtimeSecret` 独立提供 `token-event-relay-database-url`、`token-event-relay.seed`、`token-event-relay-nats-current-public-keys-sha256` 三个键；`approvalRelay.natsPublicArtifactConfigMap` 中提供 `current-v2.json`。Secret 源只投影给同镜像非 root init，`prepare-nats-seed token-event-relay` 将验证后的 seed 装入内存卷，主容器只读。公共文件采用 subPath 实体挂载；清单/私钥轮换需要受控 Pod 重启。

两种装配均设置 `EVENT_SOURCE=token`。运行时在首次读取私钥时核对其公钥与可信清单，重连继续固定该身份。Token 不会打开 Account 审批授权通道。

## 验证与恢复边界

检查 `/health/ready`、现有 relay 指标和 Token outbox 的待发布/死信状态；错误日志使用既有脱敏错误码。就绪只证明连接、身份和安装前提，不证明积压已清空，须另核对实际事件确认。

隔离验收入口为 `npm --prefix account-platform run test:token-outbox`，必须提供明确归属的空 PostgreSQL 和可执行 NATS 路径。入口拒绝必需用例跳过或缺失；CI 在现有 broker 验收后独立运行。测试经过原 manual-review 写方、真实公开迁移和运行角色、TLS NATS、真实 `event-relay`，校验原 envelope、跨域拒绝、错误初始身份/数据库拒绝及本地确认失败后新 worker 恢复。用于构造原案例的 REPAIR_REQUIRED 行为隔离合成数据，没有调用供应商或资金执行器；不是生产发布回执。

投递沿用至少一次及代次/租约围栏。broker 已确认而本地确认失败时，保留待发布事件，租约到期后重试同一发布标识。JetStream 仅在其去重窗口内去重，业务消费者仍需保持幂等；不得把 broker 去重解释为永久恰好一次。达到原重试上限后进入原死信流程，不自动重做资金动作。事件保留、Stats 容量及历史清理仍是 #1632 的独立待办，本次不增加 TTL 或删除任务。

本轮核对既定部署账本及并行 PR 清单后，将此未登记部署的候选迁移调整为 `900011` 以消除重号；原 SQL 内容和正式已安装历史保持不变。
