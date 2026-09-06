# 模型与 Token 策略字段前向整改

## 范围与依据

2026-09-06，APT-138 的独立十九项一致性审核发现两项领域政策描述落后于已经实现的严格输入合同。APT-138 按既定边界暂停接线，保留其未提交改动；本文件单独承载前置整改的需求、设计、任务与独立审核记录，不把领域修订混入激活提交。

| 批次 | 真实差异 | 前向版本 | 唯一迁移与范围 |
|---|---|---|---|
| APT-123-R3 | `TOKEN_ACCOUNT_CREATE` 的 v2 policy 缺少实际 spec 中的 owner_version、owner_identity_digest、owner_account_count、owner_account_cap、owner_account_cap_policy_version | policy v3 / snapshot schema v3 | Account 000048；创建政策、人工恢复当前政策校验及版本兼容测试 |
| APT-125-R2 | `TOKEN_ROUTE_UPSERT` 的 v2 policy 仍使用 protocol_code，实际 spec/API 使用 protocol_version_id | policy v3 / snapshot schema v3 | Account 000049；UPSERT 政策及版本兼容测试 |

`TOKEN_ACCOUNT_STATUS_CHANGE` 不在修订范围：已复核 registry 与已发布 Account 000045 第 82 行，v2 的有序字段均为 `token_account_id, expected_account_version, target_status, precheck_fingerprint, request_fingerprint`，与实际 spec 一致。保持 v2，仅增加精确回归；不因先前误判而无必要升级。

## 需求与不变量

- 两项政策的 `critical_resource_fields` 必须逐字段、按顺序与 Token 的真实 `tokenCommandSpecs` 一致，不能只比较计数、首权限或 DB 与代码交集。
- 创建仍为 AUDIT_ONLY，当前用户必须同时具备 manage 与 owner-candidates.read；政策的 requester_permission 单字段仍为 manage，双权限由已实现的应用授权合同保证。不新增审批、权限、授权版本或 owner cap policy 版本。
- 只追加新版本；不修改已发布的 000045，不更新/删除原 v1/v2 政策、既有 command/result/audit/Outbox、reservation 或审批快照。
- APT-123-R3 独立实现、审核并提交后才开始 APT-125-R2；后者独立通过并提交后恢复 APT-138。两个整改都不安装激活、开启 worker 或推送部署。

## 前向设计与恢复兼容

1. Account 000048 只追加 CREATE v3；000049 只追加 UPSERT v3。两迁移显式声明完整政策字段，冲突时不覆盖既有行，并断言已存在的同版本必须逐字段等价，否则失败关闭。其他风险、期限、执行身份与旧版保持不变。down 保留追加政策和所有引用历史，只回退迁移标记，不能通过删除政策伪造回滚；旧应用不得在政策不一致时激活新命令。
2. CREATE v3 字段顺序为 `owner_type, owner_id, owner_authorization_version, owner_version, owner_identity_digest, owner_account_count, owner_account_cap, owner_account_cap_policy_version, creation_version, precheck_fingerprint, request_fingerprint`；UPSERT v3 为 `token_account_id, expected_account_version, expected_route_version, model_id, protocol_version_id, precheck_fingerprint, request_fingerprint`。STATUS_CHANGE 的五字段与 v2 完全不动。
3. 输入 envelope 不包含 policy version。已复核 `normalizeTokenCommand`：幂等指纹绑定 canonical envelope、key digest 与可选 approval fingerprint，不包含 registry policy version。因此同一完整 payload 的历史 v2 command 在升级后必须 existing-first 返回原 v2 reference/result，而不是重写为 v3；新 key 才持久化 v3。其他 payload 仍 409；旧 reference GET 与 actor/type/key GET 保留原政策版本和结果，不能以重新预检或权威查询覆盖历史。
4. `account_provisioning_saga.go` 的人工恢复原本固定当前 CREATE policy v2，改为验证当前 v3 的 AUDIT_ONLY、schema 及完整字段。目标原 command 即使为 v2，只要仍符合既有 MANUAL_REVIEW、权限、authority、epoch 与 CAS 合同，仍可依法恢复；不改写其冻结 policy/result。这里的 command policy 版本不能与 owner cap policy 的独立修订号混淆。
5. 版本影响扫描包括 registry、Token parser/Begin/complete/GET、manual recovery、审批政策 DB、OpenAPI、Portal result validator、Node/Ajv 合同与 PG fixture。通用结果 OpenAPI 当前为 `policy_version >= 1`，如客户端仍有 v2 硬编码则同所属批次移除并补 v2/v3 接受与畸形版本拒绝测试；不改无关生命周期/目录回滚的固定 v2。

