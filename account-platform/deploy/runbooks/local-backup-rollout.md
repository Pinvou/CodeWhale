# 可选本机 PostgreSQL 加密备份工具

本工具保持显式 opt-in，不安装/启用 systemd、不创建正式秘密/目录、不执行 Compose 或备份。启用前由当前部署负责人批准实际目标、维护窗口、原地接入与同机风险；历史现场授权、制品或 PASS 均不能继承。默认 [恢复策略](../recovery-policy.json) 与 [完整恢复手册](backup-restore.md) 的异机不可变副本要求不变。同机副本不能证明主机丢失可恢复，不能独立生成 RESTORABLE 或允许公网切换。

## 与当前布局的关系

该可选 overlay 只适配当前 `production/compose.dependencies.yaml` 的 PostgreSQL 服务：UID/GID 999:999、PostgreSQL17、SCRAM 文件认证、独立 TLS/管理员/bootstrap 目录、无公开数据库端口、只读根和 internal data 网络。业务模块仍使用各自部署文件；不得混合旧整套业务栈。数据库必须已保留 `.pinvou-bootstrap-complete` 与原 system identifier，禁止重新 initdb、覆盖 PGDATA 或为接工具升级/降级数据库。

这里备份整个物理集群及角色，固定覆盖 `account_platform_account`、`account_platform_billing`、`account_platform_token`、`account_platform_keycloak`、`account_platform_newapi`。四个账号领域逻辑备份仍按完整恢复手册执行；物理集群同时含 newapi 不代表独立模型网关的挂载、Secret、配置或数据已被备份。其他数据库/布局不能直接套用本工具，须单独核验与适配，不能删除额外数据库来通过检查。

`Dockerfile.postgres-backup` 只用于受控构建 PostgreSQL17.11/pgBackRest2.59.1 的工具扩展镜像；官方 `docker.io/library/postgres` 使用原固定内容摘要，安装前后 server/client 包版本必须完全相同。构建前确认站点就是该精确服务器版本/架构，并审核所有包和镜像制品；不匹配就停止，不替换现有数据库二进制。没有实际构建/验收日志不能把该 Dockerfile 当成可用制品证明。普通 Compose 不启用归档配置或调度。

## 受控接入前提

- PGDATA、加密仓库与备份秘密目录必须互不重叠、真实绝对路径且预先存在；数据/仓库/秘密为999:999、0700，秘密文件为0600普通文件。业务容器不挂载仓库/秘密。
- 从 `pgbackrest.conf.template` 只向独立私有目录渲染配置，提供一次性托管的 AES256 仓库密码与独立 pgpass；不可写入 argv、Compose environment、仓库或日志，不得覆盖旧秘密或恢复包。pgpass 只允许实际本机 socket 的 `account_platform_admin` 身份；现有 HBA/SCRAM、TLS及bootstrap角色不能放宽。
- repository 不自动过期（`expire-auto=n`），不设置丢弃 WAL 的队列上限；每周全量/每日增量、35天每日与13个月月度恢复点仍须实际容量和完整依赖政策。磁盘不足时明确失败，不清 WAL 或备份历史。
- 由操作者审查真实合并配置，显式叠加 `compose.dependencies.yaml` 与 `compose.postgres-backup.yaml`，提供已验镜像、`POSTGRES_BACKUP_REPOSITORY`、`POSTGRES_BACKUP_SECRETS` 以及当前既有路径；保留原数据/TLS/HBA/认证/资源限制。overlay 不是部署脚本，不会自动创建目录。
- 原地重建 PostgreSQL 容器属于现场写入，必须独立批准、先有变更前加密保护副本和可用回退评估；此仓库候选不执行它。启用后核对原集群标识、五库、角色和业务认证均保持。

## 受限命令与首次初始化

容器入口 `postgres-backup.sh` 仅支持 create/check/full/incr/info/stats；拒绝任意 SQL、pgBackRest 参数、restore、expire 或删除。口令从私有 PGPASSFILE 读取；统计查询用当前管理员身份经本机 socket，网络连接仍按既有 TLS 规定。

