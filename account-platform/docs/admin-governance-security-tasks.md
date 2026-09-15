# 治理与安全整改实施任务

> 需求基线：`e15fe698`，身份 B 最终需求审核 PASS。
> 本任务拆分：身份 B 复审 PASS，提交`90b2ea08`；GSA-01 DONE / B审核PASS并以`88d37a53`提交，GSA-02 DESIGN DONE / B审核APPROVED并以`b8216c97`提交，GSA-03 DONE / B审核APPROVED并以`0dd3d481`提交；GSA-04代码已完成且B代码审核通过、以`0c9db1e2`提交，环境验收待补；GSA-05协议文档以`16895b97`提交，Provider p2兼容事实修订以`4e06e225`提交，代码获B最终APPROVED并以`cad7f48f`提交；GSA-06进入设计审核，其余PENDING，均NOT_DEPLOYED。

## 1. 所有权与执行规则

需求合同唯一引用 [整改设计](admin-governance-security-redesign.md)，不在任务表重新定义协议。主目标唯一归属：APT-141 拥有 ADM-67，APT-142 拥有 UI-60，APT-143 拥有 NFR-57。以下 GSA 编号是可独立审核、测试、提交的执行子任务；主任务作为汇总，不能以一个大提交替代子任务交付。

逐项执行：A 实现及自检 → B 独立审核 → A 修正 → B PASS → Signed-off 独立提交 → 更新证据 → 下一项。B 未通过不得进入下一执行子任务。每次提交只能交付一个 GSA 子任务；设计子任务同样必须审核并独立提交。需求取舍、未经确认的财务规则和互斥方案提交用户决定。

2026-09-15 用户明确授权“跳过pending继续完成后续任务”：GSA-04的三项隔离Linux验收仍受环境阻断，但基于该前置已有Signed-off提交`0c9db1e2`、身份B代码审核通过及本机适用验证通过，可以继续后续设计/实现、逐项A/B审核和独立提交。该顺序例外可沿依赖链传递地允许推进下一项；任何缺Linux证据的前置及受其影响的下游都必须保留`ENV_PENDING`或`DEPENDENCY_ENV_PENDING`，不得冒充最终`DONE/APPROVED`。checker无法表达最终完成时，统一保持`IN_PROGRESS / CODE_COMPLETE / B_CODE_REVIEW_PASS / DEPENDENCY_ENV_PENDING / NOT_DEPLOYED`。例外不豁免代码缺陷、任务自身适用验证、A/B审核、真实技术依赖或生产部署/activation/cutover门禁；所有Linux欠账仍须在生产启用前补齐。GSA-05自身只依赖已DONE的GSA-02，文档与代码各自审核/验证通过后可按通常规则DONE。

每项必须记录实际文件、测试命令/结果、B 结论、提交、已知差距。代码任务 DONE 要求真实行为测试及适用数据库集成测试通过，不能用 SKIP 或静态字符串断言代替；有代码完成但缺环境证据时保持 IN_PROGRESS 并记录 CODE_COMPLETE/ENV_PENDING，不关闭主目标。所有产品代码提交执行 `node scripts/account-platform.mjs verify` 和任务定向门禁；文档步骤执行任务覆盖与文档基线检查。只在改动或新失败需要时重跑已经通过的检查。

DESIGN 指事实追踪或领域设计：完成代码依据、版本/schema、不变量、验收映射、文档检查、B PASS及独立提交即可DONE，不要求尚未实现的行为测试，不表示功能已实现。CODE 指仓库实现及本地/隔离环境验证；CODE+ENV 额外要求生产等价环境或部署证据。此次提交授权不等于生产部署/激活授权；发布任务先完成工具与隔离演练，实际生产操作单独取得授权。所有子任务在此文档内顺序执行，表内依赖是最低技术依赖，不能以没有依赖为由并行实现。

## 2. 独立执行队列

所有条目初始审核为 NOT_REVIEWED、提交为无；状态只能按证据逐项更新。

