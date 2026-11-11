# 未建立执行记录的授权：人工恢复

本流程适用于 Billing proof 授权已经 grant、但 Account 没有执行记录的情况。
采用已确认的业务口径：按原授权复用案件并追加证据；取得原命令的权威最终
结果，或“动作已经结束且无业务效果”的完成证据后，由两名独立授权人员
复核收尾。查不到记录、当前查询为 ABSENT、请求到期或失效，都不能代替
完成证据。流程不重执行命令、不补造 execution，也不自动处置资金。

## 安装与发现

使用现有公开 Account 迁移入口安装前向迁移
`900023_approval_grant_manual_recovery`。它不依赖 900022 或其只读 CLI。
新 worker 必须与新迁移配套；迁移尚未安装时原 SLA 同步保持兼容，但新
恢复 API 不可用。down 明确拒绝，避免删除证据或移除执行围栏；恢复走向前修复。

已有 Account `manual-review-sla` worker 启动时和后续每分钟同步发现。
只有数据库实际执行期限已过、投递仍 DOMAIN_INFLIGHT、且没有任何 Account
execution 的原授权才会建案；不会在正常 grant→Begin 的有效窗口抢占主流程。
每批合计不超过原 SLA 同步上限，新发现最多 100 项、最多使用 5 秒调用预算；
被锁请求跳过，下一轮继续。每个 `(original_event_id,durable)` 只有一个案件，
并关联原有 P1 SLA、分配/升级及普通 manual-review outbox 事件。

案件一旦建立，数据库触发器永久阻止该原请求插入 execution，覆盖仍运行的
旧 Account 二进制；这只是执行围栏，不证明过去没有外部效果。发现前已合法
Begin 的请求继续使用原执行恢复合同，不会被误纳入本流程。请求后来被 expiry
标记 INVALIDATED，不能自动关闭本案件。

## 角色与证据

案件列表要求 `platform.billing.ledger.read`。详情允许该读权限或原命令的 requester /
approver 权限。追加观察允许原命令任一写权限，提出结案要求 requester 权限，
复核要求 approver 权限。所有写操作要求真人会话、15 分钟内近期认证。
复核者不能是本次提案者；复核时再次核验双方当前权限及提案者原绑定和授权
版本。权限撤销、绑定更换或授权版本变化后，旧提案不能继续，须重新提出。

证据真实性由两名授权人员分别核验。本系统保存其独立人工核验声明、受控
记录编号、原文字节 SHA256、原授权绑定和完成围栏编号，不自动获取、签名
认证或推断外部财务文件的真实性。不得把一次只读查询或自行生成的编号
冒充权威完成记录。原件和敏感交易内容继续保存在其受控权威系统中。

提案前，两人应能分别核对：原 event / request、批准版本、epoch、命令指纹，
原命令涉及的交易、主体、金额、币种和资金归属，以及权威系统已完成该动作、
不会再出现旧在途执行的证据。无法证明动作结束、资金处置不完整或证据矛盾时，
只追加观察，案件保持 OPEN。完成围栏编号必须指向该权威系统的完成/停止回执，
不能仅指向本系统的无 execution 记录。

允许的结案证据仅两类：

- `AUTHORITATIVE_FINAL_RESULT`，结果为 `SUCCEEDED` 或 `FAILED`：原命令的权威最终结果及完成围栏。
- `ACTION_ENDED_NO_EFFECT`，结果为 `NO_EFFECT`：动作已结束且无业务效果的权威完成证据及围栏。

`ABSENT`、`OBSERVED`、`UNAVAILABLE`、`EVIDENCE_CONFLICT` 只是观察状态，不能用于
提案结案。文件摘要采用 64 位小写十六进制 SHA256，编号采用 1–128 位
`A–Z a–z 0–9 . _ : -`（首位为字母或数字）。不接收原件正文、URL 或凭据。

## API 操作顺序

