# 账号平台历史实现审计、当前边界与后续差距

> 状态：`75a29f8` 历史目标审计矩阵；当前已审实现按下文 A06 边界校正，不能替代发布验收
>
> 历史校准日期：2026-08-23；集成本地边界校正：2026-10-06
>
> 状态定义：`已实现`、`部分实现`、`降级实现`、`未实现`、`移除`、`非当前范围`。下列 331 行保留原审计结论，不声称本轮重新实现或复审了全部目标。

> 对应目标需求：`requirements.md`

## 0. 状态证据规则

本文保留目标重构前的实现判断和差距，供后续规划及任务覆盖检查使用。原审计逐项记录 331 个目标编号；其源码基线、测试计数和部署判断仅描述 `75a29f822f339787c3a3457f521209368e2f6f34`，不能覆盖随后集成的修复或用作当前发布证明。

当前继承实现基线为 A06 `c5f0a2bfab58287bac880ea1bb198ef73b50e39d`。A07 的新客户端迁移须独立审查固定源码，不能由下列历史矩阵推断已完成。与历史文字不同、但当前必须保全的边界如下：

- 短信有通用 HTTP 与 Aliyun Adapter；结果未知保留冷却，只有完整、权威的明确未接受响应可解除冷却。没有企业商城也可注册、bootstrap 钱包和使用开发模拟器。
- 注册已持久化同一业务意图，丢失响应或本地完成失败可经新滑块/SMS 恢复唯一 Keycloak 标记主体；首次密码保留由 `password_preserved` 披露，不接管旧主体、不补偿删除已接受的身份。没有实现完整目标钱包异步 Saga 或账号注销 Saga。
- 新个人 PLAN 按付款快照从入账起计有效期且不可退款；旧 LEGACY/普通 Credit 不追加期限，原四档组织模拟充值继续保留。首次退款未入账的订单保持 `MANUAL_REVIEW`，迟到成功不自动入账。
- 模型请求必须携带每逻辑调用的键，不按用户/Session/body 自动生成；Gateway 不自动重发模型请求。未知消费继续保护预占，管理员明确最终免单须经持久意图和 Billing 唯一赢家裁决，保留真实 usage/原成本证据。
- 普通自管 Token/credentials/eligible/单笔 usage 和平台托管权限边界是既有能力。新内置模型迁移不能因旧 SCOPE-02 文字删去已审功能。
- 当前平台管理员仍由 Keycloak `platform-admin` Realm role 授权；业务停用/删除不自动撤角色。全局上下文、细粒度平台 RBAC、组成级 BASE/BONUS、发布/安装门禁等仍属后续目标。
- A06 已有真 PG/NATS/Redis、Keycloak 扩展、Compose devup/E2E/fault/auth 和真实 NewAPI HTTP 健康的隔离执行证据。同输入才可继承，变化输入须重验；这不表示真实银行、短信商、模型定价、生产 NewAPI TLS、Kubernetes 或发布验收通过。

下表“当前状态”是原审计在其基线上的状态字段，不表示当前波实现进度；任务检查只证明编号、字段、归属和依赖一致，不证明功能或部署完成。代码存在性、静态配置和自动化测试也不能替代生产验收：

| 字段 | 要求 |
|---|---|
| `source_commit` | 原审计所依据的提交 `75a29f822f339787c3a3457f521209368e2f6f34`。它证明历史结论对应的代码快照，不表示当前代码或所有实现均由该提交引入 |
| `tests` | 覆盖正常、权限拒绝、幂等、异常与恢复路径的测试名称或报告 |
| `deployment_version` | 已验证的部署版本、环境和验证时间；未部署写 `NOT_DEPLOYED` |
| `known_gaps` | 与目标需求的剩余差异、Provider 降级和未验证风险 |

各历史矩阵中的“当前实现/边界”是原 `known_gaps` 的展开说明。原审计使用下列共享证据编号，未取得生产部署、制品版本或验证时间证据，其 `deployment_version=NOT_DEPLOYED` 原样保留；上面的 A06 隔离测试不是生产部署记录。

原审计汇总：`已实现 54`、`部分实现 102`、`未实现 171`、`降级实现 2`、`移除 1`、`非当前范围 1`，合计 `331` 项；不是本波测试或实现计数。

| 证据编号 | 审计内容 | 结果 |
|---|---|---|
| `AUDIT_HEAD` | 原审计快照的代码、数据库迁移、OpenAPI、Compose/Helm 和客户端实现；基线 `75a29f822f339787c3a3457f521209368e2f6f34` | 原代码审阅记录；不覆盖 A01–A07 集成结果，不等同生产部署 |
| `CODE-AUDIT` | 逐项检查路由、领域实现、迁移、配置、客户端调用和测试覆盖；确认目标能力存在、部分存在或缺失 | 331/331 个唯一目标编号均有结论 |
| `DOC-COVERAGE` | 对比 `requirements.md` 全部稳定编号与本节规范证据行 | 331 个唯一编号一一覆盖，无缺失、无额外编号 |
| `GO-UNIT` | `node scripts/account-platform.mjs test unit` | 所有 Go 包测试通过（项目内置 Go，`nomsgpack`） |
| `CONTRACT-36` | 在 `account-platform/` 执行 `npm test` | OpenAPI lint 与 36 个合同测试通过 |
| `DESKTOP-39` | 在 `pinvou3-app/` 执行账号平台状态、三语覆盖、计划任务定向测试 | 39 个测试通过 |
| `VERIFY-ALL` | `node scripts/account-platform.mjs verify` | Go 单测与 Vet、OpenAPI/36 个合同测试、架构守卫和差异检查全部通过 |

### 0.1 target_requirement_id 规则

- 功能矩阵第一列是规范的 `target_requirement_id`，必须与 `requirements.md` 同义；不得使用行号、标题或自由文本代替。
- 历史 `CR-*` 是变更记录编号，不是目标需求编号，必须通过 1.1 节的 `target_requirement_id` 列映射到一个或多个目标能力。
- `ID/ACC/TOK/BILL/PAY/UI/ADM/NFR` 状态项与同名目标编号一一对应；目标文档新增或修改编号时，必须在同一变更中完成代码核验并更新证据行，不再写入无结论占位。
- `AUDIT_HEAD` 表示上述代码审计基线；`DOC_CURRENT` 表示本文件所在 Git 提交。两者都不能单独作为生产完成或部署证据。

| target_requirement_id | 当前状态 | source_commit | tests | deployment_version | known_gaps |
|---|---|---|---|---|---|
| REQ-ID-01 | 已实现 | DOC_CURRENT | DOC-COVERAGE | NOT_APPLICABLE | 需求文档共 331 个唯一目标编号，规范证据矩阵逐项一一覆盖且无缺失或额外编号 |
| REQ-ID-02 | 已实现 | DOC_CURRENT | DOC-COVERAGE | NOT_APPLICABLE | 331 个规范状态行均包含状态、源码基线、验证证据、部署版本和已知差距字段 |
| REQ-ID-03 | 已实现 | DOC_CURRENT | DOC-COVERAGE | NOT_APPLICABLE | 规范性表格和补充规则均复用稳定 target_requirement_id；重复出现作为同一能力的补充验收条件 |
| CORE-01 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Keycloak/Identity 与 Account 映射边界已存在；生产部署与 Identity 边界密码策略仍待补 |
| CORE-02 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Account 自有用户、组织和 Session 上下文；平台权限仍依赖 Keycloak Realm Role，全局上下文未实现 |
| CORE-03 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 只有新华三专用 `mall_user_sync_jobs`，尚无供应商中立 Commerce Mapping 模型 |
| CORE-04 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 平衡不可变账本已实现；组成级权益、应收和完整支付事实模型未实现 |
| CORE-05 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Provider 凭据加密持久化且只在 Gateway 使用时解密；生产 KMS/Vault 待部署证明 |
| CORE-06 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 模型不确定结果可进入 REVIEW 并核对 New API；支付和流式派发证据状态机不完整 |
| CORE-07 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 三服务具备 Outbox/Inbox/Relay；注册、注销、退款和 Provisioning Saga 缺失 |
| SCOPE-01 | 已实现 | AUDIT_HEAD | DESKTOP-39 | NOT_DEPLOYED | 本地独立模型仍受保护，账号平台迁移不删除用户自建模型 |
| SCOPE-02 | 已实现 | AUDIT_HEAD | DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 个人中心已移除自管 Token Account 页面；Gateway 仍保留受限普通用户 API，不影响当前 UI 范围结论 |
| SCOPE-03 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 普通用户接口过滤 PLATFORM 账号并拒绝修改；组织业务策略与平台路由的完整隔离矩阵未建立 |
| SCOPE-04 | 已实现 | AUDIT_HEAD | DESKTOP-39 | NOT_DEPLOYED | 桌面运行时不再引用旧 `llmapi_hub`/旧功能开关；仍缺安装与发布制品证明 |
| SCOPE-05 | 部分实现 | AUDIT_HEAD | DESKTOP-39 | NOT_DEPLOYED | 当前改动范围可审计，但尚无自动化发布范围门禁 |
| ID-15 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 注册在 Identity 与 Account 提交后返回，Billing 仅在后续 bootstrap 调用；钱包失败不会删除已提交的已验证身份或 Account |
| ID-17 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少密码预检、最终策略和注册幂等指纹只位于 Identity 边界的强制隔离 |
| ID-18 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少首尾空格明确拒绝及禁止 trim、规范化、转换和截断的端到端契约 |
| ACC-02 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Identity 与 Account 创建成功后，注册接口返回稳定 `user_id` 和 `PROVISIONING`，不调用 Billing；部署证据待补 |
| ACC-03 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | `PROVISIONING` Identity 可登录，Account 资料可读取，模型授权上下文要求 `ACTIVE`；充值门禁、稳定错误语义及 `REPAIR_REQUIRED` 闭环尚不完整 |
| ACC-04 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无 Provisioning SLA、自动重试和 `REPAIR_REQUIRED` 状态 |
| ACC-15 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有平台管理员创建接口/UI、字段校验、串行事务和审计/Outbox；缺目标 Account 平台权限与稳定幂等键 |
| ACC-16 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有按名称或 ID 搜索及稳定更新时间排序；缺状态筛选、游标分页和 Account 平台权限模型 |
| ACC-17 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有 DISABLED→ACTIVE 幂等转换、成员/授权版本更新和审计；缺跨 Billing 恢复条件检查 |
| ACC-18 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有 ACTIVE→DISABLED、成员暂停、Session 回退和审计；未阻止 Billing 购买、分配和既有 Hold |
| ACC-20 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有 DELETED 状态、成员撤销、Session 回退和资料脱敏；未执行 ACC-19 全量预检，重复删除返回未找到而非幂等原结果 |
| ACC-21 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有显式目标组织、父节点状态校验、同组织闭包写入和审计；缺稳定幂等键 |
| ACC-23 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有独立重命名接口、显式编辑 UI、名称校验、节点 ID/层级保持和审计；服务端权限仍是迁移期角色模型 |
| ACC-24 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有同组织移动、防自身/后代成环、串行事务和版本更新；缺稳定幂等键及独立结构版本契约 |
| ACC-25 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有叶节点检查和审计，但当前物理删除节点并删除成员/角色关联，未按目标对授予、路由和任务执行阻断后逻辑删除 |
| ACC-27 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有用户/组织状态、重复成员、角色边界、授权版本和 Outbox；重复有效成员返回冲突而非幂等原结果 |
| ACC-28 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有成员撤销、角色清理、授权版本和 Outbox；管理端路径未自动回退活动上下文，也未处理 BILL-13 授予暂停 |
| ACC-31 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 普通角色接口拒绝 OWNER/平台角色并递增授权版本；缺起止时间、细粒度权限码和稳定幂等键 |
| ACC-32 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 角色替换会递增用户/Session 授权版本且 OWNER 走专用路径；缺单绑定幂等撤销、有效期和缓存失效契约 |
| ACC-35 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 组织管理路由显式携带 organization_id 且不会切换活动上下文；授权仍依赖迁移期固定角色而非目标权限码 |
| ACC-36 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 平台管理路由仅检查平台管理员身份，不读取或切换聊天计费活动上下文 |
| ACC-37 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | usage_request 与 Hold 保存上下文、钱包、价格和路由等请求开始事实；缺完整授权、目录、协议和公式版本快照 |
| ACC-38 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 组织停用和普通成员移除可回退对应 Session；平台管理端移除成员未回退，且当前上下文不是用户级全局值 |
| ACC-39 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 部分 Account/Gateway 响应返回 membership_version；尚未由全部认证业务 API 返回 context_version 和 authorization_version |
| ACC-40 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少持久化、幂等、可恢复且禁止跳阶段的注销 Saga |
| ACC-41 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少注销静默期新业务冻结和修复动作白名单 |
| ACC-42 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少待核对模型用量的注销阻断及平台承担安全终态 |
| ACC-43 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少拒付权益冲销、应收认定及清偿完成门禁 |
| ACC-44 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少资金性与纯信息对账差异分类及注销门禁 |
| ACC-45 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少外部待确认关单、晚到审核和 Provider 事实盲区注销门禁 |
| ACC-46 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 Commerce Mapping 任务分类、取消和主体生命周期版本防迟到回写 |
| ACC-47 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少注销时资料修改 Saga 自动取消或向前收敛规则 |
| ACC-48 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少最后一个有效超级管理员注销保护，以及其他平台/组织角色与临时授权的自动撤销和缓存失效 |
| ACC-49 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少未接受邀请自动失效及标识复用隔离 |
| ACC-50 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 Identity 不可认证、自动清理完成和生命周期隔离组成的标识释放原子门禁 |
| ACC-51 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少正式映射与活动预约的数据库最终唯一约束及提交时重检 |
| ACC-52 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 SMS_PENDING、REGISTRATION_IN_PROGRESS、IDENTITY_CREATED 分阶段预约及无 TTL 恢复锁 |
| ACC-53 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少贯穿短信、Identity、Account 和钱包的稳定注册操作 ID、指纹与幂等键 |
| ACC-54 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 Identity 已创建但 Account 未提交的30分钟恢复、24小时补偿门禁和安全释放顺序 |
| ACC-55 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少不可枚举注册操作查询、签名令牌和脱敏恢复状态契约 |
| ACC-56 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT | NOT_DEPLOYED | `users.id` 是 UUID 主键并由 UUIDv7 创建，现有领域写路径不修改且逻辑删除保留原值；缺数据库级不可变守卫、跨迁移验证和显式永久不复用契约 |
| ACC-57 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | `users` 表和领域模型没有独立 `user_code`，也没有安全随机生成、格式/校验码、唯一约束、冲突重试及永久登记 |
| ACC-58 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 本人资料、桌面/Web 和运营后台均未展示 `user_code`，也没有按完整编码精确查询及注销主体受控识别能力 |
| TOK-04 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前只有 `/v1/chat/completions`，没有版本化协议能力目录 |
| TOK-07 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 没有独立模型目录和 `CATALOG_VISIBLE` 状态 |
| TOK-09 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 托管路由 PENDING/READY/FAILED/REPAIR_REQUIRED 状态机及自动/人工修复待实现 |
| TOK-10 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 目录、托管路由 Provisioning、上下文资格、请求预占、Hold 与上游调用的强制顺序待实现 |
| TOK-12 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Gateway 每请求检查 Account 上下文和路由共享策略；缺目录/Provisioning 联合资格状态 |
| TOK-16 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Gateway 会在请求时重选路由并预占；客户端提示与目标资格版本未完整实现 |
| TOK-17 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 只有固定输入/输出/缓存输入定价，无协议版本化预占公式 |
| TOK-18 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无公式参数默认值及公式配置缺失运行时契约 |
| TOK-20 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 请求级最大费用在上游前计算并创建 Hold；公式、组成和跨服务原子边界未实现 |
| TOK-21 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 请求保存价格与路由引用；没有公式/协议/逐单位价格完整快照 |
| TOK-23 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 没有长调用 Hold 续租和追加预占 |
| TOK-25 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 幂等键仍需统一为稳定用户级模型推理命名空间并移除 Session/上下文/协议作用域差异 |
| TOK-26 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少版本化规范化请求指纹及同键异请求 409 防护 |
| TOK-27 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少非终态、结果载荷、流事件、防重墓碑的分层保留与 410 过期契约 |
| TOK-28 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少加密流事件日志、单调序号、完整性校验及按确认序号重放 |
| TOK-29 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少断线不取消及按执行状态恢复流、结果或核对状态的统一契约 |
| TOK-30 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 Hold 后派发状态机、EnsureHold 幂等、派发不确定时禁止盲重试及过期恢复规则 |
| TOK-35 | 部分实现 | AUDIT_HEAD | DESKTOP-39 | NOT_DEPLOYED | 已有盘点报告、配置备份、幂等迁移、会话/任务模型迁移和凭据清理重试；缺安装崩溃注入与制品级验收 |
| TOK-36 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少发布、安装、运行三层门禁，更新器尚未证明迁移预检失败不替换旧版本及二进制/配置原子提交回滚 |
| TOK-37 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少不创建用户 Hold、不产生用户费用的客户端切换 readiness、隔离合成验收身份及绑定制品/环境/服务版本的短时签名准入证明 |
| TOK-38 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少按原请求所有者授权、持久化取消意图、派发前安全终止及竞态处理的幂等取消命令 |
| TOK-39 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | Model Gateway Adapter 尚未声明显式取消、断流可确认取消或不支持取消的统一能力 |
| TOK-40 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 客户端连接、交付状态与服务端模型执行仍缺少可验证的独立状态契约 |
| TOK-41 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少取消结果、执行终态和最终实际用量的统一证据核对闭环 |
| TOK-42 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少独立于调用资格的 DRAFT/PUBLISHED/DEPRECATED/REMOVED 目录管理状态机、并发控制和审计 |
| TOK-43 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少上游模型消失后的连续核验、历史显示快照、目录墓碑及不依赖实时 Model Gateway 的历史会话展示 |
| TOK-44 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少稳定模型 ID、版本化显示名称和无歧义别名映射及冲突校验 |
| TOK-45 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少以历史版本为来源向前创建新版本、按当前依赖重新校验并原子发布的目录回滚 |
| TOK-46 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少逻辑模型、目录、协议、能力、价格和路由代次的独立版本生成规则及请求全版本快照 |
| TOK-47 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少弃用宽限期、新会话/历史继续分离门禁、显式替代分支及下架在途请求处理 |
| BILL-04 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 注册接口不调用 Billing，但后续 `/v1/account/bootstrap` 同步调用 `EnsurePersonalWallet`；没有 Outbox 驱动的异步初始化和后台重试 |
| BILL-05 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 商品有 BASE/BONUS 字段，但入账和权益包只保存合计，未形成组成类型约束 |
| BILL-09 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Hold 只按聚合权益包分配，没有按 BASE/BONUS 组成分配 |
| BILL-10 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无组成级批量授权及逐项部分成功契约 |
| BILL-11 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无组成级授予撤回与重建 |
| BILL-12 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无 `ACCOUNT_CLOSED` 专用回收和原组成恢复 |
| BILL-13 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无成员移除后的授予暂停/恢复状态 |
| BILL-16 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 仅按权益包到期时间消费，无法执行同日 BONUS 优先于 BASE |
| BILL-20 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 只有固定三元组价格，没有版本化计量单位 |
| BILL-23 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 公式引用单位与固定价格/模型级显式继承配置项的严格绑定待实现 |
| BILL-24 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | INPUT_TOKEN 与 CACHED_INPUT_TOKEN 互斥归一化和防重复计费待实现 |
| BILL-25 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 固定零价格、模型级显式继承与缺失配置的数据、发布和运行时语义区分待实现 |
| BILL-29 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无独立应收表和钱包负余额同事务双记模型 |
| BILL-30 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无应收驱动的上下文级冻结 |
| BILL-31 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无应收清偿分配和来源限制 |
| BILL-32 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无 BASE/BONUS 债务隔离和延迟激活 |
| BILL-33 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 订单级拒付冲销隔离、关联权益撤销和对应钱包上下文冻结待实现 |
| BILL-34 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少可信实际生成量、客户端交付量和平台不可恢复交付故障之间的计费证据边界 |
| BILL-35 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少取消期间 Hold 的派发前释放、派发后续租、期限收敛和晚到证据处理规则 |
| BILL-36 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少权益组成可用、有效授予、暂停授予和关联 Hold 四个互斥桶的资金守恒校验 |
| BILL-37 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少同权益包同组成可用余额上限及禁止跨包、跨组成、跨钱包超额授权的强约束 |
| BILL-38 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少组成级锁或等价 CAS、请求指纹、并发分配和批量子幂等事务边界 |
| BILL-39 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少账号注销后组织授予隔离、不自动回池、禁止恢复/普通撤回及未回收剩余量到期失效规则 |
| BILL-40 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少账号去标识化后的授权管理员专用全额回收、资格校验、守恒事务、幂等边界和完整审计 |
| BILL-41 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少模型覆盖优先、无覆盖才使用全局及同作用域缺失/歧义稳定失败的确定匹配器 |
| BILL-42 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少逐单位 FIXED_PRICE/INHERIT_GLOBAL 解析、零价阻止回退及最终来源快照 |
| BILL-43 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 PUBLISHED/ACTIVE 时间区间排斥约束、并发发布锁/CAS、ETag 和幂等冲突处理 |
| BILL-44 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 DRAFT/PUBLISHED/ACTIVE/ENDED/CANCELLED 生命周期、生效前撤销和 ACTIVE 显式提前结束模式 |
| BILL-45 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少逐单位最终价格与来源的 Hold 不可变快照，以及撤销、结束、替代和回滚后的续租/结算约束 |
| PAY-11 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 订单一对多支付流水、权益包单一成功流水来源及多成功流水独立履约/复核待实现 |
| PAY-12 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | `payment_notifications` 保留回调证据；查询/账单统一不可变证据模型未实现 |
| PAY-13 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 订单关闭、支付流水、退款和拒付独立状态机及订单派生摘要待实现 |
| PAY-14 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前订单/流水更新有状态校验，但独立支付、退款、拒付状态机未建立 |
| PAY-19 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少72小时边界最后查询、停止高频查询及转人工审核的确定性任务 |
| PAY-20 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 一单一流水唯一约束无法表达晚到多笔实收及逐笔履约/复核 |
| PAY-23 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无组织购买和组织付款人 Commerce Mapping |
| PAY-24 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 退款目标同时绑定支付流水和权益包并校验来源关系待实现 |
| PAY-25 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 现有退款按订单聚合剩余量与比例计算，无法检查 BASE/关联 BONUS 累计消费和资金异常 |
| PAY-26 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 只按目标全部未消费 BASE 计算现金退款、关联 BONUS 零折现及无普通部分退款状态待实现 |
| PAY-27 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 退款意图锁定、BASE/关联 BONUS 消费复查与冻结、仅按 BASE 预约现金金额、冲销快照、请求与 Outbox 原子提交待实现 |
| PAY-28 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 现有退款没有目标权益包组成冻结和 Outbox 恢复 |
| PAY-29 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 通用接口调用原 Provider 退款；新华三明确不支持，且目标绑定/幂等恢复未实现 |
| PAY-31 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Provider capabilities 可拒绝不支持动作；UI 仍显示禁用按钮，未按需求完全隐藏入口 |
| PAY-32 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 渠道部分强制退款作为既成外部事实、不受普通退款资格预检拒绝的独立异常、权益冻结、复核和订单应收待实现 |
| PAY-35 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 无账单降级尚未与无回调 Provider 的正式收款准入和72小时人工审核闭合 |
| PAY-37 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少72小时未收敛订单的唯一人工审核案件、责任人、复核 SLA、next_review_at 和逾期升级 |
| PAY-38 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少对无回调无账单 Provider 的历史查询、有限晚到窗口、权威终态和正式收款准入校验 |
| PAY-39 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少最晚确认期限、最终成功查询、权威未支付终态及冲突/申诉检查组成的归档门禁 |
| PAY-40 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少 PROVIDER_BLIND 状态、责任升级、禁止静默归档和必要时停用新订单的闭环 |
| UI-02 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 桌面端将注册与后续自动登录分开并可提示“注册成功但自动登录失败”，但没有展示钱包初始化、`PROVISIONING` 重试或 `REPAIR_REQUIRED` 修复状态 |
| UI-13 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少停止接收、取消处理中、Provider 不支持取消、待核对和已结算的用户状态反馈 |
| UI-14 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少注销阶段、脱敏阻断项和用户恢复动作展示 |
| UI-15 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少未完成注册查询、处理中/修复中展示及防重复注册恢复流程 |
| ADM-11 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 组织模型业务策略与平台 Token Account、真实路由及凭据运营权限隔离待实现 |
| ADM-12 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无计量单位定义表、生命周期或后台入口 |
| ADM-13 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有固定三元组价格版本管理；缺单位/公式/完整生命周期和并发发布约束 |
| ADM-14 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 管理后台可查询钱包状态；生产授权隔离仍需部署验证 |
| ADM-15 | 已实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 管理后台可查询 Journal/Posting，数据库保护已入账分录不可变 |
| ADM-16 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无独立应收模型和管理页面 |
| ADM-17 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有人工调整申请/审批和平衡入账；尚无统一审批命令框架 |
| ADM-18 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有 Hold 查询/释放；缺目标异常状态与组成级证据 |
| ADM-19 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有订单查询、同步和修复；目标多流水与完整支付状态机缺失 |
| ADM-20 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 只有旧订单级部分退款，不满足目标普通退款运营模型 |
| ADM-21 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有模拟器账单对账基础；新华三无账单且四方证据模型未完成 |
| ADM-22 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有支付修复动作；缺统一幂等命令、审批和完整状态快照 |
| ADM-23 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无模型协议定义管理 |
| ADM-24 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无协议预占公式版本管理 |
| ADM-25 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 可管理平台 Token Account 生命周期；目标平台业务 RBAC 未实现 |
| ADM-26 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 可查询脱敏凭据元数据；目标细分角色隔离未实现 |
| ADM-27 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有凭据检查动作；目标细分角色隔离和统一错误契约待补 |
| ADM-28 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有路由 CRUD 和确定性选择；模型目录/Provisioning 代次缺失 |
| ADM-29 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无托管 Provisioning 状态和代次视图 |
| ADM-30 | 未实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无托管 Provisioning 修复命令 |
| ADM-31 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 可查询用量请求和上游引用；缺流事件/请求指纹/完整证据 |
| ADM-32 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有用量核对人工重试且不重发模型请求；完整幂等收敛仍待目标实现 |
| ADM-33 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少晚到支付案件筛选、认领、受审计查询、复核安排、归档和事实盲区升级页面 |
| ADM-34 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少注销 REPAIR_REQUIRED 队列、阶段重试及禁止绕过门禁的运营能力 |
| ADM-35 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少注册 REPAIR_REQUIRED 队列、Identity/Account 阶段重试及受证明门禁约束的补偿能力 |
| ADM-36 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少覆盖全部后台写命令的版本化审批分类注册表和未注册命令失败关闭门禁 |
| ADM-37 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少人工财务变更强制审批命令清单及统一服务端门禁 |
| ADM-38 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少正常状态机、治理命令和自动任务的仅审计清单及异常路径隔离 |
| ADM-39 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少审批过期、撤回、不可重开、重新提交关联和并发状态转换规则 |
| ADM-40 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少不可变命令/资源/策略快照、关键字段声明及资源变化自动失效 |
| ADM-41 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少执行前对申请人/审批人实时授权复核及授权不确定时失败关闭 |
| ADM-42 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少审批 Outbox、最小权限执行身份、短执行窗口和端到端幂等执行 |
| NFR-04 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 错误响应和多数日志避免直接输出秘密，合同测试扫描配置；缺统一字段白名单、日志/Trace 正文脱敏和自动秘密扫描门禁 |
| NFR-21 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Event Relay 使用 JSON 结构化日志，部分后台任务使用 slog；尚无所有服务统一 schema、必需字段和发布门禁 |
| NFR-22 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | HTTP 中间件生成/返回 Request ID，审计和部分下游调用可关联；尚未证明所有入口、事件、错误和后台任务端到端传播 |
| NFR-23 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | HTTP 客户端传播 W3C traceparent，Outbox 记录 trace_id，并有 OTLP 配置测试；NATS 消费和全部后台任务的跨信号传播不完整 |
| NFR-24 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 共享 HTTP Server 提供 live/ready 并覆盖依赖失败测试，Relay 提供 live；尚未逐工作负载统一 readiness 语义 |
| NFR-25 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 业务 HTTP Server 和 Event Relay 提供独立 metrics 监听及低基数测试；目标核心指标覆盖和所有工作负载隔离仍不完整 |
| NFR-26 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少固定生产等价环境、数据集、样本量和平台/Provider 分段计时的统一性能基线 |
| NFR-27 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少关键内部读写接口 P95/P99 和错误率 SLO 证据 |
| NFR-28 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少登录、短信、支付下单端到端与平台附加延迟的分离 SLO |
| NFR-29 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少首个非空内容事件定义、平台派发耗时和模型路由 TTFT SLO |
| NFR-30 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少200并发流式调用容量、硬上限准入和公平反压验收 |
| NFR-31 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少十万有效 Hold 及持续/突发事务容量与锁等待验收 |
| NFR-32 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少百万订单/三百万流水数据规模及下单/证据归并吞吐验收 |
| NFR-33 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少百万事件积压容量、排空速度及高水位停止高风险写入门禁 |
| NFR-34 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少资金事实1分钟、账号配置5分钟的分级 RPO 验证 |
| NFR-35 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少核心60分钟、非关键4小时的分级 RTO 验证 |
| NFR-36 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少持续日志归档、日增量/周全量、加密、密钥分离及异地不可变备份闭环 |
| NFR-37 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少月度抽样、季度端到端和年度灾难恢复演练及整改复测 |
| NFR-38 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少支付/账本/审计/用量三年、普通日志90天及到期清除证明的分类策略 |
| NFR-39 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少登录按IP/设备/标识/用户/会话的统一限流和反枚举验证 |
| NFR-40 | 部分实现 | AUDIT_HEAD | GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有手机号60秒与小时限制基础，缺少IP/设备/用途/日上限及统一Provider配额门禁 |
| NFR-41 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少支付按付款人/受益组织/IP/订单/动作/Provider的多维限流 |
| NFR-42 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少模型按用户/上下文/组织/模型/路由的请求速率和并发准入 |
| NFR-43 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少跨个人、组织、节点、角色、缓存、批量、导出和事件路径的自动隔离矩阵 |
| NFR-44 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少Provider能力/路由级失败窗口、熔断、半开和不确定高风险请求保护 |
| NFR-45 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少人工队列P0/P1/P2量化认领、响应、止损、解决和超时升级 SLA |
| NFR-46 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少三授时源、100ms目标、250ms告警、1s退出就绪及时间敏感操作门禁 |
| NFR-47 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少当前及前两桌面版本合同测试、90天兼容、60天通知和稳定淘汰响应 |
| NFR-48 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少审计导出脱敏、独立PII权限、水印、加密、短时下载和配额控制 |
| UI-16 | 部分实现 | AUDIT_HEAD | DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 当前桌面和 Web 路由三语覆盖测试通过；缺目标页面、错误码和发布制品级门禁 |
| NFR-49 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少模型流事件、完整/聚合结果、防重墓碑的最大保留期、注销清除、案件保全和备份密码学销毁闭环 |
| NFR-50 | 未实现 | AUDIT_HEAD | CODE-AUDIT | NOT_DEPLOYED | 缺少用户编码及受鉴权精确查询接口，尚无编码查询的无泄露响应、多维限流、审计和反枚举门禁 |