| 子任务 | 主任务 | 独立交付 | 技术依赖 | 类型 | 状态 |
|---|---|---|---|---|---|
| GSA-01 | APT-141 | policy-only 推进门禁与停用后的撤回/拒绝 | APT-008, APT-138 | CODE | DONE |
| GSA-02 | APT-141 | 逐命令现状、策略/快照/接口兼容矩阵 | GSA-01 | DESIGN | DONE |
| GSA-03 | APT-141 | Billing owner-domain activation 投影及既有三命令兼容 | GSA-02 | CODE | DONE |
| GSA-04 | APT-141 | Account control 模型、持久状态和事务锁接口 | GSA-03 | CODE | IN_PROGRESS |
| GSA-05 | APT-141 | typed closed RFC8785完整整数编码与legacy/新授权wire适配 | GSA-02 | CODE | DONE |
| GSA-06 | APT-141 | 授权 Outbox epoch/sidecar 前向迁移及旧 writer 兼容 | GSA-04, GSA-05 | CODE | DESIGN_IN_REVIEW / DEPENDENCY_ENV_PENDING |
| GSA-07 | APT-141 | fence-aware Relay 与授权事件永久禁止人工 replay | GSA-06 | CODE | PENDING |
| GSA-08 | APT-141 | 六命令 consumer、持久尝试及确认式 DLQ 收口 | GSA-07 | CODE | PENDING |
| GSA-09 | APT-141 | 历史审批 reconciler、exclusion 与 SAFE 收敛 | GSA-08 | CODE | PENDING |
| GSA-10 | APT-141 | 安全命令目录、历史可见集合及无泄露读取 | GSA-03, GSA-04 | CODE | PENDING |
| GSA-11 | APT-141 | 有界 Account membership snapshot/cursor | GSA-10 | CODE | PENDING |
| GSA-12 | APT-141 | v2 最小列表、统计与六命令 closed detail | GSA-11 | CODE | PENDING |
| GSA-13 | APT-141 | Account proof 客户端、批准检查点与版本化动作 | GSA-04, GSA-05, GSA-12 | CODE | PENDING |
| GSA-14 | APT-141 | Token 三命令 proof 及执行时 revision/epoch 检查 | GSA-13 | CODE | PENDING |
| GSA-15 | APT-141 | Billing 三命令 proof 及执行时 revision 检查 | GSA-13, GSA-14 | CODE | PENDING |
| GSA-16 | APT-141 | 逐命令失败分类、查证与安全恢复 worker | GSA-09, GSA-15 | CODE | PENDING |
| GSA-17 | APT-142 | Portal v1/v2 会话锁定、BFF、capability 与动作代次 | GSA-12, GSA-15 | CODE | PENDING |
| GSA-18 | APT-142 | 审批中心导航、目录、分页、详情和决定交互 | GSA-16, GSA-17 | CODE | PENDING |
| GSA-19 | APT-142 | 业务页编辑、中心提交和替代申请双向闭环 | GSA-18 | CODE | PENDING |
| GSA-20 | APT-142 | 审计/导出、系统/安全、偏好与全分区 UI 验收 | GSA-19 | CODE | PENDING |
| GSA-21 | APT-141 | 权益组成调整领域设计 | GSA-16, GSA-20 | DESIGN | PENDING |
| GSA-22 | APT-141 | 权益组成调整实现与领域 E2E | GSA-21 | CODE | PENDING |
| GSA-23 | APT-141 | 账本修复领域设计 | GSA-22 | DESIGN | PENDING |
| GSA-24 | APT-141 | 账本修复实现与领域 E2E | GSA-23 | CODE | PENDING |
| GSA-25 | APT-141 | 应收核销领域设计 | GSA-24 | DESIGN | PENDING |
| GSA-26 | APT-141 | 应收核销实现与领域 E2E | GSA-25 | CODE | PENDING |
| GSA-27 | APT-141 | 支付财务修复领域设计 | GSA-26 | DESIGN | PENDING |
| GSA-28 | APT-141 | 支付财务修复实现与领域 E2E | GSA-27 | CODE | PENDING |
| GSA-29 | APT-141 | 退款异常修复领域设计 | GSA-28 | DESIGN | PENDING |
| GSA-30 | APT-141 | 退款异常修复实现与领域 E2E | GSA-29 | CODE | PENDING |
| GSA-31 | APT-141 | 拒付财务修复领域设计 | GSA-30 | DESIGN | PENDING |
| GSA-32 | APT-141 | 拒付财务修复实现与领域 E2E | GSA-31 | CODE | PENDING |
| GSA-33 | APT-141 | 对账财务修复领域设计 | GSA-32 | DESIGN | PENDING |
| GSA-34 | APT-141 | 对账财务修复实现与领域 E2E | GSA-33 | CODE | PENDING |
| GSA-35 | APT-141 | 用量结算修复领域设计 | GSA-34 | DESIGN | PENDING |
| GSA-36 | APT-141 | 用量结算修复实现与领域 E2E | GSA-35 | CODE | PENDING |
| GSA-37 | APT-142 | 八命令业务入口及审批 presenter 联调 | GSA-36, GSA-20 | CODE | PENDING |
| GSA-38 | APT-143 | 切换/回滚工具、资源清单与只读 readiness | GSA-37 | CODE | PENDING |
| GSA-39 | APT-143 | 生产等价集成、性能、N/N-1 与故障演练 | GSA-38 | CODE+ENV | PENDING |
| GSA-40 | APT-143 | 授权后的逐命令灰度与生产证据归档 | GSA-39 | CODE+ENV | PENDING |

## 3. 任务边界与验收

### GSA-01～09：推进门禁与持久执行链

- **01**：Account create/update 已有静态 executable 检查，保留并补齐 submit/APPROVE/Begin。policy-only 不得APPROVE、写APPROVED决定/授权 Outbox/新 execution；已有 EXECUTING/EXECUTED 结果读取保持。仅 SUBMITTED 的本人 WITHDRAW、独立审批人 REJECT 在 Token PRE/SAFE 和 Billing INACTIVE/policy-only 下均可安全收口，仍检查身份、权限、版本、禁止自审。单元+真实 Account PG 覆盖六可执行/八不可执行、两域安全收口、DRAFT 不可撤回、无权限/并发与零领域调用。此项不宣称完整 owner-domain activation 已交付。
- **02**：以[逐命令事实与兼容基线](admin-governance-security-command-baseline.md)冻结已有 API、代码位置、版本、实际快照、近期认证、consumer/executor、部署前置与缺口，覆盖六既有及八 policy-only，并映射 GSA 所有权。只做事实与版本矩阵，不新建功能；DESIGN通过不表示目标proof、v2、activation或恢复协议已实现。
- **03**：当前 Billing 尚无持久 activation 行，三类既有命令以静态`Executable=true`运行、八类以静态`Executable=false`失败关闭；本任务才由 Billing 建立其权威投影，前向迁移保全三命令 ACTIVE、物化八命令 INACTIVE。Account 只消费该投影，Token 三命令继续复用 Token 既有状态。迁移幂等、缺行/未知版本/不可用失败关闭及 N/N-1 回归；无实际生产激活。发布唯一顺序为先执行 Billing `000054` 前向迁移，再部署提供闭合只读投影的 Billing 服务，最后部署强读该投影的 Account；回滚必须先回滚 Account、再回滚 Billing 服务，并永久保留已有事实的 `000054` current/history 表，不执行其数据破坏性 down。
- **04**：按设计 §5.4 实现唯一 ADMIN_APPROVAL control、双 epoch、8 行动作矩阵、合法边及窄事务接口。共享/排他行锁与 CAS 的真实 PG 双连接测试；安全默认 LEGACY，尚未接线的功能不能开启。
- **05**：按 §8/§9.1.1 交付独立窄编码包，重复键、整数精度、Unicode/转义、null 与封闭结构拒绝、大小边界及跨实现 golden；LEGACY_TYPED_V1 精确使用 Event.Marshal，历史重构 provenance 不证明原 broker bytes。
- **06**：epoch/sidecar/additive migrations、原事件不变、旧 writer trigger FOR SHARE、完整事务重试及最终验证；覆盖迁移前 DLQ、并发写、maintenance version3 兼容、错误 epoch、不可重构数据失败关闭，不修改已发布迁移。
- **07**：仅 Account 授权 subject 的 Relay control 接线及 SAFE_DRAIN；同一原行自动至少一次恢复、永久禁止人工 replay（Portal/HTTP/workload/DB），其他事件合同不变。PubAck-close 崩溃及超过去重窗的重复投递不产生重复业务效果；验证 FENCE/Claim 锁竞争和旧权限撤销。
- **08**：三 Billing/三 Token 唯一 durable 映射、MaxDeliver=-1、应用层持久最多 12 次领域尝试、DLQ PubAck→ACK_PENDING→DoubleAck，崩溃只恢复确认；receipt/权限/资源 manifest 一致。真实隔离 NATS 和 PG 验证；生产 Q 与资源迁移由38～40执行。
- **09**：按 §8.6/§8.7/§9 实现 Begin 同库线性化、5分钟 deadline、历史状态清零、无效果才失效、合法 DLQ exclusion、未知效果阻断及 SAFE 恢复。不得另造授权事件、把消息 receipt 当业务效果或让旧 epoch 越过 gate。只实现受控工具和验证，不运行生产切换。

