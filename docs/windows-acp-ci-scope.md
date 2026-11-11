# Windows ACP CI smoke 的私有工作区

Windows ACP smoke 原来直接在 checkout 的 `src-tauri/target/windows-runtime` 准备 Bridge。
自托管 runner 会保留该构建缓存；CI 已出现删除旧目标后替换目录返回 `EPERM` 的失败。
仅凭该错误无法确定具体占用者，重跑或清扫整个缓存也不能证明资源归属。

`windows-codex-runtime-test` 现在先进入 `ci-fixture-process.py` 的私有目录及 Windows Job，
再由 `test-windows-acp-runtime.py` 从固定 Git tree 提取 smoke 所需源码、配置和公开资源。
私有 app 中执行原有 npm smoke；使用相同锁文件及真实 ACP 握手，不复用或修改 checkout
中的 runtime/cache，也不依赖私有 Windows runtime。npm 仍可使用现有下载缓存并按锁校验依赖。

整个 npm、Node 及其后代都受原有进程监督边界约束。成功、失败和取消均由拥有者确认后代退出后
清理私有目录；未确认会使门禁失败。新增专属退出收据独立上传，上传超时仍是辅助诊断失败，
不覆盖 smoke 或资源收尾的原始结果。没有增加业务重试、放宽握手断言或扩大清理对象。

Node/npm 的合法深层依赖路径可能超过 Windows MAX_PATH。监督器仍先核对原有目录身份并确认
后代退出，仅把同一个待清理目录转换为 Windows 扩展长度路径后删除；驱动器、UNC 和已扩展
路径保持同一对象语义。身份不符或删除失败仍失败，不增加强删、重试或共享目录清扫。

本地原生 Windows 可用以下入口（已有 Node/npm/Git，安装仅使用现有公开 ACP 锁文件）：

```powershell
python scripts/ci-fixture-process.py --receipt "$env:TEMP/pinvou-acp-exit.json" -- python scripts/test-windows-acp-runtime.py
```

每次验证使用新的收据名；`--source-ref <40位固定tree>` 用于提交前的固定树验证。
这项 smoke 证明现有 Node 下的社区版 Bridge 及资源映射，不替代私有 runtime staging、完整安装包
或现场模型调用验收。生产构建的缓存准备逻辑未在本修复中改变。
