# 全新业务服务部署接线

状态：实施前设计 / NOT_RELEASED。继承 `production-security-infrastructure.md` 的正式依赖、身份、恢复和逐项审核约束。
旧测试账号平台与 New API 数据不迁移；已创建的正式数据库、KMS、身份和加密材料必须保留。
固定 Keycloak 26.7.0、New API v1.0.0-rc.26；采用已确认的正式商城及两个公开 origin，不新增付费设施。

## 串行任务

| 子任务 | 内容与验收 |
| --- | --- |
| C4A | 接收端 workload JWKS 专用 CA 接线；真实 TLS/签名、错误信任与权限拒绝回归，独立提交 |
| C4B | 基于已审源码构建业务制品；独立生产配置只引用既有依赖，逐服务文件/短期身份/真实 subject 映射，不使用开发默认项 |
| C4C | 核对备份与准确空库后执行现有迁移、NATS 固定资源初始化和现有生产安全门禁；保留部分完成状态，不删除正式库重来 |
| C4D | 启动业务与指定 New API，完成原生 KMS fresh-install/轮换证明、权限、健康和内部闭环；正式商城配置通过受控渠道投递 |
| C4E | 公开证书、两个 origin 精确代理与日志隔离；注册/登录、模型与凭据、商城支付回调等适用业务验收；随后精确清理旧测试目标并统一推送交接 |

每项先文档审核、再实现和实际验证、再独立提交；公网放量仍须通过既有发布门禁。
遇到必须保管人真实终端参与的操作只请求该必要步骤，不索取密码、密钥或令牌。
当前不以内部健康、制品构建或静态配置验证冒充用户端验收。

## C4A 接收端信任缺口

目前 `serviceauth.newAuthenticator` 的远程 JWKS 客户端使用默认系统信任，不能验证本次私有 Keycloak CA。
签发代理成功取得令牌不等于业务接收端能够校验；不能通过全进程 `SSL_CERT_FILE` 替换公网根证书，
否则会影响商城、用户登录及其他 HTTPS 连接。

只在 `internal/platform/serviceauth` 增加 `WORKLOAD_IDENTITY_CA_FILE`：可选、绝对规范路径、无软链，
只读挂载中的有界普通文件，禁止组/其他用户写；打开后核对同一文件。最多 1 MiB，仅接受当前有效且
具有签证用途的 CA PEM 公钥证书，不接受私钥、非 CA、垃圾、空包或部分有效包。错误不输出文件内容。
专用 CA 在认证器创建时读取，信任更新需重启消费方；不宣称热更新。

远程 JWKS 使用独立 HTTP client，通过 `oidc.ClientContext` 传入实际 `NewRemoteKeySet`：TLS 1.2+、
保留主机名验证、禁止环境代理及重定向、有界请求超时；显式 CA 只影响该实例，不修改全局 Transport/信任。
未配置 CA 时保留系统根证书兼容。显式 CA 无效，或配置 CA 却未配置 workload issuer 时，认证器失败关闭，
不得回退静态 token；既有 middleware 构造接口不返回错误，因此此处是认证拒绝，不声称所有业务进程会退出。
正式投递与启动前校验必须另外检查该文件及配置是否可用。

保留现有 issuer/JWKS/audience/subject/有效期/精确 permissions 判断，不新增通配授权，不替换真实 UUID。
在本次显式 CA 模式下拒绝带 userinfo、query、fragment 或非规范编码的 HTTPS 地址；
没有显式 CA 的既有地址约束保持兼容。JWKS 可以是显式配置的独立受信 HTTPS 地址，不强制改为同 origin。

验收包含实际中间件使用受信私有 TLS 获取 JWKS 并验证 RSA 签名、正确身份成功、未知身份及未授予权限拒绝，
错误 CA/主机名、跳转不跟随、无代理与全局客户端未受污染；错误/危险 CA 文件失败关闭且不回退静态 token。
Windows 不适用的 POSIX 权限检查必须在同源 Linux 测试中实际执行，不能用跳过结果替代。
本项不引入通用证书框架，不为重构而改动此前已通过的身份预配、Redis 或 KMS 客户端。

