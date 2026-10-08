# 账号平台通用安全与运行库

本阶段提供可独立验证的 HTTP 服务运行库、OIDC 与服务身份验证、TLS/Redis 连接原语、可信时钟、观测、断路器、迁移执行器，以及事件格式、普通与冻结投递协议。独立工具模块提供离线 age 密钥托管、受控 KMS 初始化及交互式解封、PKI/NKey 制品生成。

业务服务、业务 SQL、审批可执行命令、NATS 业务授权、网页和桌面账号入口尚未加入。事件库通过存储与 broker 接口运行；具体数据库存储、租约栅栏及 schema 随首次使用的业务功能交付。断路器只提供状态和只读探针接口，实际调用方的恢复接线随所属功能验证。

普通 `events.Relay` 必须显式设置非空 `AllowedSubjects`（精确主题或末尾 `.>` 子树）。`OutboxStore.Claim` 的最后一个参数为该主题范围，存储必须在领取租约前过滤，并同时保留自身的事件排除规则。越域返回整批失败且不修改重试或死信状态。普通崩溃重领保留 `PublicationGeneration` 和 broker 去重 ID；仅明确重新发布可推进代次。冻结投递使用独立协议。

## 本地验证

在本目录执行 `go test -count=1 ./...`、`go vet ./...`。工具使用独立模块，在 `tools/kms-recovery/` 执行同样命令。真实 PostgreSQL、Keycloak 和 OpenBao 用例为显式 opt-in；条件跳过不代表实际集成或部署验收通过。Windows 的文件 mode 测试不证明 POSIX 权限或 Windows ACL，实际使用须按各工具文档限制宿主权限。

PR Check 对账号平台路径改动在 Draft、Ready 和 Merge Queue 均执行 Go race 测试与 vet，逐个发现主模块及嵌套工具模块；使用主模块 `go.mod` 锁定的工具链与 `-tags=nomsgpack -mod=readonly`，Linux runner 开启 CGO。相关检查失败、取消或意外跳过均阻断 `required-gate`。本地未提供 C 编译器的 Windows 环境不能证明 race 检测通过，须以实际 Linux CI 结果验证。此门禁不配置真实服务凭据，不替代显式集成测试或部署验收。

密码和真实密钥仅由保管人在受控本机终端输入，不能通过 Agent 或聊天提供。PKI/NKey 的 READY 标记只描述生成制品；解封成功只描述 KMS 状态，均不代表业务部署或发布完成。
