# 正式环境安全基础设施补建

状态：实施中 / 整体 NOT_RELEASED；独立 KMS 已加密初始化，业务尚未切换。2026-09-07，部署负责人明确授权将缺失的 KMS 和服务身份签发设施纳入全新部署。

## 需求与范围

本变更补齐既有生产 Secret Provider、短期 workload 身份和 APT-137 环境前置，不另造业务授权体系。部署负责人已确认旧账号平台及 New API 是测试数据，不保留、不迁移；该豁免只适用于本次明确目标，不能删除其他系统数据，也不能豁免新环境的密钥保护、备份与恢复要求。正式商城配置仅通过受控秘密渠道注入。

1. KMS 使用 OpenBao Transit（Vault 兼容 API），不自研密码算法或伪装远端本地 AES。AES-256-GCM、随机 nonce、强制 AAD，密钥禁止导出与明文备份；业务仅持有密文及 key ID。
2. 复用 Keycloak，以独立 workload realm 和逐服务 confidential client 的 client-credentials 流程签发 JWT。最长 15 分钟，目标 5 分钟，后台自动续取；每个客户端只能代表自己的固定 subject，禁止用户登录、通配权限和共享静态业务 token。
3. 保留接收端既有 issuer/JWKS/audience/subject/精确 permissions 验证。实际 service-account subject 必须与部署授权表一致，不能用改写任意客户端 sub 的通配 mapper 绕过绑定。
4. KMS 和 workload 使用经验证的 TLS。KMS 只对指定服务及受控运维网络开放，无公网管理入口；凭据 Portal 继续使用独立 origin、OIDC client 和 audience。
5. 初始化主密钥前确定解封材料保管人及加密接收方式；不把解封份额、root token 或 CA 私钥留在业务容器、仓库、日志或聊天。初始化后撤销临时 root token。重启后需要人工解封的可用性限制必须明确交接。
6. 当前单服务器方案不承诺跨主机高可用或 HSM 保护。宿主机 root 仍在信任边界内。恢复演练、备份存放位置、密钥保管未落实前不能宣称正式可用。

## 设计

### SECINF-03C-9C 附加身份策略检查（已部署）

04B3 独立审核发现 OpenBao `lookup-self` 的 token policies 与 `identity_policies` 分开返回。把同一最小权限约束同步到既有 Agent 现场检查/持续监测和备份捕获器：额外 entity/group 策略、非数组或 null 必须拒绝，缺省及空数组合法。只收紧观察/捕获前置，不修改既有 KMS 策略、角色、密钥或 SecretID，也不重新初始化、解封或索取 custody。先单元及实际隔离身份附权负例通过，再发布监测脚本/备份工具并只读验证正式三身份，最后独立提交。若正式检查出现拒绝，保留失败状态并调查，不自动撤权或放宽门禁。该项不代表新业务已部署。

验证与发布（2026-09-08）：Go 工具模块全量测试及 vet 通过，operator 10.646 秒；Python 13 项 11 通过、2 项 Windows 不适用的 POSIX 用例跳过。真实隔离双 Agent 验证 Python checker 对附加 entity 策略拒绝、移除恢复；真实 Linux operator 在两个全新 TLS/Raft 实例中验证备份 AppRole 附权后捕获拒绝且零文件，移除后撤销初始 root，再完成加密捕获、离线验证及恢复，三个原有实际通过 marker 齐备，退出 0。独立复审通过后核验新旧 SHA，保留原制品并原子替换精确监测脚本与备份二进制，保全原 owner/mode；正式三身份 `--lease-only --require-renewal` 通过。未手动生成正式快照，未调整调度、策略、角色或密钥；新备份二进制由既有小时计划使用。

### SECINF-04C 生产依赖接线（串行，文档先审）

现场差距：现有 Compose 基础文件仍是 development、Keycloak start-dev、明文 PostgreSQL/Redis/NATS，不能仅更换镜像即宣称生产配置完成。不得为缩短上线时间关闭既有生产身份或 TLS 门禁。现有正式 KMS 与其他团队服务保持独立，不纳入重建。

2026-09-08 部署优先调整：负责人要求快速推进部署。剩余工作以服务实际启动、身份与密钥安全、业务验收及切换为关键路径；冻结非上线必需的新工具、通用化重构、自动化平台扩展和 UI 优化。复用已通过独立审核的证书、NKey、资源初始化、原生 KMS 和发布门禁成果；能由既有命令和可核验运维步骤完成的接线，不另外开发通用框架。该调整不削减既有业务范围、恢复要求或生产安全约束，不把缺少证据的检查改为通过。

推进以三个可观察里程碑报告：①生产依赖可用（04C2）；②真实身份与全新业务服务可用（04C3/04C4）；③外部入口和业务回归通过后完成旧测试实例清理、统一推送与部署交接（05C）。原任务编号和逐项验收保留，但不为文案、格式或已冻结且未受影响的代码重复启动全量审核/测试；每个实际变更提交仍需独立审核，影响范围变化或集成失败时补跑必要回归。当前仍为第一里程碑实施中，不以本优先级调整或代码提交计作现场部署完成。

| 子任务 | 验收边界 | 状态 |
| --- | --- | --- |
| 04C1 | NATS 资源初始化与业务运行分离：生产 broker 只读取并校验既定 stream/consumer，业务不得修改共享 stream；开发兼容、缺失/漂移拒绝、真实 JetStream 权限正负例 | 代码与真实隔离验证通过 / NOT_DEPLOYED |
| 04C2 | 私有 TLS、逐服务 NATS 身份与 ACL、PostgreSQL/Redis 身份及连接校验；公开模板与私密投递分离，不发布依赖公网端口 | A/B 代码与隔离验证完成；C/D 待实施 |
| 04C3 | Keycloak 生产启动、用户与 workload realm 分离、七个服务真实 subject 及逐服务短期文件接线、凭据专用 OIDC client；真实签发/过期/错角色拒绝 | 待依赖就绪 |
| 04C4 | 新业务镜像及生产配置组合、空库迁移、原生 KMS fresh-install prepare/保管人轮换/verify、正式外部入口与商城验证 | 待前置验收 |
| 05C | 精确确认旧测试服务/数据库/卷，按既有授权清理，统一切换与公开入口回归；保全正式 KMS 和范围外系统 | 待切换门禁 |

04C1 需求：当前每个 `NewNATSBroker` 都执行 `CreateOrUpdateStream`，每个消费启动也执行 `CreateOrUpdateConsumer`，无法直接配置只允许业务收发的生产最小权限。生产固定既有 `ACCOUNT_PLATFORM` stream 和五个既有 durable consumer，不新增事件或消费者；资源由一次受控初始化负责，业务启动只能查询/验证。保留当前非生产开发/测试自动初始化，不改变事件格式、去重、保留期、ACK、重试和死信语义。三个 relay 只发布各自领域事件；计费与 Token 命令执行器只使用其既有 consumer，不能借初始化权限修改共享保留策略或扩大订阅范围。

04C1 设计与任务：先提取既有 stream/consumer 配置为唯一规格；生产通过显式的 broker 配置（由各 CLI 已读取的运行环境传入，不在底层隐式猜测环境）选择 provisioned 模式，禁止运行时创建/更新，缺失或关键规格漂移返回固定配置错误且不降级。独立受控初始化入口仅创建缺失的固定资源、已有资源只校验，禁止 update/delete/purge 和任意名称/订阅参数；失败保留已创建部分，按同一规格重入，不覆盖已有资源。业务与初始化使用独立身份，初始化身份不投递到业务。定义核验字段覆盖名称、subjects/filter、存储、保留策略/期限、去重窗口、ACK/重试/无限重试语义及消费权限相关字段；不以存在性代替一致性。测试先覆盖已有行为兼容、精确配置及零变更拒绝，再在独立真实 NATS TLS/JetStream 中验证业务无 create/update 权限仍可发布/消费/确认，跨领域发布和访问其他 consumer 拒绝，初始化重入不覆盖漂移。每批代码经原独立审核者通过后单独提交；仅 04C1 完成不代表 NATS 正式部署。

04C1 固定消费矩阵（所有 consumer 属于 `ACCOUNT_PLATFORM`，无额外 push 订阅）：

| 运行角色 | durable 名称 | filter | MaxDeliver |
| --- | --- | --- | --- |
| billing-command-executor | billing-command-executor-v1 | account.approval-command.authorized.v1 | 12 |
| billing-command-executor | billing-refund-executor-v1 | billing.refund.requested.v1 | 12 |
| token-command-executor | token-model-lifecycle-executor-v1 | account.approval-command.authorized.v1 | -1 |
| token-command-executor | token-catalog-rollback-executor-v1 | account.approval-command.authorized.v1 | -1 |
| token-command-executor | token-credential-revoke-executor-v1 | account.approval-command.authorized.v1 | -1 |

每个运行角色仅校验自己实际使用的 consumer，不为 relay 或 Token 授予其他 consumer 查询权限。三个 relay 保留 `account-platform.dead-letter.<本域事件>` 发布；计费保留两个既定 filter 对应的死信发布，不把“各自领域”误解为阻断既有失败闭环。初始化 CLI 默认只读 check，只有显式 create-only 才可创建；并发已存在或创建失联仅可回读核验，不用 update 重试。create-only 是工具行为约束，NATS 部分 consumer 创建/更新共用 API subject，不能声称仅 ACL 就禁止受控初始化身份更新。

`production` 与 `production-*` 运行强制 provisioned；未知模式或在生产要求 auto 必须拒绝。唯一规格按当前 NATS 实际规范化核验：BackOff 首项 1 秒会覆盖 AckWait 30 秒；默认容量/副本/等待等服务端补全值不能与安全属性混淆。除显式配置外，subject transform、mirror/source/republish、rollup、direct get、push deliver subject、额外 filters、起始序号/时间、headers-only、pause/inactive 等影响隔离与投递的字段必须保持既有安全语义，非允许规范化的漂移拒绝。C1 实际 TLS 测试所需的显式 CA 可通过窄 broker 配置注入，真实域名验证、错误 CA 拒绝，不加跳过校验开关；证书正式签发/投递仍归 C2。真实矩阵覆盖发布、拉取、ACK/NAK/双 ACK、死信以及跨 consumer 拉取/ACK、直接读取 stream 消息和创建/更新的拒绝。

04C1 初始化入口同步登记中央 CLI 授权目录，使用固定策略 `workload.fixed-event-resources-check-or-create-only`，只增加已说明的部署入口，不新增 HTTP/任务/事件权限。合同测试将实际 CLI 数量 19 更新为 20，并验证删除该项仍被授权覆盖门禁拒绝。生产连接使用 `_INBOX.<固定运行角色>` 的独立回复前缀，服务端 ACL 只允许订阅本角色前缀，防止借共享 `_INBOX.>` 旁听其他 consumer 的拉取回复；初始化工具使用自己的前缀。真实验收还直接运行 `Consume` 与 `ConsumeUntilResolved` 的合法事件处理/重投，不以 Fetch 原语代替正式入口验证。

04C1 实测（2026-09-08）：受影响 events 与三个运行 CLI、初始化 CLI 的 Go 测试及 vet 通过；授权覆盖六项合同通过，实际 339 HTTP/27 task/6 event/20 CLI，未扩大业务授权。Windows NATS 2.11.9 与 Linux NATS 2.11.17 的独立真实 TLS/JetStream 测试均取得 `NATS_PROVISIONED_TLS_ACTUAL_PASS`，无跳过。覆盖空资源只读拒绝、create-only/重入、固定配置核验、五种受限角色、错误 CA/明文连接拒绝、逐角色收件箱、正常发布/拉取/NAK/双 ACK/死信、正式两个消费入口失败重投、跨领域与跨 consumer/stream 管理/直读拒绝、已有漂移不覆盖及开发自动初始化兼容。仅使用临时证书、随机测试身份、loopback 端口和临时存储；正式身份/ACL 投递尚未执行，不将该证明当作生产就绪。

### SECINF-04C2 私有依赖安全配置（分步验收）

04C2 不增加第三方 CA 服务，也不借用正式 KMS 的 CA、Transit 密钥或运维身份。沿用已有标准 X.509、age 加密保管和受控目录交付能力；只补部署所需的固定证书、身份和配置。正式密钥生成及服务替换在对应代码和隔离验收通过后执行，不把测试证书用于正式环境。