C4A 验证结果：Windows 接收认证/签发代理包测试与 vet 通过；同源 Linux 静态测试在无网络、非 root、
只读根目录且仅挂载测试二进制的独立容器中全包通过，无跳过，包括实际 POSIX 权限与软链拒绝。
真实 TLS/JWKS/RSA 经过 HTTP 与 Gin 认证入口，授权成功、未授权 403、未知主体 403、静态令牌 401、
重定向拒绝、精确证书错误及默认客户端不受污染均通过。首轮 Linux 容器因测试日志配置的 max-file=1
与压缩冲突未启动；仅对准确的未运行测试容器重建为 max-file=2，同源测试通过后移除，没有变更正式服务。
审核发现的损坏 PEM 前块被跳过问题已改为只解析首个完整候选块，并覆盖损坏前置/后置和嵌套块拒绝。
架构和差异检查通过；接收端生产制品及正式挂载尚未部署。

## C4A2 既有业务客户端同类 PEM 解析修复（代码与隔离验证通过 / NOT_DEPLOYED）

同类检查发现身份预配、Redis 专用 CA、Token 原生 Transit 专用 CA 的解析也会把剩余整包传给
`pem.Decode`，存在损坏前块被跳过而接受后续有效 CA 的相同边界问题。该项紧随 C4A、在业务制品构建前完成。
只修改三个既有解析循环：逐个首块定界、拒绝嵌套/损坏前置/后置块，保留原文件路径、权限、有效期、用途、
信任隔离和错误语义，不抽取通用框架、不改变密文格式或实际 KMS 状态。每处增加相同三负例和合法多 CA 包
正例，运行各包相关 TLS/连接回归及 vet；不把解析器测试冒充新的正式 KMS 恢复或生产接线。
该修复单独审核与提交；不改动已经通过 C4A 的接收端解析规则。

C4A2 实测：三个解析器及各自三项损坏块拒绝、合法双 CA 保留测试通过独立审核。
Windows 身份预配与 Redis 包全量、Token 相关三项 TLS/解析测试和三包 vet 通过。
同源 Linux 三份静态制品在三个无网络、非 root、只读根目录、仅单一测试文件挂载的隔离容器中串行通过，
包括原有 TLS 错误信任/主机名拒绝、权限/软链和客户端隔离回归，无跳过；测试容器按准确 ID 清理，正式设施未改。
架构和差异检查通过。此处没有运行或替代正式 Transit 密钥轮换、恢复及发布门禁。

## C4B1 生产业务制品构建（构建与制品验证通过 / NOT_DEPLOYED）

先构建、后配置、再迁移和启动。本批仅准备 Linux amd64 制品，不启动业务、不连接正式数据库/KMS、不修改公网。
固定 Go 1.26.5 和现有 `account-platform/Dockerfile` 的 scratch/UID65532 运行形态，保留 nomsgpack、trimpath、
版本/提交标识、原健康检查、token-policy-bootstrap 与 migrations。应用版本读取根 VERSION，不自行升级。

准确 SERVICE 清单为 15 项：account-service、identity-service、billing-service、token-gateway、token-portal、
token-credential-portal、token-command-executor、event-relay、migrate、event-resources、account-security、
token-governance、token-release-prepare、hold-recovery、event-replay。三个领域 relay 共用 event-relay 制品，
各迁移角色共用 migrate 制品；工具构建不等于授权执行。已有七身份代理和 Keycloak 镜像不重建，
不构建 mock-newapi 或 payment-simulator；New API 沿用已确认的独立固定版本制品。

当前服务器容量已低于 20% 预警线。为避免 15 个 SERVICE 重复保存下载/编译缓存，只调整现有 Dockerfile
构建阶段：SERVICE/VERSION/COMMIT 参数移至依赖下载之后，Go module 和 build cache 使用两个固定命名的
BuildKit cache mount（sharing=locked），每次编译都显式重新执行；缓存不进入最终镜像，不配置外部缓存上传。
构建镜像支持公开 `GO_BUILD_IMAGE` 参数，默认仍为 Go 1.26.5-alpine；本次正式构建强制已核验的不可变摘要。
不改变公开二进制接口、运行用户、依赖版本或编译标签，不新增发布框架。

冻结已审候选源码树，只导出 Dockerfile、go.mod/go.sum、cmd、internal、migrations 的已跟踪公开内容，
按 LF 归档并复核传输 SHA-256；排除站点 env、凭据、私有记忆、未跟踪文件和 Git 元数据。每个镜像使用新唯一
标签及制品清单，不覆盖已有标签或使用 latest；逐个构建，成功记录镜像 ID/源码树/版本/构建参数，失败保留现场。
每个构建前复查磁盘，剩余低于 8 GiB 时停止后续构建并先评审容量处置；不自行删除正式备份、卷或其他服务镜像。
构建阶段固定 GOMAXPROCS=2、GOMEMLIMIT=1GiB，限制 Go 默认并行度并提供软内存目标，不冒充 cgroup 硬限制；
每个构建前检查 MemAvailable 至少 2.5 GiB，低于该值不开始下一制品，运行镜像不继承这些构建环境变量。

