# 模型与 Token 发布计划和可选部署配置（APT-137 G0～G4）

状态：IN_PROGRESS / ENV_PENDING / NOT_DEPLOYED。当前包含已提交 G0 新域基础、G1 离线计划、G2 可选配置、G3 受控工具与本组 G4 安全入口/只读探针；后续 G5～G6 尚未交付。DNS 未添加不阻塞本地代码验证/提交，但实际部署仍须完整环境证据；本工具不授权推送、部署或任何现场写入。

## 1. 实际入口与依赖

`node scripts/account-platform-model-token-release-plan.mjs --plan <本地JSON文件>`

只接受上述两个参数，拒绝超过 1 MiB 的 JSON，拒绝任意层重复/转义重复 key、非法 JSON、未知字段与类型强转。依赖仓库既有 Ajv/ajv-formats，以及本组 `contracts/deployment/model-token-release-plan.schema.json` 和 `model-token-release-evidence.schema.json`；无网络请求、无子命令/数据库连接、无 apply 或 GO 模式。成功只输出 `PLAN_VALID / ENV_PENDING / NOT_DEPLOYED`；失败非零退出且仅稳定错误码，不打印原始输入或环境秘密。

## 2. 提案校验而非状态授权

计划绑定候选 commit、镜像/配置/迁移清单/策略/handler 摘要及批准/操作者引用。PRODUCTION 接受 production/production-new，CLOUD_UAT 接受 staging；隔离凭据 origin 仅接受 G0 新同环境域，启用隔离 gate 必须配置它。[DNS 清单](model-token-dns.md) 是待实施输入，不是生效证明。

activation 的 NOT_INSTALLED 要求 epoch/安装摘要全 null；真实 PRE/0 两摘要为 null，导出的 `activationFromGovernanceSnapshot` 只在明确 PRE/0 将 SQL NULL 或 CLI 空串归一，缺字段/不安全整数/非法摘要拒绝。ACTIVE/SAFE 要求正的安全整数 epoch 和与候选相符摘要；不支持的摘要跨版本切换拒绝，不自动迁移或转换状态。声明 ACTIVE 并不构成数据库强读或可写授权。

十三个 gate 全部显式 boolean，默认全 false、既有父子依赖由 schema 校验。写动作声明要求 ACTIVE；隔离凭据要求既有 Token 详情/动作前置及固定 origin；读详情、价格投影和编辑等保持独立开关。它们仍是既有部署配置，不新增业务/客户端 cohort 开关，本组不修改 Compose/Helm 或运行中环境。

两 runtime 身份必须分离、不复用 retired/保护组/运维身份，名称为最多 63 字节的小写无引号 PostgreSQL 标识符；token_gateway 是权限组不是本代登录。Account policy 运维角色声明必须包含 SHARE_NOWAIT 锁合同，不能将其误称纯 SELECT 身份。这里只校验声明，实际非超级用户锁权限、跨库依赖、NOLOGIN/成员链和旧会话 fence 属后续受控角色与现场验收。

INTERNAL→LIMITED→FULL 固定有序，流量严格增加且末阶段 100%；观察时长/最低样本为正安全整数，cohort/审批/SLO 引用完整，量化 advance_max 不大于 pause_above，未经授权效果阈值必须为零，容量不超过既有 0.70/0.85 边界。阈值和观察参数是提案，不代表已经采集样本或达到推进条件；本组不读取测量报告或决定放量。旧链接真实 GA 时间/版本必须同时有值或同时未知，不以提交日期冒充 GA，兼容保留判定留后续实际证据。

## 3. Envelope 的有限保证

导出 `validateModelTokenEvidenceEnvelope` 仅校验本组闭合 envelope schema、与计划完全相同的候选/环境、合法起止且结束不晚于输入 now、独立执行者/复核者引用。返回 `ENVELOPE_VALID / ENV_PENDING / NOT_DEPLOYED`，不是对 status、报告内容或来源真实性背书。即使 envelope 写 PASS，也没有验证报告文件存在、实际字节 hash、签名、时效上限/完整 case 或观察指标；不得用于生产放量。完整离线工件与外部信任验证在后续 G5 交付，不引用尚未提交的模块。

## 4. 精确候选验证与后续交付

本组具名回归：`node --test account-platform/test/contract/model-token-release-plan.test.mjs`，覆盖十三 gate、所有依赖、环境/新域/状态摘要、身份、真实 PRE 归一与类型、观察阈值、envelope 绑定、重复 JSON 和默认 CLI 无 apply/GO。根在 G0 加本组四个代码/schema/test 文件的精确候选 `e96f6387c539d3a44296fea3f1da94df1103f030` 实际运行九项 Node 全 PASS/0 SKIP（1.784 秒），exit 0；本组最小文档另交 B 复核。此为当前四文件依赖已齐的真实运行，不继承原整树 PASS。G5 前其他两项旧 Node 基线断言仍可能失败，须如实记录。

