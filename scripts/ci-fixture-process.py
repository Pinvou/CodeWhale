"""Run one test command in an owned process tree and confirm retirement.

Linux uses a subreaper so detached browser grandchildren cannot escape by
changing process groups. Windows assigns a SUSPENDED child to a fresh nested
Job Object before its first instruction. Never targets a Runner's parent job.
"""
import argparse
import ctypes
import faulthandler
import hashlib
import json
import ntpath
import os
import re
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

cancelled = 0
diagnostics = False
diagnostic_start = 0


def diagnostic_phase(name):
    if diagnostics:
        # Never include command arguments, environment values or fixture data.
        try:
            print(json.dumps({'fixture_phase': name, 'elapsed_seconds': round(time.monotonic() - diagnostic_start, 3)}), file=sys.stderr, flush=True)
            faulthandler.cancel_dump_traceback_later()
            faulthandler.dump_traceback_later(3, repeat=False)
        except (OSError, ValueError, RuntimeError):
            pass  # Diagnostic delivery cannot replace the command/cleanup verdict.


def cancel(signum, _frame):
    global cancelled
    cancelled = signum


def linux_children():
    result = []
    for item in Path('/proc').iterdir():
        if not item.name.isdecimal():
            continue
        try:
            # comm may contain spaces and parentheses; split after its LAST ).
            fields = (item / 'stat').read_text().rsplit(')', 1)[1].split()
            if int(fields[1]) == os.getpid():
                result.append((int(item.name), fields[0]))
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
    return result


