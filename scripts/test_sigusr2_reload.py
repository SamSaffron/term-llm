#!/usr/bin/env python3
"""Isolated Linux acceptance proof for SIGUSR2. Never targets an existing service.

Build two different versions, then run:
  python3 scripts/test_sigusr2_reload.py --binary-a ./build-a --binary-b ./build-b

Uses a temporary HOME/XDG registry, loopback MCP server and one owned child PID.
No provider/API calls, production config, databases, or installed binary changes.
"""

import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import urllib.request


def until(check, description, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.02)
    raise AssertionError(f"timed out: {description}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary-a", required=True, type=Path)
    parser.add_argument("--binary-b", required=True, type=Path)
    parser.add_argument("--symlink", action="store_true", help="upgrade the invocation symlink instead of its target")
    parser.add_argument("--hold-drain", type=float, default=0,
                        help="hold admitted work during drain (0-20 seconds; stays within the 30-second cancellation grace period)")
    args = parser.parse_args()
    if not 0 <= args.hold_drain <= 20:
        parser.error("--hold-drain must be between 0 and 20 seconds")
    if not hasattr(signal, "SIGUSR2") or not Path("/proc").exists():
        parser.error("this proof requires Linux procfs and pidfd support")

    with tempfile.TemporaryDirectory(prefix="term-llm-reload-") as temp:
        root = Path(temp)
        installed = root / "term-llm"
        if args.symlink:
            shutil.copy2(args.binary_a, root / "image-a")
            installed.symlink_to("image-a")
        else:
            shutil.copy2(args.binary_a, installed)
        for name in ("home", "config", "data", "cache", "runtime"):
            (root / name).mkdir(mode=0o700)
        env = {"PATH": os.environ.get("PATH", os.defpath), "SHELL": "/bin/sh",
               "LANG": "C.UTF-8", "TERM": "dumb"}
        env.update(HOME=str(root / "home"), XDG_CONFIG_HOME=str(root / "config"),
                   XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"),
                   XDG_RUNTIME_DIR=str(root / "runtime"))
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        log = (root / "server.log").open("w+")
        child = subprocess.Popen([str(installed), "--no-session", "serve", "mcp",
                                  "--host", "127.0.0.1", "--port", str(port),
                                  "--tools", "shell", "--yolo"],
                                 cwd=root, env=env, stdout=log, stderr=log)
        record_path = root / "runtime" / "term-llm-processes" / f"{child.pid}.json"

        def record():
            if child.poll() is not None:
                log.flush()
                raise AssertionError(f"child exited {child.returncode}: "
                                     f"{(root / 'server.log').read_text()}")
            try:
                return json.loads(record_path.read_text())
            except FileNotFoundError:
                return {}

        def ready():
            value = record()
            return value if value.get("phase") == "ready" else None

        def call(command):
            body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                               "params": {"name": "shell", "arguments": {"command": command, "timeout_seconds": 60}}}).encode()
            request = urllib.request.Request(f"http://127.0.0.1:{port}/mcp", data=body,
                      headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json",
                               "Accept": "application/json, text/event-stream"})
            with urllib.request.urlopen(request, timeout=45) as response:
                payload = response.read().decode()
            assert '"isError":true' not in payload, payload
            assert '"error":' not in payload, payload
            return payload

        try:
            before = until(ready, "first boot ready")
            token = until(lambda: next((line.split(": ", 1)[1] for line in
                          (root / "server.log").read_text().splitlines()
                          if line.startswith("auth token: ")), None), "generated token")
            ledger, unblock = root / "effects", root / "release"
            command = (f"echo started >> {shlex.quote(str(ledger))}; "
                       f"while [ ! -f {shlex.quote(str(unblock))} ]; do sleep 0.05; done; "
                       f"echo finished >> {shlex.quote(str(ledger))}; echo tool-complete")
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                response = pool.submit(call, command)
                until(lambda: ledger.exists(), "tool start")
                replacement = root / "replacement"
                if args.symlink:
                    shutil.copy2(args.binary_b, root / "image-b")
                    replacement.symlink_to("image-b")
                else:
                    shutil.copy2(args.binary_b, replacement)
                replacement.replace(installed)
                if args.hold_drain:
                    # An explicit client wait limit must not cancel the accepted restart.
                    limited = subprocess.run([str(installed), "process", "restart", str(child.pid),
                                              "--timeout", "1s"], env=env, cwd=root,
                                             text=True, capture_output=True, timeout=10)
                    assert limited.returncode != 0, limited.stdout + limited.stderr
                    assert "SIGUSR2 was delivered" in limited.stdout + limited.stderr
                    assert "may still restart" in limited.stdout + limited.stderr
                else:
                    for _ in range(3):
                        os.kill(child.pid, signal.SIGUSR2)
                draining = until(lambda: (value if (value := record()).get("phase") == "draining" else None), "drain")
                assert draining["instance"] == before["instance"]
                assert not response.done(), "reload cancelled the tool"
                if args.hold_drain:
                    time.sleep(args.hold_drain)
                    held = record()
                    assert held["phase"] == "draining" and held["instance"] == before["instance"], held
                    assert not response.done(), "pending reload cancelled admitted work"
                unblock.touch()
                assert "tool-complete" in response.result(timeout=15)
            after = until(lambda: (value if (value := ready()) and value["instance"] != before["instance"] else None), "new boot ready")
            assert after["pid"] == before["pid"] == child.pid
            assert after["os_start"] == before["os_start"]
            assert after["executable"] == str(installed), after
            assert after["build_id"] != before["build_id"], "supply two differently built binaries"
            assert ledger.read_text().splitlines() == ["started", "finished"]
            assert "token-scrubbed" in call('test -z "${TERM_LLM_MCP_RELOAD_TOKEN+x}" && echo token-scrubbed')

            # Failed kernel exec must keep serving AND restore the USR2 listener.
            installed.chmod(0o600)
            os.kill(child.pid, signal.SIGUSR2)
            failed = until(lambda: (value if (value := ready()) and value.get("last_error") else None), "failed exec, ready again")
            assert failed["instance"] == after["instance"]
            assert "still-serving" in call("echo still-serving")
            installed.chmod(0o700)
            # Exercise the actual safe-targeting CLI on the same failed instance.
            result = subprocess.run([str(installed), "process", "restart", str(child.pid),
                                     "--timeout", "15s"], env=env, cwd=root,
                                    text=True, capture_output=True, timeout=20)
            assert result.returncode == 0, result.stdout + result.stderr
            final = until(ready, "CLI replacement ready")
            assert final["instance"] != after["instance"]
            assert "final-serving" in call("echo final-serving")
            print(f"PASS: PID {child.pid}, builds {before['build']} -> {after['build']}; "
                  f"symlink={args.symlink}, held drain={args.hold_drain}s, tool effects once, full response drained, token preserved/scrubbed, "
                  "failed exec stayed usable, CLI retry succeeded")
        finally:
            child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
            log.close()


if __name__ == "__main__":
    main()