后续仍按 G2 部署配置、G3 受控角色/CLI、G4 安全代理/HTTPS/scope/实际脚本 VM、G5 原始证据与兼容、G6 持续 CI 顺序独立验证/签署。共同文档随已存在接口更新，不能把后续实现提前写成已交付。DNS/TLS、OIDC/workload、KMS 全用途旧密文、同一 NATS 集群/ACL、备份/HBA、实际灰度/SAFE 回滚仍需现场证据；本地 plan PASS 不能关闭 APT-137/UI-58 或解除部署前置。

## 5. G2 可选部署配置：只做结构验证，不执行上线

G1 已独立提交 `33209285`。本组使用现有 Go Dockerfile的 SERVICE 参数构建 credential portal、executor 与 token-governance，不增加业务服务职责：gateway 内仍唯一拥有两个 worker。Compose overlay/profile 需显式加载；Helm 三新组件默认 false，governance Job suspend=true、无 hook/自动 apply/重试。runtime 与运维 Secret 分离，executor 不获得数据库/KMS，Token Relay 与 gateway 同代 writer 共同停止/fence/恢复，不能沿用已退休登录。实际角色准备与受控启用程序见第6节，但不得据模板直接切换，现场操作仍须明确批准与独占窗口。

公开配置仅用于解析。Compose 在 deploy/compose 目录运行 `docker compose --env-file .env.example --env-file model-token.env.example -f compose.yaml -f compose.model-token.yaml --profile model-token --profile governance-operations --profile real-newapi config --format json`，输出保存到本次受控临时文件，再从仓库根运行 `node scripts/account-platform-model-token-deployment-check.mjs --compose-render <文件>`；不能读现场 .env 或用这些不可用占位值 up/run。Helm 对已有默认、staging、production-new 与新增 `values-model-token-test.yaml` lint，渲染 fixture 后用同一 checker 的 `--helm-render <文件>`。真实 Docker/Helm 解析不是静态字符串替代，结构合法仍输出 DEPLOYMENT_CONFIG_VALID/ENV_PENDING/NOT_DEPLOYED。

十三 gate 环境值透传到既有所属服务，不改变领域逻辑。正式配对为 account.pinvou.com / credentials.pinvou.com，公开 UAT fixture 配对为 account-uat.pinvou.com / credentials-uat.pinvou.com。credential OIDC issuer 与 authorization/token/JWKS 必须精确同 origin HTTPS、无任何显式端口，不采用普通 Portal 内网 HTTP JWKS路径。外部可信短期 workload token-file/投射身份和各服务独立 NATS creds 必须就绪；不得继续共享开发静态 Token，也不在本任务临时造签发器。

七个 broker 客户端使用同一受控外部 TLS NATS；Helm 开新治理组件时拒绝内置匿名开发 NATS，URL/creds/Ping 不证明真实 ACL 或 JetStream 历史。受限 workload 排除旧宽策略选择器，checker 核全部命中允许规则；目标 New API 明确只允许 credential 与 gateway 所需入口。Compose 网络本身不充当防火墙，依赖 CIDR/Host/TLS 身份与可信出站边界仍需现场证明。credential 独立 loopback18108、端口/IP范围重叠必须拒绝；所有 runtime/附加容器禁止运维URL，控制Job/entrypoint必须固定只读默认，不可用wrapper或init容器自动执行。

配置从 local AES 切 remote Secret Provider 前，须由既有受控KMS流程证明全部持久密文（provider credential、事件流/结果回放和dispatch等）原 key_id、格式、AAD 可读及备份可恢复；没有本地解密 fallback，不能清旧KEK/删除历史后声称迁移成功。配置 Secret/issuer/ACL/DNS/有效证书/代理与HBA缺项均保持 ENV_PENDING，不绕过启动验证。本组只提供结构/制品，G3 操作包装器由第6节说明，G4 安全反代/HTTPS/scope工具见第7节，G5 验签工具尚未交付；既有 `token-governance` 默认 check 可独立构建，但任何 apply/激活须遵循本节及第6节受控步骤并取得现场授权。

