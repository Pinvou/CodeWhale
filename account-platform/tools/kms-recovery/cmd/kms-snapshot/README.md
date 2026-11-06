# KMS 加密快照

独立 Linux 单次备份命令，不修改离线 `kms-recovery` 的无网络边界，不读取恢复身份或 AppRole SecretID，不负责恢复、解封、轮换、删除或上传。

```text
kms-snapshot -config /bao/config/snapshot.json
```

公开 JSON 字段：`endpoint`（原始 KMS HTTPS origin）、`ca_file`、`recipient_file`、`recipient_sha256`、`instance_id`、`token_file`、`output_directory`。路径须为受控绝对路径，无符号链接；token/输出目录私有，输入与输出不得重叠。recipient 使用已经验证的保管人公钥及固定 SHA-256，不能临时换成部署者新建公钥。endpoint/实例/CA/recipient 与原初始化 operator 配置一致，以便离线绑定验证。

程序检查备份 Agent 的 token 只含 `pinvou-kms-backup` 策略、service 类型、剩余 60–300 秒且显式最长不超过 900 秒，再取快照；不会代申请或续租 token。可信 TLS、无代理/重定向/自动重试、整体网络期限 60 秒。压缩快照最大 64 MiB，gzip 完整性验证的展开上限 512 MiB；超过上限报失败，不能截断当作成功。

每次创建 `snapshot-<UTC时间>-<随机后缀>/`（0700），输出 `snapshot.age`（0600）与同步后的 `SNAPSHOT_READY`（0600）。包采用标准 age，明文是固定版本标识、加密内元数据及 Raft gzip；元数据包含实例/origin/CA/recipient/捕获时间/快照摘要。快照不落明文文件；只有完整密文写入、sync、读回摘要一致后才写完成清单。完成清单只包含时间、版本、密文长度和摘要，不包含令牌或解封材料。失败尝试保留，没有完成清单不能用于恢复。

保管人下载成功目录后，以本机 `kms-operator -action verify-snapshot -config <既有公开operator配置> -directory <custody目录> -bundle <snapshot.age绝对路径>` 输入既有恢复口令，验证完整 age 认证、绑定和 gzip CRC；仅输出 `KMS_SNAPSHOT_VERIFIED_OFFLINE`。不会连接服务器、输出快照或执行 restore。该命令不依赖正在运行的 SSH 隧道；CA 文件仍必须可读以核对原始绑定。保留捕获时的公开 CA 与 operator 配置供未来 CA 轮换后的恢复核对。

## 调度和保管

`deploy/openbao/compose.backup.yaml` 运行固定非 root 单次容器，仅挂载公开配置/recipient/CA、备份 Agent 独立 sink（全部只读）和加密输出目录（可写）。不挂载 KMS 数据、审计、引导身份或其他服务 token。宿主 root 仍是可信操作边界；不得用容器隔离描述成宿主 root 无法读取秘密。

安装对应 systemd service/timer，每小时执行，最多随机延迟 60 秒，错过后补跑。失败为 failed unit，不能忽略退出码；没有有效 token、KMS 封存、磁盘满或超限应修复原因后重跑，新建包不覆盖旧包。初期不自动删除快照，至少每日检查容量和最近成功时间，容量保留与主动告警接线前保持业务未放量。

已选异机策略：保管人定期下载并离线保存，不自动上传任何其他服务器。每个工作日至少保存最新成功包，身份/密钥变更后立即补存，记录实际时间和密文摘要。下载后先比对清单，再在本机做完整解密验证，最后将成功目录复制到离线介质；口令与恢复身份按既有制度分开保管。服务器“每小时成功”不是异机 RPO，真正可恢复点取决于最近离线包。尚未实施自动删除，不能把无限保留当容量保障。

快照必须与 custody、初始化解封包、CA 包共同可用。离线验证成功不等于真实恢复演练；隔离双实例测试证明机制可恢复，正式灾难恢复仍须受控目标与保管人参与，不在日常备份工具里开放 `snapshot-force`。