def linux_run(command, env, grace, *, exit_evidence=None):
    # Optional observation for release callers. It never changes the existing
    # cancellation return code, retirement verdict, reaping or cleanup gate.
    if exit_evidence is not None:
        if type(exit_evidence) is not dict:
            raise TypeError('root exit evidence must be a plain dictionary')
        exit_evidence.clear()
        exit_evidence['started'] = False
    libc = ctypes.CDLL(None, use_errno=True)
    parent = os.getppid()
    if libc.prctl(36, 1, 0, 0, 0) or libc.prctl(1, signal.SIGTERM, 0, 0, 0):
        raise OSError(ctypes.get_errno(), 'owned test subreaper unavailable')
    if os.getppid() != parent:
        return 143, True, False
    child = subprocess.Popen(command, env=env, start_new_session=True)
    if exit_evidence is not None:
        exit_evidence['started'] = True
    ended = None
    forcing = False
    dirty = False
    deadline = None
    while True:
        if ended is None:
            ended = os.waitid(os.P_PID, child.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
        if exit_evidence is not None and ended is not None and 'exit_code' not in exit_evidence:
            exit_evidence['exit_code'] = ended.si_status if ended.si_code == os.CLD_EXITED else 128 + ended.si_status
        children = linux_children()
        alive = [pid for pid, state in children if state != 'Z' and (pid != child.pid or ended is None)]
        # Keep the root zombie unreaped until all descendants are retired. Its
        # PID cannot be reused while cancellation/cleanup still addresses it.
        for pid, state in children:
            if state == 'Z' and pid != child.pid:
                os.waitpid(pid, os.WNOHANG)
        if ended is not None and not alive:
            _, status = os.waitpid(child.pid, 0)
            child.returncode = os.waitstatus_to_exitcode(status)
            code = child.returncode if child.returncode >= 0 else 128 - child.returncode
            return (128 + cancelled if cancelled else code), dirty, True
        if cancelled or os.getppid() != parent:
            if deadline is None:
                deadline = time.monotonic() + grace
                for pid in alive:
                    try:
                        os.kill(pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass  # An unreaped owned child cannot have a reused PID.
        elif ended is not None and deadline is None:
            deadline = time.monotonic() + grace
        if deadline is not None and time.monotonic() >= deadline:
            dirty = True
            for pid in alive:
                try:
                    os.kill(pid, signal.SIGKILL if forcing else signal.SIGTERM)
                except ProcessLookupError:
                    pass
            if forcing:
                # Still permit newly adopted grandchildren to be observed.
                if time.monotonic() >= deadline + grace:
                    return 86, True, False
            else:
                forcing = True
                deadline = time.monotonic() + grace
        time.sleep(0.025)


def windows_run(command, env, grace, *, exit_evidence=None):
    if exit_evidence is not None:
        if type(exit_evidence) is not dict:
            raise TypeError('root exit evidence must be a plain dictionary')
        exit_evidence.clear()
        exit_evidence['started'] = False
    from ctypes import wintypes as w
    kernel = ctypes.WinDLL('kernel32', use_last_error=True)

    class IO(ctypes.Structure):
        _fields_ = [(name, ctypes.c_ulonglong) for name in ('ReadOperationCount', 'WriteOperationCount', 'OtherOperationCount', 'ReadTransferCount', 'WriteTransferCount', 'OtherTransferCount')]

    class Limits(ctypes.Structure):
        _fields_ = [('PerProcessUserTimeLimit', ctypes.c_longlong), ('PerJobUserTimeLimit', ctypes.c_longlong), ('LimitFlags', w.DWORD), ('MinimumWorkingSetSize', ctypes.c_size_t), ('MaximumWorkingSetSize', ctypes.c_size_t), ('ActiveProcessLimit', w.DWORD), ('Affinity', ctypes.c_size_t), ('PriorityClass', w.DWORD), ('SchedulingClass', w.DWORD)]

    class Extended(ctypes.Structure):
        _fields_ = [('BasicLimitInformation', Limits), ('IoInfo', IO), ('ProcessMemoryLimit', ctypes.c_size_t), ('JobMemoryLimit', ctypes.c_size_t), ('PeakProcessMemoryUsed', ctypes.c_size_t), ('PeakJobMemoryUsed', ctypes.c_size_t)]

    class Accounting(ctypes.Structure):
        _fields_ = [('TotalUserTime', ctypes.c_longlong), ('TotalKernelTime', ctypes.c_longlong), ('ThisPeriodTotalUserTime', ctypes.c_longlong), ('ThisPeriodTotalKernelTime', ctypes.c_longlong), ('TotalPageFaultCount', w.DWORD), ('TotalProcesses', w.DWORD), ('ActiveProcesses', w.DWORD), ('TotalTerminatedProcesses', w.DWORD)]

    class Startup(ctypes.Structure):
        _fields_ = [('cb', w.DWORD), ('lpReserved', w.LPWSTR), ('lpDesktop', w.LPWSTR), ('lpTitle', w.LPWSTR), ('dwX', w.DWORD), ('dwY', w.DWORD), ('dwXSize', w.DWORD), ('dwYSize', w.DWORD), ('dwXCountChars', w.DWORD), ('dwYCountChars', w.DWORD), ('dwFillAttribute', w.DWORD), ('dwFlags', w.DWORD), ('wShowWindow', w.WORD), ('cbReserved2', w.WORD), ('lpReserved2', ctypes.POINTER(ctypes.c_byte)), ('hStdInput', w.HANDLE), ('hStdOutput', w.HANDLE), ('hStdError', w.HANDLE)]

    class Process(ctypes.Structure):
        _fields_ = [('hProcess', w.HANDLE), ('hThread', w.HANDLE), ('dwProcessId', w.DWORD), ('dwThreadId', w.DWORD)]

    signatures = {
        'CreateJobObjectW': ([ctypes.c_void_p, w.LPCWSTR], w.HANDLE),
        'SetInformationJobObject': ([w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD], w.BOOL),
        'QueryInformationJobObject': ([w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD, ctypes.c_void_p], w.BOOL),
        'CreateProcessW': ([w.LPCWSTR, w.LPWSTR, ctypes.c_void_p, ctypes.c_void_p, w.BOOL, w.DWORD, ctypes.c_void_p, w.LPCWSTR, ctypes.POINTER(Startup), ctypes.POINTER(Process)], w.BOOL),
        'AssignProcessToJobObject': ([w.HANDLE, w.HANDLE], w.BOOL),
        'ResumeThread': ([w.HANDLE], w.DWORD),
        'WaitForSingleObject': ([w.HANDLE, w.DWORD], w.DWORD),
        'GetExitCodeProcess': ([w.HANDLE, ctypes.POINTER(w.DWORD)], w.BOOL),
        'TerminateJobObject': ([w.HANDLE, w.UINT], w.BOOL),
        'TerminateProcess': ([w.HANDLE, w.UINT], w.BOOL),
        'GetStdHandle': ([w.DWORD], w.HANDLE),
        'SetHandleInformation': ([w.HANDLE, w.DWORD, w.DWORD], w.BOOL),
        'OpenProcess': ([w.DWORD, w.BOOL, w.DWORD], w.HANDLE),
        'CloseHandle': ([w.HANDLE], w.BOOL),
    }
    for name, (args, result) in signatures.items():
        function = getattr(kernel, name)
        function.argtypes, function.restype = args, result

    def checked(value):
        if not value:
            raise ctypes.WinError(ctypes.get_last_error())
        return value

    diagnostic_phase('create_job')
    job = checked(kernel.CreateJobObjectW(None, None))
    process = Process()
    parent = kernel.OpenProcess(0x100000, False, os.getppid())
    assigned = False
    try:
        if not parent:
            raise RuntimeError('test parent process could not be observed')
        limits = Extended()
        limits.BasicLimitInformation.LimitFlags = 0x2000  # KILL_ON_JOB_CLOSE
        checked(kernel.SetInformationJobObject(job, 9, ctypes.byref(limits), ctypes.sizeof(limits)))
        startup = Startup()
        startup.cb, startup.dwFlags = ctypes.sizeof(startup), 0x100
        for attribute, number in [('hStdInput', -10), ('hStdOutput', -11), ('hStdError', -12)]:
            handle = kernel.GetStdHandle(number)
            setattr(startup, attribute, handle)
            if handle and handle != ctypes.c_void_p(-1).value:
                checked(kernel.SetHandleInformation(handle, 1, 1))
        executable = shutil.which(command[0])
        if not executable:
            raise FileNotFoundError('test executable unavailable')
        command_line = ctypes.create_unicode_buffer(subprocess.list2cmdline([executable, *command[1:]]))
        environment = ctypes.create_unicode_buffer('\0'.join(f'{k}={v}' for k, v in sorted(env.items())) + '\0\0')
        diagnostic_phase('create_process')
        checked(kernel.CreateProcessW(executable, command_line, None, None, True, 0x4 | 0x400 | 0x200, environment, None, ctypes.byref(startup), ctypes.byref(process)))
        if exit_evidence is not None:
            exit_evidence['started'] = True
        diagnostic_phase('assign_process')
        checked(kernel.AssignProcessToJobObject(job, process.hProcess))
        assigned = True
        diagnostic_phase('resume_process')
        if kernel.ResumeThread(process.hThread) == 0xffffffff:
            raise ctypes.WinError(ctypes.get_last_error())
        deadline = None
        dirty = False
        diagnostic_phase('observe_process_tree')
        observation_started = time.monotonic()
        observed_stall = False
        while True:
            accounting = Accounting()
            checked(kernel.QueryInformationJobObject(job, 1, ctypes.byref(accounting), ctypes.sizeof(accounting), None))
            ended = kernel.WaitForSingleObject(process.hProcess, 0) == 0
            if exit_evidence is not None and ended and 'exit_code' not in exit_evidence:
                root_code = w.DWORD()
                checked(kernel.GetExitCodeProcess(process.hProcess, ctypes.byref(root_code)))
                exit_evidence['exit_code'] = root_code.value
            if ended and accounting.ActiveProcesses == 0:
                code = w.DWORD()
                checked(kernel.GetExitCodeProcess(process.hProcess, ctypes.byref(code)))
                return (128 + cancelled if cancelled else code.value), dirty, True
            orphaned = kernel.WaitForSingleObject(parent, 0) == 0
            if diagnostics and not observed_stall and time.monotonic() - observation_started >= 3:
                observed_stall = True
                try:
                    print(json.dumps({'fixture_process_state': {'root_exited': ended, 'active_processes': accounting.ActiveProcesses, 'parent_exited': orphaned}}), file=sys.stderr, flush=True)
                except (OSError, ValueError):
                    pass
            if (ended or cancelled or orphaned) and deadline is None:
                deadline = time.monotonic() + grace
            if deadline is not None and time.monotonic() >= deadline:
                dirty = True
                checked(kernel.TerminateJobObject(job, 86))
                if time.monotonic() >= deadline + grace:
                    return 86, True, False
            time.sleep(0.025)
    finally:
        diagnostic_phase('close_process_handles')
        if process.hProcess and not assigned:
            kernel.TerminateProcess(process.hProcess, 86)  # Never resumed outside its job.
        for handle in (process.hThread, process.hProcess, parent, job):
            if handle:
                kernel.CloseHandle(handle)


def atomic_receipt(path, value):
    temporary = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    temporary.write_text(json.dumps(value), encoding='utf-8')
    os.replace(temporary, path)


def windows_extended_path(path):
    value = ntpath.normpath(str(path))
    if not ntpath.isabs(value):
        raise ValueError('owned Windows workspace path must be absolute')
    prefix = '\\\\?\\'
    if value.startswith(prefix):
        return value
    if value.startswith('\\\\'):
        return prefix + 'UNC\\' + value[2:]
    return prefix + value


def remove_owned_workspace(root, identity):
    current = root.lstat()
    if root.is_symlink() or (current.st_dev, current.st_ino) != identity:
        raise RuntimeError('test workspace identity changed')
    # Node/npm can create legal paths beyond MAX_PATH. Keep the same verified
    # owned directory, expressed through Windows' extended path namespace.
    target = windows_extended_path(root) if sys.platform == 'win32' else root
    shutil.rmtree(target)


def main(argv=None):
    global diagnostics, diagnostic_start
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--receipt', type=Path, required=True)
    parser.add_argument('--cleanup-seconds', type=float, default=5)
    parser.add_argument('--short-workspace-parent', type=Path)
    parser.add_argument('--diagnostics', action='store_true', help='bounded stage and stalled Python stack evidence; does not change deadlines')
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    diagnostics, diagnostic_start = args.diagnostics, time.monotonic()
    diagnostic_phase('prepare_workspace')
    if not 0 < args.cleanup_seconds <= 15:
        raise ValueError('cleanup deadline out of range')
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        raise ValueError('test command required')
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, cancel)
    # Chrome's Linux SingletonSocket is an AF_UNIX pathname (108 bytes max).
    # Runner TMPDIR can already exceed that budget before Chrome appends its
    # own directory and socket name. Keep the private scope under the short
    # system temporary path; never alter the Runner's global environment.
    parent = None
    if args.short_workspace_parent is not None:
        parent = args.short_workspace_parent
        if (sys.platform != 'win32' or not parent.is_absolute() or
                not re.fullmatch(r'[A-Za-z]:', parent.drive) or
                len(str(parent)) + 10 > 35 or any(c.isspace() for c in str(parent)) or
                not parent.is_dir() or parent == Path(parent.anchor)):
            raise ValueError('short workspace parent must be an existing short local Windows directory')
        for ancestor in (parent, *parent.parents):
            if ancestor.is_symlink() or getattr(ancestor.lstat(), 'st_file_attributes', 0) & 0x400:
                raise ValueError('short workspace parent must not traverse a reparse point')
    ledger = args.receipt.parent / (args.receipt.stem + '-resources')
    ledger.mkdir()
    receipt = {'schema': 1, 'kind': 'process', 'id': uuid.uuid4().hex, 'state': 'registered', 'pid': os.getpid(), 'cleanup_failed': True}
    scope = uuid.uuid4().hex
    root = None; identity = None; created = False
    try:
        if parent is not None:
            # Persist an intent before creating anything outside the default
            # TEMP. Cancellation here remains visible and fails closed; an
            # allocating receipt never authorizes deletion without identity.
            root = parent / ('p' + uuid.uuid4().hex[:8])
            receipt.update(state='allocating', workspace={'path': str(root), 'dev': None, 'ino': None})
            atomic_receipt(args.receipt, receipt)
            root.mkdir()  # exclusive: never adopt a pre-existing directory
        else:
            root = Path(tempfile.mkdtemp(prefix='pinvou-ci-fixture-', dir='/tmp' if sys.platform == 'linux' else None))
        created = True
        receipt['workspace'] = {'path': str(root), 'dev': None, 'ino': None}
        identity = root.stat()
        receipt.update(state='registered', workspace={'path': str(root), 'dev': identity.st_dev, 'ino': identity.st_ino})
        (root / '.pinvou-fixture-scope').write_text(scope, encoding='ascii')
        if sys.platform == 'linux':
            receipt['scope'] = {'id': scope, 'uid': os.getuid(), 'cgroup': hashlib.sha256(Path('/proc/self/cgroup').read_bytes()).hexdigest(), 'controller_start': Path('/proc/self/stat').read_text().rsplit(')', 1)[1].split()[19]}
        atomic_receipt(args.receipt, receipt)
    except BaseException as error:
        receipt.update(state='unconfirmed', cleanup_failed=True, error_class=type(error).__name__)
        if created and identity is not None:
            try:
                remove_owned_workspace(root, (identity.st_dev, identity.st_ino))
            except (OSError, RuntimeError) as cleanup_error:
                receipt['cleanup_error_class'] = type(cleanup_error).__name__
            else:
                receipt['state'] = 'released'
        try:
            atomic_receipt(args.receipt, receipt)
        except OSError:
            pass  # retain the original failure and any already-persisted intent
        raise
    env = {**os.environ, 'PINVOU_CI_PROCESS_SCOPE': scope, 'PINVOU_CI_FIXTURE_ROOT': str(root), 'PINVOU_CI_RESOURCE_LEDGER': str(ledger), 'TMPDIR': str(root), 'TMP': str(root), 'TEMP': str(root)}
    try:
        if sys.platform == 'linux':
            diagnostic_phase('observe_process_tree')
            code, dirty, clean = linux_run(command, env, args.cleanup_seconds)
        elif sys.platform == 'win32':
            code, dirty, clean = windows_run(command, env, args.cleanup_seconds)
        else:
            raise RuntimeError('owned process-tree observation is supported on Linux and Windows')
        # An in-test cleanup failure stays a failure even if OS containment was
        # subsequently able to retire the process. Never hide it with a retry.
        diagnostic_phase('validate_retirement_receipts')
        for record in ledger.glob('*.json'):
            if record.is_symlink() or record.stat().st_size > 65536:
                dirty = True
                continue
            value = json.loads(record.read_text(encoding='utf-8'))
            if value.get('schema') != 1 or value.get('state') != 'released':
                dirty = True
        if clean:
            diagnostic_phase('retire_workspace')
            remove_owned_workspace(root, (identity.st_dev, identity.st_ino))
        receipt.update(state='released' if clean else 'unconfirmed', cleanup_failed=dirty or not clean, exit_code=code)
        return code if code else 86 if receipt['cleanup_failed'] else 0
    except Exception as error:
        receipt.update(state='unconfirmed', cleanup_failed=True, error_class=type(error).__name__)
        raise
    finally:
        diagnostic_phase('final_receipt')
        atomic_receipt(args.receipt, receipt)
        if diagnostics:
            faulthandler.cancel_dump_traceback_later()


if __name__ == '__main__':
    sys.exit(main())
