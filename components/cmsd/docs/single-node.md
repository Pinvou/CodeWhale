# 单机部署与恢复

设计理由、职责和已知限制见[单机领域决策](single-node-decisions.md)；具体 API 以各主题合同为准。

Cube Manage Software（后台守护进程 `cmsd`）提供本机 Unix socket `/v1` 管理接口及回环地址推理网关。设备 ID 仅标识本机状态，不授予远程管理权。

## 用户级安装

构建使用 `CARGO_BUILD_JOBS=1 cargo build`。将二进制安装至 `/usr/local/bin/cmsd`，或修改
`deploy/cmsd-user.service` 的 `ExecStart` 为实际绝对路径。复制单元前先检查
`systemctl --user status cmsd`，已有服务应先核对配置、任务与升级窗口，不能覆盖开发实例。

新安装先将 `deploy/cmsd.toml.example` 保存为 `~/.config/megacube/cmsd.toml`（目录0700、文件0600），按需补充本用户资源路径。它显式启用 state 模型单元隔离；已有安装不要直接覆盖配置或切换命名空间，见[原生服务实例隔离](native-unit-isolation-v1.md)。

将单元放入 `~/.config/systemd/user/cmsd.service`，然后：

```bash
systemctl --user daemon-reload
systemctl --user enable --now cmsd
systemctl --user status cmsd
journalctl --user -u cmsd -n 60
curl --unix-socket "$XDG_RUNTIME_DIR/megacube/cmsd.sock" http://localhost/v1/device
```

非 root 默认配置 `~/.config/megacube/cmsd.toml`，状态 `$XDG_STATE_HOME/megacube`
（未设置则 `~/.local/state/megacube`），socket `$XDG_RUNTIME_DIR/megacube/cmsd.sock`。
没有运行时目录时 socket 回落到用户状态目录。卡片、权重与商店 socket 均跟随
`PINVOU3_HOME`，默认 `~/.pinvou3`。自定义安装时必须给 cmsd 与 Pinvou 相同的
`PINVOU3_HOME`、`CMSD_SOCKET`；配置文件支持 `store_socket_path` 等覆盖，参见 config.rs。
用户级单元的运行目录 0700、UMask 0077。需要退出桌面会话后继续开机运行时，按设备策略为
服务用户启用 linger；关闭 Pinvou 窗口本身不要求 linger。

### 原有 system 模式的边界

本批 Pinvou 商店闭环要求后台与桌面使用同一个非 root 账号，按上面的用户级方式部署。
原有 system 模式继续用于独立模型管理；root daemon 与桌面用户商店的跨账号接入尚未支持，
不能将它配置成此次商店闭环，也不通过放宽数据库、卡片或锁的账号校验来接入。

原有系统级安装采用 `deploy/cmsd.service`，创建 `megacube` 组并授权实际桌面用户。
`/etc/megacube/cmsd.env` 必须显式提供 `CMSD_RUN_USER`、`PINVOU3_HOME`、
`CMSD_ENGINE_ROOT` 及需要的资源路径；新安装设置 `CMSD_CONFIG=/etc/megacube/cmsd.toml` 并在该配置中写入 `native_unit_namespace = "state"`；不能以 root 的 HOME 推断模型用户。
系统状态目录 0700，socket 目录 0770，socket 0660。Docker 组本身具备高权限；不要
通过把 socket 改为 0666 解决访问错误。模型服务 `User=` 使用配置的真实用户。

## Pinvou 管理 Cube Manage Software

当前入口为“侧栏设备管理（普通设备为运行状态） → MegaCube → Cube Manage Software”，提供当前用户服务的
安装、启动、停止、重启、离线更新、回退、卸载服务、恢复安装、维护恢复与日志。
未安装或失联时仍保留入口；系统服务/归属未知不会静默提权或覆盖。
普通维护先取得持久任务准入门，正在换胎或安装时拒绝；救援停止单独说明影响。
操作由同一 Pinvou 二进制的独立 helper 执行，UI 退出后继续。

完整 API、包格式与边界见 [本机组件维护](component-maintenance-v1.md)。这里的卸载保留
模型、权重、配置、设备状态、任务和回退缓存，不等于清空所有文件。用户服务不自动修改 linger。

## 后台商店与关闭桌面

Pinvou 在 Linux 桌面启动时使用同一个可执行文件的 `--model-store-host`
模式；该入口在 WebKit、窗口与单实例插件初始化前执行。UI 通过 Unix socket 使用同一
执行器和任务互斥。后台商店从启动起就独立于桌面和 cmsd 安装状态，首次安装 cmsd 后无需切换队列或重启桌面。

无人登录运行可安装 Pinvou 仓的 `packaging/linux/pinvou-model-store.service`，修改
`ExecStart` 并为两个服务配置一致的用户目录与 socket。不要同时启动不同版本执行器。
宿主锁防止多个执行器并发清理或执行；协议/版本冲突明确拒绝，等待已有任务结束后升级。

