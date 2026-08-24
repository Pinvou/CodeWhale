# Nginx 入口

`account-platform.conf.template` 将公开入口代理到 Compose 仅绑定在
`127.0.0.1` 的服务端口。安装前必须把 `__PUBLIC_HOST__` 替换为实际域名或
地址，并将渲染结果写到 Nginx 的站点配置目录；不要直接修改模板提交环境专属
地址。

当前支付 Provider 回调复用现有官网根域名 `pinvou.com`，不新增 DNS 记录。生产
入口不得用本目录的完整账号平台 `server` 模板覆盖现有官网配置；应把
`payment-callback-locations.conf.template` 替换 `__BILLING_UPSTREAM__` 后
include 到现有 `pinvou.com` HTTPS `server` 块。该片段只增加以下两个精确路径，
官网其他路径保持原配置：

- `POST https://pinvou.com/external/v1/payment-callback/token`
- `POST https://pinvou.com/external/v1/payment-callbacks`

Billing 与官网入口同机时可替换为 `http://127.0.0.1:18102`；不同主机时只能
替换为受控内网、VPN 或固定网关地址。Provider 侧地址保持不变。代理必须保留
`Authorization`、`Content-Type` 和请求体，不得缓存、重定向或改写这两个请求。
两个回调入口不依赖把账号平台全局 `PUBLIC_BASE_URL` 改为 `pinvou.com`；只有
整个账号平台原本就通过该根域名发布时才同步修改全局地址。

远程 Compose 同时加载 `deploy/compose/compose.remote.yaml`，并通过
`PUBLIC_BASE_URL` 配置 Keycloak、Portal、Account、Token 和支付模拟器的一致
公开地址。例如：

```text
docker compose --env-file .env -f compose.yaml -f compose.remote.yaml up -d --build --wait
```

仓库完整模板只提供 HTTP 开发入口。生产环境必须在 `pinvou.com` 入口配置可信证书、强制 HTTPS，
并把 `PUBLIC_BASE_URL` 设置为对应的 `https://` 地址。支付模拟器和本地密钥
Provider 仅用于开发与验收，不得作为生产支付或生产凭据存储。
