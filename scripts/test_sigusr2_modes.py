#!/usr/bin/env python3
"""Hermetic Linux web/jobs SIGUSR2 acceptance; requires two distinct binaries."""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import urllib.request
from test_sigusr2_reload import until


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary-a', required=True, type=Path)
    parser.add_argument('--binary-b', required=True, type=Path)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='term-llm-modes-') as temp:
        root = Path(temp)
        installed = root / 'term-llm'
        shutil.copy2(args.binary_a, installed)
        env = {'PATH': os.environ.get('PATH', os.defpath), 'SHELL': '/bin/sh', 'TERM': 'dumb', 'LANG': 'C.UTF-8'}
        for key, name in [('HOME', 'home'), ('XDG_CONFIG_HOME', 'config'), ('XDG_DATA_HOME', 'data'), ('XDG_CACHE_HOME', 'cache'), ('XDG_RUNTIME_DIR', 'runtime')]:
            (root / name).mkdir(mode=0o700)
            env[key] = str(root / name)
        cfg = root / 'config/term-llm'
        cfg.mkdir()
        (cfg / 'config.yaml').write_text('default_provider: debug\nproviders:\n  debug:\n    model: fast\n')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        with (root / 'server.log').open('w+') as log:
            child = subprocess.Popen([str(installed), 'serve', 'web', 'jobs', '--port', str(port), '--no-auth', '--provider', 'debug:fast', '--tools', 'shell', '--yolo', '--disable-widgets', '--disable-extensions'], cwd=root, env=env, stdout=log, stderr=log)
            def record():
                if child.poll() is not None:
                    raise AssertionError((root / 'server.log').read_text())
                try:
                    return json.loads((root / f'runtime/term-llm-processes/{child.pid}.json').read_text())
                except FileNotFoundError:
                    return {}
            def request(path, data=None):
                req = urllib.request.Request(f'http://127.0.0.1:{port}/ui{path}', data=None if data is None else json.dumps(data).encode(), headers={'Content-Type': 'application/json'})
                with urllib.request.urlopen(req, timeout=40) as response:
                    return json.load(response)
            def ready():
                r = record()
                return r if r.get('phase') == 'ready' else None
            try:
                before = until(ready, 'ready')
                project_id = request('/v1/projects')['data'][0]['id']
                job = request('/v2/jobs', {'name': 'reload-proof', 'enabled': True, 'runner_type': 'program', 'runner_config': {'command': 'sh', 'args': ['-c', 'echo started >> job-effects; while [ ! -f release ]; do sleep .05; done; echo finished >> job-effects'], 'working_dir': str(root)}, 'trigger_type': 'manual', 'trigger_config': {}})
                request('/v2/jobs/' + job['id'] + '/trigger', {})
                until(lambda: (root / 'job-effects').exists(), 'job started')
                with concurrent.futures.ThreadPoolExecutor() as pool:
                    response = pool.submit(request, '/v1/responses', {'model': 'fast', 'include_server_tools': True, 'project_id': project_id, 'input': 'shell echo started >> web-effects; while [ ! -f release ]; do sleep .05; done; echo finished >> web-effects; echo complete', 'stream': False})
                    def started():
                        if response.done():
                            raise AssertionError(f'web returned before tool: {response.result()}')
                        return (root / 'web-effects').exists()
                    until(started, 'web tool started')
                    shutil.copy2(args.binary_b, root / 'replacement')
                    (root / 'replacement').replace(installed)
                    os.kill(child.pid, signal.SIGUSR2)
                    until(lambda: record().get('phase') == 'draining', 'draining')
                    assert not response.done(), 'reload cancelled admitted work'
                    (root / 'release').touch()
                    result = response.result(timeout=20)
                    assert 'completed successfully' in json.dumps(result), result
                after = until(lambda: r if (r := ready()) and r['instance'] != before['instance'] else None, 'replacement ready')
                assert before['pid'] == after['pid'] == child.pid
                assert before['build_id'] != after['build_id']
                for name in ['web', 'job']:
                    assert (root / f'{name}-effects').read_text().splitlines() == ['started', 'finished']
                runs = request('/v2/runs?job_id=' + job['id'])
                assert len(runs['data']) == 1 and runs['data'][0]['status'] == 'succeeded', runs
                installed.chmod(0o600)
                os.kill(child.pid, signal.SIGUSR2)
                until(lambda: (r := ready()) and r.get('last_error'), 'failed exec rollback')
                assert record()['instance'] == after['instance']
                assert request('/v1/responses', {'input': 'markdown*10', 'stream': False})['object'] == 'response'
                installed.chmod(0o700)
                os.kill(child.pid, signal.SIGUSR2)
                until(lambda: (r := ready()) and r['instance'] != after['instance'], 'retry ready')
                print('PASS: combined web/jobs drain, full response, exactly-once effects, failed exec and retry')
            except BaseException:
                print((root / 'server.log').read_text())
                raise
            finally:
                (root / 'release').touch()
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()


if __name__ == '__main__':
    main()
