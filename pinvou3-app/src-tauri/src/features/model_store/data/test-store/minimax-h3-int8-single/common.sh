#!/bin/bash
# 六个入口脚本共用的一段壳。
#
# 这张卡片跟别的测试卡不一样：厂商交的**就是**六个 LMM 入口脚本——整个交付包是
# 照《LMM 模型卡片脚本规范》1.0 做的，探针、心跳、进度上报、weights.sha256 逐文件
# 校验、坏文件删除、幂等判断，规范里点名的东西它自己都做了。所以这一层刻意做得
# 很薄：原件一字不改放在 package/ 下，入口脚本只补三件属于 LMM 这一侧的事。
#
#   ① 补齐 🟡 多源变量。厂商按规范 3.2 写的，认 LMM_SOURCE_KIND /
#      LMM_SOURCE_BASE_URL，而且照规范 3.4「不得回退到硬编码地址」的要求，
#      拿不到就直接 die。可这两个变量 LMM 至今没实现（规范里标的就是 🟡），
#      原样跑起来 download_model.sh 第一句话就是「LMM 未提供 LMM_SOURCE_BASE_URL」。
#      所以由这一层临时顶班，见 lmm_fill_source_env。
#   ② 把交付包 sources/ 下那两个二进制 tar.gz 补回来。理由长，写在
#      install_engine.sh 开头。
#   ③ 心跳。厂商脚本自己有心跳，但它有几段 40GB 的 sha256sum 是闷着头算的，
#      在下载那一步会撞上 LMM 的 5 分钟静默上限，见 lmm_vendor_bg。
#
# **为什么厂商原件必须放 package/、不能平铺在卡片目录根上**：它的六个脚本跟
# LMM 的六个入口脚本**同名**。两者都落到同一个卡片目录里，入口脚本是后写的
# （见 ModelManager::prepare），平铺等于把厂商原件静默覆盖掉——脚本还在、内容
# 变了，报出来的错会指向一个完全无关的地方。test_store_data.rs 里那条
# assets_never_shadow_the_six_entry_scripts 测试就是防这个。
# 放进 package/ 之后还有个附带好处：厂商脚本自己算出来的 PKG_DIR 正好指向
# package/，它读 weights.sha256 / workflows/ / sources/ 的相对位置一点没变。
#
# 交付包里有三样东西**没有**放进 package/，各有各的原因：
#   - sources/*.tar.gz（两个，合计 18.4MB）：二进制，商店通道只能下发文本。
#     怎么处理见 install_engine.sh 开头那段。
#   - card.json：内置卡的元数据在 test_store_data.rs 里，那才是唯一的真相来源
#     （字段值逐条对着 card.json 抄的，见那边的注释）。留一份在这里，自检工具
#     会把 package/ 当成一个独立交付包去查，我们这一层的六个入口脚本反而没人看；
#     两份元数据也迟早会各改各的。
#   - SHA256SUMS：它列的是完整交付包的 14 个文件，包含上面那两个 tar.gz。搬过来
#     就是一份**永远校验不过**的清单，`sha256sum -c` 一跑就红，还会把真问题淹掉。
#     那两个归档的完整性锁没有丢，它在 sources/SOURCE_LOCKS.json 里，install_engine
#     每次取归档都按它校验。代价是自检工具会报一句「厂商包里没有 SHA256SUMS」
#     ——那句话对这个交付包是不准的，厂商给了，是我们没法原样带。

set -euo pipefail

CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PKG_DIR="$CARD_DIR/package"
cd "$CARD_DIR"

[ -f "$PKG_DIR/install_engine.sh" ] || {
    echo "ERROR: 卡片包不完整，缺少 package/install_engine.sh" >&2
    exit 1
}

# 下面三个路径是厂商六个脚本里写死的默认值，这里显式 export 一遍，保证六步之间
# 用的是同一套目录（用户自己 export 过就让路）。**值必须跟 package/*.sh 里的
# 默认值一字不差**，换包时要一起核对。
export LMM_ENGINE_DIR="${LMM_ENGINE_DIR:-$HOME/.local/share/lmm/engines/comfyui-minimax-h3-0.35.0-cu130}"
export LMM_DEPLOY_DIR="${LMM_DEPLOY_DIR:-$HOME/.local/share/lmm/deployments/minimax-h3-int8-single}"

