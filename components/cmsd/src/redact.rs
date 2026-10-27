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
        let lower = out.to_ascii_lowercase();
        let Some((kw_pos, val_start, greedy)) = next_secret(&lower, from) else {
            break;
        };
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
        from = kw_pos + 1;
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
}
