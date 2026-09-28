#!/bin/bash
set -euo pipefail
: "${LMM_ENGINE_NAME:?}"

if [ "$LMM_ENGINE_NAME" != "sglang" ]; then
    echo "ERROR: unsupported engine: $LMM_ENGINE_NAME" >&2
    exit 1
fi

TARGET_UID="${PKEXEC_UID:-$(id -u)}"
TARGET_HOME="$(getent passwd "$TARGET_UID" | cut -d: -f6)"
[ -n "$TARGET_HOME" ] || { echo "ERROR: 无法解析目标用户 HOME" >&2; exit 1; }
VENDOR_DIR="$TARGET_HOME/.local/share/local-model-manager/vendor/qwen38-nvfp4-mtp"

command -v docker >/dev/null 2>&1 || { echo "ERROR: 未找到 docker，请先在设备上安装 Docker Engine + NVIDIA Container Toolkit" >&2; exit 1; }
command -v nvidia-smi >/dev/null 2>&1 || { echo "ERROR: 未找到 nvidia-smi，请确认 NVIDIA 驱动已安装" >&2; exit 1; }

# LMM_PROBE_ONLY=1：以普通用户身份跑的只读探针（模型管理器在每次下载前都会
# 先跑一次，命中就跳过下面整套安装流程）。它同时负责判断这次安装需不需要
# 管理员权限，见下面的 LMM_NEED_ROOT。这里只判断运行时镜像在不在，不发任何安装/配置命令。
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    # 先告诉 LMM 这次安装到底需不需要管理员权限。
    #
    # 真正需要 root 的只有一件事：NVIDIA Container Toolkit 还没注册成 docker
    # runtime（要改 /etc/docker 的配置并重启 docker 服务），那是设备一次性的
    # 环境配置。其余动作普通用户都能做——docker pull / docker load 靠 docker
    # 组权限，部署包写的是用户自己的 ~/.local/share/local-model-manager。
    # 不该为一个通常根本不会执行的分支，让用户每次下载都输一遍管理员密码。
    if ! docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q 'nvidia'; then
        echo "LMM_NEED_ROOT=1"
    fi
    IMAGE="lmsysorg/sglang:v0.5.16-cu130"
    if docker image inspect "$IMAGE" >/dev/null 2>&1; then
        echo "probe: sglang engine present ($IMAGE)"
        exit 0
    fi
    echo "probe: sglang engine missing ($IMAGE)" >&2
    exit 1
fi

# NVIDIA Container Toolkit 装了但没注册成 Docker runtime，是个常见的设备环境
# 缺口——这项检查本来要等部署阶段的 preflight.sh 才会跑到，等于让用户先空等
# 一整轮引擎安装 + 模型下载才发现环境没配好。这里提前到 install_engine（本来
# 就是 root 权限执行）自动检测并修复；如果连 nvidia-ctk 命令本身都没有（说明
# 工具包压根没装），没法自动装包，只能提示用户手动装。
if ! docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q 'nvidia'; then
    # 走到这里说明确实要改系统配置（写 /etc/docker + 重启 docker 服务）。
    # LMM 会先跑上面那个探针，探针报了 LMM_NEED_ROOT=1 它才会用 pkexec 重跑
    # 一遍，所以正常情况下执行到这里已经是 root 身份了。万一不是（老版本
    # LMM、或者用户手工执行脚本），把话说清楚，而不是抛一堆 permission denied。
    if [ "$(id -u)" != "0" ]; then
        echo "ERROR: 注册 NVIDIA Container Toolkit 运行时需要管理员权限，请重新点击下载并在弹出的授权窗口中输入密码" >&2
        exit 1
    fi
    if command -v nvidia-ctk >/dev/null 2>&1; then
        echo "检测到 NVIDIA Container Toolkit 未注册为 Docker runtime，正在自动配置…"
        nvidia-ctk runtime configure --runtime=docker
        systemctl restart docker
    else
        echo "ERROR: 未检测到 nvidia-ctk，请先安装 NVIDIA Container Toolkit（apt install nvidia-container-toolkit）后重试" >&2
        exit 1
    fi
fi

