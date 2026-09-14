# 治理与安全整改实施任务

> 需求基线：`e15fe698`，身份 B 最终需求审核 PASS。
> 本任务拆分：身份 B 复审 PASS，提交`90b2ea08`；GSA-01 DONE / B审核PASS并以`88d37a53`提交，GSA-02 DESIGN DONE / B审核APPROVED，其余PENDING，均NOT_DEPLOYED。

## 1. 所有权与执行规则

需求合同唯一引用 [整改设计](admin-governance-security-redesign.md)，不在任务表重新定义协议。主目标唯一归属：APT-141 拥有 ADM-67，APT-142 拥有 UI-60，APT-143 拥有 NFR-57。以下 GSA 编号是可独立审核、测试、提交的执行子任务；主任务作为汇总，不能以一个大提交替代子任务交付。

逐项执行：A 实现及自检 → B 独立审核 → A 修正 → B PASS → Signed-off 独立提交 → 更新证据 → 下一项。B 未通过不得进入下一执行子任务。每次提交只能交付一个 GSA 子任务；设计子任务同样必须审核并独立提交。需求取舍、未经确认的财务规则和互斥方案提交用户决定。

每项必须记录实际文件、测试命令/结果、B 结论、提交、已知差距。代码任务 DONE 要求真实行为测试及适用数据库集成测试通过，不能用 SKIP 或静态字符串断言代替；有代码完成但缺环境证据时保持 IN_PROGRESS 并记录 CODE_COMPLETE/ENV_PENDING，不关闭主目标。所有产品代码提交执行 `node scripts/account-platform.mjs verify` 和任务定向门禁；文档步骤执行任务覆盖与文档基线检查。只在改动或新失败需要时重跑已经通过的检查。

DESIGN 指事实追踪或领域设计：完成代码依据、版本/schema、不变量、验收映射、文档检查、B PASS及独立提交即可DONE，不要求尚未实现的行为测试，不表示功能已实现。CODE 指仓库实现及本地/隔离环境验证；CODE+ENV 额外要求生产等价环境或部署证据。此次提交授权不等于生产部署/激活授权；发布任务先完成工具与隔离演练，实际生产操作单独取得授权。所有子任务在此文档内顺序执行，表内依赖是最低技术依赖，不能以没有依赖为由并行实现。

## 2. 独立执行队列

所有条目初始审核为 NOT_REVIEWED、提交为无；状态只能按证据逐项更新。

| 子任务 | 主任务 | 独立交付 | 技术依赖 | 类型 | 状态 |
|---|---|---|---|---|---|
| GSA-01 | APT-141 | policy-only 推进门禁与停用后的撤回/拒绝 | APT-008, APT-138 | CODE | DONE |
| GSA-02 | APT-141 | 逐命令现状、策略/快照/接口兼容矩阵 | GSA-01 | DESIGN | DONE |
| GSA-03 | APT-141 | Billing owner-domain activation 投影及既有三命令兼容 | GSA-02 | CODE | PENDING |
| GSA-04 | APT-141 | Account control 模型、持久状态和事务锁接口 | GSA-03 | CODE | PENDING |
| GSA-05 | APT-141 | typed closed RFC8785 编码与 legacy 编码适配 | GSA-02 | CODE | PENDING |
| GSA-06 | APT-141 | 授权 Outbox epoch/sidecar 前向迁移及旧 writer 兼容 | GSA-04, GSA-05 | CODE | PENDING |
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
- **03**：当前 Billing 尚无持久 activation 行，三类既有命令以静态`Executable=true`运行、八类以静态`Executable=false`失败关闭；本任务才由 Billing 建立其权威投影，前向迁移保全三命令 ACTIVE、物化八命令 INACTIVE。Account 只消费该投影，Token 三命令继续复用 Token 既有状态。迁移幂等、缺行/未知版本/不可用失败关闭及 N/N-1 回归；无实际生产激活。
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
