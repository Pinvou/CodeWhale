# Community 回流处理优化

## 目标与基线

本方案只优化 Issue 被领取后的回流处理，不把 `backflow:pending` 等待领取时间计入处理耗时，
也不把一次性 Windows 文本编码事故计入可重复收益。

历史样本的处理阶段中位数约 51.6 分钟；排除明显依赖阻塞项后的平均值约 42.5 分钟。
主要耗时来自依赖等待与重复 rebase、本地 Rust 冷构建、CI 慢路径，而不是 Issue 收尾。

目标：

| 指标 | 目标 |
|---|---:|
| 准备阶段中位数 | 不超过 10 分钟 |
| 正常 PR 阶段中位数 | 不超过 15 分钟 |
| 最终 CI 中位数 | 7～9 分钟 |
| 无依赖回流总耗时 | 30～35 分钟 |
| 回流总耗时 P90 | 60～75 分钟 |

## 两维执行模型

回流阶段不能按 Ubuntu、Windows、macOS 分层。分层描述的是验证目的，平台描述的是执行位置：

1. 本地 L0/L1/L2 在实际创建或处理 PR 的平台执行。
2. GitHub CI 用 Windows、macOS、Linux 三平台轻门禁独立复核同一个 head。
3. 公共重型检查只运行一次；原生检查按实际改动路由到对应平台。
4. Merge Queue 验证最新 `main` 与 PR 的组合提交，主线日常前进不触发人工 rebase。

```text
Windows / macOS / Linux 处理节点
  -> 本地 preflight
  -> push + PR provenance
  -> 三平台 backflow-portability（并行）
  -> 按路径选择的产品门禁（并行）
  -> required-gate
  -> Merge Queue
  -> 自动收尾
```

## 跨平台 CLI

`scripts/community-backflow.mjs` 仅依赖 Node.js 22 和 Git：

```shell
# 从开放的 pending/processing Issue 聚合 community-backflow-plan marker
node scripts/community-backflow.mjs queue \
  --repo Pinvou/pinvou3 \
  --output queue.json

# 根据显式依赖、路径重叠和 Release 顺序生成可并行波次
node scripts/community-backflow.mjs plan --input queue.json --output plan.json

# 在实际处理平台运行；不安装依赖、不编译 Rust
node scripts/community-backflow.mjs preflight --base origin/main --source 244

# 推送固定分支、创建或更新 PR、写后回读，并评论关联 Issue
node scripts/community-backflow.mjs submit \
  --repo Pinvou/pinvou3 \
  --issue 602 \
  --source 244 \
  --title "chore(release): 回流版本升级至 0.8.0" \
  --body-file pr-body.md
```

`queue` 和 `submit` 优先使用 `GITHUB_TOKEN` 或 `GH_TOKEN`；使用 Git Credential Manager
时由操作者显式传入 `--credential <github-username>`，工具不内置个人账号。GitHub API 请求
使用 15 秒超时，并对网络错误、限流和临时 5xx 最多重试 3 次。请求正文始终以 UTF-8 字节发送，创建或更新后逐字回读
标题和正文。Token 不写入输出、文件或 PR。

PR 正文包含与当前 head 绑定的 provenance，记录实际处理 OS、CLI 版本、base/head SHA
和轻量检查。任何后续 push 都必须重新运行 `submit` 更新 provenance。

## 依赖调度

监控器在同一次扫描中提前读取所有新来源 PR 的文件列表并生成 DAG：

- 没有依赖且路径不重叠的回流位于同一波次，可以由不同平台并行处理。
- 修改相同路径的回流按来源合并时间串行，减少冲突和重复 rebase。
- Release 回流依赖同批次更早的非 Release 回流。
- Issue 中的 `community-backflow-plan` 是可恢复的远端 checkpoint；`queue` 命令从全部开放的
  `backflow:pending`/`backflow:processing` Issue 聚合 marker，换处理机器时无需重做扫描。