| 顺序 | 内容 | 验收 |
| --- | --- | --- |
| 04C2A | 独立私有依赖 CA 与固定五个 TLS 服务叶证书，根私钥只生成于内存并立即 age 加密；公开恢复接收方复用，不读取恢复口令/KMS 材料 | 加密恢复、逐叶密钥隔离/用途/名称验证、真实 TLS、错误输入及覆写拒绝 |
| 04C2B | 逐角色 NATS NKey 身份和固定 ACL、私密种子文件投递；与原 JWT credentials 互斥且不破坏兼容；实例级初始化身份与五个业务身份分离 | 真实 NKey 挑战认证、逐角色收件箱/收发/死信、越权拒绝、失效旧身份拒绝 |
| 04C2C | PostgreSQL SCRAM+verify-full、Redis TLS+独立 ACL、NATS TLS 与持久化配置，生产依赖的独立 Compose 文件；禁用模拟器、无公网依赖端口 | 实际配置解析及隔离三依赖启动、错误 CA/明文/错身份拒绝、最小角色成功；不复用旧测试数据库 |
| 04C2D | 正式受控目录/逐制品摘要与权限投递、私有网络和依赖启动、监测/证书更新与恢复交接 | 现场只读配置和 TLS/权限/备份验证；旧业务切换仍归 04C4/05C |

04C2A 设计：在现有独立 Go 工具模块新增窄基础设施 PKI 入口，复用既有受控读写、标准加密和证书实现，不修改 `kms-pki` 的默认输入或输出。仅接受公开 recipient 文件及指纹、全新私有输出目录；不接受任意 DNS/IP、客户端用途、CA 文件、远端地址或命令。新 CA 为 P-256、5 年、禁止中间 CA；五个独立 P-256 叶证书固定为 `postgres`、`redis`、`nats`、`keycloak`、`newapi`，仅对应单个 DNS SAN、serverAuth 和 90 天有效期，不含 KMS 或公开域名。TLS 连接依靠私有网络的稳定服务别名，禁止用跳过主机名校验解决连接问题。

根私钥只出现在用途明确为 `pinvou-platform-internal-ca` 的 age 密文包中，禁止明文 CA 私钥输出。每个服务有独立私有叶证书目录，运行容器只能挂载自身叶证书/私钥及公开 CA，不能挂载根包或其他服务的叶私钥。先持久化根加密包再写叶制品，逐文件独占创建、同步/读回，最后生成包含公开证书及文件摘要的就绪清单；任何中途失败保留未完成目录，不能因部分文件存在而放行部署。Windows 输出父目录 ACL 沿用既有保管人和 SYSTEM 限制；Linux 检查 0700 父目录、0600 文件及非符号链接。

证书更新不依赖运行容器持有签名私钥：到期前至少 30 天告警、14 天进入部署阻断；受控生成新独立 CA/五叶包 → 先投递新旧公开 CA 信任合集并滚动验证 → 逐服务切换新叶证书并核对实际序列号和 TLS → 全部客户端就绪后移除旧信任。旧叶未到期且未撤权时可回滚叶证书，不能先删除旧信任再切换。每次新根密文都归既有离线保管流程；未完成信任更新、实际 TLS 或离线保管确认不标记现场完成。该方案明确是受控 CA/证书包替换，不冒称自动续签；现场监测及切换步骤在 04C2D 验收，正式启动前必须具备可执行的交接说明。

双信任与旧叶回滚仅适用于正常到期更新。叶密钥或 CA 疑似泄露时不得因“尚未到期”回滚旧材料或长期保留双信任；先隔离受影响服务/流量，生成独立新包，按应急停机窗口在五服务及所有客户端撤除旧信任并重新置备，逐一验证新身份后再恢复，不声称已有 CRL/OCSP 或自动撤销。应急停机和恢复步骤由 04C2D 现场交接。

04C2A 验收同时生成两套独立测试包：验证两根互不相同、五叶各自唯一私钥、精确单 DNS/EKU/pathLen 及密钥对应；通过隔离真实 TLS 验证旧信任→双信任→新信任，错误主机名/CA/clientAuth 拒绝。仅测试用临时 age 身份解密根包，验证 purpose、公私钥对应、错误 recipient 与损坏拒绝，正式生成不读取 custody 私钥。公开清单覆盖根密文、CA 与五叶全部十二个文件的 SHA-256；`READY` 最后写入且绑定清单摘要。没有完整匹配清单/READY 的部分目录不可覆盖或部署。

04C2A 实测（2026-09-08）：新增独立离线 `infrastructure-pki`，原 KMS PKI 接口不变。冻结最终文件后，Windows 工具模块全量测试与 vet 通过（operator 46.133 秒、PKI 6.800 秒），架构守卫与 diff 检查通过；同源 Linux PKI 二进制六项测试全部通过，实际双包 TLS 主用例 0.20 秒，取得 `INFRASTRUCTURE_PKI_TWO_BUNDLES_TLS_PASS`。五张 serverAuth 叶均通过显式客户端证书回调参与双向 TLS 并被服务端拒绝，不以客户端未发送证书冒充用途拒绝。验证含 Linux 私有权限与符号链接拒绝、完整清单/就绪绑定、恢复包认证、独立密钥和原 KMS PKI 回归。早期一轮测试因与文件格式化交叉读取而失败，不计入通过结果。仅临时测试身份与 loopback TLS，无正式生成、投递或业务切换。

04C2B 按两个有独立验收的提交串行实施：B1 为现有消息客户端接入 NKey 私密文件及真实认证/撤权/兼容验证；B2 才生成固定六身份包及服务端固定 ACL、加密保管和逐角色投递说明。B1 不宣称正式身份已经创建，也不以测试 ACL 代替最终生产配置。

04C2B1 需求与设计：保留既有 `NATS_CREDENTIALS_FILE` JWT 接入，增加 `NATS_NKEY_SEED_FILE`；两个文件模式互斥，文件身份与 URL 用户名/密码或 token 同时出现也拒绝，不通过优先级悄悄选择。NKey 模式强制经验证 TLS，即使非生产也不允许明文发送挑战应答。采用既有官方 `nkeys`/`nats.go` 标准用户 NKey、挑战签名和 `nats.Nkey`，不自研签名、不发行 JWT；不接受 operator/account/server 类型种子。文件必须绝对规范路径、非符号链接、普通有界文件，Linux 文件不允许组或其他人读写执行且直接父目录私有；Windows 部署由受控 ACL 保证。每次连接/重连签名都重新校验路径、权限、文件身份、大小及种子用途，读取后尽力清零并 Wipe；固定初次公钥，文件被换成其他身份时必须拒绝，不能沿用旧公钥签名或悄悄换角色。仅输出固定错误，不记录种子、URL、签名或原始认证错误。

NKey 种子本身没有租期，不能把它称作短期 workload JWT。正常轮换由受控新身份包实现：服务端短时并存新旧公钥且 ACL 不变 → 按角色替换私密投递并重建客户端 → 验证新身份 → 撤除旧公钥并让既有旧连接失效 → 验证旧种子不能重新连接。仅替换文件不足以完成轮换；疑似泄露先撤旧身份/断开其连接，不等待平滑窗口，也不允许回滚旧身份。B1 在隔离 NATS 中验证完整重连/旧连接失效；B2/C2D 负责正式包及现场执行和审计。

04C2B1 验收：复用 C1 的真实隔离 TLS/JetStream 矩阵，以标准 NKey 替代测试密码再跑五角色的发布/消费/ACK/NAK/死信与越权拒绝，同时保留密码/原 JWT 选项构造兼容测试；单元覆盖双文件/URL 混用、无效/非用户种子、不安全文件、同路径替换、公钥绑定及回调不泄漏错误。另用隔离实例验证重连再次读取种子，换身份文件不被自动接受，服务端撤权后旧连接/旧种子失效，新身份可正常连接。所有测试使用临时随机身份和独占临时存储，不操作现有 broker、正式密钥或其他团队服务。

04C2B1 实测（2026-09-08）：修正无协议 URL 的身份混用检查，按官方客户端规则先补协议再逐地址核验，保留无身份裸主机的 JWT 兼容；默认异步错误只记录固定 `NATS_ASYNC_OPERATION_FAILED`，不输出原始错误/主题。最终 Windows events 全包实际验证通过（14.341 秒，撤权 3.24 秒），四个受影响 CLI 测试与五包 vet 通过；Linux 最终同源二进制 NATS 测试全部通过，撤权 2.64 秒、password/NKey 五角色矩阵分别 2.08/2.07 秒，取得两类真实 marker，无实际测试跳过。授权合同六项、架构与 diff 检查通过。曾有一轮 Windows 等待新身份重连超时，未计入通过，也未断言生产根因；单项原稿连续三次通过后，将 fixture 的新 broker 经 TLS 验证就绪与既有客户端重连分为独立有界阶段并增加不含秘密的失败诊断，保留各阶段 10 秒及全部撤权断言，最终两平台完整重跑通过。正式身份包/ACL/投递尚未实施，JWT 只验证既有官方回调兼容，不将其表述为真实 JWT 签发联验。

04C2B2 需求与设计：身份生成与消息授权各自保留职责边界。现有离线工具模块新增 `nats-identities`，复用 age 加密保管并使用仓库已采用版本的官方 `nkeys` 库，不在业务进程生成私钥。配置仅接受恢复公钥路径/指纹和全新私有输出目录；固定生成 `account-outbox-relay`、`billing-outbox-relay`、`token-outbox-relay`、`billing-command-executor`、`token-command-executor`、`event-resource-operator` 六个不同用户 NKey，不接受自定义角色或权限。全部种子只在内存汇总并先写 age 包（独立目的 `pinvou-nats-service-identities`），再写各角色独立私密种子文件和公开 `public-keys.json`，最后写八文件摘要清单及绑定清单的 `READY`。公开映射与加密包带版本/目的标识；逐一核验种子与公钥对应，禁止部分结果、符号链接、不安全权限、已有目录覆写。保管人与 SYSTEM 的 Windows ACL、Linux 私有父目录/0700 子目录/0600 文件边界沿用 C2A；失败保留部分材料，不把未完整匹配清单当作可投递包。

ACL 由 Account Platform 消息模块基于 C1 固定 stream/consumer 规格构建，离线工具只知道六个固定身份名称，不复制消费主题/重试等领域规则。`event-resources` 增加互斥的纯离线公开授权配置渲染模式：读取公开六角色映射及可信 SHA-256（不读取 seed 或联网），验证版本/目的/精确角色集合、合法用户公钥及全局唯一性后，写全新输出文件；与资源 check/create-only 不可混用。仅显式正常轮换重叠模式可附加第二份有可信摘要的旧公开映射，六角色 ACL 完全相同且十二公钥不重复；不接受任意 users/subjects/权限模板。可信摘要由 C2D 的已验证清单独立传入，不能自动从同一不可信文件生成摘要后冒充校验。配置仅包含公开 NKey 和固定 publish/subscribe 规则，无密码、JWT 或种子。

公开映射限 32 KiB，要求绝对规范路径、普通非符号链接且不可组/其他用户写，打开后再次核验文件身份并对实际读取字节校验可信 SHA-256；新旧两份分别验证。JSON 必须闭合，拒绝重复键、额外字段、错类型或尾随数据；角色集合精确为六个。渲染输出在受控父目录排他新建、同步并读回校验，失败保留部分文件且不覆盖，不打印配置内容。离线分支必须先于任何 NATS 环境读取、种子/credentials 文件访问或连接初始化；测试以不可读身份文件和可观察的本机探测地址验证成功/失败均零连接、零私密读取。正常重叠只在受控短时现场窗口使用，撤旧与已有连接退场沿 B1 验收；生成双公钥配置不能标记轮换完成。

运行五角色的权限复用 C1 矩阵和独立收件箱。初始化角色只允许固定 stream INFO/CREATE、五个固定 consumer INFO/CREATE（包括精确 filter 的官方 API subject）及自己的回复收件箱，不允许业务发布、其他 stream/consumer、stream UPDATE/DELETE/PURGE/MSG.GET。consumer 的 CREATE/UPDATE 共用 subject 的限制仍由 C1 工具 create-only/既有完整校验兜底，不能声称 ACL 能区分同资源的创建与更新。初始化种子不投递到普通业务容器，只在串行初始化窗口受控使用；初始化完成后撤掉服务器上临时种子投递和操作连接，离线加密包保留以便未来受控检查。NKey 公钥撤除和已有连接退场仍按 B1 明确操作，不依赖种子文件删除使连接自动失效。

