# 失败诊断文本脱敏对照

离线实验，不会上传用户日志，也不会替换应用或 Relay 的脱敏规则。需要 Node.js、Rust/Cargo 及本机已缓存的应用依赖 serde_json、sha2；小型验证程序离线编译真实生产模块，不编译 Tauri。故障 HTTP 服务只监听本机回环地址，其他行为为本机子进程、读取一个不存在的临时文件，或明确标记的注入 fixture。

在 `remote-control-relay/` 运行：

```powershell
npm run eval:diagnostic-redaction
```

输出路径见命令结果和 `../.cache/failure-redaction-comparison/latest.json`。每次独立生成运行目录。`manifest.json` 是答案及来源，不能交给盲查者。

首次盲查：将三个 `set-*.json` 分别交给三个隔离上下文的审核者，只允许各自读取该文件，隐藏分组、源码、其他样本组及标准答案。按文件内 codebook 对每个 packet 输出 `packet_id`、`root_cause`、`confidence`（confirmed/likely/unknown）、1–3 条原文非敏感 `evidence`、`next_step`，保存为对应的 `answers-*.json` 数组。不能因结果不理想而更改规则、引用或答案。随后执行：

```powershell
node eval/failure-redaction/score.mjs <运行目录>
```

评分检查分类、原文引用、最低因果证据及禁止引用模拟私密值；同时计算预设证据、重复关联 ID 和原文实际存在的敏感标记。标记存在重叠，不能把标记数当独立秘密数量。

如需重放自行保留的运行结果，先生成新运行目录，再运行：

```powershell
node eval/failure-redaction/replay.mjs <新运行目录> <自行保留的冻结结果目录>
```

重放先核验全部 packet 和冻结答案的 SHA256，任一 packet 改变就拒绝复用旧答案。重放不是新增独立盲查。需要验证新脱敏规则时，应重新设计未见过的样本，并重新盲查。

2026-10-20 实验生成的运行包、独立答案、评分文件、依赖锁、日志及评估报告已清理，仓库保留生成器和回归测试代码。需要重新验证时，请生成新样本并重新独立盲查。不同来源版本的 packet 可能不同，禁止覆盖旧结果来伪装相同输入。验证程序仅在模拟实验中固定客户端与服务端匿名 scope，以便重放；生产应用与服务端均使用私有随机值。

新格式与安全边界的常规 JS 回归在 `test/diagnostic-redaction.test.js`；跨语言对照需显式启用：

```powershell
$env:PINVOU_TEST_NATIVE_REDACTION='1'
node --test test/diagnostic-redaction.test.js
```

常规 Relay 测试不要求安装 Rust；Ready PR 和 Merge Queue 的 `rust-test` 作业显式启用上述跨语言用例，两个生产实现、测试语料或助手锁变化都会触发该作业。助手使用 `native-runner.Cargo.lock` 和 `--offline --locked`，锁定的依赖来自现有应用依赖图；修改助手依赖时须同步复核锁及对应 CI 路由。Windows 本地执行还需要 MSVC C++ 链接工具，不把缺工具的失败或未启用的跳过算作跨语言验证通过。Rust 模块另有随应用运行的单元测试和预览集成测试。

C 是为本实验设计的字段级原型，仅用于说明可保留哪些信息；本实验不构成其通用隐私安全或未知样本泛化验证。
