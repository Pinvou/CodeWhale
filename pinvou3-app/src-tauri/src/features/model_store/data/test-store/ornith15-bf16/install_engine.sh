#!/bin/bash
set -euo pipefail
: "${LMM_ENGINE_NAME:?}"

if [ "$LMM_ENGINE_NAME" != "vllm" ]; then
    echo "ERROR: unsupported engine: $LMM_ENGINE_NAME" >&2
    exit 1
fi

TARGET_UID="${PKEXEC_UID:-$(id -u)}"
TARGET_HOME="$(getent passwd "$TARGET_UID" | cut -d: -f6)"
[ -n "$TARGET_HOME" ] || { echo "ERROR: 无法解析目标用户 HOME" >&2; exit 1; }
VENDOR_DIR="$TARGET_HOME/.local/share/local-model-manager/vendor/ornith15-bf16"

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
    IMAGE="vllm/vllm-openai:v0.27.1-aarch64"
    if docker image inspect "$IMAGE" >/dev/null 2>&1; then
        echo "probe: vllm engine present ($IMAGE)"
        exit 0
    fi
    echo "probe: vllm engine missing ($IMAGE)" >&2
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
cat > "$VENDOR_DIR/compose.yaml" << 'LMM_COMPOSE_YAML'
services:
  ornith15:
    image: ${IMAGE}
    container_name: ${CONTAINER_NAME}
    network_mode: host
    ipc: host
    gpus: all
    restart: unless-stopped
    ulimits:
      memlock: -1
      stack: 67108864
    volumes:
      - ${MODEL_PATH}:/model:ro
      - ${CACHE_PATH}/huggingface:/root/.cache/huggingface
      - ${CACHE_PATH}/vllm:/root/.cache/vllm
    command:
      - /model
      - --served-model-name=${SERVED_MODEL_NAME}
      - --host=${HOST_BIND}
      - --port=${PORT}
      - --dtype=bfloat16
      - --max-model-len=${MAX_MODEL_LEN}
      - --gpu-memory-utilization=${GPU_MEMORY_UTILIZATION}
      - --max-num-seqs=${MAX_NUM_SEQS}
      - --enable-prefix-caching
      - --enable-auto-tool-choice
      - --tool-call-parser=qwen3_xml
      - --reasoning-parser=qwen3
      - --trust-remote-code
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://127.0.0.1:${PORT}/health || exit 1"]
      interval: 15s
      timeout: 5s
      retries: 4
      start_period: 600s
LMM_COMPOSE_YAML
cat > "$VENDOR_DIR/.env.example" << 'LMM__ENV_EXAMPLE'
COMPOSE_PROJECT_NAME=ornith15-bf16
IMAGE=vllm/vllm-openai:v0.27.1-aarch64
CONTAINER_NAME=ornith15-api
MODEL_PATH=${HOME}/models/Ornith-1.5-35B-A3B
CACHE_PATH=${HOME}/.cache/ornith15-bf16
HOST_BIND=0.0.0.0
PORT=8001
SERVED_MODEL_NAME=Ornith-1.5-35B-A3B
MAX_MODEL_LEN=32768
GPU_MEMORY_UTILIZATION=0.70
MAX_NUM_SEQS=1
HF_REPO=ornith-ai/Ornith-1.5-35B-A3B
HF_REVISION=main
LMM__ENV_EXAMPLE
cat > "$VENDOR_DIR/scripts/common.sh" << 'LMM_SCRIPTS_COMMON_SH'
#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
APP_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
ENV_FILE="${APP_DIR}/.env"
COMPOSE_FILE="${APP_DIR}/compose.yaml"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
info() { printf '[ornith15] %s\n' "$*"; }
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
  : "${SERVED_MODEL_NAME:?SERVED_MODEL_NAME 未配置}"
}

