"""Release subprocess adapter over the repository's exact process ownership.

No CI gate is changed or waived: forced retirement remains explicit evidence,
and an unexpected leftover fails the release command. stdin is private control,
never inherited by the command. stdout/stderr are concurrently drained by Node.
"""
import importlib.util
import json
import os
from pathlib import Path
import signal
import sys
import threading


def main():
    # Keep a separate control descriptor before redirecting the child stdin.
    with os.fdopen(os.dup(0), 'r', encoding='utf-8') as control:
        line = control.readline(1024 * 1024 + 1)
        if len(line) > 1024 * 1024:
            raise ValueError('release_process_request_invalid')
        request = json.loads(line)
        timeout = request['timeout_ms']
        if not isinstance(timeout, int) or isinstance(timeout, bool) or not 1 <= timeout <= 3600000:
            raise ValueError('release_process_timeout_invalid')
        if not isinstance(request['nonce'], str) or len(request['nonce']) != 32:
            raise ValueError('release_process_request_invalid')
        command, environment = request['command'], request['environment']
        if not isinstance(command, list) or not command or not all(isinstance(x, str) and '\0' not in x for x in command):
            raise ValueError('release_process_request_invalid')
        if not isinstance(environment, dict) or not all(isinstance(k, str) and isinstance(v, str) and '\0' not in k + v for k, v in environment.items()):
            raise ValueError('release_process_request_invalid')
        with open(os.devnull, 'rb') as nothing:
            os.dup2(nothing.fileno(), 0)
        spec = importlib.util.spec_from_file_location('release_process_ownership', Path(__file__).with_name('ci-fixture-process.py'))
        owner = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(owner)
        reason = []
        exit_evidence = {}
        primary_exit = []
        lock = threading.Lock()
        completed = threading.Event()

        def stop(value):
            with lock:
                if not reason:
                    reason.append(value)
                    # An already observed failing root is the primary error;
                    # cancelling the remaining descendants must not replace it.
                    known = exit_evidence.get('exit_code')
                    if known is not None and known != 0:
                        primary_exit.append(known)
                owner.cancelled = signal.SIGTERM

        for number in (signal.SIGINT, signal.SIGTERM):
            signal.signal(number, lambda *_: stop('cancelled'))

        def monitor():
            # A cancellation line OR loss of the parent control pipe cancels.
            control.readline(32)
            if not completed.is_set():
                stop('cancelled')

        watcher = threading.Thread(target=monitor, daemon=True)
        watcher.start()
        timer = threading.Timer(timeout / 1000, lambda: stop('timeout'))
        timer.daemon = True
        timer.start()
        try:
            execution_error = None
            try:
                if sys.platform == 'win32':
                    code, forced, retired = owner.windows_run(command, environment, 5, exit_evidence=exit_evidence)
                elif sys.platform == 'linux':
                    code, forced, retired = owner.linux_run(command, environment, 5, exit_evidence=exit_evidence)
                else:
                    raise RuntimeError('release_process_platform_unsupported')
            except Exception:
                # The original ownership boundary has completed its finally.
                # Only a positively unstarted command can be known empty here;
                # a started command without retirement evidence stays unknown.
                retired = exit_evidence.get('started') is False
                execution_error = 'start_failed' if retired else 'execution_failed'
                code, forced = None, False
            root_exit = exit_evidence.get('exit_code')
            first_failed_exit = primary_exit[0] if primary_exit else None
            if not reason and root_exit is not None and root_exit != 0:
                first_failed_exit = root_exit
            result = {'nonce': request['nonce'], 'exit_code': code, 'forced': forced,
                      'root_exit_code': root_exit, 'primary_exit_code': first_failed_exit,
                      'execution_error': execution_error,
                      'cleanup_confirmed': retired, 'reason': reason[0] if reason else None}
            # Descendants have retired before this final control receipt. The
            # random correlation value was never in the command's stdin/env.
            completed.set()
            sys.stderr.write('\nPINVOU_RELEASE_PROCESS ' + json.dumps(result, separators=(',', ':')) + '\n')
            sys.stderr.flush()
            return 0
        finally:
            timer.cancel()


if __name__ == '__main__':
    sys.exit(main())
