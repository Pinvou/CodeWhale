//! 本地 v1 JSON：有界解析器与序列化器。
//!
//! 覆盖 RFC 8259 常用面：对象/数组/字符串（含 \u 转义与代理对）/数字/布尔/null。
//! 数字内部统一 f64；需要精确整数的场景（字节数、端口）用 `as_u64` 取整，
//! 2^53 以内安全（权重字节数 ~2.3e10，远小于 9e15）。

use std::collections::BTreeMap;
use std::fmt;

#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Num(f64),
    Str(String),
    Arr(Vec<Value>),
    Obj(BTreeMap<String, Value>),
}

impl Value {
    pub fn obj() -> Value {
        Value::Obj(BTreeMap::new())
    }

    pub fn get(&self, key: &str) -> Option<&Value> {
        match self {
            Value::Obj(m) => m.get(key),
            _ => None,
        }
    }

    pub fn set(&mut self, key: &str, val: Value) {
        if let Value::Obj(m) = self {
            m.insert(key.to_string(), val);
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Value::Str(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_f64(&self) -> Option<f64> {
        match self {
            Value::Num(n) => Some(*n),
            _ => None,
        }
    }

    pub fn as_u64(&self) -> Option<u64> {
        match self {
            Value::Num(n) if *n >= 0.0 && *n < 18_446_744_073_709_551_616.0 && n.fract() == 0.0 => {
                Some(*n as u64)
            }
            _ => None,
        }
    }

    pub fn as_i64(&self) -> Option<i64> {
        match self {
            Value::Num(n)
                if *n >= -9_223_372_036_854_775_808.0
                    && *n < 9_223_372_036_854_775_808.0
                    && n.fract() == 0.0 =>
            {
                Some(*n as i64)
            }
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Value::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_arr(&self) -> Option<&Vec<Value>> {
        match self {
            Value::Arr(a) => Some(a),
            _ => None,
        }
    }

    pub fn as_obj(&self) -> Option<&BTreeMap<String, Value>> {
        match self {
            Value::Obj(m) => Some(m),
            _ => None,
        }
    }

    /// 序列化为紧凑 JSON 文本（经 Display 走，不另造 inherent 方法）。
    pub fn parse(input: &str) -> Result<Value, JsonError> {
        Self::parse_limited(input, 1_000_000)
    }

    pub fn parse_limited(input: &str, max_values: usize) -> Result<Value, JsonError> {
        let mut p = Parser {
            bytes: input.as_bytes(),
            pos: 0,
            depth: 0,
            values: 0,
            max_values,
        };
        p.skip_ws();
        let v = p.parse_value()?;
        p.skip_ws();
        if p.pos != p.bytes.len() {
            return Err(p.err("尾随多余内容"));
        }
        Ok(v)
    }
}

impl fmt::Display for Value {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let mut out = String::new();
        write_value(self, &mut out);
        f.write_str(&out)
    }
}

impl From<&str> for Value {
    fn from(s: &str) -> Value {
        Value::Str(s.to_string())
    }
}
impl From<String> for Value {
    fn from(s: String) -> Value {
        Value::Str(s)
    }
}
impl From<u64> for Value {
    fn from(n: u64) -> Value {
        Value::Num(n as f64)
    }
}
impl From<usize> for Value {
    fn from(n: usize) -> Value {
        Value::Num(n as f64)
    }
}
impl From<i64> for Value {
    fn from(n: i64) -> Value {
        Value::Num(n as f64)
    }
}
impl From<f64> for Value {
    fn from(n: f64) -> Value {
        Value::Num(n)
    }
}
impl From<bool> for Value {
    fn from(b: bool) -> Value {
        Value::Bool(b)
    }
}
impl From<Vec<Value>> for Value {
    fn from(a: Vec<Value>) -> Value {
        Value::Arr(a)
    }
}

fn write_value(v: &Value, out: &mut String) {
    match v {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Num(n) => write_num(*n, out),
        Value::Str(s) => write_str(s, out),
        Value::Arr(items) => {
            out.push('[');
            for (i, item) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_value(item, out);
            }
            out.push(']');
        }
        Value::Obj(map) => {
            out.push('{');
            for (i, (k, val)) in map.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_str(k, out);
                out.push(':');
                write_value(val, out);
            }
            out.push('}');
        }
    }
}

fn write_num(n: f64, out: &mut String) {
    if !n.is_finite() {
        out.push_str("null");
        return;
    }
    if n.fract() == 0.0 && n.abs() < 9.0e15 {
        // 整数直写，避免 1.0 这种尾巴
        out.push_str(&format!("{n:.0}"));
    } else {
        out.push_str(&format!("{n}"));
    }
}

fn write_str(s: &str, out: &mut String) {
    out.push('"');
    for ch in s.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0c}' => out.push_str("\\f"),
            c if (c as u32) < 0x20 => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

#[derive(Debug)]
pub struct JsonError {
    pub message: String,
    pub pos: usize,
}

impl fmt::Display for JsonError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "JSON 解析失败（位置 {}）：{}", self.pos, self.message)
    }
}