验收先检查 Dockerfile 的共享缓存/参数顺序与原运行合同，再实际构建并核对 15 项镜像 UID、入口、版本标识和
三项二进制及迁移文件存在；使用未启动的准确测试容器导出文件后按 ID 删除，不通过执行业务入口来验证文件存在。
只有公开 Go 依赖下载允许使用既有镜像代理，构建不挂载正式材料或 Docker socket 到业务镜像。
后续 C4B2 单独细化生产配置、文件与实际 UUID 映射及容器边界，构建通过不提前放行迁移或接流量。

依据：[Docker 官方构建缓存说明](https://docs.docker.com/build/cache/optimize/)。

首镜像已经构建，但初版私有验证器错误假定二进制包含 `version=…` 的 ldflags 文本，因而在制品检查时
误拒；Go trimpath 制品中实际是链接后的版本/源码树常量，不保留该参数文本。未启动任何业务容器。
保留原始脚本、构建日志、计划、首镜像和未启动的检查容器，独立审核窄范围续建：先完整复验已有首镜像，
再构建余下 14 个原定新标签，不修改冻结源码或重建首镜像。所有主程序与两个公共工具均核实际 Go buildinfo
中的准确 cmd path、Go 1.26.5、Linux/amd64、nomsgpack、trimpath 和 CGO=0；七个使用 httpserver 的服务
另核实际链接版本与源码树常量。其他工具只证明构建来源和工具链，不声称存在运行时版本端点。
构建参数、冻结源码树和归档摘要关联到独立制品回执，不能以静态常量检查代替服务启动后的实际版本响应。

现场还发现 Docker Hub 的固定引用解析每次约两分钟，依赖已缓存仍重复发生。允许在精确单镜像断点
将后续构建改用公开 ECR 的同一不可变摘要；先独立验证远端 manifest 内容摘要及本地 image ID/所有层一致。
已经构建的镜像不重建，活动子构建不被终止；父执行器准确暂停，待子构建确认成功退出后记录并退出旧执行器。
新续建器必须绑定原候选/归档、已有完成回执、最后已构建镜像及新的来源引用，独立审核后才能续建剩余项。
该变更只选择同一制品的公开下载来源，不更换工具链版本、不修改源码、不调整主机全局镜像配置。

C4B1 实测结果：15 个 Linux amd64 镜像均已构建并取得独立完成回执，45 份主程序/公共工具的 ELF、
Go buildinfo 与迁移文件摘要全部通过；七个 HTTP 服务的版本/源码树常量通过，两个公共工具在所有镜像中
内容一致。全部未启动的检查容器已按准确 ID 移除，无业务启动、迁移或公网切换。固定源码树与 LF 归档
在换源/续建过程中保持不变，前三份完成回执及前四个已构建镜像未被覆盖或重建。
共享缓存实际复用，换源后样本服务编译约 4.2 秒、导出约 2.1 秒；此为单项样本，不作为全量耗时承诺。
构建结束磁盘仍高于本批 8 GiB 停止线，但低于 20% 容量预警线，仍须继续容量监测。
三项构建合同、架构与差异检查通过。业务配置、容器内实际连接、运行版本接口及完整业务验收仍待后续步骤。

## C4B2 生产配置接线（设计已审，待实现，不启动）

使用独立 production 业务 Compose，不合并开发 Compose，不重新定义或重建已验收的 PostgreSQL、
Keycloak、Redis、NATS、KMS 与七个签发代理。业务镜像只能引用 C4B1 实测清单中的精确 image ID，
禁用拉取/现场构建；生成配置时缺少必需值即失败，不写开发默认值、模拟器或占位 UUID。
本项交付模板、配置/材料清单和静态校验；迁移、角色启用及真实容器启动仍归 C4C/D。

按现有进程职责逐项配置，而非给所有进程复制相同环境：

| 消费方 | 依赖与身份边界 |
| --- | --- |
| Account | Account 库身份；自己的短期 workload 文件；接收端私有 JWKS CA；商城/SMS 与现有业务签名配置；不挂 NATS seed |
| Identity | 自己的短期 workload 文件；用户域 provisioner 专用 secret 文件及 CA；独立 Identity Redis 身份；不挂业务 DB/KMS/NATS |
| Billing | Billing 运行库身份、自己的 workload 文件；billing-command-executor NKey；商城支付配置；不挂迁移身份 |
| Token Gateway | 新 release-scoped gateway 库身份、自己的 workload/KMS sink；不挂迁移/credential 身份或 NATS seed |
| Token Credential Portal | 与 gateway 分离的 release-scoped credential 库身份及自身 workload/KMS sink；只使用凭据专用 OIDC client，不挂普通 Portal 会话或 NATS seed |
| Token Command Executor | 自己的 workload 文件和 token-command-executor NKey；调用 Account/Token 管理接口，不挂数据库/KMS |
| 三个 Event Relay | 各自领域库身份及对应 outbox-relay NKey；不共享全域种子、不挂工作负载签发 secret |
| 普通 Portal | 自己的 Redis 身份、公开用户 OIDC client 和业务地址；不挂数据库/KMS/workload/provisioner secret |
| 一次性工具 | 使用独立显式 profile、restart=no；各工具仅挂当次必要库身份或 event-resource-operator NKey，不随业务启动 |

接收端规则沿用既有精确权限集合，将服务名映射到正式签发回执中的真实 subject UUID，并验证七个主体
互异、服务名一致。仅消费者自己的轮换目录可只读挂载；不能挂父目录、签发 bootstrap 或 custody。
所有私有 CA 单独作用于对应客户端；用户 OIDC 仍以公开用户域为 issuer，不暴露 master/workload 管理入口。

数据库使用原生 pgx/pgconn 已支持的无密码 DSN 与 `PGPASSFILE`，DSN 固定目标库/用户、
`sslmode=verify-full`、`host=postgres` 和只读公开 CA；密码文件仅有该消费者所需精确匹配项，
0600/消费 UID，不使用 `*`。Token 两个 release-scoped 登录身份必须由后续现有 release 工具准备并验证，
不得先用旧 token_gateway 或 migrator 启动业务。缺少 release 角色时配置状态明确为待准备，不冒充可部署。
Identity provisioner 使用已完成的 secret-file 接口；Redis URL 和其他仍采用环境接口的业务秘密只进入
受控的逐服务私有文件与消费进程，不进入公开 Compose、命令行、Git 或配置/日志输出。
该兼容方式仍处于宿主 root/Docker 管理员信任边界内，不宣称 Docker 元数据对其隐藏秘密。

运行容器固定非 root、只读根目录、cap-drop ALL、no-new-privileges、core=0、有限 tmpfs/日志/CPU/内存/PID。
网络按现有 credential-control/provider、governance-control/operations 和普通业务职责拆分；
不能为了访问数据库把凭据服务接入普通 backend。对已运行依赖的新增网络连接须另列准确容器/网络和验证，
不得通过开发 Compose 重建依赖；仅有网络名称不等于出口防火墙，实际可达性由后续验收证明。
本项不得监听公网；若预置入口端口只绑定回环并先核对无占用，正式外部代理仍在 C4E 单独切换。

现有生产门禁保持：至少三个实际响应的独立时间源、真实管理员值班 UUID、正式商城/SMS 必需配置、
版本/能力开关与审批策略一致。不得填写随机用户、虚构时间源、把 APP_ENV 降为 development 或关闭隔离
来获得健康状态。缺失业务材料仅记录名称和责任方，不打印秘密；新的业务签名材料走既有加密备份机制，
不复用旧测试签名密钥，不新增密码托管设施。

验收要求：正例模板可合并、所有镜像与挂载/身份/网络符合清单，负例覆盖缺字段、错误主体、共享秘密父目录、
开发项、模拟器、明文依赖 URL、凭据加入普通网络和工具默认启动。实际密码文件连接及跨库/错 CA 拒绝
在 C4C/D 使用原生消费者验证；静态检查不取代真实连接或完整发布门禁。

### C4C 已识别的前置接线项

现有生产 PostgreSQL HBA 只允许初始化阶段的固定 Token 角色，新 release-scoped 登录角色尚不在白名单。
后续准备准确两角色后，须单独审核对目标 Token 库、这两个明确角色、实际来源网段和 TLS/SCRAM 的窄范围
规则更新与 reload；不使用通配角色、扩大为 host all 或修改密码认证方式。保留原角色拒绝及原有依赖连接的
回归检查，不能以角色 SQL 已创建声称可登录。DBA 工具执行位置和本机受控连接方式仍须细化，不能临时开放
超级管理员 TCP 入口。该段只登记已识别前置项，不授权执行迁移、角色创建或数据库配置修改。
