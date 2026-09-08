#!/usr/bin/env python3
"""Exercise installed specifications and real runners without touching a supervisor.

Usage: python3 scripts/service_smoke.py ./term-llm
Build first with make build. All account state/native files stay under ./tmp.
"""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

if sys.platform != 'linux':
    raise SystemExit('This isolated file-backed smoke is Linux-only; it must not create real Keychain items.')

binary = str(Path(sys.argv[1] if len(sys.argv) > 1 else './term-llm').resolve())
Path('tmp').mkdir(exist_ok=True)
root = Path(tempfile.mkdtemp(prefix='service-smoke.', dir='tmp')).resolve()
env = {key: os.environ[key] for key in ('PATH', 'LANG', 'USER', 'SHELL') if key in os.environ}
env.update(HOME=str(root), XDG_CONFIG_HOME=str(root / 'config'),
           XDG_DATA_HOME=str(root / 'data'), XDG_CACHE_HOME=str(root / 'cache'))
config = root / 'config/term-llm/config.yaml'
config.parent.mkdir(parents=True)
config.write_text('default_provider: debug\nproviders:\n  debug:\n    model: fast\n')
process = None


def cli(*args):
    result = subprocess.run([binary, 'service', *args], env=env, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f'service {args[0]} failed: {result.stderr}')
    return result.stdout


def spec_path(kind):
    return root / f'config/term-llm/services/{kind}/service.json'


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def request(url, token='', body=None):
    headers = {}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    if body is not None:
        headers['Origin'] = url.split('/ui/')[0]
        headers['Content-Type'] = 'application/json'
    req = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=2) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def start(kind):
    global process
    spec = json.loads(spec_path(kind).read_text())
    log = root / f'{kind}.log'
    with log.open('wb') as output:
        process = subprocess.Popen([binary, 'service', 'run', kind, '--spec', str(spec_path(kind))],
                                   env=env, stdout=output, stderr=subprocess.STDOUT)
    local = f"http://127.0.0.1:{spec['port']}{spec['base_path']}/"
    for _ in range(100):
        if process.poll() is not None:
            raise RuntimeError(f'{kind} runner exited: {log.read_text()}')
        try:
            if request(local + 'healthz')[0] == 200:
                return spec, local
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(.1)
    raise RuntimeError(f'{kind} runner failed health check')


def stop():
    global process
    if process is not None:
        process.terminate()
        process.wait(timeout=20)
        if process.returncode:
            raise RuntimeError(f'runner did not shut down cleanly: {process.returncode}')
        process = None


try:
    web_port = port()
    public = f'http://localhost:{web_port}/ui/'
    cli('install', 'web', '--no-start', '--yes', '--', '--port', str(web_port),
        '--public-url', public, '--disable-widgets', '--disable-extensions', '--no-projects')
    enrollment = json.loads((spec_path('web').parent / 'enrollment.json').read_text())
    spec, local = start('web')
    assert request(public + 'api/auth/bootstrap/verify', body={'code': enrollment['secret']})[0] == 200
    assert request(local + 'v1/models')[0] == 401
    assert enrollment['secret'] not in (root / 'web.log').read_text()
    stop()
    print('PASS: managed Web passkey runner consumes private enrollment capability; no log disclosure')

    cli('install', 'web', '--no-start', '--yes', '--', '--auth', 'bearer', '--port', str(web_port),
        '--disable-widgets', '--disable-extensions', '--no-projects')
    token = cli('token', 'web').strip()
    cli('install', 'web', '--no-start', '--yes')
    assert cli('token', 'web').strip() == token
    spec, local = start('web')
    assert request(local + 'v1/models', token)[0] == 200
    assert token not in (root / 'web.log').read_text()
    assert token not in spec_path('web').read_text()
    stop()
    print('PASS: custom bearer Web service, stable credentials on reinstall, authenticated API')

    hub_port = port()
    secret_file = root / 'import.env'
    secret_file.write_text('TERM_LLM_HUB_REGISTRATION_TOKEN=service-smoke-registration-secret\n')
    secret_file.chmod(0o600)
    cli('install', 'hub', '--no-start', '--yes', '--secrets-file', str(secret_file), '--',
        '--auth', 'bearer', '--port', str(hub_port), '--contain=false')
    hub_token = cli('token', 'hub').strip()
    spec, local = start('hub')
    assert request(local + 'api/nodes', hub_token)[0] == 200
    assert request(local + 'api/nodes')[0] == 401
    registration_status, registration_body = request(local + 'api/registration-info', hub_token)
    assert registration_status == 200 and json.loads(registration_body)['enabled'] is True
    assert hub_token not in (root / 'hub.log').read_text()
    assert 'service-smoke-registration-secret' not in (root / 'hub.log').read_text()
    stop()
    print('PASS: independent managed Hub runner, private registration credential, clean SIGTERM')
    shutil.rmtree(root)
except Exception:
    print(f'Isolated failure artifacts: {root}', file=sys.stderr)
    raise
finally:
    if process is not None and process.poll() is None:
        process.terminate()
        process.wait(timeout=20)