### 0.2 既有目标编号证据索引

以下编号保留原审计对功能矩阵、路由、领域实现、迁移和测试的映射。状态描述原快照；A06 修复及本波迁移必须另按固定输入审查，不能继承此表中的测试计数或旧缺口判断。

| target_requirement_id | 当前状态 | source_commit | tests | deployment_version | known_gaps |
|---|---|---|---|---|---|
| ID-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 8–128 位可打印 ASCII，至少一个字母和一个数字。 |
| ID-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 前端核对两次密码，后端仍独立校验用户名、手机号和密码。 |
| ID-03 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已在短信前完成用户名、手机号和密码预检，但密码仍由 Account 接收和校验，未收敛到 Identity Adapter |
| ID-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 服务端生成一次性挑战，客户端用覆盖登录框的弹层完成；不是第三方 CAPTCHA。 |
| ID-05 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 同手机号至少间隔 60 秒，最多 5 次/小时；前端显示倒计时。供应商明确发送失败不占冷却。 |
| ID-06 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 5 分钟有效，最多 5 次核验，成功后不可复用。 |
| ID-07 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Account 只保存 Provider 挑战标识和状态元数据。 |
| ID-08 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 业务层只管理挑战、用途、风控和结果；Provider 适配层负责厂商协议。既支持厂商托管验证码，也支持平台安全生成、仅通过短信厂商发送的模式。 |
| ID-09 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 桌面端通过 Keycloak Direct Access Grant；不持久化密码。 |
| ID-10 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Keycloak 自定义 Direct Grant 调用 Account 内部核验接口；仅中国大陆手机号。 |
| ID-11 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 未知手机号返回相同外部响应且不发送短信，但尚无统一耗时和完整多维限流证据 |
| ID-12 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Refresh Token 仅存系统凭据库，启动时刷新。 |
| ID-13 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 失败路径删除持久化 Refresh Token，不回退历史缓存。 |
| ID-14 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Identity 与 Account 均成功后返回可登录的 `PROVISIONING` 且不依赖钱包；Identity 已创建而 Account 提交失败仍使用即时删除补偿，缺少持久恢复 Saga |
| ID-16 | 非当前范围 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Keycloak 具备扩展基础，但当前产品闭环未实现；短信找回密码已纳入 ACC-09。 |
| ACC-01 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有 Keycloak 身份管理 Adapter 接口；Account 仍接收注册密码并直接编排厂商管理语义，边界未完全收敛 |
| ACC-05 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有新华三专用 mall_user_sync_jobs 异步任务；缺供应商中立 Commerce Mapping、Adapter 版本和通用状态模型 |
| ACC-06 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 用户名和手机号变更使用唯一变更请求、字段预约、版本检查、Identity Provider 更新、Account 提交、会话撤销及 Commerce 异步同步；失败可重试或进入人工修复。 |
| ACC-07 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 个人中心发起，重新认证后检查规范化用户名唯一，并通过身份 Adapter 与 Account 保持一致。 |
| ACC-08 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 必须验证新手机号并重新认证；同步身份 Provider、Account 和当前 Commerce Customer 映射，失败进入可恢复状态。 |
| ACC-09 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 未登录入口使用手机号短信验证，通过身份 Adapter 重置密码并撤销既有 Refresh Token/会话。 |
| ACC-10 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 重新认证后可以持久化注销请求；完成前强一致检查个人余额、未清应收、本人订单/退款、Hold、待核对用量、拒付、资金性差异、外部关单和 Commerce 任务，并要求转让组织所有权。组织余额和未消费组织授予不阻断；授予按 BILL-39 进入账号注销隔离暂停。 |
| ACC-11 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 当前管理员删除保留 UUID 并清除部分资料，但完整注销 Saga、用户名/手机号安全释放、三年分类保留和永久 `user_code` 墓碑均未实现。 |
| ACC-12 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 成员关系独立存储；个人中心只显示当前用户关联组织。 |
| ACC-13 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 组织角色与作用域已有基础，但权限判断仍硬编码，平台权限继续依赖 Keycloak Realm Role |
| ACC-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 顶层组织创建位于平台管理 API/UI。 |
| ACC-19 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有逻辑删除与审计，但删除前未跨 Billing/Token 核验余额、权益、Hold、支付、差异和待核对用量 |
| ACC-22 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 管理 UI 提供启动编辑动作，避免误改。 |
| ACC-26 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 当前仅支持电子邮箱 + 一次性 Token 邀请；手机号/用户名邀请未实现 |
| ACC-29 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有专用串行事务维持唯一 OWNER 并更新版本/审计；缺稳定幂等键和目标 Account 权限码 |
| ACC-30 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 可查看组织角色定义、成员和本人角色；缺 Account 平台角色、授权有效期及完整平台角色关联 |
| ACC-33 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 个人中心按组织列表动态隐藏。 |
| ACC-34 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有个人/组织活动上下文和版本字段，但按 OIDC Session 保存，不是用户级全局上下文，也无跨端同步 |
| TOK-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 用户在桌面直接配置的本地模型或第三方云模型由本地模型系统管理，不携带账号平台身份、不使用平台钱包和 Token Gateway。 |
| TOK-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 启用账号平台后，Rust 运行时以当前 Access Token 调用 Gateway。 |
| TOK-03 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | `POST /v1/chat/completions` 支持 JSON 和 SSE；桌面原生流式路径使用该入口。 |
| TOK-05 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有模型网关调用与 UsageEvidenceProvider 抽象；通用字段、数据库和核对流程仍含 New API 专属语义 |
| TOK-06 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Gateway 同步模型目录并提供 `CATALOG_VISIBLE` 用户模型列表；上下文资格和请求级预占不作为目录同步字段，桌面不再读取旧模型服务或固定环境变量列表。 |
| TOK-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 平台托管、幂等创建，普通用户不可见不可改。 |
| TOK-11 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Gateway 从 Account 获取服务端可信上下文，不接受客户端伪造 payer。 |
| TOK-13 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 按作用域、默认级别和优先级选择，歧义拒绝。 |
| TOK-14 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | API 只返回类型、状态、指纹、更新时间等元数据。 |
| TOK-15 | 移除 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 不提供个人供应商 Key、Token Account、共享和路由管理；现有相关个人入口及公开能力从目标范围移除。平台/组织运营管理与本地独立模型配置不受此条影响。 |
| TOK-19 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有固定三元组估算和 Hold 预占；缺协议绑定公式、版本化计量单位、Tokenizer/估算器、续租和追加预占 |
| TOK-22 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 每请求独立 Hold；可用余额能够覆盖即可并发。 |
| TOK-24 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 幂等键能阻止重复上游调用，但重复请求返回 `USAGE_REPLAY_BLOCKED`，不会返回原结果/状态 |
| TOK-31 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 成功响应有 usage 时按实际成本结算并释放差额；取消终态、累计可信用量和取消核对路径不完整 |
| TOK-32 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Gateway 对上游结果不确定或网络失败不自动重发模型请求，避免重复消费 |
| TOK-33 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | REVIEW/SETTLEMENT_PENDING 可按上游证据退避核对并人工重试，且不重发模型请求；协议仍绑定 New API 且完整收敛语义不足 |
| TOK-34 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 提供个人用量请求列表和详情，不暴露内部凭据。 |
| BILL-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 钱包按 Owner 类型和 ID 唯一；组织请求不能隐式扣个人钱包。 |
| BILL-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Journal Entry 与 Posting 平衡，不能直接覆盖余额。 |
| BILL-03 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | `10,000 microcredits = 1 展示积分`；金额使用分。 |
| BILL-06 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有支付来源聚合权益包、有效期和余额；未持久化 BASE/BONUS 组成，且来源绑定订单而非支付流水 |
| BILL-07 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 企业管理员购买时明确受益组织；支付成功幂等创建组织权益包和组织钱包分录，付款人个人钱包不参与。 |
| BILL-08 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 企业管理员从组织有效权益中给成员分配，员工无领取接口；分配、撤回、占用和消费均审计。 |
| BILL-14 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 权益包增加基础/赠送组成及各自剩余、预占、已用；按到期顺序分包返回。 |
| BILL-15 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Hold 分配与结算关联具体权益包。 |
| BILL-17 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 人工赠送积分必须形成独立权益组成，自入账起 30 天到期，不允许创建无期限人工赠送余额。 |
| BILL-18 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有到期任务关闭权益包和账本处理；缺 BASE/BONUS 组成及与不确定 Hold 的完整目标规则 |
| BILL-19 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 权益到期时直接释放并移除仍不确定的 Hold，不恢复已过期可用额度；之后确认的上游用量进入人工损失复核，不重新扣减其他权益。 |
| BILL-21 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前与历史价格并存，结算/核对使用对应时点价格。 |
| BILL-22 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 支持全局默认和模型覆盖；每个策略可为统一价格或按 `Asia/Shanghai` 时间段计价，区间不重叠且完整可预览。 |
| BILL-26 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 申请、批准、拒绝和账本入账可审计。 |
| BILL-27 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 已有钱包、分录、Hold、调整和对账查询；应收、组成级权益及完整授权/导出保护未实现 |
| BILL-28 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 普通退款成功时撤销目标支付全部未消费 BASE 与关联 BONUS、BONUS 零现金退款及平衡账本待实现 |
| PAY-01 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | H3C 与 Simulator 均实现 Provider 接口；部分通用流程仍按当前能力和旧状态模型耦合 |
| PAY-02 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Provider capabilities 已存在并驱动部分 API/UI；UI 对退款仍显示禁用入口，目标能力快照不完整 |
| PAY-03 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前生产候选流程只有新华三支付宝；开发/测试 Payment Provider 的银联、微信等渠道不计入生产能力 |
| PAY-04 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 商品直接保存单一外部 SKU，无多 Provider 映射版本表 |
| PAY-05 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前目录仍明确为 UAT 暂定 |
| PAY-06 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有新华三手机号同步任务；尚未抽象供应商中立 Commerce Mapping |
| PAY-07 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 充值选择、二维码和结果留在品悟应用/Portal 内 |
| PAY-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 每次充值从服务端商品选择开始，不恢复旧二维码作为新订单 |
| PAY-09 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 服务端按 `product_id` 派生 SKU、金额和权益，拒绝客户端覆盖 |
| PAY-10 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | H3C 二维码和实付金额来自 Provider 响应，不由客户端生成 |
| PAY-15 | 降级实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Simulator 有签名回调；H3C 无回调，仅靠服务端查询，尚无统一回调/查询证据归并 |
| PAY-16 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Billing 后台查询不依赖客户端二维码页面持续打开 |
| PAY-17 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 有过期/修复基础；无72小时确定性人工审核案件 |
| PAY-18 | 降级实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 当前 H3C 不支持外部关单，只能停止本地展示并继续查询 |
| PAY-21 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 确认成功可幂等入账并更新额度；真实 Provider 仅查询收敛，且权益仍为聚合模型 |
| PAY-22 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 无组织在线购买和受益组织履约 |
| PAY-30 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | Provider 确认普通退款后的 BASE/关联 BONUS 原子冲销、退款预约消费及平衡账本待实现 |
| PAY-33 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 拒付、渠道部分强制退款、重复支付、晚到支付、退款失败和状态冲突进入统一复核队列，同时保持各领域状态独立；拒付绑定原支付流水、订单和权益包，按 BILL-33 撤销关联权益并为不足现金本金登记订单级应收，保留证据、责任人、结论和审计。 |
| PAY-34 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 每日获取 Provider 账单，对订单、支付流水、退款和 Billing 账本入账进行四方核对，生成可追踪差异。 |
| PAY-36 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36 | NOT_DEPLOYED | 每条差异必须包含分类、严重度、状态、责任人、修复动作、复核结果和审计；修复通过幂等命令写入，不允许直接改表。 |
| UI-01 | 已实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 不跳外部浏览器；个人信息也在弹层中展示。 |
| UI-03 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 组织页签按成员关系隐藏；当前身份模块只放在概览；移除普通用户 Token Account/供应商 Key 管理入口。 |
| UI-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 模型准备、内部结算状态、供应商凭据等从普通用户视图移除。 |
| UI-05 | 未实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 增加用户名、手机号修改和忘记密码入口及完整安全校验。 |
| UI-06 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 个人上下文显示个人权益；组织上下文只显示当前成员被授予的组织额度，不同时展示其他上下文余额。 |
| UI-07 | 未实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 桌面与 Web 展示同一个服务端活动上下文；收到更高上下文版本、窗口重新聚焦或切换成功时清空旧额度/权限缓存并刷新。 |
| UI-08 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 需展示总可用积分、每个包的 BASE/BONUS 余额、来源、到期时间、消耗顺序、即将到期提醒，以及 BONUS 零现金退款但普通退款成功时关联未消费 BONUS 随 BASE 撤销。 |
| UI-09 | 未实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 登录后的账号 Popover 增加入口按钮；使用专用 Tauri 命令打开受信 Portal URL，不复用任意外链接口。 |
| UI-10 | 已实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | Web 个人中心已提供个人权益充值入口；后续重构不得删除。 |
| UI-11 | 未实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 具有组织采购权限的企业管理员可在显式目标组织管理页购买组织权益。 |
| UI-12 | 未实现 | AUDIT_HEAD | CODE-AUDIT; DESKTOP-39; CONTRACT-36 | NOT_DEPLOYED | 当前能力不足时显示禁用按钮；目标要求完全隐藏不可执行退款入口 |
| ADM-01 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 当前目标已禁止8位初始密码；尚缺 BOOTSTRAP_PENDING 权限门禁、交互式无回显直接设密，以及默认256 bit/最少128 bit的一次性引导凭据、Base64URL字符集、15分钟有效期、5次失败锁定、一次读取销毁、原子兑换和重新签发代次隔离。 |
| ADM-02 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 仅在凭据丢失、意外停用或身份故障等异常导致全部超级管理员无法登录时，通过主机/集群受控离线命令创建短时紧急授权或恢复账号；禁止公网 API、禁止绕过最后一个有效超级管理员主动注销保护或替代计划内交接，执行必须告警并写不可删除审计。 |
| ADM-03 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 服务账号、后台任务和 Event Relay 使用独立工作负载身份、短期凭据和最小权限，不继承人类平台角色；审计记录 `actor_type=SERVICE`、服务名和任务运行 ID。 |
| ADM-04 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 支持列表、搜索、详情和生命周期操作。 |
| ADM-05 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有组织成员、固定角色和所有权管理；缺 Account 平台 RBAC、授权有效期和目标权限码体系 |
| ADM-06 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 角色绑定支持开始/到期时间；临时和紧急授权到期自动失效、撤销缓存并记录审计。 |
| ADM-07 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 允许运营/审批角色兼任和不同用户交叉审批，但禁止自审；ADM-37 强制审批命令只需一名独立审批人。 |
| ADM-08 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 不嵌入普通后台；通过受限网络/隧道访问。 |
| ADM-09 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有筛选和导出基础；需按 NFR-48 增加近期重新认证、默认脱敏、PII临时权限、水印、加密、一次性下载和导出配额。 |
| ADM-10 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 展示实际部署的 Account、Billing、Gateway、Portal、身份 Provider、模型网关、事件基础设施和 Relay；目标改为配置驱动探针，不在 Portal 写厂商判断分支。 |
| NFR-01 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 反向代理、Compose/Helm 和配置校验要求远程 HTTPS、开发 HTTP 限 loopback；无实际部署与证书验收证据 |
| NFR-02 | 已实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | Compose/Helm/反向代理模板将数据库、Redis、NATS、Relay、内部 API、New API 管理面和 Keycloak Admin 保持非公开；部署证据仍为 NOT_DEPLOYED |
| NFR-03 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 仓库无明文生产密钥且 Provider 凭据加密保存；生产 KMS/Vault、密钥轮换和部署证明未实现 |
| NFR-05 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 账号、资金、Token 和部分修复动作已有审计；注册/注销 Saga、审批、Provider 事实修复等目标动作缺失 |
| NFR-06 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 钱包、支付入账、Hold、默认路由、结算和事件消费有幂等基础；注册操作、结果重放、注销和多项修复缺完整指纹契约 |
| NFR-07 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 三服务具备事务 Outbox/Inbox、至少一次 Relay、死信和人工重放；业务最终收敛依赖死信处置且多项 Saga 缺失 |
| NFR-08 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 活动上下文按 OIDC Session 保存，未建立用户级全局 context_version、期望版本写入和跨端缓存失效 |
| NFR-09 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 迁移按服务版本化且 up/down 成对并受合同检查；缺已执行迁移不可修改门禁及环境级迁移前后证明 |
| NFR-10 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有备份恢复、切换回滚 runbook 和版本目录约定；无实际备份、恢复演练和部署验收证据 |
| NFR-11 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 当前单机 Compose 不具备高可用；Helm 仅为未部署模板，不能证明多副本或故障转移 |
| NFR-12 | 未实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 内部服务主要共享长期 INTERNAL_SERVICE_TOKEN，未使用逐工作负载短期身份、mTLS/OIDC 和 Subject 级授权 |
| NFR-13 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 管理员删除可清理部分资料和 Session；未完成 Identity 不可认证、用户名/手机号安全释放、三年分类保留及自动清除 |
| NFR-14 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有 SMSProvider 接口和厂商错误映射基础；启动路径及挑战模型仍绑定阿里云托管核验模式 |
| NFR-15 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | OIDC 负责主体、Account 负责组织权限；平台后台仍直接依赖 Keycloak platform-admin Realm Role |
| NFR-16 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 新华三客户端已隔离在 Adapter，但 Account/Billing 数据模型仍含 mall_*、单一 SKU 和厂商同步语义 |
| NFR-17 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有 PaymentProvider capabilities 并驱动部分操作；缺历史保留、权威终态、晚到窗口、账单及统一 UI/API/任务快照 |
| NFR-18 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有模型网关调用和 UsageEvidenceProvider 抽象；数据库、字段和核对流程仍含 New API 专属语义 |
| NFR-19 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 已有系统状态聚合和探针函数；组件列表及 New API/Relay 处理仍在 Portal 主流程硬编码 |
| NFR-20 | 部分实现 | AUDIT_HEAD | CODE-AUDIT; GO-UNIT; CONTRACT-36; DESKTOP-39 | NOT_DEPLOYED | 桌面运行时旧 llmapi_hub 路径已移除并有迁移测试；发布、安装、运行三层门禁及非计费 readiness 尚不完整 |

