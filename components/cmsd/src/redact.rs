//! 日志脱敏（T8）：屏蔽 token / key / 口令类敏感串。
//!
//! 无正则依赖，逐行迭代扫描关键字（大小写不敏感），命中后把紧随其后的值
//! 掩成 ***。覆盖：Authorization/Bearer 头、api-key/api_key/apikey/token/
//! secret/password 键值、--api-key 命令行参数、sk- 开头的 OpenAI 风格串。

const KEYWORDS: &[&str] = &[
    "authorization",
    "bearer",
    "api-key",
    "api_key",
    "apikey",
    "token",
    "secret",
    "password",
    "passwd",
];

fn is_value_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '=' | '+' | '/')
}

/// r23 R1/R2：长度保持的逐字符折叠。完整 Unicode 小写会变长
/// （U+0130 → "i" + U+0307），会破坏折叠串与原串的字节偏移对齐；
/// ASCII 逐字符小写，另加两个单码点特例（U+212A KELVIN SIGN→k、
/// U+0130→i），与 Python 版 _fold 一一对应——两侧关键字命中域一致。
fn fold_char(c: char) -> char {
    if c.is_ascii() {
        c.to_ascii_lowercase()
    } else {
        match c {
            '\u{212a}' => 'k', // KELVIN SIGN
            '\u{0130}' => 'i', // LATIN CAPITAL LETTER I WITH DOT ABOVE
            other => other,
        }
    }
}

/// 返回（折叠串, 偏移映射）：map[i] = 折叠串字节 i 在原串中的字节偏移。
/// 折叠逐字符 1:1；U+212A/U+0130 折成单字节 ASCII 造成的字节长度变化由
/// map 承接，折叠域命中的下标经 map 回到原串域。
fn fold_aligned(text: &str) -> (String, Vec<usize>) {
    let mut folded = String::with_capacity(text.len());
    let mut map = Vec::with_capacity(text.len() + 1);
    for (offset, c) in text.char_indices() {
        let f = fold_char(c);
        // map 条目数 = 折叠后字符的字节长（与折叠串下标一一对应）
        for _ in 0..f.len_utf8() {
            map.push(offset);
        }
        folded.push(f);
    }
    map.push(text.len());
    (folded, map)
}

/// 在 lower 里从 from 开始找下一个命中，返回 (关键字起点, 值起点, 是否贪婪)。
/// 贪婪 = Authorization 头：值可能含空格（"Bearer x"），掩到引号/逗号/括号为止。
fn next_secret(lower: &str, from: usize) -> Option<(usize, usize, bool)> {
    let mut best: Option<(usize, usize, bool)> = None;
    for kw in KEYWORDS {
        let mut search = from;
        while let Some(pos) = lower[search..].find(kw) {
            let abs = search + pos;
            let after = abs + kw.len();
            // 词边界：关键字后紧跟字母/数字的是更长的词（tokens、tokenizer），跳过
            let next_char = lower[after..].chars().next();
            if let Some(c) = next_char {
                if c.is_ascii_alphanumeric() {
                    search = abs + 1;
                    continue;
                }
            }
            // 值起点：跳过 :、=、空格、引号
            let mut val_start = after;
            for (i, c) in lower[after..].char_indices() {
                if matches!(c, ':' | '=' | ' ' | '"' | '\'') {
                    val_start = after + i + 1;
                } else {
                    break;
                }
            }
            if val_start < lower.len()
                && lower[val_start..]
                    .chars()
                    .next()
                    .map(is_value_char)
                    .unwrap_or(false)
            {
                let greedy = *kw == "authorization";
                best = match best {
                    Some((b, _, _)) if b <= abs => best,
                    _ => Some((abs, val_start, greedy)),
                };
                break;
            }
            search = after;
        }
    }
    // sk- 风格独立匹配（要求词边界，避免 task-/desk- 这类词被误伤）；
    // 保留 sk- 前缀便于识别密钥类型
    {
        let mut search = from;
        while let Some(pos) = lower[search..].find("sk-") {
            let abs = search + pos;
            let at_boundary = abs == 0
                || !lower[..abs]
                    .chars()
                    .next_back()
                    .map(is_value_char)
                    .unwrap_or(false);
            if at_boundary {
                best = match best {
                    Some((b, _, _)) if b <= abs => best,
                    _ => Some((abs, abs + 3, false)),
                };
                break;
            }
            search = abs + 1;
        }
    }
    best
}

