# 模型卸载与恢复维护事务（v1）

本机 Unix socket 维护协议；不包含平台授权和远程写入。
本接口供 Pinvou 模型商店后台持有，跨越 cmsd 停服务、商店卸载脚本、cmsd 注销与清理。
不依赖聊天模型在执行期间在线，LLM 不持有维护凭据。

## 请求与权限

`POST /v1/models/{card}/maintenance`：

```json
{"action":"begin","owner":"client-generated-private-transaction-id"}
```

- action 为 `begin`、`end` 或 `failed`。owner 为客户端随机生成并先私有落盘的1至128位
  ASCII 字母、数字、连字符或下划线，不能作为普通工具参数或日志输出。
- 明确托管模型取得普通维护门。标准声明卡还可取得 `reservation_only` 清理预留：先用
  新鲜事实核实运行时不存在或已停止，且无独立自启；未知或活动的外部资源拒绝预留。
  已成功下载但尚未部署（含部署失败或保留权重卸载）的权威 current 也可取得清理预留；须有一致的当前版本证据，历史、删除中或绑定未知继续拒绝。已拥有生命周期权限的条目在这个未部署状态也只走 reservation，不授予新的启停权；如实际仍活跃，先经原有生命周期归属核查安全停止，再重试预留。
  预留不授予停止、默认切换或重启权。任意实验产物登记不能据此取得生命周期权限。
  同卡任务、其他设备生命周期任务、组件维护与模型维护互斥，共用状态锁原子准入。
- 已存在同 owner、同系统 UID 的 begin 幂等；即使原停止任务在跑、模型已注销也返回原门。
  系统 UID、PID 取自 Unix `SO_PEERCRED`，请求不能自报身份。
- 门持久化到私有 `model-maintenance/{card}.json`，文件0600、目录0700。
  fsync、原子改名与目录同步后才确认。失联、超时、PID退出、开机不会自动释放。

门存在时，启用、安装、设默认与切换的两侧均拒绝竞争。只有同 UID 和私有 owner 可调用：

```http
POST /v1/models/{card}/disable
Content-Type: application/json

{"maintenance_owner":"client-generated-private-transaction-id"}
```

`purge` 使用同样正文。**携带私有owner的purge只撤管理登记**，原生和容器均不删除任何
权重或运行时，文件保留/删除由商店按用户选择执行。预留模式的disable仅核实已停止或不存在，
不会执行外部启停。原 v1 无owner调用保持兼容：普通原生purge仍有删除权重语义，不能用于
商店“保留权重卸载”。所有普通调用不能越过维护门。
每项动作仍返回既有 `task_id`，客户端查询终态；接单不代表卸载完成。

## 断线与恢复

`GET /v1/models/{card}/maintenance` 在模型注销后仍可读，返回：

```json
{
  "card":"example", "maintenance":true, "model_present":false,
  "phase":"recovery_required", "holder_alive":false, "recovery_required":true, "reservation_only":false,
  "last_task_id":"t-example", "last_action":"purge", "last_task_status":"succeeded"
}
```

不返回 owner。`last_task_*` 独立持久化，历史任务裁剪后仍有最近回执。
任务落盘失败不启动 worker，并撤掉内存中的未启动 running 任务；失败回执保留门。
daemon 重启后收敛孤儿回执，不凭一次 POST 失联自动重发破坏性脚本。

`failed` 显式进入恢复期。`end` 拒绝尚未终结的同卡任务，并按门中持久化的运行时引用重新
查询Docker/systemd，核实已停止或不存在及无独立自启；卡片删除后仍能核查。查询失败继续保护。
后端查询在全局Store锁外执行，避免慢Docker/systemd拖住状态读取或正在运行的推理。
查询前保存持久生命周期版本、模型运行关联摘要和完整私有维护记录；回到锁内原子核对后
才准入动作/发布门/结束门，期间任何变化返回409 `precondition_changed`，需要重新核实。
新门还带独立随机代际，旧记录继续兼容。仅把查询移出锁而不重验前置条件是不安全的。
共享状态尚未成功持久化时也拒绝解门，不能将内存中的已完成误当作落盘成功。
原生停止证明还检查同账号进程。普通桌面进程可能因 ptrace 规则禁止读取 exe；仅此权限错误可使用
有界、完整的 argv 与前后相同的 PID/启动时钟/进程名排除明确无关者，允许正常 R/S 调度变化。
任何原生执行名、身份/档位旗标或目标模型/权重线索仍保持未知保护；无法完整读取身份同样拒绝清理，
不按 exe 读取失败直接跳过进程。该排除不授予生命周期控制权，原有单元、自启和监听核查继续执行。
PID 判断组合 boot ID 与 `/proc/<pid>/stat` 启动时钟，PID复用不能冒充原持有者。

商店还须保存自己的卸载阶段、脚本进程证据和执行围栏，确认脚本已经终结才结束事务。
cmsd 无法仅从其任务成功推断商店文件操作已经结束；自动重跑卸载脚本不能作为恢复手段。
`model.maintenance.reservation` 声明预留和owner-purge只撤登记的新合同；只有旧
`model.maintenance` 时不能猜测原生保留权重安全。客户端缺所需能力应明确升级。
`/v1/models` 的 `managed` 仅表示实际生命周期管理权；`can_adopt` 表示已部署声明卡
可以经显式enable接管，`can_reserve_uninstall` 表示可申请清理预留，三者不能互相替代。

## 验证边界

单元测试覆盖并发 owner、组件互斥、切换两侧、UID 绑定、损坏记录、持久失败和历史裁剪。
真实后端延迟和并发压力需在独立实例另行核查，不以单元测试替代性能验收。
Pinvou 的 UI 和后台商店事务需要在对应仓库另行联调；单元测试不构成模型推理或整机稳定性验收。
