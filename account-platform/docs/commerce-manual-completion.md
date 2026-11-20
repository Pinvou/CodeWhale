# Commerce 客户同步人工收尾

本流程落实 #1632 已确认的收尾规则。案件以原任务、供应商及适配器版本绑定，保留 mapping 代次、主体生命周期和原尝试次数；同一任务重复发现复用原案件。原件留在受控系统，Account 只保存记录编号、SHA256、独立人员的核验与最小脱敏回调。`FINANCIAL` 任务不能使用客户同步证明结案。

## 安装和入口

先暂停旧 Commerce worker，使用现有 `cmd/migrate`、真实 `account_migrator` 登录及 `account_service` 当前角色安装 `900025_commerce_manual_completion`，再启动新版本 Account 与 worker。迁移双向身份固定，Down 返回 55000；恢复采用向前修复，保留案件、回执和历史 SQL。DDL 会锁定相关表，需按站点规模安排维护窗口。未验证混用新旧 worker 的滚动发布，禁止据本地验收宣称现场就绪。

新 worker 将耗尽重试与过期租约导入人工队列；过期本身不证明供应商调用结束或没有效果。SLA worker 也会发现历史记录；既有 SLA 升级先执行，随后商城扫描跳过正被占用的主体/映射/任务，每批最多使用 5 秒，下次扫描重试忙任务。人工登记可冻结已有 RETRY，或为已经取消的历史调用建立仅保留证据的案件。首次尚未执行的 PENDING、正常已完成任务和资金任务不进入本流程。案件链接建立后不再取得原任务的派发权。

公开合同：[commerce-manual-completion.json](../contracts/openapi/commerce-manual-completion.json)。入口在受控 Account 服务，使用真实人类 OIDC 会话；本次不新增 Portal 代理或图形页面。

| 操作 | Account 路径 | 平台权限 |
|---|---|---|
| 有界盘点 | `GET /admin/v1/commerce-repairs?limit=50&after_id=<上页末项ID>` | `platform.commerce.repairs.read` |
| 案件、观察及回执 | `GET /admin/v1/commerce-repairs/{case_id}` | 同上 |
| 按原任务登记/定位案件 | `POST /admin/v1/commerce-repairs` | `platform.commerce.repairs.request` |
| 追加受控证据提案 | `POST /admin/v1/commerce-repairs/{case_id}/observations` | 同上 |
| 独立复核并收尾 | `POST /admin/v1/commerce-repairs/{case_id}/actions/confirm` | `platform.commerce.repairs.approve` |

`PLATFORM_COMMERCE_OPERATOR` 提供读取与提案权限，`PLATFORM_COMMERCE_REVIEWER` 提供读取与复核权限。通过既有平台角色管理分配；超级管理员虽拥有两项权限，也不能复核自己的提案。审计员只读。复核时重新锁定并验证两人的当前权限、提案人原授权绑定及版本，锁等待后的数据库时钟会使已到期的授权失效。

列表每页最多 100 项；用末项 ID 继续分页，不能把分页当成增量变更订阅。详情中人工观察与系统回调分别最多 100 项，用末项 `case_version` 的 `after_observation_version`、末项 `sequence` 的 `after_callback_sequence` 继续读取，直到对应 `more_*` 为 false。回调顺序由数据库序列确定，不依赖多实例墙钟。

## 证据和双人核验

登记正文仅包含原 `job_id`。以下 UUID 和摘要是示例，执行前替换为真实原任务及受控记录；不要上传原件、供应商凭据、手机号或未脱敏客户编号。

```json
{"job_id":"11111111-1111-4111-8111-111111111111"}
```

1. 提案人先读取完整案件和所有新增回调，核对原任务、主体、mapping 版本、生命周期、供应商、适配器版本及全部尝试。供应商客户“存在”或查询“未找到”，均不能单独证明原调用结果。
2. 成功结案须有与原绑定相符的权威成功记录，并证明全部原尝试已结束。无效果结案须证明全部原调用已结束且没有外部效果，不能以超时、租约过期或未查到记录代替。无法建立历史关联时继续 UNCONFIRMED。
3. 将完整核验材料保留在受控系统，以限定字符的记录编号及原件 SHA256 关联。提案是人类对该材料的声明，系统不会把编号、摘要或自由输入伪装成供应商机器签名。
4. 另一名独立授权人员读取原件及当前案件，核对同一原绑定和最终结果后，以准确提案 ID、版本提交复核。缺失、矛盾或仍在途的证据继续阻断。

