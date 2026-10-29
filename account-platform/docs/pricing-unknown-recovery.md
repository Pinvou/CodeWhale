# 价格 UNKNOWN 后显式新操作

原请求恢复仍使用相同操作类型、参数、target及幂等key；成功回执才走已有确认恢复。
重放412/422不能证明原请求没有执行。未确认时，管理员可使用以下独立流程：

1. 读取**原操作**的目标配置：价格target按ID读取，再读取其实际namespace；新草稿读取原输入namespace；单位操作读取完整单位配置。价格空间截断、失败、无权或过时响应均不形成继续操作的资格。
2. 查看当前配置后，明确点击“保留未知记录并允许新操作”。当前配置不能证明旧请求失败。
3. 客户端先将完整原请求存入同用户/会话边界的history并读回验证，再清除active标记。任何失败保留当前锁定并提示；不会自动分配新key或发送新请求。
4. 同用户/会话的所有已挂载页面中，原编辑器、单位表单和确认框关闭，原确认凭据退役；同步旧回调和迟到读取不能重新打开这些原意图。管理员必须重新选择并明确提交后续操作；沿用既有权限、ETag、发布预检和ACTIVE生命周期限制。

历史仍标记为UNKNOWN，可查看原kind、target、input和key；新操作再次UNKNOWN时不覆盖旧历史。
历史key位于已有owner/sessionEpoch命名空间内，不新增跨用户共享或自动清理期限。
sessionStorage仅为当前浏览器用户/会话的本地恢复证据，不是服务端审计或跨会话持久保证；清除浏览器数据仍会失去这些本地记录。

资格绑定当前owner、会话epoch、上下文、权限及读取代次。变化后需要重新读原配置；
较旧响应、失败重读、权限丢失后恢复、较新竞争读取都不能沿用旧资格。相同key的普通成功恢复仍保留。

回归入口：`node --test account-platform/test/contract/pricing-unknown-recovery.test.mjs account-platform/test/contract/pricing-presenter-retirement.test.mjs`；
management UI npm入口包含真实React/AdminClient的受控HTTP smoke。
可在`pinvou3-app`运行`node tests/account_platform_pricing_unknown_ui_smoke.mjs --serve-only`启动受控浏览器入口。
同一终端输入`q`回车关闭并校验清理自有临时目录；该模式不启动或下载浏览器。
这些本地证据不代表真实Account/Billing服务或财务渠道验收。

恢复重放意图会同步退休所有较早的配置读取资格，并取消其在途请求。即使重放仍返回503且较早读取忽略取消后迟到，也必须在重放结束后重新读取配置才能明确开始新操作；此顺序不依赖React的loading状态更新时机。
