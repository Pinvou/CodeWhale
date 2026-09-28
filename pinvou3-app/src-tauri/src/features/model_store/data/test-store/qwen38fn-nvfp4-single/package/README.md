# Qwen3.8-Flash-Next NVFP4 单台 MegaCube 部署包

版本：1.0.0-rc3，2026-09-15。修复rc1/rc2首次安装时镜像身份不匹配的问题，固定新的ARM64镜像清单；沿用rc2运行参数与9份补丁。统一提供引擎准备、下载、部署、启停、状态、日志、校验和保留权重卸载。

**交付状态：候选修复包。新镜像的仓库清单与配置摘要已在线核实，控制器已做本地测试；新镜像与补丁组合尚未完成GB10推理验收。** 历史单机性能数据不能作为新镜像验收结果。升级步骤见 UPGRADE.md，验证范围见 VALIDATION.md。

## 1. 设备前提

- 1台GB10 MegaCube，128GB统一内存，Linux aarch64，Python >=3.10、Bash。
- 厂商支持的NVIDIA驱动、Docker和NVIDIA Container Toolkit已安装；登录用户可以直接运行`docker info`和GPU容器。无需Compose、SSH或第二台设备。
- 使用普通登录账号运行，不要`sudo bash megacube.sh`。本包不更换驱动、升级系统或停止其他模型。
- 模型必须放在本地高速NVMe。权重约123.57GiB（132.7GB）；建议模型、镜像和缓存合计预留至少250GiB。Docker数据盘若独立，需另行确保至少60GiB空闲。脚本的模型盘检查不覆盖其他挂载盘。
- 启动前可用统一内存至少100GiB；请先停止其他重负载。本包不会清空系统page cache，也不会强制删除其他服务。
- 首次下载需访问Docker Hub及Hugging Face。首次模型下载时间由外网速度决定；模型启动历史约12~14分钟，默认最多等待3600秒。

## 2. 最简单用法

将压缩包复制到MegaCube，在登录账号的家目录解压：

```bash
cd ~
sha256sum -c MegaCube-Qwen3.8-Flash-Next-NVFP4-single-1.0.0-rc3.tar.gz.sha256
tar -xzf MegaCube-Qwen3.8-Flash-Next-NVFP4-single-1.0.0-rc3.tar.gz
cd megacube-qwen38fn-single
sha256sum -c SHA256SUMS
bash megacube.sh deploy
```

`deploy`自动创建默认配置，检查环境，准备固定摘要的镜像，下载并校验权重，挂载全部9份运行补丁，启动服务并执行真实生成请求。首次下载可中断后重试；Hugging Face保留下载元数据，并对摘要不符的文件重新下载一次。重复部署前需先停止已运行的服务，避免边读边改模型。

后续均在同一目录使用同一个脚本：

```bash
bash megacube.sh engine      # 只准备固定镜像并检查GPU/补丁目标路径，不下载权重或启动模型
bash megacube.sh download    # 只下载镜像和模型，完整校验，不启动推理
bash megacube.sh start       # 启动；已运行且配置一致时只检查就绪
bash megacube.sh stop        # 停止服务，保留容器、模型和缓存
bash megacube.sh status      # 容器状态、OOM记录、HTTP健康检查
bash megacube.sh logs        # 最近200行容器日志，不持续跟踪
bash megacube.sh smoke-test  # 模型列表核对 + 真实文本生成
bash megacube.sh verify      # 停止服务后，按官方摘要完整校验模型
bash megacube.sh uninstall   # 停止并删除本服务容器，保留权重
```

`uninstall`会先归档最近2000行日志。模型权重、编译缓存、镜像、配置、来源锁文件和日志全部保留，之后直接`start`即可重新创建服务；如果镜像被外部删除，再执行`deploy`恢复。**本包没有删除模型的命令，也不运行docker prune。**

