# NATS 闭合身份离线保管包

本工具在现有离线工具模块中使用官方 nkeys v0.4.15 和 age，不联网、不读取恢复私钥/口令/KMS 材料，不生成 broker 权限。构建：`go build ./cmd/nats-identities`，运行：`nats-identities -config <绝对路径>`。

v1 配置仅含 `recipient_file`、`recipient_sha256`（去除首尾空白后的恢复公钥摘要）、`output_directory`（未存在的新目录）。v2 还必须显式给出 `artifact_version: 2`、已经完整验证的 v1 `previous_directory` 与 5 分钟至 7 天内的 UTC 秒级 `overlap_expires_at`。v2 输出目录不得等于或位于 v1 目录内部。Windows 父目录预先限制为保管人与 SYSTEM；Linux 父目录仅当前保管人可访问。正式输出不能位于仓库、共享目录或业务容器中。

v1 固定六身份：三个 `account/billing/token-outbox-relay`、`billing-command-executor`、`token-command-executor` 和 `event-resource-operator`。v2 新增 `account-approval-relay`，沿用其余五个无关角色的原始 seed，并为 generic Account Relay 与授权 Relay 生成两个互不复用的新 seed。角色和权限均不能由调用方输入。

输出先写 `identities.age`（目的为 `pinvou-nats-service-identities`，包含闭合种子和公开映射），然后写每个 `<角色>/nkey.seed`、`public-keys.json`，最后写文件 SHA-256 清单 `manifest.json` 和绑定其摘要的版本化 `READY`。v1 清单含八个被摘要文件，v2 含九个。Linux 目录 0700、文件 0600；写后同步并读回，已有输出禁止覆盖。失败保留部分目录，不可部署或原地重试。种子和序列化缓冲尽力清零，Go/库内部内存不保证完全擦除。

`READY` 不代表现场验收。部署仍须核验闭合文件集合、摘要、公钥与种子对应及权限。只向各业务投递自己的种子；broker 只需公开映射派生的 ACL。初始化角色不投递给普通业务，串行初始化窗口结束后撤除其临时投递并关闭连接。`identities.age`、公开映射、清单及独立可信摘要交既有保管人离线保存，与口令分开；不要把整个未加密种子目录复制到业务服务器。

ACL 由 Account Platform 的 `event-resources -render-authorization` 基于固定领域规格生成。它只读经过可信 SHA-256 核验的公开映射，不能读取本包种子。v2 的有界重叠渲染包含五个共享身份、两个新 Account 身份和一个旧 Account 身份，共八个唯一用户；不带 previous 渲染最终七身份。静态配置不会自行到期，操作者必须在截止前受控重载最终七身份配置，断开旧连接并验证旧 seed 重连失败。包生成或重叠渲染均不等于撤权完成。疑似泄露按应急撤权处理，不回滚旧身份。正式投递与运行维护属于 SECINF04C2D。
