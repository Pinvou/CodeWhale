# Portal 跨实例准入、容量与首次接管

本说明适用于 `token-portal` 和 Redis 8.2 standalone、数据库 0、`noeviction`。生产配置见 `deploy/production/redis.conf` 和 `redis-accounts.acl.template`（个人部署采用 `redis-personal.acl.template`）。不支持 Redis Cluster 的跨槽脚本。不得用淘汰登录票据的策略释放空间。

## 站点配置

| 环境变量 | 默认值 | 有效整数范围 | 含义 |
| --- | ---: | --- | --- |
| `PORTAL_OPERATION_RPS` | 100 | 1–10,000 | 每类操作的全站持续速率 |
| `PORTAL_OPERATION_BURST` | 200 | 1–1,000,000 | 每类操作的全站突发令牌数 |
| `PORTAL_LOGIN_FLOW_CAPACITY` | 65,536 | 1–1,000,000 | 未过期登录流程的全站数量上限 |
| `PORTAL_SESSION_HANDOFF_CAPACITY` | 8,192 | 1–1,000,000 | 未过期 handoff 及创建预留槽的全站数量上限 |

`/auth/login`（包括强制登录及未登录首页跳转）、`/auth/callback`、`POST /portal-session-handoff`、`POST /portal-session-handoff/consume` 分别使用一个所有实例共享的令牌桶。四类互不借用突发额度；不按 IP、用户或请求标识创建限流键。既有 HTTP 按进程限制继续生效。这里的默认值已按全站总量确认，不能按副本数相乘。

开发 Compose 的 `.env.example`、生产个人 Compose 的站点插值环境 和 Helm 的 `portal.operationRps`、`operationBurst`、`loginFlowCapacity`、`sessionHandoffCapacity` 对应上述变量。所有副本必须一致。改变配置时先停止旧副本并排空在途操作，再按同一组值启动新副本；不支持不同策略的滚动混用。旧令牌桶存活期间不匹配的策略返回 503，不覆盖旧桶。等待旧桶自然到期后可继续；桶到期时间是 `max(1 秒, burst/rps 秒)`，极端配置可能等待较久。不得手工删除活动票据或清空 Redis。

数量上限不是内存预留。生产 Redis 的 96 MiB 由所有已获授权的业务共同使用，大对象、会话和索引可能先触及物理上限。登录流程最大 16 KiB/10 分钟，handoff 最大 64 KiB/1 分钟；应结合实际对象大小与剩余内存调低数量阈值。写入遭遇 OOM 返回 503，保留原对象。仅四个固定且有界的速率哈希及删除脚本允许在 OOM 时工作，便于已接受的消费者释放空间；这不保证内存不足时新会话能写入成功。

## 首次升级和恢复旧快照

1. 确认 Redis 8.2、`noeviction`、运行角色采用新 ACL。首次升级先停止并排空所有旧版本 Portal 生产者和消费者。备份及受控运维凭据留在原保管系统。
2. 使用同一发布镜像的 `token-portal` 二进制，以已有获授权的 Redis 运维身份设置 `REDIS_URL`，TLS 站点同时设置 `REDIS_CA_FILE`。不要把 URL 或密码作为命令参数、回执或日志内容。此身份须能对两个原有票据前缀执行盘点，运行角色不增加全局 `SCAN` 权限。
3. 默认只读检查：`token-portal capacity-inventory`。输出仅含两个 ready 标志和当前未过期槽数量；未就绪退出非零。新安装的空库也需首次建立元数据。
4. 明确应用元数据盘点：`token-portal capacity-inventory --apply --timeout=5m`。可选超时大于零且不超过 30 分钟。每次只扫描一页，原对象与原生到期时间保持不变。中断后以同一命令继续，游标仅在完整一页处理成功后前进；不需要 IdP 配置，也不启动 HTTP 服务。
5. 确认只读输出两个 ready 都为 true 后启动新副本。新版本消费者也可在盘点期间继续消费旧票据；新创建在元数据未就绪时返回 503。后台 worker 随 HTTP 进程启动、独立于新流量推进，并随进程取消退出；使用有限运行角色时会等待上述运维盘点。不要重新启动旧版本写方。

