// Real x86 Linux, isolated in its own browser worker. No network device/relay.
import { fetchBootAssets, fetchPythonRuntime, verifyPythonRuntime } from './linux-assets.mjs';
const MiB = 1024 * 1024;
const encoder = new TextEncoder();
const decoder = new TextDecoder('utf-8', { fatal: true });
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const quote = s => "'" + String(s).replaceAll("'", "'\\''") + "'";
// The shared workspace keeps its original 16 MiB 9p guard. The Python runtime
// is host-backed 9p storage (outside guest RAM) with a separate, bounded quota:
// the 7.4 MiB tar plus the 22.7 MiB extracted standard library fit with
// headroom, and the tar is removed after extraction.
const sharedQuota = 16 * MiB;
const runtimeQuota = 32 * MiB;
export function relativePath(name) {
  if (typeof name !== 'string' || !name || /[\\\x00]/.test(name) || name.split('/').some(p => !p || p === '.' || p === '..')) throw Error(`Invalid workspace path: ${name}`);
  return name;
}
export function validateFiles(files) {
  if (!files || typeof files !== 'object' || Array.isArray(files)) throw Error('Workspace map is required');
  let bytes = 0;
  const names = Object.keys(files);
  if (names.length > 256) throw Error('Workspace exceeds 256 files');
  for (const name of names) {
    relativePath(name);
    if (typeof files[name] !== 'string' || files[name].includes('\0')) throw Error('Workspace supports UTF-8 text files only');
    bytes += encoder.encode(files[name]).length;
    if (bytes > 8 * MiB) throw Error('Workspace exceeds 8 MiB');
  }
  return files;
}
// v86's 9p storage lives OUTSIDE guest RAM. Check before allocations, including
// guest fallocate/truncate/write and inode creation, not just after execution.
// Runtime files live under runtime/ and have their own bounded quota; everything
// else shares the original workspace/bridge allowance.
export function guardFilesystem(fs) {
  const pathOf = new Map([[0, '']]);
  const isRuntime = path => path === 'runtime' || path.startsWith('runtime/');
  const childPath = (parent, name) => {
    const base = pathOf.get(parent) ?? '';
    return base ? base + '/' + name : name;
  };
  const totalFor = (bucket, id, size) => {
    let total = size;
    for (let i = 0; i < fs.inodes.length; i++) {
      if (i === id) continue;
      const inode = fs.inodes[i];
      if (!inode) continue;
      if (isRuntime(pathOf.get(i)) === bucket) total += Number(inode.size) || 0;
    }
    return total;
  };
  const checkPath = (path, id, size) => {
    if (!Number.isSafeInteger(size) || size < 0) throw Error('Invalid Linux filesystem allocation');
    const bucket = isRuntime(path);
    const limit = bucket ? runtimeQuota : sharedQuota;
    if (size > limit || totalFor(bucket, id, size) > limit) {
      throw Error(bucket ? 'Linux Python runtime quota exceeded (32 MiB)' : 'Linux shared filesystem quota exceeded (16 MiB)');
    }
  };
  const checkId = (id, size) => checkPath(pathOf.get(id) ?? '', id, size);

  // Remember names for guest-created files/directories so later writes can be
  // attributed to the shared workspace or the runtime overlay.
  for (const [method, asyncCall] of [['CreateFile', false], ['CreateDirectory', false], ['CreateSymlink', false], ['CreateBinaryFile', true]]) {
    const original = fs[method]?.bind(fs);
    if (!original) continue;
    fs[method] = (...args) => {
      const [name, parent] = args;
      const path = childPath(parent, name);
      checkPath(path, -1, asyncCall ? args[2]?.length ?? 0 : 0);
      const result = original(...args);
      return asyncCall ? result.then(id => { pathOf.set(id, path); return id; }) : (pathOf.set(result, path), result);
    };
  }

  const changeSize = fs.ChangeSize.bind(fs);
  fs.ChangeSize = async (id, size) => { checkId(id, size); return changeSize(id, size); };
  const write = fs.Write.bind(fs);
  fs.Write = async (id, offset, count, data) => {
    const size = Math.max(fs.GetInode(id).size, offset + count);
    checkId(id, Math.ceil(size * 1.5)); // v86 grows backing arrays by 1.5x
    return write(id, offset, count, data);
  };
  const inode = fs.CreateInode.bind(fs);
  fs.CreateInode = (...args) => {
    if (fs.inodes.length >= 4096) throw Error('Linux shared filesystem inode quota exceeded; reload to reset');
    return inode(...args);
  };
}
// v86 otherwise creates a nested blob worker solely for scheduling ticks.
// WebKit can fail that load after its immediate revokeObjectURL. Select v86's
// built-in timer scheduler instead; execution is already off the UI thread.
async function loadEmulator() {
  const Worker = globalThis.Worker;
  try { globalThis.Worker = undefined; return (await import('./libv86.mjs')).V86; }
  finally { globalThis.Worker = Worker; }
}
export class LinuxSandbox {
  constructor(options = {}) { this.options = options; this.vm = null; this.serial = ''; this.sequence = 0; }
  async boot() {
    if (this.vm) return;
    const V86 = this.options.V86 ?? await loadEmulator();
    const assets = await (this.options.assets ? this.options.assets() : fetchBootAssets());
    const wasm = this.options.wasm ?? await fetch(new URL('./v86.wasm', import.meta.url)).then(r => { if (!r.ok) throw Error(`v86 HTTP ${r.status}`); return r.arrayBuffer(); });
    this.vm = new V86({
      wasm_fn: async imports => (await WebAssembly.instantiate(wasm, imports)).instance.exports,
      ...assets, memory_size: 64 * MiB, vga_memory_size: MiB,
      filesystem: {}, autostart: true, disable_keyboard: true, disable_mouse: true,
      disable_speaker: true, net_device: { type: 'none' },
      cmdline: 'console=ttyS0 tsc=reliable mitigations=off random.trust_cpu=on',
    });
    this.vm.add_listener('serial0-output-byte', byte => { this.serial = (this.serial + String.fromCharCode(byte)).slice(-8192); });
    await this.waitFor(() => this.serial.endsWith('~% '), 45000, 'Linux did not reach its shell prompt');
    guardFilesystem(this.vm.fs9p);
    this.mkdir('workspace'); this.mkdir('bridge'); this.mkdir('runtime');
    await this.vm.stop();
    await this.installPythonRuntime();
    await this.vm.run();
    const bootCode = await this.consoleCommand('umount /mnt 2>/dev/null || true; mount -t 9p -o trans=virtio,cache=none host9p /mnt; stty -echo; ln -sfn /mnt/workspace /workspace', 30000);
    if (bootCode !== 0) throw Error(`Linux workspace mount failed in the guest (exit ${bootCode})`);
    await this.vm.stop();
  }
  // Loaded once into host-backed 9p on the first shell boot. The guest extracts
  // the tar into /mnt/runtime/python, then removes the archive; Python always
  // runs directly from /mnt so the 23.7 MiB standard library does not consume
  // the guest's ~39 MiB usable RAM. -B avoids __pycache__ writes.
  async installPythonRuntime() {
    const bytes = this.options.pythonRuntime ?? await fetchPythonRuntime();
    await verifyPythonRuntime(bytes);
    await this.putBytes('runtime/python-root.tar.gz', bytes);
    await this.vm.run();
    const code = await this.consoleCommand([
      '(',
      'set -e',
      'set -o pipefail 2>/dev/null || true',
      'umount /mnt 2>/dev/null || true',
      'mount -t 9p -o trans=virtio,cache=none host9p /mnt',
      'mkdir -p /mnt/runtime/python',
      'gzip -dc /mnt/runtime/python-root.tar.gz | tar -x -C /mnt/runtime/python',
      'test -x /mnt/runtime/python/bin/python3.12',
      'test -f /mnt/runtime/python/lib/python3.12/os.py',
      'rm -f /mnt/runtime/python-root.tar.gz',
      "cat > /tmp/python <<'PY'",
      '#!/bin/sh',
      'export PYTHONHOME=/mnt/runtime/python',
      'export PYTHONDONTWRITEBYTECODE=1',
      "exec /mnt/runtime/python/lib/ld-musl-i386.so.1 --library-path /mnt/runtime/python/lib /mnt/runtime/python/bin/python3.12 -B \"$@\"",
      'PY',
      'chmod +x /tmp/python',
      'ln -sf python /tmp/python3',
      'test "$(/tmp/python -V 2>&1)" = "Python 3.12.15"',
      '/tmp/python -c "import json, csv, statistics, unittest, sqlite3"',
      ')',
    ].join('\n'), 120000);
    if (code !== 0) throw Error(`Python runtime install failed in the guest (exit ${code})`);
    await this.vm.stop();
  }
  async waitFor(check, ms, message) {
    const start = Date.now();
    while (!(await check())) { if (Date.now() - start > ms) throw Error(message); await sleep(10); }
  }
  async consoleCommand(command, ms) {
    const token = `__TERMLLM_${++this.sequence}_${Math.random().toString(36).slice(2)}__`;
    this.serial = '';
    this.vm.serial0_send(`${command}; code=$?; printf '\\n%s %s\\n' '${token}' "$code"\n`);
    const pattern = new RegExp('\\r\\n' + token + ' ([0-9]+)\\r\\n');
    await this.waitFor(() => pattern.test(this.serial), ms, 'Linux console did not respond');
    return Number(this.serial.match(pattern)[1]);
  }
  mkdir(path) {
    const fs = this.vm.fs9p;
    let id = 0;
    for (const part of path.split('/')) {
      const child = fs.Search(id, part);
      if (child !== -1 && (fs.GetInode(child).mode & 0xf000) !== 0x4000) throw Error('Workspace parent is not a directory');
      id = child === -1 ? fs.CreateDirectory(part, id) : child;
    }
    return id;
  }
  async remove(path) {
    const fs = this.vm.fs9p;
    const found = fs.SearchPath(path);
    if (found.id === -1) return;
    if ((fs.GetInode(found.id).mode & 0xf000) === 0x4000) {
      for (const name of fs.read_dir(path)) await this.remove(path + '/' + name);
    }
    fs.Unlink(found.parentid, path.slice(path.lastIndexOf('/') + 1));
    await fs.DeleteData(found.id);
  }
  async put(path, content) {
    return this.putBytes(path, encoder.encode(content));
  }
  // Binary-safe upload for page-owned runtime assets. The Go workspace protocol
  // remains UTF-8 text only; only the static runtime overlay uses this path.
  async putBytes(path, bytes) {
    if (!(bytes instanceof Uint8Array)) throw Error(`Runtime asset is not binary: ${path}`);
    const fs = this.vm.fs9p;
    const slash = path.lastIndexOf('/');
    const parent = this.mkdir(path.slice(0, slash));
    let id = fs.Search(parent, path.slice(slash + 1));
    if (id === -1) id = await fs.CreateBinaryFile(path.slice(slash + 1), parent, bytes);
    else {
      if ((fs.GetInode(id).mode & 0xf000) !== 0x8000) throw Error(`Workspace path is not a regular file: ${path}`);
      await fs.ChangeSize(id, bytes.length);
      await fs.Write(id, 0, bytes.length, bytes);
    }
    return id;
  }
  async readBytes(path) {
    const found = this.vm.fs9p.SearchPath(path);
    if (found.id === -1) throw Error(`Linux file not found: ${path}`);
    if (this.vm.fs9p.GetInode(found.id).size === 0) return new Uint8Array();
    return this.vm.read_file(path);
  }
  async snapshot() {
    const fs = this.vm.fs9p;
    const files = Object.create(null);
    let entries = 0;
    const visit = async (path, prefix = '') => {
      for (const name of fs.read_dir(path)) {
        if (++entries > 1024) throw Error('Workspace contains too many entries');
        const full = path + '/' + name;
        const node = fs.GetInode(fs.SearchPath(full).id);
        const type = node.mode & 0xf000;
        if (type === 0x4000) await visit(full, prefix + name + '/');
        else if (type === 0x8000) {
          if (node.size > 8 * MiB) throw Error('Workspace file exceeds 8 MiB');
          const bytes = await this.readBytes(full).catch(error => { throw Error(`Read workspace ${full}: ${error.message}`); });
          if (bytes.includes(0)) throw Error('Workspace file tools support text only (binary output rejected)');
          files[prefix + name] = decoder.decode(bytes);
          validateFiles(files);
        } else throw Error('Workspace file tools support regular text files only (symlink/special file rejected)');
      }
    };
    await visit('workspace');
    return files;
  }
  async execute(request) {
    validateFiles(request.files);
    const cwd = request.working_dir || '/workspace';
    if (cwd !== '/workspace' && !cwd.startsWith('/workspace/')) throw Error('working_dir must be inside /workspace');
    if (cwd !== '/workspace') relativePath(cwd.slice(11));
    if (typeof request.command !== 'string' || !request.command.trim() || request.command.includes('\0')) throw Error('A shell command is required');
    await this.boot();
    for (const name of this.vm.fs9p.read_dir('bridge')) await this.remove('bridge/' + name);
    const before = await this.snapshot();
    for (const name of Object.keys(before)) if (!Object.hasOwn(request.files, name)) await this.remove('workspace/' + name);
    for (const [name, content] of Object.entries(request.files)) if (before[name] !== content) await this.put('workspace/' + name, content);
    const seconds = Math.min(30, Math.max(1, Number(request.timeout_seconds) || 10));
    const id = String(++this.sequence);
    const dir = 'bridge/c' + id;
    const base = '/mnt/' + dir + '/';
    await this.put(dir + '/command.sh', `#!/bin/sh\nulimit -f 64\nPATH=/tmp:$PATH\nexport PATH\ncd ${quote(cwd)} || exit 2\n${request.command}\n`);
    // Run with its own session/process group. Kill descendants after success too,
    // so ordinary background jobs do not outlive the command. Root inside the
    // disposable guest is not a process-isolation boundary; the worker is.
    await this.put(dir + '/run.sh', `#!/bin/sh\nrm -f ${base}done ${base}stdout ${base}stderr\nsetsid sh ${base}command.sh >${base}stdout 2>${base}stderr &\npid=$!\n(sleep ${seconds}; kill -TERM -$pid 2>/dev/null; sleep 1; kill -KILL -$pid 2>/dev/null) &\nwatch=$!\nwait $pid\ncode=$?\nkill $watch 2>/dev/null\nkill -KILL -$pid 2>/dev/null\nprintf '%s' "$code" >${base}done\n`);
    // Each call has unique mailboxes, avoiding stale guest inode/dentry caches.
    await this.vm.run();
    this.vm.serial0_send(`umount /mnt; mount -t 9p -o trans=virtio,cache=none host9p /mnt; sh ${base}run.sh\n`);
    try {
      await this.waitFor(() => {
        const file = this.vm.fs9p.SearchPath(dir + '/done');
        return file.id !== -1 && this.vm.fs9p.GetInode(file.id).size > 0;
      }, (seconds + 4) * 1000, 'Linux command exceeded deadline; guest will be reset');
      await this.vm.stop();
      const read = async name => new TextDecoder().decode(await this.readBytes(dir + '/' + name).catch(error => { throw Error(`Read mailbox ${dir}/${name}: ${error.message}; entries: ${this.vm.fs9p.read_dir(dir)}`); }));
      const exit = Number(await read('done'));
      return {
        stdout: (await read('stdout')).slice(0, 65536),
        stderr: (await read('stderr')).slice(0, 65536) + (exit === 143 || exit === 137 ? '\nCommand terminated (execution time limit).' : ''),
        exit_code: exit === 143 || exit === 137 ? 124 : exit,
        files: await this.snapshot(),
      };
    } catch (error) { await this.destroy(); throw error; }
  }
  async destroy() { if (this.vm) { await this.vm.destroy(); this.vm = null; } }
}
