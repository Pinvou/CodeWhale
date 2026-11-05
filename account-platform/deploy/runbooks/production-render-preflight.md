# 当前生产装配的手动配置预检

这两个入口恢复原 business Compose / migrations 预检职责，使用当前个人与模型模块装配、现有迁移成功依赖和 Helm hooks。它们只执行 `docker compose config` 或 `helm template`，不会启动容器、连接数据库、应用 Helm、执行迁移或自动批准制品。成功仅输出 `BUSINESS_CONFIG_VALID` / `MIGRATION_CONFIG_VALID`、`ENV_PENDING`、`NOT_DEPLOYED` 和 `migrations_executed:false`。

## 输入与信任边界

继续使用独立核验的同源不可变 image、实际 workload subject 和备份证据库存。这里没有新增完整环境配置批准文件或批准服务，也没有自动生成批准库存的命令。制品摘要与 subject 的真实性仍由既有发行和站点核验流程负责；不能把本工具从候选镜像读取到的字符串当成独立审批。

所有输入必须放在受控私有目录。Compose 的原始 private env file、pgpass、agent config、Helm 私有 values 和 Secret 快照只在内存读取；不要将完整 `compose config`、解析后的环境、Secret 快照或 renderer 错误写入终端、CI 日志或普通证据目录。工具捕获 renderer 输出，失败仅输出固定错误码。实际生产输入不能使用测试 fixture。

recipe 是 JSON 对象：

| 字段 | Compose | Helm |
| --- | --- | --- |
| `kind` | `compose` | `helm` |
| `profile` | `personal` 或 `models` | 不使用 |
| `envFile` | 私有 Compose 插值 env 文件的绝对路径 | 不使用 |
| `overlays` | 明确的当前 overlay 名称数组，缺省 `[]` | 不使用 |
| `release` / `namespace` | 不使用 | 当前实际 release / namespace |
| `valuesFile` | 不使用 | 私有站点 values 的绝对路径 |
| `secretSnapshotFile` | 不使用 | 私有 JSON：`namespace/Secret名/key` 到该 key 的实际文本值 |

`personal` 固定使用 `compose.personal.yaml` 与 `compose.personal-workload.yaml`；`models` 继续叠加 finance、reconciliation、models。可显式选择现有 finance、reconciliation、signup、organizations、invocation、closure、approval-recovery、receivable-write-off、payment-financial-repair、ledger-repair、entitlement-correction、audit-export overlays，但不能重复基础组合已含的 overlay，organizations / invocation 必须以 models 为基础。工具不接受任意 `-f`、shell 或 renderer 选项。`--profile '*'` 只用于 `config` 的完整运维声明清点，不会执行这些 profile。不同模块新引入的私有进程或环境键若不在预检允许范围内，会明确拒绝；不能据此自行扩大权限或假定该组合已验证。正式配置回归实际覆盖的组合在测试与发行证据中列明；选择器列出名字不等于所有可排列组合已经运行验证。

Helm 使用当前 chart、`values-production-new.yaml` 与私有站点 values，保留三域 `pre-install,pre-upgrade`、权重和删除策略。Secret 快照是离线配置输入，工具不调用 Kubernetes，不证明快照等于当前已安装 Secret；缺少被消费的 key 明确失败，optional key 可为空，但生产迁移的备份 key 始终必需。系统 CA 的 DSN 使用 `sslrootcert=system`；当前迁移 scratch 镜像已包含系统根证书。该模式不需要额外 pgpass/CA volume。Compose 的文件式 CA 和 pgpass 则必须来自准确只读 bind，并实际存在且没有 symlink 路径。

inventory 是 JSON，包含：

| 字段 | 含义 |
| --- | --- |
| `version` / `kind` | `1` 与 recipe 的 kind |
| `rendererSHA256` | 经官方发行摘要核验的 renderer 可执行文件 SHA-256；不是镜像摘要 |
| `images` | 精确 workload ID 到已核验不可变 image 的对象，必须覆盖完整清点，没有额外项 |
| `subjects` | 实际 service 名到独立核验 subject；Compose 使用 Keycloak service-account UUID，Helm 使用当前 namespace-qualified ServiceAccount subject，不能互相替代 |
| `namespace` | Helm 的准确 namespace |
| `databases` | 按 account / billing / token 列出准确 `host`、`port`、`name`；personal 仅 account 与 billing |
| `backup` | `id`、`completedAt`、`sha256`、`manifestFile`，最后一项是备份 manifest 文件的绝对路径 |