### GSA-10～20：服务端读写合同与界面

- **10**：安全目录仅 visible/requestable；本人历史及当前授权可见集合；不可见与不存在的 detail/子资源统一无泄露404。撤权先于 presenter/领域调用，内部 policy API 不暴露给无对应权限浏览器。
- **11**：设计 §6 的 envelope、排序/主体/scope/key 绑定、容量/限流/TTL/tombstone/deadline/清理；复用有界模式且不改变 Billing/Token 现有默认与错误合同。真实 PG 并发分页无重复遗漏、撤权/密钥轮换/过期、限流及容量；生产规模性能留39。
- **12**：六命令 oneOf presenters、未知历史安全最小 envelope、聚合权限/分母、v1原合同与v2最小列表。schema验证和真实handler响应测试，禁止原始JSON fallback。
- **13**：设计 §8 的 proof_input 映射、2秒/64KiB/60秒/时钟边界、内部身份、白名单错误和 request/proof 绑定；本域 CAS、远端调用不持锁、REJECT 零领域调用。v1/v2批准共享 proof gate，legacy observed_versions 兼容准确；未就绪保持gate关闭。
- **14**：三 Token proof、既有 activation/epoch、领域 revision 副作用前检查，Token三命令15分钟认证先于领域调用；漂移/SAFE/重复/响应丢失及三个命令真实跨域E2E。
- **15**：Wallet与Provider/Product三命令proof及领域执行重验，Wallet不新增近期认证；支付恢复沿用15分钟且只读持久 observation/resume_draft，所有浏览器批准链零Provider主动探测。真实三命令成功、漂移、依赖不可用和结果不明测试。
- **16**：逐命令 TRANSIENT_SAFE/UNKNOWN/PERMANENT/失效分类，原执行ID、有界退避/次数/窗口、独立worker身份与lease；未知错误默认查询，禁止通用重试。六命令的崩溃、并发、窗口结束及幂等验证。
- **17**：BFF精确路径/字段、proof-like输入拒绝、Redis session v1/v2原子锁定、旧session默认v1、capability/no-store/action_control_version；多副本/多标签、升降级与迟到响应不复活动作测试。
- **18**：审批中心四视图、权限导航、筛选URL/游标、摘要/详情/动作/状态、禁自审与失效重读。三语、键盘/焦点/读屏、320/768/1440px及200%缩放、真实错误/空/局部失败；截图中的侧栏重复先真实复现，不能把长截图拼接直接断定为代码缺陷。
- **19**：业务页拥有编辑；中心只提交最新未修改DRAFT；业务页编辑后可提交同一DRAFT，终态fresh superseding新ID。禁止APPROVED/EXECUTING/UNKNOWN替代，step-up后不自动重放，URL回返与六命令流程E2E。
- **20**：审计/导出、系统与安全、偏好按设计 §2 与既有合同核对及修正信息排布、操作状态、标签、无障碍/响应式；运行手册只读、Keycloak受控入口边界、真实导出生命周期。无新API或权限依据的能力不展示。此批不引入T9异常索引。

### GSA-21～37：八类财务命令

21/23/25/27/29/31/33/35 分别只交付对应命令的领域设计，逐项落实设计 §11：精确允许/禁止变化、权限/政策新版本、closed command/snapshot/proof/presenter、金额和组成守恒、补偿而非改写历史、外部证据、未知结果、CAS/幂等、迁移与回滚。B 通过后才进入下一实现项；需要用户决定的账务规则先给具体选项及影响，不猜填。每类创建独立领域设计文档，已有AUDIT_ONLY路径不升级冒充新命令。

22/24/26/28/30/32/34/36 各只实现上一设计通过的一个命令：领域schema/handler、补偿账务、proof、executor/consumer/recovery、契约、权限、审计、注册和默认INACTIVE的前向版本。正常/非法/并发/重复/未知/撤权/窗口/回滚及真实领域E2E全部验收。不得发布整组activation；代码完成仍NOT_DEPLOYED。用量结算修复绝不重发模型请求；退款异常修复不开放普通退款产品入口。

37 仅实现八类已完成后端对应的业务页表单/预检/状态深链和审批closed presenter，按八类逐项三语、错误、无障碍、权限及E2E验收；发现可独立更小交付时先更新此任务拆分并经B审核，不能在UI里补领域逻辑。

### GSA-38～40：发布工具与证据

38 交付设计 §14 唯一顺序的迁移编排、旧writer/Relay权限退役、drain/FENCE/Q/Q+1/旧durable退役/readiness、SAFE恢复、单命令activation、审计化receipt和runbook。dry-run不能写生产；隔离PG/NATS故障演练证明执行顺序与失败关闭。产品实现缺口退回其所属子任务，不把发布脚本当旁路。

39 在生产等价环境完成资源规模/分页/索引、N/N-1、权限、跨域、控制锁、迁移/回滚、网络/进程故障、旧客户端与三语UI验收。每项记录版本和原始证据；UNKNOWN effect、任一状态/资源门禁不满足或SKIP均不通过。独立于APT-137，不借用它的未完成结论；可复用它已验证工具。

40 只有取得实际生产操作授权和39通过后，按单命令执行迁移、灰度、健康观测及回滚验收；保留既有三Billing能力、Token持久gate与八类逐项activation。未部署时保持PENDING/ENV_PENDING/NOT_DEPLOYED，不以需求或代码审核PASS冒充上线。

## 4. 原方案覆盖与关闭标准

| 原计划 | 本清单映射 |
|---|---|
| T0 activation/executable | 01、03、04、06～09、13～16 |
| T1现状及兼容矩阵 | 02及各领域设计 |
| T2目录与导航 | 10、17、18、20 |
| T3 cursor | 11 |
| T4摘要/列表/详情 | 12、18 |
| T5业务编辑/提交/替代 | 19 |
| T6 Token proof | 05、13、14 |
| T7 Billing proof | 05、13、15 |
| T8失败与恢复 | 08、09、16 |
| T9可选异常索引 | 未确认，不创建执行任务，入口隐藏 |
| T10～T17八财务家族 | 21～37，每家族独立设计和实现 |
| 迁移及发布 | 04～09、17、38～40 |

APT-141 仅在归属的所有DESIGN与CODE子任务通过并提交后关闭；APT-142 同理。APT-143 必须38～40全部通过且有实际环境/部署证据才能关闭。新目标此前未实现，本清单不能追认旧任务为完成证据。

## 5. 审核与验证记录