impl std::error::Error for JsonError {}

struct Parser<'a> {
    bytes: &'a [u8],
    pos: usize,
    depth: usize,
    values: usize,
    max_values: usize,
}

impl<'a> Parser<'a> {
    fn err(&self, msg: &str) -> JsonError {
        JsonError {
            message: msg.to_string(),
            pos: self.pos,
        }
    }

    fn peek(&self) -> Option<u8> {
        self.bytes.get(self.pos).copied()
    }

    fn next(&mut self) -> Option<u8> {
        let b = self.peek();
        if b.is_some() {
            self.pos += 1;
        }
        b
    }

    fn skip_ws(&mut self) {
        while let Some(b) = self.peek() {
            if b == b' ' || b == b'\t' || b == b'\n' || b == b'\r' {
                self.pos += 1;
            } else {
                break;
            }
        }
    }

    fn expect(&mut self, b: u8) -> Result<(), JsonError> {
        if self.next() == Some(b) {
            Ok(())
        } else {
            Err(self.err(&format!("期望 '{}'", b as char)))
        }
    }

    fn parse_value(&mut self) -> Result<Value, JsonError> {
        if self.values >= self.max_values {
            return Err(self.err("JSON值数量超过限制"));
        }
        self.values += 1;
        self.skip_ws();
        if self.depth >= 64 {
            return Err(self.err("JSON 嵌套超过64层"));
        }
        self.depth += 1;
        let result = match self.peek() {
            Some(b'{') => self.parse_obj(),
            Some(b'[') => self.parse_arr(),
            Some(b'"') => self.parse_string().map(Value::Str),
            Some(b't') => self.parse_lit("true", Value::Bool(true)),
            Some(b'f') => self.parse_lit("false", Value::Bool(false)),
            Some(b'n') => self.parse_lit("null", Value::Null),
            Some(b'-') | Some(b'0'..=b'9') => self.parse_num(),
            Some(_) => Err(self.err("意外的字符")),
            None => Err(self.err("输入提前结束")),
        };
        self.depth -= 1;
        result
    }

    fn parse_lit(&mut self, lit: &str, val: Value) -> Result<Value, JsonError> {
        if self.bytes[self.pos..].starts_with(lit.as_bytes()) {
            self.pos += lit.len();
            Ok(val)
        } else {
            Err(self.err(&format!("期望字面量 {lit}")))
        }
    }

    fn parse_num(&mut self) -> Result<Value, JsonError> {
        let start = self.pos;
        if self.peek() == Some(b'-') {
            self.pos += 1;
        }
        match self.next() {
            Some(b'0') => {}
            Some(b'1'..=b'9') => {
                while matches!(self.peek(), Some(b'0'..=b'9')) {
                    self.pos += 1;
                }
            }
            _ => return Err(self.err("数字整数部分非法")),
        }
        if self.peek() == Some(b'.') {
            self.pos += 1;
            if !matches!(self.peek(), Some(b'0'..=b'9')) {
                return Err(self.err("小数部分不能为空"));
            }
            while matches!(self.peek(), Some(b'0'..=b'9')) {
                self.pos += 1;
            }
        }
        if matches!(self.peek(), Some(b'e') | Some(b'E')) {
            self.pos += 1;
            if matches!(self.peek(), Some(b'+') | Some(b'-')) {
                self.pos += 1;
            }
            if !matches!(self.peek(), Some(b'0'..=b'9')) {
                return Err(self.err("指数部分不能为空"));
            }
            while matches!(self.peek(), Some(b'0'..=b'9')) {
                self.pos += 1;
            }
        }
        let text =
            std::str::from_utf8(&self.bytes[start..self.pos]).map_err(|_| self.err("数字编码"))?;
        let n = text.parse::<f64>().map_err(|_| self.err("数字格式非法"))?;
        if !n.is_finite() {
            return Err(self.err("数字超出有限范围"));
        }
        Ok(Value::Num(n))
    }