成功提案示例：

```json
{"expected_version":1,"observation_id":"22222222-2222-4222-8222-222222222222","outcome":"SUCCEEDED","evidence_reference":"controlled-record-0001","evidence_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","all_attempts_ended":true,"external_reference_redacted":"***1234"}
```

无效果使用 `ENDED_NO_EFFECT`、`all_attempts_ended:true` 和空的外部引用；已注销或退役生命周期的成功提案也须省略外部引用，只关联受控记录及摘要。未核实使用 `UNCONFIRMED`，不能复核结案。所有新观察都保留，提案变更使用新的观察 ID 与当前版本。同一观察 ID 仅允许相同原输入重放，不能借重试改写材料。请求必须为 `application/json`，正文不超过 8 KiB；重复字段、大小写别名、UUID 别名、未知字段、null 和额外查询参数均拒绝。

复核正文使用观察响应实际返回的 `case_version`：

```json
{"expected_version":2,"observation_id":"22222222-2222-4222-8222-222222222222"}
```

## 结果、迟到回调和恢复

| 回执 resolution | 业务效果 |
|---|---|
| `SUCCEEDED_CURRENT` | 仅仍 ACTIVE、生命周期与 mapping 代次均匹配的原任务可更新 READY；原任务完成。 |
| `SUCCEEDED_EVIDENCE_ONLY` | 旧代次、已注销或不可接受当前结果的主体仅保留证据，不更新其原业务字段或当前映射。 |
| `ENDED_NO_EFFECT` | 当前任务记录 `MANUAL_NO_EFFECT`，映射继续 REPAIR_REQUIRED；旧代次/已取消任务保留原业务字段。没有同步成功，也没有新派发。 |

人工收尾不会调用供应商或发放重新执行权限。启动时更换适配器也不会自动重建被人工案件冻结的当前代次。后续重新执行须另行人工授权，不能把原任务改回 PENDING/RETRY、复用旧租约或把本次无效果回执当作执行授权；新的业务意图继续由其既有生产者和授权合同处理。

在途 Complete/Retry 结果可追加到独立、不可变的回调证据中，保留原尝试编号并去重。注销/退役生命周期不保存新的外部引用、手机号或供应商错误正文。回调返回冲突表示未获得业务完成权，不能据此再次执行供应商调用。开放案件收到新的回调证据会推进版本，使之前的人工提案失效；重新读取并核实后才能提出新提案。已确认回执保持不可变，后到回调标注 `after_completion`；若其与权威完成证据矛盾，应在原受控记录中登记调查并保留原案件关联，不自动改写回执、恢复身份或重执行。

人工提案、复核和对应业务状态、SLA 案件事件及 `commerce.repair.*` 审计在同一事务提交。系统回调通过 `COMMERCE_CALLBACK_EVIDENCE_RETAINED` 案件事件留痕；SLA 创建事件复用既有 Account outbox/relay。worker 存储失败使用固定错误码日志，不输出供应商错误正文。

HTTP 断连、500/503 或重放时的 401/403 都不能证明原操作未提交。保留原案件、观察 ID、输入和版本，重新取得权限后 GET 原案件；已确认时可用同一输入重读原复核回执。不要另建任务、替换原观察 ID 重做未知操作或重复调用供应商。

本迁移不自动删除任务、案件、观察、回调、回执或审计；按既有备份和受控证据保留政策保存。隔离 PostgreSQL 验收使用真实公开迁移和实际运行/迁移角色，供应商结果与历史材料是受控测试事实；不代表已完成真实商户、供应商环境或生产部署验收。

这里的“不可变”保证限于受支持的应用入口与已安装的行级守卫：API 校验双人权限、原绑定及版本并生成应用审计；观察、回调的 UPDATE/DELETE 和已结案案件的更新、删除由行触发器拒绝。它不是针对持有表所有权会话的完整防篡改承诺：直接 INSERT、TRUNCATE 或 DDL 不经过这些应用校验或行触发器，应用审计也不会自动覆盖这些直连操作。运维须限制数据库直连权限，并独立落实数据库审计、维护审批与备份留存；不能把应用回执当作数据库管理员无法改写数据的证明。