## 串行任务与验收

### APT-123-R3

- 状态：DONE / REMEDIATION_REVIEW_PASS；隔离候选树真实验证与 B 最终独立审核已通过，等待根代理独立签署提交。
- 文件所有权：本文件、Account 000048 up/down 与 migration manifest、registry 中 CREATE 一条、manual recovery 的当前政策校验、独立 CREATE/STATUS 精确单元与真实 PG 版本兼容测试；不带 APT-138 的 store/reader/descriptor/worker 等代码。
- 验收：真实迁移前 v2 行快照与迁移后逐字一致；新 v3 与实际十一字段相等；重跑等价，冲突政策失败；down 后 v2/v3 与历史引用仍存在。真实旧 v2 command 的 GET/同 key 同 payload POST 重放原 reference/result、零第二个领域效果，新 key 使用 v3，异 payload 冲突。v2 MANUAL_REVIEW 在当前 v3 政策下可恢复且仍受原双权限/authority/CAS 约束。STATUS v2 五字段精确不变。B 循环审核通过后独立提交。

### APT-125-R2

- 状态：PENDING；依赖 APT-123-R3 已独立提交。
- 文件所有权：本文件后续记录、Account 000049 up/down 与 migration manifest、registry 中 UPSERT 一条、独立路由政策/旧 v2 reference 与幂等重放真实 PG 测试。
- 验收：v2 原 protocol_code 描述原样保留；新 v3 七字段精确匹配 protocol_version_id；down 保留两版/引用，旧 v2 成功路由结果 GET/同 key 重放保持原 reference/version、零第二次 route mutation，新命令 v3；不放宽真实 parser 接受旧文本模型/协议。B 循环审核通过后独立提交，再恢复 APT-138 严格总门禁。

## 独立审核记录

- 2026-09-06 A：根据真实代码/迁移复核将最初三项候选差异收敛为 CREATE 与 UPSERT 两项，排除 STATUS_CHANGE；提交本文件供 B 审核，尚未编写本整改代码。
- 2026-09-06 B：正式文档 PASS，确认 STATUS 原 v2 正确，先实施 APT-123-R3，独立提交后再实施 APT-125-R2；测试不得依赖未提交的 APT-138 接线。
- 2026-09-06 A：APT-123-R3 已追加 000048/CREATE v3 及完整人工恢复政策校验，新增政策单元、真实 PG 前向/重跑/冲突/down 历史保留测试，以及真实三服务已存 v2 command 的 GET/POST 重放、异 payload 冲突和 MANUAL_REVIEW 恢复测试。历史兼容测试合法插入冻结 v2 fixture，不禁用触发器、不改写 v3 既有结果，也不声称运行了旧二进制；最终证据等待仅包含 HEAD+本整改的隔离候选树验证。
- 2026-09-06 A / 根代理实测：仅含 HEAD+本批十个文件的隔离候选树完成验证，未混入 APT-138。Account CREATE/STATUS 精确单元与 000048 真实 PG 前向、等价重跑、冲突拒绝、down 历史保留通过（迁移用例 36.55s）；最终候选 `a210f61c` 的旧 v2 三服务恢复整链通过（13.83s），当前完整恢复政策单元通过，Token 包合计 15.792s、退出 0，无 SKIP。Migration manifest 266 项、architecture guard 与 diffcheck 均通过。
- 2026-09-06 A：真实验证纠正两项 fixture 预期，不修改生产合同：首次人工恢复正式返回 202，原测试误只接受 200/201，现严格断言 202；合法插入的历史 fixture 不含初始提交事件，不能复用完整新提交流程的聚合计数。现精确校验 CREATED、PROVISIONING_COMPLETED、PROVISIONING_RECOVERY 三类审计各一条，完成事件按 payload command_reference、恢复事件按 aggregate_id 分别一条，成功 Billing 回执一条；重放前后完整持久证据不变。相关 UUID SQL 参数显式先转 uuid 再转 text，避免首次文本推断与 UUID 比较冲突。
- 2026-09-06 B：APT-123-R3 最终独立审核 PASS；代码、隔离提交边界、修订后的精确事实断言和真实 PG 证据均通过，可独立提交。本结论不代表 APT-125-R2 或 APT-138 完成。
