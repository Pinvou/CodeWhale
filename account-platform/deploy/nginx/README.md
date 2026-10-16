# 账号平台公共入口模板

现有 Compose 部署使用 [个人入口装配](../production/README.personal.md) 的 TLS 模板；凭据隔离站点使用独立 credential edge。本目录的 `account-platform.conf.template` 保留旧 loopback 上游布局，供确实采用该布局的受控站点使用，不表示当前 Compose 已挂载或部署了它。

## 旧布局的受控 TLS 入口

从已验证的站点元数据渲染 `__PUBLIC_HOST__`（无 scheme、port、path 的 DNS 主机名）与 `__PUBLIC_TLS_PORT__`（1024–65535，建议 8443）。所有标记必须替换后才可通过 `nginx -t`；公开 URL、Keycloak hostname/issuer、Portal public URL 和 OIDC origin 必须使用同一 HTTPS 主机与实际端口。不得把私有 env、密钥、DSN 或渲染后的现场输入提交到仓库或打印到日志。

TLS 叶证书与私钥来自现有专属 `/run/pinvou/public-tls/server.crt`、`server.key` 挂载，证书覆盖已核验主机，只有 nginx 运行身份可读私钥；不借用数据库或独立凭据站点的私钥。模板仅监听 TLS，既有 loopback upstream、路由、内部路径拒绝、请求大小和 Token admin deadline 保持原布局，转发完整受控 host/port 与 HTTPS。它不提供 HTTP 密码/Token 入口，也不创建证书、网络或上游服务。旧仅 HTTP 的站点输入不能当作已满足生产 HTTPS 门禁。

安装前分别核验当前源码、私有证书/权限、渲染零标记、真实 nginx 解析及适用正反入口响应；启用或替换现场配置仍须单独授权。源码静态合同或隔离 fixture 通过不能代替正式证书、上游连通性、切换或发布验收。

## 已有隔离验证工具

`scripts/account-platform-render-nginx-fixtures.mjs` 和 `account-platform/test/deployment/nginx-loopback-test.py` 只操作标记的公开测试 fixture；`scripts/account-platform-credential-browser-scope.mjs` 默认不启动浏览器，显式探测要求受控 Chromium。对应 G6 驱动已随本波恢复原实现，具体命令和机制限制见 [模型治理发布手册](../runbooks/model-token-release.md)。这些驱动使用隔离测试输入，不应指向站点配置或真实秘密；本波恢复源码不表示本 head 已执行 nginx、浏览器或部署验收。
