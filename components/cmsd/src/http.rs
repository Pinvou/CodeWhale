//! 极简 HTTP/1.1：服务端请求解析、响应写出、客户端 GET（std-only）。
//!
//! 只实现 cmsd 需要的子集：Content-Length 与 chunked 两种主体编码、
//! 请求行/状态行解析、响应头读取。网关流式转发不经过本模块的 body 解析
//! （见 gateway.rs，用 connection-close 语义直通）。

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::time::{Duration, Instant};
mod unix;

pub(crate) fn connect_unix(
    path: &std::path::Path,
    timeout: Duration,
) -> std::io::Result<std::os::unix::net::UnixStream> {
    unix::connect(path, timeout)
}

/// Bound the total Unix API reply lifetime, including partial writes to a
/// trickling reader. Streaming model inference does not use this wrapper.
pub(crate) struct UnixDeadlineWriter<'a> {
    stream: &'a mut std::os::unix::net::UnixStream,
    deadline: Instant,
}
impl<'a> UnixDeadlineWriter<'a> {
    pub fn new(stream: &'a mut std::os::unix::net::UnixStream, budget: Duration) -> Self {
        Self {
            stream,
            deadline: Instant::now() + budget,
        }
    }
    fn remaining(&self) -> std::io::Result<Duration> {
        self.deadline
            .checked_duration_since(Instant::now())
            .filter(|value| !value.is_zero())
            .ok_or_else(|| {
                std::io::Error::new(std::io::ErrorKind::TimedOut, "HTTP reply deadline exceeded")
            })
    }
}
impl Write for UnixDeadlineWriter<'_> {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        use std::os::fd::AsRawFd;
        loop {
            self.remaining()?;
            // A blocking send can keep making internal progress past its
            // SO_SNDTIMEO. Per-call nonblocking mode avoids changing O_NONBLOCK
            // on the parser's cloned fd and never raises process SIGPIPE.
            // SAFETY: borrowed live Unix fd and a valid byte buffer/length.
            let written = unsafe {
                libc::send(
                    self.stream.as_raw_fd(),
                    bytes.as_ptr().cast(),
                    bytes.len(),
                    libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
                )
            };
            if written >= 0 {
                return Ok(written as usize);
            }
            let error = std::io::Error::last_os_error();
            if error.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            if error.kind() != std::io::ErrorKind::WouldBlock {
                return Err(error);
            }
            let timeout = self
                .remaining()?
                .as_millis()
                .saturating_add(1)
                .min(i32::MAX as u128) as i32;
            let mut poll = libc::pollfd {
                fd: self.stream.as_raw_fd(),
                events: libc::POLLOUT,
                revents: 0,
            };
            // SAFETY: one valid pollfd, bounded timeout, no ownership transfer.
            let ready = unsafe { libc::poll(&mut poll, 1, timeout) };
            if ready < 0 {
                let error = std::io::Error::last_os_error();
                if error.kind() != std::io::ErrorKind::Interrupted {
                    return Err(error);
                }
            }
            // Recheck the absolute deadline even after poll reports writable.
        }
    }
    fn flush(&mut self) -> std::io::Result<()> {
        self.remaining()?;
        self.stream.flush()
    }
}

/// Bound total receive time as well as each socket read. Trickle traffic must
/// not hold an admitted API connection forever.
pub struct DeadlineReader<R> {
    inner: R,
    deadline: Instant,
}
impl<R> DeadlineReader<R> {
    pub fn new(inner: R, budget: Duration) -> Self {
        Self {
            inner,
            deadline: Instant::now() + budget,
        }
    }
}
impl<R: Read> Read for DeadlineReader<R> {
    fn read(&mut self, bytes: &mut [u8]) -> std::io::Result<usize> {
        if Instant::now() >= self.deadline {
            return Err(std::io::Error::new(
                std::io::ErrorKind::TimedOut,
                "HTTP receive deadline exceeded",
            ));
        }
        self.inner.read(bytes)
    }
}

#[derive(Debug)]
pub struct Request {
    pub method: String,
    pub path: String,
    pub query: String,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    _body_budget: crate::limits::BodyPermit,
}

impl Request {
    /// 查询串里取某个 key 的值（无 URL 解码，P1 的 query 只有 tail=N 这种纯数字）。
    pub fn query_param(&self, key: &str) -> Option<&str> {
        self.query.split('&').find_map(|pair| {
            pair.split_once('=')
                .filter(|(k, _)| *k == key)
                .map(|(_, v)| v)
        })
    }
}

const MAX_HEAD_BYTES: usize = 64 * 1024;
const MAX_BODY_BYTES: usize = 16 * 1024 * 1024;