运维盘点涉及 `SCAN`、`TYPE`、`TIME`、`PEXPIRETIME`，以及 `portal:transient-capacity:v1:*` 下的哈希/有序集合元数据写入。Redis 的全局枚举权限不能按键前缀隔离，因此只交给已有受控运维身份；不把它附加到生产 Portal 角色。检查前缀固定为 `portal:flow:*`、`portal:session-handoff:*`，不读取或导出其中的凭据正文。

上述 `token-portal` 是本地构建的二进制名。发布镜像为 scratch，实际入口是 `/service`，没有 shell，也没有名为 `token-portal` 的 PATH 命令。运行一次性运维容器时，将 `capacity-inventory`（或 `capacity-inventory --apply --timeout=5m`）作为同一发布镜像的入口参数；由部署系统注入运维身份的 `REDIS_URL`、容器内的 `REDIS_CA_FILE` 和对应只读 CA 挂载，并接入已有受控网络。例如 `docker run --rm --network <受控网络> --env REDIS_URL --env REDIS_CA_FILE <同一发布镜像> capacity-inventory`；尖括号需替换，TLS 站点还须配置上述 CA 挂载。`--env REDIS_URL` 从当前受控环境取值，不把连接串写进 argv。只读检查现有容器也可调用其 `/service capacity-inventory`，但它仍使用该容器的运行身份，不能代替运维身份执行首次盘点。

原对象没有 TTL、类型不符、元数据损坏、权限不足、OOM 或单页异常大时，盘点失败并保留原对象，不伪造 ready。先根据受控备份/来源核查错误对象；没有明确授权和事实依据时不得删对象、补造日期或缩短原 TTL。旧对象多于新容量时如实全部计入，新创建等待自然到期或正常消费释放。

### 安全诊断和元数据完整性

CLI 失败退出非零，只输出固定原因、`component`（flow/session-handoff/unknown）和 `stage`（prepare/inspect/inventory/arguments）；`--apply` 失败时不会伪造成功 JSON。worker 使用同样的安全分类；同一连续故障的每组原因/类别/阶段最多每分钟记录一次，恢复后的新故障立即可见。Redis 客户端自身诊断也仅保留固定事件，最多每分钟一次，不输出原始格式串、异常、地址、票据或凭据。HTTP 错误体继续保持既定通用错误。

| 固定原因 | 处置方向 |
| --- | --- |
| `PORTAL_INVENTORY_OPERATIONS_REQUIRED` / `PORTAL_INVENTORY_PREPARING` | 使用已授权运维身份盘点，或在原预算内继续未完成的盘点；不要给运行角色增加 SCAN |
| `PORTAL_ADMISSION_COMMANDS_REQUIRED` / `PORTAL_INVENTORY_AUTHENTICATION_FAILED` | 核对运维凭据及本节命令权限；不打印凭据 |
| `PORTAL_TRANSIENT_TTL_REQUIRED` / `PORTAL_ADMISSION_STATE_INVALID` | 在受控环境核查原对象类型/原生 TTL 及元数据类型，根据可信来源处置；不自动删除或补 TTL |
| `PORTAL_INVENTORY_PAGE_TOO_LARGE` | 保留当前游标与原对象，安排受控盘点；COUNT 是提示，重复调用不保证缩小同一页 |
| `PORTAL_INVENTORY_REDIS_OOM` | 恢复 Redis 物理容量，保持 noeviction，不删除活动票据腾空间 |
| `PORTAL_INVENTORY_CONFIGURATION_INVALID` / `PORTAL_INVENTORY_ARGUMENTS_INVALID` | 核对连接/TLS 配置或命令预算/参数 |
| `PORTAL_INVENTORY_REDIS_UNAVAILABLE` / `PORTAL_INVENTORY_TIMEOUT` / `PORTAL_INVENTORY_CANCELED` | 核实依赖与预算后明确再次调用；盘点沿已提交游标继续，不重做一次性消费 |
| `PORTAL_CAPACITY_INVENTORY_FAILED` | 未分类故障；保留非零状态，在受控环境进一步诊断，不导出原始异常或敏感数据 |