根对本组精确候选验证 `node --test account-platform/test/contract/model-token-deployment.test.mjs`，并核对相同输入/blob下真实Helm/Compose解析、语义变异和新镜像构建证据；结果另记，不自动把原整树PASS转为本组证据。部署说明详见 [Compose](../compose/README.md) 与 [Helm](../helm/account-platform/README.md)。后续实际部署/灰度/SAFE流程仍待 G3～G6 与现场证据闭合，不 push/deploy，不标 UI-58 DONE。

根对 G1 加本组十八配置/源码文件的精确候选 `d356fee020a6af006058f3cf33b3f89b9a6b4cd7` 实际运行 G1/G2 合并十八 Node 全 PASS/0 SKIP（1.846 秒），exit 0。本组十八文件、G1 plan/schema 与依赖服务构建源码相对于根第六隔离实测候选 `e371` 均无差异，因此仅在此相同输入范围继承真实 Helm 4.2.3 四 profile lint/render checker、Compose config/checker及三个新服务镜像构建的分项证据；不是把整树或现场 PASS 继承到本组。七份最小文档另交 B，G2 仍不是实际部署或 G6 CI 全部通过。

## 6. G3 独立受控角色与单次发布操作

代码可构建/验证不表示现场已批准执行。以下工具始终分开调用，不把 inventory、fence、prepare、迁移/cap、CAS、enable 或服务启动串成自动流程。先只读核验账本/31表：缺31的首次安装先受控停写/fence/准备/连续前向迁移，再强读真实 PRE/0；已有 ACTIVE 必先以匹配当前持久摘要的旧CLI确认 SAFE，再进行身份/迁移准备；已 SAFE 保留待办/只读恢复。只有 PRE→ACTIVE、ACTIVE→SAFE、SAFE→ACTIVE，不允许ACTIVE→ACTIVE或回PRE。改变治理摘要且没有已批兼容合同应拒绝，不降级校验。

SAFE请求发出不等于CAS完成；先持activation共享锁的领域事务可先提交，CAS等待，只有CAS成功后才保证旧epoch无新效果。等待/超时/未知先以原request-id查询，不盲重apply。首次quiescence不能套到SAFE恢复清空历史PENDING/MANUAL_REVIEW。实际执行前记录备份/可恢复证据、候选/迁移摘要、审批/维护窗口、全部writer含Relay、两候选及退休角色清单；中间不临时复活旧登录。migrator/其他短期运维LOGIN必须在完成后关闭并清会话，再启用候选身份；能力/worker/consumer/readiness与gates仍由实际部署独立验证。

### 6.1 本组受控工具与实际调用合同

新增 `cmd/token-release-prepare` 是独立运维程序，不进入领域服务或自动启动路径。默认 `inventory` 只读；`fence`、`prepare`、`enable` 是彼此独立的显式操作，必须分别提供 `--apply`、精确目标数据库名、批准引用与本次候选/退休角色集合。没有操作会调用下一操作或隐式激活。数据库连接仅取受控环境变量，输出闭合安全状态/计数，不回显 DSN、密码、原始 SQL 错误或凭据。角色名采用首批 63 字节 ASCII 合同；角色是集群级对象，单库绑定不能授权修改其他域共用角色。发现清单外可写 LOGIN、跨库共享身份或无法完整核验共享边界必须阻断并报告脱敏缺项，不自动 ALTER 所有盘点结果或误停 Account/Billing/DBA。

