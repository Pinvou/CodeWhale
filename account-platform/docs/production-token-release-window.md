# 正式 Token 发布权限窗口

状态：**C4C3a 原生只读清点及临时资源清理已完成；C4C3b/C4C3c 未执行。**
承接 [三库迁移](production-business-migrations.md) 与
[业务切换中的消息资源复核](production-business-cutover.md)。
只操作本次正式账号平台，不清空正式库，不触碰旧测试业务或其他系统。
本页记录当前有效方案；失败尝试仅作诊断证据，禁止重跑其执行器。

## 任务与授权边界

1. C4C3a：使用冻结的原生 `token-release-prepare` 工具执行只读 inventory，验证受控 DBA 接入和安装状态。
   独立验收后移除准确退出工具、临时 Socket 导出及唯一管理员密码消费副本。
2. C4C3b：根据实际清点结果，另行设计并独立审核初始化 writer 隔离、维护角色退场、两运行角色 NOLOGIN
   准备、新消费密码的加密保管与有限有效期、Account 策略运维身份、准确 HBA 与正负认证验证。
3. C4C3c：按既有工具验证计量上限与审批策略，治理 CAS 激活后单独 enable 本次两运行角色；
   退场临时维护身份并验证运行角色不能修改治理和策略。业务启动与 KMS 仍须后续现场验收。

本次 C4C3a 的审核及执行不包含 fence、prepare、enable、角色创建、HBA 更新或治理写入。
后两项须补充准确角色、权限、密码投递、批准依据与失败恢复步骤再审核。
原生工具保护 migrator/policy-operator，不得混入普通 retired-roles 绕过保护；
也不能用超级用户或 runtime 身份替代独立 Account 策略运维身份。

## 当前有效设计：隔离 Socket DBA 清点

### 来源及挂载边界

复用正式 PostgreSQL 本地 Unix Socket 与既有 SCRAM 管理员身份，不开放管理员 TCP，
不修改 HBA、PG 容器、PGDATA、数据库权限或归档，不在宿主直接运行 DBA 业务二进制。
使用当前冻结 image ID 内 `/service` 与 `/migrations/token`，不重新编译或下载替代版本。

来源只允许已核验的准确 PG 容器当前 PID 下 `/proc/<verified-pid>/root/run/postgresql`。
记录 container/image ID、PID、启动时间、PGDATA、system identifier、源挂载记录以及目录/Socket
的设备、inode、UID/GID、权限；与准确容器内 `stat` 逐项相等。
实际目录为 999:999/03775、Socket 为 999:999/0777，不把模板 tmpfs 的 0700 当作实际 inode 权限。
禁止 `/root/var/run` 的绝对软链跳转，也不对 `/proc/<pid>/root` 特殊入口使用 realpath 规范化。
前后若进程、挂载或 Socket 身份变化即停止，不自动追随新 PID。

Socket 导出使用同内核隔离样例已验证的一次性 FD helper，无常驻进程：

1. 在任务私有窗口下新建**空的专用父目录**，非递归 self-bind 后只将它设为 private。
   不使用包含 pgpass、回执或正式数据的整个工作窗口，不修改宿主根或其他服务的挂载传播方式。
   记录宿主父挂载及其初始空目录传播副本；父级建立时存在传播，不声称从未产生副本。
2. 父进程保持宿主命名空间，预先固定准确 PG 来源目录 FD、命名空间 FD 与宿主 `/proc` 目录 FD。
   子进程只进入准确 PG 挂载命名空间，以来源目录 FD 和空路径执行非递归 `open_tree(CLONE)`，
   在匿名 detached 状态通过 `mount_setattr` 设置 readonly/nosuid/nodev/noexec/private。
   不挂接、移动或 remount 原 PG 目录，不持有 DBA 密码或执行数据库程序。
3. 子进程通过本地 socketpair/SCM_RIGHTS 传回唯一 FD，固定消息、拒截断及额外 FD，父进程有界等待准确
   子进程正常退出。目标容器 `/proc` 可能不暴露宿主 PID，子进程通过预开的宿主 `/proc` FD 相对核验
   自身准确 PID 的命名空间；这个 FD 不挂到清点工具。
4. 父进程核对 FD 设备/inode及只读属性，核对目标 FD 所属挂载确为 private，才用 `move_mount` 挂入
   空父目录下唯一空子目录。随后验证导出 private、只读安全选项完整、无子挂载、Socket 双边身份相等。
   逐命名空间检查：只有宿主父级允许这个准确 Socket 子挂载，其他副本仍只有原空父，不得收到 Socket。

以上每个副作用前持久化排他回执；未知结果保留现场，不自动重挂、重跑或在 finally 中盲目清理。
挂载 ID 可被内核复用，必须连同目标、源设备/inode及完整挂载记录判断，不能仅凭编号判断来源或残留。

### 凭据与工具隔离

从既有管理员密码派生仅匹配 localhost:5432:目标 Token 库:既有管理员的单行 pgpass。
消费目录 999:999/0700，文件 0600；秘密不进入 env、参数、日志或本机下载，不生成或轮换原密码，
不索取新的离线保管确认。DSN 无密码，显式 `default_transaction_read_only=on`。
此连接为本地 Unix Socket，不是 TLS；不因此放宽任何既有 TCP 的 verify-full 要求。