mkdir -p "$VENDOR_DIR/scripts"
cat > "$VENDOR_DIR/scripts/common.sh" << 'LMM_SCRIPTS_COMMON_SH'
#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
APP_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
ENV_FILE="${APP_DIR}/.env"
COMPOSE_FILE="${APP_DIR}/compose.yaml"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
info() { printf '[qwen38] %s\n' "$*"; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"; }

load_env() {
  [[ -f "${ENV_FILE}" ]] || die "缺少 ${ENV_FILE}，请先运行 scripts/deploy.sh"
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
  : "${IMAGE:?IMAGE 未配置}"
  : "${CONTAINER_NAME:?CONTAINER_NAME 未配置}"
  : "${MODEL_PATH:?MODEL_PATH 未配置}"
  : "${CACHE_PATH:?CACHE_PATH 未配置}"
  : "${HOST_BIND:?HOST_BIND 未配置}"
  : "${PORT:?PORT 未配置}"
}

compose() {
  docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" "$@"
}

assert_safe_absolute_path() {
  local path="$1"
  [[ "${path}" == /* ]] || die "必须使用绝对路径: ${path}"
  case "${path}" in /|/home|/opt|/usr|/var|/root) die "拒绝操作过宽路径: ${path}" ;; esac
}

model_complete() {
  local dir="$1"
  [[ -f "${dir}/config.json" ]] && \
    find "${dir}" -maxdepth 1 -type f -name '*.safetensors' -print -quit | grep -q .
}

# MODEL_PATH 里没有完整模型时，再看一眼用户常用模型目录 ~/models/<仓库名>；
# 有就复用并把 .env 的 MODEL_PATH 改写过去，deploy/compose 跟着用同一路径，
# 已经下过的几十 GB 不再重复下载。
reuse_local_model_if_present() {
  if model_complete "${MODEL_PATH}"; then
    return 0
  fi
  local alt="${HOME}/models/${HF_REPO##*/}"
  if model_complete "${alt}"; then
    info "检测到本机已有模型: ${alt}，复用该目录，不再重复下载"
    MODEL_PATH="${alt}"
    sed -i "s|^MODEL_PATH=.*|MODEL_PATH=${alt}|" "${ENV_FILE}"
  fi
}
LMM_SCRIPTS_COMMON_SH
cat > "$VENDOR_DIR/scripts/deploy.sh" << 'LMM_SCRIPTS_DEPLOY_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"

if [[ ! -f "${ENV_FILE}" ]]; then
  cp "${APP_DIR}/.env.example" "${ENV_FILE}"
  info "已创建 ${ENV_FILE}；如路径或监听地址不同，请修改后重新运行"
fi
load_env
reuse_local_model_if_present
require_cmd docker
assert_safe_absolute_path "${MODEL_PATH}"
assert_safe_absolute_path "${CACHE_PATH}"

mkdir -p "${MODEL_PATH}" "${CACHE_PATH}/sglang" "${CACHE_PATH}/huggingface" "${APP_DIR}/runtime"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  info "本机没有运行镜像，开始下载: ${IMAGE}"
  docker pull "${IMAGE}"
fi

if ! model_complete "${MODEL_PATH}"; then
  info "本机没有完整模型，开始下载: ${HF_REPO}@${HF_REVISION}"
  "${SCRIPT_DIR}/download-model.sh"
fi

"${SCRIPT_DIR}/preflight.sh"
compose config > "${APP_DIR}/runtime/compose.resolved.yaml"
docker image inspect "${IMAGE}" > "${APP_DIR}/runtime/image.inspect.json"
info "镜像、模型与部署文件准备完成。运行 scripts/start.sh 启动服务。"
LMM_SCRIPTS_DEPLOY_SH
# 2026-09-17 那版厂商包在这里加了 `export HF_ENDPOINT="${HF_ENDPOINT:-https://hf-mirror.com}"`
# 和对应的 .env.example 条目，我们**故意不跟进**：LMM 每次执行脚本都会注入
# LMM_HF_ENDPOINT（设置里的下载源，默认就是同一个镜像站），卡片的
# download_model.sh 已经把它转成 HF_ENDPOINT 传下来。再抄一份厂商的兜底值，
# 一是永远轮不到它生效（外面那层一定有值），二是把一个下载地址写死进卡片，
# 违反"选源由 LMM 负责"这条规范。厂商那份是给脱离 LMM 手工执行的人用的。
cat > "$VENDOR_DIR/scripts/download-model.sh" << 'LMM_SCRIPTS_DOWNLOAD_MODEL_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
reuse_local_model_if_present
assert_safe_absolute_path "${MODEL_PATH}"
mkdir -p "${MODEL_PATH}"

if command -v hf >/dev/null 2>&1; then
  HF_CLI="$(command -v hf)"
else
  require_cmd python3
  tool_venv="${APP_DIR}/.tools/huggingface"
  if [[ ! -x "${tool_venv}/bin/hf" ]]; then
    info "正在部署隔离的 Hugging Face 下载工具"
    python3 -m venv "${tool_venv}" || die "无法创建 Python venv；请安装 python3-venv 后重试"
    "${tool_venv}/bin/python" -m pip install --upgrade pip huggingface_hub
  fi
  HF_CLI="${tool_venv}/bin/hf"
fi
# --max-workers 4：默认 8 并发时，网络一断这 8 个没下完的分片会被整体丢弃
# 重来（真机实测：进度从 16.8GB 掉回 4.3GB，每个分片约 1.4GB）。
# 但也不能一味降到 1——真机对照实测：8 并发聚合 5.0 MB/s，单连接只有
# 1.2 MB/s，降到 1 会让 67GB 从约 3.7 小时变成约 15 小时，代价太大。
# 取 4：多连接的带宽收益到 4 路基本已经吃满（实测聚合仍接近 5 MB/s），
# 而一次断线的损失从约 12GB 减半到约 6GB。
"${HF_CLI}" download "${HF_REPO}" --revision "${HF_REVISION}" --local-dir "${MODEL_PATH}" --max-workers 4
info "模型下载完成: ${MODEL_PATH}"
LMM_SCRIPTS_DOWNLOAD_MODEL_SH
cat > "$VENDOR_DIR/scripts/preflight.sh" << 'LMM_SCRIPTS_PREFLIGHT_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"

load_env
require_cmd docker
require_cmd curl
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 不可用"
docker info >/dev/null 2>&1 || die "当前用户无法访问 Docker daemon"
docker info --format '{{json .Runtimes}}' | grep -q 'nvidia' || die "未检测到 NVIDIA Container Toolkit runtime"
command -v nvidia-smi >/dev/null 2>&1 || die "未找到 nvidia-smi"
nvidia-smi >/dev/null || die "NVIDIA Driver/GPU 不可用"

arch="$(uname -m)"
[[ "${arch}" == "aarch64" || "${arch}" == "arm64" ]] || die "当前架构 ${arch}，已验证镜像要求 ARM64/aarch64"

assert_safe_absolute_path "${MODEL_PATH}"
assert_safe_absolute_path "${CACHE_PATH}"
[[ -d "${MODEL_PATH}" ]] || die "模型目录不存在: ${MODEL_PATH}"
[[ -f "${MODEL_PATH}/config.json" ]] || die "模型目录缺少 config.json: ${MODEL_PATH}"
find "${MODEL_PATH}" -maxdepth 1 -type f \( -name '*.safetensors' -o -name '*.safetensors.index.json' \) -print -quit | grep -q . || die "模型目录未发现 safetensors 权重"

docker image inspect "${IMAGE}" >/dev/null 2>&1 || die "镜像不存在: ${IMAGE}；请重新运行 deploy.sh 自动下载"
image_arch="$(docker image inspect "${IMAGE}" --format '{{.Architecture}}')"
[[ "${image_arch}" == "arm64" ]] || die "镜像架构为 ${image_arch}，预期 arm64"

if ss -ltnH "sport = :${PORT}" 2>/dev/null | grep -q .; then
  running="$(docker ps --filter "name=^/${CONTAINER_NAME}$" --format '{{.Names}}')"
  [[ "${running}" == "${CONTAINER_NAME}" ]] || die "端口 ${PORT} 已被其他进程占用"
fi

info "预检通过: GPU=$(nvidia-smi --query-gpu=name --format=csv,noheader | head -n1), image=${IMAGE}, model=${MODEL_PATH}"
LMM_SCRIPTS_PREFLIGHT_SH
cat > "$VENDOR_DIR/scripts/start.sh" << 'LMM_SCRIPTS_START_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
"${SCRIPT_DIR}/preflight.sh"
compose up -d

url="http://${HOST_BIND}:${PORT}/health"
[[ "${HOST_BIND}" == "0.0.0.0" ]] && url="http://127.0.0.1:${PORT}/health"
deadline=$((SECONDS + 600))
info "模型加载中，最多等待 600 秒: ${url}"
until curl -fsS --max-time 3 "${url}" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    compose logs --tail=120 qwen38 >&2 || true
    die "服务未在 600 秒内就绪"
  fi
  if ! compose ps --status running --services | grep -qx qwen38; then
    compose logs --tail=120 qwen38 >&2 || true
    die "容器提前退出"
  fi
  # 每轮打一行心跳。等待期间一声不吭的话，LMM 那边的「连续 N 秒没有输出就
  # 判定卡死」会在模型还在加载权重时把启动脚本杀掉——真机上就这么误杀过一次
  # （67GB 的 BF16 权重要加载 5-10 分钟，而静默超时是 5 分钟）。
  info "等待服务就绪…已等待 ${SECONDS}s（上限 $((deadline))s）"
  sleep 5
done
info "服务已就绪: http://${HOST_BIND}:${PORT}/v1"
"${SCRIPT_DIR}/smoke-test.sh"
LMM_SCRIPTS_START_SH
cat > "$VENDOR_DIR/scripts/status.sh" << 'LMM_SCRIPTS_STATUS_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
compose ps
url="http://${HOST_BIND}:${PORT}/health"
[[ "${HOST_BIND}" == "0.0.0.0" ]] && url="http://127.0.0.1:${PORT}/health"
if curl -fsS --max-time 3 "${url}" >/dev/null 2>&1; then
  info "健康检查: OK (${url})"
else
  info "健康检查: NOT READY (${url})"
  exit 1
fi
nvidia-smi --query-gpu=name,utilization.gpu,temperature.gpu,power.draw --format=csv,noheader || true
LMM_SCRIPTS_STATUS_SH
cat > "$VENDOR_DIR/scripts/stop.sh" << 'LMM_SCRIPTS_STOP_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
compose stop --timeout 60
info "服务已停止；容器、模型和缓存均保留。"
LMM_SCRIPTS_STOP_SH
cat > "$VENDOR_DIR/scripts/uninstall.sh" << 'LMM_SCRIPTS_UNINSTALL_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env

purge_cache=false
purge_model=false
for arg in "$@"; do
  case "${arg}" in
    --purge-cache) purge_cache=true ;;
    --purge-model) purge_model=true ;;
    *) die "未知参数: ${arg}" ;;
  esac
