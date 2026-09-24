#!/bin/bash
# 下载权重（43,317,497,012 字节 / 约 40.34GiB）到 $LMM_MODELS_DIR/<模型名>。
#
# 厂商的 package/download_model.sh 已经按规范 4.2 做全了：
#   - 断点续传（对着同一个 --local-dir 重跑 hf download，已下好的不会白费）
#   - 每 15 秒 LMM_PROGRESS_BYTES=已下载/总量，分母直接用 LMM_MODEL_SIZE_BYTES，
#     算法就是规范里那段（成品 + *.incomplete，排除 .cache，夹到总量以内）
#   - 下完按 weights.sha256 逐个文件校验，对不上的**删掉**再非 0 退出
#   - 成功后清 *.incomplete
#   - hf download --max-workers 4，正是规范建议的并发
# 一件都不用重做。这一层补两件事。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# ① 下载源。厂商认的是 🟡 LMM_SOURCE_KIND / LMM_SOURCE_BASE_URL，LMM 还不传，
#    拿不到它会直接 die（规范 3.4 要求的，不该改它）。翻译见 common.sh。
lmm_fill_source_env

TOTAL="${LMM_MODEL_SIZE_BYTES:-43317497012}"

# 厂商脚本开头写死了一句 [ "$TOTAL" = "43317497012" ] || die。真撞上了（比如有人
# 改了 test_store_data.rs 里的 size_bytes 却没同步 card.json），用户看到的是一句
# 「LMM_MODEL_SIZE_BYTES 应为 43317497012」——不知道该去哪儿改。这里先拦一道。
if [ "$TOTAL" != "43317497012" ]; then
    echo "ERROR: 卡片登记的权重总量（$TOTAL 字节）跟交付包锁定的 43317497012 不一致，交付包会拒绝下载。" >&2
    echo "       请让 test_store_data.rs 里这张卡的 size_bytes 跟交付包 card.json 里的值保持一致。" >&2
    exit 1
fi

# ② 心跳。厂商脚本在**开始下载之前**会先整个跑一遍 sha256sum -c 来判断"是不是
#    已经下好了"——40.34GiB，而且 >/dev/null 2>&1 吞掉了输出，是一段彻底的静默。
#    GB10 上就算按 200MB/s 算也要 200 秒，慢一点就会撞上 download_model 的 5 分钟
#    静默上限被判卡死，而它其实好好地在算哈希。所以整个厂商脚本放后台、前台打心跳。
lmm_vendor_bg download_model.sh "正在下载/校验 MiniMax-H3 权重"

# 成功返回后还剩的 *.incomplete 按定义就是垃圾（所有文件都已下完并移走），不清掉
# 会一直白占磁盘。厂商脚本在它自己的两条成功路径上都删过一次，这里再兜一次底：
# 它只扫 $MODEL_DIR，而 loras/ 和 upscale_models/ 是用 --local-dir 单独下的子目录，
# 各自还会有一个 .cache/huggingface——都在 $MODEL_DIR 底下，一起扫掉。
find "$MODEL_DIR" -name '*.incomplete' -delete 2>/dev/null || true

# 把进度条钉到 100%。厂商脚本末尾也打了一次，内容一样；LMM 取最后一条
# （extract_marker 是后到覆盖），重复没有副作用。留着是因为"下载成功"和"进度满格"
# 应该由同一个 exit 0 保证，不该依赖被包在里面的那个脚本。
echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
echo "权重就绪并通过 weights.sha256 校验：$MODEL_DIR"