关闭 UI 不终止 host；cmsd 重启会读取任务及其 `store_run_id`，重新查询原 handle。
host 中断后先按 boot ID/PID 身份清理登记的孤儿脚本，再把持久未终结任务标记
`interrupted`。它不擅自重发可能已有副作用的部署；用户核实后重试，下载脚本继续已有断点。
取消保留已下载部分。连接失败不允许绕过 cmsd 管理权执行旧脚本。

## 状态与救援

管理UI打不开时，可用同一实例配置执行`CMSD_CONFIG=/absolute/cmsd.toml /absolute/cmsd --diagnose`。
它只读固定本机API，区分事实未核实和网关监听；未实现的可选能力明确显示 unsupported；不自动修复或执行旧脚本，
不将监听成功作为推理证明。退出码及旧服务兼容见[无LLM诊断](diagnostics-v1.md)。

- 同一状态目录或 socket 只能运行一个 cmsd。满队列、权限拒绝及未知探测结果均保留
  原端点并明确拒绝启动；状态锁与 socket 声明锁持续持有。详见[启动与socket](startup-and-socket-v1.md)。
- `/v1/device` 的 ID 保存在 `device-identity.json`；重启保持，重装状态目录会改变。
  `identity_scope=local-state-only`，不包含签名密钥或远程配对能力。复制安装模板时不复制已有设备状态和任务。
- `GET /v1/models` 是卡片管理事实，`GET /v1/engines` 还包含外部进程和历史容器。
  仅发现/外部登记不授予停止、默认切换、重启权限。
  已有容器/原生单元的只读登记持久保存 `lifecycle_owned=false`，`managed=false`；
  明确“接管并启用”后才取得本机管理权。升级旧 v1 状态仅在存在 `enabled_at` 证据时保留
  管理权；历史纯发现记录保持只读，需要显式接管，不能直接停止或设默认。
- `enable`、`disable`、`set-default`、`switch` 为串行生命周期变更，避免并发启动重复消费
  同一 MemAvailable。`set-default` 只移动网关指针；Pinvou 当前聊天连接由用户显式选择。
- 容器 `docker start` 不更新配置。镜像、已知端口、内存比例及已声明的 compose 上下文、
  服务名、并发、KV 类型不一致会报 `configuration_changed`，不会删除旧容器或假报新配置生效。
  启用、恢复策略迁移、设置默认和网关路由共用当前部署证明，并核对同一次 inspect 的权重挂载 source；
  source 的容器内目标目录可由可信配方决定。桥接运行实例必须有同次 inspect 的 NetworkSettings
  TCP binding 证明声明的宿主端口可从 `127.0.0.1` 到达；停止实例的启用预检使用 HostConfig
  声明，允许多个端口/地址中的正确匹配。Compose host 配方还核对 NetworkMode=host 及
  声明的 `--host`/`--port`；专属非回环地址和仅 IPv6 binding 不授予 IPv4 回环路由权。
  未核实的运行事实仍可展示。等价部署重建可继续管理，不绑定永久容器 ID。
  停用/注销仍按已保存的托管 name/image 核实，卡片删除不妨碍停止旧实例；同名外来镜像不会被停用或改策略。
  未建模的 compose/脚本参数仍需人工核对；本检查不构成任意脚本的完整配置证明。
- 已知商店卡的容器首建脚本沿用当前 selection 固定的 `models_root`，不采用后来变化的默认模型目录。根目录未知/变化会在脚本前拒绝；按[固定目录恢复说明](latest-installed-model-v1.md#首建脚本的固定下载根目录)恢复匹配记录或原目录，保留现有权重，不自动下载。已有容器和原生服务不因此重启。
- 需要救援时可直接查询 socket 和日志；模型商店与设备设置页提供不依赖 LLM 的启停、
  默认选择和任务取消。管理面离线时保留恢复入口，不把失败查询显示为“没有模型”。

停止 cmsd 只停止管理与该网关，不停止现有模型；原始模型端口仍可用。恢复原二进制与配置后
`systemctl --user restart cmsd`，观察启动对账。升级与回退前保留状态目录和配置副本，勿在
安装进行时删除任务文件、权重或更换 PINVOU3_HOME。

## 当前验收边界

本机多进程、真实 CPU 模型下载/推理及已有 GPU 模型调用与物理多机分开记录。
`scripts/acceptance/cpu_model_server.py` 是可选真实 transformers CPU 服务，需实际权重及
包含 torch/transformers 的运行环境；不是 GPU 引擎性能验收，也不是模拟推理。

明确托管对象使用 cmsd 持久恢复意图，启动与崩溃恢复复用同一预算和排他；后端迁移
查询失败不会假报迁移成功。外部对象保留自己的 Docker/systemd 策略，不因发现被接管。
详见[统一预算恢复](recovery-and-robustness-v1.md)。物理多机、满上下文和长期稳定性需要单独验收。

本单机组件不读取远程身份、证书或 Plan 台账。发现 `identity-v2`、`remote-v2`、`control-v2`
或 `control-v2.initialized.json` 时保留原件并拒绝启动；使用支持这些授权格式的完整组件，
不删除授权或回填旧状态。
