# Windows CI 的 I/O 与控制边界

浏览器测试宿主的目录扫描、请求读取和原子响应写入使用单条异步 I/O 序列。
目录 I/O 尚未返回时，Worker 仍能处理配置和只读快照；通知仅作提示，保留原有
100 毫秒轮询回退。成功的输入续期仍只在真实响应发布后计数，不以空转或控制回复代替。

关闭先停止新扫描、关闭目录监听器，再等待本次读写结束。超时配置的回复可能迟到，
客户端与宿主的版本因此可能暂不一致：后续业务配置和快照仍拒绝陈旧版本；拥有该私有
MessagePort 的父进程可停止轮询并关闭宿主，不让版本失配阻止资源退出。原始配置超时
仍是失败，不会因成功清理而变成通过。专用测试用共享屏障模拟未完成目录读取和迟到回复。

relay 的 Windows 祖先路径检查把每个 UTF-8 路径独立编码为一行 base64，使用 .NET
直接解码并读取 `FileAttributes.ReparsePoint`。它不把路径拼成命令，也不需要自动加载
PowerShell JSON/文件提供器模块。缺失路径、junction、悬空 junction 和重解析祖先仍拒绝；
ACL 验证、原生 Windows 执行及输出校验不变。专用原生用例同时关闭模块自动加载，验证
检查仍实际执行，而非只检查脚本文本。

没有增加重试或放宽期限：配置确认 5 秒、原生祖先校验 15 秒，以及浏览器实际输入续期的
250/400 毫秒约束均保持原值。底层存储或调度若长时间停顿，真实业务期限仍会失败。
上述修复解除不必要的同步和模块加载依赖，并不据此宣称现场底层延迟已经完全消除。

本地验证入口：

```powershell
node --test pinvou3-app/tests/browser-wrapper-host-control.test.mjs
node --test remote-control-relay/test/windows-ancestor-paths.test.js
python scripts/ci-fixture-process.py --receipt "$env:TEMP/browser-owned-exit.json" -- node --test --test-concurrency=1 pinvou3-app/tests/browser_wrapper_windows_lifecycle.test.mjs
```

后两项需要原生 Windows 才能证明实际 Windows 行为。CI 的 Windows 浏览器门禁包含
控制边界用例；原生 relay 门禁包含路径边界用例，失败或跳过均不能当作合格证据。
