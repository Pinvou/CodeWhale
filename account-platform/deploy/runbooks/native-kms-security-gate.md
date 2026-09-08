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
| `TOKEN_SECRET_KEY_ID` | 稳定 Transit key 名，不是版本号 |
| `TOKEN_SECRET_PROVIDER_CA_FILE` | CA PEM 绝对路径 |
| `TOKEN_SECRET_PROVIDER_TOKEN_FILE` | 网关 Agent token 文件 |
| `PINVOU_SECURITY_CREDENTIAL_TOKEN_FILE` | 独立凭据 Agent token 文件 |
| `PINVOU_SECURITY_FRESH_DATABASE_URL_FILE` | 受限 PostgreSQL DSN 文件 |
| `PINVOU_SECURITY_NATIVE_ROTATION_PROOF_FILE` | 全新、尚不存在的私有探针路径 |
| `PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL` | 真实网关 readiness；HTTPS 或本机 loopback HTTP |
| `PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON` | `portal`、`identity`、`oidc`、`credentials` 四类 HTTPS 入口，含 name/url/expected_status；由部署清单指定固定预期状态，不根据失败结果修改 |

不得同时设置静态 `TOKEN_SECRET_PROVIDER_TOKEN` 或旧模式的 `PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID`。

1. 在开放业务前执行 `node scripts/account-platform-production-security-gate.mjs --prepare-native-rotation`。仅 `PREPARED_NOT_GO` 表明空库与第一版随机探针已保存，不表示可切换。
2. 保管人使用受控本机交互运维流程，对同一稳定 key 执行一次版本轮换。不得将恢复口令、运维 SecretID/root 交给 Agent；不得通过重新初始化 KMS、创建替代 key 或伪造探针越过此步。此门禁不包含运维轮换工具。
3. 在同一数据库实例、同一 CA/KMS/key、24 小时内执行 `node scripts/account-platform-production-security-gate.mjs`。它检查公开 TLS/预期响应、真实 readiness、双身份续租、版本递增、新旧随机探针解密及错误 AAD 拒绝。
4. 保存无业务秘密的结果及摘要，与独立的业务容器、身份/网络、数据库迁移、入口与结算验收证据共同审查；全部满足后才切换。版本轮换后仍须执行既有加密快照及离线备份流程。

数据库实例重启会改变绑定摘要并拒绝旧探针；应在业务仍未开放、确认确实空库的前提下保留原证据并使用新的探针路径重新准备。不得覆盖未知结果或用旧测试库伪装 fresh-install。正常会话完成后已清空的 PKCE 墓碑在 verify 时不计为旧密文；prepare 仍要求六表零行。只读查询固定 public 表/pg_catalog，RLS 遮挡会失败，而不会产生零行证明。

失败只输出固定错误码。不要发布完整环境、DSN、探针或原始响应。门禁不会降低 Transit 最低解密版本：旧版本必须保留以支持既有加密备份恢复。

## 隔离联验

本地 Node 合同测试使用 OpenSSL 动态生成测试证书，不提交测试私钥。默认从 PATH 或 Windows 标准 Git 安装目录取 OpenSSL；其他安装位置通过 `PINVOU_TEST_OPENSSL_BINARY` 指定。缺失依赖必须补齐，不能把 TLS 用例跳过后声称通过。

显式执行 `scripts/account-platform-openbao-fixture.py --isolated-fixture --config-directory <公开 OpenBao 配置目录> --native-gate-runner <scripts/account-platform-native-gate-fixture.mjs 的绝对路径>`。需要 Linux Docker 操作环境、本机 Node/psql/OpenSSL，以及预载的固定 OpenBao/PostgreSQL 镜像。

只创建全新 internal 网络、TLS/Raft KMS、双 Agent 和内存 PostgreSQL；不接入正式网络、不发布端口、不读取正式材料。真实双 AppRole 续租后运行正式门禁函数，证明空库、同版本拒绝、真实 v1→v2、六表旧密文拒绝、PKCE 墓碑及 RLS 拒绝。测试用 schema 只验证门禁 SQL/TLS/权限，不代替正式迁移或完整业务端到端验收。退出精确清理该 fixture 的容器/网络及临时目录。
