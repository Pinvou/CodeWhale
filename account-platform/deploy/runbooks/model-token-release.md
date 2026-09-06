# 模型与 Token 发布计划和可选部署配置（APT-137 G0～G2）

状态：IN_PROGRESS / ENV_PENDING / NOT_DEPLOYED。当前包含已提交 G0 新域基础、G1 离线计划/envelope 与本组 G2 可选部署配置；后续 G3～G6 尚未在本组交付。DNS 未添加不阻塞本地代码验证/提交，但实际部署仍须完整环境证据；本工具不授权推送、部署或任何现场写入。

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

G1 已独立提交 `33209285`。本组使用现有 Go Dockerfile的 SERVICE 参数构建 credential portal、executor 与 token-governance，不增加业务服务职责：gateway 内仍唯一拥有两个 worker。Compose overlay/profile 需显式加载；Helm 三新组件默认 false，governance Job suspend=true、无 hook/自动 apply/重试。runtime 与运维 Secret 分离，executor 不获得数据库/KMS，Token Relay 与 gateway 同代 writer 共同停止/fence/恢复，不能沿用已退休登录。实际角色准备与受控启用程序在后续 G3，当前不得据本组模板直接切换。

公开配置仅用于解析。Compose 在 deploy/compose 目录运行 `docker compose --env-file .env.example --env-file model-token.env.example -f compose.yaml -f compose.model-token.yaml --profile model-token --profile governance-operations --profile real-newapi config --format json`，输出保存到本次受控临时文件，再从仓库根运行 `node scripts/account-platform-model-token-deployment-check.mjs --compose-render <文件>`；不能读现场 .env 或用这些不可用占位值 up/run。Helm 对已有默认、staging、production-new 与新增 `values-model-token-test.yaml` lint，渲染 fixture 后用同一 checker 的 `--helm-render <文件>`。真实 Docker/Helm 解析不是静态字符串替代，结构合法仍输出 DEPLOYMENT_CONFIG_VALID/ENV_PENDING/NOT_DEPLOYED。

十三 gate 环境值透传到既有所属服务，不改变领域逻辑。正式配对为 account.pinvou.com / credentials.pinvou.com，公开 UAT fixture 配对为 account-uat.pinvou.com / credentials-uat.pinvou.com。credential OIDC issuer 与 authorization/token/JWKS 必须精确同 origin HTTPS、无任何显式端口，不采用普通 Portal 内网 HTTP JWKS路径。外部可信短期 workload token-file/投射身份和各服务独立 NATS creds 必须就绪；不得继续共享开发静态 Token，也不在本任务临时造签发器。

七个 broker 客户端使用同一受控外部 TLS NATS；Helm 开新治理组件时拒绝内置匿名开发 NATS，URL/creds/Ping 不证明真实 ACL 或 JetStream 历史。受限 workload 排除旧宽策略选择器，checker 核全部命中允许规则；目标 New API 明确只允许 credential 与 gateway 所需入口。Compose 网络本身不充当防火墙，依赖 CIDR/Host/TLS 身份与可信出站边界仍需现场证明。credential 独立 loopback18108、端口/IP范围重叠必须拒绝；所有 runtime/附加容器禁止运维URL，控制Job/entrypoint必须固定只读默认，不可用wrapper或init容器自动执行。

配置从 local AES 切 remote Secret Provider 前，须由既有受控KMS流程证明全部持久密文（provider credential、事件流/结果回放和dispatch等）原 key_id、格式、AAD 可读及备份可恢复；没有本地解密 fallback，不能清旧KEK/删除历史后声称迁移成功。配置 Secret/issuer/ACL/DNS/有效证书/代理与HBA缺项均保持 ENV_PENDING，不绕过启动验证。本组只提供结构/制品，不包含后续 G3 操作包装器、G4 安全反代/HTTPS/scope工具或 G5 验签工具；既有 `token-governance` 默认 check 可独立构建，但任何 apply/激活须另有已批准现场步骤和授权。

根对本组精确候选验证 `node --test account-platform/test/contract/model-token-deployment.test.mjs`，并核对相同输入/blob下真实Helm/Compose解析、语义变异和新镜像构建证据；结果另记，不自动把原整树PASS转为本组证据。部署说明详见 [Compose](../compose/README.md) 与 [Helm](../helm/account-platform/README.md)。后续实际部署/灰度/SAFE流程仍待 G3～G6 与现场证据闭合，不 push/deploy，不标 UI-58 DONE。

根对 G1 加本组十八配置/源码文件的精确候选 `d356fee020a6af006058f3cf33b3f89b9a6b4cd7` 实际运行 G1/G2 合并十八 Node 全 PASS/0 SKIP（1.846 秒），exit 0。本组十八文件、G1 plan/schema 与依赖服务构建源码相对于根第六隔离实测候选 `e371` 均无差异，因此仅在此相同输入范围继承真实 Helm 4.2.3 四 profile lint/render checker、Compose config/checker及三个新服务镜像构建的分项证据；不是把整树或现场 PASS 继承到本组。七份最小文档另交 B，G2 仍不是实际部署或 G6 CI 全部通过。
