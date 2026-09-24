#!/bin/bash
# 装引擎 = 备好 Python 3.12 + PyTorch 2.13.0+cu130 运行时、展开 ComfyUI 0.35.0 和
# MiniMax-H3 Turbo 节点、装依赖、汉化节点标签、跑一次 GB10 CUDA 自检。不下权重。
#
# 厂商的 package/install_engine.sh 已经把这件事做全了（只读探针、每 15 秒心跳、
# aarch64/GB10 前置检查、源码归档 sha256 校验、装完再自检一遍），我们不重做。
# 这一层只解决一个它自己解决不了的问题：
#
#   **它要的两个源码归档是二进制，LMM 的商店通道送不过来。**
#
# 交付包 sources/ 下有两个 tar.gz：
#   ComfyUI-source.tar.gz                    12.3MB（ComfyUI 0.35.0 源码树）
#   ComfyUI-MiniMax-H3-Turbo-source.tar.gz    6.1MB（Turbo 自定义节点，里面还包着
#                                            h3_silu_temb_grid.safetensors 5.5MB
#                                            和 node.zip 3.1MB 两个二进制）
# 而卡片附属文件在客户端是 BTreeMap<String, String>、按 UTF-8 文本读取的（规范
# 1.4：「包内只能是文本文件……二进制根本过不来」），内置卡更是靠 include_str!
# 编译期嵌入，连读都读不进来。
#
# 三条路都量过：
#
#   1) 像 z-image-turbo 那样 base64 内联进脚本。技术上能跑（那张卡就是这么干的），
#      但 18.4MB 变成 24.6MB base64，而整个仓库现在的 git pack 才 5.89MB——等于
#      为一张**我们还没验过**的测试卡把仓库和安装包体积翻三倍，每次开应用还要
#      把这 24.6MB 重写一遍到磁盘（LocalStoreClient 每次构造都 materialize）。
#      而且这种写法我们自己在规范 2.0 大纲里已经定性为「不推荐，应改走
#      bundled_assets」。
#
#   2) 改成运行时下载。办不到。两个归档都是厂商用 `git archive HEAD` 重新打的
#      （顶层目录分别叫 ComfyUI-HEAD 和 ComfyUI-MiniMax-H3-Turbo-HEAD），公网上
#      没有任何地址能给出同样的字节；换成 GitHub 的 tag 包就等于把
#      SOURCE_LOCKS.json 里那两个 sha256 锁扔掉，而那个锁正是厂商为了「避免部署时
#      拉取浮动分支」特意加的。Turbo 节点那份更是只有厂商手上有。给一张没在真机上
#      验过的卡编一个我们自己都没访问过的 URL，等于把失败推迟到用户现场。
#
#   3) 就是下面这条：从设备上那份交付包里取，取完按厂商自己的 sha256 锁校验。
#      不改原件、不丢完整性保证、不往仓库里塞二进制；代价是装引擎前设备上得有
#      那个 zip（跟 40GB 权重一起从 U 盘拷进来是这类设备的常态）。
#
# 找的顺序：package/sources/ 里已经有了（将来商店真支持二进制附件时自动走这条）
# → 设备上自动找交付包（zip / tar.gz / 已解压目录）→ 都没有就问用户要一次路径。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# 只读探针：整段交给厂商脚本。它的 engine_ready() 只查文件在不在、问一次 torch
# 版本和 GPU 能力，没有任何安装动作，正合规范 4.1 ①。
#
# 探针这一步**不能**顺手去补源码归档：探针是每次下载前都要跑一次的，而补归档要
# 扫磁盘、可能还要弹框问路径——引擎装没装跟归档在不在没有关系，装好了的机器上
# 那两个归档早就用完了。
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    exec bash "$PKG_DIR/install_engine.sh"
fi

SOURCES_DIR="$PKG_DIR/sources"
LOCKS="$SOURCES_DIR/SOURCE_LOCKS.json"
# 归档文件名 : SOURCE_LOCKS.json 里的键名
ARCHIVES="ComfyUI-source.tar.gz:ComfyUI_source_archive
ComfyUI-MiniMax-H3-Turbo-source.tar.gz:ComfyUI_MiniMax_H3_Turbo_source_archive"

need_python3() {
    command -v python3 >/dev/null 2>&1 && return 0
    echo "ERROR: 设备上没有 python3。" >&2
    echo "       这一步要用它读 SOURCE_LOCKS.json、并从交付包（zip/tar.gz）里取出源码归档。" >&2
    echo "       这张卡本来就要 Python 3.12 才能装引擎，请先确认系统 python 可用。" >&2
    return 1
}