服务使用独立容器名`megacube-qwen38fn-single`和安装目录标签。遇到其他程序/另一安装目录创建的同名容器时会拒绝操作。服务启动失败会归档日志并停止本次启动的容器，不会仅凭`docker create`成功就报告模型可用。

启动期间`status`和`logs`可以随时查看；部署与启停有互斥锁。如需中断启动，在启动终端Ctrl+C，脚本会停止本次启动的服务。系统重启后不自动启动推理，手动运行`start`。

## 3. 配置端口与目录

默认监听`127.0.0.1:8000`。默认无需手工填写；若要从其他电脑访问，先执行：

```bash
bash megacube.sh init
```

再编辑生成的`config.json`。示例（替换账号及本机地址）：

```json
{
  "bind": "192.168.1.10",
  "port": 8000,
  "model_dir": "/home/customer/megacube-qwen38fn-single/models/Qwen3.8-Flash-Next-NVFP4",
  "cache_dir": "/home/customer/megacube-qwen38fn-single/cache",
  "revision": "",
  "startup_timeout": 3600
}
```

`bind`必须是本机IPv4，或可信隔离网络下的`0.0.0.0`；当前接口未启用鉴权，需要对外提供服务时应接入客户已有鉴权网关。模型与缓存目录可改为本地NVMe绝对路径，不能重叠或使用符号链接。

停止服务后再改配置，随后`start`。仅改变端口不会重新下载模型。改变模型目录时，应将完整数据迁移过去，再`verify`或重新`download`，不要只移动部分分片。不要用旧集群IP或`/var/tmp`路径覆盖客户配置。

## 4. 版本与完整性

镜像使用固定的linux/arm64清单引用：

```text
vllm/vllm-openai@sha256:a551e05307cd2e0092139d84db32af9c97e67d2eeeff072d21e429131d8c23f0
```

2026-09-15直接核实Docker Hub：原nightly标签的索引摘要为`f5df5cc3…`，ARM64清单为上述`a551e053…`，配置摘要为`1891ada1…`。完整响应清单及配置摘要核对结果在IMAGE-PROVENANCE.json。历史实测容器的ImageManifestDescriptor确实记录`5cba4263…`，但本次在该仓库按旧摘要查询返回404；无法证明新旧镜像内容相同。因此rc3作为新镜像候选版本验收。

首次准备先检查本地固定引用，缺失时只按该摘要拉取。检查linux/arm64平台及RepoDigests中的完整仓库与清单摘要，接受Docker Hub标准仓库名前缀；单独匹配`.Id`或配置摘要不算通过。Docker传统存储与containerd存储的`.Id`含义可能不同，因此`.Id`只用于诊断记录，运行始终使用固定摘要引用。校验失败会显示期望值和实际值，不回退浮动标签。

镜像锁保存为`state/image.json`（schema 2）。已有rc1/rc2镜像锁时，先停止本服务，再运行`engine`：新镜像校验通过后备份旧锁并迁移。`start`不自动迁移旧锁。镜像缺失时运行`engine`，仍只获取同一个固定摘要。离线复用要求本地已保留该摘要引用与RepoDigests；仅有标签或裸image ID不够。

模型固定为`nvidia/Qwen3.8-Flash-Next-NVFP4`。本地归档没有足以确认9月8日实测权重完整commit SHA的来源锁文件，因此：

- `revision`为空时，首次下载解析官方仓库当时的commit SHA，立即写入`state/model-source.json`。这不表示已确认与历史实测权重逐字节相同。
- 后续下载沿用锁定SHA，不自动跟随main。若厂商已有验收通过的40位commit SHA，应在首次下载前填入`revision`，客户包即可统一该版本。
- LFS权重按官方SHA256验证，普通仓库文件按Git blob SHA1验证；检查config架构、权重索引、tokenizer和额外权重文件。
- 模型下载完成会保存验证记录。日常`start`检查来源清单、文件大小及修改时间，不重复读取133GB；需要完整字节校验执行`verify`。
- 镜像、模型版本锁和SHA256SUMS必须与交付资料一起留档。不能把“第一次解析到的版本”直接写成历史实测版本。