- `inventory` 在只读事务核对实际数据库、账本与治理表存在性、角色属性/递归成员链、写权与存量会话。31 缺失只报告 `NOT_INSTALLED`，不伪造 PRE。若已安装则读取真实状态；账本/表不一致或不支持的状态失败关闭。
- 同一目标数据库的工具操作以固定事务 advisory lock 串行；修改前必须验证全部目标而非边验证边写。31 已安装时，`fence`/`prepare`/`enable` 的状态、epoch、角色集合核验使用 activation 行锁，并保持到同一事务的全部角色/owner 修改提交。若 SAFE CAS 先提交，旧 ACTIVE 选择不能开门；若 enable 先持锁，角色启用先提交、SAFE 随后提交，之后领域 epoch guard 仍禁止旧效果。缺31首次准备在维护窗口固定迁移账本/表存在性并临界重验，不能与迁移并行；跨库共享身份检查不是跨数据库原子事务，必须先满足已批准独占 DBA 维护窗口，无法保证即阻断。只有 fence 已说明的 NOLOGIN 提交后显式清会话是分阶段例外。
- `fence` 只对显式批准的退休登录执行 NOLOGIN，不删除角色、成员关系、历史数据或密码。集合必须覆盖当前所有可经直接/递归 SET ROLE、表写权或 SECURITY DEFINER EXECUTE 获得 Token 写入能力的普通旧登录（含复用 gateway 身份的 Relay），不能漏一个旧登录后声称完成。当前 DBA 会话为明确受信例外；其他受信会话亦必须盘点，不能默默忽略。先提交 NOLOGIN 后才可用额外显式 `--terminate-sessions` 清退这些精确退休角色的会话，操作失败保留已关闭登录并返回未完成，不把部分成功当 quiesced。没有终止选项时只报告剩余会话并拒绝后续准备。已安装且仍 ACTIVE 时拒绝此升级准备路径，先用匹配摘要的旧 CLI 确认 SAFE。
- `prepare` 要求旧写入登录已 fence、零旧会话、领域表 NOWAIT 锁可取得，且状态为未安装、PRE 或 SAFE。可信 DBA 为已有卷建立/验证 NOLOGIN 的 `token_schema_owner`、`token_cap_policy_deployer`、`token_migrator`、`token_policy_operator`；migrator/operator 为 NOINHERIT，migrator 仅具已有迁移所需 gateway/cap/schema owner 成员关系。仅把目标数据库及 public schema 的控制面 owner 移交 schema owner，保留普通领域表 owner 和既有数据，不使用 REASSIGN OWNED 或改历史迁移。DBA 预装/核对 pgcrypto；29 前仅保留旧迁移所需 gateway CREATE，29 已安装则不得恢复 runtime CREATE。两候选 writer 为全新 NOLOGIN、无保护角色继承的独立名称，统一继承 gateway 业务组；组本身保持 NOLOGIN。已有不相符角色/成员链不自动修成更宽权限，拒绝并交 DBA 处理。
- 迁移 29 要求受信 migrator 与 policy operator 在操作窗口可 LOGIN；工具不生成、读取或设置密码，也不自行开放这两个运维身份。DBA 通过既有短期凭据流程准备后显式启用，迁移/显式 cap bootstrap 完成即关闭 policy operator 登录并清退会话。migrator 的 `pg_read_all_stats` 仅在需要的盘点窗口由 DBA 授予并记录，结束只撤回本次新增 membership。相关密码、认证方式及窗口证据缺失保持 ENV_PENDING。
- `enable` 不创建 ACTIVE。首次分支要求真实 ACTIVE、精确 expected epoch、候选集合等于 approved_runtime_roles、全部候选合法且 NOLOGIN、不继承保护角色；同事务持 activation 行锁后全量 ALTER ROLE LOGIN。重试分支仅在同一 epoch/集合且全部候选一致已 LOGIN 时只读确认，不执行 ALTER；混合 LOGIN/NOLOGIN、跨 epoch 或退役身份一律拒绝。响应 UNKNOWN 先 inventory/读取实际状态，不盲目再次写入。SAFE 与首次启用、重复确认、部分身份状态都纳入真实并发 PG 回归。实际服务/Relay 启动与 readiness 验收仍是独立部署步骤。
- 本批认证准备检查仅支持 DBA 已配置的 PostgreSQL SCRAM verifier：服务器 SQL 在受信连接内计算 verifier 存在且格式受支持、有效期明确且尚未到期的布尔值；查询结果不取出 verifier/密码内容，程序不读取或输出其字节，不接受操作者填 `ready:true`。其他认证方式或无法从权威目录确认时阻断。此布尔只证明服务器端已配置，不证明 pg_hba、网络或所挂 Secret 实际匹配；后续必须用受控真实登录探针证明认证与权限，不能将测试集群 trust 连接或仅存在 verifier 记为现场认证成功。

Account policy 锁单独提供只读执行探针：使用现场 `ACCOUNT_POLICY_DATABASE_URL` 的真实非超级用户身份，在事务中取得 `approval_command_policies IN SHARE MODE NOWAIT` 并读取现有策略，最后 ROLLBACK；不通过超级用户替代证明。PostgreSQL 的该锁模式仅 SELECT 不足，最小直接表授权需要 SELECT+UPDATE，因此此身份不是“技术上无法写”的只读账号，必须是短期、受信、独立运维身份，不能赋给 runtime。具体授权由 DBA 批准，工具不替 Account 自动扩权；权限不足、锁竞争、身份不符均拒绝。