done

compose down --remove-orphans
rm -rf -- "${APP_DIR}/runtime"

if [[ "${purge_cache}" == true ]]; then
  assert_safe_absolute_path "${CACHE_PATH}"
  rm -rf -- "${CACHE_PATH}"
  info "已删除缓存: ${CACHE_PATH}"
fi

if [[ "${purge_model}" == true ]]; then
  assert_safe_absolute_path "${MODEL_PATH}"
  [[ -f "${MODEL_PATH}/config.json" ]] || die "拒绝删除：目标不像模型目录 ${MODEL_PATH}"
  [[ "${CONFIRM_DELETE_MODEL:-}" == "${MODEL_PATH}" ]] || die "删除模型需设置 CONFIRM_DELETE_MODEL=${MODEL_PATH}"
  rm -rf -- "${MODEL_PATH}"
  info "已删除模型权重（不可由本脚本恢复）: ${MODEL_PATH}"
fi

info "服务已卸载。默认保留镜像、模型、缓存与 .env。"
LMM_SCRIPTS_UNINSTALL_SH
cat > "$VENDOR_DIR/scripts/logs.sh" << 'LMM_SCRIPTS_LOGS_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
compose logs -f --tail="${TAIL:-200}" qwen38
LMM_SCRIPTS_LOGS_SH
mkdir -p "$VENDOR_DIR/profiles"
mkdir -p "$VENDOR_DIR/profiles/benchmark"
cat > "$VENDOR_DIR/profiles/benchmark/compose.yaml" << 'LMM_PROFILE_BENCHMARK_COMPOSE_YAML'
services:
  qwen38:
    image: ${IMAGE}
    container_name: ${CONTAINER_NAME}
    network_mode: host
    ipc: host
    gpus: all
    restart: unless-stopped
    volumes:
      - ${MODEL_PATH}:/models/target:ro
      - ${CACHE_PATH}/sglang:/root/.cache/sglang
      - ${CACHE_PATH}/huggingface:/root/.cache/huggingface
    command:
      - python3
      - -m
      - sglang.launch_server
      - --model-path=/models/target
      - --host=${HOST_BIND}
      - --port=${PORT}
      - --context-length=${CONTEXT_LENGTH}
      - --mem-fraction-static=${MEM_FRACTION_STATIC}
      - --kv-cache-dtype=${KV_CACHE_DTYPE}
      - --trust-remote-code
      - --speculative-algorithm=NEXTN
      - --speculative-num-steps=${SPECULATIVE_NUM_STEPS}
      - --speculative-eagle-topk=${SPECULATIVE_TOPK}
      - --speculative-num-draft-tokens=${SPECULATIVE_NUM_DRAFT_TOKENS}
    healthcheck:
      test: ["CMD-SHELL", "python3 -c \"import urllib.request; urllib.request.urlopen('http://127.0.0.1:${PORT}/health', timeout=3)\""]
      interval: 15s
      timeout: 5s
      retries: 4
      start_period: 300s
