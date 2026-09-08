# NATS 固定资源初始化与运行隔离

状态：SECINF-04C1、04C2B1/B2 代码与真实隔离验证通过，NOT_DEPLOYED；不是正式 NATS 部署记录。

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

也可通过 `NATS_NKEY_SEED_FILE` 使用标准用户 NKey。该模式与 `NATS_CREDENTIALS_FILE` 互斥；使用任一文件身份时 URL 不得另含用户名、密码或 token。NKey 即使在开发环境也要求验证 TLS。种子文件使用受控绝对路径，普通非符号链接且不超过 4 KiB；Linux 文件仅本角色可访问、直接父目录私有，Windows 必须由投递 ACL 保证。签名回调每次重连重新核验路径/权限/大小/用户类型并读取种子，但公钥绑定初次连接，文件替换为其他身份会拒绝签名，必须显式重建客户端。不能将种子值直接写入环境变量或共享凭据目录。

NKey 种子不是短期 workload JWT，不自动过期。正常轮换先在 broker 配置中短时并存新旧公钥及相同最小 ACL，再逐角色投递并重建客户端、验证新身份；随后撤除旧公钥并主动让已有旧连接失效，实际验证旧种子拒绝后才算完成。隔离测试用受控 broker 重启应用新公钥集合并断开旧连接；不宣称已验证无停机热重载。疑似泄露先撤旧/断连接，不保留旧身份回滚。固定六身份包和 ACL 渲染工具已实现，正式生成、加密离线保管交接与投递仍待 C2D；当前没有生成或部署正式身份。

初始化工具的 create-only 是代码约束；NATS 的部分 consumer 创建/更新共用 API subject，不能声称初始化身份仅靠 ACL 就完全不能更新。初始化身份仅在串行部署窗口使用，退出后按该身份机制退场；业务不拥有这些 API 权限。

## 运行权限

业务只获固定 stream INFO、自己 consumer 的 INFO/NEXT/ACK 及所需回复收件箱权限；客户端固定使用 `_INBOX.<运行角色>`，服务端仅允许订阅对应前缀，禁止共享 `_INBOX.>`。不得获得 stream create/update/delete/purge/msg get、其他 consumer 的查询/拉取/确认或任意领域发布。三个 relay 发布各自领域及对应 `account-platform.dead-letter.<本域>`；计费保留两个既定 filter 的死信发布，Token 三类消费者保持未解决即无限重试，不新增终止或死信分支。

正式逐服务身份、收件箱隔离、ACL 配置和投递由 04C2 验收；本工具不是身份签发器，也不自动部署 NATS。单服务业务客户端不能因为需要初始化而获得管理员权限。

## 固定公开 ACL 离线渲染

先由独立离线 [nats-identities](../../tools/kms-recovery/cmd/nats-identities/README.md) 生成六身份保管包。取得经独立可信清单核验的 `public-keys.json` SHA-256 后，在受控目录执行：

```text
/service -render-authorization -public-keys <公开映射绝对路径> -public-keys-sha256 <可信摘要> -output <新授权配置绝对路径>
```

该模式先于所有 NATS 环境读取、身份访问或连接初始化，不能与 `-create-only` 混用。只读取最多 32 KiB 的受保护普通非链接文件，对实际字节核验摘要并拒绝 JSON 重复键、额外字段、错误类型和非精确六角色集合。输出排他新建、同步/读回，不覆盖旧文件、不打印内容；父目录权限要求与私密投递一致。输出含顶层 `authorization`，可作为受控 NATS 配置片段引入，不含 TLS、监听器或任意附加服务端设置。

正常轮换窗口才能显式附加 `-overlap -previous-public-keys <旧公开映射> -previous-public-keys-sha256 <独立可信旧摘要>`。每份独立校验，全部十二公钥必须唯一，旧新同角色 ACL 一致。切换完成后不带这三个参数生成仅新身份配置，再按已验证方式使旧连接失效；渲染工具不执行重启或撤权，不能拿输出文件存在证明操作完成。

权限使用消息模块的同一固定 consumer 规格。初始化身份只获既定 stream INFO/CREATE、五 consumer INFO/精确 CREATE API 及 `_INBOX.event-resource-operator.>`；无业务发布、其他资源管理或 stream UPDATE/DELETE/PURGE/MSG.GET。consumer CREATE 与 UPDATE 共用 subject 的既有局限不变；受控初始化仍须走工具自身的 create-only/完整核验。正式身份生成、ACL 投递与初始化窗口退场尚需 C2D 现场验收。

## 隔离测试

`PINVOU_TEST_NATS_SERVER_BINARY` 显式指定已核对的本机 NATS server 二进制，运行 `go test ./internal/platform/events -run NATS -count=1 -v`。测试自行新建 loopback/TLS/随机身份/临时 JetStream，不接受已有服务地址，退出清理仅自己创建的进程和临时目录。必须看到具名真实测试及 `NATS_PROVISIONED_TLS_ACTUAL_PASS`，未提供二进制时的跳过不能作为验收。

C2B1 要求上述五角色矩阵的 `password` 和 `nkey` 子用例均通过，并取得独立 `NATS_NKEY_RECONNECT_REVOCATION_ACTUAL_PASS`。JWT 检查只覆盖原官方选项/回调的兼容性，不冒称在此 fixture 完成真实 JWT 签发服务联验。

C2B2 的 NKey 矩阵直接采用渲染器输出，以受限初始化角色进行资源操作；独立 fixture-admin 仅用于故意制造漂移，绝不进入正式配置。另需 `NATS_RENDERED_AUTHORIZATION_OVERLAP_REVOCATION_PASS`，验证十二身份短时并存不扩大权限、撤旧后六旧拒绝/六新成功及既有连接退场。离线工具模块的 `NATS_IDENTITY_BUNDLE_CUSTODY_PASS` 验证临时恢复身份下的包完整性，不代替正式离线保管确认。