新增发布包装器只按闭合操作输入调用既有 `migrate`、`token-policy-bootstrap`、`token-governance`：默认只校验计划而不启动子进程；执行必须另显式选择操作及批准的本地控制制品 SHA-256。必须传入明确 `--direction up`、`--mode readiness/quiescence/bootstrap` 或治理参数，不能触发原程序的默认写路径。读取迁移前检查、cap readiness、治理 check/原 request-id 结果查询不产生后续转换；SAFE/ACTIVE CAS 与 cap bootstrap、迁移分别为明确批准操作，不编排成一键串联。子进程 stderr 和原始失败不透传，输出固定失败码；UNKNOWN 只使用原 request-id 查询，不能自动重试 apply。原 CLI、数据库状态机、幂等及期限均不修改。

专属单元/隔离 PG 验收覆盖已有卷和首次缺31分支、准备默认零写、角色冲突、递归旧 writer/Relay、NOLOGIN 后存量会话、明确终止/失败部分收敛、非法 ACTIVE 准备、候选保护链和错误 epoch 零开门、29 前后 CREATE 边界，以及非超级用户 Account SELECT-only 拒绝、SELECT+UPDATE SHARE 成功和真实锁竞争。固定角色测试在根准备的全新独立 PG 集群串行执行，不使用现场数据库；工具闭合性与实际环境角色/认证证据分开记录。

#### 工具调用和制品准备

从已批准候选的 `account-platform` 目录独立构建 `go build -tags=nomsgpack -o <受控输出路径> ./cmd/token-release-prepare`；与 `migrate`、`token-policy-bootstrap`、`token-governance` 控制制品分别保存 SHA-256、候选和审批引用。不得替换第二批挂起 Job 的 `SERVICE=token-governance` 或把 prepare 接入服务启动。制品存储必须受控只读，运行时校验 hash 不能替代制品目录的写入权限保护。

prepare 程序只从 `TOKEN_RELEASE_DBA_DATABASE_URL` 连接 DBA；`--operation account-policy-check` 单独从 `ACCOUNT_POLICY_DATABASE_URL` 连接真实非超级用户。所有调用必填 `--expected-database`。默认/检查调用不带 `--apply`；fence/prepare/enable 必须显式给出 `--approval-reference` 和 `--maintenance-reference`，角色清单使用排序、无重复的逗号分隔值。`--candidate-roles` 恰为 credential/gateway 两角色（Relay 复用 gateway），`--retired-roles` 是批准的旧角色全集，不自动由查询输出补齐。`--migrations` 指向本制品的 Token SQL 目录，工具验证已应用前缀 checksum；全新无领域表且无账本的数据库只报告 NOT_INSTALLED，任何已有表但缺账本均拒绝。

迁移连接与高权操作连接的 search_path 有不同职责：prepare 固定 `pg_catalog,public` 并限定业务表；既有 migrate 创建未限定的账本/领域表时使用 `public`（不显式后置 pg_catalog，内置对象仍由 PostgreSQL 隐式优先），不得让 runtime 向 pg_catalog 建表。Token migrate 显式 `MIGRATION_ROLE=token_gateway`，迁移 29/31 内部按既有 SQL 切可信 owner；不借 runtime 获得 schema owner 继承。ACTIVE CAS 完成后 migrator 也必须结束短期 LOGIN 窗口、清退会话，再由 DBA 执行 enable；仍 LOGIN 的 migrator 不会被 enable 的旧 writer 检查豁免。SAFE 时需要使用旧 CLI 的受控操作身份可另开短期窗口，但不得让旧业务身份重新登录。

包装器入口为 `node scripts/account-platform-model-token-operations.mjs --request <受控JSON文件>`，此时仅校验；唯一执行开关为额外 `--execute`。请求闭合字段为 `schema_version=1`、`operation`、`approval_reference`、`executable`（绝对路径）、`executable_sha256`、`expected_database`、`expected_account_database`（使用 Account policy 时必填，其余为 null）、`options`。可选 operation 仅 `migrate-up`、`cap-readiness`、`cap-quiescence`、`cap-bootstrap`、`governance-check`、`governance-active-check`、`governance-activate`、`governance-safe-check`、`governance-safe`、`governance-result`；未知字段、操作或参数拒绝。

