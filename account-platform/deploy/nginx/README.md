# Nginx 入口

`account-platform.conf.template` 将公开入口代理到 Compose 仅绑定在
`127.0.0.1` 的服务端口。安装前必须把 `__PUBLIC_HOST__` 替换为实际域名或
地址，并将渲染结果写到 Nginx 的站点配置目录；不要直接修改模板提交环境专属
地址。

生产环境使用独立域名 `account.pinvou.com`：

- Web 个人中心：`https://account.pinvou.com/`
- 支付回调 Token：`POST https://account.pinvou.com/api/v1/payment-callback/token`
- 通用支付回调：`POST https://account.pinvou.com/api/v1/payment-callbacks`

根目录 `deploy/nginx-pinvou-account-domain-bootstrap.conf` 用于证书签发前的
ACME 验证；DNS 生效并签发证书后必须替换为
`deploy/nginx-pinvou-account-domain.conf`。正式站点只允许 HTTP 跳转 HTTPS，
两个支付回调精确路径代理到 Billing，其余同源页面、静态资源、OIDC 和 BFF
请求代理到只监听 loopback 的 Token Portal。`pinvou.com` 不再承载个人中心、
回调接口或兼容重定向。

如由其他网关承载 HTTPS，可把
`payment-callback-locations.conf.template` 中的 `__BILLING_UPSTREAM__` 替换为
受控 upstream 后 include 到 `account.pinvou.com` 的 HTTPS `server` 块。Billing
与入口同机时可使用 `http://127.0.0.1:18102`；不同主机时只能使用受控内网、
VPN 或固定网关地址。代理必须保留 `Authorization`、`Content-Type` 和请求体，
不得缓存、重定向或改写这两个 POST 请求。

Web 个人中心配置 `PORTAL_PUBLIC_URL=https://account.pinvou.com`，桌面端固定使用
`https://account.pinvou.com/`，Keycloak 客户端登记
`https://account.pinvou.com/auth/callback`。身份服务的 `PUBLIC_BASE_URL` 仍按其
独立公网边界配置，不得因为 Portal 子域名变化而盲目改写。

远程 Compose 同时加载 `deploy/compose/compose.remote.yaml`，并通过
`PUBLIC_BASE_URL` 配置 Keycloak、Account、Token 等服务的一致公开身份地址。例如：

```text
docker compose --env-file .env -f compose.yaml -f compose.remote.yaml up -d --build --wait
```

仓库完整模板只提供 HTTP 开发入口。生产必须使用可信证书并强制 HTTPS。支付
模拟器和本地密钥 Provider 仅用于开发与验收，不得作为生产支付或生产凭据存储。