/// 从连接读一个请求。连接在对端干净关闭（还没读任何字节）时返回 Ok(None)。
pub fn read_request<R: Read>(reader: &mut BufReader<R>) -> std::io::Result<Option<Request>> {
    read_request_limited(reader, MAX_BODY_BYTES)
}

pub fn read_request_limited<R: Read>(
    reader: &mut BufReader<R>,
    maximum: usize,
) -> std::io::Result<Option<Request>> {
    let head = match read_head(reader)? {
        None => return Ok(None),
        Some(h) => h,
    };
    let mut lines = head.split("\r\n");
    let request_line = lines.next().unwrap_or("");
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("").to_string();
    let target = parts.next().unwrap_or("").to_string();
    let version = parts.next().unwrap_or("");
    if method.is_empty()
        || target.is_empty()
        || !matches!(version, "HTTP/1.0" | "HTTP/1.1")
        || parts.next().is_some()
    {
        return Err(bad_input("请求行不完整"));
    }
    let (path, query) = match target.split_once('?') {
        Some((p, q)) => (p.to_string(), q.to_string()),
        None => (target, String::new()),
    };
    let mut headers = Vec::new();
    for line in lines.filter(|line| !line.is_empty()) {
        let (k, v) = line
            .split_once(':')
            .ok_or_else(|| bad_input("头字段缺少冒号"))?;
        if k.is_empty()
            || !k
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b))
            || v.chars().any(|c| c.is_control() && c != '\t')
        {
            return Err(bad_input("头字段不合法"));
        }
        headers.push((k.to_ascii_lowercase(), v.trim().to_string()));
    }
    let (body, budget) = read_body(reader, &headers, maximum.min(MAX_BODY_BYTES))?;
    Ok(Some(Request {
        method,
        path,
        query,
        headers,
        body,
        _body_budget: budget,
    }))
}

/// 读响应（客户端侧）：状态码 + 主体（头部只用于分帧，不保留）。
pub struct Response {
    pub status: u16,
    pub body: Vec<u8>,
    _body_budget: crate::limits::BodyPermit,
}

pub fn read_response<R: Read>(reader: &mut BufReader<R>) -> std::io::Result<Response> {
    read_response_limited(reader, MAX_BODY_BYTES)
}

pub(crate) fn read_response_limited<R: Read>(
    reader: &mut BufReader<R>,
    maximum: usize,
) -> std::io::Result<Response> {
    let head = read_head(reader)?.ok_or_else(|| bad_input("响应为空"))?;
    let mut lines = head.split("\r\n");
    let status_line = lines.next().unwrap_or("");
    let status = status_line
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse::<u16>().ok())
        .ok_or_else(|| bad_input("状态行无法解析"))?;
    let mut headers = Vec::new();
    for line in lines {
        if let Some((k, v)) = line.split_once(':') {
            headers.push((k.trim().to_ascii_lowercase(), v.trim().to_string()));
        }
    }
    let (body, budget) = read_body(reader, &headers, maximum.min(MAX_BODY_BYTES))?;
    Ok(Response {
        status,
        body,
        _body_budget: budget,
    })
}

/// 读到空行为止的头部块。流起始即 EOF 返回 Ok(None)。
fn read_head<R: Read>(reader: &mut BufReader<R>) -> std::io::Result<Option<String>> {
    let mut head = String::new();
    loop {
        let line = bounded_line(reader, MAX_HEAD_BYTES.saturating_sub(head.len()))?;
        let n = line.len();
        if n == 0 {
            if head.is_empty() {
                return Ok(None);
            }
            return Err(bad_input("头部未结束连接就断了"));
        }
        if head.len() + line.len() > MAX_HEAD_BYTES {
            return Err(bad_input("头部过大"));
        }
        if line == "\r\n" || line == "\n" {
            return Ok(Some(head));
        }
        head.push_str(&line);
    }
}

/// Check the limit while consuming bytes, before allocation. read_line followed
/// by len() would allocate an unbounded unterminated attacker-controlled line.
pub(crate) fn bounded_line<R: Read>(
    reader: &mut BufReader<R>,
    maximum: usize,
) -> std::io::Result<String> {
    let mut out = Vec::new();
    loop {
        let bytes = reader.fill_buf()?;
        if bytes.is_empty() {
            break;
        }
        let length = bytes
            .iter()
            .position(|b| *b == b'\n')
            .map(|n| n + 1)
            .unwrap_or(bytes.len());
        if length > maximum.saturating_sub(out.len()) {
            return Err(bad_input("HTTP 行或头部过大"));
        }
        let ended = bytes[length - 1] == b'\n';
        out.extend_from_slice(&bytes[..length]);
        reader.consume(length);
        if ended {
            break;
        }
    }
    String::from_utf8(out).map_err(|_| bad_input("HTTP 头不是UTF-8"))
}

