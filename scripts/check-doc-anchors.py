#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""校验文档里的「某文件的某符号」引用真的指得到东西。

**为什么有这个脚本。** `docs/模型商店-框架与移植基线.md` 原先写的是
`lifecycle.rs:760` 这种「文件:行」引用。行号会漂，而漂掉之后没有任何东西会报错——
读者点过去看到一行不相干的代码，只会以为自己读错了。2026-09-23 的评审抽查 12 处、
约 10 处漂了 +40~+134 行，其中一处的行号已经超出文件长度。

所以引用改成了「文件 + 符号名」：符号跟着代码走，改名会被这个脚本当场抓住，而
移动位置不再是问题。行号一个都不许再写回去——写了就会漂。

用法：python scripts/check-doc-anchors.py [文档路径...]
不给参数时检查默认清单。退出码非 0 表示有引用失效。
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_DOCS = ["docs/模型商店-框架与移植基线.md"]
SKIP_DIRS = {".git", "node_modules", "target", "dist", ".venv", "__pycache__"}
# 同名文件按「模型商店那棵树优先」消歧——这份文档只讲这个模块。
PREFER = "features/model_store"

# `foo.rs` 的 `bar()` / foo.rs 的 bar()，两种写法都认。
PAIR = re.compile(
    r"`?([A-Za-z_][A-Za-z0-9_./-]*\.(?:rs|js|jsx|mjs|py))`?\s*(?:的|里的?)\s*`?"
    r"([A-Za-z_][A-Za-z0-9_:]*)\s*(?:\(\))?`?"
)
# 行号引用一律视为退化，直接报错。
LINEREF = re.compile(r"[A-Za-z_][A-Za-z0-9_./-]*\.(?:rs|js|jsx|mjs|sh|py):\d+")

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
    with open(os.path.join(ROOT, doc), encoding="utf-8") as f:
        lines = f.read().splitlines()
    for i, line in enumerate(lines, 1):
        for m in LINEREF.finditer(line):
            bad.append(f"{doc}:{i} 写了行号引用 {m.group(0)}——行号会漂，请改成「文件 + 符号名」")
        for m in PAIR.finditer(line):
            rel, sym = m.group(1), m.group(2)
            real = resolve(rel)
            if real is None:
                bad.append(f"{doc}:{i} 引用的文件不存在：{rel}")
                continue
            if sym not in body(real):
                bad.append(f"{doc}:{i} {real} 里找不到 `{sym}`（符号被改名或删了？）")
    return bad


def main():
    docs = sys.argv[1:] or DEFAULT_DOCS
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
