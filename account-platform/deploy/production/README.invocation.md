# 个人模型调用部署

在已核验的个人、workload、财务、对账、模型管理叠加层之后加入 `compose.invocation.yaml`。沿用原隔离网络、可信 CA、私有 DSN、凭据 Portal、真实目录和定价。普通个人模型列表、调用、用量历史、取消、JSON/SSE 结果重放和签名 readiness 在本波首次启用；组织调用仍拒绝。

`TOKEN_PORTAL_IMAGE` 必须指向同源已审 F05 Portal 镜像，包含当前生成的14项静态制品，以及个人模型目录、用量分页查询和 UUID 取消的准确 BFF 路径；不能继续使用基础模板所述 F01 或 F04 的 Portal 二进制/静态包。该叠加层复用原变量、私有会话和 CSRF 配置，不增加公开聊天转发。

已有来源明确模型引用和本地模型配置沿用原迁移及解析边界；既有 typed 引用不被迁移覆盖，旧内置名称只有与当前目录标识或显示名称实际匹配时才能解析。缺失选择或旧名称不再匹配目录时，须显式重新选择，不自动 seed 目录或替用户选择目录首项。没有显式会话选择或有效全局选择的普通新对话须先从当前真实目录选择模型；既有定时任务快照、明确的评估或模型覆盖仍按各自原绑定解析，不据此统一阻止所有升级会话。原目录模型不能继续时，客户端已提供显式选择当前可用模型并建立可追溯替代分支的入口，原对话和消息保留；这不把替代分支等同于原模型身份或自动迁移。替代分支只绑定自身所选模型，不修改全局默认；显式设置默认模型入口仍更新随后新建会话所用默认。

Token 公开迁移目录为当前31对不可变迁移。新增 client readiness、admission indexes 和 usage history groups；不要加载未来注销或组织生命周期迁移。迁移前仍由独立 operator 完成原数据库 bootstrap，普通 migrator 不获得数据库 CREATE。`000017_usage_admission_indexes` 的五个索引沿用原普通 `CREATE INDEX`，可能等待锁并阻塞写入，部署前须核查长事务并安排维护窗口；统一 migrator 的连接和迁移上下文总预算为2分钟，可因超时退出，失败的当前迁移事务会回滚。这里没有局部 `lock_timeout` 或 `CONCURRENTLY`，也未完成长期生产数据量的建索引容量实测。

第19个 Token 策略 `TOKEN_USAGE_RECONCILIATION_RETRY` 是 AuditOnly v2/snapshot2。它将既有用量排入证据对账，不重新调用模型，也不取得审批 durable。Billing3 和 Token3 审批消费者保持原资源、身份和授权边界。先离线进入 SAFE、完成 quiescence 和当前冻结证据/资源 readback，使用当前 PolicySetDigest 和十九项 HandlerSetDigest 重新发行并激活；十八项旧 ACTIVE 摘要不能视为十九项已激活。

Gateway 对 Billing 增加且仅增加 `internal.billing.hold.extend`、`internal.billing.hold.commit`，对 Account 增加 `internal.client-compatibility.read`。Account 对 Token 的内部 readiness 读取与签署只授予已有 Account workload 身份，分别为 `internal.token.client-readiness.read`、`internal.token.client-readiness.attest`；这些路径不能通过公开 edge 调用。令牌仍由已交付的私有 workload agent 发行，不能复用普通用户令牌或 executor 身份。

Gateway 已有私有 `TOKEN_GATEWAY_PRIVATE_ENV_FILE` 需提供 `CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64`（32字节 seed 或64字节 Ed25519 private key 的标准 Base64）、`TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN` 和真实 compatibility release manifest 配置。NEW_API 只读 evidence actor 的 `TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID` 同样由该私有 env_file 提供；可选 `TOKEN_RECONCILIATION_NEWAPI_BASE_URL` 也可置于该文件，缺省时沿用原 provider base URL。叠加层不应以空 shell 默认值覆盖这些私有文件值，不额外要求重复 export，也不把模板验证当作真实 Docker 部署。文件只读、私有权限、仅该进程可读；公开模板仅保存 key ID、版本与时间策略，不打印 env 或秘密。`CLIENT_READINESS_ATTESTATION_KEY_ID` 与该密钥的公开核验身份一致。客户端必须验证签名、环境、制品 SHA、版本和到期事实，不能把普通 HTTP health 作为 GO。

readiness 签发、release manifest 和发布 CLI 的版本格式与排序统一采用 [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html)：核心三段及数字预发布标识不允许前导零，空标识、空白或非法字符拒绝；同核心版本的预发布版低于正式版，数字按完整数值比较而非浮点数。构建元数据只在排序时忽略，签名、制品和版本身份仍核对完整原字符串，不把不同 build 当成同一个签名身份。release manifest 仍保留原 N/N-1/N-2 支持与公告期限，发布 CLI 按实际最低和最高版本判断，不新增“所有预发布版一律禁止”的策略。`clientcompat.Policy.Wrap` 的写入检查沿用既有读取及精确集成路径例外；本波没有新增该中间件的生产挂载，当前生产接线是 compatibility 元数据、readiness 签发及发布 CLI，不能用中间件单测声称所有线上 HTTP 写入已受该门禁保护。

供应商必须为已经准入的准确 origin，CA 与模型调用和 evidence 查询一致。NEW_API 使用真实只读 evidence actor/user ID/token；OPENAI_COMPATIBLE 没有可信查询证据时保留 UNKNOWN/REVIEW，不推断外部成功。私有管理 origin 不暴露到浏览器。现有公共 `/token/` edge 仍剥除内部和管理前缀，仅经用户身份/个人权限调用所交付 `/v1` 路径。

Gateway 启动真实 usage reconciliation、replay retention、usage history projection 和 Token review SLA worker；Billing 同进程启动现有 hold expiry 处理。JSON/SSE 的可信用量先持久化，再提交 Billing 一次性 hold/ledger 回执；缺失或不可证用量保留资金未知和对账，不以响应已送达认定已结算。

静态模板和受控 HTTP/公开 PostgreSQL 验证不能证明真实 Compose、NGINX、Keycloak/OpenBao、外部供应商、生产 ACL 或完整 E2E。按已确认延期范围保留这些限制，部署与 required CI 仍需独立实际批准。