- 步骤1：B最终PASS；需求提交 `e15fe698`。
- 步骤1验证：`node scripts/account-platform.mjs verify` exit0；Go test/vet、OpenAPI、三语、observability、architecture通过；Node 552项，550通过/2跳过/0失败。上述为既有代码基线，不是新功能验证。
- 步骤2：B复审PASS，闭合两域安全收口及DESIGN任务验收分类；任务覆盖检查143主任务/34未完成目标通过，文档基线和diff检查通过。无实现提交。

后续逐项追加：`GSA编号 | A自检 | B轮次与结论 | 测试与skip | 提交 | 已知差距 | 部署状态`。提交哈希可在后续任务的证据记录中补记，禁止伪造自引用commit。

GSA-01：代码候选已补齐静态推进检查、policy-only及未激活Token动作裁剪、两域安全收口，WITHDRAW重验当前request权限，409稳定错误同步OpenAPI。列表每个响应最多读取一次Token activation，读取失败仅关闭推进动作，不阻断历史读取与安全收口。

- A自检：无新增领域执行权限，不修改既有迁移、底座、生产配置或业务执行器；EXECUTING/EXECUTED结果读取保留原executor校验。
- B审核：首轮指出Token动态动作裁剪、真实PG推进拒绝矩阵及测试撤权列名缺口，均已修复；后续历史fixture的deadline及decision触发器前置也按真实状态链修正。既有fixture的迁移32 schema适配、独立JWKS CA配置均已获B审核PASS；整批最终代码审核APPROVED，无剩余阻断。
- 实际验证：完整`node scripts/account-platform.mjs verify` exit0；Go test/vet、OpenAPI、三语、observability、architecture通过，Node合同550 PASS/2既有环境SKIP。独立PG17上的`TestApprovalAdvancementPostgresIntegration`实际PASS/no-SKIP，覆盖14命令、PRE/SAFE动作/推进拒绝、安全收口、撤权、并发、零执行授权副作用以及14命令EXECUTING/EXECUTED历史读取。
- 最终真实PG记录：Account/Token使用独立隔离数据库并补齐既有角色前置，三组具名测试联合运行exit0/no-SKIP：`TestApprovalAdvancementPostgresIntegration` 13.08s、`TestApprovalSnapshotLifecyclePostgresIntegration` 45.49s、`TestTokenApprovalConsumersPostgresIntegration` 212.06s（生命周期、目录回滚、凭据撤销全部PASS）。此记录不是非superuser部署、NATS或生产激活证据。
- 环境与同类问题：初次同库测试的扩展不可见、角色前置不足已修正；真实回归发现并修复旧快照fixture遗漏迁移32隔离schema适配、以及JWKS测试仍错误依赖全局Transport信任的两个测试缺口。生产迁移与TLS安全边界均未修改。
- 提交前刷新：并行PG运行期间一次完整verify因两项既有CLI子进程15秒超时失败；单独复跑该文件14/14 PASS，随后无并行PG负载的完整verify最终exit0（550 PASS/2既有环境SKIP），未修改超时阈值或产品代码来绕过测试。
- 状态：DONE / B审核PASS / NOT_DEPLOYED；签名提交`88d37a53`。主目标ADM-67仅部分实现，APT-141继续IN_PROGRESS。
- 已知边界：v1列表包含Token历史项时最多等待一次5秒activation读取；失败仍返回历史并关闭推进动作。本项不交付Billing持久activation、proof或新财务命令，不构成生产上线验收。

GSA-02：身份A已基于提交`88d37a53`建立[十四命令事实与兼容矩阵](admin-governance-security-command-baseline.md)，状态为DESIGN DONE / B整批审核APPROVED / NOT_DEPLOYED。

- 交付：七份需求/设计/任务/状态文档冻结现有API、p/s/wire、权限、实际快照、近期认证缺口、consumer/executor与后续所有权；纠正ADM-64为部分实现，保留APT-138 DONE历史，新增只引用GSA-13/14实现证据的APT-138-R1 PENDING关闭记录。
- 必要门禁修复：`scripts/check-account-platform-tasks.mjs` 将APT-138前置约束限于既定三批，允许历史完成后的新整改记录；通用唯一owner、依赖、循环、目标完成约束不放宽。配套测试覆盖三前置分别阻断、历史保留、目标和依赖未完成不得关闭。
- B审核：原生/BFF路径、六类策略、迁移追加史、现状与目标状态、producer JSON与closed schema、consumer检查点、既有财务操作边界、旧统计与近期认证缺口、唯一实现所有权等问题均经A修订并获B整批复审APPROVED，无剩余阻断。
- 验证：最终完整`node scripts/account-platform.mjs verify` exit0，Go test/vet、OpenAPI（9项既有warning）、Node 550 PASS/2项既有环境SKIP、三语、observability、architecture全部通过；`node --test scripts/check-account-platform-tasks.test.mjs` 11/11 PASS；任务检查143 tasks/4 remediations/35 incomplete/354 completed PASS，文档基线与diff检查PASS，基线47个相对链接存在。
- 边界：本项仅文档和文档门禁修复，无产品代码、策略、迁移、activation或部署配置变化；未新增proof/v2/恢复/财务执行能力。APT-141继续IN_PROGRESS，ADM-64/67部分实现，APT-138-R1 PENDING。独立签名提交后才进入GSA-03。

GSA-03：以 Billing 为三类既有及八类 policy-only 命令建立唯一持久 activation current/revision 投影；三类前向物化为 ACTIVE、八类为 INACTIVE。Account 按精确 command/policy/snapshot version 强读闭合投影，Token 三命令仍只使用 Token 既有 activation/epoch，未建立第二真相。