- `migrate-up.options` 精确为 `domain`（account/billing/token）、绝对 `directory`、`manifest_sha256`、`backup_reference`、`backup_completed_at`（UTC秒精度）、`backup_sha256`。目录摘要由包装器导出的 `migrationDirectoryDigest` 对排序 SQL 文件名/原始字节 SHA-256 数组再取摘要，绑定 up/down 全包。URL 分别来自 `ACCOUNT_MIGRATOR_DATABASE_URL` / `BILLING_MIGRATOR_DATABASE_URL` / `TOKEN_MIGRATOR_DATABASE_URL`，子进程仅得到本域 DATABASE_URL；显式 `--direction up`，保持既有备份时效检查。默认 plan 只核对制品/目录，不宣称真实数据库已迁移或备份报告可信。
- `cap-readiness` / `cap-quiescence.options` 必须为空对象；`cap-bootstrap.options` 精确为 `max_per_owner`、`expected_version`、`idempotency_key`、`reason`，显式传既有 policy CLI 参数，URL 为 `TOKEN_POLICY_DATABASE_URL`。quiescence 使用获批的可观测运维身份，不因变量名误用权限不足的普通 runtime。
- `governance-check.options` 为空；`governance-result.options` 只有原 `request_id`，仅 Token URL，不依赖 Account。四个 targeted check/transition 操作的 options 为 `expected_state`、`expected_epoch`、`request_id`、`operator`、`reason`、排序 `runtime_roles`；check 永不加 `--apply`，两写操作才显式传它。SAFE/check-SAFE 不加载 Account URL；ACTIVE 与当前安装检查同时绑定 `TOKEN_MIGRATOR_DATABASE_URL` 和 `ACCOUNT_POLICY_DATABASE_URL`。

包装器对子进程采用绝对可执行文件、参数数组、无 shell、最小环境、受限输出与截止，不传递无关密钥、不输出原 stdout/stderr。成功证据只含操作/批准/请求与制品摘要、输出摘要和 OPERATION_COMPLETED，仍不是发布 GO。写入超时或失败统一标记结果未确认，保留原操作请求；先用既有只读 result/readiness/inventory 查证，不自动重试。没有安全复查入口的迁移失败交受信操作者核账本与备份，不改用 down 或创建新的业务幂等键。

第三批工具验收分开记证据：`TestTokenReleasePreparationPostgresIntegration` 验证专属 PostgreSQL 集群中的真实非超级用户角色/锁/迁移/CAS/enable；`TestTokenReleaseOperationsPostgresIntegration` 额外要求 `PINVOU_TEST_RELEASE_CLI_DIRECTORY` 指向由同一受检源码预先构建的 `migrate`、`token-policy-bootstrap`、`token-governance` 制品目录（Windows 使用 `.exe`），并使用 `PINVOU_TEST_RELEASE_DATABASE_URL` 和可用 Node。测试自行计算制品摘要、不在测试内构建，真实 Node 包装器启动原 CLI；覆盖显式 up、cap 空态拒绝/default plan 零写/非超级用户 operator bootstrap/同键重放及冲突零写、启用后候选身份 readiness、治理 check/原 request-id 查询与完整持久事实不变。隔离测试的迁移备份输入是明确标注的 fixture 记录，不是现场可恢复备份证明；loopback trust 也不证明现场 HBA/Secret 匹配。这些仍须后续环境证据，Node 注入 runner 单元测试不代替此真实子进程验收。

本工具并未替代现场 DBA/HBA/KMS/身份/网络授权，角色准备中的 server verifier布尔与隔离trust PG不是真实Secret匹配证明。跨库检查还包括全部受影响控制角色的owner/ACL/默认ACL等共享依赖及无法完整读取的业务库，不因没有DML或仅SELECT就放过；唯一例外是当前会话PID，不忽略同名DBA其他连接。包装器只允许闭合安全DSN查询参数，禁止dbname/database/host/user/options/search_path等覆盖预期目标。

本组授权登记新增token-release-prepare运维专用入口，339 HTTP/27 tasks/6 events不变、CLI由17增18；不改变G2挂起Job的SERVICE=token-governance。根对精确候选另跑单位与operations/授权Node，并仅在实际源码/依赖/原CLI工件一致时继承专属两PG与清理证据。G4入口/探针由第7节交付，G5原始工件签名与G6持续CI未交付，不把此组通过称为现场或总任务完成。

根在 G2 加本组十个源码/测试/授权文件的精确候选 `0b71b69e728c6f379f3a552de19500cdffb375be` 实际运行 operations六项与授权四项，共十 Node PASS/0 SKIP（10.828 秒）；两具名 Go 单元 PASS（1.548 秒），exit 0。根核对十文件与 `e371d69e58fe9e5831bf0e19a478f88bf80235e7` 实测源码无差异，依赖 G1 与三个原 CLI 同源，因此仅在该完整输入一致范围继承两专属 PG 16.86/64.31 秒、合计82.777秒/0 SKIP/exit0及固定角色/fixture库残留0/0的分项证据；非现场认证/部署证明。六份当前最小文档另交 B，后续 G4～G6 不提前关闭。

## 7. G4 新双域安全入口与显式只读探针

