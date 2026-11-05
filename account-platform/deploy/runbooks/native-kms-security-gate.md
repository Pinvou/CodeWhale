# 原生 KMS 全新安装门禁

适用：`vault-transit`、全新 Token 数据库、不迁移旧测试数据。既有 `remote` 的旧 key 退役门禁保持不变；原生迁移不是此流程支持的模式。运行本门禁不等于执行部署、轮换或删库。

## 前置条件

- 全新数据库已执行正式迁移，并通过独立的卷/实例来源核验；六类密文表尚无行。
- 网关及凭据服务各自的 Agent 已实际续租；固定独立角色与策略分别为 `pinvou-token-gateway`、`pinvou-credential-portal`，无 default/root 或额外 entity/group 身份策略。短期租约剩余不超过 300 秒、显式最长 900 秒。
- 专用 CA 包和叶证书有效期均超过 14 天，KMS 地址是证书匹配的 HTTPS origin。不能把私钥放入 CA 文件。
- 执行环境有 Node 和 `psql`；数据库账号仅需连接、public schema 使用及六类密文表 SELECT 权限，不需要超级用户/写权限。连接必须使用 `sslmode=verify-full` 和专用 `sslrootcert`；可选固定 `hostaddr`，其他连接选项拒绝。
- 私有证据目录为 0700；token、DSN、探针文件为 0600 普通文件，路径不经符号链接。Windows 在调用前另验仅保管人和 SYSTEM 的 ACL；门禁不声称跨平台 mode 能替代 ACL。
- 现场执行身份可以读取两个受限 token sink，不得把两个身份合并，也不得把这份运维读取权限赋给任一业务容器。

## 配置与顺序

通过受控进程环境设置以下非秘密配置；密码仅存入受限 DSN 文件，不放命令行、示例或日志。

| 变量 | 含义 |
| --- | --- |
| `TOKEN_SECRET_PROVIDER` | 固定 `vault-transit` |
| `PINVOU_SECURITY_INSTALL_MODE` | 固定 `fresh-install` |
| `TOKEN_SECRET_PROVIDER_URL` | 专用 KMS HTTPS origin |
| `TOKEN_SECRET_KEY_ID` | 当前部署的稳定 Transit key 名（默认 `pinvou-token-data`），不是版本号 |
| `TOKEN_SECRET_PROVIDER_CA_FILE` | CA PEM 绝对路径 |
| `TOKEN_SECRET_PROVIDER_TOKEN_FILE` | 当前网关 sink `/run/pinvou-kms-agents/pinvou-token-gateway/token` 对应的运维可读绝对路径 |
| `PINVOU_SECURITY_CREDENTIAL_TOKEN_FILE` | 当前独立凭据 sink `/run/pinvou-kms-agents/pinvou-credential-portal/token` 对应的运维可读绝对路径 |
| `PINVOU_SECURITY_FRESH_DATABASE_URL_FILE` | 受限 PostgreSQL DSN 文件 |
| `PINVOU_SECURITY_NATIVE_ROTATION_PROOF_FILE` | 全新、尚不存在的私有探针路径 |
| `PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL` | 真实网关 readiness；HTTPS 或本机 loopback HTTP |
| `PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON` | `portal`、`identity`、`oidc`、`credentials` 四类 HTTPS 入口，含 name/url/expected_status；由部署清单指定固定预期状态，不根据失败结果修改 |

不得同时设置静态 `TOKEN_SECRET_PROVIDER_TOKEN` 或旧模式的 `PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID`。

1. 在开放业务前从仓库根执行 `npm --prefix account-platform run security:prepare-native-rotation`。仅 `PREPARED_NOT_GO` 表明空库与第一版随机探针已保存，不表示可切换。
2. 保管人使用受控本机交互运维流程，对同一稳定 key 执行一次版本轮换。不得将恢复口令、运维 SecretID/root 交给 Agent；不得通过重新初始化 KMS、创建替代 key 或伪造探针越过此步。此门禁不包含运维轮换工具。
3. 在同一数据库实例、同一 CA/KMS/key、24 小时内执行 `npm --prefix account-platform run security:production-readiness`。它检查公开 TLS/预期响应、真实 readiness、双身份续租、版本递增、新旧随机探针解密及错误 AAD 拒绝。
4. 保存无业务秘密的结果及摘要，与独立的业务容器、身份/网络、数据库迁移、入口与结算验收证据共同审查；全部满足后才切换。版本轮换后仍须执行既有加密快照及离线备份流程。

