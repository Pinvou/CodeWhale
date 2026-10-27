#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""校验文档里的「某文件的某符号」引用真的指得到东西。

**为什么有这个脚本。** `docs/模型商店-框架与移植基线.md` 原先写的是
`lifecycle.rs:760` 这种「文件:行」引用。行号会漂，而漂掉之后没有任何东西会报错——
读者点过去看到一行不相干的代码，只会以为自己读错了。2026-09-23 的评审抽查 12 处、
约 10 处漂了 +40~+134 行，其中一处的行号已经超出文件长度。

所以引用改成了「文件 + 符号名」：符号跟着代码走，改名会被这个脚本当场抓住，而
移动位置不再是问题。行号一个都不许再写回去——写了就会漂。

**只认「文件在前、符号在后」的写法**：`` `foo.rs` 的 `bar` ``、`` `foo.rs` 的
`bar()` ``（带不带反引号都认）。英文语序 "bar() of foo.rs" 不是锚点——只有文件在前的
写法才能确定去哪个文件里找符号。裸的 "foo.rs bar"（没有「的」也没有调用括号）是
普通行文，不是锚点，check() 会跳过。

**扩展名覆盖**。成对引用与行号引用识别
`.rs .js .jsx .mjs .json .ts .tsx .sh .py`（`.json` 排在 `.js` 的备选之后，
靠尾部 `\b` 与 `.js` 区分，不会只匹配一半）。之外的扩展名不算锚点。

**默认范围**。不给参数时检查 DEFAULT_DOCS 清单：两篇 recovery 文档（还没有锚点，
作为锚点文风的存放处保留）加上今天就能通过的带锚点文档，让默认运行真的校验到
引用，而不是空转通过。行号风格与时点记录类文档（tool-governance、system-prompt
的历史章节等）刻意不在范围内——收录它们需要先把引用改写成锚点文风。

用法：python scripts/check-doc-anchors.py [文档路径...]
路径相对仓库根。退出码非 0 表示有引用失效。
"""
import argparse
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_DOCS = [
    "docs/模型商店-框架与移植基线.md",
    "docs/chat-steer-recovery.md",
    "docs/workspace-rebind-recovery.md",
    "docs/capability-governance.md",
    "docs/记忆功能-架构设计.md",
    "docs/底座升级验收清单.md",
]
SKIP_DIRS = {".git", "node_modules", "target", "dist", ".venv", "__pycache__"}
# 同名文件按「模型商店那棵树优先」消歧——这份文档只讲这个模块。
PREFER = "features/model_store"

# `bar()` of `foo.rs` / bar() of foo.rs —— 只认「文件在前」的写法（见模块 docstring）；
# 裸的 "foo.rs bar" 是普通行文，由 check() 跳过。尾部 \b 让 `.json` 不会被当成 `.js` 的一半。
PAIR = re.compile(
    r"`?([A-Za-z_][A-Za-z0-9_./-]*\.(?:rs|js|jsx|mjs|json|ts|tsx|sh|py))\b`?\s*(的?)\s*`?"
    r"([A-Za-z_][A-Za-z0-9_:]*)\s*(\(\))?`?"
)
# 行号引用一律视为退化，直接报错。
LINEREF = re.compile(
    r"[A-Za-z_][A-Za-z0-9_./-]*\.(?:rs|js|jsx|mjs|json|ts|tsx|sh|py):\d+"
)

_resolved = {}


def resolve(rel):
    """把文档里写的短路径解析成仓库里的真实路径。"""
    if rel in _resolved:
        return _resolved[rel]
    got = rel if os.path.isfile(os.path.join(ROOT, rel)) else None
    if got is None:
        name = rel.split("/")[-1]
        hits = []
        for dp, dn, fn in os.walk(ROOT):
            dn[:] = [d for d in dn if d not in SKIP_DIRS]
            if name in fn:
                p = os.path.relpath(os.path.join(dp, name), ROOT).replace("\\", "/")
                if rel in p or p.endswith("/" + rel):
                    hits.append(p)
        hits.sort(key=lambda p: (PREFER not in p, len(p)))
        got = hits[0] if hits else None
    _resolved[rel] = got
    return got


_src = {}


def body(path):
    if path not in _src:
        with open(os.path.join(ROOT, path), encoding="utf-8", errors="replace") as f:
            _src[path] = f.read()
    return _src[path]


def check(doc):
    bad = []
    with open(os.path.join(ROOT, doc), encoding="utf-8", errors="replace") as f:
        lines = f.read().splitlines()
    for i, line in enumerate(lines, 1):
        for m in LINEREF.finditer(line):
            bad.append(f"{doc}:{i} 写了行号引用 {m.group(0)}——行号会漂，请改成「文件 + 符号名」")
        for m in PAIR.finditer(line):
            rel, sym = m.group(1), m.group(3)
            if not m.group(2) and not m.group(4):
                # 既没有「的」也没有调用括号：普通行文，不是锚点。
                continue
            real = resolve(rel)
            if real is None:
                bad.append(f"{doc}:{i} 引用的文件不存在：{rel}")
                continue
            if sym not in body(real):
                bad.append(f"{doc}:{i} {real} 里找不到 `{sym}`（符号被改名或删了？）")
    return bad


def main():
    parser = argparse.ArgumentParser(
        description="校验文档锚点（文件 + 符号引用）在本仓库是否指得到真东西。"
    )
    parser.add_argument(
        "docs",
        nargs="*",
        help="要检查的文档路径，相对仓库根（缺省用本脚本的 DEFAULT_DOCS 清单）",
    )
    args = parser.parse_args()
    docs = args.docs or DEFAULT_DOCS
    bad = []
    for d in docs:
        if not os.path.isfile(os.path.join(ROOT, d)):
            bad.append(f"文档不存在：{d}")
            continue
        bad += check(d)
    if bad:
        print("文档锚点检查未通过：")
        for b in bad:
            print("  -", b)
        return 1
    print(f"文档锚点检查通过（{len(docs)} 份文档）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
