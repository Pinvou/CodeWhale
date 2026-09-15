# 审批授权专用连接与发布门禁

> GSA-06 的配置合同；不是生产激活记录。GSA-04 的 Linux 环境证据未补齐前，不得执行生产 QUIESCING、切流或激活。

## 身份隔离

- 常驻 Account 服务只接收 `APPROVAL_WRITER_DATABASE_URL`，登录身份必须为 `account_approval_writer`；不得复用 `DATABASE_URL`、迁移身份或回填身份。
- 回填由独立受控进程持有 `account_approval_backfill` 凭证，通过 `NewApprovalBackfillPool` 验证真实会话身份、角色属性和权限。不得把回填凭证放进 Account 服务环境、Helm 运行时注入或常驻容器。
- 两个登录角色均不得具有超级用户、建库、建角色、复制或绕过 RLS 能力；不得具有正向或反向角色成员关系。运行身份不能成为任何受保护 owner 的成员。
- 生产 Compose 的 writer DSN 不含密码，使用现有只读 `PGPASSFILE` 中对应 writer 的独立条目及 `verify-full` TLS。创建角色、设置独立凭据、HBA 与密码文件投影必须先完成；仅增加 DSN 不代表具备连接能力。
- Helm 只向 Account 注入 `account-approval-writer-database-url` Secret 键。该键允许缺失，以保留查询和安全收口；缺失时批准能力关闭，不能据此视为发布成功。Secret 的生成、权限和轮换走现有受控部署流程，不把明文写进 values、日志或命令行。

## 可用性与探针

常规 `/health/ready` 继续检测普通服务依赖；专用 `GET /health/ready/approval-writer` 检查 writer 的实际会话、属性、所需权限及不应具有的权限。它仅返回 `ready` 或 `not_ready`，始终 `Cache-Control: no-store`；不可用返回 503 和 `Retry-After: 5`，不返回连接串或数据库诊断。

发布门禁必须单独检查 writer 探针，禁止让新版本承接批准流量直到该探针通过。不要简单用它替换整个实例的存活/就绪探针，否则 writer 故障会连带撤掉仍能拒绝、撤回和读取历史的实例。探针通过不代替审批 activation、控制状态、当前权限、版本和事务内的检查。

批准入口发现 writer 不可用时返回稳定 `APPROVAL_WRITER_UNAVAILABLE`，不回退普通身份。连接或权限在事务中发生变化时仍应整事务回滚；提交结果不确定时必须查询原申请，不得因为收到 503 或超时而自动重新提交。

连接配置只在进程启动时设置。有效配置遇到短暂连接/权限故障时保留独立连接池，后续探针可恢复；变更 DSN 或凭据文件后按既有发布流程回收旧连接/重启实例。服务关闭时同时关闭普通池和 writer 池。

## 独立回填命令

由受控运维会话从秘密管理设施注入 `APPROVAL_BACKFILL_DATABASE_URL` 后，在仓库根目录执行：

```sh
node scripts/account-platform.mjs approval-backfill --batch=100 --deadline=5m
```

不要把 DSN 写进参数、终端历史或报告。发布包可独立构建 `account-platform/cmd/approval-authorization-backfill`，运行同样的参数；该命令不会启动 HTTP 服务，也不读取普通服务或 writer 的 DSN。每批允许 1–200 行，总时限允许 1 秒–30 分钟；它只扫描启动时捕获的高水位，超时/取消可以停止并由操作者再次运行。

标准输出为脱敏 JSON 状态和计数；单行阻断仅追加事件 ID、有限原因码、schema/serialization 版本和 12 位 payload 摘要前缀，不输出数据库错误、凭据或审批 payload。摘要前缀仅用于定位，不是完整性证明。退出码 0 仅表示这次高水位回填完成，不代表整个在线数据集已静止，也不允许激活；1 表示失败或仍有验证缺口，2 表示参数或凭据来源缺失。正式切换仍须显式执行只读最终验证及后续任务拥有的控制 CAS。

## 回滚与保留风险

迁移 `000051` 的 down 只能在 LEGACY/RUNNING、epoch 1、wire 1、proof 关闭且没有已标记授权 epoch、sidecar 或 pending 记录时执行。它先独占控制行，再锁定 Outbox；检查失败必须保留全部历史。成功 down 必须验证普通旧写入仍可运行，且可重新执行 up。

`account_service` 仍是旧数据库所有者，仍可通过 public schema 所有权破坏性删除对象；新的表所有权隔离不等于消除了这项风险。此边界及原有身份事实/受控锁函数的所有者信任仍属于已记录的部署收敛债务，不能以本机权限测试代替 Linux 部署验收。
