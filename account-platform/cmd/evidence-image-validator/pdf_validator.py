"""Opt-in local PDF child candidate; not a locked or approved runtime."""
import hashlib
import io
import struct
import sys
import warnings

MAX_BYTES = 10 * 1024 * 1024
VALID, INVALID, UNAVAILABLE, PASSWORD_REQUIRED = 1, 2, 3, 4


class _Discard:
    def write(self, data):
        return len(data)

    def flush(self):
        pass


def validate(data):
    if not data or len(data) > MAX_BYTES:
        return INVALID
    with warnings.catch_warnings():
        # Stop on the first Python warning without retaining its content.
        warnings.simplefilter("error")
        try:
            import pikepdf
            if pikepdf.__version__ != "10.17.0" or pikepdf.__libqpdf_version__ != "12.4.2":
                return UNAVAILABLE
        except Warning:
            return INVALID
        except BaseException:
            return UNAVAILABLE
        pdf = None
        result = UNAVAILABLE
        stream = io.BytesIO(data)
        try:
            pdf = pikepdf.open(stream, password="", suppress_warnings=True, attempt_recovery=False)
            if pdf.get_warnings():
                result = INVALID
            else:
                problems = pdf.check_pdf_syntax()
                if problems or pdf.get_warnings():
                    result = INVALID
                else:
                    linear_ok = pdf.check_linearization(_Discard()) if pdf.is_linearized else True
                    result = INVALID if not linear_ok or pdf.get_warnings() else VALID
        except Warning:
            result = INVALID
        except pikepdf.PasswordError:
            result = PASSWORD_REQUIRED
        except (pikepdf.PdfError, pikepdf.DataDecodingError):
            result = INVALID
        except BaseException:
            result = UNAVAILABLE
        finally:
            if pdf is not None:
                try:
                    pdf.close()
                    if pdf.get_warnings():
                        result = INVALID
                except Warning:
                    result = INVALID
                except BaseException:
                    if result != INVALID:
                        result = UNAVAILABLE
            try:
                stream.close()
            except Warning:
                result = INVALID
            except BaseException:
                if result != INVALID:
                    result = UNAVAILABLE
    return result


def _read_exact(stream, n):
    out = bytearray()
    while len(out) < n:
        part = stream.read(n - len(out))
        if not part:
            return None
        out.extend(part)
    return out


def main():
    header = _read_exact(sys.stdin.buffer, 60)
    if header is None or header[:4] != b"PIV2" or header[4:8] != b"\x03\0\0\0":
        return UNAVAILABLE
    length = struct.unpack(">I", header[56:60])[0]
    if length == 0 or length > MAX_BYTES:
        return UNAVAILABLE
    data = _read_exact(sys.stdin.buffer, length)
    if data is None:
        return UNAVAILABLE
    try:
        if hashlib.sha256(data).digest() != header[24:56]:
            return UNAVAILABLE
        return validate(data)
    finally:
        data[:] = b"\0" * len(data)


if __name__ == "__main__":
    try:
        status = main()
    except BaseException:
        status = UNAVAILABLE
    try:
        sys.stdout.buffer.write(bytes([status]))
        sys.stdout.buffer.flush()
    except BaseException:
        pass