# 权重目录。厂商的 download_model.sh / deploy.sh 都是
# "$LMM_MODELS_DIR/$LMM_MODEL_NAME" 这么算的，跟着它算，不要另起一套——两边
# 算出不同目录的后果是「下载说下好了、部署说找不到」。
#
# 这里不去覆盖 LMM_MODEL_NAME（虽然把它钉成卡片 id 能防住"用户改名后目录对不上"）：
# LMM 卸载后提示残留目录时猜的就是 ${LMM_MODELS_DIR}/<模型名>，钉死了那条提示
# 就永远猜不中。名字在下载时确定、之后存在数据库里不再变，风险比收益小。
MODEL_DIR="${LMM_MODELS_DIR:-$HOME/.local/share/lmm/models}/${LMM_MODEL_NAME:-minimax-h3-int8-single}"
export MODEL_DIR

# ---------------------------------------------------------------------------
# ① 下载源：替还没上线的多源机制顶班
# ---------------------------------------------------------------------------
# 规范 3.2 说得很清楚：「脚本不要自己做选择，也不要写死任何地址」，选源是 LMM
# 的职责。厂商照做了，所以它拿不到 LMM_SOURCE_BASE_URL 就罢工——这是对的，不该
# 去改它。缺的那一半在 LMM 这边，那就在 LMM 这一侧补：把用户在设置里填的 HF
# 镜像地址（🟢 LMM_HF_ENDPOINT，默认 hf-mirror）翻译成一个 hf_endpoint 源。
#
# 刻意**不**在这里实现 card.json 里声明的 offline-media（local_dir）源：那要扫
# U 盘、要按 search_globs 展开、要比可达性，全是规范 3.2 划给 LMM 的活。卡片自己
# 去做，等 LMM 真上线多源时就会两套逻辑打架。用户现在就想用离线目录的话，自己
# export LMM_SOURCE_KIND=local_dir 和 LMM_SOURCE_BASE_URL=<目录> 即可，下面会让路。
lmm_fill_source_env() {
    if [ -n "${LMM_SOURCE_BASE_URL:-}" ]; then
        export LMM_SOURCE_KIND="${LMM_SOURCE_KIND:-hf_endpoint}"
        echo "下载源：$LMM_SOURCE_BASE_URL（kind=$LMM_SOURCE_KIND，由 LMM 指定）"
        return 0
    fi
    if [ -z "${LMM_HF_ENDPOINT:-}" ]; then
        echo "ERROR: 没有可用的下载源。" >&2
        echo "       LMM 还没实现多源选择（规范 3.2 的 LMM_SOURCE_* 是 🟡），这张卡目前靠" >&2
        echo "       设置里的「HF 镜像地址」下载，而它是空的。请在设置里填一个镜像地址后重试。" >&2
        return 1
    fi
    export LMM_SOURCE_ID="hf-endpoint-from-settings"
    export LMM_SOURCE_KIND="hf_endpoint"
    export LMM_SOURCE_BASE_URL="$LMM_HF_ENDPOINT"
    echo "下载源：$LMM_SOURCE_BASE_URL（取自 LMM 设置里的 HF 镜像地址）"
}

# ---------------------------------------------------------------------------
# ③ 心跳
# ---------------------------------------------------------------------------
# 长时间不出声会被 LMM 判定为卡死并强杀（规范 6.2：装引擎/部署/启用 30 分钟，
# 下载/停用/卸载 5 分钟）。厂商脚本在下载、pip、复制运行时这些地方都打了心跳，
# 唯独几处 `sha256sum -c`（40.34GiB，判断"是不是已经下好了"）是一声不吭的。
# 把整个厂商脚本放后台、前台打心跳，比去改它的原件稳妥。
#
# 用法：lmm_vendor_bg <厂商脚本名> <心跳文案>
lmm_vendor_bg() {
    local name="$1" what="$2" pid start
    [ -f "$PKG_DIR/$name" ] || {
        echo "ERROR: 卡片包不完整，缺少 package/$name" >&2
        return 1
    }
    start=$SECONDS
    bash "$PKG_DIR/$name" &
    pid=$!
    # 先 sleep 再打，避免厂商脚本自己刚打完一行、我们又紧跟一行重复的噪音
    while kill -0 "$pid" 2>/dev/null; do
        sleep 15
        kill -0 "$pid" 2>/dev/null || break
        echo "$what…已用时 $((SECONDS - start))s"
    done
    wait "$pid"
}
