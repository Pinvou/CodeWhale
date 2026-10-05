# ADR-002：Keycloak 与业务账号边界

- 状态：Accepted
- 日期：2026-08-12

## 背景

系统需要统一个人注册、多设备登录和组织授权，同时不能把密码安全与业务权限揉成同一个边界。

## 决策

Keycloak 负责持久身份、登录、密码、MFA、会话、OIDC 和令牌签发；独立 Identity Adapter 是密码策略、注册短期密码材料和 Keycloak 管理协议的唯一应用边界。Account Service 以 `issuer + subject` 映射业务用户，并拥有组织、角色和业务状态，只通过服务端口使用不透明密码引用。Web 客户端使用 Authorization Code + PKCE；桌面客户端使用仅对公开桌面 client 开启的 Direct Access Grant，在应用内完成基础账号密码登录。所有客户端禁用 Implicit。

桌面登录密码只在一次 Tauri IPC 和 HTTPS Token 请求中存在；注册密码只通过 Tauri 发往 Identity Adapter，Account 与短信 API 仅接收 15 分钟短期不透明引用。密码不持久化、不写日志；仅 Refresh Token 进入系统凭据存储。Direct Access Grant 不承担注册、找回密码、第三方身份或复杂 MFA，这些能力必须走独立的受控流程。

注册中 Identity 与 Account 均成功后，账号事实成立并持久进入 `PROVISIONING`，Billing 故障不能回滚或删除已经验证成功的手机号身份。Account 提交同时写入钱包初始化任务和 Outbox；后台任务使用稳定幂等键初始化钱包，成功后激活账号，超过冻结的 SLA 时进入 `REPAIR_REQUIRED`。登录后的 bootstrap 只建立会话，不同步初始化钱包。注册丢失响应及本地完成失败通过同一持久操作恢复，`password_preserved` 披露首次密码保留。

桌面注册查询凭据由 Rust 管理，优先写入系统凭据库；无法持久化时仅在当前进程内保存绑定当前会话代次和注册事实的恢复记录，界面明确提示不能跨重启恢复。查询令牌不进入 WebView JavaScript，也不降级写入设置文件。

密码引用失效不能证明远端身份不存在。创建前安全终止须同时满足 Account 持久首次尝试 CAS、Identity 对该次请求尚未派发的明确结果，以及本地收尾事务对该次 attempt version 的精确比较；任何后续尝试会改变版本，使旧结果不能释放预约。旧注册意图、远端结果未知、Identity 重启丢失绑定或不满足比较条件时进入既有恢复与人工修复流程，不能猜测补偿。

JWT 只携带稳定身份、会话和粗粒度上下文，不携带余额、组织树或完整资源授权。高风险操作实时回源 Account Service。

## 被拒绝方案

- 自建密码和 OAuth/OIDC：安全面过大。
- 把组织完整保存在 Keycloak：复杂业务授权、账务和审计难以演进。
- 继续使用 BIOS 序列号作为用户身份：设备不等于自然人，无法支持多设备和组织。

## 影响与回滚

Keycloak 不可用会阻止新登录，但已签发短令牌可按服务策略继续。若撤销应用内密码登录，应先恢复桌面 Authorization Code + PKCE 入口，再关闭该 client 的 Direct Access Grant；不能回退为设备身份。