两个原票据前缀、两个容量有序集合和两个 inventory 哈希必须按同一个一致性快照备份/恢复。`ready=1` 只表示曾完成一次扫描，不会持续证明索引完整；`--apply` 会跳过已有 ready 的类别。因此不得只恢复原票据、只删除索引、或把 ready/count 输出当作完整性校验。全部票据自然到期后索引正常消失不属于损坏。

运维需配置元数据偏差巡检与告警：通过受控身份按固定原票据前缀分批 SCAN，在 Redis 侧用 TYPE、PEXPIRETIME、TIME 与对应 ZSCORE 检查仍有效的原对象是否具有同一到期时间的索引成员，只返回检查/不匹配数量，不导出票据 key/id 或正文。缺成员、截止时间不同或类型异常均应告警并阻止继续按旧 ready 判断安全；运行角色不获得此枚举能力。本 PR 不提供自动巡检调度或自动修复。只读 `HMGET <对应inventory键> ready cursor` 可定位进度，但零计数或总数相等均不能代替上述逐项一致性核查；在线检查存在到期/消费交错，修复前必须完成下述停排空核实。

发现部分元数据丢失/损坏后：

1. 停止并排空全部 Portal 生产者和消费者，确认外部交换/在途写入已结束。至少等待最后一次准入产生的 2 分钟 handoff 预留自然到期；无法确认在途状态时保持停止，不删除未知对象。
2. 在受控身份下核对并保留原对象及备份。仅在已确认元数据需要重建时，单次显式删除以下四个元数据键，再执行上述发布镜像的 `capacity-inventory --apply --timeout=5m`：`DEL portal:transient-capacity:v1:flow portal:transient-capacity:v1:flow:inventory portal:transient-capacity:v1:session-handoff portal:transient-capacity:v1:session-handoff:inventory`。此受控重建需要运维身份的 DEL 权限，不增加运行角色权限，不使用通配符，不删除原票据或全库。
3. 检查两个 ready 都为 true、偏差核查无异常后再启动副本。若失败，保持停止，依据安全分类修正原因后继续；不要重启旧写方或伪造 ready。正常中断续盘不需要执行 DEL。

inventory 哈希没有 TTL。版本回退同样先停排空全部新实例；旧写方可能生成未计数对象，之后重新升级时必须按上述受控重建流程处理，不能沿用旧 ready。不要以清理 inventory 为由删除原票据或整个 Redis 数据库。

## 拒绝、未知结果和继续操作

确定额度或容量不足返回 429、`RATE_LIMITED` 和向上取整的 `Retry-After`（1–600 秒）；有效期、一次性凭据和既有浏览器会话不因该拒绝被改写。消费者在领取前检查操作配额，额度恢复后可在原有效期内重试原票据。Redis 依赖故障、策略冲突或未完成盘点返回 503；在操作准入阶段发现盘点未完成时附带 1 秒重试提示，后续保存/预留路径的存储错误不承诺此响应头。

handoff 创建在 Account/IdP 调用前预留容量。未完成调用最多占用 2 分钟预留槽；正常失败主动释放。成功写入的票据保持至原到期或一次性消费，迟到清理和未知成功写入不能释放其占位。后台进程取消时清理有独立的 3 秒预算；崩溃遗留预留自然到期。

一次性读写不做 Redis 客户端自动重试。服务端已消费但响应丢失时，本次返回 503，不能承诺旧票据可再次使用。后续明确重试可能得到过期/已消费，应重新发起登录或申请新的 handoff；这是临时登录凭据，不是财务命令的自动重执行。写入成功但交付 ticket 丢失时对象保留至原 TTL，禁止通过删除或重复外部交换猜测成功。

## 验证边界

`scripts/test-portal-redis.py` 强制执行 15 个真实 Redis 根用例及指定容量子场景，跳过、漏选或失败均不接受。CI 使用锁定的官方 Redis 8.2 镜像、独立 loopback 端口和匿名卷，先核对容器与转发端口的同一运行身份，再建立测试角色和伪造对象；最后核对容器与卷已退出。纯 Python 守卫测试不能代替实际 Docker 执行。隔离运行证据也不等于生产容量压测、现场切换、合并或发布资格。
