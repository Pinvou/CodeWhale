# 回滚网关的两跳 Host / Origin 合同

浏览器 → 公网代理 → Enrollment gateway → loopback Legacy Relay 是两个独立的 WebSocket upgrade。代理发给 gateway 的 `Host` 可以是公网站点；gateway 使用配置的 loopback upstream URL 新建到 Relay 的连接，其 `Host` 是该 URL 的地址／端口。gateway 保留原浏览器 `Origin`，不会把公网 `Host` 或可伪造的 `X-Forwarded-Host` 当成 Relay 的受信 Host。

因此，公网浏览器的 Origin 与 gateway 入站 Host 相同，只能满足第一跳的默认同源检查；它不会与第二跳 loopback Host 同源。没有匹配的 Relay Origin allowlist 时，第二跳仍拒绝。这是已有失败关闭行为，不是本次新增默认允许，也不是 Origin 绕过。

## 精确配置与回滚

对 gateway 和 Legacy Relay 的两个进程都设置 `PINVOU_REMOTE_ALLOWED_WEB_ORIGINS`，内容是实际受信浏览器站点的精确 origin（scheme、host、有效 port），不是页面路径、通配符或整个 URL 集合。两者使用同一部署合同；例如文档占位值：

```dotenv
PINVOU_REMOTE_ALLOWED_WEB_ORIGINS=https://remote.example.invalid
```

实际部署应替换为受信公开站点。gateway 的 allowlist 非空时只接受其中的精确 origin；Relay 保留原 exact allowlist 或与其直接请求 Host 同源的兼容行为。不要为了连通而清空 Origin、改写成 loopback Origin、信任公网 forwarded-host 或增设泛 allowlist。Nginx 的公网 Host 配置不能自动改变 gateway 新建上游连接的 Host。

`PINVOU_REMOTE_GATEWAY_TRUSTED_PROXY_IPS` 指定能提供客户端来源的直接代理。代理覆盖 `X-Forwarded-For` 为单个真实来源地址，gateway 拒绝缺失／无效／多地址链或非受信来源的该头；Origin allowlist 不替代这个边界。gateway 发往 Relay 的来源头继续受 Relay 既有 trusted-proxy 配置约束。Native 不发送 Origin 的请求仍可通过 Origin 检查，但仍须通过原 enrollment／凭据／协议门禁，不能把无 Origin 当作授权。

回滚恢复时使用受保护的同一份精确 origin 配置启动 gateway 与 Relay，保留原注册／凭据／数据和公开路由；运行现有每公开入口的 gateway preflight 与回滚覆盖门禁，再做真实浏览器连接。若旧拓扑只在公网入口通过第一跳而上游 403，优先检查两个进程的配置是否一致，不放宽生产检查。此文档不授权绕过已有覆盖门禁、发布未验收控制器或重新派发旧代际业务任务。

## 本点回归边界

`remote-control-relay/test/enrollment-origin-contract.test.js` 使用实际 `server.js` 子进程和实际 Enrollment gateway，经真实 loopback WebSocket 两跳验证：公网同源仅第一跳通过但第二跳拒绝、双方精确 allowlist 成功、错 Origin、Native 无 Origin、错误代理来源头、forwarded-host 不能修复上游 Origin，以及恢复精确配置后相同拓扑／测试凭据可连接。

gateway registry 使用精确匹配测试凭据的适配器；不声称持久 EnrollmentRegistry、官方身份服务或存量 enrollment 数据恢复 E2E。公开代理头在 loopback 测试中模拟，没有运行 Nginx 或真实 TLS／DNS／公网部署。生产门禁函数没有修改；验证结果以该 PR 实际日志为准。
