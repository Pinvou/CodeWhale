# 显式、隔离的验收入口

这些入口不在默认测试中启动环境、注册用户或调用真实收款渠道。纯配置合同通过不代表真实 OIDC、资金结算、Prometheus 或部署验收通过；每个运行记录应保存当前源码身份、命令、实际用例结果与未运行项，历史结果不能替代当前 head。

## 登录后的业务链路

仅在操作者已准备好的可丢弃 loopback 测试栈运行。该脚本会创建组织和部门、切换上下文、执行模拟付款和一次模型调用，失败不会自动重试写操作或清理业务数据。不得指向需保留数据的环境或真实支付渠道。

通过现有私密环境配置供应以下变量，不把密码、访问令牌或环境文件提交到仓库、命令行记录和日志：

- `PINVOU_TEST_ACCOUNT_ORIGIN`、`PINVOU_TEST_TOKEN_ORIGIN`、`PINVOU_TEST_PAYMENT_REQUEST_ORIGIN`：实际可丢弃服务的 loopback origin，不能包含路径、凭据、查询或 fragment。
- `PINVOU_TEST_OIDC_TOKEN_URL`：同一隔离栈的 loopback OIDC token endpoint；不跟随重定向。
- `PINVOU_TEST_OIDC_CLIENT_ID`、`PINVOU_TEST_OIDC_AUDIENCE`、`PINVOU_TEST_OIDC_USERNAME`、`PINVOU_TEST_OIDC_PASSWORD`：已配置的测试登录身份。用户必须已经有经过验证的测试手机号、`platform.organizations.create` 权限、个人充值和模型读取/调用权限；脚本不替用户授权或验证手机号。
- `PINVOU_TEST_PRODUCT_ID`：实际个人充值目录内可用于 ALIPAY 模拟渠道的测试产品。
- `PINVOU_TEST_MODEL_ID`：实际模型目录中已完成路由配置、当前上下文可用的模型 UUID；`PINVOU_TEST_RESPONSE_MODEL` 为该受控上游的实际响应模型名。不能用旧别名假定模型设施已安装。

完成配置后从仓库根目录显式执行：

```sh
npm --prefix account-platform run test:auth-business -- --execute-disposable-fixture
```

全部服务 origin 只接受显式 IP loopback（`127.0.0.1` 或 `[::1]`）。没有默认地址、凭据或产品回退。组织通过现有管理员入口创建并指定当前用户为初始所有者；上下文变更使用刚读取的版本；模型调用使用独立幂等键；金额比较保留 int64 精度。支付模拟器只接收原支付路径，不接收登录令牌。只有实际运行整条链成功，才能登记该次业务验收通过；受控 HTTP 合同测试不能算真实 OIDC 或财务验收。

## 实际渲染告警规则

先用现有 Helm 入口生成当前配置的实际 YAML，准备一个不存在的新输出目录及本机真实 `promtool` 绝对路径，再执行：

```sh
npm --prefix account-platform run test:prometheus-rules -- --rendered /absolute/current-render.yaml --output /absolute/new-fixture-output --promtool /absolute/promtool
```

Windows 使用相应的绝对路径。渲染时开启 `observability.prometheusRulesEnabled=true`、`availability.enabled=true` 和 `approvalRelay.requireReady=true`。包装器提取实际 `PrometheusRule`，组合原八个普通 Relay 场景与六个授权 Relay 场景，再运行 `promtool test rules`。工具缺失、目录已存在、渲染缺规则或执行失败均返回失败。只生成文件时使用 `prepare:prometheus-fixtures`。

这些测试没有部署、切换、发布或连接线上 Prometheus 的授权。当前分支修复报告须分别披露显式入口合同测试、真实 promtool 运行及真实业务验收是否执行。
