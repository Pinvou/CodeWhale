# Engine 冷启动观测

Pinvou 在首次发送前需要准备运行时模型、物化 Skills、创建 Engine，并加载和同步会话。不要仅凭一次体感延迟加入预热；先用本地 timing sidecar 收集冷、热样本，再判断瓶颈和优化方向。

## 收集样本

观测随正常对话自动写入 `~/.pinvou3/sessions/<session-id>/timing_events.jsonl`，不上传数据。记录只包含阶段耗时和 `cold`、`rebuilt`、`reused` 类型，不包含消息、模型、路径、凭据或原始错误。

- `cold`：该会话没有可用 Engine，需要完整创建。
- `rebuilt`：已有 Engine 因模型或 MCP 配置变化而重建。
- `reused`：复用现有 Engine。

新会话、应用重启后的首次发送和定时任务会自然产生 cold 样本；同一会话的连续发送产生 reused 样本。空闲 Engine 在 30 分钟后可能被回收，下一次发送也会产生 cold 样本。无需为了凑样本连续重启应用。

旧版本 sidecar 没有 `engine_startup` 字段，报告会忽略这些记录。升级到包含本观测的版本后，应至少收集 30 个 cold 和 30 个 reused 样本：

```bash
cd pinvou3-app
npm run perf:engine-startup
```

可用 `--input <sessions目录或单个 timing_events.jsonl>` 分析指定数据，`--json` 输出机器可读结果。`--min-samples`、`--cold-p95-ms` 和 `--min-improvement-percent` 可调整实验门槛；非法或缺失的数值会直接报错，不会生成结论。

## 判读规则

默认同时满足以下条件才确认冷启动值得进入预热实验：

1. cold 与 reused 各至少 30 个样本；
2. cold Engine 获取 P95 超过 300 ms；
3. cold 与 reused P95 的差距至少为 cold P95 的 20%。

300 ms 是本轮实验采用的内部 Engine-ready 决策预算，不是历史基线或用户可感知延迟结论。第三项用 reused 近似预热能够达到的理论上限：如果 reused 同样慢，预热不能解决主要瓶颈，应优先处理报告中占比最高的模型准备、Skills 物化、Engine 创建或历史同步阶段。

报告中的“发送到首输出”只统计首个非空 `MessageDelta`，不把 `ThinkingDelta` 算作可见正文。Engine 获取失败没有完整阶段数据，不进入冷、热分布；失败本身仍由既有 turn 终态记录。

收集生产数据后，先保留同一设备和应用版本的优化前报告。实际优化完成后使用相同平台、样本量和命令重新采样，比较 cold/reused P50、P95、发送到 Engine 就绪和发送到首输出，不使用单次最好值作为结论。