fn read_body<R: Read>(
    reader: &mut BufReader<R>,
    headers: &[(String, String)],
    maximum: usize,
) -> std::io::Result<(Vec<u8>, crate::limits::BodyPermit)> {
    static BUDGET: std::sync::OnceLock<std::sync::Arc<crate::limits::BodyBudget>> =
        std::sync::OnceLock::new();
    let mut budget = BUDGET
        .get_or_init(|| {
            crate::limits::BodyBudget::new(4 * 1024 * 1024, 16 * 1024 * 1024, 64 * 1024)
        })
        .permit();
    let find = |name: &str| {
        headers
            .iter()
            .find(|(k, _)| k == name)
            .map(|(_, v)| v.clone())
    };
    let count = |name: &str| headers.iter().filter(|(key, _)| key == name).count();
    if count("content-length") > 1
        || count("transfer-encoding") > 1
        || (count("content-length") > 0 && count("transfer-encoding") > 0)
    {
        return Err(bad_input("HTTP 主体分帧冲突"));
    }
    if let Some(te) = find("transfer-encoding") {
        if te.eq_ignore_ascii_case("chunked") {
            return read_chunked(reader, maximum, budget);
        }
        return Err(bad_input("不支持的transfer-encoding"));
    }
    if let Some(cl) = find("content-length") {
        let len: usize = cl.parse().map_err(|_| bad_input("content-length 非法"))?;
        if len > maximum {
            return Err(bad_input("主体过大"));
        }
        if !budget.reserve(len) {
            return Err(bad_input("HTTP buffer budget exhausted"));
        }
        let mut buf = Vec::new();
        buf.try_reserve_exact(len)
            .map_err(|_| bad_input("HTTP buffer budget exhausted"))?;
        buf.resize(len, 0);
        reader.read_exact(&mut buf)?;
        return Ok((buf, budget));
    }
    Ok((Vec::new(), budget))
}

fn read_chunked<R: Read>(
    reader: &mut BufReader<R>,
    maximum: usize,
    mut budget: crate::limits::BodyPermit,
) -> std::io::Result<(Vec<u8>, crate::limits::BodyPermit)> {
    let mut out = Vec::new();
    loop {
        let size_line = bounded_line(reader, 8192)?;
        if size_line.is_empty() {
            return Err(bad_input("chunked 流截断"));
        }
        let size_text = size_line.trim().split(';').next().unwrap_or("");
        let size = usize::from_str_radix(size_text, 16).map_err(|_| bad_input("chunk 尺寸非法"))?;
        if size == 0 {
            // 收尾的 trailer 行直到空行
            let mut trailer_bytes = 0usize;
            loop {
                let line = bounded_line(reader, MAX_HEAD_BYTES.saturating_sub(trailer_bytes))?;
                if line.is_empty() {
                    return Err(bad_input("chunked trailer 截断"));
                }
                trailer_bytes += line.len();
                if line == "\r\n" || line == "\n" {
                    break;
                }
            }
            return Ok((out, budget));
        }
        if size > maximum.saturating_sub(out.len()) {
            return Err(bad_input("主体过大"));
        }
        let start = out.len();
        if !budget.reserve(start + size) {
            return Err(bad_input("HTTP buffer budget exhausted"));
        }
        out.try_reserve_exact(size)
            .map_err(|_| bad_input("HTTP buffer budget exhausted"))?;
        out.resize(start + size, 0);
        reader.read_exact(&mut out[start..])?;
        let mut crlf = [0u8; 2];
        reader.read_exact(&mut crlf)?;
        if crlf != *b"\r\n" {
            return Err(bad_input("chunk 数据未正确结束"));
        }
    }
}

/// 服务端响应写出（connection: close 语义，调用方随后关闭流）。
pub fn write_response<W: Write>(
    w: &mut W,
    status: &str,
    content_type: &str,
    body: &[u8],
    extra_headers: &[(&str, &str)],
) -> std::io::Result<()> {
    write!(
        w,
        "HTTP/1.1 {status}\r\ncontent-type: {content_type}\r\ncontent-length: {}\r\nconnection: close\r\n",
        body.len()
    )?;
    for (k, v) in extra_headers {
        write!(w, "{k}: {v}\r\n")?;
    }
    write!(w, "\r\n")?;
    w.write_all(body)?;
    w.flush()
}