- 迁移与只读边界：Billing `000054` 在同一事务维护 current 与不可变 revision history；state 变化必须逐一递增版本，current/history 的 DELETE/TRUNCATE、history 的 UPDATE 均失败关闭。`billing_service` 只能读取 current；新 workload API 固定返回按命令排序的11行 closed schema，任一缺行、乱序、未知、混合版本、policy-only ACTIVE、仓储错误或畸形时间均整份503且`no-store`。真实签名workload测试证明只有具有`internal.billing.approval-activation.read`的Account身份可读，缺精确权限或其他workload在仓储前拒绝。
- Account 边界：create/update/submit/APPROVE/新 Begin 按冻结 policy 与 snapshot schema 读取对应 activation；不可用与 INACTIVE 使用独立稳定503，历史 EXECUTING/EXECUTED 读取和 SUBMITTED REJECT/WITHDRAW 安全收口不依赖当前activation。Submit 在任何Billing远调前完成当前强授权及冻结binding/version/policy/schema校验；撤权、授权版本变化或snapshot schema漂移保持DRAFT与版本不变且Billing调用为0。单元表驱动分别覆盖三类Billing命令的ACTIVE/INACTIVE/缺行/错p/s，并确认八类policy-only继续在领域读取前禁止推进。
- 实际数据库验证：在Account、Token、Billing三个隔离数据库上运行的五组具名PG17用例均PASS/no-SKIP。Billing activation migration/runtime用例1.99s（package 2.739s），覆盖非superuser迁移、初始11行历史、状态修订、事务回滚无孤儿、运行时最小权限、重入与down历史保护；Account Billing集成边界用例1.57s（package 2.491s），使用真实Billing handler/HTTP client与真实Account PG repository，但Billing repository为受控stub，覆盖Wallet activation状态、撤权/版本/schema顺序、零副作用和历史安全读取，不冒称双库领域E2E或三Billing命令真实领域验收。既有三组回归为Advancement 2.14s、SnapshotLifecycle 11.34s、TokenConsumers 88.02s（生命周期40.59s/目录26.57s/凭据20.85s，package 102.570s），均未跳过。
- 合同与部署验证：AJV 2020-12真实编译并拒绝部分、额外、重复、乱序、错p/s、policy-only ACTIVE、未知命令和额外字段；授权覆盖计数及新路由精确权限测试通过。Compose和Helm模板静态解析证明仅Account获得新权限且原权限集合保留；本机没有Helm CLI，因此这项是模板解析证据，不冒称实际`helm template`渲染或部署证据。迁移manifest当前278文件校验通过，OpenAPI lint通过且仅保留9项既有warning。
- 审核迭代：B要求并由A补齐不可变revision history/current TRUNCATE封锁、store/handler共享服务端validator、真实workload allow/deny、Submit鉴权顺序、三Billing及八policy-only矩阵、发布/回滚顺序和OAS固定位置schema。首次完整verify只因新增路由后旧授权总数仍断言342而失败（其余550 PASS/2既有环境SKIP）；断言已按实际343更新并增加精确分类回归，未放宽授权checker。第二轮完整`node scripts/account-platform.mjs verify`最终exit0：Node 555项中553 PASS/0 FAIL/2项既有环境SKIP，Go test/vet、OpenAPI（9项既有warning）、i18n、observability、architecture、manifest 278、授权路由343及文档/任务门禁全部通过。B最终复审确认产品代码、迁移、接线与测试APPROVED，无阻断或重要问题。
- 状态：DONE / B审核APPROVED / NOT_DEPLOYED；Signed-off提交`0dd3d481`。APT-141继续IN_PROGRESS，ADM-67保持部分实现，其余GSA任务仍为PENDING；发布固定为Billing迁移→Billing服务→Account，回滚反序且保留`000054`事实。未交付proof、Account control、consumer改造、八类执行能力或任何生产activation。

GSA-04：实现尚未接线的唯一 `ADMIN_APPROVAL` control 持久模型、双 epoch 与窄事务锁接口；状态为 CODE_COMPLETE / B_CODE_REVIEW_PASS / IN_PROGRESS / ENV_PENDING / NOT_DEPLOYED，尚未通过最终环境验收。

- 最新审核收口：B 已复审凭据生成与显式九项数据库秘密映射修复，确认 Account migration 与 Keycloak 均被投递；开发环境示例、Compose/Helm 说明及精确十项 getenv 合同已补齐，暂无新增代码阻断。尚未获得最终 APPROVED；依用户 2026-09-15 顺序例外可继续后续设计/实现、逐项A/B审核与独立提交，但GSA-04自身仍不标记DONE。
- 环境欠账（均未执行）：隔离 Linux 上的实际 age 密码生成/恢复测试、实际 root-owned 凭据投影测试，以及 `python3 scripts/account-platform-postgres-fixture.py --run` 生产初始化链测试（含新增迁移身份 TLS/HBA 登录与 runtime 无法切换 owner/migrator）。三项均保持 ENV_PENDING，不由静态合同、手工 PG 迁移或用户顺序调整替代。
- 前两项命令分别为 `python3 -m unittest scripts.tests.test_dependency_passwords.PasswordTests.test_actual_encrypted_first_and_recovery` 和 `python3 -m unittest scripts.tests.test_dependency_projection.ProjectionTests.test_fixed_projection_and_failures`；必须按测试要求配置隔离 Linux 工具目录 `PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY`，后者使用隔离 root 环境，运行结果不得 SKIP。
- 最终本机完整验证：root 与 B 分别运行完整 `node scripts/account-platform.mjs verify`，均 exit 0；Node 555 项为 553 PASS / 0 FAIL / 2 既有 SKIP，Go/vet/OpenAPI（9 既有 warning）/三语/observability/architecture/manifest280/auth343/任务与文档门禁通过。此处 Node 既有跳过项与 Python 4 PASS / 2 适用 Linux-only SKIP 是不同证据，不能混同。
- 本机证据：六组真实 PostgreSQL 用例联合 PASS/no-SKIP（package 79.568s），覆盖新控制状态/权限/锁竞争/迁移失败，以及既有 Advancement、SnapshotLifecycle、Billing activation Account 边界和 Token consumers。凭据 Python 用例 4 PASS/2 Linux-only SKIP；生产 PostgreSQL 合同修正旧计数后 4/4 PASS。完整 verify 前次为 552 PASS/1 FAIL/2 SKIP，唯一合同失败为旧凭据数量断言；修复后的最终完整复跑结果见上。

