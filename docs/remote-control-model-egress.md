# 临时模型的推理出口

临时模型在准备远程轮次时解析主模型和可选视觉路线，仅接纳 HTTPS 和全部为公网的
地址集合。冻结路线、凭据及地址快照一同交给 EnginePool。地址快照只在当前轮次使用。
每个远程轮次拥有独立的引擎所有者。新轮次重新准备地址并创建引擎，轮次结束时回收
该所有者的引擎。底座会将脱敏后的端点地址写入诊断日志，日志仍可能包含主机、路径
和部分查询参数。请通过专用凭据字段传递密钥，避免将其放入端点 URL。

每个 HTTPS 源（主机和有效端口）最多保留 32 个解析地址。主模型和视觉路线同源时
共用这份快照，路线、模型和凭据各自保留。域名解析共享四个阻塞工作槽。占满时，任务准备立即以
`model_endpoint_unresolved` 失败。槽释放后，控制端可使用新的 `task_id` 和 `client_request_id` 提交新任务。
任务记录仍在保留期、当前授权有效且任务指纹匹配时，用相同的 `client_request_id` 重放会
读取已有任务。相同 `client_request_id` 的任务指纹有变化时返回冲突。`request_id` 只标识传输请求。
中继仍保留提交缓存时，变更模型包也会返回冲突。任务记录重放不会重新安装模型包。
更换模型包时使用新的 `task_id` 和 `client_request_id`。
取得槽后等待解析结果最多五秒。
超时或取消只结束等待，系统解析工作仍持有槽直到实际退出，因此不会通过重复超时
无限创建解析工作。若系统解析长期不返回，可用槽会减少；不承诺取消操作系统调用。
解析结果超过 32 个地址也会返回 `model_endpoint_unresolved`。数字 IP 直接走相同公网
分类，不创建解析线程。

App 将 `RuntimeEgress` 安装到 CodeWhale 的宿主配置 `Config.model_http_policy`。
主客户端、HTTP/1 回退及独立 `image_analyze` 客户端通过该配置得到同一策略，
包括单独配置视觉模型的情况：

- 使用此次校验过的地址快照，保持 URL 原主机名、端口、TLS 主机名及证书校验。
- 不向系统解析器回退到其他域名，不接受未准备的路线构造。
- 禁用环境/系统代理及自动重定向，限制为 HTTPS。

该 hook 不从 TOML/JSON 反序列化。普通本地模型不安装此远控策略，独立视觉
遵循 CodeWhale 的共享传输配置，包括 HTTP/1、TLS 信任及非回环 HTTP 设置。
被控桌面的启动环境若带 CLI 凭据覆盖标记，准备阶段会拒绝临时模型。该判断以标记
为准，未提供密钥时也会触发。临时主模型不接受凭据存储占位符。普通模型继续使用
自己的凭据配置。
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
用例校验 TLS SNI 和 HTTP Host，并验证未信任测试 CA 或主机名与证书不符时连接失败。窄 harness 可验证同一生产模块的
传输行为，但不能代替完整 App/底座装配，报告须分别记录执行目标与有效配置。

通用 hook 已通过 [CodeWhale #151](https://github.com/Pinvou/CodeWhale/pull/151)
进入公开维护分支。公开维护头与固定标签仍由父仓门禁校验。