/// 客户端 GET：带超时，返回状态码与主体。供健康检查与容量探测用。
pub fn client_get(
    host: &str,
    port: u16,
    path: &str,
    timeout: Duration,
) -> std::io::Result<Response> {
    let addr = format!("{host}:{port}");
    let stream =
        TcpStream::connect_timeout(&addr.parse().map_err(|_| bad_input("地址非法"))?, timeout)?;
    stream.set_read_timeout(Some(timeout))?;
    stream.set_write_timeout(Some(timeout))?;
    let mut w = stream.try_clone()?;
    write!(
        w,
        "GET {path} HTTP/1.1\r\nhost: {addr}\r\nconnection: close\r\n\r\n"
    )?;
    w.flush()?;
    let mut reader = BufReader::new(DeadlineReader::new(stream, timeout));
    read_response(&mut reader)
}

pub fn bad_input(msg: &str) -> std::io::Error {
    std::io::Error::new(std::io::ErrorKind::InvalidData, msg.to_string())
}

pub fn status_line(status: u16) -> &'static str {
    match status {
        200 => "200 OK",
        201 => "201 Created",
        202 => "202 Accepted",
        400 => "400 Bad Request",
        401 => "401 Unauthorized",
        403 => "403 Forbidden",
        404 => "404 Not Found",
        409 => "409 Conflict",
        410 => "410 Gone",
        413 => "413 Payload Too Large",
        422 => "422 Unprocessable Content",
        429 => "429 Too Many Requests",
        501 => "501 Not Implemented",
        503 => "503 Service Unavailable",
        _ => "500 Internal Server Error",
    }
}

