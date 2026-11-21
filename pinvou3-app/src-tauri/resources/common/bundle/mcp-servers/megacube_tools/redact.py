"""日志脱敏（r18 #4）：cmsd 侧 redact.rs 关键字扫描的 Python 移植。

商店直连路径（store_recovery）出站的 error 与事件行必须与 cmsd 侧
install.rs/api.rs 遵循同一脱敏边界——商店侧 run 日志可能回显脚本 env
注入的凭据行（LMM_HF_TOKEN 形态），原文会直接进入模型上下文。
实现保持 stdlib-only，算法与 Rust 版一一对应：逐行扫描关键字（大小写
不敏感，词边界防误伤 tokens/tokenizer），命中后把紧随其后的值掩成 ***；
Authorization 头贪婪掩到引号/逗号；独立 sk- 串保留前缀便于识别类型。
"""

KEYWORDS = (
    "authorization",
    "bearer",
    "api-key",
    "api_key",
    "apikey",
    "token",
    "secret",
    "password",
    "passwd",
)


def _is_value_char(char):
    return char.isascii() and (char.isalnum() or char in "-_.=/+")


def _next_secret(lower, start):
    """返回 (关键字起点, 值起点, 是否贪婪)；未命中返回 None。"""
    best = None
    for keyword in KEYWORDS:
        search = start
        while True:
            pos = lower.find(keyword, search)
            if pos < 0:
                break
            after = pos + len(keyword)
            # 词边界：关键字后紧跟字母/数字的是更长的词（tokens、tokenizer）
            if after < len(lower) and lower[after].isalnum():
                search = pos + 1
                continue
            val_start = after
            while val_start < len(lower) and lower[val_start] in ":= \"'":
                val_start += 1
            if val_start < len(lower) and _is_value_char(lower[val_start]):
                greedy = keyword == "authorization"
                if best is None or best[0] > pos:
                    best = (pos, val_start, greedy)
                break
            search = after
    # sk- 风格独立匹配（要求词边界，避免 task-/desk- 被误伤）；
    # 保留 sk- 前缀便于识别密钥类型。
    search = start
    while True:
        pos = lower.find("sk-", search)
        if pos < 0:
            break
        if pos == 0 or not _is_value_char(lower[pos - 1]):
            if best is None or best[0] > pos:
                best = (pos, pos + 3, False)
            break
        search = pos + 1
    return best


def redact_line(line):
    out = list(line)
    start = 0
    for _ in range(64):  # 每行敏感串数量的 sanity 上限
        found = _next_secret("".join(out).lower(), start)
        if found is None:
            break
        kw_pos, val_start, greedy = found
        end = val_start
        index = val_start
        while index < len(out):
            char = out[index]
            if _is_value_char(char) or (greedy and char == " "):
                end = index + 1
                index += 1
            else:
                break
        while end > val_start and out[end - 1] == " ":
            end -= 1
        if end > val_start:
            out[val_start:end] = "*"*3
        start = kw_pos + 1
    return "".join(out)


def redact_text(text):
    return "\n".join(redact_line(line) for line in text.splitlines())
