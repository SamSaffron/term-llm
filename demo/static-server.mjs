// Ephemeral loopback nginx, using the exact proposed production locations.
// No application API handlers. Requires nginx on PATH; run after build.mjs.
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, writeFile, rm } from 'node:fs/promises';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { once } from 'node:events';
import net from 'node:net';

export async function startStaticServer() {
  const here = fileURLToPath(new URL('.', import.meta.url));
  const scratch = resolve(here, '../tmp');
  await mkdir(scratch, { recursive: true });
  const dir = await mkdtemp(scratch + '/demo-nginx-');
  const socket = net.createServer();
  socket.listen(0, '127.0.0.1');
  await once(socket, 'listening');
  const port = socket.address().port;
  await new Promise(resolve => socket.close(resolve));
  const config = `
    daemon off;
    pid "${dir}/nginx.pid";
    error_log stderr;
    events {}
    http {
      access_log off;
      client_body_temp_path "${dir}/client-body";
      proxy_temp_path "${dir}/proxy";
      fastcgi_temp_path "${dir}/fastcgi";
      uwsgi_temp_path "${dir}/uwsgi";
      scgi_temp_path "${dir}/scgi";
      types { text/html html; text/css css; application/javascript js mjs;
        application/json json; application/manifest+json webmanifest;
        image/png png; image/svg+xml svg; font/woff2 woff2; }
      server {
        listen 127.0.0.1:${port};
        root "${here}dist";
        index index.html;
        add_header X-Content-Type-Options nosniff always;
        include "${here}nginx-locations.conf";
      }
    }`;
  await writeFile(dir + '/nginx.conf', config);
  const child = spawn('nginx', ['-p', dir, '-c', dir + '/nginx.conf'], { stdio: ['ignore', 'ignore', 'pipe'] });
  let errors = '', failure;
  child.stderr.on('data', data => { errors += data; });
  child.on('error', error => { failure = error; });
  const origin = `http://127.0.0.1:${port}`;
  const close = async () => {
    if (child.exitCode === null && child.signalCode === null && !failure) {
      const exited = once(child, 'exit');
      child.kill('SIGTERM');
      await exited;
    }
    await rm(dir, { recursive: true, force: true });
  };
  // Bounded readiness polling, not a long-lived service or API emulator.
  for (let attempt = 0; attempt < 100; attempt++) {
    if (failure || child.exitCode !== null) break;
    try {
      if ((await fetch(origin + '/healthz')).ok) return { origin, close };
    } catch { /* nginx is still starting */ }
    await new Promise(resolve => setTimeout(resolve, 20));
  }
  await close();
  throw Error(`nginx failed to start: ${failure ?? errors}`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const server = await startStaticServer();
  console.log(server.origin);
  for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, async () => {
    await server.close();
    process.exit(0);
  });
}
