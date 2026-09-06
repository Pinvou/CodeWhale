# APT-122-R3：安全计量公式的单位键识别整改

## 需求与根因

APT-138 真实 PostgreSQL 与 HTTP 总验收发现，合法 `TOKEN_FORMULA_DRAFT_CREATE` 的 `required_unit_versions.INPUT_TOKEN` / `expression.INPUT_TOKEN` 被通用敏感字段扫描误判为认证 Token，预检返回 400。原公式领域已经支持 `INPUT_TOKEN`、`CACHED_INPUT_TOKEN`、`OUTPUT_TOKEN` 及六种安全参数映射；不得通过改单位名、放宽表达式执行、跳过 Billing 单位校验或关闭敏感扫描来通过测试。

本批为已提交 APT-122-R2 的独立窄整改，不混入未提交的 APT-138 接线。状态 `DONE / REMEDIATION_REVIEW_PASS`，由根代理独立签署提交；未统一推送或部署，不代表 APT-138 总装通过。

## 最小设计

- 保留全局 `sensitiveTokenGovernanceFieldName` 的 Token 后缀、密码、密钥、凭据等拒绝规则，不把 INPUT_TOKEN 等加入任意上下文的全局白名单。
- 只识别同一 JSON 对象中名称精确为 `required_unit_versions` 与 `expression` 的一对 map。该对 map 必须通过既有 `normalizeSafeFormula`：1～32 项、非零 UUID、同一单位键集合、六种既有安全参数映射。复用原单位 trim+uppercase 归一化，不引入新的单位定义或表达式语言。
- 在上述完整且安全的公式 map 内，三种既有计量单位名 `INPUT_TOKEN`、`CACHED_INPUT_TOKEN`、`OUTPUT_TOKEN` 可以作为单位键；其他单位仍按原递归敏感字段规则检查，其他所有兄弟字段、嵌套对象与数组继续扫描。不接受 access_token、api_key、任意 token 后缀字段、畸形 UUID、秘密字符串映射、缺另一半 map 或大小写字段别名伪装公式上下文。
- 同一窄规则适用于规范命令与前后状态投影中的完整公式对象，保证写入和结果审计不会互相矛盾。Billing 权威单位版本匹配、公式不可变版本、幂等键、事务与策略版本完全不变。

## 任务与验收

- 仅本说明、`command_governance.go` 的敏感扫描上下文分支、专属 `command_formula_metadata.go` 纯验证助手、专属 `command_formula_metadata_test.go`（单元与真实 PG）。按文档 B 审、实现 B 审、HEAD+本批隔离候选实测与独立签署提交顺序执行，不携带同文件的 APT-138 修改。
- 单元覆盖三个合法 Token 计量单位、普通非敏感单位、原单位归一化；孤立/伪造上下文、非法值/不匹配集合/嵌套 secret、access_token/api_key 等仍拒绝，原敏感字段测试保持通过。
- 实际 PostgreSQL + 原治理 HTTP handler 用合法单位完成协议草稿/发布、公式草稿和发布；预检、Billing 单位验证、通用命令、不可变公式版本、审计与 Outbox 保持闭环。原 `TestTokenProtocolFormulaGovernedCommandsPostgresIntegration` 同时回归提交响应丢失/原键恢复/异内容冲突，不替换合法单位名。不把 Billing fixture 当作真实 Billing 服务或把缺 PG 环境的 SKIP 计为通过。
- 新增具名精简 PG 测试只依赖已提交领域/helper；核心安全负例不得调用真实敏感服务，恶意字段在治理入口被拒且数据库效果不增。

## 独立审核记录

- 2026-09-06 A：完成误判根因和最小上下文识别方案，提交 B 文档审核；尚未编码。
- 2026-09-06 B：文档正式审核 PASS。A 已完成局部上下文分支、纯验证助手和单元/真实 PG 测试代码，尚未宣称实际测试通过，提交代码复审。
- 2026-09-06 B：独立候选边界复核发现已提交旧 HTTP helper 尚未包 CorrelationHandler；A 仅在本新测试外包正式 request-ID middleware，不携带 APT-138 的旧 helper 修订。四文件独立候选以新具名完整治理 PG 与原敏感字段单元验收；旧 `TestTokenProtocolFormulaGovernedCommandsPostgresIntegration` 的完整回归在包含已审核 fixture 修订的 APT-138 候选中另行报告，不将未执行或 baseline fixture 失败计为本批通过。
- 2026-09-06 根代理：隔离候选 `26b636bbeb636dc6f90a1b37306356f5cc0e946c` 仅含 HEAD 加本批四文件。`TestGovernedFormulaMeteringUnitPostgresIntegration` 真实 PG 全 PASS（2.42 秒），新上下文安全测试及原安全单元共四项均 PASS，Token 包 4.422 秒、退出码 0，无 SKIP。真实证据包括合法 INPUT_TOKEN 协议/公式草稿及发布、Billing fixture 权威校验、精确一次审计/Outbox、原键重放完整持久事实不变及恶意单位字段被拒且零效果。
- 2026-09-06 B：最终独立审核 PASS；局部三单位上下文、全局敏感规则不放宽及四文件独立候选证据均通过。仅本批可独立签署，旧大治理 PG 与 APT-138 剩余验收仍另行执行和报告。