Account 入口为 `/admin/v1/approval-grant-recovery-cases`。Portal 的相同资源位于
`/api/admin/account/admin/v1/approval-grant-recovery-cases`，使用已有登录会话及
写请求 CSRF 合同；浏览器自行携带的 Authorization 不会取代会话凭据。
没有新增浏览器页面，也不需要手工操作数据库。

1. `GET` 列表：`?limit=25&after=CASE_UUID`；每页 1–100 项，按返回的 `next` 继续。
2. `GET /CASE_UUID` 读当前案件及操作记录；历史分页用 `?limit=25&after_version=N`。
   仅历史有下一页时返回 `next_version`。多页巡检不是全局快照，每轮应从首重扫。
3. `POST /CASE_UUID/observations` 追加观察；每次新的观察使当前提案失效。
4. `POST /CASE_UUID/proposals` 由第一人提交已核验的完整证据。
5. 第二人独立核对全部观察、权威原件及绑定后，`POST /CASE_UUID/reviews` 确认相同提案和摘要。

所有 POST 要求 `Content-Type: application/json`，不接受查询参数；顶层结构如下。
`operation_id` 是调用者事先保存的 UUID，`expected_version` 为刚读到的案件版本。

```json
{
  "operation_id": "11111111-1111-4111-8111-111111111111",
  "expected_version": 1,
  "evidence": {
    "state": "ABSENT",
    "reference": "CONTROLLED-QUERY-RECORD",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
}
```

上例仅说明 observations 的形状；编号和摘要必须替换为实际受控记录，不能拿示例结案。
proposals 的 `evidence` 字段如下，所有原授权字段须与案件精确一致：

```json
{
  "authority": "BILLING",
  "original_event_id": "22222222-2222-4222-8222-222222222222",
  "approval_request_id": "33333333-3333-4333-8333-333333333333",
  "command_fingerprint": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "approved_request_version": 3,
  "approval_epoch": 1,
  "kind": "ACTION_ENDED_NO_EFFECT",
  "outcome": "NO_EFFECT",
  "reference": "AUTHORITATIVE-COMPLETION-RECORD",
  "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "completion_fence_reference": "AUTHORITATIVE-ENDED-FENCE",
  "all_observations_resolved": true
}
```

reviews 的 `evidence` 为 `proposal_id`（提案操作的 operation_id）、相同 `sha256`，
以及第二人核验后设置的 `independently_verified: true`。这些声明是具名人工责任，
不能由未知结果恢复 worker 自动填充。新观察、旧版本、错对象、旧提案或不同摘要
会阻止旧复核。

收到网络超时或结果未知时，原样重发相同 operation_id、expected_version 和 evidence。
同键异内容会冲突；原样重放返回该操作的不可变原始回执，可能不是案件当前版本，
应另用 GET 读取当前状态。401 表示需重新认证，403 表示当前权限不足，409 表示
版本、绑定或证据冲突；均不能通过生成新 nonce 自动绕过业务门禁。

## 收尾与后续观察

双人确认在一个 Account 事务内持久化人工回执、审计、SLA 结案和原授权结束状态，
将该投递置为 RESULT_CONFIRMED，不增加领域尝试次数。原消费者之后才执行真实
DoubleAck 并登记 TERMINAL_CONFIRMED。NATS 不可用时保留 RESULT_CONFIRMED，等待原
投递恢复；不会以人工决定伪造 ACK。完成回执记录外部结果，不把缺失的 Account
execution 补成成功或失败，也不把原审批的 INVALIDATED 当成零资金效果。

结案后的新观察仍追加到同一案件并重新打开人工关注，旧完成回执保留不改。
已经发生的结果确认或 ACK 是历史事实，不回退成新执行权；原执行围栏始终保留。
若需要更正，应提交新的权威证据并再次双人复核，不能自动重执行或自动修复资金。
本流程阻断的是该案件的无证据结案和原授权执行，不替代其他命令的既有治理规则。

迁移会建索引并安装不可变证据与触发器，应按站点规模安排维护窗口。本地隔离
验证不代替生产升级、真实供应商证据验收或全副本 broker 演练。