pub fn write_receive_error<W: Write>(
    writer: &mut W,
    error: &std::io::Error,
) -> std::io::Result<()> {
    let (status,body)=match error.to_string().as_str() {
        "主体过大"=>("413 Payload Too Large",br#"{"error":{"code":"request_body_limit","message":"request body exceeds this endpoint limit"}}"#.as_slice()),
        "HTTP buffer budget exhausted"=>("503 Service Unavailable",br#"{"error":{"code":"request_buffer_limit","message":"device request buffer busy; retry later"}}"#.as_slice()),
        _=>("400 Bad Request",br#"{"error":{"code":"invalid_http_request","message":"invalid or incomplete HTTP request"}}"#.as_slice()),
    };
    write_response(writer, status, "application/json", body, &[])
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unix_reply_deadline_survives_partial_progress_and_restores_timeout() {
        use std::os::fd::AsRawFd;
        use std::sync::{
            atomic::{AtomicBool, Ordering},
            Arc,
        };
        let (mut writer, mut reader) = std::os::unix::net::UnixStream::pair().unwrap();
        writer
            .set_write_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let small: libc::c_int = 8192;
        // SAFETY: valid socket fd and pointer/size of the integer socket option.
        assert_eq!(
            unsafe {
                libc::setsockopt(
                    writer.as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_SNDBUF,
                    (&small as *const libc::c_int).cast(),
                    std::mem::size_of_val(&small) as libc::socklen_t,
                )
            },
            0
        );
        reader
            .set_read_timeout(Some(Duration::from_millis(20)))
            .unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let ended = stop.clone();
        let receiver = std::thread::spawn(move || {
            let mut bytes = [0; 2048];
            while !ended.load(Ordering::SeqCst) {
                let _ = reader.read(&mut bytes);
                std::thread::sleep(Duration::from_millis(10));
            }
        });
        let begun = Instant::now();
        let result = {
            let mut deadline = UnixDeadlineWriter::new(&mut writer, Duration::from_millis(100));
            deadline.write_all(&vec![b'x'; 1024 * 1024])
        };
        stop.store(true, Ordering::SeqCst);
        receiver.join().unwrap();
        assert_eq!(result.unwrap_err().kind(), std::io::ErrorKind::TimedOut);
        assert!(begun.elapsed() >= Duration::from_millis(80));
        assert!(begun.elapsed() < Duration::from_millis(400));
        assert_eq!(
            writer.write_timeout().unwrap(),
            Some(Duration::from_secs(2))
        );
    }
    use std::io::Cursor;

    fn req_from(text: &str) -> Request {
        let mut reader = BufReader::new(Cursor::new(text.as_bytes().to_vec()));
        read_request(&mut reader).unwrap().unwrap()
    }

    #[test]
    fn parses_simple_get() {
        let r = req_from("GET /v1/status?tail=5 HTTP/1.1\r\nHost: x\r\n\r\n");
        assert_eq!(r.method, "GET");
        assert_eq!(r.path, "/v1/status");
        assert_eq!(r.query_param("tail"), Some("5"));
        assert!(r.headers.iter().any(|(k, v)| k == "host" && v == "x"));
    }

    #[test]
    fn parses_post_with_body() {
        let r = req_from(
            "POST /v1/chat/completions HTTP/1.1\r\nContent-Length: 15\r\n\r\n{\"model\":\"abc\"}",
        );
        assert_eq!(r.body, b"{\"model\":\"abc\"}");
    }

    #[test]
    fn endpoint_limit_rejects_content_length_and_chunks_before_body_read() {
        for message in [
            "POST /x HTTP/1.1\r\nContent-Length: 65537\r\n\r\n",
            "POST /x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n10001\r\n",
        ] {
            let error =
                read_request_limited(&mut BufReader::new(Cursor::new(message)), 65536).unwrap_err();
            assert_eq!(error.to_string(), "主体过大");
            let mut response = Vec::new();
            write_receive_error(&mut response, &error).unwrap();
            assert!(response.starts_with(b"HTTP/1.1 413"));
        }
    }

    #[test]
    fn parses_chunked() {
        let text = "POST /x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nWiki\r\n5\r\npedia\r\n0\r\n\r\n";
        let r = req_from(text);
        assert_eq!(r.body, b"Wikipedia");
    }

    #[test]
    fn eof_at_start_is_none() {
        let mut reader = BufReader::new(Cursor::new(Vec::<u8>::new()));
        assert!(read_request(&mut reader).unwrap().is_none());
    }

    #[test]
    fn parses_response() {
        let text =
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}";
        let mut reader = BufReader::new(Cursor::new(text.as_bytes().to_vec()));
        let resp = read_response(&mut reader).unwrap();
        assert_eq!(resp.status, 200);
        assert_eq!(resp.body, b"{}");
    }
}

#[cfg(test)]
mod robustness_tests {
    use super::*;
    use std::cell::Cell;
    use std::rc::Rc;

    struct EndlessLine(Rc<Cell<usize>>);
    impl Read for EndlessLine {
        fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
            buffer.fill(b'A');
            self.0.set(self.0.get() + buffer.len());
            Ok(buffer.len())
        }
    }
    #[test]
    fn unterminated_header_and_chunk_lines_are_bounded_before_allocation() {
        let count = Rc::new(Cell::new(0));
        let mut reader = BufReader::new(EndlessLine(count.clone()));
        assert!(read_request(&mut reader).is_err());
        assert!(count.get() <= MAX_HEAD_BYTES + 8192);
        let count = Rc::new(Cell::new(0));
        let prefix =
            std::io::Cursor::new(b"POST /x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n");
        let mut reader = BufReader::new(prefix.chain(EndlessLine(count.clone())));
        assert!(read_request(&mut reader).is_err());
        assert!(count.get() <= 16384);
    }
    #[test]
    fn ambiguous_framing_overflow_bad_chunks_and_trailers_are_rejected() {
        for tail in [
            "Content-Length: 0\r\nContent-Length: 0\r\n\r\n",
            "Content-Length: 0\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
            "Transfer-Encoding: gzip, chunked\r\n\r\n",
            "Transfer-Encoding: chunked\r\n\r\nffffffffffffffff\r\n",
            "Transfer-Encoding: chunked\r\n\r\n1\r\nxXX0\r\n\r\n",
            "Transfer-Encoding: chunked\r\n\r\n0\r\n",
        ] {
            let mut reader =
                BufReader::new(std::io::Cursor::new(format!("POST /x HTTP/1.1\r\n{tail}")));
            assert!(read_request(&mut reader).is_err(), "{tail}");
        }
    }
    #[test]
    fn receive_deadline_cannot_be_extended_by_additional_bytes() {
        let mut reader = DeadlineReader::new(std::io::Cursor::new(b"GET /"), Duration::ZERO);
        assert_eq!(
            reader.read(&mut [0u8; 1]).unwrap_err().kind(),
            std::io::ErrorKind::TimedOut
        );
    }
    #[test]
    fn deterministic_malformed_packet_corpus_never_panics() {
        let mut seed = 0x9e37_79b9u32;
        for case in 0..4096usize {
            let mut bytes = Vec::new();
            for _ in 0..case % 1024 {
                seed ^= seed << 13;
                seed ^= seed >> 17;
                seed ^= seed << 5;
                bytes.push((seed & 0x7f) as u8);
            }
            let _ = read_request(&mut BufReader::new(std::io::Cursor::new(bytes.clone())));
            let _ = read_response(&mut BufReader::new(std::io::Cursor::new(bytes)));
        }
    }
}
