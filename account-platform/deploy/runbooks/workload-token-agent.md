# 服务身份轮换器

状态：SECINF-02A 实施交付；独立 Keycloak realm、真实签发联验及 Compose/Helm 接线尚未完成，不能单独以本文宣告生产就绪。

## 边界

一个进程只服务一个 Keycloak confidential client。使用标准 client-credentials 申请短期访问令牌；不签名、不生成角色或扩大权限。接收端仍按独立 workload realm 的 issuer、JWKS、audience、真实 service-account subject 和逐接口权限验证。不要把用户登录 realm 的 token 或 Keycloak 管理员 token 当 workload token。

每个服务使用独立只读 bootstrap 文件及投递目录。业务不能挂载 bootstrap 目录、其他服务的目录或 agent 的配置。客户端秘密轮换后按文件重新读取，不依赖重启。日志仅固定状态，禁止 HTTP body/debug 日志、shell tracing 和输出凭据。

## 配置与运行

以下仅为占位示例，不能原样部署。路径使用 Linux 规范绝对路径（例如 `/run` 而非指向它的 `/var/run` 符号链接）。所有目录预先创建，禁止组/其他用户写；秘密文件 0600 或受控 0640，输出由 agent 创建为 0600。agent 和只读消费方使用同一个指定 UID，通过单服务只读挂载隔离；此机制不隔离宿主机 root。

```json
{
  "issuer": "https://identity.example.test/realms/workload",
  "audience": "platform-internal",
  "client_id": "service-client",
  "subject": "replace-with-keycloak-service-account-uuid",
  "client_secret_file": "/run/bootstrap/client-secret",
  "token_file": "/run/projected/token"
}
```

配置文件也需限制写权限并以 0600/0640 挂载。秘密只能出现在专用文件中，不在 JSON、参数或环境中。由服务镜像执行：

```text
/service -config /run/config/workload.json
/service -config /run/config/workload.json -check
```

`-check` 检查已经投递的令牌及签名，不获取新令牌、不读取 bootstrap 秘密内容。至少还剩 30 秒有效期才健康；新启动的独立健康进程可能需要读取 JWKS，签发设施不可用时健康检查会失败，不绕过签名。定时续取在剩余有效期一半时发生，失败按 5/10/20/30 秒退避重试。服务应依赖首次健康投递后再接流量，并监控续期失败与就绪状态。

仅接受 HTTPS realm URL，token/certs 端点由该地址固定派生，不跟随任何 HTTP 跳转。使用系统可信 CA；私有 CA 必须通过受控挂载加入可信包，不允许关闭证书校验。HTTP 请求超时 10 秒，响应至多 64 KiB，JWT 至多 32 KiB。签名固定 RS256，精确绑定 issuer、audience、sub、azp；iat 不得在未来、nbf 不得未生效、生命周期为正且最多 15 分钟，续取不相信 `expires_in`。

投递流程为同目录临时文件 → 写入及 fsync → 关闭 → 原子替换。必须挂载整个单服务投递目录，单文件挂载会固定旧 inode。只有 agent 有目录写权限，部署保证单 writer。投递文件可在重启后重新签发，不作为需要恢复的长期秘密备份。

失败保留旧文件，但绝不延长其签名有效期；接收端每次请求检查实际 exp，过期拒绝。文件丢失或非法时不回退静态 token。主机需要同步可信时间：未来 iat/nbf 严格拒绝，不以放宽时钟窗口代替修复时钟。

## 验证与后续

本地：`go test -tags=nomsgpack ./internal/platform/workload ./internal/platform/serviceauth ./cmd/workload-token-agent`。Windows 不以 POSIX mode bit 判断 ACL；相关权限断言只在 Linux 执行，Windows 开发目录由操作者收紧 ACL。Windows 普通读句柄会阻止并发 rename，不能运行本 agent 的正式投递服务；并发读写原子性断言同样仅在 Linux 执行。Windows 单次功能测试不能代替这两项 Linux 验证。真实部署固定 Linux。

SECINF-02B 必须在隔离 Keycloak 验证真实签发、服务账户 UUID、错误主体/受众、权限拒绝、续期与过期；关闭用户交互授权、密码授权、隐式授权、设备授权、Token Exchange、offline access 和 refresh token，不给服务账户 realm-management 权限。此后 SECINF-04 才将已验证配置与镜像纳入部署 overlay，不直接沿用旧模板中的示例 subject。