pub fn redact_line(line: &str) -> String {
    let mut out = line.to_string();
    let mut from = 0;
    for _ in 0..64 {
        // 每行敏感串数量的 sanity 上限
        let (lower, map) = fold_aligned(&out);
        let Some((kw_pos_folded, val_start_folded, greedy)) = next_secret(&lower, from) else {
            break;
        };
        // 折叠域命中点经映射回到原串域（Kelvin/İ 折成单字节后偏移会前移）；
        // from 仍留在折叠域，供下一轮 next_secret 继续使用
        let kw_pos = map[kw_pos_folded];
        let val_start = map[val_start_folded];
        // 掩码：普通情形掩连续值字符；贪婪情形连空格一起掩到引号/逗号/括号
        let mut end = val_start;
        for (i, c) in out[val_start..].char_indices() {
            let take = is_value_char(c) || (greedy && c == ' ');
            if take {
                end = val_start + i + c.len_utf8();
            } else {
                break;
            }
        }
        // 贪婪模式尾部空格不掩
        while end > val_start && out[..end].ends_with(' ') {
            end -= 1;
        }
        if end > val_start {
            out.replace_range(val_start..end, "***");
        }
        from = kw_pos_folded + 1;
    }
    out
}

pub fn redact_text(text: &str) -> String {
    text.lines().map(redact_line).collect::<Vec<_>>().join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn masks_bearer() {
        // Authorization 是贪婪掩码：整个头值（含 Bearer scheme）都掩掉，token 不漏
        assert_eq!(
            redact_line("Authorization: Bearer abcdef123456"),
            "Authorization: ***"
        );
    }

    #[test]
    fn masks_api_key_arg() {
        assert_eq!(
            redact_line("tachyon-serve --api-key mysecretkey123 --port 18080"),
            "tachyon-serve --api-key *** --port 18080"
        );
    }

    #[test]
    fn masks_json_kv() {
        assert_eq!(
            redact_line(r#"cfg={"api_key":"xyz987","port":1}"#),
            r#"cfg={"api_key":"***","port":1}"#
        );
    }

    #[test]
    fn masks_sk_style() {
        assert_eq!(
            redact_line("key sk-abcDEF123ghi 已失效"),
            "key sk-*** 已失效"
        );
    }

    #[test]
    fn keeps_normal_lines() {
        let line = "2026-10-12T08:00:00 engine ready, model Ornith-1.5-35B-A3B-NVFP4 loaded";
        assert_eq!(redact_line(line), line);
    }

    #[test]
    fn masks_case_insensitive() {
        assert_eq!(redact_line("TOKEN=tok123"), "TOKEN=***");
        assert_eq!(redact_line("Password: hunter2"), "Password: ***");
    }

    #[test]
    fn masks_multiple_in_one_line() {
        assert_eq!(
            redact_line("token=a1b2 secret: c3d4"),
            "token=*** secret: ***"
        );
    }

    #[test]
    fn keyword_without_value_untouched() {
        // “token” 后面没有值字符时不误伤正文
        assert_eq!(redact_line("token 桶已空"), "token 桶已空");
    }

    #[test]
    fn longer_words_untouched() {
        // tokens / tokenizer 这类更长的词不是密钥（V2 真机日志里踩过）
        assert_eq!(
            redact_line("KV 32,768 tokens, fp8, explicit"),
            "KV 32,768 tokens, fp8, explicit"
        );
        assert_eq!(
            redact_line("loading tokenizer.json"),
            "loading tokenizer.json"
        );
    }

    #[test]
    fn sk_inside_word_untouched() {
        // task-/desk- 里的 sk- 不是密钥
        assert_eq!(redact_line("task-123 done"), "task-123 done");
        assert_eq!(redact_line("desktop"), "desktop");
    }

    /// r23 R3：与 pinvou3-app/tests/megacube_redact_parity_test.py 共享的
    /// 向量表逐字镜像——Python 侧 (输入, 期望) 完全一致，任何一侧行为
    /// 漂移两边同时红，钉住「算法与 Python 版一一对应」的模块声明。
    /// （r22 R1/R2：İ 前缀偏移漂移 / U+212A Kelvin 关键字单侧漏掩。）
    #[test]
    fn parity_vectors_pinned() {
        let vectors: &[(&str, &str)] = &[
            ("Authorization: Bearer abcdef123456", "Authorization: ***"),
            ("Authorization:Bearer", "Authorization:***"),
            (
                "tachyon-serve --api-key mysecretkey123 --port 18080",
                "tachyon-serve --api-key *** --port 18080",
            ),
            (
                r#"cfg={"api_key":"xyz987","port":1}"#,
                r#"cfg={"api_key":"***","port":1}"#,
            ),
            ("key sk-abcDEF123ghi invalid", "key sk-*** invalid"),
            ("TOKEN=tok123", "TOKEN=***"),
            ("Password: hunter2", "Password: ***"),
            ("token=a1b2 secret: c3d4", "token=*** secret: ***"),
            (
                "LMM_HF_TOKEN=hf_ABCDEF1234567890abcdefghij",
                "LMM_HF_TOKEN=***",
            ),
            // r23 R1：İ（U+0130）lower 变长曾使掩码落点漂移直至整段泄漏
            ("\u{0130} token=q", "\u{0130} token=***"),
            // r23 R2：U+212A KELVIN SIGN 关键字双端均可命中
            ("TO\u{212a}EN=hf_SECRETVALUE", "TO\u{212a}EN=***"),
            ("API\u{212a}EY=q7z9x", "API\u{212a}EY=***"),
            ("s\u{212a}-abc123xyz", "s\u{212a}-***"),
            // 正文不误伤：更长的词 / 无值关键字 / sk- 词内
            ("token 桶已空", "token 桶已空"),
            (
                "KV 32,768 tokens, fp8, explicit",
                "KV 32,768 tokens, fp8, explicit",
            ),
            ("loading tokenizer.json", "loading tokenizer.json"),
            ("task-123 done", "task-123 done"),
            ("desktop", "desktop"),
            ("token=", "token="),
            // 值域非 ASCII 终止扫描：双端一致（形态缺口在台账登记，不在此钉掩码）
            (
                "token=\u{0130}STANBUL-secret99",
                "token=\u{0130}STANBUL-secret99",
            ),
            // 多行 / 空行
            ("TOKEN=a\npassword=b", "TOKEN=***\npassword=***"),
            ("", ""),
        ];
        for (line, expected) in vectors {
            assert_eq!(&redact_line(line), expected, "vector: {line:?}");
            // 幂等：掩码结果再扫一遍不变
            assert_eq!(&redact_line(expected), expected, "idempotent: {expected:?}");
        }
        // İ 前缀 ×37：折叠域/原串域字节偏移映射的端到端用例
        let padded = format!(
            "{} LMM_HF_TOKEN=hf_ABCDEF1234567890abcdefghij",
            "\u{0130}".repeat(37)
        );
        let masked = format!("{} LMM_HF_TOKEN=***", "\u{0130}".repeat(37));
        assert_eq!(redact_line(&padded), masked);
        assert_eq!(redact_line(&masked), masked);
        assert_eq!(
            redact_text("TOKEN=a\n\npassword=b"),
            "TOKEN=***\n\npassword=***"
        );
    }
}