04C2B2 验收与任务：先实现并验证六身份包（仅临时 age 私钥解密测试、用途/角色/密钥一致、不同包不同身份、摘要/部分/权限/覆写拒绝）；再实现固定 ACL 构造与既有 CLI 离线渲染模式，验证错误摘要/缺失或额外角色/非用户或重复公钥/未经显式选择的重叠模式/互斥参数均拒绝且不联网。最后在真实隔离 NATS TLS/JetStream 中实际采用渲染出的规则，验证受限初始化角色 create-only/check/重入及管理越权拒绝、五业务角色既有收发/重试/死信矩阵，正常两包重叠与撤旧策略不扩大权限。仅 fixture 可额外持有与生成包无关的管理身份制造漂移，正式渲染输出不得含此身份。同步运行手册、CLI 授权模式约束及合同测试，经 B 复审通过再独立提交。正式材料生成、逐角色投递和服务器启动仍归 C2D。

04C2B2 实测（2026-09-08）：六身份包、公开映射闭合解析及固定 ACL 渲染已实现，既有 `event-resources` 中央策略更新为 `protocol.offline-fixed-nats-authorization-or-workload-fixed-resources`，CLI 数量仍为 20，不新增 HTTP/任务/事件权限。首轮恢复测试发现官方 `Seed()` 返回 SDK 内部缓冲，`Wipe()` 会破坏该缓冲；已在 Wipe 前取得独立副本，并在落盘前重新解析种子核对公钥，失败轮次不计通过。最终 Windows 工具全模块测试/vet 通过（operator 39.202 秒、身份包 5.544 秒），events 实际全包 22.526 秒、CLI 离线/兼容 5.586 秒及其他受影响 CLI 测试与五包 vet 通过；授权合同六项、架构与 diff 检查通过。

Linux 同源最终三份测试二进制全部通过：加密身份恢复 0.07 秒及实际 POSIX 权限负例、CLI 零工作负载访问/零连接/严格文件校验通过；真实 NATS 直接使用渲染器输出，受限初始化角色 create-only/check/重入与七项越权拒绝、五业务角色全矩阵通过（password 2.07 秒，NKey 2.09 秒）。十二身份重叠均可认证且非法发布拒绝；应用仅新身份配置并受控重启后，保留既有旧连接失效/既有新连接恢复断言，六个旧种子全拒绝、六个新种子全成功，3.04 秒并取得 `NATS_RENDERED_AUTHORIZATION_OVERLAP_REVOCATION_PASS`。全部真实用例无跳过；固定错误日志来自预期拒绝。以上只证明代码及临时隔离设施，不是正式身份生成、离线保管交接、ACL 投递或业务切换。

### Native Transit 适配

既有 `remote` 是自定义 `/v1/encrypt|decrypt` 协议，不等于 Vault API，保留其兼容行为。增加显式 `vault-transit` 模式，通过 `/v1/transit/encrypt/{key}`、`decrypt/{key}` 调用原生 API；不部署自制加密中转服务。

- 使用 `X-Vault-Token`，生产仅允许从绝对路径的短期 token 文件读取；每次调用重新读取，以支持 OpenBao Agent 原子轮换。Agent 与业务分别挂载单服务目录，业务只读，不可接触 AppRole bootstrap 身份。
- AAD 原样 Base64 编码为 `associated_data`，密文原样保存 OpenBao 返回的带版本封装，不自行解密。密钥名限定单路径段；轮换后按持久 key ID 解密，由服务端窄 ACL 决定历史 key 可用性。
- 严格 HTTPS、证书验证、禁止跳转、超时、请求/响应大小限制、结构与封装校验。错误只返回稳定信息，不透传 KMS 请求/响应内容、token 或明文。生产禁止静态 token 和 local fallback。
- Agent 使用逐服务窄授权，仅指定 key 的 encrypt/decrypt update；不得具备 create-key、export、rotate、policy、token 管理权限。密钥生命周期由独立运维身份操作。

### 服务令牌投递

轮换器只负责调用可信 Keycloak token endpoint 并原子写入该服务的 token 文件，不签名、不授予权限。客户端 bootstrap secret 独立保管；业务进程只读短期 JWT。签发结果须先通过 issuer/JWKS/audience/subject/签发时间/过期验证再投递。失败保留尚有效的旧 token，过期后业务认证拒绝；禁止把失败响应写为 token 或静默切换 development。

SECINF-02 按串行可验证交付细分为 02A（投递程序与安全负例）和 02B（独立 realm/客户端配置、真实 Keycloak 签发与接收端联验），两者完成前不标记 SECINF-02 完成。02A 使用 `cmd/workload-token-agent`，一个实例仅服务一个客户端，不接触其他服务秘密。唯一输入参数为非秘密配置文件路径，配置字段为 `issuer`、`audience`、`client_id`、`subject`、`client_secret_file`、`token_file`；后两者必须是分离目录中的绝对路径。客户端秘密每次从只读 bootstrap 文件读取，不接受明文参数/环境变量。

端点由固定 Keycloak realm issuer 派生为 `protocol/openid-connect/token` 和 `certs`，强制 HTTPS、可信证书、禁止跳转及有界响应；只调用 client-credentials，不请求 refresh token。投递前要求 RS256 签名、精确 issuer/audience/sub/azp、非未来 iat、有效 nbf（若存在）、正数且最多 15 分钟的生命周期，并至少剩余 30 秒有效期。不把 `expires_in` 或未验证 payload 当可信时间。正常在剩余有效期约一半时续取；失败按有界退避重试，只记录固定错误阶段，不输出 HTTP 响应或 JWT。保留旧文件不延长其有效期，接收端和 `-check` 始终按签名与实际 exp 拒绝过期身份。

02B 交付无秘密的 realm 配置生成器：固定账号、身份、计费、Token 网关、命令执行器、凭据 Portal、Keycloak 手机验证器 7 个客户端，沿用既有内部 audience；事件 relay 使用 NATS 身份，不为其增建无用途的 HTTP client。配置不预置 client secret、service-account UUID、管理员或测试用户；Keycloak 创建后通过受控管理接口取得真实账户 UUID 与逐客户端秘密，秘密只进入 agent bootstrap 文件，UUID 进入实际部署授权表。生成器不联网、不部署、不修改既有 realm。隔离联验使用全新专用 realm、随机临时管理员、loopback TLS、无业务数据和公网端口，结束后删除该测试容器；测试秘密与正式设施无关。

投递采用同目录临时文件、0600 权限、sync、原子 rename；运行目录必须预先创建且非符号链接，仅该服务的 agent 可写，业务以相同 UID 只读挂载。不能挂载单个 token 文件（会固定旧 inode），必须挂载单服务目录。正式运行宿主为 Linux；Windows 仅用于开发测试，不能将其 rename 的平台差异视为 Linux 持久化证明。`-check` 仅验证已有投递文件，不申请新令牌，供健康探针使用。02B 固定默认 RS256 与 300 秒有效期，关闭标准/隐式/密码授权/设备授权/Token Exchange 和 offline access，仅保留服务账户；读取服务账户真实 UUID，绑定授权表，禁止自定义 sub 或动态扩权。

### 部署顺序

SECINF-03 继续串行细分：03B 交付固定版本的 TLS/Raft/声明式审计/逐服务 AppRole 与 Agent 配置，并以独立实例验证持久化、封存、解封、权限、续租和审计；03C 才处理本机加密初始化响应、正式初始化、离线交接和备份恢复。03B 的随机测试主密钥不用于正式实例，不将其测试结果等同于保管人的正式备份确认。

03B 固定 OpenBao 2.6.2 及既有已验证摘要，采用专用 Raft 数据目录、独立审计目录、非 root UID 65532 和只读配置/TLS。业务只能连接私有 KMS 网络的 TLS API，正式部署不发布 KMS 公网端口；Agent 无 HTTP 代理或监听器。宿主禁用未加密 swap/转储，数据目录和审计目录不得被其他业务挂载。声明式 file audit 始终开启 HMAC 脱敏，不允许 API 新建审计设备；轮转必须重开文件并验证继续留痕，不能静默丢弃审计。

网关与凭据 Portal 使用不同 AppRole 和秘密文件，即使访问同一个业务 Transit key，也独立撤销和审计。业务策略仅允许指定 key 的 encrypt/decrypt（强制 AAD 参数，禁止 runtime 指定旧 key_version 或创建 key），以及本人 token 的 lookup/renew/revoke；不得创建 token、修改其他 token、密钥、策略、认证或审计配置。Token TTL 5 分钟、显式最长 15 分钟，Agent 达到最长周期后重新 AppRole 登录。bootstrap SecretID 为可撤销的持久机器秘密，不冒充短期身份：仅 Agent 可读取，需在交接中登记周期轮换及泄露撤销步骤。短期 token sink 放在单服务受控运行目录，业务只读整个目录，且无法读取 bootstrap。

隔离实例以新建私有 internal Docker 网络运行，不发布任何宿主端口，测试控制进程仅通过该独立网络的已验证容器地址连接，绝不使用既有业务端口/数据卷。初始化 root 与解封材料只在测试进程内存中使用，不输出，不落明文文件；叶证书、临时测试 Agent bootstrap 及 Raft 数据均在具名临时目录，结束精确清理。测试主动缩短租期仅用于观测，公开正式角色模板仍固定 300/900 秒。

2026-09-07 保管方式已获负责人同意：本机口令保护的离线恢复包，异地/离线另存备份，口令分开保管，不通过聊天交付明文。先完成 SECINF-03A 本机托管工具，再进行正式初始化；此子任务不代表 SECINF-03 整体完成。

恢复包采用标准 age 格式，独立工具模块固定 `filippo.io/age v1.3.2`，不改变业务服务依赖。`prepare` 仅在本机终端无回显读取及确认口令，生成 X25519 恢复身份、用 scrypt recipient 加密为 `identity.age`，只输出公钥 `recipient.txt`；`seal` 从标准输入接收材料，用该公钥加密，绝不写入明文文件。`verify` 在内存解密并验证完整性，不打印明文，不接受参数/环境口令。拒绝覆盖已有文件、短口令、损坏包和超限输入；文件写入后 sync。口令以及丢失后的不可恢复风险由保管人负责。加密不防护已被控制的本机；操作前应关闭会记录输入/内存的录屏或调试工具。

本机身份文件不含正式 KMS 密钥；正式材料只能在确认该身份可解密及备份安排后生成。仅初始化托管身份不操作服务器、不初始化 OpenBao、不删除任何数据。Windows 发布目录继承权限需显式收紧至当前用户与 SYSTEM。单人保管不冒充多人分权；长期多人 custody 为后续治理选项，不自行创建未知保管人。

03C 先交付独立的 Linux `kms-initializer`，不向已有离线 `kms-recovery` 程序添加网络能力。初始化器只持有保管人的公开 age recipient 和固定私有 TLS CA；公钥 SHA-256 必须与人工核对的配置一致。目标仅允许无路径/查询/用户信息的 HTTPS origin，无代理、跳转或自动重试。实例编号、目标 origin、CA 指纹、时间和原始响应一并封装在加密包内，用于后续受控解封核对；实例编号是部署关联信息，不代替 TLS 身份验证。

初始化前必须显式确认全新实例和保管人离线备份安排；程序验证目标确实未初始化且已封存。先独占创建私有输出目录、密文文件和同步的尝试标记，再发起一次 `sys/init`（当前单人 custody 为 1/1，不冒充多人分权）。响应只在内存读取并立即加密，文件和目录同步、密文读回一致后才创建成功标记；不自动解封、不配置策略、更不删除/重置服务。所有错误仅返回固定阶段，不透传服务响应、URL 或秘密。

结果不确定必须 fail closed：只要开始初始化请求，超时、断网、非预期状态、损坏/截断响应、写盘失败均不能自动重试。非空已收到响应也保存为加密诊断包，但没有成功标记不能认定恢复材料齐备。所有输出保留且拒绝覆盖。网络请求与服务持久化不具备跨进程/掉电原子性；若服务已初始化而一次性响应丢失，须人工核对这一尚未承载业务的实例并另行决定恢复或重新置备，程序不得自行清库。后续本机人工解封、运维身份、根 token 撤销及真正快照恢复仍是独立验收项。

公开模板及隔离测试 → OpenBao/Keycloak 固定版本与镜像摘要 → TLS/受限网络/保管安排 → 正式初始化和逐服务策略 → 真实 KMS/签发轮换负例 → 维护窗口复核精确旧目标 → 清理测试数据并从空库安装 → NATS TLS/ACL、治理角色及初始化 → 双域登录、模型与计费验收 → 放量。

本次空库安装无需旧测试密文迁移；现有生产安全门禁中依赖旧 key 的轮换证明仍需增加独立 fresh-install 分支及真实新环境轮换演练，不能伪造旧 key 或直接跳过整项 gate。

## 串行任务与验收

