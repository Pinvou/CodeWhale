"""Absolute network budgets for Linux CI commands and owned service sockets."""
from contextlib import contextmanager
import signal
import socket
import threading
import time


class DeadlineExceeded(RuntimeError):
    """A terminal overall deadline, never an idle timeout eligible for retry."""


class Deadline:
    def __init__(self, seconds=None, *, end=None):
        self.end = time.monotonic() + seconds if end is None else end
        self.lock = threading.Lock()
        self.sockets = []
        self.timer = None
        self.closed = False
        self.expired = False

    def remaining(self, idle=None):
        remaining = self.end - time.monotonic()
        if remaining <= 0:
            raise DeadlineExceeded('transfer total budget exhausted')
        return min(remaining, idle) if idle is not None else remaining

    def check(self):
        self.remaining()

    def watch(self, connection):
        with self.lock:
            expired = self.expired or self.closed or time.monotonic() >= self.end
            if not expired:
                self.sockets.append(connection)
        if expired:
            self.shutdown(connection)
            raise DeadlineExceeded('transfer total budget exhausted')

    @staticmethod
    def shutdown(connection):
        try:
            # Do not mutate SSLSocket's Python TLS state from another thread.
            socket.socket.shutdown(connection, socket.SHUT_RDWR)
        except OSError:
            pass

    def expire(self):
        with self.lock:
            if self.closed:
                return
            self.expired = True
            connections = list(self.sockets)
        for connection in connections:
            self.shutdown(connection)

    def __enter__(self):
        self.timer = threading.Timer(self.remaining(), self.expire)
        self.timer.daemon = True
        self.timer.start()
        return self

    def __exit__(self, *args):
        with self.lock:
            self.closed = True
            self.sockets.clear()
        self.timer.cancel()
        self.timer.join()


@contextmanager
def command_deadline(end):
    """Interrupt even a continuously readable HTTP parser in a Linux CLI.

    The entrypoints run on Linux. Non-POSIX unit fixtures still check expiry
    after each operation; they are not evidence of a Windows network budget.
    Nested budgets restore the previous alarm's remaining duration.
    """
    budget = Deadline(end=end)
    if not hasattr(signal, 'setitimer') or threading.current_thread() is not threading.main_thread():
        yield budget
        budget.check()
        return
    previous_handler = signal.getsignal(signal.SIGALRM)
    previous_timer = signal.getitimer(signal.ITIMER_REAL)
    started = time.monotonic()

    def expired(signum, frame):
        raise DeadlineExceeded('transfer total budget exhausted')

    seconds = budget.remaining()
    signal.signal(signal.SIGALRM, expired)
    try:
        signal.setitimer(signal.ITIMER_REAL, min(seconds, previous_timer[0]) if previous_timer[0] else seconds)
        yield budget
        budget.check()
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous_handler)
        if previous_timer[0]:
            signal.setitimer(signal.ITIMER_REAL, max(0.000001, previous_timer[0] - (time.monotonic() - started)),
                             previous_timer[1])
