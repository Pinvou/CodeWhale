# 推理网关监听事实与冲突恢复

旧版网关绑定失败后线程直接退出，`/v1/status.gateway`仍返回配置地址。实际占用测试
复现：管理查询64次成功，但占用方退出6秒后网关仍无法连接。这不能表示推理可用。

保留旧`gateway`字符串兼容客户端，新增`gateway_status`（version=1）作为运行观测：

| 字段 | 含义 |
|---|---|
| phase / ready | starting、unavailable、listening；ready只代表此进程持有监听 |
| configured_listen / bound_listen | 配置与实际地址；未监听时后者为null，端口0测试可读取实际分配端口 |
| bind_attempts / changed_at | 本进程的绑定尝试次数与状态更新时间，不是持久生命周期版本 |
| error | 稳定错误码、retryable与提示；正常监听为null |
| retry_interval_ms / retry_after_ms | 重试间隔与读取瞬间的剩余毫秒；无自动重试时为null |
| upstream_health_verified | 固定false；此观测不执行模型健康/推理，也不代表默认模型已选择 |

地址占用、地址不可用、权限拒绝等错误保留本机管理和组件救援，每5秒重试**相同配置地址**。
不停止占用进程、不选择另一个端口、不改变模型或后台任务。占用解除后同一daemon自动恢复。
连续相同错误不逐次刷日志，状态仍保留最新尝试。accept非中断错误会退出该监听并有限重绑；
连接线程创建失败不会把监听worker变成不可见的panic退出。意外worker退出标记
gateway_worker_stopped，无自动恢复承诺，需从组件入口检查日志与重启服务。

监听成功不证明上游模型成功；无默认模型仍由实际网关返回结构化503 no_active_model。
停止/恢复cmsd本身仍不停止既有独立模型服务，程序化LLM换胎的持久执行决策不变。
运行状态不写入模型状态/授权台账，软件回退保留当前任务、身份、吊销与版本文件。

## 客户端接入建议

本批 `--diagnose` 读取 `gateway_status.ready`，报告 `gateway_listener_ready`
与需要检查的状态；完整监听地址与错误事实由 `/v1/status` 返回。
Pinvou 当前本机状态卡只展示原 `gateway` 配置字符串，尚未展示新增监听事实；
配置地址本身不证明网关已监听或模型健康。

客户端后续接入可分别展示配置地址、实际监听和恢复说明；旧版没有
`gateway_status` 或事实互相矛盾时应显示未核实。重试秒数是读取时的采样值，
不能作为实时倒计时；管理可达性、监听可用性和模型健康也应分别表达。
