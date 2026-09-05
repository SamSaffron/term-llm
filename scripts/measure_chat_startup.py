#!/usr/bin/env python3
"""Compare POSIX chat launch latency through the first complete composer frame.

Usage: python3 scripts/measure_chat_startup.py --temp-dir . /path/to/before /path/to/after
Uses isolated configuration, a trusted temporary workspace, local tools, and
copies of the same initialized databases. No prompt is submitted; the provider
has a placeholder credential and a loopback endpoint. Results are warm-launch
measurements, not cold-cache startup or provider-request benchmarks.
"""

import argparse
import fcntl
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import sqlite3
import statistics
import struct
import tempfile
import termios
import time


def stop_child(pid):
    try:
        os.killpg(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        if os.waitpid(pid, os.WNOHANG)[0]:
            return
        time.sleep(0.005)
    os.killpg(pid, signal.SIGKILL)
    os.waitpid(pid, 0)


def launch(binary, root, data_dir):
    started = time.perf_counter()
    pid, fd = pty.fork()
    if pid == 0:
        try:
            for key in list(os.environ):
                if key.startswith("TERM_LLM_"):
                    del os.environ[key]
            os.environ.update(
                HOME=str(root / "home"),
                XDG_CONFIG_HOME=str(root / "config"),
                XDG_DATA_HOME=str(data_dir),
                XDG_CACHE_HOME=str(root / "cache"),
                XDG_STATE_HOME=str(root / "state"),
                TERM="xterm-256color",
                TERM_LLM_SKIP_UPDATE_CHECK="1",
            )
            os.chdir(root / "work")
            fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
            os.execv(str(binary), [str(binary), "chat", "--provider", "openai",
                                   "--approval", "prompt", "--tools",
                                   "read_file,write_file,edit_file,shell,glob,grep"])
        except Exception as exc:
            print(exc, flush=True)
            os._exit(1)
    output = b""
    try:
        while time.perf_counter() - started < 10:
            if not select.select([fd], [], [], 0.05)[0]:
                continue
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                break
            output += chunk
            if b"\x1b[6n" in chunk:
                os.write(fd, b"\x1b[1;1R")
            if b"\x1b[c" in chunk:
                os.write(fd, b"\x1b[?1;2c")
            if b"Type a message..." in output and b"\x1b[?25h" in output:
                return (time.perf_counter() - started) * 1000
        raise RuntimeError(f"No composer frame from {binary}: {output[-4000:]!r}")
    finally:
        stop_child(pid)
        os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binaries", nargs="+", type=Path)
    parser.add_argument("--runs", type=int, default=40)
    parser.add_argument("--warmups", type=int, default=4)
    parser.add_argument("--temp-dir", type=Path, default=None,
                        help="Fixture parent; use a disk-backed directory to measure SQLite I/O (often /tmp is tmpfs)")
    args = parser.parse_args()
    if args.runs < 1 or args.warmups < 0:
        parser.error("runs must be positive and warmups nonnegative")
    binaries = [p.resolve(strict=True) for p in args.binaries]
    with tempfile.TemporaryDirectory(prefix="term-llm-startup-", dir=args.temp_dir) as temp:
        root = Path(temp).resolve()
        for directory in ["home", "config/term-llm", "work", "seed"]:
            (root / directory).mkdir(parents=True)
        (root / "config/term-llm/config.yaml").write_text(
            "default_provider: openai\nproviders:\n  openai:\n"
            "    api_key: benchmark-placeholder\n    model: gpt-4.1\n"
            "    base_url: http://127.0.0.1:9/v1\n"
            "chat:\n  approval_mode: prompt\n"
            "tools:\n  read_dirs: ['.']\n  write_dirs: ['.']\n"
        )
        trust = root / "config/term-llm/remembered-workspaces.yaml"
        trust.write_text(f"version: 1\nworkspaces:\n  - path: {root / 'work'}\n")
        trust.chmod(0o600)
        launch(binaries[0], root, root / "seed")
        # The process-wide tracking stores may leave WAL files at exit. Use
        # SQLite to checkpoint the seed rather than copy recovery work per run.
        for path in (root / "seed").rglob("*.db"):
            db = sqlite3.connect(path)
            try:
                db.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchall()
            finally:
                db.close()
        samples = {binary: [] for binary in binaries}
        for iteration in range(args.warmups + args.runs):
            order = binaries if iteration % 2 else list(reversed(binaries))
            for binary in order:
                with tempfile.TemporaryDirectory(dir=root) as data:
                    shutil.copytree(root / "seed", data, dirs_exist_ok=True)
                    elapsed = launch(binary, root, Path(data))
                if iteration >= args.warmups:
                    samples[binary].append(elapsed)
        baseline = statistics.median(samples[binaries[0]])
        for binary, values in samples.items():
            median = statistics.median(values)
            p90 = sorted(values)[min(len(values) - 1, int(len(values) * 0.9))]
            print(f"{binary}: median {median:.3f} ms; p90 {p90:.3f} ms; "
                  f"{(baseline - median) / baseline:.1%} faster than baseline "
                  f"({len(values)} runs)")


if __name__ == "__main__":
    main()