03C-2 增加单独的本机 `kms-operator unseal`：保管人在真实终端输入口令，解密 identity 和初始化包只在内存中完成；先校验包的版本、完整状态、实例编号、原始 HTTPS origin、专用 CA 和 recipient 指纹，再请求封存状态。只允许已初始化 Shamir 1/1 实例的单次解封，不发送 root token，不提供 init/reset/migrate 或任意 API 代理。已经解封时不重复提交份额。请求失败或状态不确定只报告固定阶段，不回显响应或重试写入。

本机访问私有 KMS 可经既有 SSH 隧道：可选连接覆盖仅允许 loopback IP 和明确端口，HTTP origin、Host 与 TLS 证书验证仍使用包内原始目标，不把 localhost 当成可信证书名，也不关闭验证。隧道由部署步骤建立，operator 不启动外部命令、不自动配置 SSH/DNS。提取共享的纯离线 age/身份读取能力供两个命令复用，默认 `kms-recovery` 保持无网络，不修改已有加密格式。此子项不代替后续策略/运维身份初始化、root 撤销或快照恢复。

03C-3 独立恢复证据：两个真实 Raft 实例使用不同私有网络和数据/审计目录，恢复目标不能访问源实例。源创建 v1 密文并轮换至 v2，在撤销初始 root 后，以仅具 snapshot read/sudo 的限时独立测试身份取快照，外层 age 加密并落盘；恢复前完整解密验证，错误身份与损坏包必须在恢复 API 调用前拒绝。目标为明确全新的隔离实例，仅此测试流程可使用 `snapshot-force` 替换其数据，不向正式 operator 暴露此操作。恢复后使用源份额解封、验证旧密文与 v2 状态、两端旧 root 均拒绝。该证明不等于正式定时备份、保留策略、保管人恢复交接和生产灾备可用性。

03C-4 私有证书初始化使用独立离线 `kms-pki`：在保管人受控目录中生成 ECDSA P-256 专用根 CA（5 年、禁止中间 CA）与 KMS 叶证书（90 天、仅 serverAuth、固定 DNS `openbao` 和已核对的私有 IPv4 地址）。根 CA PKCS#8 私钥只在内存中产生，直接封装为保管人 age 公钥加密包，禁止明文 root key 文件；服务器只接收独立叶证书/私钥及公开 CA。新输出目录不可覆盖，私有文件写入同步并验证证书链与密钥对应关系后才写就绪标记。正式 Windows 输出父目录 ACL 必须先核对，受控加密包与身份、口令分别保管；叶证书到期前轮换及告警仍纳入 SECINF-04 运行交接。

| 任务 | 交付与验收 | 状态 |
| --- | --- | --- |
| SECINF-01 | 原生 Transit provider、逐调用 token-file、两服务接线；HTTPS、AAD、轮换、权限拒绝、无泄漏、跳转及有界响应测试 | 代码已验证 / NOT_DEPLOYED |
| SECINF-02 | Keycloak 独立服务客户端配置与轮换器；真实签发/轮换/过期/错 subject、audience、权限拒绝 | 02A/02B 隔离验证通过，整体 NOT_DEPLOYED |
| SECINF-03A | 保管人已同意的本机离线托管工具；口令无回显、标准 age、加密身份读回、拒绝损坏/覆盖；在正式初始化前完成 | 代码与隔离测试已验证；custody 备份已确认，正式 CA/初始化加密包已生成并由保管人完成解封 |
| SECINF-03 | 固定版本 OpenBao/Agent 部署、逐服务策略、TLS/网络、初始化与解封交接、备份恢复；明确单节点边界 | 独立 KMS/TLS、三 Agent、root 撤销、定时加密备份及首份离线交接、检测与审计轮转已完成；业务接入未完成 |
| SECINF-04 | Compose/Helm/生产安全门禁原生 KMS 与 fresh-install 适配、真实依赖验收及部署运行手册 | 04A 私有 CA 接线已通过真实隔离联验；其余待实施 |
| SECINF-05 | New API 候选兼容与安全验证、精确清理旧测试数据、空库部署及商城生产联调 | 05A1 来源固定及 05B1 实际空库基础验收完成；完整安全/调用核对验收及切换待实施 |

每项独立自检、验证、提交，不能将模板解析/单元测试当作现场上线证据。2026-09-08 负责人明确选择 New API `v1.0.0-rc.26`，在安全与兼容性验证通过后用于本次正式部署；该决定不将候选版改称官方稳定版，也不授权改用更新 RC。主密钥保管、真实上游模型凭据及任何需新资源采购的选择应由负责人提供或批准。

SECINF-05A 版本固定：核对官方 tag 对应源码提交、官方镜像仓库的多架构 manifest 与服务器架构子摘要，以摘要引用替代漂移 tag；检查官方已知安全公告和本次实际使用的接口/功能边界。镜像拉取与隔离验证不替换旧服务。05B 在独立测试网络/空库下验证非 root 启动、PostgreSQL 配额字段、管理与 Token API、用量记录和安全拒绝；仅使用临时测试凭据和模拟上游，不调用真实收费模型。05C 才在原生 KMS、服务身份、NATS、数据库和外部入口验收齐备后，按精确已核对目标执行旧测试服务清理与全新业务切换。已有正式 KMS 及其他团队服务不在清理范围。

05A1（2026-09-08）：官方 OCI index、amd64 manifest/config 摘要已按原始响应复算，镜像 revision 与 rc.26 发布提交一致；站点缓存返回相同 index 并完成摘要拉取。Compose 与两份 Helm values 同步固定版本，35 项相关 contract、架构守卫和 diff 检查通过。仅完成来源与配置验证，不代表完整安全审查或业务部署通过。详细公开摘要见 `deploy/runbooks/newapi-deployment.md`。

05B1 空库基础验收：提供显式 opt-in 的 Linux Docker fixture，镜像必须预先下载并匹配固定摘要；每次创建随机命名且带唯一所有权标签的 internal 网络、tmpfs PostgreSQL 与非 root 只读 New API，不发布任何端口，宿主 runner 仅探测经过网络归属核验的临时容器私有 IP。测试凭据随机生成、仅内存及 Docker 私有配置中使用，不输出 HTTP 正文、环境变量、数据库内容或原始错误。验证全新初始化、重复初始化拒绝、登录、未认证管理/模型请求拒绝、额度列为 bigint、超过 32 位额度读写及业务进程重启后状态。仅精确清理此次创建并再次核对标签的容器和网络；不使用卷、已有数据库、旧账号凭据、正式 KMS 或付费上游。完整 Token/Channel、JSON/SSE 与核对为后续 05B2，不以基础验收替代。

05B1（2026-09-08）已实际运行最终脚本并取得 `NEWAPI_FRESH_FIXTURE_PASS`，7 项基础检查全部通过；6 项离线安全保护测试、架构守卫及 diff 检查通过。退出后再次查询 fixture 标签，无残留容器或网络。测试仅覆盖用户 quota 的 bigint/50 亿值与应用重启，不包含 Redis、数据库重启恢复或完整漏洞验收；未切换任何旧服务。

05B2 调用与核对验收：复用 05B1 的临时资源边界，在同一个独立 internal 网络新增仅内存 Redis 与由本仓源码编译的 Mock New API 上游。通过 rc.26 真实管理 API 创建 Channel、模型限定 Token 和独立核对用管理 Token；验证模型发现、JSON/SSE 的 usage、`X-Oneapi-Request-Id` 与消费日志精确关联、重复只读查询不重复扣费、模型越权/撤销拒绝及凭据元数据脱敏。测试不调用真实模型、不将 mock 作为正式上游、不改写正式配置；基线 fixture 仍可单独运行。结果必须区分上游接口兼容与账号平台完整业务链路验收，不以模拟输入证明真实供应商服务可用。

05B2（2026-09-08）：实际 rc.26 + PostgreSQL 17 + Redis 8.2 + 本仓 Go Mock 上游取得 `NEWAPI_BUSINESS_FIXTURE_PASS`，共 14 项检查通过。发现并验证新增渠道在默认 Redis/进程缓存模式下需要等待 60 秒同步周期；已将等待及正式路由开放前健康验收记入运行手册，没有通过关闭缓存或自动重发模型请求规避问题。该步骤不是生产切换，也不是账号平台账本完整验收。

### SECINF-04B 原生部署接线与全新安装门禁（串行实施）

需求：原生 KMS 业务容器只能取得自身短期 token 和公开 CA；不再为满足旧模板插值而提供静态 KMS token。保留既有 remote 协议的部署兼容，不把新增配置误称现场部署完成。空库安装不读取旧测试密文，也不因此豁免真实轮换、TLS、身份与隔离验证。

| 子任务 | 实现与验收边界 | 状态 |
| --- | --- | --- |
| 04B1 | Compose 显式原生覆盖层、双服务独立目录只读挂载、专用 CA/私有网络；真实 Compose 合并输出正负例、原 remote 回归 | 代码及真实解析通过 / NOT_DEPLOYED |
| 04B2 | Helm 类型化原生配置、独立 token 投递卷及 CA、窄网络规则；实际 template/lint 与错误输入拒绝 | 代码及真实解析通过 / NOT_DEPLOYED |
| 04B3 | 生产安全门禁区分既有 remote 与 native、迁移与 fresh-install；绑定实际环境的无秘密证据，不伪造旧 key、不跳过完整 gate | 代码与真实隔离联验通过 / NOT_DEPLOYED |
| 04B4 | 现场非 root 业务容器原生接线、真实轮换/权限/TLS/挂载验收；与服务身份、NATS 和数据库准备合并后才允许业务切换 | 待实施 |

04B4A（代码与隔离实测通过，正式操作未执行）：在既有独立本机 `kms-operator` 增加 `rotate-transit`，只允许固定 `pinvou-token-data` key 版本轮换。保管人真实终端输入既有恢复口令，解密并验证已有 `roles.age` 与 `BOOTSTRAP_FINALIZED`、实例/endpoint/CA/recipient 绑定；仅使用第四个运维 AppRole 的短期 token，不读取 initialization 包/root，不输出或导出任何身份。输入明确的预期旧版本（来自 fresh-install prepare）、全新私有操作记录目录，使用相同配置/版本/目录处理结果不确定。先核对最小身份与 key 禁止导出/明文备份/删除等保护；独占持久化无秘密尝试记录，再发送一次 rotate。实际新版本必须恰好旧版加一，保护属性不变；撤销本次临时运维 token 后才保存完成标记。任何重入看到尝试记录都只能查询，不得再次 rotate；未变化、并发多次变化、错误实例或未知结果均失败关闭，保留记录供核查。轮换 API 不支持 CAS，故现场须单保管人串行窗口，不能声称客户端消除了所有其他运维并发。

04B4A 验收：原动作与非 TTY 拒绝不回归；缺失/错误身份包、错误配置/版本、附加权限/错误 key 属性在 mutation 前拒绝；真实或模拟响应丢失后以相同记录只读确认、不重复轮换，成功与失败都尝试撤销临时 token；拒绝覆写、输出含秘密、版本并发及过期记录。隔离 OpenBao 在 bootstrap root 已撤销后用既定第四身份实际轮换并验证重复调用没有再次变更。工具完成不代表正式轮换已执行；正式操作必须等全新数据库 prepare 和业务接线就绪，再交保管人本机执行；之后立即做既有加密快照/离线备份及完整门禁，旧 key 版本仍保留。

独立复审进一步收紧所有 invocation 的身份退场：操作目录先保存绑定上下文，再为每次可能申请身份的调用独占持久化 `CURRENT_INVOCATION`，覆盖首次、重入和同目录并发；记录成功落盘前不得登录。接到 token 后立即由本动作负责独立超时清理，不沿用会忽略撤销错误的旧 bootstrap 登录封装。合法 lookup-self 的服务器签发时间与显式 900 秒上限写入当前调用的有界租约记录；不持久化 token。退场确认后将本次目录原子归档，之后才允许新调用或完成标记。现存未退场调用禁止新登录；只有匹配退场标记，或 KMS 自身时间证明已过已验证租约硬上限加裕量，才可归档恢复，不能依赖保管人本机时钟前跳。登录响应丢失/未能验证租约就崩溃时，不假设期限，也不自动恢复，需独立审计核查；残缺记录、撤销未确认不得删除后重试。`ROTATION_ATTEMPT` 仍只在所有预检通过、发送唯一 rotate 前持久化；仅有上下文/身份尝试而没有该记录，不能在重入时冒称完成轮换。验收补充成功后重入撤销失败、立即再次重入、租约过期、登录响应校验失败及取消/清理失败、并发输家与崩溃记录等序列。

