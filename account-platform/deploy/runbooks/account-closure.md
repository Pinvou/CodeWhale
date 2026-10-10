# 自助注销与隐私清理（F08）

资料变更提交与注销准备统一先锁用户、再锁资料请求。已提交请求仍按原 ID 只读重放；CLOSING/DELETED/ANONYMIZED 或已取消但尚未提交的请求保留冲突及身份已确认事实，由注销 Prepare 接管。重试不会清除 identity_updated_at；不能把未确认的身份变更当成功提交。

此前冻结版本的 main Go 模块默认测试及 vet 均实际 EXIT0；相邻传播后的验证单独记录，不沿用此前结果。显式数据库配置缺失的集成测试仍 Skip，不以默认测试代替下述数据库专项；本地 Windows 未执行 race，生产进程关停、真实 Keycloak/NATS 和线上部署仍未验收。

本轮 Commerce 正式角色数据库回归三项共六个子例通过（25.087s），旧 bootstrap/startup/disabled 三子例通过（10.720s）。资料变更正式无 overlay 五子例通过（41.402s），覆盖 ACTIVE、原 DISABLED 行为、USER 先于 profile 的锁等待、已确认 RETRY 由 Prepare 接管及未确认取消不写回。此前受控 TEMP overlay 仅暂停原提交路径以安排并发；释放调度锁后原 USER/profile 反序返回 40P01，证明锁序缺口，不证明生产个人信息泄露。Commerce 首轮闭环失败及修复中两项夹具/返回契约失败日志均保留；不以未执行子例的 no-tests-to-run 当通过。

当前分支承接前序注册、赠额资格、财务人工核实和永久重放规则。注销不自动退钱、消除应收或解除未知财务冻结。个人请求需明确确认和近期认证；管理员只能读取队列、以原理由和原幂等键重试阶段，不能跳过阻塞或恢复匿名身份。

个人 GET `/v1/me/account-closure-request`，POST `/v1/me/account-closure-requests`，确认值 `CLOSE_MY_ACCOUNT`；管理员 GET `/admin/v1/account-closure-requests`，POST `/admin/v1/account-closure-requests/{id}/actions/retry`。权限为 `personal.account.close.read/request` 和 `platform.account-closures.read/repair`。同主体、同键、同事实可恢复请求；更换键、GET 状态、401/403 或超时不能证明本次写入成功或没有提交。CLOSING 仅保留注销读/请求权限；匿名化后若原身份已删除，保留未知凭证并联系管理员，不借新身份自动补写。

生产 Account 接注销 worker，复用动态 workload token。精确接收权限：Account→Identity `internal.identity.user.delete`；Account→Billing `internal.billing.account-closure.assess/isolate`；Account→Token `internal.token.account-closure.assess/replay-clear`；Billing→Account `internal.account.closure.read`；Token→Billing `internal.billing.closed-grant.reclaim`。保留原内部 HTTP 服务网络隔离、外部 HTTPS 和 JWT/JWKS/CA 合同；测试静态 Bearer 不是生产身份。先正式迁移及更新接收规则，再发布同源服务镜像和 Portal 产物。前序 F07 HMAC、资格 backfill 和 readiness 必须保持，不能由旧钱包 worker 激活 EVALUATE 请求。

Account 027/028、赠额 087、钱包索引 093 和前序 Token 硬化 SQL 保持原字节。F08 Token 清理围栏使用独立 900004 身份，避免与已发布 900001 硬化碰撞。正式 Ordered/Up/Verify 保持原角色边界；永久事实的 forward-only Down 拒绝回滚。

worker 外呼前续约仍有效的同版本 claim，行锁等待后再查数据库时钟；不持行锁跨 RPC。删除、隔离、清理需绑定原主体和生命周期的权威回执。丢 ACK 按原阶段恢复。SIGTERM 取消 worker 上下文、停止新领取，并在关闭数据库池前最多等待注销 worker 十秒；不代表所有旧 worker 生命周期已重构。

匿名范围限于可证明属于主体的可变账号、身份、注册/密码重置/资料变更联系方式、关联短信挑战、Commerce 映射、付款联系人及可清理重放内容。金额、来源、永久赠额资格、财务回执、审计及 outbox 保留。只有手机号匹配的旧 slider 或未绑定挑战无法证明历史属主，不猜测清除；已接受/撤销邀请终态不破坏不可变证据。短信风险摘要没有手机号明文，但单向摘要不等于不可关联或不可识别，仍遵循原保留策略。既有受控 replay retention case 加密内容和法律保全不自动实体删除，永久围栏阻止清理后新复制/新重放内容。