数据库实例重启会改变绑定摘要并拒绝旧探针；应在业务仍未开放、确认确实空库的前提下保留原证据并使用新的探针路径重新准备。不得覆盖未知结果或用旧测试库伪装 fresh-install。正常会话完成后已清空的 PKCE 墓碑在 verify 时不计为旧密文；prepare 仍要求六表零行。只读查询固定 public 表/pg_catalog，RLS 遮挡会失败，而不会产生零行证明。

失败只输出固定错误码。不要发布完整环境、DSN、探针或原始响应。门禁不会降低 Transit 最低解密版本：旧版本必须保留以支持既有加密备份恢复。

## 隔离联验

本地 Node 合同测试使用 OpenSSL 动态生成测试证书，不提交测试私钥。默认从 PATH 或 Windows 标准 Git 安装目录取 OpenSSL；其他安装位置通过 `PINVOU_TEST_OPENSSL_BINARY` 指定。缺失依赖必须补齐，不能把 TLS 用例跳过后声称通过。

从仓库根运行 `node --test account-platform/test/contract/production-security-gate.test.mjs account-platform/test/contract/native-kms-gate.test.mjs`。测试实际启动本机 HTTPS fixture，使用默认公开 TLS/HTTP 客户端和真实 Node CLI；原生路径实际使用双身份 Transit HTTPS 协议客户端，但 KMS 的版本/角色/AAD 响应由受控服务提供，数据库检查在该联验中注入受控结果。DSN 拒绝用例在启动 psql 前失败，不能代替真实 PostgreSQL 的 TLS/RLS/六表查询验收。测试不启动 Docker/OpenBao、Agent 或业务服务，不读取现场身份，不授权任何轮换或部署。

现场仍须以真实 `psql`、正式迁移后的空库、真实短期双 AppRole、实际 Agent 续租及同一稳定 key 的保管人轮换执行上述 prepare/verify；补齐六表读取/RLS 拒绝、同实例绑定、当前及历史版本可解密证据。已有隔离 OpenBao fixture 或 `kms-agent-check` 通过，均不能代替这些完整联验。缺少现场依赖时保持未验证，不将受控测试结果改称生产 GO。

## 既有 remote 迁移分支

`TOKEN_SECRET_PROVIDER=remote` 与原生 fresh-install 分支独立。保留原 remote 服务协议，设置固定 `TOKEN_SECRET_KEY_ID`、不同的 `PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID`、受控的 `TOKEN_SECRET_PROVIDER_TOKEN`、`PINVOU_SECURITY_KMS_HEALTH_URL`，以及同名 public/readiness 配置；显式设置安装模式时必须为 `migration`。remote 不支持 prepare 命令。公开入口至少包含 `portal`、`identity`、`oidc`，各自声明精确状态；TLS 证书仍须超过 14 天。

运行 `npm --prefix account-platform run security:production-readiness` 后，只有真实 readiness 为 ready、remote 健康证据中 active key 为 `ENCRYPT_DECRYPT` 且旧 key 为 `DISABLED` 才产生该工具的 GO。不会执行旧密文迁移、禁用 key 或修改服务配置；现场必须另证所有旧密文、快照与恢复的兼容。remote 的旧 key 禁用条件不适用于 vault-transit 原生版本历史，不能据此提高原生最低解密版本。

此手动汇总工具补充现有职责：`kms-agent-check` 检查运行身份/租约/ACL，`kms-monitor` 负责持续健康与到期监控，`kms-operator` 负责受控单次轮换及原上下文未知结果恢复。它不取代任一工具，也不改变 Compose `depends_on`、Helm hooks 或现有迁移启动流程。