## 1. 产品目标

账号平台需要让个人用户以统一身份在品悟桌面端完成注册、登录、自动登录、模型对话、用量查看和充值；让组织用户在显式组织上下文中共享授权资源和组织钱包；让平台管理员能够管理账号、组织、角色、钱包、定价、支付异常、Token 路由、用量核对、审计和服务状态。

系统必须满足四个核心不变量：

1. 当前配置的 Identity Provider 是认证、密码和登录会话的唯一事实源；Account Service 是业务用户、用户名/手机号映射、组织关系和权限的唯一事实源；Commerce Provider Mapping 只维护外部商城客户映射，不参与通用认证和授权。
2. 余额只有 Billing 的不可变平衡账本一个资金事实源。
3. 模型供应商密钥只在 Token Gateway 的受控边界内解密。
4. 外部支付、模型网关或网络结果不确定时进入可核对状态，不猜测成功，也不重复执行高风险请求。

### 1.1 原记录中的目标变更

以下保留原文于 2026-08-22 记录的目标和变更历史，不代表本次集成用户重新批准了全部破坏性变更。状态描述原代码与目标的差距；本次已确认并保护的行为见第 0 节，目标冲突须单独评审，不能把历史记录或“需求已确认”写成当前功能完成。

| 变更编号 | target_requirement_id | 已确认需求 | 当前实现判断 | 目标验收摘要 |
|---|---|---|---|---|
| CR-01 | BILL-08–BILL-13 | 组织额度由企业管理员手动分配给成员，员工不可自主领取 | 未实现 | 组织钱包作为总额度池；分配只形成组织内成员额度，不转入个人钱包；调用同时占用成员额度和组织钱包 |
| CR-02 | BILL-21–BILL-25 | 系统管理后台设置模型价格，支持统一价格或按时间段分段计价 | 部分实现、计量与匹配口径已由 CR-42、CR-57 细化 | 当前仅支持“某模型立即生效的新价格版本”；需增加全局默认、分时、未来生效、生命周期、显式继承、冲突校验和预览，并遵守 CR-57 的确定匹配与 Hold 快照规则 |
| CR-03 | TOK-19, TOK-21, TOK-23, BILL-30 | 预占按照具体模型上下文动态计算 | 未实现 | 移除全局 128K 输入上限作为统一计费依据，改用模型上下文、请求输入和模型输出上限计算 |
| CR-04 | ACC-34, UI-06 | 只能使用当前活动个人/组织上下文授予的额度，个人中心只展示该上下文额度 | 部分实现 | Gateway 已按活动上下文找钱包；需让所有额度/权益 API 和 UI 严格按活动上下文返回，组织场景只显示该成员被授予额度 |
| CR-05 | ACC-06–ACC-09, UI-05 | 个人中心支持修改手机号、用户名和忘记密码 | 未实现 | 增加重新认证、短信验证、唯一性校验、身份 Provider/Account 一致性、会话撤销和外部 Commerce Customer 映射同步；具体 Keycloak/新华三协议留在 Adapter |
| CR-06 | ACC-19 | 组织删除为逻辑删除；有余额、有效权益包或未结算 Hold 时禁止删除 | 部分实现 | 当前已逻辑删除，但缺少跨 Billing 的删除前置检查；需增加冻结、预检、幂等删除和审计 |
| CR-07 | TOK-06, TOK-07, TOK-12, TOK-16, TOK-20 | 符合可用条件的模型都可供用户选择 | 已由 CR-35 细化状态语义 | 原记录把目录展示、上下文资格和请求预占混为一个可用条件；模型目录同步要求继续有效，状态划分以 CR-35 为准 |
| CR-08 | BILL-14, BILL-17, BILL-16, UI-08 | 分别展示基础积分、赠送积分、到期顺序、即将到期提醒和多个权益包，同时展示可用总积分 | 部分实现 | 当前有多个权益包、总可用数和最近到期摘要，但包内没有基础/赠送余额拆分，UI 展示不足 |
| CR-09 | SCOPE-04, SCOPE-05, TOK-15, TOK-35, TOK-36, TOK-37, NFR-20 | 删除旧模型路径及兼容代码，全量迁移到账号平台 + Token Gateway | 未实现 | 实现范围仅限品悟客户端；除删除功能开关、旧 `llmapi_hub` 身份/Token/模型回退和运行时双路径外，还缺少客户端数据迁移、凭据清理、三层门禁、非计费 readiness、升级矩阵及版本级回滚验收；TOK-37 服务端前置能力必须由独立任务先行完成 |
| CR-10 | ACC-13, ADM-05, ADM-06, ADM-07 | 基于当前全部能力重新设计角色系统 | 设计完成、待实现 | 采用平台与组织两级、权限码驱动、作用域明确、默认拒绝、资金审批分离的 RBAC；见 `role-system-design.md` |
| CR-11 | UI-09 | 品悟应用内个人中心提供 Web 个人中心入口 | 未实现 | 桌面账号弹层增加“打开 Web 个人中心”按钮，通过受信配置打开 `/account.html`；不在 URL 中携带 OIDC Token |
| CR-12 | UI-10, PAY-07, PAY-08 | Web 个人中心提供充值入口 | 已实现、需保留 | 当前个人活动上下文的“额度”页已经支持套餐选择、商城二维码和到账查询；后续 UI/角色重构不得删除 |
| CR-13 | ID-08, NFR-14 | 短信业务流程与供应商实现隔离 | 部分实现、待重构 | 当前已有 `SMSProvider` 接口，但接口绑定“供应商生成并核验验证码”模式且启动代码直接构造阿里云实现；目标支持阿里云号码认证、阿里云普通短信和其他平台的可替换适配器，见 `sms-verification-provider-design.md` |
| CR-14 | TOK-05, NFR-18 | 模型网关与 New API 具体实现隔离 | 部分实现、待重构 | Token Gateway 领域、数据库、公开 API 和管理 UI 统一使用 `upstream_request_id`、`UsageEvidenceProvider`、`ModelCatalogProvider` 等供应商中立概念；New API 请求头、日志接口、响应字段、状态与错误映射只能存在于 New API Adapter |
| CR-15 | ACC-05, PAY-06, PAY-01, NFR-16 | 外部商城账号和订单语义与新华三实现隔离 | 部分实现、待重构 | `mall_user_sync_jobs`、`mall_sync_status`、`mall_order_id` 等厂商语义迁移为外部客户开通状态和 Provider 订单引用；Account、Gateway、Billing 及用户 UI 不直接识别新华三商城，充值前置条件由当前支付 Provider 的能力与客户要求表达 |
| CR-16 | CORE-01, CORE-02, ACC-01, NFR-15 | 身份认证 Provider 与业务授权隔离 | 部分实现、待重构 | Keycloak 用户创建/删除迁入身份 Adapter；OIDC 只提供标准主体和会话身份；平台及组织权限由 Account 权限系统判定，业务服务不再把 Keycloak `realm_access` 或固定 `platform-admin` Realm Role 作为长期授权事实源 |
| CR-17 | PAY-01–PAY-04, NFR-17 | 生产/非生产支付 Provider 与支付渠道能力隔离 | 部分实现、待重构 | 保留通用 `PaymentProvider` 端口，将 H3C Mall 与 Simulator 实现移出 Billing 业务包；远程 Compose 已隔离 Simulator 主进程，但测试数据库初始化/迁移仍在基础清单，启动和 Helm 仍有模拟器命名配置；需迁移为供应商中立配置并彻底排除生产测试依赖；前端按商品渠道和 Provider capabilities 渲染，多 Provider 使用独立商品映射 |
| CR-18 | ADM-10, NFR-19 | 系统运行探针配置驱动 | 部分实现、待重构 | 系统状态页通过探针注册表声明服务名称、URL、协议和响应判定器；Portal 不使用 `if newapi` 等厂商分支。页面可以展示实际部署组件名称，但采集流程不能依赖固定厂商实现 |
| CR-19 | PAY-12, PAY-14–PAY-15, PAY-18–PAY-20, PAY-23, PAY-25, PAY-28–PAY-31, PAY-33–PAY-36 | 支付按完整生命周期设计并允许 Provider 能力降级 | 部分实现、退款范围已由 CR-41 取代 | 原记录包含全额/部分退款；现行支付履约隔离、退款目标、金额和意图事务以 CR-41 及 PAY-24–PAY-31 为准，其他支付生命周期要求继续有效 |
| CR-20 | BILL-07, PAY-22, PAY-23, UI-11 | 组织资金通过企业管理员在线购买组织权益入账 | 未实现 | 企业管理员在组织管理界面选择组织商品并在线支付；订单付款人与权益受益组织分离，成功后直接形成组织权益包和组织钱包账本，不经过个人钱包 |
| CR-21 | SCOPE-01, SCOPE-02, TOK-15, TOK-01 | 不实现个人中心自有供应商 Key/自管 Token Account | 移除 | 本地应用直接配置的云端或本地模型完全绕过账号平台，继续由桌面模型配置管理；个人中心不提供供应商 Key、个人 Token Account、共享策略或路由管理 |
| CR-22 | ACC-29 | 每个有效组织恰好一个所有者 | 待重构 | 所有权只能通过专用原子转让命令变更；普通管理员允许多人，不能用普通角色分配接口增删所有者 |
| CR-23 | TOK-19, TOK-21, TOK-23, BILL-30 | 动态预占采用已确认的边界策略 | 部分由 CR-36 取代 | 无 Tokenizer 时安全裕量为 0、Hold 续租和追加预占继续有效；“未传最大输出按 1,000,000 Token”和固定 Token 预占公式由 CR-36 取代 |
| CR-24 | TOK-06, TOK-07, TOK-12, TOK-16, TOK-20 | 只展示符合可用条件的模型 | 已由 CR-35 取代 | 原记录要求把上下文和路由资格作为目录展示前提；现行规则允许 CATALOG_VISIBLE 独立展示，并分别计算 CONTEXT_ELIGIBLE 与请求级 RESERVABLE |
| CR-25 | BILL-16–BILL-17, BILL-19, BILL-28, PAY-24–PAY-31 | 权益退款、消费顺序和过期 Hold 采用确定规则 | 未实现 | 普通退款现金只按目标支付流水与权益包组合的全部未消费 BASE 计算，成功时同时撤销该支付产生的全部未消费关联 BONUS，BONUS 不产生现金退款；人工赠送积分有效期 30 天；同日到期按赠送、基础排序；权益过期时直接释放并移除不确定 Hold，后续证据转人工损失复核 |
| CR-26 | BILL-05, BILL-08–BILL-13 | 组织额度按权益包分配并保留成员占用 | 未实现 | 管理员选择具体权益包和组成；批量操作逐项提交；非账号注销的成员移除使未消费授予不回池而转为可恢复暂停，原用户 ID 重新加入且未过期时恢复；管理员可给自己分配；授予不能改额，只能撤回后重建；账号注销另按 BILL-39 处理 |
| CR-27 | ACC-34, ACC-35–ACC-39, UI-07 | 桌面与 Web 共享一个用户级活动上下文 | 待重构 | 活动上下文不再按 OIDC Session 独立保存；任一端切换后其他端自动同步，进行中的调用按开始快照结算；成员移除或组织停用自动回个人；所有业务 API 返回上下文版本 |
| CR-28 | ADM-01–ADM-03, ADM-06 | 超级管理员初始化、恢复和授权生命周期 | 未实现、ADM-01 初始化凭据由 CR-58 取代 | 原记录使用一次性8位随机初始密码；现行首个管理员初始化改用交互式直接设密或高熵一次性引导凭据；Break-glass 仅处理全部管理员异常失联，不绕过最后一个有效超级管理员主动注销保护，临时授权和自动回收部分保持有效 |
| CR-29 | ADM-07 | 审批允许角色兼任和交叉审批 | 未实现、由 CR-59 细化 | 同一用户可同时拥有计费运营和审批角色，仍禁止审批自己的申请；不同用户可以互相审批；强制审批命令只需一名独立审批人，普通退款和组织删除不进入财务审批流，全部写审计 |
| CR-30 | ACC-26 | 组织邀请支持手机号、用户名和一次性链接 | 部分实现、待重构 | 三种方式都使用统一邀请实体、过期与一次性消费规则；邀请链接只保存摘要，手机号/用户名查询防枚举，接受时绑定当前已认证用户 |
| CR-31 | ACC-10, ACC-11 | 用户可主动注销并保留三年法定记录 | 未实现、由 CR-52 细化 | 有个人余额、未清应收、本人未完成订单、未结算 Hold 或尚未转让的组织所有权时禁止完成注销，其他资金/外部事实及自动清理门禁按 CR-52；未消费组织授予不阻止注销并按 CR-51 处理；注销后标识可复用，法定记录以不可反查业务身份保存 3 年 |
| CR-32 | ACC-07, ACC-08, ACC-06 | 用户资料修改采用可恢复 Saga | 未实现 | Account 创建带版本的变更请求并保留旧值/新值；经重新认证和验证后依次更新 Identity Provider、Account 与 Commerce Mapping；通过幂等重试、状态机和人工修复收敛，不宣称跨系统原子事务 |
| CR-34 | BILL-29–BILL-33, PAY-33 | 用量负余额与拒付应收分场景记账并按钱包上下文隔离冻结 | 未实现 | 用量超额结算按 BILL-29 双记钱包负余额与独立应收；拒付按原支付流水和权益包撤销可撤销 BASE/关联 BONUS，不从聚合钱包反推或冲销其他权益；不足现金本金形成订单级应收，后续现金充值 BASE 优先清偿；个人与组织债务只冻结各自上下文 |
| CR-35 | TOK-07, TOK-12, TOK-16, TOK-20 | 模型状态拆分为目录展示、上下文资格和请求级实时预占 | 未实现 | CATALOG_VISIBLE 只表示目录展示；CONTEXT_ELIGIBLE 判断当前上下文权限、策略和路由；RESERVABLE 在具体请求创建 Hold 时实时计算；客户端“可调用”仅为提示，Gateway 每次请求必须重新判断 |
| CR-36 | TOK-04, TOK-17–TOK-20, BILL-20–BILL-21, BILL-23–BILL-25 | 模型协议和计价抽象为版本化计量单位及协议预占公式 | 未实现、价格口径由 CR-42 细化 | 使用版本化单位并为每种协议发布独立预占公式；公式缺省参数按 0，历史 1,000,000 Token 缺省输出规则失效；公式与价格集合、输入互斥计量及零价语义以 BILL-23–BILL-25 为准 |
| CR-37 | ID-14, ID-15, ACC-02–ACC-04, BILL-04, UI-02 | 注册采用持久 PROVISIONING，钱包初始化异步重试 | 未实现 | Identity 与 Account 成功即账号存在；钱包完成前允许登录和资料管理但禁止平台模型及充值；超过 SLA 才进入 REPAIR_REQUIRED；Billing 暂时不可用不得删除已验证手机号身份 |
| CR-38 | SCOPE-01, SCOPE-04, SCOPE-05, TOK-35, TOK-36, TOK-37, NFR-20 | 补齐品悟客户端旧模型路径的数据迁移和切换验收 | 未实现、由 CR-44 细化门禁 | 只改品悟客户端代码及相关文件；一次性幂等迁移旧客户端状态并保护本地独立模型，按三层门禁和升级矩阵完成失败行为、请求目的端及版本级回滚验收，不保留运行时兼容分支；TOK-37 作为独立前置能力，不进入客户端迁移提交 |
| CR-39 | PAY-33, PAY-14, PAY-12, PAY-32, PAY-13, PAY-26 | 删除主动部分退款状态并拆分支付领域状态机 | 未实现 | 主动退款不含 PARTIALLY_REFUNDED；渠道部分强制退款作为被动异常进入复核；订单关闭、支付流水、退款和拒付分别维护状态，订单资金状态只保存可重建派生摘要 |
| CR-40 | TOK-07–TOK-10, TOK-12, TOK-16, TOK-20, ADM-25, ADM-28–ADM-30 | 闭合默认托管路由创建与模型资格判断顺序 | 未实现 | 目录可见后先完成托管 Token Account/路由 Provisioning，只有 READY 路由参与 CONTEXT_ELIGIBLE；状态查询和修复分别由 ADM-29、ADM-30 验收，再执行请求级 RESERVABLE、原子 Hold 和上游调用 |
| CR-41 | BILL-28, PAY-11, PAY-20, PAY-24–PAY-31, PAY-33 | 支付履约隔离并按实际流水和权益包原子创建退款意图 | 未实现 | 订单可包含多笔支付流水，每个支付权益包只绑定一笔成功流水；普通退款目标为 payment_transaction_id + entitlement_package_id，任一 BASE/关联 BONUS 曾消费即拒绝，锁定、冻结、只按全部未消费 BASE 计算并预约现金退款、快照全部待撤销 BASE/关联 BONUS、refund_request 与 Outbox 在一个事务提交，提交后才调用 Provider |
| CR-42 | BILL-20, BILL-23–BILL-25, ADM-12–ADM-13 | 闭合公式单位、输入缓存计量和零价语义 | 未实现、由 CR-57 扩展继承语义 | 每个公式引用单位必须有配置项，INPUT_TOKEN 与 CACHED_INPUT_TOKEN 互斥，显式零价与缺价分离；模型配置项可以按 CR-57 显式继承全局，不能以缺失表达继承 |
| CR-43 | SCOPE-03, ADM-11, ADM-25–ADM-30 | 隔离组织模型业务策略与平台 Token Gateway 运营权限 | 未实现 | 组织模型管理员只能管理模型允许/拒绝策略、目录范围等业务策略；任何组织角色都不能读取或管理真实 Token Account、路由、凭据和 Provisioning，相关平台原子权限只授予 PLATFORM_MODEL_ADMIN |
| CR-44 | SCOPE-04, SCOPE-05, TOK-35, TOK-36, TOK-37, NFR-20 | 将客户端切换拆分为发布、安装和运行三层门禁并移除计费准入请求 | 未实现 | 服务端未就绪时不发布；迁移预检或暂存提交失败时不替换旧版本并原子恢复旧二进制/配置；新版本运行时只显示不可用和修复动作，不回退旧模型路径；准入使用无 Hold readiness 或隔离合成身份，不使用真实用户付费请求 |
| CR-46 | TOK-24–TOK-30 | 补齐模型请求幂等、流式重放、断线恢复和 Hold 后派发恢复契约 | 未实现 | TOK-24 继续定义同一逻辑请求不重复调用上游；新增稳定用户级作用域、分层保留、同键异请求冲突、加密流事件重放、断线状态恢复和上游派发不确定时禁止盲重试 |
| CR-47 | REQ-ID-01–REQ-ID-03, CORE-01–CORE-07, SCOPE-01–SCOPE-05, ID-01–ID-16, ACC-01–ACC-39, TOK-01–TOK-37, BILL-01–BILL-33, PAY-01–PAY-36, UI-01–UI-12, ADM-01–ADM-32, NFR-01–NFR-25 | 删除退休需求并按依赖关系重排、重新编号全部目标能力 | 文档完成、实现状态保持原判断 | 删除编号稳定与禁止重排规则；移除目标基线中的退休能力占位；按基础能力、依赖顺序和领域相关性形成连续新编号，并同步全部文档和实现状态引用 |
| CR-48 | TOK-29, TOK-38–TOK-41, BILL-34–BILL-35, UI-13 | 补齐用户取消、连接断开与模型计费收敛契约 | 未实现 | 取消意图、Provider 取消结果、服务端执行、客户端交付和计费结算分别保存；断线不取消，Provider 不支持取消时继续核对实际用量，按可信实际生成量计费并在取消期间安全续租、结算或超时释放 Hold |
| CR-49 | PAY-17, PAY-19, PAY-35, PAY-37–PAY-40, ADM-33 | 72小时未收敛订单转人工审核并闭合无回调无账单 Provider 的晚到支付处理 | 未实现 | 前72小时查询计划不变；边界最后查询后创建带责任人和 SLA 的审核案件，前7天每日、此后每周人工查询；只有有限晚到窗口、长期历史查询和权威终态的 Provider 才能正式收款，并以最终成功查询作为未支付归档门禁 |
| CR-50 | BILL-36–BILL-38 | 明确组织权益组成资金守恒、禁止超额授权及并发分配边界 | 未实现 | 每个权益包 BASE/BONUS 组成满足可分配总量等于可用余额、有效授予未消费额、暂停授予未消费额和关联 Hold 之和；所有桶互斥，新增授予只使用组成可用余额，并通过组成级锁/CAS、幂等键和请求指纹防止多管理员并发超分 |
| CR-51 | ACC-10, ACC-11, BILL-12, BILL-13, BILL-36, BILL-39 | 注销用户的未消费组织授予终局占用至原到期日 | 未实现、由 CR-54 调整 | 原规则禁止任何回收；现行规则仍要求 `ACCOUNT_CLOSED` 授予不自动回池、不可恢复、消费或普通撤回，但允许按 CR-54 的独立专用命令主动回收 |
| CR-52 | ACC-10–ACC-11, ACC-40–ACC-50, BILL-39, UI-14, ADM-34 | 用持久注销 Saga 区分资金阻断、系统自动清理和标识释放门禁 | 未实现 | 待核对用量、拒付、资金性差异、外部待确认关单及资金相关 Commerce 任务阻断注销；组织所有权及最后一个有效超级管理员角色须先交接，资料 Saga、其他角色、临时授权和邀请不要求用户处理但必须自动收敛；全部门禁通过且 Identity 不可认证后才能释放标识，迟到任务不得作用于新账号 |
| CR-53 | ID-03, ID-14–ID-15, ID-17–ID-18, ACC-02, ACC-51–ACC-55, UI-15, ADM-35, NFR-04, NFR-06 | 闭合注册预检并发、唯一值预约、幂等、密码边界和孤儿 Identity 补偿 | 未实现 | 预检不代替数据库唯一约束；短信前创建短预约，验证后升级，Identity 创建后预约无 TTL；稳定操作 ID 贯穿全流程；密码只进入 Identity Adapter且不得 trim；Identity 孤儿先向前恢复，30分钟转修复、24小时且证明无业务事实后才可补偿 |
| CR-54 | BILL-12, BILL-36, BILL-38–BILL-40 | 允许授权管理员选择性回收注销账号的未消费组织授予 | 未实现 | 注销授予不自动回池；账号完成 `ANONYMIZED` 且不存在消费、Hold、待核对用量或资金冻结时，授权管理员可用专用命令一次性全额回收至原权益包原组成，禁止部分回收、跨组成转换、现金退款或直接授予新账号 |
| CR-55 | TOK-42–TOK-45 | 补齐模型目录管理生命周期、历史展示、重命名/别名和版本回滚 | 未实现 | 目录管理使用 DRAFT/PUBLISHED/DEPRECATED/REMOVED；上游删除不清除历史快照；模型 ID 稳定且名称/别名版本化；回滚通过复制历史期望状态形成经当前依赖重新校验的新版本 |
| CR-56 | TOK-46–TOK-47 | 明确模型关联版本生成边界及下架后的新旧会话和在途请求策略 | 未实现 | 协议、能力、价格、路由和目录独立版本化并冻结到请求；弃用可配置新会话/继续会话截止时间，移除后历史仍可读、替代必须显式分支，在途请求按受理快照完成或走证据化紧急取消 |
| CR-57 | BILL-21–BILL-25, BILL-41–BILL-45, ADM-13 | 明确定价匹配优先级、显式单位继承、时间冲突、生命周期及 Hold 不变性 | 未实现 | 模型覆盖优先；单位只能固定定价或显式继承，零价阻止回退；同作用域未来区间不得重叠；价格使用 DRAFT/PUBLISHED/ACTIVE/ENDED/CANCELLED；撤销和提前结束不影响既有 Hold |
| CR-58 | ADM-01, ID-18, NFR-04 | 用交互式设密或高熵一次性引导替代8位超级管理员初始密码 | 未实现 | admin 激活前保持 BOOTSTRAP_PENDING；直接设密使用高权限密码策略；引导凭据默认256 bit、最少128 bit、Base64URL、15分钟有效、最多5次失败、一次读取销毁且成功兑换原子失效 |
| CR-59 | ADM-07, ADM-17, ADM-22, ADM-36–ADM-42 | 补齐管理命令审批对象、生命周期、失效和执行契约 | 未实现 | 所有后台写命令显式归类为强制审批或仅审计；人工财务变更申请审批分离，普通状态机与治理命令只审计；审批支持过期、撤回、新申请重提、关键资源/策略变化失效、执行前实时授权复核及 Outbox 幂等执行 |
| CR-60 | NFR-26–NFR-48, ADM-09 | 建立性能、容量、灾备、保留、限流、隔离、Provider韧性、时钟、客户端兼容和审计导出的量化验收基线 | 未实现 | 在生产等价环境按固定样本测量P95/P99/TTFT和容量；定义分级RPO/RTO、加密备份与演练、分类保留、多维限流、租户隔离矩阵、熔断/人工SLA、授时偏差、N/N-1/N-2桌面兼容及受控审计导出 |
| CR-61 | ACC-02, ACC-11, ACC-56–ACC-58, NFR-50 | 建立永久内部用户 ID 和独立可展示用户编码 | 部分实现 | 现有 UUIDv7 主键和逻辑删除已保留内部 ID；需增加数据库级不可变/不复用守卫、独立随机 `user_code`、永久编码登记、本人展示、运营精确查询及防枚举控制 |

