import test from 'node:test';
import assert from 'node:assert/strict';
import { LinuxSandbox, validateFiles, relativePath, guardFilesystem } from './web/linux.mjs';

const MiB = 1024 * 1024;

test('workspace paths and text snapshot quotas are checked before execution', () => {
  for (const name of ['/etc/passwd', '../escape', 'x/../y', 'a\\b', 'x//y', 'x\0y']) assert.throws(() => relativePath(name));
  assert.equal(relativePath('playground/test.sh'), 'playground/test.sh');
  assert.throws(() => validateFiles({ x: 123 }));
  assert.throws(() => validateFiles({ x: 'a'.repeat(8 * 1024 * 1024 + 1) }));
  assert.throws(() => validateFiles(Object.fromEntries(Array.from({ length: 257 }, (_, n) => ['file' + n, '']))));
});

test('9p allocation guards reject huge truncate/write before calling allocator', async () => {
  let allocated = false;
  const fs = { inodedata: {}, inodes: [], GetInode: () => ({size:0}), ChangeSize: async () => { allocated = true; }, Write: async () => { allocated = true; }, CreateBinaryFile: () => { allocated = true; }, CreateInode: () => { allocated = true; } };
  guardFilesystem(fs);
  await assert.rejects(fs.ChangeSize(1, 1e12), /quota/);
  await assert.rejects(fs.Write(1, 0, 1e12, null), /quota/);
  assert.throws(() => fs.CreateBinaryFile('big', 0, {length:1e12}), /quota/);
  fs.inodes.length = 4096;
  assert.throws(() => fs.CreateInode(), /inode quota/);
  assert.equal(allocated, false);
});

test('Python runtime gets a separate bounded 9p quota from the workspace', async () => {
  const inodes = [{ size: 0, mode: 0x4000 }];
  const fs = {
    inodes,
    GetInode: id => inodes[id],
    ChangeSize: async (id, size) => { inodes[id].size = size; },
    Write: async () => {},
    CreateInode: () => ({ size: 0 }),
    CreateFile: () => { inodes.push({ size: 0, mode: 0x8000 }); return inodes.length - 1; },
    CreateDirectory: () => { inodes.push({ size: 0, mode: 0x4000 }); return inodes.length - 1; },
    CreateBinaryFile: async (name, parent, data) => { inodes.push({ size: data.length, mode: 0x8000 }); return inodes.length - 1; },
  };
  guardFilesystem(fs);
  const runtime = fs.CreateDirectory('runtime', 0);
  const python = fs.CreateDirectory('python', runtime);
  const workspace = fs.CreateDirectory('workspace', 0);
  await fs.CreateBinaryFile('stdlib.tar', python, new Uint8Array(24 * MiB));
  assert.throws(() => fs.CreateBinaryFile('too-big', python, new Uint8Array(9 * MiB)), /runtime quota/);
  assert.throws(() => fs.CreateBinaryFile('data', workspace, new Uint8Array(17 * MiB)), /shared filesystem quota/);
  await fs.ChangeSize(workspace, 16 * MiB);
});

