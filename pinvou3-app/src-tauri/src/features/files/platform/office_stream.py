"""Convert one sealed Linux attachment through LibreOffice's local UNO stream.

The caller manages the entire process tree and its deadline. No source bytes
are materialized at a mutable pathname, and no remote UNO listener is opened.
"""
import fcntl
import os
import pathlib
import stat
import subprocess
import sys
import time
import uuid


def pipe_node_path(pipe):
    # Match LibreOffice's Linux OSL path selection; TMPDIR does not select it.
    for root in ("/tmp", "/var/tmp"):
        if os.access(root, os.W_OK):
            parent = pathlib.Path(root)
            info = parent.lstat()
            if (not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.geteuid())
                    or (info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX)):
                raise RuntimeError("unsafe office pipe directory")
            return parent / ("OSL_PIPE_" + str(os.getuid()) + "_" + pipe)
    raise RuntimeError("office pipe directory unavailable")


def checked_pipe_node(node):
    info = node.lstat()  # Never follow a pre-created symlink.
    if (not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid()
            or stat.S_IMODE(info.st_mode) & 0o077):
        raise RuntimeError("unsafe office pipe endpoint")
    return info.st_dev, info.st_ino


def wait_for_context(resolver, pipe, process, clock=time.monotonic, pause=time.sleep):
    node = pipe_node_path(pipe)
    deadline = clock() + 60
    while True:
        try:
            identity = checked_pipe_node(node)
        except FileNotFoundError:
            identity = None
        if identity is not None:
            try:
                context = resolver.resolve("uno:pipe,name=" + pipe + ";urp;StarOffice.ComponentContext")
            except Exception:
                pass  # A private socket can appear before UNO is ready.
            else:
                # The trusted sticky parent prevents replacement by other UIDs.
                # Check again before any services or attachment bytes are sent.
                if checked_pipe_node(node) != identity:
                    raise RuntimeError("office pipe endpoint changed")
                return context
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
    node = pipe_node_path(pipe)
    try:
        node.lstat()
    except FileNotFoundError:
        pass
    else:
        # Do not connect to, remove, or chmod a pre-existing endpoint.
        raise RuntimeError("office pipe endpoint already exists")
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