这些需求之间的强制关系：

- 活动上下文决定“谁在使用”，角色决定“允许做什么”，额度授予决定“最多能花多少”，模型目录/策略决定“可以选择什么模型”；四者必须独立判断。
- 组织额度分配不是钱包转账。员工切回个人上下文后，不能看到或使用组织授予额度。
- 组织总额度池通过企业管理员在线购买组织权益形成；成员分配仍只是限额授权，不得把组织权益转入个人钱包。平台人工调整只用于受审计修复，不是正常采购入口。
- 模型目录展示、托管路由准备、上下文资格和请求级预占必须按 TOK-10 独立且有序判断：`CATALOG_VISIBLE` 决定目录展示，Provisioning READY 提供可命中托管路由，`CONTEXT_ELIGIBLE` 判断当前上下文权限/策略/路由，`RESERVABLE` 紧邻具体请求的 Hold 原子创建实时计算。
- 模型目录管理生命周期与模型调用资格相互独立：TOK-42–TOK-47 决定目录修订如何发布、弃用、移除、展示历史、回滚、生成关联版本以及处理下架会话；每次新请求仍从当前已发布目录版本重新执行 TOK-10，历史会话展示只读取请求时快照。
- 定价先按 BILL-41 选择模型或全局作用域，再按 BILL-42 逐单位解析固定价/显式继承；BILL-43–BILL-44 保证同作用域时间唯一和生命周期合法，BILL-45 保证任何后续撤销、结束、替代或回滚都不重算已有 Hold。
- 首个超级管理员初始化不再创建可直接登录的短密码：ADM-01 只允许受控交互式直接设密或高熵一次性引导，admin 在原子完成正式密码设置前保持 BOOTSTRAP_PENDING；凭据失败、读取、兑换、过期和重新签发都必须可审计且不可恢复明文。
- 删除旧路径采用客户端一次性幂等数据迁移和明确切换，不保留长期运行时兼容分支；实现改动不得越过品悟客户端及其直接相关文件边界。
- Web 个人充值只增加个人钱包额度；具备组织采购权限的企业管理员可以在组织上下文显式购买组织权益。付款人、订单所有者和权益受益组织必须明确保存，不能把个人充值静默记入组织钱包。
- 桌面打开 Web 个人中心时只传递目标页面，不在 URL、查询参数、Fragment 或浏览器命令中传递 Access/Refresh Token；浏览器使用自己的 Portal OIDC 会话，未登录时进入正常 Web 登录流程。
- 注册、登录、改手机号和找回密码只依赖统一验证码挑战服务，不识别阿里云 Action、签名、模板 CODE 或响应字段。更换短信方案不得修改这些业务流程、公开 API 或 Keycloak 手机登录协议。
- “删除旧模型路径”不仅是关闭功能开关：桌面持久化模型中不得继续保存 New API 用户 ID、Token ID、后台密码或 API Key，也不得在账号平台不可用时回退到旧 New API 用户开通和设备账号流程。
- 模型调用领域只认识上游模型网关、上游请求关联和用量证据；`X-Oneapi-Request-Id`、`New-Api-User`、New API 日志查询参数及其错误文本必须由 New API Adapter 转换为统一模型。
- 手机注册成功后的商城账号开通属于外部 Commerce Customer Provisioning，不属于用户授权上下文本身。商城未开通只能影响需要该商城客户身份的支付动作，不能成为模型调用、组织授权或普通账号功能的通用前置条件。
- 身份 Provider 负责“用户是谁”，Account 权限系统负责“允许做什么”。Keycloak Realm Role 只允许作为迁移期引导或受控运维入口，不能继续散落在 Account、Billing、Token Gateway 的业务路由上。
- Provider 特殊规则可以存在于对应 Adapter 内，例如新华三只支持支付宝、没有退款/关单/回调接口；通用 Billing 状态机和前端只能消费能力声明及统一错误，不得按 Provider 名称分支。
- 本地应用中由用户直接配置、完全不经过账号平台的云端或本地模型不属于 Token Account 管理，也不属于需要删除的旧内置模型路径；个人中心不新增用户供应商 Key 功能。
- 退款、对账和支付异常以完整产品闭环为目标。Provider 缺少某项能力时只能按 capabilities 执行明确降级，不能反向删除通用领域状态、审计和人工复核要求。
- 注销资格中的资金阻断与系统自动清理必须分离：待核对用量、拒付、资金性差异、外部待确认关单和资金相关 Commerce 任务未收敛时不能释放身份；组织所有权和最后一个有效超级管理员角色必须先交接，资料 Saga、其他角色、临时授权和邀请不要求用户手工处理，但自动清理未完成同样不能越过标识释放门禁。
- 注册短信前预检只用于减少无效短信，数据库正式映射与活动预约才是并发唯一性事实源。密码策略、原始密码及同键异密码判断只存在于 Identity 边界；Identity 已创建后预约不能按 TTL 释放，必须由 Account 提交或经过证明的补偿收敛。