`account_subject_retention_records` 到期 `PURGED` 仅是元数据标记，不代表所有财务或加密实体已销毁。实体销毁、法律保全及不能证明属主的数据治理见 [Issue #1663](https://github.com/Pinvou/pinvou3/issues/1663)。不得直接修改账本、资格或匿名标记来完成注销。

Commerce 的 USER enqueue、claim、complete、retry 与本地清理按用户→mapping→job 锁序串行化；已关闭主体或旧生命周期的晚回执不回写本地联系人、外部引用或证据。金融任务未权威完成仍阻止清理。非金融任务的 claim 事务提交后，供应商 EnsureCustomer 可能已在途；当前通用 adapter 没有供应商删除或取消回执，本地 noop 不证明外部客户已删除或供应商未留存联系方式。这一外部保留边界需按 Issue #1663 的治理方案核实，不伪造外部 ACK。

组织回收保原 `/v1/organizations/{org}/account-closed-grants/{grant}/reclaim` 及 `account-closed-grants:reclaim-batch`。`org.grants.reclaim_closed` 人类委托绑定组织、grant、原用户；Billing 另核 Account ANONYMIZED/COMPLETED 生命周期。过期、冻结、预占拒绝。原 200 及 207 `SUCCEEDED/FAILED` 保持；未知 transport/错误目标回执 `COMMAND_RESULT_UNKNOWN` 保原键/事实。FAILED 不证明没有财务效果；批量逐项稳定派生键，不变成跨项原子操作。

当前实际验证尚未收口：隔离三域正式 PostgreSQL 组合 63.992 秒、Token 36→37 正式升级并保旧账户事实 10.486 秒、强制退款 blocker 四状态 19.055 秒、拒付四状态 30.896 秒已执行通过。拒付的未知终态测试是缺少权威账本证据的 CONFIRMED，不冒称覆盖旧 RESOLVED 完整升级。赠额交集首轮失败：丢 ACK 前未提交收据；仅加低敏诊断后同预算重跑通过 55.072 秒，未取得首轮 HTTP 状态，不能断言根因已修，仍需非作者复跑，见 [Issue #1672](https://github.com/Pinvou/pinvou3/issues/1672)。后续组合中的组织回收对账夹具因 UTC 日期误作上海业务日失败；仅修夹具后赠额五例及回收八例组合通过 88.388 秒。以上不是生产部署、真实 Keycloak 删除、NATS 发布或全桌面验收；最终结果须绑定冻结源码和独立记录，不继承旧私有候选的测试数字。

追加作者验证：三域正式组合及子 Account claim 三例通过 76.385 秒，包含预检查后持久组织所有权出现时清理事务重新拒绝、过期 claim 无法续约/记录 ACK、丢 ACK 原阶段恢复。该所有权用例是受控持久事实，不声称公开入口允许给 CLOSING 用户授予所有权。付款联系人清理和原 isolation receipt 重放真实 PostgreSQL 通过 8.081 秒，订单其他字段、交易、权益、钱包及账本均不变。真实 Portal 代理通过 7.695 秒，覆盖个人/管理 GET、HEAD、POST 及组织单项/批量、session/CSRF/权限错误；初次 HEAD 白名单遗漏失败留档，窄修后重跑通过。Identity 删除入口使用真实签名 JWT/JWKS 校验和受控目录适配验证权限四例，未对生产 Keycloak 发请求。Windows PostgreSQL 子进程未运行 race；Linux race 和最终独立审核另行记录。

相邻传播后，Commerce 保留前序当前 mapping/provider/adapter/subject/version 筛选、锁内重核及锁后数据库租约时钟。入队记录允许 ACTIVE、DISABLED、REPAIR_REQUIRED、PROVISIONING 的合法 producer，派发仍仅允许 ACTIVE。真实注册与已确认 MOBILE 四态共五子例通过；三域赠礼注销组合通过 65.29 秒。旧代际禁止重新派发，原有效在途回调及历史证据保留。

会话撤销 ACK 按 USER→profile→sessions 锁序记录；迟到完成不覆盖 CANCELLED_ACCOUNT_CLOSURE 的状态或原因，原 COMPLETED 保持只读回放。正式无 overlay 九子例及映射锁租约专项通过 37.052 秒，其中并发 Prepare 实际遇到一次 40001：首事务回滚后核实原 claim/version/live lease 仍有效，以同 claim 恢复成功，未接受 40P01。测试中的 Account 主体快照未变，不冒称该组实测 Billing 余额。此前两次新增测试分支/观察分类失败及组合中的旧映射查询观察失败均留档。最终独立审核、生产外部系统验收和前述保留治理边界仍分别记录。
