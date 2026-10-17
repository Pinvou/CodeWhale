# Enrollment migration 容量观测与历史保留

本地管理员可在运行 Gateway 的同一受信环境下执行：

```sh
node remote-control-relay/scripts/enrollment-migrate.js --capacity
```

此模式使用现有 loopback HMAC control 配置，向实时持锁 writer 的独立 `POST /capacity` 请求聚合观测，不打开、初始化或恢复磁盘状态。未配置 control、错误授权、失效 writer 或不可验证响应均失败关闭。凭证只从现有环境配置读取，不作为 CLI 参数或输出。原 `/coverage` v1 请求及响应不新增字段。

响应具有独立 v1 closed schema 和 HMAC domain，绑定 nonce、store binding、gateway instance、检查时间，以及相同的 store revision。`storage.serialized_bytes` 是当前共享状态的真实持久编码字节数（包括末尾换行），`max_bytes` 是该 writer 的配置上限；默认共享预算仍为 16 MiB，适用于所有 namespace。`ledger` 仅提供总行数、pending / succeeded / failed 行数、原固定 100000 行上限和 revision。它不公开 request ID、endpoint、路径、digest、凭证或原始请求参数。

观测是当前快照，不是下一次 CAS 的空间预留。其他 namespace 和并发写仍会消耗预算，达到既有行数或共享字节上限时拒绝新增请求。既有 ID 仍先按原参数摘要和状态验证：pending 可恢复，terminal 返回既有结果，参数冲突仍拒绝。观测不推进命令、不改 revision、不重新派发旧代际任务。

当前安全合同保留所有历史 request ID 与终态证据；restore/import 不能擦除该账本。没有自动 GC、保留期限、额外硬上限或固定百分比告警。操作方可依据这些数值接入现有监控，容量逼近时应保留证据并规划已有配置支持的扩容，不能删除 terminal 行来复用旧 ID。

本点仅交付实时容量观测。运营告警阈值、长期归档或压缩 tombstone 需要单独设计及验收，必须继续保留防复用、参数冲突和终态幂等事实；没有这些证明不能清理历史。本地测试使用生产 StateStore / Registry / control 与 CLI，Windows 测试安全适配口不代表真实磁盘 DACL 或部署验收。