### 1.2 平台实现隔离的统一验收规则

本节适用于短信、身份认证、外部商城、支付、模型网关和运维探针。隔离完成必须同时满足：

1. 领域服务、核心状态机、数据库核心字段、公开 OpenAPI 和普通用户 UI 使用供应商中立名称，不以 `aliyun`、`keycloak`、`h3c`、`mall`、`newapi`、`oneapi` 或 `simulator` 定义业务语义。
2. 厂商 Action、请求头、签名、Endpoint、状态码、响应结构、错误文本和重试约束仅存在于该厂商 Adapter；Adapter 必须把它们映射为统一能力、状态和错误分类。
3. `cmd/*`、Compose、Helm、Secret 和运维文档可以出现实际厂商名称，因为它们承担 Provider 选择和依赖装配；这些配置不得反向渗入业务 Handler、领域实体或前端条件分支。
4. 替换同类 Provider 时，注册/登录、充值、模型调用等业务 Handler、公开 API、桌面业务交互和核心账本结构不需要修改；只允许新增/替换 Adapter、配置、Provider 专属迁移或商品映射。
5. 历史记录必须保存创建时使用的 Provider ID、Adapter 版本和脱敏外部引用，保证 Provider 切换期间仍能查询、核验和收敛；不得用当前 Provider 解释旧订单、旧验证码或旧用量请求。
6. Provider 不支持的能力通过 capabilities 明确返回并在 UI 禁用，不能用本地状态伪造外部成功，也不能因切换 Provider 自动重复高风险请求。
7. 每个 Adapter 使用同一套合同测试验证正常、拒绝、超时不确定、重复请求、状态映射和敏感信息保护；替换验收必须证明业务层没有新增厂商条件分支。

允许保留的实现细节包括：`internal/platform/**` 或明确 Adapter 包中的厂商客户端，`cmd/*` 中的 Provider Factory，Compose/Helm 的实际镜像和环境变量，以及系统状态页对真实部署组件的展示名称。PostgreSQL、Redis 和 NATS 当前已经分别通过 Repository、Store 和事件端口形成基本边界，本轮不要求为了消除名称而机械替换。

## 2. 用户与角色

| 参与者 | 能力边界 |
|---|---|
| 未登录用户 | 密码登录、短信登录、手机注册、滑块与短信验证。 |
| 个人用户 | 个人上下文、余额/权益包、充值、个人用量、平台托管模型路由。不能创建顶层组织。 |
| 组织成员 | 在已加入组织内按角色和节点范围使用组织资源；不能越过当前会话上下文。 |
| 组织所有者/管理员 | 管理组织节点、成员和可分配角色；所有者转让走专用流程。 |
| 平台管理员 | 当前使用有效 Keycloak `platform-admin` Realm role 进入后台；Account 平台作用域权限是后续 RBAC 目标，不是现有授权事实。 |
| 运维人员 | 通过受限网络管理 Keycloak、数据库、NATS、New API 和部署，不等同于平台管理员。 |

## 3. 功能需求矩阵

后续矩阵保留原审计实现判断与目标差距，不是本波功能完成清单。当前适用边界须同时按第 0 节的 A06 校正读取；原字段“当前实现/边界”仅对应 AUDIT_HEAD。

### 3.1 注册、认证和会话

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| ID-01 | 密码支持常用英文符号 | 已实现 | 8–128 位可打印 ASCII，至少一个字母和一个数字。 |
| ID-02 | 手机注册必须填写唯一用户名、手机号和两次一致的密码 | 已实现 | 前端核对两次密码，后端仍独立校验用户名、手机号和密码。 |
| ID-03 | 用户名、手机号和密码在发短信前预检 | 部分实现 | 已在短信前完成用户名、手机号和密码预检，但密码仍由 Account 接收和校验，未收敛到 Identity Adapter。 |
| ID-04 | 获取短信前完成滑块挑战 | 已实现 | 服务端生成一次性挑战，客户端用覆盖登录框的弹层完成；不是第三方 CAPTCHA。 |
| ID-05 | 短信请求限流 | 已实现 | 同手机号至少间隔 60 秒，最多 5 次/小时；前端显示倒计时。供应商明确发送失败不占冷却。 |
| ID-06 | 短信挑战有过期、尝试次数和一次性消费约束 | 已实现 | 5 分钟有效，最多 5 次核验，成功后不可复用。 |
| ID-07 | 阿里云发送并核验验证码，平台不保存明文验证码 | 已实现 | Account 只保存 Provider 挑战标识和状态元数据。 |
| ID-08 | 可替换短信验证后端 | 部分实现 | 业务层只管理挑战、用途、风控和结果；Provider 适配层负责厂商协议。既支持厂商托管验证码，也支持平台安全生成、仅通过短信厂商发送的模式。 |
| ID-09 | 账号密码登录支持手机号或用户名 | 已实现 | 桌面端通过 Keycloak Direct Access Grant；不持久化密码。 |
| ID-10 | 手机号 + 短信验证码登录 | 已实现 | Keycloak 自定义 Direct Grant 调用 Account 内部核验接口；仅中国大陆手机号。 |
| ID-11 | 短信反枚举 | 部分实现 | 未知手机号返回相同外部响应且不发送短信，但尚无统一耗时和完整多维限流证据。 |
| ID-12 | 登录成功后下次启动自动登录 | 已实现 | Refresh Token 仅存系统凭据库，启动时刷新。 |
| ID-13 | 登出、刷新失败或凭据错误后停止自动登录 | 已实现 | 失败路径删除持久化 Refresh Token，不回退历史缓存。 |
| ID-14 | 注册不留下不可登录的半成品账号 | 部分实现 | Identity/Account 成功后已持久返回可登录的 `PROVISIONING`，钱包 bootstrap 不决定注册响应；Identity 已创建而 Account 提交失败仍依赖即时删除补偿，尚无持久恢复 Saga。 |
| ID-16 | 除密码/短信外的 MFA 和账号申诉 | 非当前范围 | Keycloak 具备扩展基础，但当前产品闭环未实现；短信找回密码已纳入 ACC-09。 |
| ID-17 | 密码只在 Identity 边界处理 | 未实现 | 当前注册预检和编排尚未证明 Account、短信及其他服务完全不接收密码，也没有密码策略版本快照和 Identity 边界短期幂等指纹。 |
| ID-18 | 密码原始字符策略 | 未实现 | 当前缺少端到端首尾空格明确拒绝、中间半角空格允许、原始序列精确比较及禁止 trim/规范化/转换/截断测试。 |

验收重点：Identity 与 Account 已成功时必须返回“账号已创建”的 PROVISIONING/ACTIVE 结果；钱包短暂失败不得返回注册失败、密码错误或删除手机号身份。短信前预检不替代数据库唯一约束；Identity 创建后的预约不得按 TTL 释放。密码只进入 Identity 边界，首尾空格明确拒绝且任何层不得 trim；密码、验证码、Access Token 和 Refresh Token 不得出现在日志或前端业务 JSON 中。

### 3.2 用户、组织和角色

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| ACC-01 | 身份 Provider 管理适配 | 部分实现 | 已有 Keycloak 身份管理 Adapter 接口；Account 仍接收注册密码并直接编排厂商管理语义，边界未完全收敛。 |
| ACC-05 | 外部 Commerce Customer 开通 | 部分实现 | 已有新华三专用 `mall_user_sync_jobs` 异步任务；缺供应商中立 Commerce Mapping、Adapter 版本和通用状态模型。 |
| ACC-06 | 资料变更 Saga | 未实现 | 用户名和手机号变更使用唯一变更请求、字段预约、版本检查、Identity Provider 更新、Account 提交、会话撤销及 Commerce 异步同步；失败可重试或进入人工修复。 |
| ACC-07 | 修改用户名 | 未实现 | 个人中心发起，重新认证后检查规范化用户名唯一，并通过身份 Adapter 与 Account 保持一致。 |
| ACC-08 | 修改手机号 | 未实现 | 必须验证新手机号并重新认证；同步身份 Provider、Account 和当前 Commerce Customer 映射，失败进入可恢复状态。 |
| ACC-09 | 忘记密码 | 未实现 | 未登录入口使用手机号短信验证，通过身份 Adapter 重置密码并撤销既有 Refresh Token/会话。 |
| ACC-10 | 用户主动注销 | 未实现 | 重新认证后可以持久化注销请求；完成前强一致检查个人余额、未清应收、本人订单/退款、Hold、待核对用量、拒付、资金性差异、外部关单和 Commerce 任务，并要求转让组织所有权。组织余额和未消费组织授予不阻断；授予按 BILL-39 进入账号注销隔离暂停。 |
| ACC-11 | 注销记录保留 | 未实现 | 当前管理员删除保留 UUID 并清除部分资料，但完整注销 Saga、用户名/手机号安全释放、三年分类保留和永久 `user_code` 墓碑均未实现。 |
| ACC-12 | 一个用户可属于多个组织 | 已实现 | 成员关系独立存储；个人中心只显示当前用户关联组织。 |
| ACC-13 | 角色系统按权限码和作用域管理 | 部分实现 | 组织角色与作用域已有基础，但权限判断仍硬编码，平台权限继续依赖 Keycloak Realm Role。 |
| ACC-14 | 个人用户不能创建顶层组织 | 已实现 | 顶层组织创建位于平台管理 API/UI。 |
| ACC-15 | 创建顶层组织 | 部分实现 | 已有平台管理员创建接口/UI、字段校验、串行事务和审计/Outbox；缺目标 Account 平台权限与稳定幂等键。 |
| ACC-16 | 搜索组织 | 部分实现 | 已有按名称或 ID 搜索及稳定更新时间排序；缺状态筛选、游标分页和 Account 平台权限模型。 |
| ACC-17 | 启用组织 | 部分实现 | 已有 DISABLED→ACTIVE 幂等转换、成员/授权版本更新和审计；缺跨 Billing 恢复条件检查。 |
| ACC-18 | 停用组织 | 部分实现 | 已有 ACTIVE→DISABLED、成员暂停、Session 回退和审计；未阻止 Billing 购买、分配和既有 Hold。 |
| ACC-19 | 组织安全逻辑删除 | 部分实现 | 已使用 `DELETED` 状态并保留审计；需在删除前确认余额、有效权益包、未结算 Hold 均为零。 |
| ACC-20 | 逻辑删除组织 | 部分实现 | 已有 DELETED 状态、成员撤销、Session 回退和资料脱敏；未执行 ACC-19 全量预检，重复删除返回未找到而非幂等原结果。 |
| ACC-21 | 创建组织子节点 | 部分实现 | 已有显式目标组织、父节点状态校验、同组织闭包写入和审计；缺稳定幂等键。 |
| ACC-22 | 组织名称和节点名称显式进入编辑态后修改 | 已实现 | 管理 UI 提供启动编辑动作，避免误改。 |
| ACC-23 | 重命名组织节点 | 部分实现 | 已有独立重命名接口、显式编辑 UI、名称校验、节点 ID/层级保持和审计；服务端权限仍是迁移期角色模型。 |
| ACC-24 | 调整组织节点父节点 | 部分实现 | 已有同组织移动、防自身/后代成环、串行事务和版本更新；缺稳定幂等键及独立结构版本契约。 |
| ACC-25 | 删除组织节点 | 部分实现 | 已有叶节点检查和审计，但当前物理删除节点并删除成员/角色关联，未按目标对授予、路由和任务执行阻断后逻辑删除。 |
| ACC-26 | 组织邀请 | 部分实现 | 当前支持电子邮箱和一次性邀请 Token，并有创建、接受、过期/撤销状态；手机号、用户名邀请及目标反枚举恢复契约未实现。 |
| ACC-27 | 添加组织成员 | 部分实现 | 已有用户/组织状态、重复成员、角色边界、授权版本和 Outbox；重复有效成员返回冲突而非幂等原结果。 |
| ACC-28 | 移除组织成员 | 部分实现 | 已有成员撤销、角色清理、授权版本和 Outbox；管理端路径未自动回退活动上下文，也未处理 BILL-13 授予暂停。 |
| ACC-29 | 所有者安全转让 | 部分实现 | 已有专用串行事务维持唯一 OWNER 并更新版本/审计；缺稳定幂等键和目标 Account 权限码。 |
| ACC-30 | 查看角色定义和用户关联角色 | 部分实现 | 可查看组织角色定义、成员和本人角色；缺 Account 平台角色、授权有效期及完整平台角色关联。 |
| ACC-31 | 授予普通组织角色 | 部分实现 | 普通角色接口拒绝 OWNER/平台角色并递增授权版本；缺起止时间、细粒度权限码和稳定幂等键。 |
| ACC-32 | 撤销普通组织角色 | 部分实现 | 角色替换会递增用户/Session 授权版本且 OWNER 走专用路径；缺单绑定幂等撤销、有效期和缓存失效契约。 |
| ACC-33 | 无组织用户不显示组织页签 | 已实现 | 个人中心按组织列表动态隐藏。 |
| ACC-34 | 当前上下文按用户全局保存并跨端同步 | 部分实现 | 已有个人/组织活动上下文和版本字段，但按 OIDC Session 保存，不是用户级全局上下文，也无跨端同步。 |
| ACC-40 | 持久化注销 Saga | 未实现 | 当前没有独立注销聚合、阶段版本、幂等恢复、SLA 和 REPAIR_REQUIRED；现有注销无法证明跨 Identity、Account、Billing 与 Commerce 的崩溃恢复。 |
| ACC-41 | 注销静默期业务冻结 | 未实现 | 当前没有主体生命周期版本及注销修复动作白名单，无法统一禁止新模型、Hold、订单、资料、成员和授权事实。 |
| ACC-42 | 待核对模型用量阻断注销 | 未实现 | 当前注销预检只检查 Hold，未覆盖上游可能已派发、取消不确定、SETTLEMENT_PENDING、REVIEW 及以后仍可能追扣的用量状态。 |
| ACC-43 | 未解决拒付阻断注销 | 未实现 | 当前没有把拒付案件、权益冲销结果和订单级应收作为同一注销门禁。 |
| ACC-44 | 资金性对账差异阻断注销 | 未实现 | 当前对账差异缺少强制 financial_impact 分类及“未分类默认阻断”规则，也没有防止只改标签绕过的复核门禁。 |
| ACC-45 | 外部待确认关单阻断注销 | 未实现 | 当前未把 CLOSE_REQUESTED、LOCALLY_CLOSED_EXTERNAL_PENDING、晚到审核和 PROVIDER_BLIND 纳入注销预检。 |
| ACC-46 | 注销处理 Commerce Mapping 任务 | 未实现 | 当前同步任务缺少资金/资料分类和主体生命周期版本，迟到同步可能在 Mapping DETACHED 或标识复用后回写。 |
| ACC-47 | 注销自动收敛资料修改 Saga | 未实现 | 当前没有区分 Identity 更新前取消与更新后向前收敛，也未证明预约能安全释放且不会产生第二个业务用户。 |
| ACC-48 | 注销自动撤销角色与临时授权 | 未实现 | 当前没有在组织所有权先转让的前提下，由注销协调器统一撤销平台/普通组织角色、未来授权、临时/紧急授权并递增授权版本和清除缓存。 |
| ACC-49 | 注销自动失效未接受邀请 | 未实现 | 当前邀请没有 ACCOUNT_CLOSURE 终态，也未阻止复用手机号/用户名的新账号接受旧邀请。 |
| ACC-50 | 注销标识释放门禁 | 未实现 | 当前没有在 Identity 不可认证、自动清理完成、Mapping DETACHED 和生命周期防迟到写之间建立原子门禁。 |
| ACC-51 | 注册最终唯一约束 | 未实现 | 当前短信前预检可能被当作最终判断，尚未证明正式用户名/手机号映射与所有活动预约由数据库唯一约束共同防并发。 |
| ACC-52 | 注册唯一值分阶段预约 | 未实现 | 当前缺少发送短信前5分钟预约、短信验证后30分钟预约及 Identity 创建后无 TTL 恢复锁和确定释放规则。 |
| ACC-53 | 注册操作稳定幂等 | 未实现 | 当前没有服务端注册操作 ID 贯穿短信发送/核验、Identity、Account 和钱包，也未定义同键异用户名、手机号、挑战或密码冲突。 |
| ACC-54 | Identity 孤儿恢复与补偿 | 未实现 | 当前缺少持久状态、按外部关联查询防重复创建、30分钟向前恢复 SLA、24小时补偿证明及先停用 Identity 后释放预约的顺序。 |
| ACC-55 | 注册操作安全查询 | 未实现 | 当前没有按操作 ID 和签名令牌查询未完成注册的接口，超时后客户端只能收到笼统失败并可能重复注册。 |
| ACC-56 | 内部用户 ID 永久稳定 | 部分实现 | `users.id` 是 UUID 主键并由 UUIDv7 创建，现有领域写路径不修改且逻辑删除保留原值；缺数据库级不可变守卫、跨迁移验证和显式永久不复用契约。 |
| ACC-57 | 独立用户编码 | 未实现 | 当前没有独立于主键的 `user_code` 字段、安全随机生成、格式/校验码、唯一冲突重试或永久编码登记。 |
| ACC-58 | 用户编码展示与精确查询 | 未实现 | 本人资料、桌面/Web 和运营后台均未展示编码，也没有权限受控的完整编码精确查询和注销主体识别。 |

