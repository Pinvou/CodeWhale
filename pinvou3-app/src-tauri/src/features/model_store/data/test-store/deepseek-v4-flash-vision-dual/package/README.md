# MegaCube 双台直连 DeepSeek 部署包

版本：1.0.0-rc1，2026-09-09。状态：本地逻辑与打包检查版，**尚未在两台直连 MegaCube 上完成端到端验收**。原有双节点部署已实测，本包新增的网络初始化与管理控制器需要现场验证后才能标为正式客户版本。

## 1. 适用范围与拓扑

模型：`deepseek-ai/DeepSeek-V4-Flash-Vision-Exp`。两台均为 GB10、128GB 统一内存、Linux aarch64；两端本地均保存完整权重，推理 TP=2 / PP=1。约 168GB 是每台磁盘权重大小，不是每台 GPU 装载量。

```text
用户应用 ── Head:8888
               │
         MegaCube A（Head，Rank 0）
               ║ 一根经硬件厂商确认兼容的 QSFP 直连线
         MegaCube B（Worker，Rank 1，headless）
```

推理通信、SSH 控制、模型和镜像分发都走 QSFP 直连，不需要交换机。首次在线下载仅要求 Head 能访问 GHCR 和 Hugging Face，可使用已有 Wi-Fi/业务网络；Worker 无需互联网。主机的业务网口可以供客户访问 API，业务网络不参与 TP 通信。

一根物理线可能带起两条逻辑网卡。本包支持选 1 条或 2 条 rail：单 rail 便于最小验证，不保证用满线缆带宽；双 rail 需要两端接口对应，并有共同可用的本机 GID index。不要配置 Linux bond。本版 MTU 固定 1500，先保证兼容，后续 9000 与带宽调优应单独验收。

