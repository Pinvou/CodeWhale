# 模型与 Token 离线发布计划（APT-137 G1）

状态：IN_PROGRESS / ENV_PENDING / NOT_DEPLOYED。本提交仅包含已提交 G0 的新域基础与 G1 离线计划/envelope 校验；后续 G2～G6 尚未在本组交付。DNS 未添加不阻塞本地代码验证/提交，但实际部署仍须完整环境证据；本工具不授权推送、部署或任何现场写入。

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
