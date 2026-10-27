//! Console progress records accept CR, LF and CRLF. HTTP parsing deliberately
//! retains its own strict line semantics; this reader is only for child output.
use std::io::{self, BufRead};

pub(super) struct Records<R> {
    reader: R,
    skip_lf: bool,
}
impl<R: BufRead> Records<R> {
    pub(super) fn new(reader: R) -> Self {
        Self {
            reader,
            skip_lf: false,
        }
    }
    pub(super) fn next(&mut self) -> io::Result<Option<String>> {
        if self.skip_lf {
            self.skip_lf = false;
            if self.reader.fill_buf()?.first() == Some(&b'\n') {
                self.reader.consume(1);
            }
        }
        let mut bytes = Vec::new();
        loop {
            let buffer = self.reader.fill_buf()?;
            if buffer.is_empty() {
                if bytes.is_empty() {
                    return Ok(None);
                }
                break;
            }
            let end = buffer.iter().position(|b| matches!(b, b'\r' | b'\n'));
            let take = end.map(|v| v + 1).unwrap_or(buffer.len());
            if take > 8192usize.saturating_sub(bytes.len()) {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "script output record exceeds 8KiB",
                ));
            }
            self.skip_lf = end.is_some() && buffer[take - 1] == b'\r';
            bytes.extend_from_slice(&buffer[..take]);
            self.reader.consume(take);
            if end.is_some() {
                break;
            }
        }
        String::from_utf8(bytes)
            .map(Some)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "script output is not UTF-8"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufReader, Cursor};
    #[test]
    fn mixed_records_preserve_unicode_and_final_unterminated_output() {
        let mut records = Records::new(BufReader::with_capacity(
            2,
            Cursor::new("中文\r\n日本語\rEnglish\nfinal"),
        ));
        let mut result = Vec::new();
        while let Some(row) = records.next().unwrap() {
            result.push(row);
        }
        assert_eq!(result, ["中文\r", "日本語\r", "English\n", "final"]);
    }
    #[test]
    fn unbounded_or_invalid_utf8_still_fails_without_echoing_output() {
        let mut records = Records::new(BufReader::new(Cursor::new(vec![b'x'; 100_000])));
        assert_eq!(
            records.next().unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );
        let mut records = Records::new(BufReader::new(Cursor::new(vec![0xff, b'\n'])));
        assert_eq!(
            records.next().unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );
    }
}