    fn parse_string(&mut self) -> Result<String, JsonError> {
        self.expect(b'"')?;
        let mut out = String::new();
        loop {
            match self.next() {
                None => return Err(self.err("字符串未闭合")),
                Some(b'"') => return Ok(out),
                Some(b'\\') => match self.next() {
                    Some(b'"') => out.push('"'),
                    Some(b'\\') => out.push('\\'),
                    Some(b'/') => out.push('/'),
                    Some(b'b') => out.push('\u{08}'),
                    Some(b'f') => out.push('\u{0c}'),
                    Some(b'n') => out.push('\n'),
                    Some(b'r') => out.push('\r'),
                    Some(b't') => out.push('\t'),
                    Some(b'u') => {
                        let hi = self.parse_hex4()?;
                        let ch = if (0xD800..0xDC00).contains(&hi) {
                            // 高代理：期待 \uXXXX 低代理
                            if self.next() == Some(b'\\') && self.next() == Some(b'u') {
                                let lo = self.parse_hex4()?;
                                if (0xDC00..0xE000).contains(&lo) {
                                    let cp = 0x10000 + ((hi - 0xD800) << 10) + (lo - 0xDC00);
                                    char::from_u32(cp).ok_or_else(|| self.err("Unicode码点非法"))?
                                } else {
                                    return Err(self.err("低代理非法"));
                                }
                            } else {
                                return Err(self.err("缺少低代理"));
                            }
                        } else {
                            char::from_u32(hi).ok_or_else(|| self.err("Unicode代理不成对"))?
                        };
                        out.push(ch);
                    }
                    _ => return Err(self.err("非法转义")),
                },
                Some(b) if b < 0x20 => return Err(self.err("字符串包含未转义控制字符")),
                Some(b) if b < 0x80 => out.push(b as char),
                Some(b) => {
                    // UTF-8 多字节原样收集（输入来自 &str，必为合法 UTF-8）
                    let len = utf8_len(b);
                    let start = self.pos - 1;
                    self.pos += len - 1;
                    if self.pos > self.bytes.len() {
                        return Err(self.err("UTF-8 截断"));
                    }
                    out.push_str(
                        std::str::from_utf8(&self.bytes[start..self.pos])
                            .map_err(|_| self.err("UTF-8 非法"))?,
                    );
                }
            }
        }
    }

    fn parse_hex4(&mut self) -> Result<u32, JsonError> {
        let mut v: u32 = 0;
        for _ in 0..4 {
            let b = self.next().ok_or_else(|| self.err("\\u 截断"))?;
            let d = match b {
                b'0'..=b'9' => (b - b'0') as u32,
                b'a'..=b'f' => (b - b'a' + 10) as u32,
                b'A'..=b'F' => (b - b'A' + 10) as u32,
                _ => return Err(self.err("\\u 十六进制非法")),
            };
            v = v * 16 + d;
        }
        Ok(v)
    }

    fn parse_arr(&mut self) -> Result<Value, JsonError> {
        self.expect(b'[')?;
        let mut items = Vec::new();
        self.skip_ws();
        if self.peek() == Some(b']') {
            self.pos += 1;
            return Ok(Value::Arr(items));
        }
        loop {
            items.push(self.parse_value()?);
            self.skip_ws();
            match self.next() {
                Some(b',') => continue,
                Some(b']') => return Ok(Value::Arr(items)),
                _ => return Err(self.err("数组期望 ',' 或 ']'")),
            }
        }
    }

    fn parse_obj(&mut self) -> Result<Value, JsonError> {
        self.expect(b'{')?;
        let mut map = BTreeMap::new();
        self.skip_ws();
        if self.peek() == Some(b'}') {
            self.pos += 1;
            return Ok(Value::Obj(map));
        }
        loop {
            self.skip_ws();
            let key = self.parse_string()?;
            self.skip_ws();
            self.expect(b':')?;
            let val = self.parse_value()?;
            if map.insert(key, val).is_some() {
                return Err(self.err("对象键重复"));
            }
            self.skip_ws();
            match self.next() {
                Some(b',') => continue,
                Some(b'}') => return Ok(Value::Obj(map)),
                _ => return Err(self.err("对象期望 ',' 或 '}'")),
            }
        }
    }
}