04B4A 最终实测：Windows Go 1.26.5 独立工具全模块测试及 vet 通过（operator 34.320 秒）；新增异常属性 24 组合、服务器健康/时间 8 反例，以及重入撤销失败、未知登录、并发锁、残缺/错绑定/归档冲突和取消后清理测试通过。Linux 同源二进制在独立 OpenBao 2.6.2 TLS/Raft 实例中，撤销初始 root 后执行真实 `RunRotateTransit` 两次，版本仅从 1 增加到 2；轮换、Agent 暂存、bootstrap、operator 四个具名 fixture 标记均通过。临时设施已清理，不涉及正式密钥、数据库或业务切换。

04B3 设计：保留默认 remote/migration 的旧密钥退役语义；显式 `vault-transit` + `fresh-install` 使用独立原生门禁模块，其他原生安装模式拒绝而非降级。原生 Transit 的稳定 key 名与 `vault:vN` 版本不能混同；新安装不伪造“旧 key 已禁用”，也不降低最低解密版本而破坏未来备份恢复。

原生验收采用 prepare/verify 两阶段：prepare 通过受限 PostgreSQL 只读查询确认六类密文表为空（凭据、四类用量密文及凭据隔离会话的 PKCE），记录数据库身份摘要；使用真实网关 Agent 身份加密随机探针，绑定 endpoint、专用 CA、稳定 key、时间和数据库摘要，将不含实际业务秘密的探针状态独占写入私有文件。prepare 不是 GO。之后只有保管人的本地交互运维工具可读取离线运维身份执行版本轮换；Agent 不取得运维身份、root 或恢复口令。verify 必须在同一数据库和 KMS 上证明新加密版本严格递增、旧探针仍可解密、错误 AAD 被拒绝；两服务各自角色/短租期/实际续租正确且具有所需加解密能力，数据库仍无历史异 key 密文，公开四类入口和业务 readiness 均通过才输出 GO。探针过期、替换数据库/CA/endpoint、同版本、共享 token 文件、读取不安全文件、接口异常均拒绝。

数据库连接来自受限文件，调用本机 `psql` 仅执行固定只读 SQL，密码不进命令行或输出；证据只含计数与身份摘要。仍需另行证明全新服务/卷的精确部署来源、身份及网络隔离，不能把一份配置声明当作现场事实。门禁只探测，不负责删库、KMS 轮换、改变策略或提高自身权限。

查询固定 public 表及 pg_catalog 搜索路径，关闭行安全的静默过滤（受 RLS 限制时查询必须失败），防止“看不见旧行”被误认为空库；仅在 verify 阶段排除已合法清空的 PKCE 墓碑。隔离联验复用全新 OpenBao TLS/Raft fixture，增加全新 PostgreSQL TLS 实例及正式门禁执行，实际验证双 AppRole 续租、空库采样、同版本拒绝、轮换后旧探针解密及六类异 key 拒绝。该联验不读取或改变正式 KMS，测试 schema 的联验不能替代正式迁移与业务容器验收。

04B3 验证（2026-09-08）：8 项定向合同测试无 SKIP 通过；真实 OpenBao 2.6.2 TLS/Raft/双 Agent + PostgreSQL 17 TLS 隔离联验输出 `NATIVE_GATE_ACTUAL_TLS_POSTGRES_ROTATION_PASS` 和 `OPENBAO_NATIVE_GATE_FIXTURE_PASS`，退出 0。除轮换、六表、墓碑及 RLS 场景外，两个真实 entity 分别附加额外身份策略后拒绝、移除后恢复；错误 CA 在 HTTPS 握手失败且不写探针。测试运行后全部所属容器/网络已清理，无业务切换。独立审核发现的 `identity_policies` 附加授权遗漏已修复并经审核通过；只接受缺省/空数组，不因 token policies 正确而忽略 entity/group 权限。首次临时 PostgreSQL IP 与 Agent 自动分配地址冲突，调整隔离地址后完整重跑通过，不冒充生产故障修复。操作说明见 [原生门禁手册](../deploy/runbooks/native-kms-security-gate.md)；现场业务及保管人轮换仍归 04B4。

04B3 全量回归：447 项合同测试中 445 通过、2 项既有 Docker/Helm opt-in 跳过，0 失败；不把跳过算作现场验收。18 项 Python 设施测试 17 通过、1 项 Windows 不适用的 POSIX 检查跳过；架构与 diff 检查通过。首轮全量仅因 Windows PATH 未含 OpenSSL 失败，测试增加标准 Git 安装目录发现后按默认完整命令重跑通过，无 TLS 跳过或生产代码降级。

04B1 设计：既有 `compose.model-token.yaml` 仍默认 remote，但把仅 remote 所需的静态 token 插值改为可空，由合并后的部署检查和业务启动按实际 provider 拒绝缺失；原生覆盖层固定 `vault-transit` 并清空静态 token/local KEK。网关和凭据 Portal 使用两个不同且互不包含的宿主绝对目录，挂载整个 token sink 以跟随 Agent 原子换文件；单文件挂载、共享父目录、可写卷和自动创建缺失目录均拒绝。公开 CA 使用独立只读目录，不挂载包含叶私钥的 TLS 目录。仅这两个业务加入显式指定的既有 internal KMS 网络，不向命令执行器或其他服务开放 KMS 挂载；不部署/重建 KMS 或 Agent。

合并输出检查验证 provider 配对、HTTPS origin、单段 key ID、固定容器 token/CA 路径、非 root/只读根目录、完整只读 bind、两个 sink 不同且不重叠、非 KMS 消费者没有同源或祖先目录挂载。真实宿主目录是否链接、UID/0600、网络实际 internal、令牌身份/租期与 CA 内容在现场另验；静态配置不能证明这些事实。原生模板不自动删除旧业务、不执行初始化或轮换。测试用域名/目录均为无秘密 fixture，实际 Compose config 输出可能包含其他业务秘密，现场不得打印完整配置。

04B1 验证：本机完整合同测试 439 项中 438 通过、1 项 Docker opt-in 跳过；该项已在隔离公共 fixture 目录使用真实 Docker Compose 5.4.0 执行，四项测试全部通过、无 SKIP（宿主 Node 20；本机合同测试 Node 26）。实际解析 remote/native 两组完整合并输出，原生路径无需静态 token，既有工作负载/NATS/端口检查仍通过。拒绝共享/父目录、可写/单文件/阴影挂载、网络别名/共享服务网络和越界身份等配置；未启动业务或修改 KMS。架构守卫及 diff 检查通过。首次全量发现既有 workload 投递器漏登 CLI，已单独修复后完整重跑通过，未改变授权门禁。

04B2 设计：新增闭合的 `nativeKms` Helm 配置，默认不启用；仅选择 `vault-transit` 才为网关及启用的凭据服务各添加一个固定摘要 OpenBao Agent sidecar。两服务使用不同的既有 bootstrap Secret（只取 role-id/secret-id），不得与任何 runtime/operations Secret 同名；业务容器不挂载 bootstrap，Agent 不挂载数据库、业务 JWT 或其他应用秘密。短期 token 写入各 Pod 独立的 memory emptyDir，业务只读整个目录；CA 从仅含公开证书的既有 ConfigMap 只读挂载。不能直接以 Kubernetes Secret 的符号链接 token 替代 Agent sink，也不能修改原生 provider 的安全读取规则来适配错误挂载。

Pod 与 Agent 使用 UID/GID/fsGroup 65532，bootstrap 文件仅 root/该组可读，sink 由 Agent 生成 0600 普通文件；Agent 无监听器/代理、能力全移除、只读根目录及有界资源，独立内存临时目录。初次 Agent 登录之前业务启动可能暂时失败，Kubernetes 重启后重读 token；只有真实应用 readiness 成功才就绪，不以文件存在代替有效身份。副本不共享 token sink；节点转储/swap、etcd Secret 加密和外部 AppRole 周期轮换必须在集群验收中落实。

只向这两个组件增加经显式 CIDR 限制的 KMS TCP 端口（443 或 8200，与 HTTPS origin 一致），不向 executor/其他服务添加此规则。Chart 不创建 KMS、Secret 或 AppRole，不使用宿主路径/静态业务 token。真实 Helm template/lint 必须覆盖原 remote 和 native，缺身份/共享身份/非 HTTPS/宽 CIDR/未知字段拒绝；渲染结果需检查 sidecar 双向秘密隔离和卷/网络边界。渲染测试不等同于集群 token 轮换和 NetworkPolicy 执行，现场步骤仍归 04B4；当前服务器部署继续使用 Compose，不引入 Kubernetes 集群。

04B2 验证：本机 Helm 3.19.0、隔离容器 Helm 4.2.3 均完成真实 remote/native lint/template；类型化配置、安全声明、真实渲染及错误输入三组测试全部通过、无 SKIP。检查实际 sidecar 数量为两份、固定镜像/命令/非 root、独立内存 sink、应用无 bootstrap、Agent 无业务 JWT/数据库挂载，并拒绝共享身份、别名投影、子路径/阴影卷、特权/宿主进程空间和静态 token。新增网络策略仅两类消费者、明确 IPv4 `/32` 与端口；不把 CNI 规则渲染当作执行证明。

完整本机合同测试 442 项中 441 通过、1 项 Compose opt-in 跳过（此前真实 Docker 已独立通过），其中 Helm opt-in 实际运行；后续收紧配置检查后再跑 17 项定向合同及最新 Helm 4 实测均通过。架构守卫和 diff 检查通过。没有修改 Go provider、投递身份、生产服务或 Kubernetes 集群，fresh-install 门禁与现场业务验收仍未完成。

## 依据