准确工具命令只为 `/service --operation inventory --expected-database <固定 Token 库> --migrations /migrations/token`。
不传 apply、fence、prepare、enable、terminate-sessions 或候选角色。
新 CID 创建后先 inspect 全部配置，随后只启动一次：

- UID/GID=999:999、network=none、无宿主端口、只读根、cap_drop=ALL、no-new-privileges、core=0。
- 256 MiB、0.5 CPU、64 PIDs、restart=no、无健康检查、私有 noexec tmpfs、有界日志。
- 只有两项精确只读 rprivate bind：已验 Socket 导出目录、该工具自己的 pgpass 目录。
- 不挂 PGDATA、管理员原目录、整个宿主 /proc、Docker Socket 或其他身份。
- 保留原生 30 秒 context，外层最多等 50 秒；超时、非零退出或证据不符立即保留，不重试。

成功要求退出 0、原生 JSON 精确匹配，并再次验证完整 Token 迁移账本的版本/名称/摘要、治理记录和角色
属性与执行前一致；原数据库/身份九个容器健康、工具连接为零。仅 count 或退出码不能代替这些验证。

### 精确清理

独立验收后先按准确 CID 非 force、非删除卷地移除已退出工具。
随后正常卸载准确 Socket 导出，全命名空间核无残留，再确认底层目录非挂载点且为空，只删除该空子目录。
最后正常卸载准确私有父挂载；其宿主父级仍为原共享根，卸载传播回收原空父副本，逐命名空间再次核零。
不使用 lazy、force 或递归删除，不直接删除已挂载目录中的内容。

确认无其他容器通过祖先挂载消费本窗口 pgpass 后，按准确路径、设备/inode、属主与权限删除唯一消费副本。
原管理员材料摘要及权限保持不变；正式数据、备份、加密恢复包和全部回执保留。
删除消费副本不等于管理员凭据已撤销。

## 诊断与隔离实测

- 初次 OCI 直接绑定跨命名空间来源失败，工具没有启动；随后传统 bind 被用户态路径规范化引向宿主
  Socket，来源守卫在新工具创建前拒绝。仅加 `--no-canonicalize` 的方案在执行前停止：传统 bind 的
  内核跨命名空间限制仍存在。
- 首次 FD 样例已挂接只读副本，但共享宿主父级使其重新成为 shared 并传播；最终属性守卫拒绝。
  第二样例尝试逐命名空间回收空父副本，因 PrivateTmp 遮蔽路径停止，尚未接 Socket。
  两次样例均无秘密或数据库操作，各自准确挂载及传播副本已全部回收，回执保留。
- 核对内核实现后明确：挂接时共享父级会传播；卸载传播也取决于**被卸载挂载的父级**，不是该挂载自身
  是否 private。因此最终方案不再进入其他服务命名空间执行卸载，改为上面的宿主父子生命周期。
- 第三次完整样例实际通过：目标 `/proc/self` 不可用、四类错误 FD 消息拒绝且 FD 关闭、共享父级在
  clone 前拒绝、真实 Socket 连接、导出拒写 EROFS、源仍可写、Socket 无传播，以及父子/全部副本精确清理。
  独立现场复核三个样例在当前全部 45 个挂载命名空间均无残留。

机制依据：[Linux 5.15 挂载实现](https://raw.githubusercontent.com/torvalds/linux/v5.15/fs/namespace.c)、
[卸载传播实现](https://raw.githubusercontent.com/torvalds/linux/v5.15/fs/pnode.c)、
[mount 路径规范化说明](https://man7.org/linux/man-pages/man8/mount.8.html)。
最终结论以相同内核的隔离实测和正式前后核验为准，不以文档推断替代现场结果。

## C4C3a 正式验收结果

受审导出执行器成功，Socket 仅在宿主 private 父级下以只读方式出现，旧错误路径及其副本均已清除。
随后当前冻结原生清点镜像实际执行一次，退出 0，独立现场核对镜像/命令/环境/限制/两挂载/日志全部通过：

| 字段 | 实际结果 |
| --- | --- |
| decision / environment_status | CHECKED / ENV_PENDING |
| state / epoch | PRE_ACTIVATION / 0 |
| migration_version | 31 |
| writer_logins / writer_sessions | 3 / 0 |
| applied | false |

可登录 writer 为 token_gateway、token_migrator、token_policy_operator；这不等于已隔离或可启动业务。
完整账本、治理状态/epoch/候选角色/摘要/更新时间、transition 数及五个控制角色属性前后严格一致。
原 PG 进程与源挂载仍可写且未改变，九个原服务健康，工具数据库连接为零，Socket 未传播。
最后受审清理执行器成功移除准确退出容器、导出及全部空父副本和唯一管理员密码消费副本，
原管理员材料、正式数据、备份与回执保留。

本项仅完成 DBA 清点与隔离适配；未变更业务 Go/前端代码、未启动业务服务、未激活治理、未切换公网。
后续 C4C3b/C4C3c 仍须独立设计、审核、执行与验收，不能据此声明完整部署完成。