模型下载凭证可通过运行前设置的`HF_TOKEN`环境变量传入下载容器，不写入config或运行服务容器。拥有Docker管理权限的人在下载期间仍可查看下载容器环境。

## 5. 运行配方

| 项目 | 固定设置 |
|---|---|
| TP | 1 |
| PLE | staged，MMAP启用，64个聚合线程 |
| MTP | 3，draft vocab 65536 |
| 上下文配置上限 | 262144 |
| 调度并发上限 | 1 |
| 批处理Token | 2048 |
| GPU memory utilization | 0.80 |
| KV精度 | auto |
| Compile / CUDA Graph | vLLM默认：VLLM_COMPILE / FULL_AND_PIECEWISE |
| Prefix cache / FlashInfer autotune | **开启** / 关闭 |
| API模型名 | qwen3.8-flash-next |

9份补丁覆盖PLE、model state、MTP reduced vocabulary、QSA、平台接口及ModelOpt。ModelOpt包含本项目修复：MTP RoutedExperts显式采用动态激活、128×128块FP8方法，避免`weight_scale_inv`加载错误。保留这些补丁是单机运行的必要条件。

262144是配置上限；默认并发上限为1，优先保障个人交互的TTFT与单请求速度。历史最新SOP明确接近满256K的端到端稳定性和长时稳定性尚未验收。PLE需要按需读取NVMe，所以不能只看权重大小就换成普通磁盘或网络文件系统。

<!-- [LMM 本地补丁 2026-09-22] 下一段原文含交付方的内部设备编号与内部实测日期，
     按 CONTRIBUTING.md「不得提交内部地址等内部信息」抹去，只改这两处措辞，
     测得的技术数据一字未动。 -->

前缀缓存适用于Agent多轮会话、固定系统提示词和重复工具定义。在单台GB10 MegaCube上使用约19.9K-token公共前缀、相同问题与相同输出上限复测：冷前缀TTFT为11.579秒；稳定热缓存TTFT中位数为1.486秒，命中17,600/19,884 token（88.51%），TTFT降低87.17%。首次请求用于构建缓存；由于混合GDN/Mamba缓存与预热状态，首次复用不保证立即命中，建议服务启动后执行至少两轮代表性预热。

## 6. API例子

在MegaCube本机执行：

```bash
curl -f http://127.0.0.1:8000/health
curl -f http://127.0.0.1:8000/v1/models
curl -f http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.8-flash-next","messages":[{"role":"user","content":"你好，请介绍一下你自己。"}],"max_tokens":256,"temperature":0.6}'
```

## 7. 常见问题

- **镜像身份不一致：**保留报错，核实厂商镜像；不要删除校验逻辑或换浮动镜像。
- **网络下载失败：**检查本机互联网、DNS与Hub访问权限，重新运行`engine`或`download`；镜像代理若无法提供固定摘要，应由管理员检查代理源，不要改成标签。首次部署没有模型和镜像时不支持全离线。
- **内存不足：**停止其他工作负载，确认是128GB GB10设备。不要关闭PLE staged或增大并发尝试绕过。
- **加载失败/超时：**查看`logs`和`logs/`归档；检查NVMe速度、模型校验、驱动、镜像、补丁路径。首次启动需要十多分钟。
- **端口占用：**停止占用端口的应用，或修改config端口；脚本不会杀掉该应用。
- **status显示未安装：**容器不存在；不代表模型权重不存在。
- **service stopped后verify：**完整校验读取全部权重，请预留时间；该检查不需要GPU推理。

上游Apache-2.0许可证和NOTICE保存在runtime中。本包不包含模型权重、Docker镜像、旧集群账号或远程访问凭证。
