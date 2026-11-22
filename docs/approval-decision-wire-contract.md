# 审批决定的请求枚举

治理审批界面保留 `APPROVE` / `REJECT` 动作词。`AdminPlatformClient.decideGovernedApproval`
在请求边界将它们分别转换为既有服务端枚举 `APPROVED` / `REJECTED`，同时接受这两个
规范值。其他参数在 capability 或 POST 请求前以 `INVALID_APPROVAL_DECISION` 拒绝，
并标记 `NOT_DISPATCHED` 与输入无效；不自动修剪、改变大小写或转换其他类型。

两种现有审批 wire 均使用该转换。原理由、请求版本、控制版本、CSRF 和响应身份校验
保持原流程；这项转换不提供未知结果恢复或财务执行成功的证明。

永久回归：`node --test pinvou3-app/tests/account_platform_approval_decision_contract.test.mjs`。
测试通过实际 client 检查两种 wire、界面动作及规范参数、原理由和头字段，以及非法值零请求。
