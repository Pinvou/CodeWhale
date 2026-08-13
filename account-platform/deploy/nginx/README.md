# Nginx 入口

`account-platform.conf.template` 将公开入口代理到 Compose 仅绑定在
`127.0.0.1` 的服务端口。安装前必须把 `__PUBLIC_HOST__` 替换为实际域名或
地址，并将渲染结果写到 Nginx 的站点配置目录；不要直接修改模板提交环境专属
地址。

远程 Compose 同时加载 `deploy/compose/compose.remote.yaml`，并通过
`PUBLIC_BASE_URL` 配置 Keycloak、Portal、Account、Token 和支付模拟器的一致
公开地址。例如：

```text
docker compose --env-file .env -f compose.yaml -f compose.remote.yaml up -d --build --wait
```

此配置只提供 HTTP 开发入口。生产环境必须在 Nginx 配置可信证书、强制 HTTPS，
并把 `PUBLIC_BASE_URL` 设置为对应的 `https://` 地址。支付模拟器和本地密钥
Provider 仅用于开发与验收，不得作为生产支付或生产凭据存储。