- 当前候选：Account `000050` 仅初始化 `LEGACY/wire1/proof=false/RUNNING/approval_epoch1/relay_epoch1/version1`，数据库约束及守卫冻结四种mode元组、Relay正交状态、合法边、各自epoch/version递增、DB时间、singleton、no-op/跨组/删除/截断拒绝。Go包只提供caller-owned `pgx.Tx` 的普通读、固定SECURITY DEFINER共享锁和owner/migrator直接排他锁，以及8行typed动作分类；没有Transition/CAS写API，也未连接create/update/submit/APPROVE/Begin/Relay/consumer/recovery。
- 角色边界：受保护对象位于 `account_approval_control` 独立schema并由 `account_approval_control_owner NOLOGIN NOINHERIT` 持有；`account_migrator LOGIN NOINHERIT` 使用独立凭据并显式assume既有`account_service`及新owner角色。三角色高权属性必须全部关闭，runtime不属于owner/migrator且仅有表SELECT、schema USAGE与固定共享锁函数EXECUTE。现有`account_service`仍保留Account数据库owner的既有DoS边界；全Account角色收敛留待proof启用前的后续安全任务，不在本批扩大迁移范围。
- 迁移前置：fresh install创建新角色及独立迁移凭据；既有集群由DBA在受控窗口验证角色后，仅向`account_migrator`临时授予目标数据库CREATE。迁移成功时创建独立schema后立即撤销；迁移事务失败会回滚schema/ledger并恢复外部前置CREATE，失败runbook必须先撤销该临时授权再调查。随机schema旧fixture只机械改写`000050`的固定schema到唯一测试schema且精确检查命中次数；产品SQL保持固定schema，不接受测试注入。
- 尚未完成/不得宣称：身份B已通过代码审核，本机完整回归通过，三项隔离Linux环境验收仍待补；未交付capability API、proof、v2、Outbox sidecar、Relay/consumer接线、cutover/readiness、生产角色变更或activation。后续任务可按上述用户例外继续，但真实技术依赖与验收标准不变，受此环境欠账影响的下游须显式`DEPENDENCY_ENV_PENDING`。GSA-04已以Signed-off提交`0c9db1e2`保存当前证据，但状态仍为IN_PROGRESS/ENV_PENDING，不因提交而改为DONE。

GSA-05：先冻结typed closed RFC8785完整整数协议与legacy/新授权wire适配合同；当前状态为CONTRACT_B_APPROVED / CODE_COMPLETE / B_APPROVED / DONE / SIGNED_COMMIT `cad7f48f` / NOT_DEPLOYED。

- 用户裁决：旧业务存储、v1接口、历史摘要及`events.Event.Marshal/Unmarshal`保持不变；初始且尚未部署的`proof_schema_version="1"`直接使用完整精度表示。六命令closed `proof_input`中所有领域`int64`统一编码为规范十进制JSON字符串，只有逐命令常量`policy_version/snapshot_schema_version`保留闭合number。拒绝前导零、`+`、`-0`、指数、小数、空白与int64越界，解码后仍须执行逐字段正/非负/非0/null等业务约束。
- 独立代码交付范围：新增不依赖普通`json.Marshal`或Token专用canonicalizer的公共typed closed encoder/decoder；提供`LEGACY_TYPED_V1`只读兼容adapter、带显式`serialization_version="APPROVAL_RFC8785_DECIMAL_V1"`的新授权event closed adapter，以及只面向授权event原始bytes的dual decoder窄接口。新envelope的`aggregate_version`、payload中的`approval_control_epoch`和所有嵌套领域int64均为规范字符串；GSA-05只以纯typed `ExpectedAuthorizationBinding`（或等价命名）接收调用方给出的expected approved request version与approval epoch，golden验证wire值与外部expected不等时拒绝，不读取或冒充DB/Outbox/sidecar事实。`trace_id`只沿用当前非空且trim后非空约束，不虚构长度/字符集。不得按number/string或值大小猜协议，也不得parse失败fallback。本项不写Outbox、不发布消息、不接consumer，也不改变六命令业务资格。
- 后续所有权：GSA-06才在APPROVE同事务从锁定request/control读取expected binding、生成proof-era sidecar/new writer并原子写Outbox version/epoch与冻结bytes，以真实PG证明四者一致，同时保留legacy bytes；GSA-07只逐字节发布冻结delivery bytes；GSA-08必须新增或使用仅授权subject的raw-message consume seam，在当前`events.NATSBroker.ConsumeUntilResolved`调用generic `events.Unmarshal`之前调用GSA-05 dual decoder，且不得修改通用decoder或非授权消费，并由GSA-08/Begin按其合同重验绑定；旧consumer排空前不得投递新wire。`proof_digest`由Account对完整验证后的closed 11字段响应本地派生，领域不传第12字段；`command_fingerprint`单独校验，不能混称。
- 测试与完成条件：共享Go/JS raw UTF-8 golden覆盖六命令、int64最小/最大/0、小值和所有非法词法，JS必须以词法/BigInt处理，不得先转Number；Provider p2 golden必须使用生产兼容的32位lowercase `active_mapping_set_fingerprint`并证明64位替代值失败，其他fingerprint与新digest保持64位SHA-256。覆盖U+2028/U+2029、非BMP、控制字符、组合Unicode原样及duplicate/unknown/sensitive/64KiB±1。固定runner和Go版本以`-benchtime=100x -count=5 -benchmem`记录五轮合法最大向量与畸形64KiB输入的`ns/op,B/op,allocs/op`，首次结果经B审核后才成为回归基线；安全校验不得为性能数字删除。GSA-05只有实现、定向测试、适用完整verify、B代码PASS和独立Signed-off提交后才能DONE；当前文档审核不满足代码完成条件。
- 当前代码候选：新增`internal/platform/approvalencoding`窄包及共享golden，六命令legacy numeric事实经typed closed decoder转为完整精度十进制字符串；新授权wire使用显式判别与外部`ExpectedAuthorizationBinding`，legacy adapter逐字节复用`events.Event.Marshal`且仅接受epoch1。候选未接Outbox、Relay、consumer或领域proof；逐命令DENY reason白名单仍由GSA-13～15在领域响应校验中落实，公共`DeriveProofDigest`只校验closed transport及全局reason union后派生摘要，不冒充授权validator。
- 当前本机验证：新Go包非缓存全包PASS，独立Node Go/JS golden对拍5/5 PASS；最终产品候选完整`node scripts/account-platform.mjs verify` exit 0，Node 560项为558 PASS / 0 FAIL / 2既有SKIP，Go test/vet、i18n、observability、architecture及其余门禁通过。完整verify结束后在固定`go1.26.5 windows/amd64`、Intel Core i7-8700、12逻辑CPU，以`100x/count=5/benchmem`独立得到合法64KiB transport `ns/op=[369162,303479,294077,325614,465264]`（中位325614）、`B/op=[178535,178500,178458,178417,178405]`（中位178458）、`allocs/op=[984,985,984,984,984]`（中位984），冻结最差基线为`465264 ns/op、178535 B/op、985 allocs/op`；畸形64KiB `ns/op=[198469,282149,213439,217433,200248]`（中位213439）、`B/op=[145449,145451,145448,145448,145449]`（中位145449）、`allocs/op=565`，冻结最差基线为`282149 ns/op、145451 B/op、565 allocs/op`。合法样例是以尾随空白填充到64KiB的最大transport输入，不是最大业务语义payload或端到端SLO；后续只在同runner/Go版本按合同比较20%回退。

GSA-06：实现授权Outbox epoch/sidecar前向迁移与LEGACY N/N-1 writer兼容；当前状态为DESIGN_IN_REVIEW / DEPENDENCY_ENV_PENDING / NOT_DEPLOYED，不得开始代码或冒称已迁移。