LMM_PROFILE_BENCHMARK_COMPOSE_YAML
cat > "$VENDOR_DIR/profiles/benchmark/.env.example" << 'LMM_PROFILE_BENCHMARK__ENV_EXAMPLE'
COMPOSE_PROJECT_NAME=qwen38-nvfp4-mtp
IMAGE=lmsysorg/sglang:v0.5.16-cu130
CONTAINER_NAME=qwen38-nvfp4-mtp
MODEL_PATH=${HOME}/models/Qwen3.8-27B-NVFP4
CACHE_PATH=${HOME}/.cache/qwen38-nvfp4-mtp
HOST_BIND=127.0.0.1
PORT=30000
CONTEXT_LENGTH=8192
MEM_FRACTION_STATIC=0.70
KV_CACHE_DTYPE=bf16
SPECULATIVE_NUM_STEPS=3
SPECULATIVE_TOPK=1
SPECULATIVE_NUM_DRAFT_TOKENS=4
HF_REPO=RadixArk/Qwen3.8-27B-NVFP4
HF_REVISION=main
LMM_PROFILE_BENCHMARK__ENV_EXAMPLE
cat > "$VENDOR_DIR/profiles/benchmark/smoke-test.sh" << 'LMM_PROFILE_BENCHMARK_SMOKE_TEST_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
require_cmd curl
base="http://${HOST_BIND}:${PORT}"
[[ "${HOST_BIND}" == "0.0.0.0" ]] && base="http://127.0.0.1:${PORT}"
payload='{"model":"/models/target","messages":[{"role":"user","content":"只回复 OK"}],"temperature":0,"max_tokens":8,"chat_template_kwargs":{"enable_thinking":false}}'
response="$(curl -fsS --max-time 120 -H 'Content-Type: application/json' -d "${payload}" "${base}/v1/chat/completions")"
printf '%s' "${response}" | grep -q 'choices' || die "API 冒烟响应缺少 choices"
info "OpenAI-compatible API 冒烟测试通过"
LMM_PROFILE_BENCHMARK_SMOKE_TEST_SH
mkdir -p "$VENDOR_DIR/profiles/production-sampling"
# 下面这份 compose 取自厂商交付包 deploy-qwen38-nvfp4-mtp-production-sampling.zip
# （2026-09-17 收到的那版），原样照抄。相对上一版厂商原件有三处启动参数变化，
# 都是厂商在 README「已验证配置」里点名的，不是我们改的：
#   --tp-size=1                  单卡显式声明，不让 SGLang 去猜
#   --attention-backend=flashinfer  固定成本次验证采用的后端
#   --tool-call-parser=qwen3_coder  从 auto 换成 Qwen3.8 官方推荐的解析器
# 同时去掉了 --trust-remote-code：模型已被当前 SGLang/Transformers 原生识别，
# 厂商刻意收窄供应链信任面。基准测试方案是另一套验证结果，不跟着改。
cat > "$VENDOR_DIR/profiles/production-sampling/compose.yaml" << 'LMM_PROFILE_PRODUCTION_SAMPLING_COMPOSE_YAML'
services:
  qwen38:
    image: ${IMAGE}
    container_name: ${CONTAINER_NAME}
    network_mode: host
    ipc: host
    gpus: all
    restart: unless-stopped
    volumes:
      - ${MODEL_PATH}:/models/target:ro
      - ${CACHE_PATH}/sglang:/root/.cache/sglang
      - ${CACHE_PATH}/huggingface:/root/.cache/huggingface
    command:
      - python3
      - -m
      - sglang.launch_server
      - --model-path=/models/target
      - --served-model-name=${SERVED_MODEL_NAME}
      - --host=${HOST_BIND}
      - --port=${PORT}
      - --context-length=${CONTEXT_LENGTH}
      - --tp-size=1
      - --mem-fraction-static=${MEM_FRACTION_STATIC}
      - --kv-cache-dtype=${KV_CACHE_DTYPE}
      - --attention-backend=flashinfer
      - --reasoning-parser=qwen3
      - --tool-call-parser=qwen3_coder
      - --sampling-defaults=model
      - '--preferred-sampling-params={"temperature":1.0,"top_p":0.95,"top_k":20,"min_p":0.0,"presence_penalty":0.0,"repetition_penalty":1.0}'
      - --speculative-algorithm=NEXTN
      - --speculative-num-steps=${SPECULATIVE_NUM_STEPS}
      - --speculative-eagle-topk=${SPECULATIVE_TOPK}
      - --speculative-num-draft-tokens=${SPECULATIVE_NUM_DRAFT_TOKENS}
    healthcheck:
      test: ["CMD-SHELL", "python3 -c \"import urllib.request; urllib.request.urlopen('http://127.0.0.1:${PORT}/health', timeout=3)\""]
      interval: 15s
      timeout: 5s
      retries: 4
      start_period: 300s