参考 NVIDIA [双机直连说明](https://build.nvidia.com/spark/connect-two-sparks/stacked-sparks)；该文档针对 DGX Spark，MegaCube 的线缆、固件、逻辑网口命名仍须以实际设备为准。

## 2. 出厂软件基线

交付机应预装并验证：

- Ubuntu 24.04 系列 / Python >=3.10 / Bash / NetworkManager（`nmcli` 管理所选网卡）。
- 厂商 GB10 NVIDIA 驱动、RDMA 驱动，`/dev/infiniband` 存在。
- Docker Engine、Docker Compose >=2.30、NVIDIA Container Toolkit，普通登录用户可执行 `docker info` 和 GPU 容器。
- 两台的 OpenSSH server 均启用；Head 可用 `ssh-copy-id`，两台有 `rsync`、`ip`、`ping`。
- 普通登录用户可 `sudo`，首次网络初始化会要求 sudo 密码；不要 `sudo bash megacube.sh`。
- 每台模型所在盘建议至少 250GiB 空闲；Docker 数据目录若在另一个分区，也应至少预留 60GiB。JIT 缓存和镜像实际大小会影响预算。
- 两台 GPU 应空闲，不与其他大模型并行运行。脚本不会停止客户的其他容器。

脚本检查上述关键条件，但不自动更换驱动、升级操作系统或安装 Docker。只交付两台裸操作系统、不预装 NVIDIA 软件栈，不能承诺“插线即部署”。出厂预装基线可让客户操作缩减为以下步骤。

保留系统防火墙策略。直连专用接口需允许对端 TCP 通信（SSH 22、分布式 master 25000、vLLM 动态通信端口）及 RoCEv2 UDP 4791；API 按需放行业务端口 8888。不要仅放行 25000 就认为分布式通信已完整开放。仅信任这两台设备的专用直连链路。

## 3. 首次使用：两台各初始化一次

把同一个压缩包放到两台机器。以两台均使用 `customer` 账号为例，在**每台**执行：

```bash
cd ~
tar -xzf megacube-deepseek-direct-1.0.0-rc1.tar.gz
cd ~/megacube-deepseek
sha256sum -c SHA256SUMS
```

先接好 QSFP 线。在 Worker 执行：

```bash
bash megacube.sh init --role worker
```

脚本会列出 ACTIVE RDMA 网卡，按提示填入直连网卡名称；双 rail 用逗号分隔。随后在 Head 执行：

```bash
bash megacube.sh init --role head \
  --worker-user customer --worker-dir /home/customer/megacube-deepseek
bash megacube.sh pair
```

`pair` 首次需要核对 Worker SSH 主机指纹并输入 Worker 登录密码，此后使用 SSH 密钥。网络配置和模型分发不需要共享交换机。脚本不会禁用 SSH 主机密钥校验，也不保存登录密码。

如果接口已经确定，可以免交互指定（以下名称仅为示例，必须与实际 ACTIVE 列表一致）：

```bash
# Worker
bash megacube.sh init --role worker --ifaces enp1s0f1np1,enP2p1s0f1np1
# Head
bash megacube.sh init --role head --ifaces enp1s0f1np1,enP2p1s0f1np1 \
  --worker-user customer --worker-dir /home/customer/megacube-deepseek
```

两端第一个接口对应 rail 0，第二个接口对应 rail 1。默认分别使用 `192.168.240.0/30`、`192.168.241.0/30`，Head 为 `.1`，Worker 为 `.2`；无网关、不改变默认路由。现有 IPv4 地址或路由冲突会拒绝初始化，不覆盖业务网络。换子网时两端都传相同的 `--subnets 192.168.242.0/30,192.168.243.0/30`。

默认 API 绑定 Head 的 `127.0.0.1:8888`。需要业务网络访问时，在 Head 的 `init` 增加 `--bind <Head业务IPv4>`，或在隔离可信网络使用 `--bind 0.0.0.0`。当前入口未增加用户鉴权；对外发布应使用客户已有鉴权网关。本版不包含 WebUI。

首次必须两台各初始化：Worker 尚无可用地址和 SSH 信任时，Head 无法仅靠物理接线远程配置它。若厂商在交付前完成两台初始化与配对，客户可以直接从下一步的一条部署命令开始。

## 4. 一条命令部署，以后只操作 Head

```bash
cd ~/megacube-deepseek
bash megacube.sh deploy
```

部署依次执行：双机系统/网络检查 → 固定镜像下载 → 镜像经直连导入 Worker → Head 下载固定 revision 权重 → 生成 SHA256 清单 → rsync 断点分发模型 → 两端完整校验 → 双 Rank 启动 → `/health` 和文本生成验收。

首次镜像/权重下载与校验需要较长时间，取决于外网和磁盘速度；服务加载默认等待上限为 3600 秒。下载失败可重新执行 `deploy`，Hugging Face 本地下载缓存与 rsync partial 文件用于继续传输。如果服务在运行，先 `stop`，脚本不会覆盖运行中的模型。

可选 `HF_TOKEN` 从 Head 的进程环境传入下载容器，不保存到配置文件、不复制给 Worker。能够查看 Docker 的管理员在下载时仍可读取容器环境。

```bash
bash megacube.sh start                  # 已部署后，一键启动两台
bash megacube.sh stop                   # 一键停止两台，保留容器和数据
bash megacube.sh status                 # 展示两台容器状态
bash megacube.sh logs                   # 每台最近 150 行日志
bash megacube.sh check                  # 重新检查系统、直连与 GID
bash megacube.sh verify                 # 两台重新做完整模型 SHA256 校验
bash megacube.sh smoke-test             # 文本 + 原生图片识别冒烟
bash megacube.sh uninstall              # 删除本项目两台服务容器，保留权重
bash megacube.sh uninstall --purge-data  # 同时永久删除本包两台专用数据目录内容
```

`status` 是容器运行状态，Worker 的容器健康状态不代表端到端推理通过；用 `smoke-test` 检查服务。`start` 会检测双机镜像 ID 与模型清单是否相同；启动失败打印日志并停止两端。启动期间如需中断，在启动终端 Ctrl+C；生命周期操作有锁，避免重复启动或部署相互覆盖。

`uninstall` 不卸载全局 Docker、NVIDIA 驱动、其他模型，也不执行 `docker system prune`。SSH 配对、直连网络、运行镜像与工具本身保留，以便再次部署。彻底回收本包数据用 `--purge-data`，此操作不可恢复。网络复原需先完成双机 uninstall，再在**每台本地控制台**执行 `bash megacube.sh network-reset`；它删除本工具创建的 NetworkManager 配置并尝试恢复此前活动连接，直连 SSH 会断开。不要通过直连 SSH 执行网络复原。

系统重启后网络配置自动恢复，推理服务不会自动启动，避免两 Rank 独立重启失配；重新从 Head 执行 `start`。

已成功部署的 Head 保留本包镜像和模型清单时，可以 `deploy --offline` 重新分发；首次全离线交付须由厂商另备经过校验的镜像、模型和清单，本源码压缩包不含约 168GB 的权重。`--offline` 不提供从空盘凭空恢复模型的能力。

## 5. 运行配方

| 项目 | 设置 |
|---|---|
| 运行实现 | 归档 DSpark/vLLM Compose + 完整原生 Vision patches |
| 原始镜像 | `ghcr.io/anemll/dspark-vllm-gx10:0.1.1`，digest 固定为 `a83948492cf13df455170fb42885f5ef4db54fefe0feff0f841ecbff464ac9d8` |
| 镜像本地分发名 | `megacube/deepseek-direct:0.1.1-a83948492cf1`，双端 image ID 核对 |
| 模型 revision | `86f746b36186f0e567729a5c06a8c918caba82a9` |
| TP / PP | 2 / 1 |
| 上下文 / 并发 | 262144 / 8 |
| 每批 token / GPU memory utilization | 16384 / 0.83 |
| KV / 投机解码 | `nvfp4_ds_mla` / DSpark MTP=6 |
| 前缀缓存 / 分块预填充 | 开启 / 开启 |
| NCCL | 强制 IB 后端，RoCE v2 GID 按每台 IPv4/接口解析，不回退 TCP |
| 容器网络 / IPC | host / host |
| 模型运行 | 本地模型目录，离线推理 |

`runtime/` 复用了项目归档文件；`node.json`、`runtime/node.env`、`data/` 是客户运行时产生的文件，均不在发布压缩包中。不要直接覆盖旧 `.env.dspark.example`：它的默认 1M 上下文、并发 6 等并非此前最终实测配置。

## 6. 现场验收与发布门槛

此包目前只通过本地模拟与静态检查，不能把它宣称为已经验证的客户正式版本。至少在两台目标 MegaCube 上记录以下结果：

1. 单线直连；两端 ACTIVE 网卡、HCA、GID、路由、MTU 和实际链路速率留档。
2. 固定版本镜像在 GB10 容器内识别到 CUDA capability `(12,1)`；镜像 digest 可拉取，两端 image ID 相同。
3. 首次下载和断点恢复；两端完整 SHA256 校验通过。
4. NCCL 日志确认所选 RoCE 设备及 NET/IB 路径；用 NVIDIA NCCL/perftest 测带宽并留档。本包 `check` 的 ping/GID 校验不能替代实际 RDMA 带宽测试。
5. `deploy`、`start`、`stop`、重启后 `start`、保留权重卸载、清除权重卸载各执行一次。
6. 文本与图片冒烟；至少 C1/C4/C8 以及 250K 长上下文回归，比较 TTFT、TPOT、成功率与资源占用。
7. Worker 离线、错误网卡、端口占用、损坏 shard、加载超时的失败路径；不得报告假成功。

原集群性能仅作为回归参考：16项正式矩阵成功率100%，最大输出吞吐102.19 tok/s，250004输入 token验证通过。直连单/双 rail 的实际性能需新测，不作同等性能承诺。

本包保留上游 MIT 许可证于 `runtime/LICENSE.upstream`；模型、镜像及其组件遵循各自许可。镜像和模型未包含在源码交付包内。
