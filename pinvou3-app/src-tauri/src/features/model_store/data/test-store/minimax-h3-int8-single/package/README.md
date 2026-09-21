# MiniMax-H3 INT8 单机视频生成｜LMM 应用商城卡片

本包按《LMM 模型卡片脚本规范》1.0 制作，用于把已在 GB10 MegaCube 上跑通的 MiniMax-H3 单机方案复现到其他同型号机器。六个入口脚本可由 LMM 调用，也可由同事在普通登录用户下手动执行。

## 锁定的模型与引擎版本

| 项目 | 锁定版本或文件 |
|---|---|
| 目标设备 | 单台 NVIDIA GB10，ARM64/aarch64，sm_121，128 GB 统一内存 |
| 已验证系统 | Linux kernel 6.17.0-1031-nvidia，驱动 580.173.02 |
| Python / PyTorch | Python 3.12.3；PyTorch 2.13.0+cu130；torchvision 0.28.0+cu130；torchaudio 2.11.0+cu130 |
| ComfyUI | 0.35.0；frontend 1.51.10；workflow templates 0.11.57；embedded docs 0.5.11；comfy-kitchen 0.2.33；comfy-aimdo 0.5.3 |
| 主模型 | `minimax_h3_fl2va_pruned_int8_convrot.safetensors`，20,970,379,616 字节，仓库 revision `a98869194787969724c7425d95d0ed73ce9202af` |
| 文本编码器 | `qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors`，NVFP4 AWQ，15,687,142,551 字节，同上 revision |
| 视频 / 音频 VAE | FP16 5,207,808,496 字节；FP32 605,254,808 字节，同上 revision |
| Turbo LoRA | `minimax_h3_turbo_v4_step600_ema.safetensors`，779,849,816 字节，revision `43a74557ac3f6539db8e0f2a959d03feb7a81480` |
| 1080p 超分 | `RealESRGAN_x2plus.pth`，67,061,725 字节，按官方发布文件 SHA-256 锁定；HF 镜像仓库只承担传输 |
| 权重总量 | 43,317,497,012 字节（约 40.34 GiB） |

ComfyUI 与 Turbo 节点使用包内源码归档，SHA-256 分别为 `e42f1265...886ba` 与 `a1398cac...a39`，避免部署时拉取浮动分支。完整值见 `sources/SOURCE_LOCKS.json`。启动参数固定为 `--listen 0.0.0.0 --port 8188 --fast autotune`；注意力实现使用 PyTorch attention。

## 设备前提

- 目标必须是单台 MegaCube / NVIDIA GB10 ARM64；至少预留 60 GiB 模型空间和约 10 GiB 引擎空间。
- 驱动必须能被 `nvidia-smi` 识别。部署用户应能读写 `$HOME/.local/share/lmm` 和模型目录。
- 优先复用经 MegaCube 验证的 Python 3.12 + PyTorch 2.13.0+cu130 基础虚拟环境。默认查找 `$HOME/minimax-h3-runtime`，其他位置通过 `LMM_BASE_RUNTIME` 指定；没有基础运行时时，会用系统 `python3.12` 在用户目录创建环境并安装锁定的 CUDA 13.0 轮子。
- 在线安装引擎依赖需要可用的 Python 软件源；模型下载只使用 LMM 传入的 `LMM_SOURCE_BASE_URL`。

## LMM 生命周期

依次执行 `install_engine.sh`、`download_model.sh`、`deploy.sh`、`enable.sh`。停用执行 `stop.sh`；卸载执行 `uninstall.sh`。`uninstall.sh` 默认保留 40.34 GiB 权重；只有显式设置 `LMM_REMOVE_ENGINE=1` 才删除本卡片专用引擎。

手动执行示例：

```bash
export LMM_MODELS_DIR="$HOME/lmm-models"
export LMM_MODEL_NAME="minimax-h3-int8-single"
export LMM_MODEL_SIZE_BYTES="43317497012"
export LMM_SOURCE_KIND="hf_endpoint"
export LMM_SOURCE_BASE_URL="https://hf-mirror.com"
export LMM_BASE_RUNTIME="$HOME/minimax-h3-runtime"
bash install_engine.sh
bash download_model.sh
LMM_DEPLOY_PROFILE=quality-current bash deploy.sh
bash enable.sh
```

离线源目录必须与模型目录结构一致，包含 `diffusion_models/`、`text_encoders/`、`vae/`、`loras/`、`upscale_models/`。从下载完成的机器复制整个模型目录即可；脚本用 `rsync --partial` 续传并按 `weights.sha256` 复核。

## 工作流

- `quality-current` 是当前实际使用的质量配置：1376×768、243 帧、24 fps、10 步、denoise 1.0、Turbo LoRA strength 1.3、low_vram 关闭；同时输出 768p 和经 RealESRGAN 2× 逐帧超分、Lanczos 精确缩放后的 1920×1080 视频。
- `balanced-fast` 是此前稳定配置：1376×768、124 帧、6 步、Turbo LoRA strength 1.0，同样提供 1080p 输出链路。
- 本地节点显示名为“本地 · MiniMax H3 Image to Video”“本地 · MiniMax-H3 Turbo LoRA”“本地 · MiniMax-H3 Turbo Sampler (4-step)”。

## 验收状态与已知限制

已在单台 GB10 上完成 CUDA 计算验证、模型加载、文生视频/图生视频、同步音频与 1080p 超分，连续多次生成任务均成功。卡片脚本另通过官方 `check_card.py` 静态自检。

首次启用只检查 ComfyUI `/queue` 健康接口，不会自动提交耗时的视频任务。MiniMax-H3 的 768p 生成和逐帧 1080p 超分会占用较多统一内存，建议单任务串行。文本准确排版不是该视频模型的强项。RealESRGAN 文件来自第三方 Hugging Face 镜像仓库，但必须与官方 v0.2.1 发布文件 SHA-256 完全一致，否则脚本会删除并报错。
