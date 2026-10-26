"""Convert one sealed Linux attachment through LibreOffice's local UNO stream.

The caller manages the entire process tree and its deadline. No source bytes
are materialized at a mutable pathname, and no remote UNO listener is opened.
"""
import fcntl
import os
import pathlib
import subprocess
import sys
import time
import uuid


def wait_for_context(resolver, pipe, process, clock=time.monotonic, pause=time.sleep):
    deadline = clock() + 60
    while True:
        try:
            return resolver.resolve("uno:pipe,name=" + pipe + ";urp;StarOffice.ComponentContext")
        except Exception:
            if clock() >= deadline or process.poll() is not None:
                raise RuntimeError("office stream startup failed") from None
            pause(0.05)


def convert():
    import uno
    from com.sun.star.document.MacroExecMode import NEVER_EXECUTE
    from com.sun.star.document.UpdateDocMode import NO_UPDATE

    os.umask(0o077)
    source, output, conversion, executable, maximum = sys.argv[1:]
    output = pathlib.Path(output)
    with open(source, "rb") as handle:
        required = fcntl.F_SEAL_WRITE | fcntl.F_SEAL_GROW | fcntl.F_SEAL_SHRINK | fcntl.F_SEAL_SEAL
        if fcntl.fcntl(handle.fileno(), fcntl.F_GET_SEALS) & required != required:
            raise ValueError("unsealed source")
        data = handle.read(int(maximum) + 1)
        if len(data) > int(maximum):
            raise ValueError("source exceeds bound")

    def property_value(name, value):
        prop = uno.createUnoStruct("com.sun.star.beans.PropertyValue")
        prop.Name, prop.Value = name, value
        return prop

    pipe = "pinvou_" + uuid.uuid4().hex
    process = subprocess.Popen(
        [executable, "-env:UserInstallation=" + (output / "profile").as_uri(),
         "--headless", "--norestore", "--nodefault", "--nofirststartwizard",
         "--accept=pipe,name=" + pipe + ";urp;StarOffice.ComponentContext"],
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    document = None
    desktop = None
    try:
        local = uno.getComponentContext()
        resolver = local.ServiceManager.createInstanceWithContext("com.sun.star.bridge.UnoUrlResolver", local)
        context = wait_for_context(resolver, pipe, process)
        services = context.ServiceManager
        stream = services.createInstanceWithContext("com.sun.star.io.SequenceInputStream", context)
        stream.initialize((uno.ByteSequence(data),))
        desktop = services.createInstanceWithContext("com.sun.star.frame.Desktop", context)
        document = desktop.loadComponentFromURL("private:stream", "_blank", 0, (
            property_value("InputStream", stream), property_value("Hidden", True),
            property_value("ReadOnly", True), property_value("MacroExecutionMode", NEVER_EXECUTE),
            property_value("UpdateDocMode", NO_UPDATE),
        ))
        if document is None:
            raise RuntimeError("office stream input rejected")
        parts = conversion.split(":", 2)
        extension = parts[0]
        if len(parts) > 1:
            filter_name = parts[1]
        else:
            family = "impress" if document.supportsService("com.sun.star.presentation.PresentationDocument") else (
                "calc" if document.supportsService("com.sun.star.sheet.SpreadsheetDocument") else "writer"
            )
            filters = {
                ("pdf", "impress"): "impress_pdf_Export", ("pdf", "calc"): "calc_pdf_Export",
                ("pdf", "writer"): "writer_pdf_Export", ("html", "impress"): "impress_html_Export",
                ("html", "calc"): "HTML (StarCalc)", ("html", "writer"): "HTML (StarWriter)",
            }
            filter_name = filters[(extension, family)]
        properties = [property_value("FilterName", filter_name), property_value("Overwrite", True)]
        if len(parts) > 2:
            properties.append(property_value("FilterOptions", parts[2]))
        target = output / (pathlib.Path(source).stem + "." + extension)
        document.storeToURL(target.as_uri(), tuple(properties))
    finally:
        try:
            if document is not None:
                document.dispose()
        finally:
            # Keep soffice in the Rust-owned process group. A new session/group
            # here would escape the caller's timeout and cancellation boundary.
            if desktop is not None:
                try:
                    desktop.terminate()
                except Exception:
                    pass
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


if __name__ == "__main__":
    try:
        convert()
    except ModuleNotFoundError:
        print("office_stream_dependency_missing", file=sys.stderr)
        sys.exit(78)
    except Exception:
        # Keep bytes, B-local paths and document diagnostics out of the wire.
        print("office_stream_conversion_failed", file=sys.stderr)
        sys.exit(1)
