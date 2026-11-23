# 配置检查与错误隔离

cmsd 使用默认值、配置文件、环境变量的顺序加载；环境变量最后覆盖。
默认配置不存在时可以使用默认值。显式设置的 `CMSD_CONFIG` 必须存在且可读，
不能为空；默认位置的已有文件不可读或为损坏链接也报错，不静默退回另一状态目录。
`--version` 和 `--component-info` 仍在读取配置前退出，离线制品检查不打开服务状态。

## 不启动服务的检查

```bash
CMSD_CONFIG=/absolute/private/cmsd.toml /absolute/cmsd --check-config
```

成功返回JSON与退出码0，错误为退出码2。检查使用最终环境覆盖后的路径、模式和端口，
不创建状态、锁、密钥、监听或模型任务。`runtime_access_verified=false`：不证明
目录可写、端口空闲、systemd/Docker可达或服务健康，需要实际启动后验活。
输出只包含主要资源路径、模式和不提供远程配对的标记，不打印HF端点等配置原始值。
原有system模式要求显式 `CMSD_RUN_USER`，不提供本批新增的桌面用户商店跨账号接入。
本单机版本只接受本机管理接口与推理网关配置，不提供远程控制监听或信任配置。

文件必须是最大64KiB的UTF-8普通文件；允许指向普通配置文件的符号链接。
打开带NONBLOCK，FIFO不会阻塞服务。错误提示省略源值和解析器原文，不将凭据行写入日志。
错误在打开状态、绑定socket或生成systemd模板前报告。

## 支持的格式和键

使用标准TOML平面键；支持引号中的`#`、转义、单引号和行尾注释。
拒绝重复键、段表、未知键和错误类型，防止拼错`state_dir`后悄悄使用默认目录。
不再接受旧解析器静默忽略的错误行、任意段头、数值字符串或错误模式。
更新前可用候选制品运行只读检查，修正配置后再从可信组件入口更新。

| 类型 | 键与约束 |
|---|---|
| 绝对路径字符串 | socket_path、state_dir、cards_dir、engine_root、unit_dir、models_dir、staging_dir、proc_root、meminfo_path、store_socket_path；禁止NUL |
| Unix端点 | socket_path与store_socket_path各最多107字节，不能是相同路径 |
| 监听字符串 | gateway_listen为字面IP:port，IPv6带方括号；本单机版本不接受 control_listen |
| 功能要求字符串 | require_native_profile_budget、require_store_selection均仅v1；不支持的旧程序应拒绝，分别见原生档位预算与当前部署选择说明 |
| 模式字符串 | systemd_mode仅user/system；native_unit_namespace仅legacy/state，默认legacy，新安装使用state |
| 端口池整数 | pool_start、pool_end为1—65535，start不超过end |
| 就绪期限整数 | ready_timeout_secs为1—86400秒 |
| 商店轮询整数 | store_poll_ms为50—60000毫秒 |
| 测试整数 | debug_available_mib非负；仅明确的测试环境使用 |
| 其他字符串 | hf_endpoint、docker_cli、nvidia_smi_cli；不新增shell解释 |

既有CMSD_*路径/模式/监听/商店环境覆盖保持，值错误明确退出；完整映射见src/config.rs。
常规运行不要配置debug_available_mib。网关端口0仅适合独立测试自动分配，不作为可复现服务地址。
回退到旧制品时使用其支持的原始平面配置形式；旧解析器不正确支持全部标准TOML语法。
本批不更改设备、吊销、任务或操作状态格式，软件回退不应回滚这些权威状态。
