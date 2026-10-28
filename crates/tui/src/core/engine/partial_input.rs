//! Suffix-only proof for the existing per-delta tool-input parser.
//!
//! Replaces repeated full-prefix evaluation only when an unfinished string
//! cannot be repaired and its selected raw JSON segment has not changed.
//! All other input retains dispatch::parse_tool_input semantics.
//!
//! Strict parsing and repair cannot close a valid unfinished string. In that
//! case only the raw balanced-segment fallback can produce a value. Reuse the
//! prior Some/None disposition only while that segment's byte range is stable.
//! This state is owned by one append-only tool block, never by a tool ID.
//! Invalid Unicode hex disables reuse conservatively; unlike the escape-match
//! rule, this is defense in depth, not a separately mutation-killed condition.
#[derive(Clone, Debug, PartialEq, Eq)]
enum Segment {
    Root(usize),
    Array(usize, usize),
}
#[derive(Debug, Clone, Default)]
pub(super) struct PartialInputProof {
    bytes: usize,
    first_seen: bool,
    starts_object: bool,
    disabled: bool,
    in_string: bool,
    escape: bool,
    unicode: u8,
    root_start: Option<usize>,
    root_depth: i32,
    root_end: Option<usize>,
    array_start: Option<usize>,
    array_depth: i32,
    array_end: Option<usize>,
    certified: bool,
    previous: Option<Segment>,
    last_some: bool,
    #[cfg(test)]
    pub(super) reuse_count: usize,
}
impl PartialInputProof {
    /// The owner must append exactly `delta` and invalidate before any other
    /// buffer or mirror mutation. Length/suffix checks reject stale hints, but
    /// cannot certify ownership after an equal-length prefix replacement.
    pub(super) fn reuse(&mut self, buffer: &str, delta: &str) -> Option<bool> {
        if self.disabled {
            return None;
        }
        if buffer.len().checked_sub(delta.len()) != Some(self.bytes) || !buffer.ends_with(delta) {
            self.invalidate();
            return None;
        }
        self.feed(delta).then_some(self.last_some)
    }
    pub(super) fn record(&mut self, some: bool) {
        self.last_some = some;
    }

    pub(super) fn invalidate(&mut self) {
        self.disabled = true;
        self.certified = false;
    }
    fn feed(&mut self, text: &str) -> bool {
        for (offset, b) in text.bytes().enumerate() {
            let pos = self.bytes + offset;
            if !self.first_seen && !b.is_ascii_whitespace() {
                self.first_seen = true;
                self.starts_object = b == b'{';
                if !self.starts_object {
                    // Unsupported top-level forms always take the parser path;
                    // avoid scanning their later suffixes for a certificate.
                    self.disabled = true;
                    return false;
                }
            }
            // Match extract_balanced_segment's raw (not quote-aware) selection.
            if self.root_start.is_none() && b == b'{' {
                self.root_start = Some(pos);
                self.root_depth = 1;
            } else if self.root_start.is_some() && self.root_end.is_none() {
                if b == b'{' {
                    self.root_depth += 1;
                } else if b == b'}' {
                    self.root_depth -= 1;
                }
                if self.root_depth == 0 {
                    self.root_end = Some(pos + 1);
                }
            }
            if self.array_start.is_none() && b == b'[' {
                self.array_start = Some(pos);
                self.array_depth = 1;
            } else if self.array_start.is_some() && self.array_end.is_none() {
                if b == b'[' {
                    self.array_depth += 1;
                } else if b == b']' {
                    self.array_depth -= 1;
                }
                if self.array_depth == 0 {
                    self.array_end = Some(pos + 1);
                }
            }
            // Reject fences and raw controls. Invalid escapes must also disable
            // reuse: quote-unaware closer removal can merge escape runs and
            // change quote parity. There is no invalid-escape repair pass.
            if b == b'`' {
                self.disabled = true;
            }
            if self.in_string {
                if self.unicode > 0 {
                    if !b.is_ascii_hexdigit() {
                        self.disabled = true;
                    }
                    self.unicode -= 1;
                } else if self.escape {
                    self.escape = false;
                    match b {
                        b'u' => self.unicode = 4,
                        b'"' | b'\\' | b'/' | b'b' | b'f' | b'n' | b'r' | b't' => {}
                        _ => self.disabled = true,
                    }
                } else {
                    match b {
                        b'"' => self.in_string = false,
                        b'\\' => self.escape = true,
                        0..=31 => self.disabled = true,
                        _ => {}
                    }
                }
            } else {
                match b {
                    b'"' => self.in_string = true,
                    b'\\' => self.disabled = true,
                    0..=31 if !b.is_ascii_whitespace() => self.disabled = true,
                    _ => {}
                }
            }
        }
        self.bytes += text.len();
        let selected = if let Some(end) = self.root_end {
            Some(Segment::Root(end))
        } else if let (Some(start), Some(end)) = (self.array_start, self.array_end) {
            Some(Segment::Array(start, end))
        } else {
            None
        };
        let eligible = self.starts_object
            && !self.disabled
            && self.in_string
            && !self.escape
            && self.unicode == 0;
        let reuse = eligible && self.certified && selected == self.previous;
        self.certified = eligible;
        self.previous = selected;
        reuse
    }
}