LMM_PROFILE_PRODUCTION_SAMPLING_COMPOSE_YAML
cat > "$VENDOR_DIR/profiles/production-sampling/.env.example" << 'LMM_PROFILE_PRODUCTION_SAMPLING__ENV_EXAMPLE'
COMPOSE_PROJECT_NAME=qwen38-nvfp4-mtp
IMAGE=lmsysorg/sglang:v0.5.16-cu130
CONTAINER_NAME=qwen38-nvfp4-mtp
MODEL_PATH=${HOME}/models/Qwen3.8-27B-NVFP4
CACHE_PATH=${HOME}/.cache/qwen38-nvfp4-mtp
HOST_BIND=0.0.0.0
PORT=30000
SERVED_MODEL_NAME=qwen3.8-27b
CONTEXT_LENGTH=262144
MEM_FRACTION_STATIC=0.70
KV_CACHE_DTYPE=fp8_e4m3
SPECULATIVE_NUM_STEPS=3
SPECULATIVE_TOPK=1
SPECULATIVE_NUM_DRAFT_TOKENS=4
HF_REPO=RadixArk/Qwen3.8-27B-NVFP4
HF_REVISION=main
LMM_PROFILE_PRODUCTION_SAMPLING__ENV_EXAMPLE
cat > "$VENDOR_DIR/profiles/production-sampling/smoke-test.sh" << 'LMM_PROFILE_PRODUCTION_SAMPLING_SMOKE_TEST_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
require_cmd curl
base="http://${HOST_BIND}:${PORT}"
[[ "${HOST_BIND}" == "0.0.0.0" ]] && base="http://127.0.0.1:${PORT}"
payload='{"model":"qwen3.8-27b","messages":[{"role":"user","content":"只回复 OK"}],"temperature":0,"max_tokens":8,"chat_template_kwargs":{"enable_thinking":false}}'
response="$(curl -fsS --max-time 120 -H 'Content-Type: application/json' -d "${payload}" "${base}/v1/chat/completions")"
printf '%s' "${response}" | grep -q 'choices' || die "API 冒烟响应缺少 choices"
info "OpenAI-compatible API 冒烟测试通过"
LMM_PROFILE_PRODUCTION_SAMPLING_SMOKE_TEST_SH
cp "$VENDOR_DIR/profiles/benchmark/compose.yaml" "$VENDOR_DIR/compose.yaml"
cp "$VENDOR_DIR/profiles/benchmark/.env.example" "$VENDOR_DIR/.env.example"
cp "$VENDOR_DIR/profiles/benchmark/smoke-test.sh" "$VENDOR_DIR/scripts/smoke-test.sh"
chmod +x "$VENDOR_DIR"/scripts/*.sh
# 以 root 身份跑的那一次（设备首次配置环境）产生的文件属主是 root，必须改回
# 用户；以普通用户身份跑时文件本来就是他自己的，不需要也不该 chown。
if [ "$(id -u)" = "0" ]; then
    chown -R "$TARGET_UID":"$TARGET_UID" "$TARGET_HOME/.local/share/local-model-manager" 2>/dev/null || true
fi

# 找用户可能手动放好的离线镜像包（比如从别的联网设备上导出、U 盘拷进来的），
# 找常见的暂存目录 + U 盘挂载点，文件名要带 sglang 字样才当候选，避免误加载
# 不相关的镜像包。
IMAGE="lmsysorg/sglang:v0.5.16-cu130"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    ARCHIVE=""
    SEARCH_START=$SECONDS
    for dir in "$TARGET_HOME/LMM" "$TARGET_HOME/lmm-deploy" "$TARGET_HOME/Downloads" "$TARGET_HOME/桌面" "$TARGET_HOME/Desktop" /media/*/* /media/* /mnt/* /run/media/*/*; do
        [ -d "$dir" ] || continue
        # 单个目录、总耗时都设上限，避免某个挂载点（比如网络共享）很大/很
        # 慢时把整个安装过程拖住——找不到就正常走后面的网络拉取兜底。
        if [ $((SECONDS - SEARCH_START)) -ge 15 ]; then
            echo "本地查找离线镜像包超过 15 秒，不再继续找，改走网络拉取" >&2
            break
        fi
        found="$(timeout 3 find "$dir" -maxdepth 3 -type f \( -iname "*sglang*.tar.zst" -o -iname "*sglang*.tar.gz" -o -iname "*sglang*.tar" \) 2>/dev/null | head -1)" || true
        if [ -n "$found" ]; then ARCHIVE="$found"; break; fi
    done
    if [ -n "$ARCHIVE" ]; then
        echo "本机没有 $IMAGE，在 $ARCHIVE 找到本地离线镜像包，正在导入…"
        case "$ARCHIVE" in
            *.tar.zst) zstd -dc "$ARCHIVE" | docker load ;;
            *) docker load -i "$ARCHIVE" ;;
        esac
    fi