首次 stanza 与首个 full 由操作者在同一已核验容器通过该受限入口明确完成，保存其原结果和加密仓库证明。host 调度器要求已有 full 作为基线，不能用空 info 冒充初始成功。初始化动作发生未知结果时，先查原容器/仓库/进程，保留现场，禁止盲目重跑、删除仓库或重建集群来消除错误。未新增公开初始化 CLI，也不声称首次手动初始化受 host durable latch 保护。

首次真实备份、check 与隔离恢复成功后，操作者再投递 root:root 0700 的 `/opt/pinvou-postgres-backup`、0600 `job.json` 和0700 `state`。配置不含密码，精确键为 container_id/network_id/image_id/system_identifier/project/data_directory/repository/secrets_directory/mounts；mounts逐项为source/target/read_only，来自审查后的实际绑定，不从待验证对象自行推导为可信基线。

公开宿主入口只接受 full/incr/monitor：

```sh
python3 /opt/pinvou-postgres-backup/account-platform-postgres-backup-job.py incr --config /opt/pinvou-postgres-backup/job.json
```

full/incr 的显式 `--wait-lock` 最多等3200秒，仅等待锁竞争，不重试失败或未知备份。容器/镜像更换时须重新核验完整ID/挂载/网络与集群，不按名称自动追随。迁移前须另外取得新备份ID、完成时间与工件SHA256，传入当前迁移器的备份前提；本工具状态并不替代迁移来源或完整恢复集证明。

## 持久未知与进程归属

每次内部调用先 fsync 原子落盘 `state/inflight.json`，再执行原容器的固定 runner。runner 用本次独立 session/process group 和 timeout，清理并确认本组全部活进程退出后才返回确认 marker，不杀 PostgreSQL archiver或其他任务。宿主超时、Docker通道断开、启动归属不明、缺退出确认均保留原 latch；所有后续操作拒绝重放，不因 timer再次触发而消除未知。

只有收到原 runner 的退出确认才清理 latch；若工具已明确失败，状态仍非零失败。需人工解除未知时，先核完整原容器ID、原 started_at/operation、进程组已退出、仓库/WAL及原动作结果，保留审计后由现场操作者决定；没有自动强解锁/删除 latch 命令。不得通过换container ID/配置/目录让未知动作失去追踪。

## 可选调度和监测

六个 unit 文件供操作者逐项审核并显式安装/启用：周日02:00UTC full、周一至周六02:00UTC incr（Persistent），五分钟 monitor。未批准前不运行 systemctl enable/start。备份单次最多2700秒，带有界排队的systemd总上限6400秒；monitor整体120秒且服务留清理余量。

monitor 的 pgBackRest check 会产生恢复点/WAL切换，不能称只读探针。它检查实际加密仓库、归档统计、同集群/五库、60秒归档新鲜度、26小时日备份、8天全量、timer/backup service真实状态，以及仓库/PGDATA容量与inode。预警20%/5GiB，严重10%/2GiB或inode10%则失败；不因空间紧张自动删历史或关闭归档。root0600本地状态保留时间/容量/不可逆备份标识/固定错误码；只提供本地status/journal，不声称外部通知已送达。

## 本轮验证与未验证范围

无 Docker 的受控合同：`npm --prefix account-platform run test:postgres-backup-tools`（需要python3），或从仓库根 `python3 -m unittest discover -s scripts/tests -p "test_postgres_backup*.py" -v`。这验证配置、解析、绑定、未知 latch/非零状态和命令预算，子进程/权限/fsync边界有明确mock，不能称真实pgBackRest或Linux进程清理验收。

真正隔离演练必须显式执行 `python3 scripts/account-platform-postgres-backup-fixture.py --run --image <已审核本地sha256镜像ID>`，要求Linux root、Docker Compose、OpenSSL、Python3、固定已加载工具镜像。它只创建随机自有目录/容器/internal网络，不拉镜像、不接正式系统；真实full/incr、WAL、五库与角色、指定点及增量恢复、错误密码、宿主CLI/并发/进程组退出与未知窗口都须实际通过。恢复仓库只读，恢复实例关闭归档；清理前核创建ID/归属，创建结果不明保留现场。该命令会创建并删除自己的隔离资源，必须在专用主机明确授权后运行。

本候选未运行Docker构建、真实pgBackRest/PITR、Linux systemd或现场接入；不得继承旧手册中的历史现场成功。缺这些日志时保持环境未验证，不自动生成恢复证据或已部署结论。