fn utf8_len(first: u8) -> usize {
    if first >= 0xF0 {
        4
    } else if first >= 0xE0 {
        3
    } else if first >= 0xC0 {
        2
    } else {
        1
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bounds_nesting_and_rejects_ambiguous_json() {
        let deep = format!("{}0{}", "[".repeat(100_000), "]".repeat(100_000));
        assert!(Value::parse(&deep).is_err());
        assert!(Value::parse(&format!("{}0{}", "[".repeat(63), "]".repeat(63))).is_ok());
        assert!(Value::parse(&format!("{}0{}", "[".repeat(64), "]".repeat(64))).is_err());
        for bad in [
            "01",
            "-01",
            "1.",
            "1e",
            "1e+",
            "1e99999",
            "-.2",
            "NaN",
            "\"a\nb\"",
            r#""\ud800""#,
            r#""\ud800\u0041""#,
            r#""\udc00""#,
            r#"{"a":1,"\u0061":2}"#,
        ] {
            assert!(Value::parse(bad).is_err(), "accepted: {bad}");
        }
        assert_eq!(Value::parse("18446744073709551616").unwrap().as_u64(), None);
        assert_eq!(Value::parse("9223372036854775808").unwrap().as_i64(), None);
    }

    #[test]
    fn deterministic_unicode_and_structural_corpus_is_bounded() {
        let alphabet = [
            '{', '}', '[', ']', '"', '\\', '\0', '\n', 'a', '0', '-', '.', 'e', ':', ',', '😀',
            '中',
        ];
        let mut seed = 13u64;
        for size in 0..8192 {
            let mut input = String::new();
            for _ in 0..(size % 511) {
                seed = seed.wrapping_mul(6364136223846793005).wrapping_add(1);
                input.push(alphabet[(seed as usize) % alphabet.len()]);
            }
            if let Ok(v) = Value::parse(&input) {
                assert_eq!(Value::parse(&v.to_string()).unwrap(), v);
            }
        }
    }

    #[test]
    fn roundtrip_basic() {
        let src = r#"{"a":1,"b":[true,false,null],"c":"x\ny\"z","d":-2.5}"#;
        let v = Value::parse(src).unwrap();
        assert_eq!(v.get("a").unwrap().as_u64(), Some(1));
        assert_eq!(v.get("d").unwrap().as_f64(), Some(-2.5));
        assert_eq!(Value::parse(&v.to_string()).unwrap(), v);
    }

    #[test]
    fn unicode_escapes() {
        let v = Value::parse(r#""\u0041\ud83d\ude00""#).unwrap();
        assert_eq!(v.as_str(), Some("A\u{1F600}"));
    }

    #[test]
    fn big_integer_exact() {
        let v = Value::parse("22892664832").unwrap();
        assert_eq!(v.as_u64(), Some(22892664832));
        assert_eq!(v.to_string(), "22892664832");
    }

    #[test]
    fn serialize_escapes_control() {
        let v = Value::Str("a\u{0001}b".to_string());
        assert_eq!(v.to_string(), "\"a\\u0001b\"");
    }

    #[test]
    fn rejects_trailing() {
        assert!(Value::parse("{} garbage").is_err());
        assert!(Value::parse("[1,").is_err());
    }

    #[test]
    fn obj_key_order_stable() {
        let mut v = Value::obj();
        v.set("z", 1u64.into());
        v.set("a", 2u64.into());
        assert_eq!(v.to_string(), r#"{"a":2,"z":1}"#);
    }

    #[test]
    fn nested_models_response() {
        // 引擎 /v1/models 应答的形状
        let v = Value::parse(
            r#"{"object":"list","data":[{"id":"Ornith-1.5-35B-A3B-NVFP4","object":"model"}]}"#,
        )
        .unwrap();
        let id = v
            .get("data")
            .and_then(|d| d.as_arr())
            .and_then(|a| a.first())
            .and_then(|m| m.get("id"))
            .and_then(|i| i.as_str());
        assert_eq!(id, Some("Ornith-1.5-35B-A3B-NVFP4"));
    }
}
