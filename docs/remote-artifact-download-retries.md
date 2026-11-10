# 远控下载末块重试与聚合预算

B 的 `ArtifactDownloads::next_chunk_bound` 在摘要确认后释放共享 transfer 的
in-flight 和字节预算，但保留最后一个块及其完整回执。A 丢失该响应时，可用原
handle 和原末块索引读取同一结果；它不会重新读取可能变化的文件，也不会重新
创建 transfer。其他块索引、会话/任务/产物、完整租约、任务 admission 或摘要
变化均不能获得该回执，错误请求不消费合法重试。

回执从首次完成时起保留现有 `TransferLimits.ttl`（默认 10 分钟，可部署收窄），
读取不延长期限。显式 abort、任务/admission/连接清理和现有到期 worker 均会
清除对应回执。活动及已完成 handle 合计最多占用现有产物索引上限 256 个槽位；
begin 预留槽位，满时返回既有 `transfer_capacity`，不驱逐其他已接受回执。
末块 payload 最多 256 KiB，因此终态 payload 总计最多 64 MiB，另有有界身份
元数据；原 4 路共享 in-flight 和 64 MiB 连接预算保持。达到回执容量时，显式
撤销已完成下载或等待到期后可恢复 begin；该容量合同独立于活跃字节预算。

前端 `downloadRemoteArtifact` 在下一次请求、解码及聚合之前检查独立 4096 块
上限，同时保留 32 MiB 总字节上限。每个响应必须对应当前连续索引、正的有界
块长、精确累计长度及下一索引，完成标记必须与总长一致；错误响应不能进入
Blob 拼接。最终摘要失败也撤销同一个下载 handle，不返回成功 Blob。
`result_unknown` 仍保留不确定结果，不自动 abort 或重新 begin。

定向回归应通过仓库共享资源作用域运行：

```sh
node --test pinvou3-app/tests/remote_file_transfer.test.mjs pinvou3-app/tests/remote_download_budget.test.mjs
cargo test --locked --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib controller::transfer::download::tests:: -- --test-threads=1
```

真实 Chromium 回归在 `pinvou3-app` 下执行
`node tests/remote_download_budget_smoke.mjs`，使用独立 browser profile 与
loopback HTTP fixture，加载产品模块并使用原生 Blob/WebCrypto。它以受控 API
响应验证块数边界、畸形/重复/缺块、摘要失败及随后合法下载，并注册在 full 和
相关路径的浏览器 smoke 中；它不替代部署 Relay、双机传输或完整界面验收。