# 期望的 sha256 一律从厂商自己的 SOURCE_LOCKS.json 里读，**不在这里另抄一份**：
# 抄了之后换一版交付包，两边就会不一致，而且是那种"校验通过了但校验的是旧值"
# 的不一致。
lock_sha() {
    LOCK_FILE="$LOCKS" LOCK_KEY="$1" python3 -c '
import json, os, sys
with open(os.environ["LOCK_FILE"], encoding="utf-8") as f:
    locks = json.load(f)
entry = locks.get(os.environ["LOCK_KEY"]) or {}
digest = entry.get("sha256") or ""
if len(digest) != 64:
    sys.exit(1)
print(digest)
'
}

sources_ok() {
    local entry name key want got
    while IFS= read -r entry; do
        [ -n "$entry" ] || continue
        name="${entry%%:*}"
        key="${entry##*:}"
        [ -f "$SOURCES_DIR/$name" ] || return 1
        want="$(lock_sha "$key")" || return 1
        got="$(sha256sum "$SOURCES_DIR/$name" | cut -d' ' -f1)"
        [ "$got" = "$want" ] || return 1
    done <<< "$ARCHIVES"
    return 0
}

# 从一份交付包里把 sources/ 下那两个归档取出来。src 可以是 .zip、.tar.gz，
# 也可以是已经解压好的目录。
#
# 解压用 python 的 zipfile/tarfile，不用 unzip/tar：WSL 和不少精简过的 Linux
# 上压根没装 unzip（我们自己做这张卡的时候就撞上了），而 python3 这一步反正
# 也要用。只按 basename 写文件，不照搬包里的路径——包来自何处不好说，别让它
# 有机会往任意位置写。
take_sources_from() {
    local src="$1"
    if [ -d "$src" ]; then
        local entry name
        while IFS= read -r entry; do
            [ -n "$entry" ] || continue
            name="${entry%%:*}"
            [ -f "$src/sources/$name" ] || return 1
            cp -f "$src/sources/$name" "$SOURCES_DIR/$name" || return 1
        done <<< "$ARCHIVES"
        return 0
    fi
    [ -f "$src" ] || return 1
    SRC="$src" DST="$SOURCES_DIR" python3 -c '
import os, sys, tarfile, zipfile
src, dst = os.environ["SRC"], os.environ["DST"]
want = {"ComfyUI-source.tar.gz", "ComfyUI-MiniMax-H3-Turbo-source.tar.gz"}
got = set()

def emit(name, fh):
    with open(os.path.join(dst, name), "wb") as out:
        while True:
            chunk = fh.read(1 << 20)
            if not chunk:
                break
            out.write(chunk)
    got.add(name)

if zipfile.is_zipfile(src):
    with zipfile.ZipFile(src) as z:
        for info in z.infolist():
            parts = info.filename.split("/")
            if len(parts) >= 2 and parts[-2] == "sources" and parts[-1] in want:
                with z.open(info) as fh:
                    emit(parts[-1], fh)
else:
    with tarfile.open(src) as t:
        for member in t.getmembers():
            parts = member.name.split("/")
            if member.isfile() and len(parts) >= 2 and parts[-2] == "sources" and parts[-1] in want:
                fh = t.extractfile(member)
                if fh is not None:
                    emit(parts[-1], fh)
sys.exit(0 if got == want else 1)
'
}

# 在设备上找交付包。每个目录都套 timeout，总时长也封顶：某个挂载点很慢（网络
# 共享、没插好的 U 盘）时不能把整步拖死在这里。
find_delivery() {
    local dir hit start=$SECONDS
    for dir in "${LMM_STAGING_DIR:-}" "$HOME/Downloads" "$HOME/桌面" "$HOME/Desktop" \
        "$HOME" /media/*/* /media/* /mnt/* /run/media/*/*; do
        [ -n "$dir" ] && [ -d "$dir" ] || continue
        if [ $((SECONDS - start)) -ge 20 ]; then
            echo "在本地找交付包已超过 20 秒，不再继续找。" >&2
            break
        fi
        hit="$(timeout 5 find "$dir" -maxdepth 3 -type f \
            \( -name 'minimax-h3-int8-single*.zip' -o -name 'minimax-h3-int8-single*.tar.gz' \) \
            2>/dev/null | head -n 1)" || true
        if [ -n "$hit" ]; then
            printf '%s\n' "$hit"
            return 0
        fi
        # 已经解压好的交付目录：认 sources/ComfyUI-source.tar.gz 这个特征文件
        hit="$(timeout 5 find "$dir" -maxdepth 4 -type f \
            -path '*/sources/ComfyUI-source.tar.gz' 2>/dev/null | head -n 1)" || true
        if [ -n "$hit" ]; then
            printf '%s\n' "$(dirname "$(dirname "$hit")")"
            return 0
        fi
    done
    return 1
}