### 3.3 Token Account、路由和模型调用

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| TOK-01 | 本地独立模型不进入账号平台 | 已实现 | 用户在桌面直接配置的本地模型或第三方云模型由本地模型系统管理，不携带账号平台身份、不使用平台钱包和 Token Gateway。 |
| TOK-02 | 桌面聊天携带与个人中心一致的 OIDC 身份 | 已实现 | 启用账号平台后，Rust 运行时以当前 Access Token 调用 Gateway。 |
| TOK-03 | Token Gateway 提供 OpenAI 兼容聊天入口 | 已实现 | `POST /v1/chat/completions` 支持 JSON 和 SSE；桌面原生流式路径使用该入口。 |
| TOK-05 | 模型网关协议适配 | 部分实现 | 已有模型网关调用与 `UsageEvidenceProvider` 抽象；通用字段、数据库和核对流程仍含 New API 专属语义。 |
| TOK-06 | 模型目录自动提供模型 | 未实现 | Gateway 同步模型目录并提供 `CATALOG_VISIBLE` 用户模型列表；上下文资格和请求级预占不作为目录同步字段，桌面不再读取旧模型服务或固定环境变量列表。 |
| TOK-08 | 首次调用自动创建默认 Token Account 和路由 | 已实现 | 平台托管、幂等创建，普通用户不可见不可改。 |
| TOK-09 | 托管路由 Provisioning 生命周期 | 未实现 | 当前缺少按作用域、模型、协议和期望代唯一的 Provisioning 聚合，以及 PENDING/READY/FAILED/REPAIR_REQUIRED、指数退避、SLA 转修复和受审计人工重试。 |
| TOK-10 | 托管模型调用资格顺序闭环 | 未实现 | 当前首次请求可直接触发默认资源创建，但尚未以统一管线强制执行目录可见、Provisioning READY、上下文资格、请求级预占、Hold、上游调用的先后顺序和阶段原因码。 |
| TOK-11 | 请求前解析个人/组织上下文和角色 | 已实现 | Gateway 从 Account 获取服务端可信上下文，不接受客户端伪造 payer。 |
| TOK-13 | 模型和 Provider 路由确定且可审计 | 已实现 | 按作用域、默认级别和优先级选择，歧义拒绝。 |
| TOK-14 | 凭据加密且 UI 不返回明文 | 已实现 | API 只返回类型、状态、指纹、更新时间等元数据。 |
| TOK-15 | 个人中心用户自管 Token Account | 移除 | 不提供个人供应商 Key、Token Account、共享和路由管理；现有相关个人入口及公开能力从目标范围移除。平台/组织运营管理与本地独立模型配置不受此条影响。 |
| TOK-19 | 调用前按具体请求和协议动态预占 | 部分实现 | 已有固定三元组估算和 Hold 预占；缺协议绑定公式、版本化计量单位、Tokenizer/估算器、续租和追加预占。 |
| TOK-22 | 支持并发对话且防超扣 | 已实现 | 每请求独立 Hold；可用余额能够覆盖即可并发。 |
| TOK-24 | 模型请求幂等 | 部分实现 | 显式或稳定派生幂等键可阻止网络重试重复调用上游，但重复请求返回 `USAGE_REPLAY_BLOCKED`，不会按目标返回原执行结果或当前状态。 |
| TOK-25 | 模型请求幂等键作用域 | 未实现 | 当前派生键包含 OIDC Session，无法提供跨会话稳定防重；需改为稳定业务用户、固定模型推理命名空间和键摘要的唯一约束，并保存初次执行快照。 |
| TOK-26 | 同键异请求冲突检测 | 未实现 | 当前缺少带算法版本的规范化请求指纹；同键不同语义请求必须返回 409，且不得创建 Hold、调用上游或覆盖原执行。 |
| TOK-27 | 模型请求幂等记录保留 | 未实现 | 当前未形成非终态至收敛、流事件 24 小时、结果 30 天、防重墓碑 90 天及载荷过期返回 410 的分层保留证据。 |
| TOK-28 | 流式响应持久化与重放 | 未实现 | 当前流式输出未证明在确认客户端前加密持久化供应商中立事件，也没有连续 sequence、完整性校验和 Last-Event-ID/resume token 重放。 |
| TOK-29 | 模型请求断线重连 | 未实现 | 当前缺少“断线不取消”语义及按 STREAMING、终态、SETTLEMENT_PENDING/REVIEW、载荷过期分别恢复的统一响应契约。 |
| TOK-30 | Hold 后上游派发恢复 | 未实现 | 当前缺少从 RECEIVED 到 REVIEW/终态的持久派发状态、Billing EnsureHold 唯一性，以及区分确定未派发和可能已派发后的安全恢复规则。 |
| TOK-31 | 实际用量结算并释放差额 | 部分实现 | 成功且有 usage 时可以提交实际成本并释放剩余 Hold；取消终态、可信累计用量和取消核对路径仍需按 TOK-38–TOK-41、BILL-34–BILL-35 扩展。 |
| TOK-32 | Gateway 自动重试模型请求 | 已实现 | 上游是否执行可能不确定，自动重试会造成重复消费；当前 Gateway 不自动重发模型请求。 |
| TOK-33 | 不确定结果自动核对和人工重试 | 部分实现 | `REVIEW`/`SETTLEMENT_PENDING` 后通过 `UsageEvidenceProvider` 查询证据，24 小时内退避重试并支持管理员重试；协议仍绑定 New API 且完整收敛语义不足。 |
| TOK-34 | 普通用户查看用量历史 | 已实现 | 提供个人用量请求列表和详情，不暴露内部凭据。 |
| TOK-37 | 非计费客户端切换 Readiness | 未实现 | 当前没有覆盖兼容版本、OIDC、Account/Gateway 契约、目录、READY 路由和计价完整性的无副作用 readiness，也没有与真实用户钱包/账单隔离的专用合成验收身份或供桌面安全验证的短时签名准入证明。 |
| TOK-38 | 用户取消模型请求 | 未实现 | 缺少按原请求所有者授权、持久化取消意图、派发前终止、完成竞态和幂等重复取消的统一命令。 |
| TOK-39 | Provider 取消能力适配 | 未实现 | 当前 Model Gateway Adapter 未统一声明 EXPLICIT_CANCEL、STREAM_ABORT_CONFIRMED_CANCEL、UNSUPPORTED，也未证明取消重试幂等和最终用量获取能力。 |
| TOK-40 | 传输连接与模型执行状态隔离 | 未实现 | 缺少 execution/cancel/delivery/settlement 四类独立状态；断线、应用退出和停止显示尚未形成“不取消、不判失败、不释放 Hold”的可验证契约。 |
| TOK-41 | 取消结果和实际用量核对 | 未实现 | 缺少取消时间线、Provider 证据、取消不确定状态退避查询、最终用量核对和超时转 REVIEW 的闭环。 |
| TOK-42 | 模型目录管理生命周期 | 未实现 | 当前目录同步与展示状态未形成 DRAFT/PUBLISHED/DEPRECATED/REMOVED 管理状态机，也没有合法迁移、ETag/版本并发控制、幂等和审计。 |
| TOK-43 | 已移除模型的历史展示 | 未实现 | 当前未证明 Model Gateway 删除模型后仍能以不可变目录快照展示历史会话，也没有上游缺失连续核验、墓碑和历史数据保留边界。 |
| TOK-44 | 模型重命名和别名 | 未实现 | 当前缺少稳定逻辑模型 ID、版本化显示名称、别名有效期、唯一解析和冲突/循环校验。 |
| TOK-45 | 模型目录版本回滚 | 未实现 | 当前缺少复制历史版本形成新版本、按当前协议/价格/路由/凭据重新校验、原子切换和禁止复活 REMOVED 模型的回滚流程。 |
| TOK-46 | 模型关联对象版本生成边界 | 未实现 | 当前没有证明目录、协议、能力、价格和路由代次独立演进，也没有冻结全部版本引用的请求快照；配置变化可能无法与新逻辑模型身份变化区分。 |
| TOK-47 | 模型下架后的会话处理 | 未实现 | 当前缺少新会话与历史继续的独立截止时间、下架后显式替代分支、旧缓存拒绝及生效点前后在途请求/紧急取消的确定规则。 |

### 3.4 钱包、积分、定价和权益

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| BILL-01 | 个人和组织钱包独立 | 已实现 | 钱包按 Owner 类型和 ID 唯一；组织请求不能隐式扣个人钱包。 |
| BILL-02 | 余额以不可变平衡账本为准 | 已实现 | Journal Entry 与 Posting 平衡，不能直接覆盖余额。 |
| BILL-03 | 积分使用整数最小单位 | 已实现 | `10,000 microcredits = 1 展示积分`；金额使用分。 |
| BILL-06 | 充值形成有有效期的权益包 | 部分实现 | 当前持久化支付来源、起止时间和聚合剩余额度；商品虽有基础/赠送字段，权益包未保存 BASE/BONUS 组成，来源绑定订单而非成功支付流水。 |
| BILL-07 | 组织权益在线购买入账 | 未实现 | 企业管理员购买时明确受益组织；支付成功幂等创建组织权益包和组织钱包分录，付款人个人钱包不参与。 |
| BILL-08 | 组织成员额度手动分配 | 未实现 | 企业管理员从组织有效权益中给成员分配，员工无领取接口；分配、撤回、占用和消费均审计。 |
| BILL-14 | 基础积分与赠送积分分项核算展示 | 未实现 | 权益包增加基础/赠送组成及各自剩余、预占、已用；按到期顺序分包返回。 |
| BILL-15 | 消费按最早到期权益包优先 | 已实现 | Hold 分配与结算关联具体权益包。 |
| BILL-17 | 人工赠送积分有效期 | 未实现 | 人工赠送积分必须形成独立权益组成，自入账起 30 天到期，不允许创建无期限人工赠送余额。 |
| BILL-18 | 权益到期处理 | 部分实现 | 已有到期任务关闭权益包和账本处理；缺 BASE/BONUS 组成及与不确定 Hold 的完整目标规则。 |
| BILL-19 | 过期不确定 Hold | 未实现 | 权益到期时直接释放并移除仍不确定的 Hold，不恢复已过期可用额度；之后确认的上游用量进入人工损失复核，不重新扣减其他权益。 |
| BILL-21 | 定价按模型和版本管理 | 已实现 | 当前与历史价格并存，结算/核对使用对应时点价格。 |
| BILL-22 | 全局统一价格和分时段计价 | 未实现 | 支持全局默认和模型覆盖；每个策略可为统一价格或按 `Asia/Shanghai` 时间段计价，区间不重叠且完整可预览。 |
| BILL-23 | 价格只配置协议公式引用的计量单位 | 未实现 | 当前未支持“全局单位必须固定价、模型单位可固定价或显式继承”的完整配置项约束；运行时也未证明只使用公式引用单位和不可变绑定。 |
| BILL-24 | 输入与缓存输入互斥计量 | 未实现 | Adapter 需把 Provider 输入事实规范化为互斥的非缓存输入和缓存输入；当前缺少跨 Provider 一致约束及矛盾证据核对。 |
| BILL-25 | 零价、显式继承与缺失分离 | 未实现 | 需要允许并快照固定零价、模型级显式继承并拒绝缺失配置；当前不能证明零价阻止回退，且缺失必须返回 PRICE_MISSING。 |
| BILL-26 | 人工积分调整需审批 | 已实现 | 申请、批准、拒绝和账本入账可审计。 |
| BILL-27 | 管理员查看钱包账本详情 | 部分实现 | 已有钱包、分录、Hold、调整和对账查询；应收、组成级权益及完整授权/导出保护未实现。 |
| BILL-28 | 退款权益与账本冲销 | 未实现 | 普通退款成功时冲销目标支付流水与权益包组合的全部未消费 BASE、该支付产生的全部未消费关联 BONUS 及对应账本负债；现金退款只按 BASE 计算，BONUS 零折现；人工赠送和其他支付来源权益不受影响。 |
| BILL-33 | 订单级拒付冲销隔离 | 未实现 | 拒付只撤销原支付流水对应权益包的可撤销 BASE 和关联 BONUS；不足现金本金形成订单级应收，不冲销其他权益包，应收未清时冻结对应钱包上下文。 |
| BILL-34 | 模型计费使用可信实际生成量 | 未实现 | 当前未区分 Provider 实际生成量、Gateway 持久化序号和客户端确认序号，也没有平台不可恢复交付故障转损失复核的计费边界。 |
| BILL-35 | 取消期间 Hold 生命周期 | 未实现 | 缺少派发前取消全释放、派发后继续续租、Provider 不支持取消时自然收敛、版本化核对期限及晚到用量不得重新扣用户的规则。 |
| BILL-36 | 权益组成资金守恒 | 未实现 | 当前没有按权益包与 BASE/BONUS 组成核验“可分配总量＝可用＋有效授予未消费＋暂停授予未消费＋Hold”，也未证明四桶互斥。 |
| BILL-37 | 权益组成禁止超额授权 | 未实现 | 当前缺少只允许从同包同组成可用余额创建授予的数据库强约束，暂停授予和 Hold 可能无法证明不会被重复分配。 |
| BILL-38 | 并发分配锁与幂等边界 | 未实现 | 当前未证明分配、撤回、Hold、结算、到期和退款共用组成级锁/CAS；缺少同键异请求冲突和多管理员竞争余额的并发测试。 |
| BILL-39 | 注销授予隔离至到期 | 未实现 | 当前没有区分普通成员移除的可恢复暂停与账号注销的 `ACCOUNT_CLOSED` 隔离暂停，也没有强制不自动回池、不可恢复/普通撤回、未回收部分到期失效及新用户 ID 不继承。 |
| BILL-40 | 注销授予可选全额回收 | 未实现 | 当前没有账号 `ANONYMIZED` 后的专用授权回收命令，也没有对消费、Hold、待核对用量、资金冻结、全额边界、原组成守恒、幂等、批量部分成功与完整审计的强制校验。 |
| BILL-41 | 模型覆盖价优先匹配 | 未实现 | 当前缺少按请求时间和价格作用域唯一选择模型覆盖、无覆盖才选全局的确定匹配器，也未统一 PRICE_MISSING/PRICE_AMBIGUOUS。 |
| BILL-42 | 计量单位显式继承解析 | 未实现 | 当前没有逐单位 FIXED_PRICE/INHERIT_GLOBAL 数据模型、同一时刻全局来源解析、固定零价阻断回退及最终来源快照。 |
| BILL-43 | 未来价格区间冲突拒绝 | 未实现 | 当前缺少同定价空间 PUBLISHED/ACTIVE 区间排斥约束、发布事务锁/CAS、ETag、幂等指纹和并发发布测试。 |
| BILL-44 | 定价版本生命周期与撤销 | 未实现 | 当前未形成 DRAFT/PUBLISHED/ACTIVE/ENDED/CANCELLED 状态机，也没有生效前取消、ACTIVE 非追溯提前结束及接续/显式全局/阻断模式。 |
| BILL-45 | 已创建 Hold 价格不可变 | 未实现 | 当前虽保存部分价格版本，但未证明逐单位最终价格、继承来源、分母和舍入完整冻结，也未覆盖撤销、结束、替代、回滚后的续租、追加预占与核对。 |

### 3.5 充值、支付 Provider 与新华三商城

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| PAY-01 | 支付实现位于独立 Adapter | 部分实现 | H3C Mall 与 Simulator 分别实现通用 `PaymentProvider`；仍需用合同和架构门禁证明业务状态机不依赖具体 Provider。 |
| PAY-02 | 渠道和动作由能力声明驱动 | 部分实现 | 已有 Provider capabilities 并驱动部分 API/UI；尚未覆盖历史保留、晚到窗口、权威终态和完整能力快照，退款不支持时 UI 仍显示禁用入口。 |
| PAY-03 | 多支付渠道 | 未实现 | 当前生产候选流程仅支持新华三支付宝；开发/测试 Payment Provider 的银联、微信能力不属于生产能力。 |
| PAY-04 | 多 Provider 商品映射 | 未实现 | 通用充值商品与外部 SKU 分表映射，至少包含 Provider、外部 SKU、金额校验、状态和有效期；历史订单固定创建时的映射版本。 |
| PAY-05 | 充值商品正式冻结 | 未实现 | 当前 SKU/金额/权益为 UAT 暂定，正式收款前必须冻结。 |
| PAY-06 | 个人用户与当前 Commerce Provider 客户一一对应 | 部分实现 | 当前通过同一手机号异步确保新华三商城账号；尚未抽象供应商中立 Commerce Mapping。 |
| PAY-07 | 充值全程留在品悟应用内 | 已实现 | 用户图标旁额度入口进入商品选择、二维码和结果页，不跳商城后台网页。 |
| PAY-08 | 每次点击充值先进入商品选择 | 已实现 | 不恢复到上次二维码页。 |
| PAY-09 | 服务端按 product_id 派生 SKU、金额和积分 | 已实现 | 客户端不能覆盖金额、SKU 或权益。 |
| PAY-10 | 二维码和支付金额完全采用商城返回事实 | 已实现 | H3C Provider 不本地生成二维码。 |
| PAY-11 | 支付流水与权益包履约隔离 | 未实现 | 需明确 payment_order 1:N payment_transaction，并为支付来源权益包建立唯一 source_payment_transaction_id；多笔成功流水不得共享权益包，逐笔独立履约或进入重复/晚到支付复核。 |
| PAY-13 | 支付领域独立状态机与订单派生摘要 | 未实现 | 现有单一订单状态链仍需拆成订单关闭流程、支付流水、退款和拒付状态机；订单摘要需改为可重建投影，禁止作为资金事实源。 |
| PAY-15 | 支付回调与主动查询 | 降级实现 | Simulator 有签名回调；新华三不支持回调，当前以 Billing 主动查询收敛。尚未形成回调、查询、账单统一不可变证据归并。 |
| PAY-16 | 客户端关闭后订单仍继续收敛 | 已实现 | 查询任务运行在 Billing 服务端。 |
| PAY-17 | 超时订单不永久停留普通 PENDING | 部分实现 | 本地过期和部分异常修复已实现；尚未按 PAY-37 将72小时未收敛订单确定性转换为具有责任人、SLA 和下次复核时间的人工审核案件。 |
| PAY-18 | 主动关闭未支付订单 | 降级实现 | 用户或系统可以关闭未支付订单；Provider 支持时调用外部关单并核验，Provider 不支持时降级为停止本地支付展示并持续查询，只有外部事实明确后才收敛最终状态。 |
| PAY-19 | 主动查询退避与72小时边界 | 未实现 | 当前查询和过期处理尚未证明在72小时执行最后查询、停止高频任务并幂等转入 REVIEW_REQUIRED_LATE_PAYMENT。 |
| PAY-21 | 支付后自动更新额度 | 部分实现 | 当前真实 Provider 通过服务端主动查询确认支付后，幂等创建聚合权益包和账本入账；回调并非新华三的主要路径，目标组成级权益和多流水履约仍缺失。 |
| PAY-22 | 企业管理员在线购买组织权益 | 未实现 | 有 `org.entitlements.purchase` 权限的企业管理员在组织上下文选择组织商品并支付；订单保存付款用户与受益组织，到账后只增加组织权益。 |
| PAY-24 | 退款目标绑定支付流水和权益包 | 未实现 | 退款请求和执行命令需同时保存并校验 payment_transaction_id、entitlement_package_id；payment_order_id 仅作归属和审计，不能独立执行退款。 |
| PAY-26 | 退还目标全部剩余可退金额 | 未实现 | 不接受退款金额输入；现金退款只按目标支付流水实收、退款预约/确认、权益包全部未消费 BASE 和 Provider 事实核对全部剩余金额，关联 BONUS 零折现但在成功时全部撤销，主动退款状态机无 PARTIALLY_REFUNDED。 |
| PAY-27 | 退款意图原子事务 | 未实现 | 需增加稳定锁顺序、权益冻结竞争控制、退款金额预约、refund_request 与 Outbox 原子提交，以及提交后 Provider 幂等调用和崩溃恢复；Provider 不保证幂等时不确定结果不得自动重发。 |
| PAY-30 | 退款同步冲销 | 未实现 | Provider 确认普通退款后，在同一 Billing 事务中消费现金退款预约、冲销目标支付流水与权益包组合的全部未消费 BASE 及该支付产生的全部未消费关联 BONUS、更新退款状态并写平衡账本；人工赠送、其他支付流水和无来源关系 BONUS 不受影响。 |
| PAY-32 | 渠道部分强制退款异常 | 未实现 | Provider 主动产生部分强制退款时将既成外部资金事实建立为独立异常并进入人工复核；不适用 PAY-25 普通退款资格预检、不复用普通退款且不覆盖支付成功，复核前冻结原权益包，无法撤销的已退本金形成原订单应收。 |
| PAY-33 | 支付异常人工复核 | 未实现 | 拒付、渠道部分强制退款、重复支付、晚到支付、退款失败和状态冲突进入统一复核队列，同时保持各领域状态独立；拒付绑定原支付流水、订单和权益包，按 BILL-33 撤销关联权益并为不足现金本金登记订单级应收，保留证据、责任人、结论和审计。 |
| PAY-34 | 每日四方账单对账 | 未实现 | 每日获取 Provider 账单，对订单、支付流水、退款和 Billing 账本入账进行四方核对，生成可追踪差异。 |
| PAY-35 | 账单能力降级 | 未实现 | 当前缺少无账单 Provider 的受控文件/查询降级证据，也未对同时无回调的 Provider 执行 PAY-38 准入和 PAY-37 人工审核。 |
| PAY-36 | 对账差异闭环 | 未实现 | 每条差异必须包含分类、严重度、状态、责任人、修复动作、复核结果和审计；修复通过幂等命令写入，不允许直接改表。 |
| PAY-37 | 72小时未收敛订单转人工审核 | 未实现 | 缺少边界最后查询、唯一审核案件、前7天每日/此后每周的人工复核 SLA、逾期告警和晚到支付幂等履约。 |
| PAY-38 | 无回调无账单 Provider 正式收款准入 | 未实现 | 缺少历史订单查询保留期、有限支付/确认窗口、权威终态及能力快照校验，现有 Provider 可在这些条件未证明时被配置用于收款。 |
| PAY-39 | 未支付订单最终归档 | 未实现 | 缺少 final_archive_eligible_at、期限后成功查询、权威未支付终态、冲突证据和用户申诉组成的原子归档门禁。 |
| PAY-40 | Provider 事实盲区处理 | 未实现 | 查询失效、历史丢失、无限 PENDING 或能力违约尚未形成 PROVIDER_BLIND 案件、升级和停用新订单规则。 |

