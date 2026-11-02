import contextlib
import importlib.util
import io
import pathlib
import sys
import types
import unittest
import warnings
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("candidate_pdf", pathlib.Path(__file__).with_name("pdf_validator.py"))
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)

class PdfError(Exception): pass
class PasswordError(Exception): pass
class FakePDF:
    def __init__(self, behavior): self.behavior, self.closed = behavior, False
    def get_warnings(self):
        if not self.closed and self.behavior == "get_warning": warnings.warn("private warning")
        if self.closed and self.behavior == "close_get_warning": warnings.warn("private warning")
        if not self.closed and self.behavior == "native_warning_close_error": return ["private warning"]
        return ["private warning"] if self.closed and self.behavior == "close_native_warning" else []
    def check_pdf_syntax(self):
        if self.behavior in ("stream_warning", "warning_close_error"): warnings.warn("private warning")
        return ["private warning"] if self.behavior == "syntax_problem" else []
    @property
    def is_linearized(self): return self.behavior in ("linearization", "linearization_warning")
    def check_linearization(self, stream):
        if self.behavior == "linearization_warning": warnings.warn("private warning")
        stream.write("private diagnostic")
        return False
    def close(self):
        self.closed = True
        if self.behavior == "close_warning": warnings.warn("private warning")
        if self.behavior in ("close_error", "warning_close_error", "native_warning_close_error"): raise RuntimeError("private error")

class PDFCandidateTests(unittest.TestCase):
    def test_warning_close_and_status_contract(self):
        for behavior, expected in [("valid",1), ("open_warning",2), ("get_warning",2), ("stream_warning",2), ("warning_close_error",2), ("native_warning_close_error",2), ("syntax_problem",2), ("linearization",2), ("linearization_warning",2), ("close_warning",2), ("close_get_warning",2), ("close_native_warning",2), ("close_error",3), ("password",4), ("bad_pdf",2), ("exception",3)]:
            with self.subTest(behavior=behavior):
                def open_pdf(*args, **kwargs):
                    if behavior == "open_warning": warnings.warn("private warning")
                    if behavior == "password": raise PasswordError("private")
                    if behavior == "bad_pdf": raise PdfError("private")
                    if behavior == "exception": raise RuntimeError("private")
                    return FakePDF(behavior)
                module = types.SimpleNamespace(__version__="10.17.0", __libqpdf_version__="12.4.2", open=open_pdf, PasswordError=PasswordError, PdfError=PdfError, DataDecodingError=PdfError)
                out, err = io.StringIO(), io.StringIO()
                with patch.dict(sys.modules,{"pikepdf":module}), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                    self.assertEqual(candidate.validate(b"%PDF-fixture"),expected)
                self.assertEqual(out.getvalue(),"")
                self.assertEqual(err.getvalue(),"")

    def test_first_warning_stops_unbounded_emission(self):
        count = []
        class ManyWarnings(FakePDF):
            def check_pdf_syntax(self):
                for i in range(10000):
                    count.append(i)
                    warnings.warn("private warning")
                return []
        module = types.SimpleNamespace(__version__="10.17.0", __libqpdf_version__="12.4.2", open=lambda *a, **k: ManyWarnings("valid"), PasswordError=PasswordError, PdfError=PdfError, DataDecodingError=PdfError)
        err = io.StringIO()
        with patch.dict(sys.modules,{"pikepdf":module}), contextlib.redirect_stderr(err):
            self.assertEqual(candidate.validate(b"%PDF-fixture"),2)
        self.assertEqual(count, [0])
        self.assertEqual(err.getvalue(), "")

    def test_unapproved_runtime_fails_closed(self):
        module = types.SimpleNamespace(__version__="wrong", __libqpdf_version__="12.4.2")
        with patch.dict(sys.modules,{"pikepdf":module}):
            self.assertEqual(candidate.validate(b"%PDF-fixture"),3)

    def test_input_stream_close_warning_is_invalid(self):
        class WarningStream(io.BytesIO):
            def close(self):
                super().close()
                warnings.warn("private warning")
        module = types.SimpleNamespace(__version__="10.17.0", __libqpdf_version__="12.4.2", open=lambda *a, **k: FakePDF("valid"), PasswordError=PasswordError, PdfError=PdfError, DataDecodingError=PdfError)
        err = io.StringIO()
        with patch.dict(sys.modules, {"pikepdf": module}), patch.object(candidate.io, "BytesIO", WarningStream), contextlib.redirect_stderr(err):
            self.assertEqual(candidate.validate(b"%PDF-fixture"), 2)
        self.assertEqual(err.getvalue(), "")

    def test_warning_survives_stream_close_error(self):
        class ErrorStream(io.BytesIO):
            def close(self):
                super().close()
                raise RuntimeError("private close error")
        module = types.SimpleNamespace(__version__="10.17.0", __libqpdf_version__="12.4.2", open=lambda *a, **k: FakePDF("stream_warning"), PasswordError=PasswordError, PdfError=PdfError, DataDecodingError=PdfError)
        out, err = io.StringIO(), io.StringIO()
        with patch.dict(sys.modules, {"pikepdf": module}), patch.object(candidate.io, "BytesIO", ErrorStream), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.assertEqual(candidate.validate(b"%PDF-fixture"), 2)
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "")

if __name__ == "__main__": unittest.main()