mkdir -p "$SOURCES_DIR"
[ -f "$LOCKS" ] || {
    echo "ERROR: 卡片包不完整，缺少 package/sources/SOURCE_LOCKS.json（两个源码归档的 sha256 锁在里面）" >&2
    exit 1
}

if sources_ok; then
    echo "源码归档已就位并通过 SOURCE_LOCKS.json 校验，跳过这一步。"
else
    need_python3
    echo "这张卡的 ComfyUI 与 Turbo 节点源码是两个二进制 tar.gz，商店只能下发文本，"
    echo "需要从交付包里取。正在设备上查找 minimax-h3-int8-single 交付包…"
    delivery=""
    if ! delivery="$(find_delivery)"; then
        delivery=""
    fi
    if [ -z "$delivery" ]; then
        # **刻意不弹输入框。**
        #
        # 这里原来是 `LMM_PROMPT=text|…` + `read`，2026-09-17 真机上把整个任务挂死了
        # 3 小时 13 分：用户界面上没看到输入框，脚本就一直阻塞在 read 上；而
        # max_concurrent_downloads=1，这一个卡住把后面排队的下载全堵死了。
        #
        # 两个理由决定改成直接失败：
        #   1. 让用户在任务卡片里手打一个文件系统路径本来就是坏体验——他得先去文件
        #      管理器找到那个 zip、复制路径、再粘回来，中间任何一步断了都只能干等。
        #   2. 更要命的是**失败模式**：输入框万一没渲染出来（真机上就是如此），
        #      用户看到的是一个永远转圈、没有进度也没有报错的任务。而把话说清楚、
        #      让他把文件放到一个固定位置，是他一眼能看懂、也能自己验证的动作。
        #
        # 所以：说清楚缺什么、放哪、然后退出。重跑时 find_delivery 会自动找到。
        echo "ERROR: 设备上没有找到 MiniMax-H3 的交付包，装引擎无法继续。" >&2
        echo "" >&2
        echo "  这张卡的 ComfyUI 与 Turbo 节点源码是两个二进制 tar.gz（共约 18MB），" >&2
        echo "  而模型商店只能下发文本文件，所以需要从研究团队的交付包里取一次，取完就不用了。" >&2
        echo "" >&2
        echo "  请把 minimax-h3-int8-single-v1.0.0.zip 放到下面任意一个位置，然后重新点「下载」：" >&2
        echo "    ${LMM_STAGING_DIR:-$HOME/.local/share/local-model-manager/staging}/    ← 推荐" >&2
        echo "    $HOME/Downloads/" >&2
        echo "    U 盘挂载点（/media/* 或 /mnt/*）" >&2
        echo "" >&2
        echo "  已解压的目录也可以，只要里面有 sources/ComfyUI-source.tar.gz。" >&2
        exit 1
    fi
    echo "从交付包取源码归档：$delivery"
    take_sources_from "$delivery" || {
        echo "ERROR: 没能从 $delivery 里取到 sources/ 下的两个源码归档。" >&2
        echo "       需要的是研究团队交付的 minimax-h3-int8-single-v1.0.0.zip（约 18MB），" >&2
        echo "       包内 sources/ComfyUI-source.tar.gz 和 sources/ComfyUI-MiniMax-H3-Turbo-source.tar.gz。" >&2
        exit 1
    }
    if ! sources_ok; then
        rm -f "$SOURCES_DIR/ComfyUI-source.tar.gz" \
              "$SOURCES_DIR/ComfyUI-MiniMax-H3-Turbo-source.tar.gz"
        echo "ERROR: 取到的源码归档跟 SOURCE_LOCKS.json 里的 sha256 对不上，已删除。" >&2
        echo "       多半是交付包版本不对（这张卡锁的是 card_version 1.0.0）或文件损坏，" >&2
        echo "       换一份完整的交付包再试。" >&2
        exit 1
    fi
    echo "源码归档校验通过（sha256 取自厂商的 SOURCE_LOCKS.json）。"
fi

# 剩下的全是厂商的活。exec 掉，让它的输出、退出码、信号处理都原样归它。
exec bash "$PKG_DIR/install_engine.sh"
