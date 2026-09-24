#!/bin/bash
# 这张卡片跟其它测试卡片不一样：研究团队交付的是一个完整的部署包
# （controller.py + model_assets.py + runtime/patch 共 23 个文件，SHA256SUMS
# 自校验），而不是六段可以随便改写的 shell。所以 LMM 的六个入口脚本一律做成
# 薄壳，只负责"把 LMM 的约定翻译成 megacube.sh 的子命令"，不复制它的业务逻辑
# ——包一升级，我们这边不用跟着改。
#
# 包被 LMM 当作卡片附属文件（assets）整包落到
# ~/.local/share/local-model-manager/cards/qwen38fn-nvfp4-single/ 下，
# 六个入口脚本也写在同一层，所以 controller.py 的 ROOT 就是这个目录。
# 包自己产生的 config.json / state/ / logs/ 不在 SHA256SUMS 里，
# materialize_assets 又只写不删，bundle_check 不会被我们的文件干扰。

set -euo pipefail

CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$CARD_DIR"

# 包的 configuration() 会拒绝末段是符号链接的路径，也只认绝对路径，所以这里
# 先 realpath -m 归一化（-m：路径还不存在也照样解析），免得用户把模型盘设成
# 一个软链就在校验那一步莫名其妙地失败。
MODEL_DIR="$(realpath -m "${LMM_MODELS_DIR:-$HOME/models}/Qwen3.8-Flash-Next-NVFP4")"
CACHE_DIR="$(realpath -m "$CARD_DIR/cache")"

[ -x ./megacube.sh ] || { echo "ERROR: 卡片包不完整，缺少 megacube.sh" >&2; exit 1; }

# 生成/纠正 config.json。
#
# 包的默认 model_dir 是 ROOT/models/…，也就是 LMM 的隐藏数据目录里——133GB
# 权重不该落在那儿。统一改指到用户在设置里选的模型盘，跟其它卡片同一个约定
# （$LMM_MODELS_DIR/<HF 仓库名>），用户手动放过去的权重天然能被复用。
#
# 每个入口都调一次而不是只在下载时调：存量安装的 config.json 可能是旧版本
# 写的，只在下载时纠正的话，后面 start 读到的仍是对不上的老路径。
# 端口/bind/startup_timeout 这些用户可能自己调过的字段一律保留。
lmm_sync_config() {
    mkdir -p "$MODEL_DIR" "$CACHE_DIR"
    ./megacube.sh init >/dev/null   # config.json 不存在时由包自己按默认值创建
    MODEL_DIR="$MODEL_DIR" CACHE_DIR="$CACHE_DIR" python3 - <<'PY'
import json, os, pathlib
path = pathlib.Path('config.json')
c = json.loads(path.read_text(encoding='utf-8'))
c['model_dir'] = os.environ['MODEL_DIR']
c['cache_dir'] = os.environ['CACHE_DIR']
tmp = path.with_suffix('.tmp')
tmp.write_text(json.dumps(c, indent=2) + '\n', encoding='utf-8')
tmp.replace(path)
PY
}

# 读 config.json 里的某个字段（配置已由 lmm_sync_config 纠正过）。
lmm_config_get() {
    python3 -c "import json,sys; print(json.load(open('config.json'))['$1'])"
}