- [OpenBao Transit API](https://openbao.org/docs/api/secret/transit/)
- [OpenBao Agent AppRole](https://openbao.org/docs/agent-and-proxy/autoauth/methods/approle/)
- [OpenBao Seal/Unseal](https://openbao.org/docs/concepts/seal/)
- [Keycloak Service Accounts](https://www.keycloak.org/docs/latest/server_admin/index.html#_service_accounts)

当前仅为实施约束与任务拆分，不是生产验收报告。GitHub Issue/PR 检索因本机 gh 未认证未完成；本地未发现现成 Transit 适配或 workload 签发器。

## SECINF-01 实测记录

- 原生 provider、既有 remote 回归及生产 local 拒绝：Go 1.26.5，`go test -tags=nomsgpack ./internal/token -run 'TestTransitProvider(Native|Rejects)|TestRemoteEnvelopeProvider|TestProduction.*SecretProvider' -count=1`，PASS（6.219 秒）。
- 两服务入口：`go test -tags=nomsgpack ./cmd/token-gateway ./cmd/token-credential-portal`，gateway PASS（3.284 秒），credential 仅编译通过（无该包测试）。对应 `go vet` 通过。
- 真实 OpenBao 2.6.2 镜像锁定 `quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641`。独立容器 `--network none`，无业务卷/端口，受限 CPU/内存，测试 root token 随机生成、无日志输出；完成后精确移除测试容器及其内存数据。
- `TestTransitProviderOpenBaoIntegration` 实际执行 PASS（0.01 秒），无 SKIP：真实 native API、随机密文、AAD 拒绝、跨 key/ACL 拒绝、v1→v2 轮换后旧密文读取、撤销后拒绝。测试显式 opt-in `PINVOU_TEST_OPENBAO_ISOLATED=1`，只接受该隔离网络命名空间中的 `http://127.0.0.1:8200`；测试 bootstrap token 从环境注入，不能指向正式服务。
- 首次 fixture 启动因只读根目录未配置 `-dev-no-store-token` 失败，第二次只读 rootfs 阻止 docker cp；修正为不保存 dev token、只读挂载测试二进制后上述具名测试通过。未降低业务 TLS 或认证要求。
- 架构守卫通过；`workload-identity.test.mjs` 三项通过。自检确认现有 remote 协议未变、两入口接线齐备、生产静态 token 拒绝、token 文件缺失不复用缓存、错误不回显外部内容。
- 此处真实 OpenBao 仍为明确的**隔离开发模式 fixture**，不代表生产 TLS、持久化、解封、Agent、正式身份/网络或恢复验收。部署覆盖与生产安全门禁仍归 SECINF-03/04，不能直接用现有强制 remote 的 overlay 上线 native provider。

## SECINF-02A 实测记录

- [轮换器运行手册](../deploy/runbooks/workload-token-agent.md)说明单服务文件隔离、启动与健康检查、失败/过期语义及平台限制。新增程序未改变现有业务配置，不自动启用签发设施。
- Go 1.26.5：轮换器 TLS fixture 验证真实 RSA 签名/JWKS、双 token 与 bootstrap secret 轮换、错误 issuer/audience/sub/azp、未来 iat/nbf、缺失/异常/超长/过期生命周期、错误签名、HTTP 拒绝/跳转/超限/损坏响应、不可信 TLS、续期失败保留及过期拒绝、取消与固定日志。
- 接收端同类问题修复：新增严格签发/生效/失效时间检查，8 个边界子用例；不改变 subject→服务→精确权限规则，不扩大既有权限。
- Linux 独立容器、非 root UID、network none、只读根目录、无业务挂载，7 个顶层测试全部 PASS（无 SKIP），其中包含 40 次替换下并发读取完整性、0600 权限、公开秘密文件/符号链接/可写目录拒绝。容器执行结束已自动移除；只使用随机隔离测试签名密钥及假客户端秘密。
- Windows 首次并发替换测试实际暴露普通读句柄阻止 rename 的平台差异；未增加不安全的删除重建或宽松回退。正式 agent 明确仅支持 Linux，Windows 对 POSIX 权限和并发替换 2 项显式跳过，不能将 Windows 结果冒充生产原子性证明。
- 对应 Go 测试及 vet、架构守卫、既有 workload 身份契约 3 项已通过。上述签发端是隔离 TLS fixture，不是 Keycloak 联验；真实 realm、服务账户 UUID 与权限拒绝仍需 SECINF-02B。

## SECINF-02B 实测记录

- 无秘密 realm 生成器 4 项 Python 测试通过；显式固定 7 个 confidential client、300 秒 RS256、无交互式授权/离线/刷新/交换/设备授权，不预置 secret、用户 UUID 或角色。
- 真实官方 Keycloak 26.7.3 固定摘要 `sha256:ff4257d0d64efbe99ed1ddfaf07765cc3c36dc7518bf8324d41961327f441c54`；隔离容器 network none、无公开端口/业务挂载、独立 H2 测试数据，TLS 证书真实验证。具名 `TestKeycloakWorkloadIntegration` 实际 PASS（114.64 秒），无 SKIP；退出后测试容器、数据、临时秘密、启动日志均清理。
- 通过真实管理 API 核对 7 个客户端实际导入后的授权模式及 scope，读取 Keycloak 生成的服务账户 UUID 和随机客户端秘密；实际签发并验证 300 秒令牌，未使用测试进程自签 JWT 代替 Keycloak。
- 错 subject/audience 拒绝；另一个真实服务账户对未授权接收端 403；同一主体未授予的接口 403；服务 token 访问 Keycloak 管理 API 403。客户端秘密真正轮换后旧秘密无法再签发，新秘密自动读回并成功续取。
- 实际验证不返回 refresh token，密码授权、offline access、Token Exchange 和设备授权被拒绝。仅隔离客户端临时缩短为 40 秒后签发真实令牌、禁用客户端，再验证续期失败保留文件；等待真实 exp 后，健康检查失败且接收端返回 401。正式生成器仍固定 300 秒，未缩短生产策略。
- 过程发现并修复两项 fixture/配置问题：导入文件名必须与 realm 一致；省略 realm 层 scope 声明会自动附加内置 scope，已显式置空并补回归断言。早期 Go 测试进程 137 实际与 Keycloak 启动退出 1 关联，不能认定为 OOM；最终容器 memory.events 的 max/oom/oom_kill 均为 0。调试日志使用受限本地驱动，修复 max-file=1 与压缩冲突，并保证先 create 再 start、失败清理准确目标。
- 这些证明是独立签发器与接收端链路的真实隔离验证，不是手机认证扩展镜像回归、正式域名/realm 部署、KMS 初始化、恢复演练或全系统上线验收。正式设施创建、精确 subject 授权表和 secret-file 投递配置仍由 SECINF-03/04 部署。

## SECINF-03B 实测记录

- 固定 OpenBao 2.6.2 摘要的真实非 dev 实例，TLS、Raft、声明式 HMAC 审计及两个独立 Agent。专用 internal 网络，无宿主发布端口，非 root、只读根目录、受限能力/内存/CPU。测试 root 与解封材料只在控制进程内存中使用；结束清理自身容器、网络和测试数据。
- 最终完整运行输出 `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`，无跳过。可信 TLS 正例、不可信 CA/错主机名负例、双服务实际不同 token 且仅各自策略、AES-256-GCM 禁止导出/明文备份/删除配置均核实。
- 运行身份不能创建/轮换/导出/备份密钥、创建 token、撤销其他 token、修改策略或访问其他 key；缺失/空 AAD、错 AAD 与指定旧 key_version 均拒绝。真实 v1→v2 后默认写入 v2，旧 v1 密文仍可解密。
- 真实 Agent 产生续租审计并重新认证，投递文件为 0600；等待前一 token 实际过期后验证 403，不把新 token 提前投递误认为旧 token 已失效。正式模板为 300/900 秒，隔离 fixture 为 12/24 秒。
- 审计中未出现明文、Base64 明文、AAD、密文、root/运行 token 或解封 canary；rename/create 后 SIGHUP 确认新审计继续写入。重启后真实封存拒绝 503，原材料解封后通过重新投递身份读取历史密文，最终撤销测试 root 并确认 403。
- 实测发现并修正 fixture 可执行文件路径、internal 网络无宿主映射和 Agent sink mode 类型：该固定版本要求整数 `384`（0600），不能使用字符串 `"0600"`。早期启动诊断曾出现 Raft initialization lock 报错，未据此宣称原因已修复；最终完整实例就绪及上述验证通过。
- 四项静态配置测试、架构守卫、差异检查通过。尚未执行独立 Raft 快照恢复、加密初始化包与本机解封流程；重启持久化不能代替完整恢复验收。正式设施与业务部署均未改变。

## SECINF-03C-1 实测记录

- [单次加密初始化器](../tools/kms-recovery/cmd/kms-initializer/README.md)与离线托管程序分别构建，未改变后者的网络边界或业务服务依赖。Linux 命令必须显式确认新实例和保管人备份；新目录独占、目录/文件同步、密文读回，所有失败保留现场。
- Windows Go 1.26.5 全模块测试通过：原离线托管 13.330 秒、初始化模块 2.880 秒，命令编译通过（无该包测试）；`go vet` 通过。真实实例测试默认显式跳过，POSIX 权限断言只在 Linux 执行，不把 Windows 结果视作目录同步证明。
- TLS 单元 fixture 验证正确包解密与元数据绑定、错误身份/损坏包拒绝；HTTP/路径/查询/用户信息/错误公钥指纹/无确认拒绝；有效但不可信 CA 拒绝；已初始化和缺失状态拒绝；500/307/截断/超限/缺失材料/连接断开均不重试、不生成成功标记，非空响应保留为加密包；已有包不覆盖，关闭文件写入失败报告不确定。
- 同源 Linux 测试二进制在新建 OpenBao 2.6.2 非 dev、TLS、Raft 隔离实例上执行整个初始化模块测试集，输出 `OPENBAO_INITIALIZER_FIXTURE_PASS`，未跳过；真实用加密包恢复的材料解封、使用包内 root 认证、撤销 root 后 403，再次初始化及覆盖拒绝；确认所有输出没有实际 root/解封明文，Linux 私有文件和目录检查实际执行。该实例与其测试材料已精确清理。
- 四项 OpenBao 配置测试与架构守卫通过。此子项仅完成加密初始化捕获：用户的正式主密钥、CA、正式恢复包均未生成；本机人工解封/配置运维身份、独立快照恢复、业务部署仍未完成。初始化响应丢失的跨系统非原子性风险保留，未增加自动重置回退。

## SECINF-03C-2 实测记录

- 独立 [kms-operator](../tools/kms-recovery/cmd/kms-operator/README.md) 只提供人工解封；共享纯离线 custody 包，原有 `kms-recovery` 格式、输入和网络边界保持兼容。保管人口令不经工具或聊天输入，本轮只使用随机测试身份和固定测试口令。
- Windows Go 全模块通过：既有离线测试 8.580 秒、初始化 3.030 秒、operator 2.429 秒；`go vet` 通过。两个 CLI 编译通过，无 CLI 包测试；真实实例测试默认跳过，真实本机终端交接尚待部署时执行。
- 单元 TLS fixture 覆盖解封及已解封不重复、12 类包/绑定/身份/损坏拒绝且零网络请求、原目标 Host 与 TLS 名称通过 loopback 保留、错误 TLS 名称拒绝、5 类写失败不自动重试及不回显、未知/未初始化/错误 Shamir 阈值拒绝、HTTP/路径/查询/用户信息/远程或非数字 loopback 覆盖拒绝。
- Linux 同源 operator 模块全部测试在独立 OpenBao 2.6.2 TLS/Raft 实例真实执行，无跳过，输出 `OPENBAO_OPERATOR_FIXTURE_PASS`。使用标准 scrypt 加密测试身份及初始化加密包，错误口令拒绝；经透明 loopback TLS 转发实际解封，直连已解封不重复提交；重新封存后再次实际解封，最终撤销测试 root 并验证 403。只在测试控制进程中使用测试 root 完成封存/撤销，正式 operator 解封从不发送 root。
- 配置测试、架构守卫与差异检查通过。测试容器及临时材料精确清理；未生成正式主密钥、没有更改旧业务服务或数据，独立快照恢复及生产配置接线仍未完成。

## SECINF-03C-3 实测记录

- 同源 Linux operator 测试二进制配合 `--restore-fixture`，在两个新建 internal 网络、独立 Raft 与审计目录的 OpenBao 2.6.2 实例实际运行，输出 `OPENBAO_SNAPSHOT_RESTORE_FIXTURE_PASS` 和 `OPENBAO_OPERATOR_FIXTURE_PASS`，未跳过。没有挂载业务数据或发布宿主端口，结束精确清理两个实例与网络。
- 源实例产生 v1 密文并轮换 v2；限时 180 秒的 snapshot-only 与 readback 测试身份分别授权，后者不能取快照。撤销源 root 并确认 403 后，再由 snapshot-only 身份取得真实 Raft 快照、age 外层加密落盘，错误解密身份和损坏包拒绝，读回并完整认证后才提交给新恢复目标。
- 空目标独立初始化和解封后实际执行 `snapshot-force`；随后真实重新封存，使用源恢复包、原逻辑 TLS 身份及受控 loopback 路径再次解封。确认旧 v1 密文读回、密钥版本仍为 v2，源已撤销 root 和恢复目标被替换的 root 都返回 403，不因恢复重新生效。
- 两个隔离实例仅在本测试中共享临时测试叶证书（包含两个测试 IP），用于模拟同一逻辑 KMS 的替换；不涉及正式 CA/证书复用。快照与 root/份额仅用于此临时证明；正式 operator 没有新增 `snapshot-force` 入口。
- Go operator 单元 2.624 秒及 vet、4 项配置测试、架构守卫通过。该 fixture 将小型快照限制为 1 MiB，不能据此宣称正式容量/定时备份/留存/介质恢复已验证；正式备份工具、真实介质和容量验收仍需部署交接补齐。

## SECINF-03C-4 证书工具实测记录

- 独立离线 `kms-pki` 不联网、不接触 KMS 主密钥，只生成专用 CA 与叶证书；CA 私钥直接 age 封装，输出目录拒绝覆盖。Windows 全模块测试通过（PKI 6.294 秒），`go vet`、架构守卫与差异检查通过。
- 对应 Linux 测试二进制 3 项全部通过，无跳过：解密加密 CA 包并核对实际私钥与证书对应、叶/根密钥分离与用途/期限/身份约束、使用生成的叶证书完成真实 TLS 握手、错误身份/损坏包拒绝、现有目录保护和 9 类错误输入拒绝。Linux 0700/0600 与可写公钥拒绝实际执行。
- 正式生成前已核对本机恢复父目录继承关闭，仅当前保管人与 SYSTEM 有权；仅检查原 custody 的加密文件元数据，不读取口令或解密私钥。首次生成工具不提供续签/告警，正式证书轮换与运维自动化仍需后续交接，不能据此标记安全设施全部完成。

## SECINF-03C-5 设施启动记录

- 独立 OpenBao Compose 模板不发布端口、不自动初始化或解封；只接入新建 internal 网络，配置/证书只读，唯一持久化写目录为登记的 data/audit。生产数据不与测试 fixture 或旧业务目录共享。
- 实际 Docker Compose 解析和新建容器检查发现两项序列化/镜像细节：`core=0` 在 config JSON 中省略数值，但真实 HostConfig 已核实 soft/hard 均为 0；能力名规范化为 `CAP_IPC_LOCK`。未把表示差异当作运行配置缺失。
- 固定镜像另有两个隐式匿名卷，启动前拦截并改为明确 tmpfs。仅移除本次新建、从未启动的容器及其两个已核实为空、无其他使用者的匿名卷，保留新证书和绑定目录，未清理旧业务。最终实际容器只有 5 个受控 bind，无额外持久卷，禁用转储、只读根目录、限额和单网络均通过。
- 对隔离 helper 同步加上 core=0、noexec 与镜像路径 tmpfs 后，真实 TLS/Raft/双 Agent/AAD/ACL/续租/重认证/审计轮转/重启解封回归再次完整通过 `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`；5 项配置测试及架构守卫通过。
- 正式专用 CA/叶证书已在保管人受控本机生成，CA 私钥仅以 age 包保存在本机，未上传服务器；服务器已启动新 KMS 并通过实际 HTTPS 证书校验，启动核对时 `initialized=false`。这仅为独立设施启动，不代表主密钥交接、root 撤销、定时备份或业务切换完成。

### 正式初始化与保管人交接进度

- 保管人确认 custody 已存储后，正式新实例通过一次性初始化器生成 1/1 Shamir 材料，输出 `INIT_CAPTURED`；响应仅内存处理并直接 age 加密。未把 root、份额或 CA 私钥写入聊天/环境变量/业务镜像/明文文件。此后不得再次运行 init 或把该设施当成可丢弃测试实例。
- 初始化加密包已传输到本机受控恢复目录，服务端与本机 SHA-256 一致；目录 ACL 仅保管人与 SYSTEM。新 CA 加密包与初始化包都需加入离线备份，原 custody 备份不能代替新材料备份。
- 保管人通过本机交互窗口完成操作并确认“已解封”；随后独立只读核对 initialized=true、sealed=false、n=t=1，真实 TLS 健康检查返回 200。没有通过 Agent 读取口令或解密其身份代办。
- 本机暂存叶私钥在与受控服务端副本核对哈希一致后精确清理；服务器叶私钥、CA 加密包及初始化包未删除。此初始化阶段尚未创建正式 AppRole 或撤销 root；后续完成状态见 03C-6。旧账号平台、New API 和其他业务服务/数据尚未切换，也未完成全平台生产验收。

### SECINF-03C-6 有界身份引导与 root 退场（已正式执行）

需求：普通解封仍不解析或发送 root。另设显式本机 `bootstrap` 操作，使用同一恢复包绑定校验及真实终端口令，root 仅内存使用。仅面向已解封、尚无 transit/approle 的新实例，不修改已有挂载、角色或密钥；不提供任意 API、策略输入、删除或重置。

固定计划：创建不可导出/不可明文备份/不可删除的 AES-256-GCM `pinvou-token-data`；分别创建网关、凭据服务、快照备份、离线运维四个 AppRole。前两者沿用强制 AAD 的 runtime 策略；备份仅快照 read/sudo 及自身租约；运维只读/轮换该 key、管理这四个既定角色的 SecretID，不得修改角色策略、创建任意 token、管理挂载或直接加解密。四角色统一 300 秒 token / 900 秒显式上限，不附加 default policy。持久 SecretID 是单独的机器引导秘密，不冒充短期 token；运维 SecretID 只能留在离线加密包，不部署进业务服务器。

持久化与不确定状态：先独占创建受控输出目录并同步尝试标记，每个一次性 SecretID 响应立即 age 加密、同步、读回；最终生成绑定实例/origin/CA/recipient 的完整身份包。所有输出拒绝覆盖；失败不自动重试创建、不回滚或销毁已生成身份，保留初始 root 以便受控恢复。runtime/backup 的明文文件投递另行受控完成，本步骤只生成加密包。

退场分为独立 `finalize-bootstrap` 操作：完整解密身份包、验证四个身份的实际登录与限定权限、加解密及快照读取，再撤销初始 root 并确认 403，重新验证四角色不依赖 root 存活。root 撤销结果不确定时只能用同一包重新核对/完成退场，不能重新 bootstrap。就绪标记只在撤销及后验均成功后创建；加密包需加入既定离线备份。本步骤不等于正式备份调度、身份投递或业务上线。

验收：错误包/目标在联网前拒绝；已有实例配置与已有输出在写入前拒绝；策略和角色参数回读；四身份正向及越权负例、AAD 缺失/不符、错误 TLS、错误凭据、响应超限/断线/跳转、部分失败及重复退场；真实隔离 OpenBao 上验证初始 root 403 与四角色仍可登录。按该边界实现、验证并独立提交后，才交付本机正式执行入口。

实测与自检（2026-09-07）：

- Windows 独立工具模块全量测试 PASS：离线工具 10.527 秒、初始化 4.271 秒、operator 9.894 秒、PKI 4.288 秒，vet PASS。默认真实环境测试仍显式 opt-in，不能把默认 SKIP 当真实环境验收。
- 最新同源 Linux operator 测试二进制在全新独立 TLS/Raft 实例完整执行，无 SKIP，输出 `OPENBAO_BOOTSTRAP_FIXTURE_PASS` 和 `OPENBAO_OPERATOR_FIXTURE_PASS`。真实证明四角色创建、加密包读回、权限/AAD、备份读取、初始 root 403、重复退场、四角色存活、运维轮换 key、跨角色 SecretID 拒绝及轮换/销毁后拒绝。
- 联验修复了两处 API 假设：策略目录不是 GET 列表，改为逐个固定策略 GET/404；OpenBao 2.6.2 创建及轮换 key 返回带元数据的 200，而非 204。未放宽为“任意 2xx”，并对策略/角色/key 安全属性做实际回读。失败均发生在隔离实例，未操作正式配置。
- 本地负例覆盖创建响应断线/截断/超限/跳转、加密诊断保存、已有挂载与输出拒绝、完整包探测失败后的无重建续办、角色包九类绑定/重复身份拒绝、额外权限和错误凭据阻止 root 撤销，以及撤销响应丢失后的同包收敛。普通 unseal 仍不发送 root；默认离线程序无网络新增。
- 自检边界：只有固定 key/四角色，无任意策略或代理；运维可签发四角色 SecretID，属于离线特权身份，不宣称它完全无法间接获得运行身份。销毁 SecretID 不立即终止旧 token，最长 15 分钟窗口与紧急隔离已写入手册。快照读取不是持久备份；部分创建失败不伪称可自动恢复。正式业务/Agent 投递、备份调度及整个平台部署仍未完成。

正式执行：保管人完成 `BOOTSTRAP_PREPARED`，明确确认新身份包、CA 包及初始化包离线备份，再完成 `BOOTSTRAP_FINALIZED`。本机完成标记与 `roles.age` 摘要独立核对一致，真实 TLS 健康检查 200。正式 key、四类 AppRole 和初始化 root 撤销已由受控操作器逐项验证；Agent 未读取口令或解密身份包。旧业务服务/数据未切换，不再重复初始化或保留有效初始 root。

### SECINF-04A 业务侧专用 CA 与原生 TLS 接线（代码及隔离联验通过）

需求：网关与凭据服务支持 `TOKEN_SECRET_PROVIDER_CA_FILE`，仅对 `vault-transit` 生效；显式配置无效时启动拒绝，不回退系统 CA。未配置时保持系统信任库兼容。专用信任只属于该 KMS HTTP client，不修改 `SSL_CERT_FILE`、全局默认 Transport 或 OIDC/商城等其他 HTTPS client。

设计：CA 为受控只读挂载中的绝对路径 PEM 证书包，大小有界、只允许有效 CA 证书，拒绝缺失、非普通文件、组/全局可写和混入私钥/垃圾内容。允许 Kubernetes 只读投影文件的符号链接，经解析、文件身份核对及权限检查读取；挂载本身必须防业务写入。每实例创建独立 Transport，TLS 至少 1.2，关闭环境代理和透明写重试，保留主机名验证、禁止跳转与有界超时/响应。CA 在启动时加载，轮换信任包需受控滚动重启，不能误称自动热更新。

任务验收：两服务入口接线；专用 CA 正向、错误 CA/主机名、损坏/危险路径与权限负例、没有全局信任污染、不会继承代理；现有本地/remote 模式兼容（非 native 模式显式配置 CA 时拒绝，避免静默忽略）。真实隔离 TLS/Raft/Agent 下由业务原生 provider 使用投递 token 文件完成加解密与 AAD/ACL 拒绝，不借用测试 client 的 TLS Transport。完成后独立提交；本子项不提前修改仍要求 remote 的 Compose/Helm/门禁或宣称业务已接入。

实测：原生及既有 remote/生产拒绝回归 PASS（2.243 秒）；网关入口测试 PASS（1.764 秒），凭据入口编译通过（无该包测试）；三包 vet、6 项设施配置测试、架构守卫通过。默认两项真实 Transit 测试保留 opt-in，未将 SKIP 当作实测。

固定真实 OpenBao TLS/Raft/双 Agent fixture 在初始 root 已撤销后运行同源 Linux 业务测试二进制，专用 CA 两组测试与真实 provider 测试无 SKIP，输出 `OPENBAO_TRANSIT_TLS_FIXTURE_PASS` / `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`。包括真实 CA、文件 token、随机密文、AAD/跨 key 拒绝与错误主机名；Linux 文件权限和只读投影 CA 链接均实际覆盖。该 provider 测试由隔离控制进程执行，不冒充正式业务容器挂载或非 root 端到端验收。

首次新增 TLS 联验被 CA 校验拦截：旧 fixture 根证书依赖宿主 OpenSSL 默认用途。模板已明确指定 CA:TRUE 和 keyCertSign/cRLSign，与正式专用 CA 的限制一致；未放宽应用信任校验，完整重跑通过。自检确认未改全局 TLS/代理、未改 legacy remote 协议、未更改密文格式或移除 token 轮换读取；正式 Compose/Helm、安全门禁和业务接入仍待后续任务。

### SECINF-03C-7 Agent 引导文件受控投递（暂存工具通过，正式投递待执行）

需求：只把网关、凭据服务、快照备份三个 Agent 所需的 role-id/secret-id 投递给对应 Agent，离线运维身份、初始化 root、解封份额、恢复私钥和口令均不得进入投递文件。业务容器只接触短期 token，不读取这些长期机器引导秘密。

实现任务：独立本机 `kms-operator stage-agents` 通过真实终端口令解开既有 `roles.age`，完整验证实例/origin/CA/recipient/身份唯一性及 FINALIZED 摘要标记，不读取初始化包、不联网、不创建身份或撤销权限。只允许输出前三个固定角色的 role-id/secret-id，各文件 0600，逐文件同步与读回，写入受控的新 staging 目录并生成无秘密的摘要清单；既有输出拒绝覆盖。staging 与 custody/恢复包所在目录分离，不能误作为离线恢复备份长期留存。Windows 父目录先核对仅保管人和 SYSTEM；失败保留部分输出、不得宣称交付完成。

部署任务：经既有验证的 SSH 传输到专用 Agent bootstrap 目录，逐文件摘要及 UID/权限核对，Agent 以只读目录挂载，单独的短期 token sink 放在受控 /run 内存目录；仅确认投递和 Agent 登录后精确移除本机明文暂存文件。长期 bootstrap 在 Agent 侧按既定轮换制度保护，不冒充短期凭据。此任务不允许用聊天/工具读取口令或输出文件内容，不生成或恢复 root，不把运维角色捆绑上传。正式备份任务与业务 Compose 接线仍分别验收。

验收：错误身份/绑定/完成标记在写出前拒绝，缺失/重复角色拒绝，只产生六个固定文件与摘要清单、不包含运维或 root 数据，路径重叠/覆盖拒绝；真实隔离 bootstrap 完成后的同一包可正确投递，三身份可登录，离线步骤不发起任何网络请求；实际 Agent 只读 bootstrap、独立 sink 和租约须另行核对后记录现场完成。

03C-7A 实测（2026-09-08）：Windows 工具模块全量测试及 vet 通过（离线 12.750 秒、初始化 6.768 秒、operator 16.269 秒、PKI 5.285 秒）。离线测试先关闭 TLS 服务器再暂存仍成功，网络计数不变；仅六个身份文件和一个摘要清单，运维/root 排除、摘要读回、未退场/错误包/目录重叠与覆盖拒绝均通过。

最新同源 Linux operator 全包在真实隔离实例无 SKIP 通过，输出 `OPENBAO_AGENT_STAGING_FIXTURE_PASS`、`OPENBAO_BOOTSTRAP_FIXTURE_PASS` 和 `OPENBAO_OPERATOR_FIXTURE_PASS`。真实暂存文件中的三个身份再次登录并完成各自权限正负例，探测 token 撤销；四类身份及 root 退场回归也通过。初次 Linux 暂存被父目录权限拦截：Go 测试临时子目录使用 0777 再受 umask 限制，不等于受控 0700。测试现明确创建 0700 staging 父目录，未放宽工具权限要求。

此处仅为受控暂存工具验收；正式暂存需保管人本机输入既有口令，随后仍需验证 SSH 传输、服务端文件隔离、Agent 登录及本机明文副本清理。没有提前声称正式 Agent 已启动或业务已部署。

### SECINF-03C-7B 独立 Agent 部署与现场验收（通过）

保管人已完成 `AGENT_FILES_STAGED`。本任务只部署三个 Agent，不修改正式 KMS、策略、root 或旧业务。使用独立 Compose 项目，固定已验证镜像、非 root UID、只读根目录、全部移除 capabilities、禁止 core dump、限制资源及日志；只连接既有私有 KMS 网络，不发布端口、不提供代理。每个服务单独挂载只读 bootstrap 和可写 sink；配置与 CA 只读，运维身份不在部署清单内。

sink 使用宿主 `/run` 的 tmpfs，systemd-tmpfiles 在系统启动时先建立受限目录；父目录 root 0700、各服务子目录 UID 65532/0700，文件由 Agent 原子生成 0600。不能用文件存在代替令牌有效性，验收程序必须通过专用 CA/TLS 实际查询三枚令牌，验证角色唯一、有效 TTL 不超过 300 秒、显式最长 900 秒、可续租及权限拒绝，并检查真实 Docker 的隔离挂载与资源限制。验收程序只输出固定状态码，不打印请求、响应或令牌；只做自身查询、随机加解密验证和有界快照读取，不修改策略、轮换密钥或撤销 Agent token。

顺序：文档与部署模板/验收测试 → SSH 投递及摘要核对 → 启动三个 Agent → 实际挂载、身份、权限和续租核验 → 精确清除本机六份明文机器引导文件 → 独立提交。加密恢复包保留；远端 Agent bootstrap 仍为需周期轮换的长期秘密。快照读取仅为权限探测，不等于正式备份调度；主机重启后的 KMS 解封仍需保管人参与，不以 Agent 自动重启宣称自动解封或高可用。

现场结果：六文件通过本机身份包绑定、摘要、完整清单和 ACL 检查，SSH 传输后再次摘要核验，远端 UID/GID 65532、0700/0600。独立三个 Agent 启动，`KMS_AGENTS_VERIFIED` 通过；真实 TLS 查询唯一角色、活跃 300/900 秒租约、网关/凭据随机加解密及 AAD 拒绝、备份读取和越权拒绝全部通过。随后 `KMS_AGENTS_RENEWAL_VERIFIED` 确认三个身份已实际续租；并非只验证 renewable 标志。校验器最初误将 `last_renewal` 当数字，实测它为 RFC3339Nano，且宿主 Python 3.10 不能直接解析九位小数；已修正为严格格式解析并补纳秒回归测试，不调整 KMS 或角色租期来掩盖问题。

实际 Docker 校验：无主机端口/额外网络/匿名卷，全部 capabilities 移除、非 root、只读根目录、core=0、资源受限；每角色只读 bootstrap 与独立可写 tmpfs sink。系统 tmpfiles 已安装并创建目录，配置的启动顺序在 sysinit 前；未为验证而重启正式主机，不能冒充正式断电恢复验收。验证后精确删除本机六个明文暂存文件，既有加密恢复包及无秘密的摘要清单保留，服务端长期机器引导文件保留供 Agent 重新登录。

验证：Windows 14 项测试中 13 通过，1 项真实 POSIX UID/文件权限用例明确跳过；同源 Linux 六项校验器测试全部通过无 SKIP（包含真实权限/符号链接/有界读取），8 项配置测试通过；架构守卫及差异检查通过。KMS 保持健康，旧账号平台、New API、其他团队服务均未改动。正式业务接入、持续告警和定时备份尚未完成。

### SECINF-03C-8 定时加密快照（实施中）

需求：备份任务只能读取备份 Agent 的短期 token、公开 CA 和已核对的恢复 recipient，不挂载 AppRole 引导文件、运维身份或恢复私钥。通过可信 TLS 查询自身身份，确认唯一备份策略及有界有效期后读取真实 Raft 快照；不创建身份、不修改密钥、不解封、不执行恢复。快照未经外层 age 加密不得落盘。

03C-8A：交付独立 Linux `kms-snapshot` 单次命令。输入为公开配置文件，固定 API 与标准 age；有界响应及 gzip 完整性检查、实例/origin/CA/recipient/时间/快照摘要绑定。每次新建私有唯一目录，仅加密包落盘，sync 后读回密文摘要，再独占写完成清单；失败保留不完整尝试，不能覆盖或把它视为可恢复备份。当前压缩快照上限 64 MiB、展开上限 512 MiB，超限报错，后续随容量评审调整而非截断数据。离线验证入口通过保管人本机既有口令完整认证解密并验证绑定，不输出快照内容；真正 restore-force 仍只在隔离测试使用。

03C-8A 验收：真实 TLS/错误 CA、错角色/过期 token、重定向、损坏/截断/超限快照、损坏包/错误恢复身份、输出覆盖/不安全路径/权限均拒绝；既有隔离双实例恢复演练改用生产备份函数产物，源 root 已撤销后取得快照，恢复到新实例后能读回轮换前密文，旧 root 不能复活。单元测试不代替实际恢复证据。

03C-8B：通过后部署非 root、只读根目录且仅备份 Agent sink 可见的单次容器，由 systemd 每小时触发（随机延迟最多 60 秒，错过时补跑），每次运行有超时/内存/CPU 限制，失败必须表现为失败 unit。落盘目录只存加密包与无秘密完成清单。首次现场执行核对角色、密文、权限、完成标记和调度状态，并把首份加密包复制到保管人本机供离线验证。负责人已选择“由保管人定期下载并离线保存”，不配置向其他服务器或对象存储上传；每个工作日至少下载最新成功包，身份/密钥变更后立即补存。离线恢复点取决于最后实际下载的包，不能把服务器每小时备份频率当作异机 RPO。业务尚未放量，暂不自动删除任何正式快照；保留策略、容量/失败告警和正式恢复交接须单独记载实际验收，不能把同机文件数量或首次备份成功等同于灾备可用。

03C-8A 实际恢复验证通过：固定 OpenBao TLS/Raft 的双独立实例中，源 root 撤销后由仅含快照读取及自身查询策略的限时备份身份调用生产 `CaptureSnapshot`，生成带完整性清单的 age 包。原始错误身份与损坏包被拒绝，本机离线口令验证入口也执行通过；新实例恢复后可解密 v1 历史密文，key 最新版本仍为 v2，两枚过期 root 均拒绝。输出 `OPENBAO_SCHEDULED_SNAPSHOT_FIXTURE_PASS`、`OPENBAO_SNAPSHOT_RESTORE_FIXTURE_PASS`、`OPENBAO_OPERATOR_FIXTURE_PASS`，operator 全包无 SKIP。此证据使用随机隔离测试密钥，不读取正式恢复私钥，尚不代表正式快照已下载离线保存。

03C-8B 现场调度通过、离线交接待确认：受限单次容器的实际六个绑定挂载、非 root/只读根目录、仅备份 sink、无公开端口/额外网络/匿名卷、512 MiB/0.5 CPU/32 PID 等核对通过；验证用未运行空容器按明确 ID 删除，没有删除任何业务数据。正式 unit 首次运行 `Result=success`、`ExecMainStatus=0`，每小时 timer enabled/active，单次工作容器正常退出无残留，KMS 仍健康。

首份正式 `snapshot.age` 和完成清单已经由 SSH 下载到保管人本机受限目录，服务器与本机密文长度/摘要核对一致。提供本机下载与离线验证窗口，可重复下载最新成功包但不覆盖不一致文件；用户仅在本机输入既有口令，完整验证后自行复制到离线介质并确认。当前仍未收到本机完整验证/离线保存的完成反馈，不提前宣称这两项完成。加密包不包含恢复私钥；口令与原有恢复材料不通过聊天/工具读取。

验证：独立工具全模块 Windows 测试与 vet 通过；实际 Linux 双实例全包恢复验证通过；部署配置及既有 Agent 校验共 16 项 Python 测试中 15 通过、1 个 Windows 不适用 POSIX 用例跳过（其 Linux 实测已通过）。架构与差异检查通过。未配置异机自动上传、未删除正式备份；持续失败/容量告警、保留策略及完整业务上线仍未完成。

保管人已反馈 `KMS_SNAPSHOT_OFFLINE_SAVED`：首份正式快照通过本机完整认证校验并已复制到离线介质，首次离线交接完成。此确认不自动涵盖未来生成的快照；继续按已选人工下载制度更新离线副本，不重复生成 custody、密钥或初始化包。

### SECINF-03C-9 运行检查与审计日志轮转（实施中）

每 5 分钟独立检测：真实 KMS TLS/解封健康、线上证书剩余天数、三 Agent 实际短期身份、最近成功快照的摘要与新鲜度、备份 timer/service 结果、磁盘空间/inode、审计文件权限及持续写入。普通检查仅查询身份，不进行加解密或再次取快照；低于 14 天的证书、超过 90 分钟的备份、损坏完成包、备份任务失败及空间不足必须输出固定检查项失败和非零退出码。状态原子写入受限目录，不打印秘密、HTTP 响应或内部资源明细。

磁盘预警线为可用不足 20% 或 5 GiB，严重线为不足 10% 或 2 GiB；inode 不足 10% 告警。监控不删备份、不重启/解封 KMS、不读取 AppRole 内容；通知渠道另向负责人确认，未接通外部渠道时只记为本机检测与 journal/failed unit，不能宣称消息已送达。

审计轮转独立按小时检查，超过 10 MiB rename/create，保持 UID/GID 65532 和 0600，保留所有归档不自动删除，不使用 copytruncate。SIGHUP 只发送给核对过的专用 KMS 容器，随后查询实际 Agent 身份并确认新日志持续写入；失败返回非零，不静默丢弃。首次受控轮转保留旧日志并实际核验新日志，不重启服务器或触碰旧业务。日志归档/备份保留和容量处理仍需运维结合离线副本安排，不能无限保留后忽略容量告警。

03C-9A 本机检测已部署：六项真实检查均为 ok（线上 TLS/证书、Agent 租约、备份 unit、快照新鲜度与摘要、磁盘/inode、审计权限与活动）。每 5 分钟 timer 已启用，受限 oneshot 的首轮 `Result=success`、`ExecMainStatus=0`；仅状态目录可写，输出原子状态 JSON 和 journal，不读取 AppRole 内容、不做加解密/取快照探测。状态文件超过 10 分钟未更新必须视为检测器异常，不沿用旧绿色结果。通知方式尚在向负责人确认，当前明确标记 `notification_delivery=local_status_only`，不宣称已通知到人。

验证：监控与既有 Agent 的 Linux 用例全部通过（真实权限/原子状态文件用例无 SKIP），Windows 跳过两项仅 POSIX 适用的文件权限证明；实际新快照摘要验证和受限 unit 运行通过。当前阶段不自动删备份或日志，不声称审计轮转、证书续签或全平台上线已完成。

03C-9B 审计轮转现场通过：安装独立按小时的 10 MiB 阈值轮转，不压缩、不删除历史归档、不 copytruncate。首次受控强制轮转前记录当前 inode/长度/前缀摘要，轮转后确认原 inode 只移至唯一归档且原有字节摘要一致；新日志 inode 不同、UID/GID 65532、0600，实际身份查询继续产生日志。重新打开程序先核对专用容器的镜像、项目/服务标签、启动命令和审计挂载，再向该确切容器 ID 发 HUP，不调用 restart 或其他服务。输出 `FORMAL_AUDIT_ROTATION_PRESERVED_OLD_BYTES_AND_NEW_WRITES_VERIFIED`。

配置校验发现宿主没有名为 `65532` 的账号，logrotate 不能用账号名方式解析 `create 0600 65532 65532`；改为 `create 0600` 继承被轮转文件的原数字 UID/GID，重新打开程序再次强制核对归属。仅 dry-run 失败时没有轮转或更改日志，未为绕过解析而创建新的宿主用户。已根据实际安装版本手册确认 `rotate -1` 且无 `maxage` 保留所有归档。

常规定时器不带 force，只有超过阈值才轮转；首次受控验收与按小时 unit 均成功。检测器增加 `audit_rotation_job`，现场七项全部 ok，KMS 保持 healthy，旧业务及其他团队服务未变。Windows 25 项相关测试中 23 通过、2 项仅 POSIX 用例跳过；Linux 重新打开目标校验和监控测试均通过，真实受限 unit/日志验证通过。外部通知仍待负责人选择，当前不宣称已发送到人；证书续签及原生业务接入仍待后续任务。
