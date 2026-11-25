"""Bounded status-line observation for opt-in real stream acceptance drivers."""
import time


def response_line(stream, timeout=3):
    original = stream.gettimeout()
    deadline = time.monotonic() + timeout
    raw = bytearray()
    try:
        while len(raw) < 8192:
            remaining = deadline - time.monotonic()
            if remaining <= 0: raise TimeoutError('HTTP status line deadline')
            stream.settimeout(remaining)
            part = stream.recv(min(1024, 8192 - len(raw)))
            if not part:
                if not raw: return b''  # Malformed requests may be closed silently.
                raise EOFError('incomplete HTTP status line')
            raw.extend(part)
            end = raw.find(b'\r\n')
            if end >= 0: return bytes(raw[:end])
        raise ValueError('HTTP status line exceeds 8 KiB')
    finally:
        stream.settimeout(original)
