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

## APT-137 隔离凭据入口（离线配置，不自动安装）

`credential-edge.example.json` 是公开未配置 fixture。受信操作者在仓库外复制并填入当前环境的证书、日志、受控 upstream 和 OIDC/workload 引用；不要把站点路径、令牌、私钥或真实配置提交。两套 Portal/credential origin 成对固定，不能混用 UAT 与 Production。

```text
node scripts/account-platform-credential-edge.mjs --config <受控配置.json>
node scripts/account-platform-credential-edge.mjs --config <受控配置.json> --render http
node scripts/account-platform-credential-edge.mjs --config <受控配置.json> --render credential
node scripts/account-platform-credential-edge.mjs --config <受控配置.json> --render handoff
```

第一条只验证；其余分别输出 nginx `http {}` 内一次性的安全 log_format、独立 credential HTTP拒绝/HTTPS server、现有 Portal HTTPS server 内的 handoff locations。它们不写站点、不调用 nginx、不签证书。不要把 handoff 放入 credential server，也不要替换或删除现有两个支付回调 location。敏感路径保留应用的 CSP 和阶段安全头；不以全站 `no-referrer` 覆盖 handoff 的 `origin`。自定义重试、缓存、调试/WAF/APM body/header 日志、Cookie scope 或 redirect 改写都不属于模板允许修改项。

HSTS 由这层 edge 唯一输出：仅移除上游同名 HSTS，并在 credential server/handoff 精确 location 固定一份（不会因 Portal 父 server 或应用重复而产生逗号合并值）。其他应用头不隐藏，`proxy_intercept_errors off` 保留应用 401/403/303、JSON、Cookie 和 Location。nginx 自产 400/404/413/502/504 等错误分别返回原状态、固定安全 JSON 及 no-store/Referrer/COOP/CSP；不是统一改成 404。固定入口同时比对原始 `$request_uri`，百分号别名/空 query 不会因为 nginx 规范化而进入原应用；OIDC callback 仅接受原始固定 path 和可选非空 query，参数内容仍由原应用严格验证。

`upstreams.*.mode` 只接受 `loopback`、`encrypted_tunnel` 或 `verified_tls`。前两项 URL 必须为明确 loopback 端口；跨主机隧道是否加密、端点身份、访问范围/断链行为必须另验，`security_reference` 只是受控证据引用。HTTPS 模式需要无凭据/参数的固定 origin 和 `ca_file`，渲染固定 SNI 与 `proxy_ssl_verify on`；不能用任意公网 HTTP 或关闭 TLS 校验。证书/私钥/CA 文件和日志路径不得混用。

部署前根在隔离 fixture 上实际 `nginx -t` 并运行独占回环高端口验证拒绝页/安全头；现场安装仍需审批。所有 upstream、外层/内层 nginx、WAF、APM 和错误日志都要另行核验，尤其 HTTP 解析失败、TLS 握手失败等未进入 location 的日志；某层不可见不能由本模板推定无泄漏。公共实例维持安全 route/status/duration/request_id 指标，禁止启用包含完整 URI/query 或密钥的日志格式。

只读 HTTP 前置检查为 `node scripts/account-platform-credential-origin-probe.mjs --config <受控配置.json>`，默认不联网；显式追加 `--probe` 才请求固定双域、公开 OIDC metadata/workload JWKS，没有 Cookie、身份、业务 POST 或重定向跟随。单步绝对 5 秒、响应最多 256 KiB；即使通过也仅 `PREFLIGHT_PASSED / ENV_PENDING / NOT_DEPLOYED`。这不证明 OIDC client 已注册、JWT 实际签名/权限/轮换成功或 KMS/NATS/DNS/备份的整个发布验收完成。完整真实浏览器/身份/日志门禁边界见运行手册 7.1，不能直接把旧本地 `/start` smoke 用于现场。

独立只读浏览器范围工具为 `node scripts/account-platform-credential-browser-scope.mjs --config <受控配置.json>`；默认不启动浏览器，显式 `--probe` 时要求 `CHROME` 指向受控 Chromium 制品且已安装 app 锁定的 puppeteer-core。它使用全新 context、JS-off 唯一 page、加载前请求拦截和经严格 HTTPS/完整字节核验后的响应重放，只读检查 SW，不清理/禁用 SW。输出明确 `VALIDATED_HTTPS_REPLAY / FRESH_CONTEXT_ONLY / ENV_PENDING`，不冒充原生浏览器 TLS 或完整握手。真实机制负例运行 `node --test pinvou3-app/tests/credential_origin_scope_probe.test.mjs`（同样需 `CHROME`）；fixture 仅测试注入回环 HTTP 文档，现场 CLI 没有该绕过入口。

G6 的公共 fixture renderer、nginx 1.18 测试镜像与有界回环 helper 已纳入持续门禁，包含六 profile 解析、双 runtime、ready 后故障及真实 SIGTERM 精确清理。镜像依赖下载来源与受限容器机制验证分别记录，不能以替代测试制品声称官方依赖构建成功；接口和当前证据见 [发布手册第9节](../runbooks/model-token-release.md#9-g6-持续门禁与环境交接)。