test('real Linux shell + CPython: JSON/CSV/SQLite, unittest red/fix/green, Unicode, timeout', {
  skip: !process.env.LINUX_TEST_ASSETS,
}, async () => {
  const { createRequire } = await import('node:module');
  globalThis.require = createRequire(import.meta.url);
  const { readFile } = await import('node:fs/promises');
  const { V86 } = await import('v86');
  const bytes = async name => { const b = await readFile(process.env.LINUX_TEST_ASSETS + '/' + name); return b.buffer.slice(b.byteOffset,b.byteOffset+b.byteLength); };
  const u8 = async path => { const b = await readFile(path); return new Uint8Array(b.buffer, b.byteOffset, b.byteLength); };
  const pythonRuntime = await u8(new URL('./web/runtime/python-root.tar.gz', import.meta.url));
  assert.ok(pythonRuntime.byteLength > 1, 'missing Python runtime asset; run scripts/fetch-python-runtime.sh');
  const sandbox = new LinuxSandbox({
    V86,
    wasm: await u8(process.env.LINUX_TEST_ASSETS + '/v86.wasm'),
    pythonRuntime,
    assets: async () => ({
      bios: { buffer: await bytes('seabios.bin') },
      vga_bios: { buffer: await bytes('vgabios.bin') },
      bzimage: { buffer: await bytes('linux.bin') },
    }),
  });
  try {
    // Existing BusyBox regression: pipeline, real red -> fix -> green, sync.
    let result = await sandbox.execute({ command: "sh test.sh", files: {
      'calc.sh': "awk 'BEGIN {print 2+2+1}'\n",
      'test.sh': "test \"$(sh calc.sh)\" = 4 || { echo FAIL >&2; exit 1; }; echo PASS\n",
      'empty.txt': '',
    } });
    assert.equal(result.exit_code, 1); assert.match(result.stderr, /FAIL/);
    result.files['calc.sh'] = "awk 'BEGIN {print 2+2}'\n";
    result = await sandbox.execute({ command: "sh test.sh; printf '3\\n1\\n2\\n' | sort -n | awk '{s+=$1} END {print s}'; echo שלום > unicode.txt; touch new-empty.txt", files: result.files });
    assert.equal(result.exit_code, 0); assert.equal(result.stdout, 'PASS\n6\n');
    assert.equal(result.files['unicode.txt'], 'שלום\n'); assert.equal(result.files['new-empty.txt'], '');
    delete result.files['unicode.txt']; result.files['empty.txt'] = 'changed by file tool\n';
    result = await sandbox.execute({ command: 'cat empty.txt; test ! -f unicode.txt; rm calc.sh; exit 7', files:result.files });
    assert.equal(result.exit_code, 7); assert.equal(result.stdout, 'changed by file tool\n'); assert.equal(Object.hasOwn(result.files,'calc.sh'),false);

    // Real CPython, including the stdlib modules the model is told it has.
    result = await sandbox.execute({ command: 'python -V; python3 -V', files: result.files });
    assert.equal(result.exit_code, 0);
    assert.equal(result.stdout, 'Python 3.12.15\nPython 3.12.15\n');

    result = await sandbox.execute({ command: `python - <<'PY'
import base64, binascii, gzip, io, zipfile
assert base64.b64decode(base64.b64encode(b'bonsai')) == b'bonsai'
assert binascii.hexlify(b'bonsai') == b'626f6e736169'
assert gzip.decompress(gzip.compress(b'bonsai')) == b'bonsai'
buf = io.BytesIO()
with zipfile.ZipFile(buf, 'w') as archive:
    archive.writestr('hello.txt', 'שלום')
with zipfile.ZipFile(buf) as archive:
    assert archive.read('hello.txt').decode('utf-8') == 'שלום'
print('COMPRESSION_OK')
PY`, files: result.files });
    assert.equal(result.exit_code, 0);
    assert.equal(result.stdout, 'COMPRESSION_OK\n');

    result.files['scores.csv'] = 'name,score\nalpha,10\nbeta,20\n';
    result.files['inspect.py'] = `import csv, json, sqlite3, statistics
with open('scores.csv', newline='', encoding='utf-8') as f:
    rows = list(csv.DictReader(f))
values = [float(row['score']) for row in rows]
con = sqlite3.connect(':memory:')
con.execute('create table scores(name text, score real)')
con.executemany('insert into scores values (?, ?)', [(row['name'], float(row['score'])) for row in rows])
db_mean = con.execute('select avg(score) from scores').fetchone()[0]
result = {'mean': statistics.mean(values), 'median': statistics.median(values), 'names': [row['name'] for row in rows], 'db_mean': db_mean, 'unicode': 'שלום 👋'}
with open('summary.json', 'w', encoding='utf-8') as f:
    json.dump(result, f, ensure_ascii=False, sort_keys=True)
with open('unicode.txt', 'w', encoding='utf-8') as f:
    f.write(result['unicode'])
print(json.dumps(result, ensure_ascii=False, sort_keys=True))
`;
    result = await sandbox.execute({ command: 'python inspect.py; cat summary.json', files: result.files });
    assert.equal(result.exit_code, 0);
    const summary = JSON.parse(result.files['summary.json']);
    assert.deepEqual(summary, { mean: 15, median: 15, names: ['alpha', 'beta'], db_mean: 15, unicode: 'שלום 👋' });
    assert.equal(result.files['unicode.txt'], 'שלום 👋');
    assert.match(result.stdout, /"db_mean": 15/);

    // Real unittest red -> fix -> green. The module is deliberately wrong first.
    result.files['stats.py'] = 'def mean(values):\n    return sum(values) / (len(values) - 1)\n';
    result.files['test_stats.py'] = `import unittest
from stats import mean
class MeanTest(unittest.TestCase):
    def test_mean(self):
        self.assertEqual(mean([2, 4, 6]), 4)
if __name__ == '__main__':
    unittest.main()
`;
    result = await sandbox.execute({ command: 'python test_stats.py', files: result.files });
    assert.equal(result.exit_code, 1);
    assert.match(result.stderr, /FAILED/);
    result.files['stats.py'] = 'def mean(values):\n    return sum(values) / len(values)\n';
    result = await sandbox.execute({ command: 'python test_stats.py', files: result.files });
    assert.equal(result.exit_code, 0);
    assert.match(result.stderr, /OK/);

    // Python is killed by the command deadline, then the paused guest recovers.
    result = await sandbox.execute({ command: 'python -c "import time; time.sleep(10)"', timeout_seconds: 1, files: result.files });
    assert.equal(result.exit_code, 124);
    result = await sandbox.execute({ command: 'echo after-python-timeout', files: result.files });
    assert.equal(result.stdout, 'after-python-timeout\n');

    result = await sandbox.execute({ command:'sleep 10',timeout_seconds:1,files:result.files });
    assert.equal(result.exit_code,124);
    result = await sandbox.execute({ command:'echo after-timeout',files:result.files });
    assert.equal(result.stdout,'after-timeout\n');
  } finally { await sandbox.destroy(); }
});

test('cancelling a shell request terminates the worker and frees the next call', async () => {
  const OriginalWorker = globalThis.Worker;
  class FakeWorker {
    static instances = 0;
    static reply = false;
    constructor() { FakeWorker.instances++; this.listeners = {}; }
    addEventListener(name, fn) { this.listeners[name] = fn; }
    removeEventListener(name, fn) { if (this.listeners[name] === fn) delete this.listeners[name]; }
    postMessage() {
      if (!FakeWorker.reply) return;
      setTimeout(() => this.listeners.message?.({ data: { result: { stdout: 'ok', stderr: '', exit_code: 0, files: {} } } }), 0);
    }
    terminate() { this.terminated = true; }
  }
  globalThis.Worker = FakeWorker;
  try {
    const { runLinuxShell } = await import('./web/linux-client.mjs?cancel-test');
    const controller = new AbortController();
    const first = runLinuxShell({ timeout_seconds: 1 }, { signal: controller.signal });
    controller.abort(new Error('cancel test'));
    await assert.rejects(first, /cancel test/);
    FakeWorker.reply = true;
    const second = await runLinuxShell({ timeout_seconds: 1 });
    assert.equal(second.stdout, 'ok');
    assert.equal(FakeWorker.instances, 2);
  } finally {
    globalThis.Worker = OriginalWorker;
  }
});
