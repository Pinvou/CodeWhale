# APT-134-R1：生命周期计划分页列别名修复

## 需求与根因

真实 PostgreSQL 与 HTTP 验收发现生命周期计划列表持续返回分页 503。`lifecyclePlanSnapshotSource` 的查询选择了 `plan.id::text` 和 `row_number()-1`，却没有通用快照写入器要求的 `resource_id`、`position` 别名；外层 `adminSnapshotInsert` 引用 `source.resource_id` / `source.position` 时返回 SQLSTATE 42703。

根代理已经静态核查全部 16 处 snapshot source 调用，只有生命周期计划缺这两个别名；此结论是同类静态排查，不代表所有资源的真实 PG 均通过。本批独立于尚未完成的 APT-138 总装，状态 `DONE / REMEDIATION_REVIEW_PASS`。

## 最小设计

- 只为原 SELECT 两个表达式补充 `AS resource_id` 与 `AS position`，不改变模型范围、状态/目标生命周期筛选、`plan_version DESC,id DESC` 排序、零起始位置、分页限制或快照数量上限。
- 保持原服务端快照分页、签名 cursor、用户/权限版本/模型范围/过滤条件绑定及到期机制；不改成内存切片，不绕过数据库快照，不放宽权限或非法参数检查。
- 不修改领域计划状态、任务执行、审批、worker、迁移或 APT-138 epoch guard。读取产生既有分页快照元数据，不得产生计划变更或领域审计效果。

## 单任务与验收

- `APT-134-R1` 仅包含本文、`internal/token/lifecycle_plans.go` 唯一 source SQL、专属 `internal/token/lifecycle_plan_pagination_integration_test.go`。文档 B 审核通过后编码；代码 B 审核及 HEAD+本三文件隔离候选真实 PG 通过后独立签署提交。
- 新测试使用实际 PostgreSQL repository 和正式列表 HTTP handler；显式包正式 request-ID middleware，不依赖未提交的旧 helper 修订。fixture 自行建立符合现有约束的历史计划，不禁用约束或改状态矩阵。
- 覆盖多页排序与无重复/遗漏、末页 cursor/has_more、按状态与目标生命周期组合筛选、其他模型隔离、空结果、非法筛选与 cursor 绑定拒绝。
- 第一页之后新增计划，旧 cursor 继续沿原成员快照；重复 cursor 返回相同成员/顺序/快照时间，新查询才看到新增计划。业务记录读取前后保持不变（新增测试种子除外），不把分页快照元数据写入误当业务变更。
- 以具名 `TestLifecyclePlanPaginationPostgresIntegration` 真实 PG 验收，不把 SKIP 计通过；旧大生命周期 PG 在 APT-138 已审 fixture 整合候选中另行回归，不混入本三文件或提前宣称通过。

## 独立审核记录

- 2026-09-06 A：接收根代理真实 42703 证据与 16 处同类静态排查，完成上述最小文档，提交 B 审核；尚未编码。
- 2026-09-06 B：文档审核 PASS；两处别名及三文件独立边界符合根因，不改变领域行为。
- 2026-09-06 A：两处别名与独立 HTTP/PG 回归测试已编写，已运行 gofmt 与 diffcheck；等待 B 代码审核和根代理隔离候选的实际 PG，尚未宣称测试通过。
- 2026-09-06 B：代码静态审核 PASS，确认三文件边界与测试断言符合已批准设计。
- 2026-09-06 根代理：从 HEAD 加本批三文件导出隔离候选 `373824cdea468c87502bcad261287ab79391bc1e`，编译成功；具名 `TestLifecyclePlanPaginationPostgresIntegration` 真实 PostgreSQL 与 HTTP 回归 PASS，用例 2.38 秒，exit 0，无 SKIP。多页、组合筛选、冻结游标、非法输入及领域/审计/Outbox 完整事实不变均通过；不包含未提交 APT-138 guard 或旧大生命周期测试修订。
- 2026-09-06 B：结合隔离候选实测给出最终独立审核 PASS。本整改完成，交根代理独立签署；不代表 APT-138 总验收完成。
