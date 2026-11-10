# Account 组织启动诊断

组织生命周期装配失败仍以退出码1停止启动。结构化日志通过 `component=organization-startup`、`level=ERROR`、`result=failure` 和有限的 `error_code` 分类，不输出底层错误、服务地址或凭据，也不经会折叠文本的标准日志桥传递类别。

| 错误码 | 检查项 |
|---|---|
| `ACCOUNT_ORGANIZATION_PROFILE_INVALID` | `ACCOUNT_PLATFORM_PROFILE` 是否为既有支持的模式 |
| `ACCOUNT_ORGANIZATION_BILLING_AUTHORITY_INVALID` | 组织 Billing 权威配置 |
| `ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID` | `TOKEN_HTTP_URL` 对应的组织 Token 权威配置 |
| `ACCOUNT_ORGANIZATION_AUTHORITY_INVALID` | 尚未分类的组织装配错误；保留失败关闭，排查对应版本 |
| `ACCOUNT_WALLET_AUTHORITY_INVALID` | 组织装配前的 Billing 钱包权威校验，保留原错误码并用同一结构化入口输出 |

校验顺序不变。更早的钱包权威、身份服务或凭据校验仍可能先返回其原有错误码，例如无效 Billing 地址先触发 `ACCOUNT_WALLET_AUTHORITY_INVALID`。不要通过删除必需配置或切换模式绕过错误。

`personal` 仍关闭组织入口，保留既有个人账户与钱包装配要求；`models` 仍要求两种组织权威，不自动降级。修复配置后按原流程重新启动，不改变已有权限、资金状态或客户端错误合同。
