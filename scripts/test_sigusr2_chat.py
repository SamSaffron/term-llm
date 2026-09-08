#!/usr/bin/env python3
"""Linux PTY chat reload proof using an isolated debug-provider configuration."""
import argparse
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import sqlite3
import struct
import subprocess
import tempfile
import termios
import threading
import time
from test_sigusr2_reload import until


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary-a', required=True, type=Path)
    parser.add_argument('--binary-b', required=True, type=Path)
    parser.add_argument('--exit-only', action='store_true', help='test fresh chat, hello, double Ctrl-C and resume hint without reload')
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='term-llm-chat-reload-') as temp:
        root = Path(temp)
        installed = root / 'term-llm'
        shutil.copy2(args.binary_a, installed)
        env = {'PATH': os.environ.get('PATH', os.defpath), 'SHELL': '/bin/sh', 'TERM': 'xterm-256color', 'LANG': 'C.UTF-8'}
        for key, name in [('HOME', 'home'), ('XDG_CONFIG_HOME', 'config'), ('XDG_DATA_HOME', 'data'), ('XDG_CACHE_HOME', 'cache'), ('XDG_RUNTIME_DIR', 'runtime')]:
            (root / name).mkdir(mode=0o700)
            env[key] = str(root / name)
        cfg = root / 'config/term-llm'
        cfg.mkdir()
        (cfg / 'config.yaml').write_text('default_provider: debug\nproviders:\n  debug:\n    model: fast\n    fast_model: debug:fast\n')
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
        def attach_tty():
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
        child = subprocess.Popen([str(installed), 'chat', '--provider', 'debug:fast', '--tools', 'shell', '--yolo'], cwd=root, env=env, stdin=slave, stdout=slave, stderr=slave, preexec_fn=attach_tty)
        os.close(slave)
        output = bytearray()
        stop = threading.Event()
        def consume():
            while not stop.is_set():
                if not select.select([master], [], [], .1)[0]:
                    continue
                try:
                    data = os.read(master, 65536)
                except OSError as err:
                    if err.errno == errno.EIO:
                        return
                    raise
                if not data:
                    return
                output.extend(data)
                # Respond to terminal queries rather than waiting for startup timeout.
                if b'\x1b[6n' in data:
                    os.write(master, b'\x1b[1;1R')
                if b'\x1b[c' in data:
                    os.write(master, b'\x1b[?1;2c')
        reader = threading.Thread(target=consume, daemon=True)
        reader.start()
        def record():
            if child.poll() is not None:
                raise AssertionError(f'chat exited {child.returncode}: {output.decode(errors="replace")}')
            try:
                return json.loads((root / f'runtime/term-llm-processes/{child.pid}.json').read_text())
            except FileNotFoundError:
                return {}
        def ready():
            value = record()
            return value if value.get('phase') == 'ready' else None
        try:
            before = until(ready, 'chat boot ready')
            time.sleep(1)
            if args.exit_only:
                os.write(master, b'hello\r')
                def completed_session():
                    db = root/'data/term-llm/sessions.db'
                    if not db.exists():
                        return None
                    with sqlite3.connect(db) as conn:
                        return conn.execute("SELECT number FROM sessions WHERE user_turns > 0 AND llm_turns > 0 AND status = 'complete' ORDER BY created_at DESC LIMIT 1").fetchone()
                saved = until(completed_session, 'hello persisted and completed')
                time.sleep(.5)
                os.write(master, b'\x03')
                time.sleep(.2)
                os.write(master, b'\x03')
                assert child.wait(timeout=8) == 0, output.decode(errors='replace')
                reader.join(timeout=2)
                expected = f'Resume: term-llm chat --resume={saved[0]}'.encode()
                assert expected in output, output.decode(errors='replace')
                assert output.count(expected) == 1
                print('PASS: fresh chat, hello, double Ctrl-C prints exactly one numbered resume hint')
                return
            os.write(master, b'shell echo started >> effects; while [ ! -f release ]; do sleep .05; done; echo finished >> effects\r')
            until(lambda: (root / 'effects').exists(), 'chat tool started')
            shutil.copy2(args.binary_b, root / 'replacement')
            (root / 'replacement').replace(installed)
            os.kill(child.pid, signal.SIGUSR2)
            until(lambda: record().get('phase') == 'draining', 'chat drain')
            assert (root / 'effects').read_text().splitlines() == ['started']
            (root / 'release').touch()
            after = until(lambda: r if (r := ready()) and r['instance'] != before['instance'] else None, 'chat replacement ready', timeout=25)
            assert after['build_id'] != before['build_id']
            assert after['pid'] == before['pid']
            assert (root / 'effects').read_text().splitlines() == ['started', 'finished']
            time.sleep(1)
            os.write(master, b'draft-preserved')
            time.sleep(.3)
            installed.chmod(0o600)
            os.kill(child.pid, signal.SIGUSR2)
            until(lambda: (r := ready()) and r.get('last_error'), 'chat failed exec rollback')
            assert record()['instance'] == after['instance']
            installed.chmod(0o700)
            marker = len(output)
            os.kill(child.pid, signal.SIGUSR2)
            until(lambda: (r := ready()) and r['instance'] != after['instance'], 'chat idle retry')
            until(lambda: b'draft-preserved' in output[marker:], 'restored draft display')
            print('PASS: PTY chat active tool drain, distinct build same PID, failed exec terminal rollback, idle retry and draft restoration')
        except BaseException:
            if child.poll() is None:
                print('Registry:', record())
            print(output.decode(errors='replace'))
            raise
        finally:
            (root / 'release').touch()
            child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
            stop.set()
            reader.join(timeout=2)
            os.close(master)


if __name__ == '__main__':
    main()
