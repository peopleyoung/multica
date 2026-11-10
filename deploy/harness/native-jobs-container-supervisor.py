#!/usr/bin/env python3
"""Keep native-jobs independent of SSH sessions in a container without init."""

import argparse
import fcntl
import os
import pathlib
import signal
import subprocess
import sys
import time


def supervise(args):
    state = pathlib.Path(args.state_dir)
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(str(state), 0o700)
    with (state / "supervisor.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        (state / "supervisor.pid").write_text(str(os.getpid()))
        stopping = False
        child = None

        def stop(_sig, _frame):
            nonlocal stopping
            stopping = True
            if child is not None and child.poll() is None:
                child.terminate()

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        try:
            while not stopping:
                try:
                    child = subprocess.Popen([args.binary, "--config", args.config])
                    code = child.wait()
                    if not stopping:
                        print("native-jobs exited (%d); retry in 10 seconds" % code, flush=True)
                except OSError as error:
                    print("native-jobs start failed: %s" % error, flush=True)
                for _ in range(10):
                    if stopping:
                        break
                    time.sleep(1)
        finally:
            if child is not None and child.poll() is None:
                child.terminate()
                child.wait(timeout=45)
            (state / "supervisor.pid").unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["ensure", "supervise"])
    parser.add_argument("--binary", default="/opt/multica-harness/bin/native-jobs")
    parser.add_argument("--config", default="/etc/multica-harness/native-jobs.json")
    parser.add_argument("--state-dir", default="/var/lib/multica-harness-jobs")
    args = parser.parse_args()
    os.umask(0o077)
    if args.action == "supervise":
        supervise(args)
        return
    state = pathlib.Path(args.state_dir)
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    # Only the supervisor holds this lock. Repeated ensures never replace it.
    with (state / "supervisor.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
    with (state / "supervisor.log").open("ab", buffering=0) as log:
        subprocess.Popen(
            [sys.executable, "-u", str(pathlib.Path(__file__).resolve()), "supervise",
             "--binary", args.binary, "--config", args.config, "--state-dir", args.state_dir],
            stdin=subprocess.DEVNULL, stdout=log, stderr=log,
            close_fds=True, start_new_session=True,
        )


if __name__ == "__main__":
    main()