依赖等待应单独统计为 `blocked_dependency`，不能混入代码准备或 CI。只有依赖满足后才创建
最终 PR；可以提前分析和准备补丁，但依赖合入后只做一次最终整合。

## CI 路由

可信回流 PR 和纯回流 Merge Group 固定运行 `backflow-portability`：

- `ubuntu-latest`、`windows-latest`、`macos-15` 并行。
- 不运行 `npm ci`，不初始化 CodeWhale，不调用 Cargo。
- 检查调度算法、Git diff、跨平台路径和 PR provenance。
- 目标时长 2～4 分钟，超时上限 5 分钟。
- 修改回流基础设施自身（包括 `pr-check.yml`）的普通 PR 也会触发三平台轻门禁；这是为了让
  门禁定义在影响回流前先完成跨平台自验证，会增加少量并行 runner 计算分钟。

现有前端、Rust、Windows runtime 和 macOS runtime 任务继续按路径路由。三平台轻门禁与这些
任务并行，并由 `required-gate` 汇总，因此不会把三个操作系统串成三倍墙钟时间。

`scripts/ci-rust-policy.py` 在信任校验之后继续读取事件中的 base/head SHA 和 Git 实际
diff，输出以下保守路由：

- `release`：只允许 `VERSION` 与 `sync-version.mjs` 管理的全部派生字段机械同步；
  运行 `release-contract-test`，跳过由 Cargo/package 版本字段误触发的产品编译。
- `portable_rust`：不含 Windows/平台/路径/进程/安装/权限/原生依赖、Cargo、`build.rs`、
  toolchain 或 CodeWhale gitlink 风险的普通跨平台 Rust；保留 lint、portability、定向
  测试和最新 `main` 的累计 Windows 验证。
- `windows_full`：上述 Windows 高风险或 Release 内容异常、混合改动；Ready PR 继续运行
  完整 `windows-rust-test`。
- `non_rust`：继续使用既有受影响产品门禁。
- `ordinary`：来源 marker、head/base 仓库或 base=`main` 不满足可信合同，严格保持普通
  PR 的既有路径路由。
- `full`：API、Git、diff 或 Merge Group 成员链无法完整解析；即使 paths-filter 本来会
  跳过，也强制完整 Windows，并由 `required-gate` 拒绝 skipped。

Merge Group 通过队列快照中每项 `baseCommit→headCommit` 链从事件 `head_sha` 反向走到
`base_sha`，并逐一校验链上所有 PR；不能只用累计 head 匹配最后一个 PR。外部 fork、
伪造/重复 marker 和 `ci:full-rust` 回到普通 PR 路由；无法读取 base/head、API/diff 或
成员链时走 `full`。`required-gate` 只把 release/portable/non-rust 策略导致的 skipped
视为预期结果，不接受要求完整 Windows 时的 skipped、失败、取消或未完成状态。

## Rust 与缓存

- 本地只运行受影响 package 的快速检查，不对同一 head 重复全仓冷构建。
- 完整 Rust 测试交给有缓存的 CI；可信回流仍在 PR 阶段跳过完整 Linux `rust-test`；
  Windows 高风险回流在 Ready PR 保留完整 Windows 回归，其余由最新 `main` 做累计验证。
- npm/Cargo 缓存必须包含 OS、架构、工具链和 lockfile；不得跨平台复用编译产物。
- 同一 PR 的旧 SHA 由 `concurrency.cancel-in-progress` 取消。

## 计时字段

自动化记录以下互斥阶段，避免把重叠 CI 重复计入总耗时：

| 字段 | 起止事件 |
|---|---|
| `prepare_active` | 依赖满足到首次 push |
| `blocked_dependency` | 已领取但等待前置回流 |
| `ci_critical` | 当前 head push 到 required checks 完成 |
| `merge_queue` | 入队到 merge |
| `cleanup` | merge 到 Issue synced/closed |

优化看墙钟时间、P50、P90 和失败重试次数；runner 计算分钟单独统计。三平台轻门禁会增加少量
计算分钟，但不应显著增加关键路径。