- **现状与边界**：当前唯一授权append在`internal/account/approvals.go`的APPROVE分支，先把request CAS到`APPROVED/version+1`，再经`internal/platform/events.AppendOutbox`写`outbox_events`；后者由Go `json.Marshal(map)`写入`JSONB`，event id与DB `occurred_at`在append时产生，未返回持久行。当前通用Relay从Outbox列重新构造`events.Event`，非授权subject共用该能力。GSA-06只改变授权APPROVE writer、增加sidecar/backfill/readiness；不修改通用`events.Event.Marshal/Unmarshal`、非授权append/Relay、NATS consumer、proof或control mode。
- **迁移与所有权**：新增Account `000051` up/down：把`outbox_events`所有权从`account_service`转给独立`account_approval_authorization_owner NOLOGIN NOINHERIT`并新增`approval_control_epoch BIGINT NULL`；同一owner持有新的固定`account_approval_authorization`保护schema、一对一sidecar、守卫及受控函数，`account_migrator`仅为迁移成为owner成员，runtime/writer/backfill均不得成为owner成员。迁移仅回授`account_service`旧`AppendOutbox`使用的`INSERT(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)`、所需SELECT和Relay `UPDATE(attempts,next_attempt_at,locked_by,locked_until,published_at,dead_lettered_at,last_error)`，绝不授approval epoch、ALTER/TRIGGER/DROP/DELETE/TRUNCATE，因此N/N-1非授权append与旧Relay保持而runtime不能禁用守卫。sidecar字段/oneOf/64KiB与digest约束严格沿专项§9.1.1，更新/删除/截断及冲突覆盖全部拒绝。Outbox守卫对授权行采用同一字段白名单，event id/type、aggregate id/version、payload、trace、occurred_at和epoch不可变；授权DELETE拒绝，TRUNCATE在存在授权行时拒绝，非授权行仍强制epoch为NULL且既有append/delivery行为不变。epoch在INSERT后不可变，唯一例外是owner持有的受控backfill函数在锁定行和control后把既有授权行从NULL原子推进到1并同时写入核等sidecar；runtime无直接UPDATE epoch权限，函数拒绝非NULL、非1、非授权、sidecar冲突及不匹配control。兼容期不创建无条件sidecar-presence `NOT VALID`约束；初始即安装的deferred control-transition约束只在mode离开LEGACY的事务末扫描稳态一对一，因而LEGACY旧writer可暂缺sidecar而无需第二gate。down只撤尚未启用的兼容对象；已有epoch/sidecar或进入后续mode后一律拒绝破坏性回滚。
- **跨owner迁移链**：DBA前置以PostgreSQL 17角色选项创建低权角色，并仅授`account_approval_authorization_owner`给`account_migrator WITH SET TRUE, INHERIT FALSE, ADMIN TRUE`；migrator是该owner唯一最终member/admin，反向成员及service/writer/backfill/其他成员的SET/INHERIT/ADMIN全部拒绝。`000051`先以`session_user=account_migrator/current_user=account_service`复核角色属性、成员、现有Outbox owner/ACL与GSA-04 control owner。迁移事务内由migrator凭ADMIN临时`GRANT authorization_owner TO account_service`，并由当前public schema管理者临时`GRANT CREATE ON SCHEMA public TO authorization_owner`；仅在该窗口执行`ALTER TABLE public.outbox_events OWNER TO authorization_owner`，随后立即撤销service成员与owner的public CREATE并复核最终member/ACL，再由migrator的数据库CREATE前置创建`account_approval_authorization AUTHORIZATION owner`并撤销数据库CREATE。对象创建显式`SET ROLE authorization_owner`；跨GSA-04 control表的deferred trigger则显式`SET ROLE account_approval_control_owner`创建，其trigger函数只调用被精确授权的authorization validator。fresh/existing/reentry/down真实PG逐项核Outbox/sidecar/functions owner、migrator唯一最终member/admin、service不能SET ROLE、临时两项授权不可见且已回收；任一中途失败整个迁移和临时成员/schema ACL回滚。down在安全空状态才按同样受控窗口把Outbox owner/原ACL恢复给`account_service`，不得遗留交叉owner函数或放宽PUBLIC。
- **兼容trigger与权限**：授权INSERT trigger先在调用者事务通过GSA-04固定函数取得control `FOR SHARE`；epoch NULL仅在`schema1/LEGACY/wire1/proof=false/RUNNING/approval_epoch1`时填1，当前control version与relay epoch只要求正整数；显式epoch必须等于锁定control epoch且只有允许APPROVE的mode+RUNNING组合可写，错误/0/future值不得修正，非授权event必须NULL。trigger不生成Go sidecar、不提交、不另开事务、不局部重试。新增`account_approval_writer LOGIN NOINHERIT`和`account_approval_backfill LOGIN NOINHERIT`两份独立Secret/DSN，二者与新owner均固定关闭superuser/createdb/createrole/replication/bypassrls、不得成为service/migrator/control owner成员；所有受控函数核对`session_user`。新版本对APPROVE从writer专用pool开启**唯一一个**SERIALIZABLE事务：writer仅SELECT `identities/users/role_assignments/role_permissions/authorization_versions/approval_command_policies/approval_requests/approval_request_snapshots`，仅UPDATE `approval_requests`的批准状态列，仅INSERT `approval_decisions/approval_state_events`，并只EXECUTE版本化授权append函数；它不是DB/schema/table owner，无DDL、通用Outbox、历史backfill或Relay delivery权限。backfill身份只可调用有界candidate/readiness及NULL→epoch1+sidecar函数，不能决定request或写新Outbox。不得先在service pool提交request再用第二事务补sidecar；REJECT继续走既有事务且不依赖writer凭据。旧`account_service`仅在exact LEGACY/epoch1兼容trigger下可写授权subject，QUIESCING后即使仍有非授权通用Outbox INSERT也由owner守卫拒绝旧授权形状。生产切换须逐副本停旧版本、回收旧连接并取得旧/新/backfill身份的权限负向探针；`account_service`作为数据库owner仍可通过schema/database破坏造成既有可用性风险，但因不再拥有Outbox表而不能静默禁trigger或合法写入篡改授权事实；该GSA-04 Linux环境欠账仍传递为`DEPENDENCY_ENV_PENDING`，未补证前不得生产QUIESCING。
- **可用性与错误隔离**：入口健康检查已知专用writer配置缺失、凭据错误或连接/权限不可用时不得fallback到`account_service`，APPROVE在取得request锁或产生decision/state/Outbox前失败关闭为稳定503 `APPROVAL_WRITER_UNAVAILABLE`、`Retry-After: 5`、`Cache-Control: no-store`；同一能力快照中的`allowed_actions`不得展示APPROVE。Account进程可保持启动并服务REJECT、WITHDRAW及历史GET，且这些路径不得依赖writer pool；部署readiness/发布门禁单独暴露writer degraded事实并禁止新版本承接APPROVE流量，只有专用DSN、角色属性、连接与最小权限探针全部通过才开放APPROVE。事务中途撤权/断连等竞态按下一条函数安全合同整事务rollback并保持零提交副作用，commit未知只查证；任何分支都不得静默切回旧路径。
- **函数安全**：writer append、backfill candidate/write、readiness/final validator、Outbox/control trigger的每个固定signature都必须`SECURITY DEFINER SET search_path=pg_catalog`、只引用schema全限定对象并显式核对允许的`session_user`；全部先`REVOKE ALL ... FROM PUBLIC`，再只向对应writer/backfill或跨owner trigger owner授权该精确signature。control owner持有的deferred trigger函数只可调用authorization owner validator，不获得sidecar/Outbox通用DML；authorization owner validator也不得反向获得control UPDATE。入口健康检查已知缺配置/连接时可在取锁前返回上述503；若连接在事务中断开或权限被并发撤销，只能整事务rollback并返回同一失败关闭类别、零已提交副作用，不能承诺“锁前”、不能fallback；commit结果未知不重试并进入只读查证。
- **新LEGACY writer**：APPROVE事务必须先取得control共享锁，再取得request行锁并重验version、身份/权限、activation及既有批准条件；REJECT不读取control、不创建Outbox/sidecar且保持现有安全收口。授权专用account-owned helper持有调用者`pgx.Tx`，先确定唯一event id并取得同一事务DB时间，以INSERT RETURNING的event type/id/aggregate/version/JSONB payload/trace/occurred_at事实构造`events.Event`，调用GSA-05 `EncodeLegacyAuthorizationEvent`冻结`LEGACY_TYPED_V1/FROZEN_AT_APPROVAL` bytes及SHA-256；随后在同一事务写epoch与sidecar。`aggregate_version`必须等于CAS后的request version，epoch必须等于此前锁定control，sidecar event id/serialization/digest必须与持久Outbox逐字段核等；任一步失败回滚request、decision、state event、Outbox和sidecar。control仍为LEGACY时真实路径禁止生成proof-era wire，GSA-13 proof/gate前不得产生v2。
- **proof-era兼容边界**：`000051`和Account窄包在GSA-06交付并以合成锁定request/control事实测试未接线的proof-era typed writer接口；后续GSA-13只负责接入领域proof与开启实际批准分支，本项不得把四者一致验收整体推迟。该接口必须在Go中从锁定request/snapshot经GSA-05 typed转换生成decimal-string event payload/bytes，不能让数据库把request旧numeric JSONB与新wire JSONB直接比较、SQL重写JCS或只信调用方digest；DB只验证一对一、关联、不可变和bytes SHA-256。真实PG须回读Outbox/sidecar并以dual decoder、`ExpectedAuthorizationBinding`及六命令artifacts证明request version、aggregate version、control/Outbox/wire epoch、event id和语义一致；但当前真实APPROVE只调用LEGACY分支，不得产v2。
- **backfill与readiness**：新增受信、无payload日志的幂等批处理命令，按`(occurred_at,event_id)`有界高水位读取授权行并逐行锁定；从真实持久Outbox列构造`events.Event`并调用当前`Event.Marshal`。`published_at IS NULL`（包括DLQ）写epoch1及`RECONSTRUCTED_FOR_DELIVERY` bytes/digest；已published仅写`RECONSTRUCTED_AFTER_PUBLISH` semantic digest且禁止投递字段。已有sidecar必须逐字段、逐字节核等，禁止`ON CONFLICT DO NOTHING`掩盖漂移；单行失败整行回滚并仅输出event id、稳定reason/schema/version/脱敏digest。readiness只读报告缺失/错误epoch、缺失/重复/错provenance、bytes/digest漂移及不可重构行；它不切mode、不创建exclusion、不改原event id/payload/lease/published/dead-letter事实。
- **最终验证与事务重试**：GSA-06提供owner持有但不向runtime/backfill授予EXECUTE的`validate_authorization_outbox_for_quiescing`受控函数和caller-owned transaction Go seam；函数取得control `FOR UPDATE`、要求expected control version及LEGACY/RUNNING、在同一SERIALIZABLE快照重算全部epoch/sidecar/bytes/digest约束并返回closed计数/阻断摘要。未来GSA-09只能在该同一事务内按GSA-04合法边CAS为QUIESCING；deferred transition trigger在事务末再次扫描，CAS或扫描任一失败整体回滚。本项测试owner seam但不授生产cutover身份、不执行mode CAS。仅授权APPROVE完整SERIALIZABLE事务及该最终扫描/CAS闭包可对已证明事务回滚的SQLSTATE `40001/40P01`进行最多3次总尝试；decision id、approval state-event id和authorization event id在闭包外一次生成并各次复用，每次重新取得DB时间、control共享锁和request锁并重验全部事实，第2/3次使用可注入全抖动`0~25ms/0~50ms`。context取消/超时、commit结果未知、权限/约束/永久错误不重试；trigger、单条INSERT、sidecar写或backfill局部步骤不得各自重试。
- **文件归属与验收**：实现只落Account migration/manifest、`internal/account`授权writer/backfill/readiness/retry窄包、独立命令入口、Account角色/Secret/部署合同及对应测试；不修改`internal/platform/events`通用行为或GSA-05编码合同。真实PostgreSQL须覆盖fresh/reentry/安全down、旧writer trigger两种锁序与maintenance version3、显式错epoch、非授权零影响、新writer request/control/Outbox/sidecar原子绑定与rollback/crash、published/unpublished/DLQ backfill、幂等/冲突/高水位、不可重构阻断、sidecar及授权Outbox不可变、Relay delivery更新保留、全事务重试与取消；N/N-1负向探针须证明旧连接回收和权限边界。旧GSA-04随机schema fixture只显式截取`<=000050`并继续验证历史合同，另以独立数据库执行包含`000051`的完整生产迁移链。即使代码、真实PG、本机完整verify和B代码审核通过，受GSA-04三项Linux环境欠账影响仍只能标`CODE_COMPLETE / B_CODE_REVIEW_PASS / DEPENDENCY_ENV_PENDING / NOT_DEPLOYED`，不能DONE、activation或cutover。