Compose workload ID 是 service 名；Helm workload ID 是 `apiVersion/kind/metadata.namespace/metadata.name/container/container.name`，init container 使用 `init`。缺省 namespace 的字段为空，仍按 recipe namespace 解析 Secret。image 可以是准确本地 `sha256:` ID 或 registry `@sha256:` 引用，tag-only 拒绝。不要把 image digest 与配置输出 hash 混淆。

备份 ID、时间与 hash 必须与三个迁移进程的实际输入完全相同；时间遵循当前 migrate CLI 的过去 24 小时、最多未来 5 分钟规则。manifest 的实际字节必须匹配 SHA-256。工具没有执行备份或恢复演练，不把 hash 匹配称为可恢复性证明。

三域迁移登录固定为 `account_migrator` / `billing_migrator` / `token_migrator`，`MIGRATION_ROLE` 保持 `account_service` / `billing_service` / `token_gateway`。DSN 只接受准确域目标、`verify-full` 与受控 CA，不接受 options/service、多主机或重复 query 参数。Compose migrator pgpass 只包含本域本登录；runtime 不得读取 migrator 文件。Token 迁移现使用独立 `TOKEN_MIGRATION_DATABASE_DIRECTORY`，Gateway 与 credential Portal 继续使用原 runtime 目录。不要把两种密码放进 runtime 可读的同一 pgpass。

Account 与 Billing 的值班表保持原部署库存契约：至少一个非 nil 的小写 canonical UUID（version 1–8、RFC variant），逗号分隔且不得重复，不要求两名人员。运行时解析器还接受其他 UUID 编码；该宽容行为不是本部署预检的输入形式承诺。

实际 SQL 消费者必须提供非空且符合域身份/TLS 约束的 DSN，包括启用的 Credential Portal；缺少 DSN 不会跳过核验。只有当前 `personal` Compose 的 Token facade 沿用不连接数据库的职责，`models` 与 Helm 的 Token SQL 消费者没有此例外。

## 手动执行

先按现有 CI 固定版本核验 renderer：Docker Compose 5.4.0、Helm 3.16.4。只使用离线 config/template；不运行 daemon、pull、install、upgrade、up 或迁移命令。

```sh
npm --prefix account-platform run check:production-business -- --recipe /secure/site/recipe.json --inventory /secure/site/inventory.json --renderer /verified/tools/docker-compose
npm --prefix account-platform run check:production-migrations -- --recipe /secure/site/recipe.json --inventory /secure/site/inventory.json --renderer /verified/tools/docker-compose
node scripts/account-platform.mjs production check-business --recipe /secure/site/recipe.json --inventory /secure/site/inventory.json --renderer /verified/tools/helm
```

站点须自行完成原有 issuer/JWKS、真实身份、镜像来源、网络、CA、文件权限、实际数据库角色、备份恢复与发布门禁。工具不读取真实 workload JWT 或连接 NATS/KMS 来伪造 readiness；现有专属 NATS/KMS/备份门禁和实际部署验收仍需独立执行。内置 Keycloak 是独立基础设施职责，保留原 management 9000 started/ready、独立 sticky ingress 和精确 Identity token proxy 的渲染验证，不额外改变 Keycloak 分发包的写目录策略。

原独立 Keycloak 检查入口仍可对受控私有 Helm 渲染文件使用；文件不可落普通日志：

```sh
npm --prefix account-platform run check:production-keycloak -- /secure/site/rendered-helm.yaml
```

## 回归与证据边界

`test/contract/production-business-render.test.mjs` 使用当前真实模板及固定 renderer，检查个人、模型、三域 hooks、实际私有输入与 CLI。沿用现有 mandatory-render CI 环境变量 `PINVOU_TEST_KMS_COMPOSE=1`、`PINVOU_TEST_KMS_HELM=1` 和两个 `..._BINARY` 路径；在该门禁下不得 skip。公共 fixture 凭据只用于配置测试，不启动服务。直接 Node 测试可以在没有工具时明确 skip，不能把 skip 当作 render 通过。