compose() { docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" "$@"; }

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
  info "已创建 ${ENV_FILE}"
fi
load_env
reuse_local_model_if_present
require_cmd docker
assert_safe_absolute_path "${MODEL_PATH}"
assert_safe_absolute_path "${CACHE_PATH}"
mkdir -p "${MODEL_PATH}" "${CACHE_PATH}/huggingface" "${CACHE_PATH}/vllm" "${APP_DIR}/runtime"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  info "下载 ARM64 vLLM 镜像: ${IMAGE}"
  docker pull "${IMAGE}"
fi
if ! model_complete "${MODEL_PATH}"; then
  "${SCRIPT_DIR}/download-model.sh"
fi
"${SCRIPT_DIR}/preflight.sh"
compose config > "${APP_DIR}/runtime/compose.resolved.yaml"
docker image inspect "${IMAGE}" > "${APP_DIR}/runtime/image.inspect.json"
info "部署依赖已准备完成，运行 scripts/start.sh 启动"
LMM_SCRIPTS_DEPLOY_SH
cat > "$VENDOR_DIR/scripts/download-model.sh" << 'LMM_SCRIPTS_DOWNLOAD_MODEL_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
reuse_local_model_if_present
assert_safe_absolute_path "${MODEL_PATH}"
mkdir -p "${MODEL_PATH}" "${APP_DIR}/.tools"

export HF_ENDPOINT
if [[ -n "${HF_TOKEN:-}" ]]; then export HF_TOKEN; fi

if command -v hf >/dev/null 2>&1; then
  HF_CLI="$(command -v hf)"
else
  require_cmd python3
  python3 -m venv "${APP_DIR}/.tools/huggingface"
  "${APP_DIR}/.tools/huggingface/bin/python" -m pip install -U pip huggingface_hub
  HF_CLI="${APP_DIR}/.tools/huggingface/bin/hf"
fi

info "下载 ${HF_REPO}@${HF_REVISION} 到 ${MODEL_PATH}（支持断点续传）"
# --max-workers 4：默认 8 并发时，网络一断这 8 个没下完的分片会被整体丢弃
# 重来（真机实测：进度从 16.8GB 掉回 4.3GB，每个分片约 1.4GB）。
# 但也不能一味降到 1——真机对照实测：8 并发聚合 5.0 MB/s，单连接只有
# 1.2 MB/s，降到 1 会让 67GB 从约 3.7 小时变成约 15 小时，代价太大。
# 取 4：多连接的带宽收益到 4 路基本已经吃满（实测聚合仍接近 5 MB/s），
# 而一次断线的损失从约 12GB 减半到约 6GB。
"${HF_CLI}" download "${HF_REPO}" --revision "${HF_REVISION}" --local-dir "${MODEL_PATH}" --max-workers 4
info "模型下载完成"
LMM_SCRIPTS_DOWNLOAD_MODEL_SH
cat > "$VENDOR_DIR/scripts/preflight.sh" << 'LMM_SCRIPTS_PREFLIGHT_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
require_cmd docker
require_cmd curl
require_cmd nvidia-smi
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 不可用"
docker info >/dev/null 2>&1 || die "当前用户无法访问 Docker daemon"
nvidia-smi >/dev/null || die "NVIDIA 驱动/GPU 不可用"

arch="$(uname -m)"
[[ "${arch}" == "aarch64" || "${arch}" == "arm64" ]] || die "镜像要求 ARM64，当前架构为 ${arch}"
assert_safe_absolute_path "${MODEL_PATH}"
assert_safe_absolute_path "${CACHE_PATH}"
[[ -f "${MODEL_PATH}/config.json" ]] || die "模型目录缺少 config.json: ${MODEL_PATH}"
find "${MODEL_PATH}" -maxdepth 1 -type f -name '*.safetensors' -print -quit | grep -q . || die "模型目录没有 safetensors 权重"
docker image inspect "${IMAGE}" >/dev/null 2>&1 || die "镜像不存在: ${IMAGE}"
[[ "$(docker image inspect "${IMAGE}" --format '{{.Architecture}}')" == "arm64" ]] || die "容器镜像不是 arm64"

if ss -ltnH "sport = :${PORT}" 2>/dev/null | grep -q .; then
  running="$(docker ps --filter "name=^/${CONTAINER_NAME}$" --format '{{.Names}}')"
  [[ "${running}" == "${CONTAINER_NAME}" ]] || die "端口 ${PORT} 已被其他进程占用"
fi
info "预检通过: GPU=$(nvidia-smi --query-gpu=name --format=csv,noheader | head -n1), model=${MODEL_PATH}"
LMM_SCRIPTS_PREFLIGHT_SH
cat > "$VENDOR_DIR/scripts/smoke-test.sh" << 'LMM_SCRIPTS_SMOKE_TEST_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
require_cmd curl
base="http://${HOST_BIND}:${PORT}"
[[ "${HOST_BIND}" == "0.0.0.0" ]] && base="http://127.0.0.1:${PORT}"
payload="$(printf '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"只回复 OK\"}],\"temperature\":0,\"max_tokens\":128}' "${SERVED_MODEL_NAME}")"
response="$(curl -fsS --max-time 300 -H 'Content-Type: application/json' -d "${payload}" "${base}/v1/chat/completions")"
printf '%s' "${response}" | grep -q '"choices"' || die "API 响应缺少 choices"
printf '%s\n' "${response}"
info "OpenAI-compatible API 冒烟测试通过"
LMM_SCRIPTS_SMOKE_TEST_SH
cat > "$VENDOR_DIR/scripts/start.sh" << 'LMM_SCRIPTS_START_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
"${SCRIPT_DIR}/preflight.sh"
compose up -d

base="http://${HOST_BIND}:${PORT}"
[[ "${HOST_BIND}" == "0.0.0.0" ]] && base="http://127.0.0.1:${PORT}"
deadline=$((SECONDS + 1200))
info "加载 BF16 权重，首次启动可能需要 5-10 分钟"
until curl -fsS --max-time 3 "${base}/health" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    compose logs --tail=160 ornith15 >&2 || true
    die "服务未在 1200 秒内就绪"
  fi
  if ! compose ps --status running --services | grep -qx ornith15; then
    compose logs --tail=160 ornith15 >&2 || true
    die "容器提前退出"
  fi
  # 每轮打一行心跳。等待期间一声不吭的话，LMM 那边的「连续 N 秒没有输出就
  # 判定卡死」会在模型还在加载权重时把启动脚本杀掉——真机上就这么误杀过一次
  # （67GB 的 BF16 权重要加载 5-10 分钟，而静默超时是 5 分钟）。
  info "等待服务就绪…已等待 ${SECONDS}s（上限 $((deadline))s）"
  sleep 5
done
info "API 已就绪: http://${HOST_BIND}:${PORT}/v1"
"${SCRIPT_DIR}/smoke-test.sh"
LMM_SCRIPTS_START_SH
cat > "$VENDOR_DIR/scripts/status.sh" << 'LMM_SCRIPTS_STATUS_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
compose ps
printf '\nHealth: '
curl -fsS --max-time 3 "http://127.0.0.1:${PORT}/health" >/dev/null && echo OK || echo NOT_READY
printf '\nModels:\n'
curl -fsS --max-time 5 "http://127.0.0.1:${PORT}/v1/models" || true
printf '\n'
LMM_SCRIPTS_STATUS_SH
cat > "$VENDOR_DIR/scripts/stop.sh" << 'LMM_SCRIPTS_STOP_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
compose stop --timeout 60
info "服务已停止；模型、容器和缓存均已保留"
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
info "服务已卸载；默认保留镜像、模型、缓存和 .env"
LMM_SCRIPTS_UNINSTALL_SH
cat > "$VENDOR_DIR/scripts/logs.sh" << 'LMM_SCRIPTS_LOGS_SH'
#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/common.sh"
load_env
compose logs -f --tail=200 ornith15
LMM_SCRIPTS_LOGS_SH
chmod +x "$VENDOR_DIR"/scripts/*.sh
# 以 root 身份跑的那一次（设备首次配置环境）产生的文件属主是 root，必须改回
# 用户；以普通用户身份跑时文件本来就是他自己的，不需要也不该 chown。
if [ "$(id -u)" = "0" ]; then
    chown -R "$TARGET_UID":"$TARGET_UID" "$TARGET_HOME/.local/share/local-model-manager" 2>/dev/null || true
fi

# 找用户可能手动放好的离线镜像包（比如从别的联网设备上导出、U 盘拷进来的），
# 找常见的暂存目录 + U 盘挂载点，文件名要带 vllm 字样才当候选，避免误加载
# 不相关的镜像包——跟 Z-Image 卡片处理它自己那个基础镜像时是同一个套路，
# 这里两张卡片用的刚好是同一个基础镜像，如果设备上已经因为装过 Z-Image
# 而有这个镜像了，下面这个检查会直接命中，不需要重新下载。
IMAGE="vllm/vllm-openai:v0.27.1-aarch64"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    ARCHIVE=""
    SEARCH_START=$SECONDS
    for dir in "$TARGET_HOME/LMM" "$TARGET_HOME/lmm-deploy" "$TARGET_HOME/Downloads" "$TARGET_HOME/桌面" "$TARGET_HOME/Desktop" /media/*/* /media/* /mnt/* /run/media/*/*; do
        [ -d "$dir" ] || continue
        if [ $((SECONDS - SEARCH_START)) -ge 15 ]; then
            echo "本地查找离线镜像包超过 15 秒，不再继续找，改走网络拉取" >&2
            break
        fi
        found="$(timeout 3 find "$dir" -maxdepth 3 -type f \( -iname "*vllm*.tar.zst" -o -iname "*vllm*.tar.gz" -o -iname "*vllm*.tar" \) 2>/dev/null | head -1)" || true
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
echo "ornith15 运行环境（docker + $IMAGE）已就绪，部署包已落地到 $VENDOR_DIR"
# 供"本地模型库"里"已安装引擎"删除功能使用：删除时直接 docker rmi 这个
# 镜像即可——跟 Z-Image 共用同一个基础镜像标签，如果两张卡片都装过，
# 删这张卡片的引擎不会影响 Z-Image（Z-Image 记录的是它自己 build 出来的
# 派生镜像，不是这同一个 vllm/vllm-openai 基础镜像）。
echo "LMM_ENGINE_IMAGE=$IMAGE"
