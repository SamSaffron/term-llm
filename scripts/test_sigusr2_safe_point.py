#!/usr/bin/env python3
"""Isolated real-binary mid-turn web reload; proves same-response-ID continuation."""
import argparse
import concurrent.futures
import json
import http.server
import threading
import os
from pathlib import Path
import shutil
import signal
import re
import socket
import subprocess
import tempfile
import urllib.error
import urllib.request
from test_sigusr2_reload import until


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary-a', required=True, type=Path)
    parser.add_argument('--binary-b', required=True, type=Path)
    parser.add_argument('--cancel', action='store_true', help='let the 30-second cancellation fallback stop the tool')
    parser.add_argument('--fail-exec', action='store_true', help='prove the original response resumes after failed exec')
    parser.add_argument('--check-client', action='store_true', help='also project replay through the real web reducer (requires frontend dependencies)')
    parser.add_argument('--check-auth', action='store_true', help='verify generated bearer auth survives exec without leaking to tool environments')
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='term-llm-safe-point-') as temp:
        root = Path(temp)
        installed = root / 'term-llm'
        shutil.copy2(args.binary_a, installed)
        env = {'PATH': os.environ.get('PATH', os.defpath), 'SHELL': '/bin/sh', 'TERM': 'dumb', 'LANG': 'C.UTF-8'}
        for key, name in [('HOME','home'),('XDG_CONFIG_HOME','config'),('XDG_DATA_HOME','data'),('XDG_CACHE_HOME','cache'),('XDG_RUNTIME_DIR','runtime')]:
            (root/name).mkdir(mode=0o700)
            env[key] = str(root/name)
        cfg = root/'config/term-llm'
        cfg.mkdir()
        hello = threading.Event()
        finish_hello = threading.Event()
        main_calls = []
        class Provider(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                messages = body['messages']
                main = any(m.get('role') == 'user' and m.get('content') == 'safe-point-proof' for m in messages)
                results = {m.get('tool_call_id') for m in messages if m.get('role') == 'tool'}
                if main:
                    main_calls.append(results)
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                def emit(delta, reason=None):
                    data = {'id':'fixture', 'object':'chat.completion.chunk', 'model':'fixture-model',
                            'choices':[{'index':0,'delta':delta,'finish_reason':reason}]}
                    self.wfile.write(('data: '+json.dumps(data)+'\n\n').encode())
                    self.wfile.flush()
                if not main:
                    emit({'content':'Fixture title'}, 'stop')
                elif 'second' in results:
                    emit({'content':'Finished.'}, 'stop')
                else:
                    call = 'second' if 'first' in results else 'first'
                    if call == 'second':
                        emit({'content':'Hello'})
                        hello.set()
                        if not args.cancel:
                            assert finish_hello.wait(20), 'test did not release Hello boundary'
                        command = 'echo second-exe:$(readlink /proc/$PPID/exe) >> effects; echo second-finished >> effects'
                    else:
                        command = 'echo first-started >> effects; while [ ! -f release ]; do sleep .05; done; echo first-finished >> effects'
                    if args.check_auth:
                        command = 'if [ -n "${TERM_LLM_SERVE_TOKEN+x}${TERM_LLM_SERVE_RELOAD_TOKEN+x}" ]; then exit 97; fi; touch token-private-'+call+'; '+command
                    emit({'tool_calls':[{'index':0,'id':call,'type':'function','function':{
                        'name':'shell','arguments':json.dumps({'command':command,'timeout_seconds':90})}}]}, 'tool_calls')
                self.wfile.write(b'data: [DONE]\n\n')
                self.wfile.flush()
        provider = http.server.ThreadingHTTPServer(('127.0.0.1',0), Provider)
        provider_thread = threading.Thread(target=provider.serve_forever, daemon=True)
        provider_thread.start()
        (cfg/'config.yaml').write_text(f'default_provider: fixture\nproviders:\n  fixture:\n    type: openai_compatible\n    url: http://127.0.0.1:{provider.server_port}/chat/completions\n    api_key: fixture\n    model: fixture-model\n')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1',0))
            port = sock.getsockname()[1]
        with (root/'server.log').open('w+') as log:
            child = subprocess.Popen([str(installed),'serve','web','--port',str(port),*(['--auth','bearer'] if args.check_auth else ['--no-auth']),'--provider','fixture:fixture-model','--tools','shell','--yolo','--disable-widgets','--disable-extensions'],cwd=root,env=env,stdout=log,stderr=log)
            def record():
                if child.poll() is not None:
                    raise AssertionError((root/'server.log').read_text())
                try:
                    return json.loads((root/f'runtime/term-llm-processes/{child.pid}.json').read_text())
                except FileNotFoundError:
                    return {}
            def ready():
                value = record()
                return value if value.get('phase') == 'ready' else None
            bearer_token = ''
            def request(path,data=None):
                prefix = '/ui'
                req = urllib.request.Request(f'http://127.0.0.1:{port}{prefix}{path}',data=None if data is None else json.dumps(data).encode(),headers={'Content-Type':'application/json', **({'Authorization':'Bearer '+bearer_token} if args.check_auth else {}), **({'Idempotency-Key':'safe-point-request', 'X-Term-LLM-Draft-ID':'draft_safe_point', 'X-Term-LLM-UI-Version':'1'} if path == '/v1/responses' else {})})
                return urllib.request.urlopen(req,timeout=60)
            def get(path):
                with request(path) as response:
                    return json.load(response)
            response_id = []
            original_events = []
            def body(project):
                return {'project_id':project,'input':'safe-point-proof','client_message_id':'msg_safe_point','include_server_tools':True,'stream':True}
            def events(response):
                event_type = ''
                for line in response:
                    if line.startswith(b'event: '):
                        event_type = line[7:].decode().strip()
                    elif line.startswith(b'data: '):
                        try:
                            yield {**json.loads(line[6:]), 'type':event_type}
                        except json.JSONDecodeError:
                            pass
            def stream(project):
                try:
                    with request('/v1/responses',body(project)) as response:
                        for data in events(response):
                            original_events.append(data)
                            rid = data.get('response',{}).get('id')
                            if rid and not response_id:
                                response_id.append(rid)
                except urllib.error.HTTPError as error:
                    raise AssertionError(error.read().decode()) from error
                except (OSError, EOFError):
                    pass  # Successful exec closes the old passive subscription.
            try:
                before = until(ready,'ready')
                if args.check_auth:
                    match = re.search(r'^token: ([A-Za-z0-9_-]+)', (root/'server.log').read_text(), re.MULTILINE)
                    assert match, 'server did not publish its generated fixture token'
                    bearer_token = match[1]
                    try:
                        urllib.request.urlopen(f'http://127.0.0.1:{port}/ui/v1/providers',timeout=5).close()
                    except urllib.error.HTTPError as error:
                        assert error.code == 401
                    else:
                        raise AssertionError('fixture API unexpectedly allowed an unauthenticated request')
                project = get('/v1/projects')['data'][0]['id']
                with concurrent.futures.ThreadPoolExecutor() as pool:
                    subscription = pool.submit(stream,project)
                    until(lambda: (subscription.result() if subscription.done() else None) or (response_id and (root/'effects').exists()),'first tool running')
                    shutil.copy2(args.binary_b,root/'replacement')
                    (root/'replacement').replace(installed)
                    if args.fail_exec:
                        installed.chmod(0o600)
                    if not args.cancel:
                        (root/'release').touch()
                        until(hello.is_set, 'Hello emitted before the next tool')
                    os.kill(child.pid,signal.SIGUSR2)
                    until(lambda: record().get('phase') == 'draining','draining')
                    finish_hello.set()
                    if args.fail_exec:
                        after = until(lambda: r if (r := ready()) and r.get('last_error') else None, 'failed exec rollback', timeout=45)
                        assert after['instance'] == before['instance']
                    else:
                        after = until(lambda: r if (r := ready()) and r['instance'] != before['instance'] else None,'replacement ready',timeout=45)
                        assert after['build_id'] != before['build_id']
                    assert after['pid'] == before['pid']
                    snapshot = until(lambda: r if (r := get('/v1/responses/'+response_id[0])).get('status') == 'completed' else None,'same response completes',timeout=15)
                    assert snapshot['id'] == response_id[0],snapshot
                    effects = (root/'effects').read_text().splitlines()
                    expected = ['first-started'] if args.cancel else ['first-started','first-finished']
                    expected += ['second-exe:'+str(installed)+(' (deleted)' if args.fail_exec else ''), 'second-finished']
                    assert effects == expected, effects
                    if args.check_auth:
                        assert (root/'token-private-first').exists() and (root/'token-private-second').exists(), 'tool inherited a server credential'
                    assert len(main_calls) == 3, main_calls
                    subscription.result(timeout=5)
                    # An exact retry must replay the original invocation, including
                    # its epoch/fingerprint claim, rather than create another run.
                    with request('/v1/responses',body(project)) as response:
                        assert response.headers['x-response-id'] == response_id[0]
                        replay = list(events(response))
                    epoch = original_events[0]['run_epoch']
                    assert snapshot['run_epoch'] == epoch, snapshot
                    assert all(e['response_id'] == response_id[0] and e['run_epoch'] == epoch for e in replay), replay
                    assert [e['sequence_number'] for e in replay] == list(range(1,len(replay)+1)), replay
                    assert sum(e['type'] == 'response.created' for e in replay) == 1
                    assert replay[-1]['type'] == 'response.completed', replay[-1]
                    assert len(main_calls) == 3, 'POST retry executed the request again'
                    if args.check_client:
                        repo = Path(__file__).resolve().parents[1]
                        bundle = root/'response-reducer.mjs'
                        subprocess.run([str(repo/'frontend/node_modules/.bin/esbuild'), str(repo/'frontend/src/domain/response.ts'), '--bundle', '--platform=node', '--format=esm', '--outfile='+str(bundle)], check=True, capture_output=True)
                        check = """
import {readFileSync} from 'node:fs';
const {initialProjection, reduceResponse} = await import(process.argv[1]);
const events = JSON.parse(readFileSync(0, 'utf8'));
let state = initialProjection({responseId:events[0].response_id, sessionId:'fixture', epoch:events[0].run_epoch, status:'connecting', lastSequence:0, startedRev:0, reconnects:0});
for (const event of events) state = reduceResponse(state, event);
if (state.run.status !== 'completed') throw new Error('client did not complete: '+state.run.status);
"""
                        subprocess.run(['node','--input-type=module','-e',check,bundle.as_uri()],input=json.dumps(replay),text=True,check=True)
                if args.fail_exec:
                    installed.chmod(0o700)
                    os.kill(child.pid,signal.SIGUSR2)
                    until(lambda: (r := ready()) and r['instance'] != before['instance'], 'successful retry')
                print(f'PASS: same response {response_id[0]} continued across reload; epoch, POST retry and effects preserved; cancellation={args.cancel}; failed-exec={args.fail_exec}; private-auth={args.check_auth}')
            except BaseException:
                print((root/'server.log').read_text())
                raise
            finally:
                (root/'release').touch()
                finish_hello.set()
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill(); child.wait()
                provider.shutdown()
                provider.server_close()
                provider_thread.join(timeout=3)


if __name__ == '__main__':
    main()