### 3.6 个人中心与运营后台

| target_requirement_id | 需求 | 状态 | 当前实现/边界 |
|---|---|---|---|
| UI-01 | 桌面用户入口常显，未登录在弹层内登录 | 已实现 | 不跳外部浏览器；个人信息也在弹层中展示。 |
| UI-03 | 个人中心提供概览、额度、用量和组织 | 部分实现 | 组织页签按成员关系隐藏；当前身份模块只放在概览；移除普通用户 Token Account/供应商 Key 管理入口。 |
| UI-04 | 普通用户界面不暴露内部结算/供应商运维信息 | 已实现 | 模型准备、内部结算状态、供应商凭据等从普通用户视图移除。 |
| UI-05 | 账号资料与密码恢复 | 未实现 | 增加用户名、手机号修改和忘记密码入口及完整安全校验。 |
| UI-06 | 个人中心只展示活动上下文额度 | 部分实现 | 个人上下文显示个人权益；组织上下文只显示当前成员被授予的组织额度，不同时展示其他上下文余额。 |
| UI-07 | 跨端活动上下文同步 | 未实现 | 桌面与 Web 展示同一个服务端活动上下文；收到更高上下文版本、窗口重新聚焦或切换成功时清空旧额度/权限缓存并刷新。 |
| UI-08 | 权益包、到期与退款说明 | 部分实现 | 需展示总可用积分、每个包的 BASE/BONUS 余额、来源、到期时间、消耗顺序、即将到期提醒，以及 BONUS 零现金退款但普通退款成功时关联未消费 BONUS 随 BASE 撤销。 |
| UI-09 | 桌面跳转 Web 个人中心 | 未实现 | 登录后的账号 Popover 增加入口按钮；使用专用 Tauri 命令打开受信 Portal URL，不复用任意外链接口。 |
| UI-10 | Web 个人充值入口 | 已实现 | Web 个人中心已提供个人权益充值入口；后续重构不得删除。 |
| UI-11 | Web 组织购买入口 | 未实现 | 具有组织采购权限的企业管理员可在显式目标组织管理页购买组织权益。 |
| UI-12 | 支付能力降级展示 | 未实现 | 关单、退款、回调状态和账单能力按 Provider capabilities 展示；不支持退款时完全隐藏入口，不展示不可执行按钮诱导用户。 |
| UI-13 | 模型停止与结算状态反馈 | 未实现 | 当前缺少停止接收、取消请求已受理、Provider 不支持取消、后台结束处理中、待核对和已结算的区分，也未提示取消生效前按实际用量计费。 |
| UI-14 | 注销进度与阻断原因展示 | 未实现 | 当前没有持久注销状态页、脱敏阻断分类、用户恢复动作或“自动处理中”与“需要本人处理”的区分。 |
| UI-15 | 未完成注册恢复状态展示 | 未实现 | 当前没有保存注册操作查询凭据、按 next_retry_at 恢复轮询或区分 Identity 已创建、Account 提交中、修复中和已补偿。 |
| UI-16 | 桌面与 Web 三语完整性 | 部分实现 | 当前桌面、Portal 和管理后台路由的简中/英文/日文词条测试通过；仍需覆盖全部目标页面、错误码以及发布制品级缺词条、回退和单语硬编码门禁。 |
| ADM-01 | 首个超级管理员安全初始化 | 未实现 | 当前目标已禁止8位初始密码；尚缺 BOOTSTRAP_PENDING 权限门禁、交互式无回显直接设密，以及默认256 bit/最少128 bit的一次性引导凭据、Base64URL字符集、15分钟有效期、5次失败锁定、一次读取销毁、原子兑换和重新签发代次隔离。 |
| ADM-02 | Break-glass 恢复 | 未实现 | 仅在凭据丢失、意外停用或身份故障等异常导致全部超级管理员无法登录时，通过主机/集群受控离线命令创建短时紧急授权或恢复账号；禁止公网 API、禁止绕过最后一个有效超级管理员主动注销保护或替代计划内交接，执行必须告警并写不可删除审计。 |
| ADM-03 | 服务身份权限 | 未实现 | 服务账号、后台任务和 Event Relay 使用独立工作负载身份、短期凭据和最小权限，不继承人类平台角色；审计记录 `actor_type=SERVICE`、服务名和任务运行 ID。 |
| ADM-04 | 用户与组织分为独立页签 | 已实现 | 支持列表、搜索、详情和生命周期操作。 |
| ADM-05 | 角色与成员权限管理 | 部分实现 | 已有组织成员、固定角色和所有权管理；缺 Account 平台 RBAC、授权有效期和目标权限码体系。 |
| ADM-06 | 角色授权自动回收 | 未实现 | 角色绑定支持开始/到期时间；临时和紧急授权到期自动失效、撤销缓存并记录审计。 |
| ADM-07 | 审批角色兼任和交叉审批 | 未实现 | 允许运营/审批角色兼任和不同用户交叉审批，但禁止自审；ADM-37 强制审批命令只需一名独立审批人。 |
| ADM-08 | Keycloak 安全管理入口 | 已实现 | 不嵌入普通后台；通过受限网络/隧道访问。 |
| ADM-09 | 审计筛选、详情和导出 | 部分实现 | 已有筛选和导出基础；需按 NFR-48 增加近期重新认证、默认脱敏、PII临时权限、水印、加密、一次性下载和导出配额。 |
| ADM-10 | 系统运行状态 | 部分实现 | 展示实际部署的 Account、Billing、Gateway、Portal、身份 Provider、模型网关、事件基础设施和 Relay；目标改为配置驱动探针，不在 Portal 写厂商判断分支。 |
| ADM-11 | 组织模型业务策略管理边界 | 未实现 | 组织后台仅管理显式目标组织的模型允许策略和目录范围；不得返回或接受真实 Token Account、路由、凭据或 Provisioning 字段，Gateway 将业务策略与平台运营维护的 READY 路由共同计算资格。 |
| ADM-12 | 计量单位定义管理 | 未实现 | 需增加版本化单位定义、停用和历史引用保护。 |
| ADM-13 | 价格版本管理 | 部分实现 | 已有模型价格版本；缺少全局/分时、固定价/显式继承、逐单位来源预览、生命周期、区间冲突、生效前撤销、ACTIVE 提前结束接续模式和既有 Hold 保护。 |
| ADM-14 | 钱包状态查询 | 已实现 | 已有钱包详情；生产授权过滤和部署行为仍未验证。 |
| ADM-15 | 钱包账本详情查询 | 已实现 | 已有 Journal/Posting 查询，数据库约束保护已入账分录不可变。 |
| ADM-16 | 应收管理 | 未实现 | 需增加独立 receivable、清偿分配和上下文冻结运营视图。 |
| ADM-17 | 人工积分调整 | 部分实现 | 有人工调整申请、审批和平衡入账；尚无统一审批命令框架。 |
| ADM-18 | Hold 运营 | 部分实现 | 有 Hold 查询/释放；缺目标异常状态与组成级证据。 |
| ADM-19 | 支付订单运营 | 部分实现 | 有订单查询、同步和修复；目标多流水与完整支付状态机缺失。 |
| ADM-20 | 退款运营 | 未实现 | 普通退款意图、BASE/关联 BONUS 累计消费资格预检、只按全部未消费 BASE 计算整笔现金退款、成功后同步撤销关联 BONUS 及 Provider 能力降级待实现。 |
| ADM-21 | Billing 对账管理 | 部分实现 | 已有内部对账基础；四方对账、责任人和 Provider 账单降级待实现。 |
| ADM-22 | Billing 修复执行 | 部分实现 | 已有部分修复动作；需统一幂等命令、证据、前后状态、账本影响和审批审计。 |
| ADM-23 | 模型协议定义管理 | 未实现 | 缺少独立版本化协议定义管理。 |
| ADM-24 | 协议预占公式版本管理 | 未实现 | 缺少公式校验、预览、发布和不可变历史。 |
| ADM-25 | Token Account 生命周期管理 | 部分实现 | 可管理平台 Token Account 生命周期；目标平台业务 RBAC 未实现。 |
| ADM-26 | 凭据元数据查询 | 部分实现 | 可查询脱敏凭据元数据；目标细分角色隔离未实现。 |
| ADM-27 | 凭据有效性检查 | 部分实现 | 有凭据检查动作；目标细分角色隔离和统一错误契约待补。 |
| ADM-28 | 模型路由管理 | 部分实现 | 有路由 CRUD 和确定性选择；模型目录/Provisioning 代次缺失。 |
| ADM-29 | Provisioning 状态查询 | 未实现 | 缺少代次、尝试、退避、SLA 和脱敏错误的独立视图。 |
| ADM-30 | Provisioning 修复 | 未实现 | 缺少只能推进 PENDING、由协调器验证 READY 的受审计修复命令。 |
| ADM-31 | 用量请求与证据查询 | 部分实现 | 可查询用量请求和上游引用；缺流事件、请求指纹和完整证据。 |
| ADM-32 | 用量核对人工重试 | 部分实现 | 有用量核对人工重试且不重发模型请求；完整幂等收敛仍待目标实现。 |
| ADM-33 | 晚到支付人工审核管理 | 未实现 | 缺少案件筛选、责任人认领、受审计 Provider 查询、next_review_at、逾期升级、证据归档和 PROVIDER_BLIND 处置入口。 |
| ADM-34 | 注销 Saga 修复 | 未实现 | 当前没有按阶段、阻断类型、SLA 和责任人管理 REPAIR_REQUIRED 注销任务的页面或受限重试命令，也没有禁止强制完成和直接释放标识的权限边界。 |
| ADM-35 | 注册 Saga 修复 | 未实现 | 当前没有注册操作修复页、Identity 关联查询和 Account 幂等重试，也没有24小时及全量无业务事实证明约束的补偿命令。 |
| ADM-36 | 管理命令审批分类注册 | 未实现 | 当前审批逻辑按具体功能零散存在，尚无以 command_type 为键的版本化 APPROVAL_REQUIRED/AUDIT_ONLY 注册表、覆盖检查和未登记失败关闭。 |
| ADM-37 | 强制审批命令清单 | 未实现 | 当前只有人工积分调整的局部审批，尚未统一覆盖钱包、权益、账本、应收及支付/退款/拒付/对账/用量的人工财务修复。 |
| ADM-38 | 仅审计命令清单 | 未实现 | 当前未集中声明正常退款、组织删除、治理配置、确定性自动处理等无需审批的命令，也未禁止其参数化切换为异常改账路径。 |
| ADM-39 | 审批请求生命周期 | 未实现 | 当前缺少完整持久状态机、24小时默认申请期限、5分钟执行窗口、撤回终态、新申请 supersedes 关联和并发转换控制。 |
| ADM-40 | 审批快照与资源变更失效 | 未实现 | 当前缺少规范化命令指纹、关键资源/财务/策略快照和结构化失效原因，也未区分增量调整与依赖精确状态的修复。 |
| ADM-41 | 执行前权限重新校验 | 未实现 | 当前未证明执行前绕过缓存复核申请人与审批人的用户/角色绑定/授权版本，也未定义授权不可用、撤销或到期时失败关闭。 |
| ADM-42 | 已批准命令安全执行 | 未实现 | 当前缺少审批 Outbox、受限服务执行身份、稳定执行幂等键、一次领域效果及命令变化后强制重新申请。 |

## 4. 非功能需求

本节展开原审计的实现与目标差距；规范历史状态及当前校正均见第 0 节。NFR-14 的旧厂商绑定判断已由 A06 通用 HTTP/可选商城装配修复，NFR-06 的注册恢复、NFR-09 的迁移实测也须使用对应的新输入证据，而非下表旧结论。

| target_requirement_id | 需求 | 当前实现/边界 |
|---|---|---|
| NFR-01 | 安全传输 | 反向代理、Compose/Helm 和配置校验要求远程 HTTPS、开发 HTTP 仅限 loopback；无实际部署与证书验收证据。 |
| NFR-02 | 最小暴露 | Compose/Helm/反向代理模板将数据库、Redis、NATS、Relay、内部 API、New API 管理面和 Keycloak Admin 保持非公开；部署证据仍为 `NOT_DEPLOYED`。 |
| NFR-03 | 密钥管理 | 仓库无明文生产密钥且 Provider 凭据加密保存；生产 KMS/Vault、密钥轮换和部署证明未实现。 |
| NFR-04 | 日志敏感信息保护 | 错误响应和多数日志避免直接输出秘密，合同测试扫描配置；缺统一字段白名单、日志/Trace 正文脱敏和自动秘密扫描门禁。 |
| NFR-05 | 审计 | 账号、资金、Token 和部分修复动作已有审计；注册/注销 Saga、审批、Provider 事实修复等目标动作缺失。 |
| NFR-06 | 幂等 | 钱包、支付入账、Hold、默认路由、结算和事件消费有幂等基础；注册操作、结果重放、注销和多项修复缺完整指纹契约。 |
| NFR-07 | 最终一致性 | 三服务具备事务 Outbox/Inbox、至少一次 Relay、死信和人工重放；业务最终收敛依赖死信处置且多项 Saga 缺失。 |
| NFR-08 | 全局上下文一致性 | 活动上下文按 OIDC Session 保存，未建立用户级全局 `context_version`、期望版本写入和跨端缓存失效。 |
| NFR-09 | 数据迁移 | 迁移按服务版本化且 up/down 成对并受合同检查；缺已执行迁移不可修改门禁及环境级迁移前后证明。 |
| NFR-10 | 可恢复性 | 已有备份恢复、切换回滚 runbook 和版本目录约定；无实际备份、恢复演练和部署验收证据。 |
| NFR-11 | 高可用 | 当前单机 Compose 不具备高可用；Helm 仅为未部署模板，不能证明多副本或故障转移。 |
| NFR-12 | 工作负载身份 | 内部服务主要共享长期 `INTERNAL_SERVICE_TOKEN`，未使用逐工作负载短期身份、mTLS/OIDC 和 Subject 级授权。 |
| NFR-13 | 个人信息去标识化与保留 | 管理员删除可清理部分资料和 Session；未完成 Identity 不可认证、用户名/手机号安全释放、三年分类保留及自动清除。 |
| NFR-14 | 短信供应商可替换性 | 已有 `SMSProvider` 接口和厂商错误映射基础；启动路径及挑战模型仍绑定阿里云托管核验模式。 |
| NFR-15 | 身份与授权分离 | OIDC 负责主体、Account 负责组织权限；平台后台仍直接依赖 Keycloak `platform-admin` Realm Role。 |
| NFR-16 | Commerce Provider 可替换性 | 新华三客户端已隔离在 Adapter，但 Account/Billing 数据模型仍含 `mall_*`、单一 SKU 和厂商同步语义。 |
| NFR-17 | 支付能力可发现 | 已有 `PaymentProvider capabilities` 并驱动部分操作；缺历史保留、权威终态、晚到窗口、账单及统一 UI/API/任务快照。 |
| NFR-18 | 模型网关可替换性 | 已有模型网关调用和 `UsageEvidenceProvider` 抽象；数据库、字段和核对流程仍含 New API 专属语义。 |
| NFR-19 | 运维探针可扩展 | 已有系统状态聚合和探针函数；组件列表及 New API/Relay 处理仍在 Portal 主流程硬编码。 |
| NFR-20 | 无旧模型技术债 | 桌面运行时旧 `llmapi_hub` 路径已移除并有迁移测试；发布、安装、运行三层门禁及非计费 readiness 尚不完整。 |
| NFR-21 | 结构化日志 | Event Relay 使用 JSON 结构化日志，部分后台任务使用 `slog`；尚无所有服务统一 schema、必需字段和发布门禁。 |
| NFR-22 | Request ID 关联 | HTTP 中间件生成并返回 Request ID，审计和部分下游调用可关联；尚未证明所有入口、事件、错误和后台任务端到端传播。 |
| NFR-23 | 分布式 Trace | HTTP 客户端传播 W3C `traceparent`，Outbox 记录 `trace_id`，并有 OTLP 配置测试；NATS 消费和全部后台任务的跨信号传播不完整。 |
| NFR-24 | 健康检查 | 共享 HTTP Server 提供 live/ready 并覆盖依赖失败测试，Relay 提供 live；尚未逐工作负载统一 readiness 语义。 |
| NFR-25 | 独立指标 | 业务 HTTP Server 和 Event Relay 提供独立 metrics 监听及低基数测试；目标核心指标覆盖和所有工作负载隔离仍不完整。 |
| NFR-26 | 性能与容量验收基线 | 未实现：当前没有冻结的生产等价资源、基准数据集、样本量、负载脚本和平台/Provider/排队分段计时报告。 |
| NFR-27 | 关键内部 API 延迟 | 未实现：尚无读接口 P95≤300ms/P99≤1s、事务写 P95≤500ms/P99≤1.5s及错误率≤0.1%的持续证据。 |
| NFR-28 | 外部依赖接口延迟 | 未实现：登录、短信和支付尚未分别采集平台附加耗时与Provider端到端 P95/P99。 |
| NFR-29 | 流式首个内容事件 | 未实现：当前未统一排除Header/心跳的TTFT口径，也没有平台派发和模型路由首包SLO。 |
| NFR-30 | 模型并发容量 | 未实现：尚未证明最小生产部署单元承载200并发流式请求或在容量上限前公平拒绝且不创建Hold。 |
| NFR-31 | Hold 容量 | 未实现：尚无100,000有效Hold、200次/秒稳态、500次/秒突发和组成守恒/锁等待压测。 |
| NFR-32 | 订单容量 | 未实现：尚无百万订单、三百万流水和50次/秒下单、200次/秒证据归并压测。 |
| NFR-33 | 事件积压容量 | 未实现：尚无每关键流百万待消费事件保留、十万积压15分钟排空和高水位门禁证明。 |
| NFR-34 | 分级 RPO | 未实现：现有单机部署未证明资金事实1分钟及账号/策略5分钟恢复点。 |
| NFR-35 | 分级 RTO | 未实现：现有部署未证明核心服务60分钟及非关键能力4小时恢复。 |
| NFR-36 | 备份频率与加密 | 未实现：需配置持续日志、日增量、周全量、发布前快照、TLS/静态加密、KMS分离和异地不可变副本。 |
| NFR-37 | 恢复演练 | 未实现：需建立月度抽样、季度端到端、年度整机/可用区丢失演练和30天整改复测记录。 |
| NFR-38 | 分类保留与清除 | 未实现：当前缺少统一策略及自动清除证明；不得仅凭数据库仍存在历史数据视为满足三年保留。 |
| NFR-39 | 登录多维限流 | 未实现：需增加IP、设备、账号标识、稳定用户和Session维度及失败渐进延迟/临时锁定。 |
| NFR-40 | 短信多维限流 | 部分实现、待扩展：现有手机号频率限制需补日上限、IP、设备、用途、Provider配额和统一retry_after。 |
| NFR-41 | 支付多维限流 | 未实现：需增加付款人、受益组织、IP、订单、查询/退款动作及合法回调独立容量控制。 |
| NFR-42 | 模型调用多维限流 | 未实现：需增加用户/上下文/组织/模型/路由速率和并发配额、Hold前拒绝及重连不重复计数。 |
| NFR-43 | 租户隔离验证矩阵 | 未实现：当前没有覆盖直接对象引用、批量、搜索、导出、缓存和异步事件的跨租户发布门禁测试。 |
| NFR-44 | Provider 熔断 | 未实现：各Adapter尚未统一60秒窗口、最少样本、50%失败/连续5次阈值、半开探测及高风险不重发规则。 |
| NFR-45 | 人工队列 SLA | 未实现：现有人工复核状态缺少P0/P1/P2统一时钟、认领/响应/止损/解决期限及超时自动升级。 |
| NFR-46 | 统一可信时钟 | 未实现：当前未证明三独立授时源、偏差监测、readiness门禁及数据库/单调时钟职责分离。 |
| NFR-47 | 桌面 API 兼容与淘汰 | 未实现：当前缺少N/N-1/N-2合同测试、每版90天支持、弃用通知字段及426升级闭环。 |
| NFR-48 | 审计导出保护 | 未实现：现有导出需补近期重认证、默认脱敏、PII独立权限、水印、文件加密、一次性下载和任务配额。 |
| NFR-49 | 模型结果重放载荷最小化 | 未实现：当前缺少流事件24小时、完整/聚合结果30天、防重墓碑90天的自动清除，以及注销、案件保全、备份生命周期和密码学销毁证明。 |
| NFR-50 | 用户编码查询保护 | 未实现：当前没有用户编码和对应查询入口，也没有完整等值查询、无泄露错误、多维限流、审计及禁止公开/模糊/批量枚举的实现门禁。 |

