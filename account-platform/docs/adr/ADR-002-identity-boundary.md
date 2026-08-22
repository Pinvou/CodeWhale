# ADR-002：Keycloak 与业务账号边界

- 状态：Accepted
- 日期：2026-08-12

## 背景

系统需要统一个人注册、多设备登录和组织授权，同时不能把密码安全与业务权限揉成同一个边界。

## 决策

Keycloak 负责注册、登录、密码、MFA、会话、OIDC 和令牌签发。Account Service 以 `issuer + subject` 映射业务用户，并拥有组织、角色和业务状态。Web 客户端使用 Authorization Code + PKCE；桌面客户端使用仅对公开桌面 client 开启的 Direct Access Grant，在应用内完成基础账号密码登录。所有客户端禁用 Implicit。

桌面密码只在一次 Tauri IPC 和 HTTPS Token 请求中存在，不持久化、不写日志；仅 Refresh Token 进入系统凭据存储。Direct Access Grant 不承担注册、找回密码、第三方身份或复杂 MFA，这些能力必须走独立的受控流程。

注册中 Identity 与 Account 均成功后，账号事实已经成立并持久进入 `PROVISIONING`。个人钱包由 Billing 异步初始化；Billing 故障只能延迟平台模型和充值能力，不能回滚或删除已经验证成功的手机号身份。超过 Provisioning SLA 后进入 `REPAIR_REQUIRED`，仍允许登录和管理资料。

JWT 只携带稳定身份、会话和粗粒度上下文，不携带余额、组织树或完整资源授权。高风险操作实时回源 Account Service。

## 被拒绝方案

- 自建密码和 OAuth/OIDC：安全面过大。
- 把组织完整保存在 Keycloak：复杂业务授权、账务和审计难以演进。
- 继续使用 BIOS 序列号作为用户身份：设备不等于自然人，无法支持多设备和组织。

## 影响与回滚

Keycloak 不可用会阻止新登录，但已签发短令牌可按服务策略继续。若撤销应用内密码登录，应先恢复桌面 Authorization Code + PKCE 入口，再关闭该 client 的 Direct Access Grant；不能回退为设备身份。