本组在G0～G3已提交依赖上运行，固定PRODUCTION Portal/credential为 account.pinvou.com / credentials.pinvou.com，CLOUD_UAT为 account-uat.pinvou.com / credentials-uat.pinvou.com。任一新域缺DNS、有效证书、可信入口或专用OIDC身份时保持ENV_PENDING，不能改任意域/HTTP或跳验证。DNS/CSP/代理源配置和工具可用不等于现场已部署。实际受控入口可采用TLS终止后的认证加密隧道，但须独立证明端点/监听/访问限制与失败关闭，不把公网明文上游称安全。

### 7.1 本组代理与只读探针保证

本批只交付安全代理配置和前置探针，不新增身份服务、OIDC 业务、launch/凭据协议或站点自动变更。分三个相邻模块：`scripts/account-platform-credential-edge.mjs` 校验闭合环境/固定 origin 对/受控 upstream/TLS 文件/OIDC 端点与专用 client/audience，并渲染 `deploy/nginx/` 下独立 credential server 和 Portal handoff location；`scripts/account-platform-credential-origin-probe.mjs` 显式只读外部 DNS/TLS/HTTP 预检；对应 Node 负例与独立 Chromium 只读 scope 检查。默认仅验证配置，不联网、不输出 Secret、不调用 nginx/reload/certbot；渲染与网络探针各需显式开关。

1. **代理边界**：保留既有开发模板和两个支付精确 callback，新的 credential HTTPS server 仅允许三个既有 assets、POST `/launch`、GET `/session`、GET `/oidc/callback`、POST `/credentials`；未知路径/错误方法失败关闭。HTTP 入口不转发携带 code 的回调到明文后端。handoff 以精确 location 加入当前 Portal HTTPS server，拒 query/路径变体，不经过 SPA 回退；不覆盖其应用生成的 CSP hash、`Referrer-Policy: origin`、COOP，也不将 credential 的 `no-referrer` 提前套在 handoff。credential 响应保留原应用 CSP/COOP/no-referrer/no-store，代理自产拒绝页另加安全头。不加入跨 origin CORS，不重写 redirect、Cookie domain/path 或回调参数，不缓存或重试写请求。
2. **代理链与日志**：可信 TLS 边缘清除外部可伪造的 forwarding/deadline/source 头后设固定 Host/HTTPS，应用侧只允许明确受信代理来源。upstream 只支持同机 loopback/固定受控加密隧道端口，或验证证书及固定 SNI 的 HTTPS；不允许任意公网明文 HTTP。隧道身份/监听限制/断链失败关闭和其他代理/WAF/APM 的配置属于现场证据，配置声明不是证明。credential 与 handoff 禁止 request/response body、完整 URI/query、Cookie/Authorization/Referer 日志；采用只含固定 route 类别、服务端 request_id、状态与时长的访问格式，敏感 location 的原始 error log 关闭；请求缓冲落盘及响应临时文件关闭。不得通过调试日志重新采集；无法审计某一层时保留缺项。保留无敏感值的可观测计数，不用全站禁日志影响支付排障。
3. **OIDC/workload 前置**：credential 专用 authorization-code + PKCE S256/client/audience、精确单一 `/oidc/callback` redirect URI、issuer 同源 HTTPS 三端点、无显式端口/userinfo/query/fragment按既有校验；不得拓宽普通 Portal client。workload 继续现有 issuer/JWKS/audience 与每服务精确 subject/permissions，短期 token-file/投射文件及轮换权限引用，不读取、复制或解码真实 token。清单校验拒缺项/通配权/跨用途 client，实际签发、15 分钟期限、轮换、过期拒绝、错误 audience/subject/权限拒绝由既有正式服务授权链实测并留报告，不能仅凭文件存在或 ready 布尔通过。KMS 旧密文兼容与 NATS 同集群 ACL 继续是环境前置，不能被本批 OIDC 结构校验替代。
4. **只读外部预检**：仅在显式选择固定 Production/CLOUD_UAT 后进行 DNS 和受校验 HTTPS 请求，禁止自定义 host resolver、忽略证书、HTTP 回退、自动跟随重定向和任意目标。检查两域证书主机名/有效期/链、HSTS、handoff 200 的构建期冻结脚本 hash/CSP/无 Cookie、credential 无 Cookie `/session` 的既有拒绝及安全头/本地 assets，并核对受控配置中的 OIDC discovery 与端点来源。所有请求无身份、无 Cookie、无 launch/result ID、无 POST；总量、body 大小和各步骤 deadline 有界，输出只含固定检查码、状态/摘要，不输出响应原文、原始网络错误、地址或证书主体。Chromium 使用 Puppeteer 专属全新 context 和唯一 page，goto 前禁网页 JavaScript 并安装 page request interception，仅接受固定无 query handoff GET/必要 HEAD 主文档，任何子资源、额外请求或 redirectChain 在发送前拒绝；不执行点击、表单或页面脚本。主文档暂停后由正式严格 HTTPS 客户端取得同一 URL，校验**全部冻结 HTML 字节**与精确 CSP/hash，拒绝 Link/Refresh/Report-To/NEL/Alt-Svc 等被动通道头，再只将已验证字节和安全头白名单 `request.respond` 给 JS-off 页面；未知 HTML 不进入解析，不能触发 preconnect/DNS-prefetch/iframe/表单/WS/popup。意外 target 关闭只是附加失败处理，不是前置保障。报告明确 `VALIDATED_HTTPS_REPLAY / FRESH_CONTEXT_ONLY`，不是原生浏览器 TLS 或真实握手；HTTP 层单独证明 TLS。Service Worker 不被禁用或清理，由可信 DevTools 只读检查 controller 和 registration scope，不能证明已有用户 context 或登录流程。代理清理不动真实 Origin、Referer 或 Fetch 元数据；`X-Pinvou-Tunnel-Proto` 在受控隧道模式由可信边缘固定 https，其余模式清空，不能透传外部伪造值。
5. **证据与测试**：离线严格输入/注入/跨环境/端口/日志/方法/预算/安全头回归，根用公开 fixture 在真实 nginx（兼容既有 1.18）做 `-t`，不 reload；另允许自有临时目录、自签 fixture 证书、独占 loopback 高端口与本地 mock upstream 的隔离 nginx 进程实际请求测试，验证 named error_page、方法/query/安全头与安全日志，结束只停止该精确临时实例。不使用现场私钥、现有监听或业务数据，不发外部流量；该证据仅证明代理实现，不证明公网 TLS 或真实域名链。受控 HTTP/TLS/浏览器 fixture 可用于确定性拒绝测试，但 probe 的现场入口不提供 resolver/TLS 绕过开关。只读预检通过最多 `PREFLIGHT_PASSED / ENV_PENDING / NOT_DEPLOYED`，不输出环境 VERIFIED/GO。专项设计 6.2 的完整十一点的真实双域 Chromium、专用真实 OIDC 登录、一次性隔离测试资源、权限/撤销/刷新/未知恢复以及各层 canary 日志扫描由后续环境验收单独执行；既有 `/start`/固定 launch IDs 的本地 smoke 不可直接当作现场 runner，用户未选环境或缺条件时明确列缺项，绝不发送试探性 launch/credentials 写请求。