fi
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "本机没有 $IMAGE，开始拉取…"
    # docker pull 镜像层数多、体积大，可能跑很久；这一步是以 root 身份
    # 执行的，LMM 应用本身（非 root）没有权限强行终止我们，所以后台跑 +
    # 轮询 LMM_CANCEL_FILE 是否出现，出现了就自己把 docker pull 杀掉退出
    # ——只有 root 身份的这个脚本自己能可靠取消这个操作。
    docker pull "$IMAGE" &
    PULL_PID=$!
    while kill -0 "$PULL_PID" 2>/dev/null; do
        if [ -n "${LMM_CANCEL_FILE:-}" ] && [ -f "$LMM_CANCEL_FILE" ]; then
            echo "收到取消请求，终止镜像拉取…" >&2
            kill "$PULL_PID" 2>/dev/null
            wait "$PULL_PID" 2>/dev/null
            exit 1
        fi
        sleep 1
    done
    wait "$PULL_PID" || { echo "ERROR: 镜像拉取失败" >&2; exit 1; }
fi
echo "sglang 运行环境（docker + $IMAGE）已就绪，部署包已落地到 $VENDOR_DIR"
# 供"本地模型库"里"已安装引擎"删除功能使用：删除时直接 docker rmi 这个
# 镜像即可。
echo "LMM_ENGINE_IMAGE=$IMAGE"
