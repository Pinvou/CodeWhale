# NATS 固定资源初始化与运行隔离

状态：SECINF-04C1 代码与真实隔离验证通过，NOT_DEPLOYED；不是正式 NATS 部署记录。

生产进程（`production` 及 `production-*`）使用预建资源模式。只查询 `ACCOUNT_PLATFORM` stream，并在开始消费时查询自己实际使用的 durable；不执行创建、更新、删除或 purge。资源缺失、访问拒绝或配置漂移会阻止该业务就绪，不降级到开发初始化。开发测试默认保留既有自动初始化行为。

固定 stream 与五个 consumer 的单一规格在 `internal/platform/events/nats_resources.go`。消费名称/filter/有限或无限重试矩阵见[安全基础设施设计](../../docs/production-security-infrastructure.md)。校验包含所有安全相关配置，允许明确的 NATS 服务端默认值规范化：单副本、默认容量、等待/未确认数量，以及 BackOff 首项覆盖 AckWait。忽略服务器附加的 `_nats.level`、`_nats.req.level`、`_nats.ver` 三项说明性元数据，不忽略其他自定义元数据或业务配置。

## 受控初始化

用现有 Dockerfile 构建独立 `SERVICE=event-resources` 制品。初始化身份与业务身份必须分离，不能把初始化凭据目录挂载给业务。

```text
/service
/service -create-only
```

默认只检查；第二个命令仅创建缺失的固定资源。已有资源只读取和核验，不覆盖漂移；创建响应不确定时回读原资源，不重发 update。部分创建成功后保留现场，使用同一规格重新检查，不删除资源来绕过拒绝。成功仅打印 `NATS_FIXED_RESOURCES_VERIFIED`，不打印连接地址、凭据或服务端响应。

连接通过私有 `NATS_URL`（`tls://`）、可选专用 `NATS_CA_FILE`、已有 `NATS_CREDENTIALS_FILE` 输入。禁止把含身份材料的 URL、凭据文件内容放入命令行、仓库、普通日志或共享示例；配置/凭据目录应只读，逐服务隔离。显式 CA 文件必须为受控绝对路径、普通非符号链接文件且无组/其他用户写权限，内容只含当前有效 CA；客户端至少 TLS 1.2，保持域名验证。无专用 CA 时使用系统可信根，不允许跳过证书验证。只读操作也必须使用 TLS。

初始化工具的 create-only 是代码约束；NATS 的部分 consumer 创建/更新共用 API subject，不能声称初始化身份仅靠 ACL 就完全不能更新。初始化身份仅在串行部署窗口使用，退出后按该身份机制退场；业务不拥有这些 API 权限。

## 运行权限

业务只获固定 stream INFO、自己 consumer 的 INFO/NEXT/ACK 及所需回复收件箱权限；客户端固定使用 `_INBOX.<运行角色>`，服务端仅允许订阅对应前缀，禁止共享 `_INBOX.>`。不得获得 stream create/update/delete/purge/msg get、其他 consumer 的查询/拉取/确认或任意领域发布。三个 relay 发布各自领域及对应 `account-platform.dead-letter.<本域>`；计费保留两个既定 filter 的死信发布，Token 三类消费者保持未解决即无限重试，不新增终止或死信分支。

正式逐服务身份、收件箱隔离、ACL 配置和投递由 04C2 验收；本工具不是身份签发器，也不自动部署 NATS。单服务业务客户端不能因为需要初始化而获得管理员权限。

## 隔离测试

`PINVOU_TEST_NATS_SERVER_BINARY` 显式指定已核对的本机 NATS server 二进制，运行 `go test ./internal/platform/events -run NATS -count=1 -v`。测试自行新建 loopback/TLS/随机身份/临时 JetStream，不接受已有服务地址，退出清理仅自己创建的进程和临时目录。必须看到具名真实测试及 `NATS_PROVISIONED_TLS_ACTUAL_PASS`，未提供二进制时的跳过不能作为验收。
