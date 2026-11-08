# 临时模型的推理出口

临时模型在每次远程 turn 准备时解析主模型和可选视觉路线，仅接纳 HTTPS 和全部为
公网的地址集合。准备结果保留在该 turn 的内存所有者中，与冻结路线、凭据一同交给
EnginePool；不写入 SavedModel、设置、日志或模型提示词。

每个路线最多保留 32 个解析地址。域名解析共享四个阻塞工作槽，等待最多五秒。
超时或取消只结束等待，系统解析工作仍持有槽直到实际退出，因此不会通过重复超时
无限创建解析工作。若系统解析长期不返回，可用槽会减少；不承诺取消操作系统调用。
准入或结果容量不足沿用现有 `model_endpoint_unresolved` 关闭拒绝语义，后续调用
在真实工作退出后恢复。数字 IP 直接走相同公网分类，不创建解析线程。

App 的 `RuntimeEgress` 通过 Codewhale 的内存专用 `ModelHttpPolicy` 作用于实际
HTTP builder。主客户端、HTTP/1 回退和独立 image_analyze 客户端都继承该政策：

- 使用此次校验过的地址快照，保持 URL 原主机名、端口、TLS 主机名及证书校验。
- 不向系统解析器回退到其他域名，不接受未准备的路线构造。
- 禁用环境/系统代理及自动重定向，限制为 HTTPS。

该 hook 不从 TOML/JSON 反序列化。普通本地模型没有该 hook，保留原有网络配置。
Codewhale 继续拥有模型调用和工具循环；App 不添加替代推理客户端或 HTTP 转发代理。
此约束针对模型客户端，不把普通工具的联网能力声明为临时模型网络政策的一部分。

## 验证边界

默认单元测试核对解析超时/取消后的真实工作槽生命周期、固定解析器及不安全/未准备
路线拒绝。底座结果测试通过主客户端、HTTP/1 客户端和独立视觉工具的真实 HTTP
请求验证 builder hook 生效与拒绝传播；它使用正常允许的回环 HTTP 端点。

真实 TLS/代理/重定向测试需要独立自有监听，因此默认标为 ignored。运行示例：

```powershell
python scripts/ci-fixture-process.py --receipt <唯一回执路径> -- node pinvou3-app/tests/helpers/model-egress-transport-fixture.mjs -- cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --no-default-features --lib isolated_tls_snapshot_preserves_hostname_and_blocks_proxy_and_redirect -- --ignored --test-threads=1
```

驱动器生成临时 CA/服务器证书、两个模型监听、代理与重定向陷阱；仅向子进程设置
代理环境，退出后确认监听关闭并删除测试密钥。TLS 用例专门构造回环快照以到达自有
监听，这不代表生产入口允许私网；生产分类与混合地址拒绝另由真实分类器测试覆盖。
证书主机名校验保持启用，不能使用跳过验证替代。窄 harness 可验证同一生产模块的
传输行为，但不能代替完整 App/底座装配，报告须分别记录执行目标与有效配置。

底座通用 hook 同时准备了最新上游 main 上的独立候选。公开维护分支与固定标签的
门禁保持原样；候选代码/本地验证不代表该依赖已经公开合入或具备发布资格。
