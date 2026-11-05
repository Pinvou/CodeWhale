# NewAPI 受控 TLS 入口

本入口恢复原同 Pod 部署：固定摘要的 NewAPI HTTP 服务与本仓 `newapi-tls` 同处一个 Pod；TLS 进程只向 `127.0.0.1:8080` 转发。Service 只提供 8443，不发布 8080，不设置 public Ingress、NodePort、hostPort 或 hostNetwork。F04 当前实际使用 `/v1/models` 发现与凭据验证；入口保留原固定请求方法/路径、查询、鉴权和 SSE 边界，不表示本波已交付 Token 普通调用或公开用量 producer。

NewAPI 固定为 `calciumion/new-api:v1.0.0-rc.11@sha256:bd30213d808857bb569ef47d3c9209d061a66ea089c2472ef46ce51e75517f19`。遵守其 AGPL 许可及上游声明。该版本 `PORT` 只选择端口，HTTP 监听为 `:port`，不能用两个共享 backend Compose 容器替代同 Pod 隔离。[上游启动实现](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.11/main.go)

## 准备和投放

1. 使用现有 `account-platform/Dockerfile` 的 `SERVICE=newapi-tls` 构建同一已审核源码镜像，记录制品摘要。该 CLI 仍只启动 TLS 服务，无新的部署渲染或业务命令。构建命令沿其他 Go 服务：`docker build --build-arg SERVICE=newapi-tls -t <approved-image> account-platform`，发行后以 `repository@sha256:...` 引用。
2. 在独立的受控 Kubernetes namespace 预置运行 Secret（`SQL_DSN`、`REDIS_CONN_STRING`、`SESSION_SECRET`、`CRYPTO_SECRET`）。按既有数据库/Redis权限和 TLS 合同准备连接值；不把值写入公共 YAML、命令历史或诊断。NewAPI `/data` 只作临时工作目录，真实数据在外部数据库；本模板不创建新数据库/Redis。
3. 使用当前 `kms-operator` infrastructure PKI custody 流程生成并验证 `READY_V2`、manifest 和文件 hash。将 `newapi/server.crt`、`newapi/server.key` 以同名键投影到单独 TLS Secret，私钥只挂给 TLS 容器；不能挂给 NewAPI、Gateway 或 credential Portal。`ca.crt` 加入客户端 combined CA。证书 SAN 必须覆盖实际使用的 `newapi` DNS 名；不得使用错误 SAN、过期证书或 `InsecureSkipVerify`。
4. 复制 `newapi.yaml.template` 到受控私有发行目录，仅替换全部 `__MARKER__`：namespace、两处 Secret 名、已批准的 TLS 镜像摘要、Gateway/credential Portal 源 CIDR，以及 PostgreSQL/Redis/模型供应商目的 CIDR。用目标 CNI 实际看到的源地址，尽量单主机/最小网段；不得以 `0.0.0.0/0`、`::/0` 或整个共享 backend 网段冒隔离。多个合法供应商可复制同一 443 egress 规则，每项单独批准。DNS 仅到实际 kube-system DNS pods；集群若使用不同 DNS 标签应按真实事实窄适配该规则，不开放一般外网 DNS。
5. 目标 namespace 必须由运维保证没有额外 NetworkPolicy 放宽该 Pod 的入口（Kubernetes 策略是叠加允许）。只准两客户端来源到 8443；8080 不可从其他 Pod/主机访问。先核所有标记已替换、镜像含固定摘要、Secret refs/namespace/CIDR 和实际 PKI，再按现有受控操作执行 `kubectl apply --dry-run=server -f <private-manifest>`、`kubectl apply -f <private-manifest>` 和 `kubectl rollout status deployment/newapi -n <approved-namespace>`。不把未渲染模板直接 apply。

HTTPS probes 沿用原 `/health/live`、`/health/ready` 静态入口；它们只证明 TLS 进程监听，不证明供应商、目录完整性、证书 CA/SAN/有效期或业务依赖健康。由现有 Gateway 发现和 credential 验证的真实 HTTPS 客户端核 CA/SAN/有效期、有效目录和凭据。Kubernetes kubelet 的 HTTPS probe 不能代替该证明。

## 当前客户端和初始化

配置 Gateway 的实际 `TOKEN_DEFAULT_PROVIDER=NEW_API`、`TOKEN_DEFAULT_PROVIDER_BASE_URL=https://newapi:8443`、准确 `TOKEN_PROVIDER_ALLOWED_HOSTS=newapi`、`TOKEN_PROVIDER_CA_FILE=/run/pinvou/trust/ca.crt`，并使用现有私有 env/sink 提供 default API key/models。credential Portal 使用相同已允许供应商 DNS 和 CA。当前 Compose 工作负载若在 Kubernetes 外，须由已批准私有 DNS/路由将 `newapi` 指向该私有 8443 Service，且从实际客户端验证可达；模板不创建跨环境桥接或公开端口。也可继续使用既有支持的外部 HTTPS origin，此时使用它的准确 allowlist/CA，不部署本入口。

NewAPI 首次管理员初始化只由授权 operator 使用 `kubectl port-forward --address 127.0.0.1 -n <approved-namespace> deployment/newapi 8080:8080` 临时访问同 Pod 后端。操作完成立即关闭，不建立 8080 Service/Ingress；Kubernetes operator RBAC 不授予应用。TLS 入口明确拒绝 `/api/setup`、用户管理、浏览器 Cookie 和重定向；不要通过扩展代理路径来执行初始化。

TLS 层保持原四条精确路由，拒绝转义路径和错误方法；仅传递 provider 鉴权、New-Api-User、内容类型、接受类型及原 request/trace correlation。上游 Cookie、Location 不返回客户端；连接错误只返回静态 502，不记录 caller URL/Token/异常内容。流式响应无普通写超时并及时 flush，客户端取消传到上游。

本地受控 HTTP/TLS 和静态模板验证不证明真实 Docker 构建、Kubernetes CNI/RBAC、外部 DNS/Secret/证书发行或真实 NewAPI 运行。目标部署须实际执行上述受控投放和正反连通性核验；不因此宣称完整 E2E 或当前上游 CI 全通过。