## 5. 原目标领域规则（未整体实施）

以下是后续目标规则，不能作为直接修改已审行为的授权。当前仍按 OIDC Session 保存上下文；未知消费保留 REVIEW/预占，只通过已确认的持久释放意图或显式最终免单收敛，不因超时或权益到期自动免单。旧 LEGACY/普通 Credit 不追加有效期，新个人 PLAN 不可退款；原自管 Token API/UI 保留。完整 BASE/BONUS、应收、注销、平台 RBAC 及全局上下文实现需另审方案与迁移。

### 5.1 身份、业务账号与外部客户映射

- Identity Provider 负责认证、密码、登录会话、Refresh Token 和会话撤销，不保存组织角色和资金权限事实。
- Account Service 负责稳定用户 ID、当前用户名/手机号、组织成员关系、角色权限、全局活动上下文及其版本。
- Commerce Provider Mapping 只保存用户或组织与外部客户之间的 Provider、Adapter 版本、脱敏外部引用和同步状态。
- Commerce 映射未完成只能阻止依赖该映射的支付操作，不能阻止登录、模型调用、组织管理或其他账号能力。

### 5.2 组织额度分配

1. 管理员分配时必须选择具体组织权益包及其积分组成，不能只提交一个无法追溯来源的总额。
2. 批量分配按成员逐项执行独立事务，允许部分成功；响应必须返回每一项的成功、失败原因和幂等结果。
3. 成员被移除时，未消费授予转为 `SUSPENDED`，继续占用原组织权益且不回到未分配池；成员在原到期日前重新加入同一组织时恢复，原到期时间不变。
4. 有 `org.quota.allocate` 权限的管理员可以给自己分配，仍受组织总额度、具体权益包和审计约束。
5. 已创建授予不能直接改额；调整必须先撤回未消费且未被 Hold 占用的部分，再创建新授予。
6. 显式撤回成功的未消费额度返回原权益包可分配余额；成员移除导致的暂停不是撤回。

### 5.3 动态预占与权益过期

```text
参数值 = 请求或模型元数据明确值；缺省时为 0
预占单位数量 = 版本化协议预占公式(参数, 模型元数据)
单位费用 = 计量单位数量 × 该单位版本化单价 ÷ 单价分母，并按单位规则舍入
初始 Hold = 所有计量单位费用之和
```

- 计量单位定义至少覆盖 `INPUT_TOKEN`、`CACHED_INPUT_TOKEN`、`OUTPUT_TOKEN`、`IMAGE`、`REQUEST`、`DOCUMENT`，并允许供应商中立扩展；一个价格版本只能包含其绑定协议公式实际引用的单位。
- Chat、Embedding、Rerank、图像等协议分别发布不可变公式版本；公式引用单位与价格单位集合不一致时拒绝发布或创建 Hold。显式值为 `0` 的价格行表示免费，价格行不存在表示配置缺失，二者不得合并。
- `INPUT_TOKEN` 只记录非缓存输入，`CACHED_INPUT_TOKEN` 只记录缓存输入；Provider 的含缓存总输入量必须先扣除缓存量再写入 `INPUT_TOKEN`，同一 Token 不得重复进入两个单位。

- 长调用在到期前续租 Hold；续租失败时不得静默继续产生无限费用，应终止可控调用或进入不确定核对。
- 实际费用超过 Hold 时先申请追加预占；追加成功后结算，追加失败时停止后续可控输出并进入人工复核，同时记录对应钱包负余额投影和独立应收，不能把其他上下文余额作为兜底。
- 权益到期时直接释放并移除仍不确定的 Hold。释放不会恢复已经过期的可用积分；之后确认的真实用量记入人工损失复核，不重新扣减用户其他权益。

### 5.4 权益、退款与消费顺序

- 普通退款现金价值只来自目标支付流水所绑定权益包的全部未消费 BASE；任一 BASE 或该支付产生的关联 BONUS 曾经消费（包括之后冲正）即拒绝普通退款。退款成功时必须冲销该 `payment_transaction_id + entitlement_package_id` 全部未消费 BASE、该支付产生的全部未消费关联 BONUS 及相应账本负债；BONUS 不折算为现金退款，人工赠送和同一订单其他支付流水不受影响。
- 无法冲销足够基础积分、退款状态不确定或 Provider 退款失败时进入人工复核，不允许制造负余额掩盖差异。
- 只有现金充值产生的 BASE、退款追回或受审计人工还款可以清偿独立应收；BONUS 不得清偿应收。
- 拒付冲销绑定原支付流水和原权益包，先撤销该包可撤销的 BASE 与因该支付发放的关联 BONUS；不足现金本金形成订单级应收，不冲销或改写其他权益包。
- 拒付应收未清时冻结对应钱包上下文的新 Hold 和平台模型调用；其他订单的 BONUS 不改账，清偿后仍只按原有效期恢复可用。
- 人工赠送积分自入账起 30 天到期。
- 先按到期时间排序；同日到期时依次消费赠送积分、基础积分；仍相同则按入账时间和稳定 ID 排序。

### 5.5 全局活动上下文

- 每个用户全局只有一个活动上下文，桌面与 Web 使用同一条版本化记录，不再按 OIDC Session 分别保存。
- 个人钱包负数只冻结个人聊天和计费上下文，不影响用户使用正常组织授予；组织钱包负数只冻结该组织上下文，不影响个人或其他组织上下文。
- 切换成功后递增 `context_version`；所有业务 API 响应返回该版本，客户端发现更高版本时必须丢弃旧额度、权限、路由和组织缓存。
- 切换前已经开始的模型调用保存上下文快照和版本，始终按该快照结算。
- 成员被移除或组织停用时，服务端把受影响用户的活动上下文切回个人并递增版本。
- 平台后台授权独立于活动上下文，不能因为用户切换到个人或组织而获得或丢失平台权限。

### 5.6 平台角色、服务身份与恢复

- 部署幂等创建用户名 `admin` 的第一个超级管理员并保持 `BOOTSTRAP_PENDING`，部署者选择受控终端无回显直接设置正式密码，或使用默认256 bit、最少128 bit熵、无填充 Base64URL 的一次性引导凭据。引导凭据默认15分钟有效、最多5次失败，只在受控通道读取一次；交付明文首次读取后销毁，成功兑换原子设置正式密码并删除服务端校验值，激活前不能登录后台或执行管理动作。
- Break-glass 只在凭据丢失、意外停用或身份故障等异常导致全部超级管理员无法有效登录时使用离线运维命令和主机/集群高权限身份，创建短时紧急授权或恢复超级管理员；不得提供公网恢复 API，不得绕过最后一个有效超级管理员主动注销保护或替代计划内角色交接，执行立即告警并写不可删除审计。
- 同一用户可以同时持有计费运营和审批角色，但不能审批自己创建的申请；允许不同用户互相审批各自申请。ADM-37 的强制审批命令无论金额大小都只需一名独立审批人；ADM-38 中的普通退款和组织删除只审计、不进入财务审批流。
- 所有角色绑定可设置开始和到期时间，临时或紧急授权到期后自动失效并使授权缓存失效。
- 服务账号不是人类用户，不进入组织成员和平台角色表。每个服务获得独立工作负载身份及最小内部权限；后台任务继承所属服务身份，Event Relay 只允许读取/确认本服务 Outbox 和发布允许的事件 Subject，不能调用资金审批接口。

### 5.7 邀请、注销与资料变更

- 组织邀请支持手机号、用户名和一次性邀请链接。一次性 Token 只保存摘要，三种邀请均绑定目标组织、预授予角色、有效期和邀请人。
- 注销目标采用 `CLOSURE_REQUESTED → QUIESCING → FINANCIAL_CLEARANCE → IDENTITY_CLEANUP → ANONYMIZED → COMPLETED` 持久 Saga。进入静默期后禁止新模型/Hold、订单、资料、成员和授权事实，只允许清偿、既有订单处理、证据复核、所有权转让和状态查询等明确修复动作；阶段失败幂等重试，超过 SLA 进入 ADM-34。
- 注销完成的资金门禁除个人余额、应收、本人支付/退款订单和 Hold 外，还包括待核对模型用量、未解决拒付、未分类或有资金影响的对账差异、外部待确认关单/晚到审核/PROVIDER_BLIND，以及资金相关 Commerce Mapping 任务。纯信息差异不阻断；资料同步任务可以自动取消。用户是任一有效组织唯一所有者时必须先转让所有权，是最后一个有效超级管理员时必须先向另一名有效用户完成角色交接。
- 资料修改 Saga、平台角色、普通组织角色、临时或紧急授权和未接受邀请不要求用户手工处理，也不作为资金资格阻断；组织所有者绑定仍必须先转让。注销协调器必须分别取消或向前收敛资料 Saga、撤销授权并清缓存、使邀请失效。自动清理失败属于标识释放技术门禁，进入重试或 REPAIR_REQUIRED，不能被忽略。
- 组织余额和未消费组织授予不阻断普通成员注销。注销终止全部成员关系，未消费授予以 `SUSPENDED/ACCOUNT_CLOSED` 继续计入原权益组成的暂停授予未消费额，不能通过普通接口撤回、恢复、转移或直接重新分配，也不能消费或创建新 Hold。
- 账号进入 `ANONYMIZED` 后，具有专用权限的管理员可在不存在消费、Hold、待核对用量或资金冻结时，一次性全额回收单条授予当前全部可回收剩余量；额度只回到原权益包原 BASE/BONUS 组成且原到期日不变。未回收部分到达原到期日后直接失效且不回池；回收不会把旧授予关联给相同标识注册的新账号。
- 标识释放前必须确认 Identity 已删除或永久停用且不可认证、Account 自动清理完成、Commerce Mapping 已 `DETACHED`，并用主体生命周期版本拒绝迟到命令/消息/回调。随后 Account 原子写随机墓碑值、释放用户名/手机号唯一占用并进入 `ANONYMIZED`；同一标识重新注册创建新用户 ID，不关联旧权益、邀请、授予或账本。
- 账本、订单、退款、对账和审计使用不可逆内部主体 ID 保留 3 年，不保留可直接登录或联系用户的信息；法律保全到期前除外。
- 用户名和手机号修改使用可恢复 Saga：创建变更请求并预约新值，完成重新认证/验证码验证，更新 Identity Provider，再提交 Account；手机号提交后撤销其他会话并异步更新 Commerce Mapping。
- Identity Provider 已更新但 Account 未提交时优先向前重试，不自动反向覆盖用户凭据；Account 已提交但 Commerce 同步失败时允许登录和模型调用，仅暂停新支付并进入人工修复。

### 5.8 注册并发、密码边界与补偿

- 注册可用性预检是图形挑战后的非绑定提示。发送短信前必须原子取得用户名和手机号的 `SMS_PENDING` 预约；短信验证成功升级为 `REGISTRATION_IN_PROGRESS`，Identity 创建后升级为不可由普通 TTL 释放的 `IDENTITY_CREATED` 恢复锁。
- 服务端 `registration_operation_id` 及派生幂等键贯穿短信发送/核验、Identity 创建、Account 提交和钱包初始化。同键重试返回原结果；同键改变用户名、手机号、挑战或密码返回冲突，不产生第二个 Identity 或 Account。
- 密码预检和最终创建只由 Identity Adapter 执行。Account 仅接收非秘密注册事实；原密码及其无密钥摘要不得进入数据库业务表、消息、日志、审计或 Trace。密码为8–128位可打印ASCII，允许中间半角空格，首尾空格明确拒绝，任何层不得trim、规范化、改大小写或截断。
- Identity 创建后优先向前提交 Account：30分钟未收敛进入 REPAIR_REQUIRED，预约继续占用；满24小时后只有证明 Account 从未提交且没有任何钱包、订单、成员、授予、用量或在途提交事实，才能先停用并确认 Identity 不可认证、再释放预约。结果不确定时保持修复状态。
- 客户端通过操作 ID 和签名查询令牌恢复未完成注册，运营通过 ADM-35 重试确定阶段。用户查询和运营页面都不能显示密码、验证码、密码指纹或 Provider 原文，也不能绕过证明门禁直接补偿。

## 6. 个人 PLAN 充值目录与 UAT 限制

接口只信任服务端商品 ID，以下是 A06 已保留的个人 PLAN 暂定 UAT 映射；`PLAN_TRIAL` 仅 ALIPAY，其他渠道以商品目录与 Provider capabilities 的交集为准。它不替代原四档 ORG LEGACY 模拟充值，也不表示真实商城验收通过：

| Product | SKU | 金额（分） | 基础积分 | 赠送积分 | 总积分 | 有效期 |
|---|---|---:|---:|---:|---:|---:|
| `PLAN_TRIAL` | `0502AJON` | 10 | 1,000 | 1,000 | 2,000 | 30 天 |
| `PLAN_STANDARD` | `0502AJ0Q` | 1,000 | 2,000 | 2,000 | 4,000 | 30 天 |
| `PLAN_ADVANCED` | `0502AJOR` | 3,000 | 4,000 | 5,000 | 9,000 | 30 天 |
| `PLAN_FLAGSHIP` | `0502AJOP` | 20,000 | 20,000 | 30,000 | 50,000 | 30 天 |

当前目录标记为 UAT 暂定。任何金额、SKU、名称或权益调整都必须形成新的目录/迁移变更，不能仅改客户端显示。

## 7. 正式上线门禁

以下条件未满足前，不应把当前 UAT 配置视为正式支付上线：

1. 商城书面确认四个 SKU、成交金额、二维码内容、订单查询状态和品悟权益。
2. Billing 商品目录从暂定状态冻结，生产不再开启“允许暂定价格”开关。
3. 使用非测试商城账号完成至少一笔小额支付，验证订单金额、商城订单号、二维码、支付查询、单次入账和权益到期时间。
4. 验证重复创建、重复查询、服务重启、客户端退出、网络超时和晚到支付不会重复入账。
5. 验证当前 Provider 的关单、回调、退款和账单 capabilities；不支持关单/回调时完成查询降级验收，不支持退款时确认所有入口隐藏；账单四方对账和差异责任人/修复/审计闭环通过验收。
6. 生产 Provider 使用 HTTPS、主机白名单和 Secret 管理；真实密钥不在 Compose 文件、Shell 历史或日志中。
7. 数据库备份、恢复演练、监控告警、磁盘容量和日志保留策略已由运维环境落实。
8. 当前 Model Gateway Adapter 能按上游请求引用提供稳定用量证据，Gateway 自动核对、Hold 续租/追加和人工重试通过验收。

## 8. 明确不应实现的行为

- 不允许客户端提交或覆盖充值金额、SKU、积分或支付成功状态。
- 不允许本地随机生成新华三支付二维码。
- 不允许在商城不支持时仅修改本地状态来模拟关单、退款或对账成功。
- 不允许普通用户查看真实供应商密钥、New API 管理凭据或内部服务 Token。
- 原目标曾计划取消个人中心自管 Token；本次历史保全未批准删除既有功能，现有个人/组织 Token、凭据及受限路由 API/UI 必须保留。平台托管资源仍不得由普通用户修改，本地独立模型继续留在桌面本地配置域。
- 不允许 Gateway 因网络失败自动重发模型请求。
- 不允许有效组织没有所有者或同时存在多个所有者；所有权变更必须使用唯一所有者转让流程。
- 不允许把个人上下文和组织上下文的余额、Token 路由或审计混在一起。
- 不允许将任何开发/测试 Payment Provider、测试渠道或 Mock New API 的能力描述为生产接口能力。