### 7.2 实际调用与证据归属

所有可用CLI参数见 [nginx入口说明](../nginx/README.md)，其相对文件、三模板与公开edge config均在本组。配置校验默认零网络；显式render只输出片段，不运行nginx/certbot；HTTP与scope只有显式--probe才访问固定目标。现场CLI不提供fixture resolver/TLS绕过；真实scope测试要求显式CHROME与app锁定puppeteer-core。与scope分开，`credential-domain-rename.test.mjs`直接执行G0实际脚本完整READY/BIND/SUBMIT，四正例恰一次指定目标POST、三十负例零POST/DOM；该VM不是原生浏览器TLS/完整现场登录。

静态/Node/Chrome与隔离nginx实测按根精确候选分项记录；可以在G4模板/固定域/相关原服务字节一致时明确继承新域nginx解析/回环证据，不能继承旧域525候选作为新域已验收。G6的通用renderer、nginx测试镜像/helper和workflow未随本组提交，不声称这些持续门禁已交付或GitHub已绿。完整双origin/IdP握手、安全日志、权限撤销与回滚仍要独立现场证据，不由scope或无Cookie/session401探针替代。

根在G3加本组十一个代码/模板/测试文件的精确候选 `41b501279d6ce12f2df0021a25f97a467e3a3e81` 实际运行十一 Node PASS/0 SKIP（0.649 秒），真实 Chromium scope 2/2 PASS/0 SKIP（10.041 秒），exit 0。根核对十一输入与 `e371d69e58fe9e5831bf0e19a478f88bf80235e7` 无差异，并与 `4a0d59fe5f6488e08eb909ca5888e1e5a1188e47` 新域 host-nginx机制验证输入一致，故仅继承六 profile解析、双runtime安全矩阵与故障/实际SIGTERM精确清理的G4分项，不继承G6受限容器/持续CI完成结论。六份最小文档另交B；无实际DNS/证书/部署变化，不将VM或scope误称现场完整握手。
