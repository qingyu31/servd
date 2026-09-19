import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gunzipSync } from 'node:zlib';
import { getPlatform } from '../npm/bin/servd.cjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const { name, version } = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
// npm pack renders a scoped name @scope/pkg as the tarball prefix scope-pkg.
const tarballName = (pkg) => `${pkg.replace('@', '').replace('/', '-')}-${version}.tgz`;
const tlsCA = readFileSync(join(root, 'testdata', 'tls', 'fullchain.pem'));
const platform = getPlatform();
const temporary = mkdtempSync(join(tmpdir(), 'servd-smoke-'));
const sockets = new Set();
let child;
let upstream;
let childExit;
let daemonCli;
let daemonExit;
let launcherPath;
const daemonPorts = [];

function npm(args, cwd) {
  const cli = process.env.npm_execpath;
  const command = cli ? process.execPath : 'npm';
  const result = spawnSync(command, [...(cli ? [cli] : []), ...args], {
    cwd, encoding: 'utf8', shell: false, timeout: 60_000,
  });
  if (result.error) throw result.error;
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
  return result.stdout;
}

async function listen(server) {
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  return server.address().port;
}

function request(port, path, { method = 'GET', headers = {}, body, secure = false } = {}) {
  return new Promise((resolve, reject) => {
    const req = (secure ? https : http).request({
      hostname: '127.0.0.1', port, path, method,
      ...(secure ? { ca: tlsCA, servername: 'localhost' } : {}),
      headers: { Host: 'example.org', ...headers }, timeout: 10_000,
    }, (res) => {
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.on('error', reject);
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
    });
    req.on('timeout', () => req.destroy(new Error('HTTP smoke request timed out')));
    req.on('error', reject);
    req.end(body);
  });
}

function websocket(port, secure = false) {
  return new Promise((resolve, reject) => {
    const key = Buffer.from('servd-smoke-key').toString('base64');
    const req = (secure ? https : http).request({
      hostname: '127.0.0.1', port, path: '/ws/chat?room=1', timeout: 10_000,
      ...(secure ? { ca: tlsCA, servername: 'localhost' } : {}),
      headers: {
        Host: 'example.org', Connection: 'Upgrade', Upgrade: 'websocket',
        'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': key,
      },
    });
    req.on('error', reject);
    req.on('timeout', () => req.destroy(new Error('WebSocket handshake timed out')));
    req.on('response', (res) => { res.resume(); reject(new Error(`Expected Upgrade, got ${res.statusCode}`)); });
    req.on('upgrade', (res, socket, head) => {
      sockets.add(socket);
      socket.once('close', () => sockets.delete(socket));
      socket.setTimeout(10_000, () => socket.destroy(new Error('WebSocket echo timed out')));
      socket.on('error', reject);
      try {
        assert.equal(res.statusCode, 101);
        assert.equal(res.headers['sec-websocket-accept'], createHash('sha1').update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64'));
        const payload = Buffer.from('ping');
        const mask = Buffer.from([1, 2, 3, 4]);
        const frame = Buffer.concat([Buffer.from([0x81, 0x80 | payload.length]), mask, payload.map((value, i) => value ^ mask[i % 4])]);
        let received = head;
        const consume = (data) => {
          received = Buffer.concat([received, data]);
          if (received.length < 6) return;
          try {
            assert.deepEqual(received.subarray(0, 6), Buffer.from([0x81, 4, ...payload]));
            socket.destroy();
            resolve();
          } catch (error) { socket.destroy(); reject(error); }
        };
        socket.on('data', consume);
        socket.write(frame);
        consume(Buffer.alloc(0));
      } catch (error) { socket.destroy(); reject(error); }
    });
    req.end();
  });
}

try {
  const tarballs = join(root, 'dist', 'tarballs');
  npm(['install', '--offline', '--ignore-scripts', '--no-audit', '--no-fund', '--no-package-lock',
    join(tarballs, tarballName(name)), join(tarballs, tarballName(platform.packageName))], temporary);
  assert.equal(npm(['exec', '--offline', '--no', '--', 'servd', '--version'], temporary).trim(), version);
  const web = join(temporary, 'web');
  const app = join(temporary, 'app');
  mkdirSync(web);
  mkdirSync(app);
  const content = 'servd compression smoke\n'.repeat(128);
  writeFileSync(join(web, 'index.html'), '<h1>static</h1>');
  writeFileSync(join(web, 'content.txt'), content);
  writeFileSync(join(app, 'index.html'), '<h1>spa</h1>');
  writeFileSync(join(app, 'app.js'), 'console.log("spa");');
  upstream = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => {
      res.setHeader('Content-Type', 'application/json');
      res.setHeader('Cache-Control', 'public, max-age=17');
      res.end(JSON.stringify({ url: req.url, method: req.method, body: Buffer.concat(chunks).toString(), forwarded: req.headers['x-forwarded-host'] }));
    });
  });
  upstream.on('connection', (socket) => {
    sockets.add(socket);
    socket.once('close', () => sockets.delete(socket));
  });
  upstream.on('upgrade', (req, socket, head) => {
    if (req.url !== '/socket/chat?room=1') return socket.destroy();
    const accept = createHash('sha1').update(`${req.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    let received = head;
    socket.on('data', (chunk) => {
      received = Buffer.concat([received, chunk]);
      if (received.length < 10) return;
      if (received[0] !== 0x81 || received[1] !== 0x84) return socket.destroy();
      const payload = received.subarray(6, 10).map((value, i) => value ^ received[2 + i % 4]);
      socket.write(Buffer.concat([Buffer.from([0x81, payload.length]), payload]));
      received = received.subarray(10);
    });
  });
  const upstreamPort = await listen(upstream);
  const reservation = net.createServer();
  const port = await listen(reservation);
  await new Promise((resolve) => reservation.close(resolve));
  const launcher = join(temporary, 'node_modules', ...name.split('/'), 'npm', 'bin', 'servd.cjs');
  launcherPath = launcher;
  child = spawn(process.execPath, [launcher,
    `--port=${port}`, `--static=${web}`, `--spa=/app=${app}`,
    `--proxy=/api/=http://127.0.0.1:${upstreamPort}/backend/?fixed=one`,
    `--ws=/ws=ws://127.0.0.1:${upstreamPort}/socket`,
    '--host=example.org', '--cors=http://example.org',
  ], { cwd: temporary, stdio: ['ignore', 'pipe', 'pipe'], shell: false });
  childExit = once(child, 'exit');
  let output = '';
  child.stderr.on('data', (chunk) => { output += chunk; });
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error(`CLI startup timed out: ${output}`)), 10_000);
    child.once('error', (error) => { clearTimeout(timeout); reject(error); });
    child.once('exit', (code) => { clearTimeout(timeout); reject(new Error(`CLI exited ${code}: ${output}`)); });
    child.stdout.on('data', (chunk) => {
      output += chunk;
      if (output.includes('listening on 0.0.0.0:')) { clearTimeout(timeout); resolve(); }
    });
  });
  let result = await request(port, '/content.txt', { headers: { 'Accept-Encoding': 'gzip', Origin: 'http://example.org' } });
  assert.equal(result.status, 200);
  assert.equal(result.headers['content-encoding'], 'gzip');
  assert.equal(gunzipSync(result.body).toString(), content);
  assert.equal(result.headers['cache-control'], 'no-cache');
  assert.equal(result.headers['access-control-allow-origin'], 'http://example.org');
  const etag = result.headers.etag;
  result = await request(port, '/content.txt', { headers: { 'Accept-Encoding': 'gzip', 'If-None-Match': etag } });
  assert.equal(result.status, 304);
  assert.equal(result.body.length, 0);
  result = await request(port, '/content.txt', { method: 'HEAD', headers: { 'Accept-Encoding': 'gzip' } });
  assert.equal(result.status, 200);
  assert.equal(result.body.length, 0);
  result = await request(port, '/content.txt', { headers: { Range: 'bytes=0-4' } });
  assert.equal(result.status, 206);
  assert.equal(result.body.toString(), 'servd');
  result = await request(port, '/app/dashboard', { headers: { Accept: 'text/html' } });
  assert.equal(result.body.toString(), '<h1>spa</h1>');
  assert.equal(result.headers['cache-control'], 'no-store');
  assert.equal((await request(port, '/app/missing.js', { headers: { Accept: 'text/html' } })).status, 404);
  result = await request(port, '/api/encoded%20path?q=two&q=three', { method: 'POST', body: 'payload' });
  assert.equal(result.status, 200);
  assert.equal(result.headers['cache-control'], 'public, max-age=17');
  assert.deepEqual(JSON.parse(result.body), { url: '/backend/encoded%20path?fixed=one&q=two&q=three', method: 'POST', body: 'payload', forwarded: 'example.org' });
  assert.equal((await request(port, '/', { headers: { Host: 'not-allowed.example' } })).status, 403);
  result = await request(port, '/api/', { method: 'OPTIONS', headers: { Origin: 'http://example.org', 'Access-Control-Request-Method': 'POST', 'Access-Control-Request-Headers': 'content-type' } });
  assert.equal(result.status, 204);
  assert.equal(result.headers['access-control-allow-methods'], 'POST');
  await websocket(port);
  child.kill('SIGTERM');
  const [code, signal] = await childExit;
  assert.equal(code, 0);
  assert.equal(signal, null);

  // Background daemon: the launcher process must exit after the handoff while
  // the detached supervisor keeps serving, restarts a killed worker, and stops
  // on request without leaving any process behind.
  const daemonReservation = net.createServer();
  const daemonPort = await listen(daemonReservation);
  await new Promise((resolve) => daemonReservation.close(resolve));
  daemonPorts.push(daemonPort);
  const daemonEnv = {
    ...process.env,
    SERVD_INTERNAL_CACHE_DIR: join(temporary, 'cache'),
    SERVD_INTERNAL_POLICY: '{"maxRestarts":5,"backoffBaseMs":100,"stableResetMs":60000,"stopTimeoutMs":4000}',
  };
  const runLauncher = (args) => spawnSync(process.execPath, [launcher, ...args], {
    cwd: temporary, encoding: 'utf8', shell: false, timeout: 30_000, env: daemonEnv,
  });
  const parseStatus = (output) => {
    const supervisor = output.match(/supervisor pid (\d+)/);
    const worker = output.match(/worker pid (\d+)/);
    const restarts = output.match(/restarts (\d+)/);
    assert.ok(supervisor && restarts, `status output: ${output}`);
    return { supervisor: Number(supervisor[1]), worker: worker ? Number(worker[1]) : 0, restarts: Number(restarts[1]) };
  };
  const poll = async (fn, timeout, label) => {
    const deadline = Date.now() + timeout;
    for (;;) {
      try {
        return await fn();
      } catch (error) {
        if (Date.now() > deadline) throw new Error(`${label} timed out: ${error.message}`);
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
    }
  };
  const assertGone = (pid) => poll(() => {
    try {
      process.kill(pid, 0);
    } catch (error) {
      if (error.code === 'ESRCH') return true;
      throw error;
    }
    throw new Error(`pid ${pid} still alive`);
  }, 10_000, `exit of pid ${pid}`);

  daemonCli = spawn(process.execPath, [launcher,
    '--daemon', `--port=${daemonPort}`, `--static=${web}`,
    `--proxy=/api/=http://127.0.0.1:${upstreamPort}/backend/?fixed=one`,
    `--ws=/ws=ws://127.0.0.1:${upstreamPort}/socket`,
    '--host=example.org', '--cors=http://example.org',
  ], { cwd: temporary, env: daemonEnv, stdio: ['ignore', 'pipe', 'pipe'], shell: false });
  daemonExit = once(daemonCli, 'exit');
  let daemonOutput = '';
  daemonCli.stdout.on('data', (chunk) => { daemonOutput += chunk; });
  daemonCli.stderr.on('data', (chunk) => { daemonOutput += chunk; });
  const daemonTimeout = new Promise((_, reject) => {
    setTimeout(() => reject(new Error(`daemon CLI did not exit: ${daemonOutput}`)), 20_000).unref();
  });
  const [daemonCode] = await Promise.race([daemonExit, daemonTimeout]);
  assert.equal(daemonCode, 0, daemonOutput);
  assert.match(daemonOutput, new RegExp(`daemon started on 0\\.0\\.0\\.0:${daemonPort}`), daemonOutput);
  assert.match(daemonOutput, /log: .+/);
  assert.equal((await request(daemonPort, '/')).body.toString(), '<h1>static</h1>');
  await websocket(daemonPort);

  let status = runLauncher(['--status', `--port=${daemonPort}`]);
  assert.equal(status.status, 0, status.stderr);
  let instance = parseStatus(status.stdout);
  process.kill(instance.worker, 'SIGKILL');

  await poll(async () => {
    const recovered = await request(daemonPort, '/');
    assert.equal(recovered.body.toString(), '<h1>static</h1>');
  }, 15_000, 'daemon auto-recovery');
  status = runLauncher(['--status', `--port=${daemonPort}`]);
  assert.equal(status.status, 0, status.stderr);
  const recovered = parseStatus(status.stdout);
  assert.equal(recovered.supervisor, instance.supervisor);
  assert.notEqual(recovered.worker, instance.worker);
  assert.equal(recovered.restarts, 1, status.stdout);
  result = await request(daemonPort, '/api/encoded%20path?q=two');
  assert.equal(result.status, 200);
  assert.deepEqual(JSON.parse(result.body), { url: '/backend/encoded%20path?fixed=one&q=two', method: 'GET', body: '', forwarded: 'example.org' });

  status = runLauncher(['--stop', `--port=${daemonPort}`]);
  assert.equal(status.status, 0, status.stderr);
  assert.match(status.stdout, /stopped/);
  await poll(async () => {
    try {
      await request(daemonPort, '/');
    } catch {
      return true; // listener is gone
    }
    throw new Error('daemon still serving after stop');
  }, 10_000, 'daemon shutdown');
  await assertGone(instance.supervisor);
  await assertGone(recovered.worker);
  status = runLauncher(['--status', `--port=${daemonPort}`]);
  assert.equal(status.status, 1, status.stdout + status.stderr);
  assert.match(status.stdout, /not running/);

  // TLS daemon: the same lifecycle over HTTPS/WSS, trusting the repo test
  // fixture certificate explicitly via ca (no verification bypass).
  const tlsReservation = net.createServer();
  const tlsPort = await listen(tlsReservation);
  await new Promise((resolve) => tlsReservation.close(resolve));
  daemonPorts.push(tlsPort);
  daemonCli = spawn(process.execPath, [launcher,
    '--daemon', `--port=${tlsPort}`, `--static=${web}`, `--spa=/app=${app}`,
    `--proxy=/api/=http://127.0.0.1:${upstreamPort}/backend/?fixed=one`,
    `--ws=/ws=ws://127.0.0.1:${upstreamPort}/socket`,
    '--host=example.org', '--cors=http://example.org',
    `--tls-cert=${join(root, 'testdata', 'tls', 'fullchain.pem')}`,
    `--tls-key=${join(root, 'testdata', 'tls', 'privkey.pem')}`,
  ], { cwd: temporary, env: daemonEnv, stdio: ['ignore', 'pipe', 'pipe'], shell: false });
  daemonExit = once(daemonCli, 'exit');
  let tlsOutput = '';
  daemonCli.stdout.on('data', (chunk) => { tlsOutput += chunk; });
  daemonCli.stderr.on('data', (chunk) => { tlsOutput += chunk; });
  const tlsTimeout = new Promise((_, reject) => {
    setTimeout(() => reject(new Error(`TLS daemon CLI did not exit: ${tlsOutput}`)), 20_000).unref();
  });
  const [tlsCode] = await Promise.race([daemonExit, tlsTimeout]);
  assert.equal(tlsCode, 0, tlsOutput);
  assert.match(tlsOutput, new RegExp(`https daemon started on 0\\.0\\.0\\.0:${tlsPort}`), tlsOutput);
  assert.match(tlsOutput, /log: .+/);

  result = await request(tlsPort, '/', { secure: true });
  assert.equal(result.status, 200);
  assert.equal(result.body.toString(), '<h1>static</h1>');
  result = await request(tlsPort, '/content.txt', { secure: true, headers: { 'Accept-Encoding': 'gzip', Origin: 'http://example.org' } });
  assert.equal(result.status, 200);
  assert.equal(result.headers['content-encoding'], 'gzip');
  assert.equal(gunzipSync(result.body).toString(), content);
  assert.equal(result.headers['access-control-allow-origin'], 'http://example.org');
  result = await request(tlsPort, '/app/dashboard', { secure: true, headers: { Accept: 'text/html' } });
  assert.equal(result.body.toString(), '<h1>spa</h1>');
  result = await request(tlsPort, '/api/encoded%20path?q=two', { secure: true });
  assert.equal(result.status, 200);
  assert.deepEqual(JSON.parse(result.body), { url: '/backend/encoded%20path?fixed=one&q=two', method: 'GET', body: '', forwarded: 'example.org' });
  await websocket(tlsPort, true);
  let plaintextLeaked = false;
  try {
    const leak = await request(tlsPort, '/');
    plaintextLeaked = leak.status === 200 && leak.body.toString() === '<h1>static</h1>';
  } catch { /* failed TLS handshake is the expected outcome */ }
  assert.ok(!plaintextLeaked, 'plain HTTP must not serve content from the TLS port');

  let tlsStatus = runLauncher(['--status', `--port=${tlsPort}`]);
  assert.equal(tlsStatus.status, 0, tlsStatus.stderr);
  const tlsInstance = parseStatus(tlsStatus.stdout);
  process.kill(tlsInstance.worker, 'SIGKILL');
  await poll(async () => {
    const recovered = await request(tlsPort, '/', { secure: true });
    assert.equal(recovered.body.toString(), '<h1>static</h1>');
  }, 15_000, 'TLS daemon auto-recovery');
  tlsStatus = runLauncher(['--status', `--port=${tlsPort}`]);
  assert.equal(tlsStatus.status, 0, tlsStatus.stderr);
  const tlsRecovered = parseStatus(tlsStatus.stdout);
  assert.equal(tlsRecovered.supervisor, tlsInstance.supervisor);
  assert.notEqual(tlsRecovered.worker, tlsInstance.worker);
  assert.equal(tlsRecovered.restarts, 1, tlsStatus.stdout);

  tlsStatus = runLauncher(['--stop', `--port=${tlsPort}`]);
  assert.equal(tlsStatus.status, 0, tlsStatus.stderr);
  assert.match(tlsStatus.stdout, /stopped/);
  await poll(async () => {
    try {
      await request(tlsPort, '/', { secure: true });
    } catch {
      return true; // listener is gone
    }
    throw new Error('TLS daemon still serving after stop');
  }, 10_000, 'TLS daemon shutdown');
  await assertGone(tlsInstance.supervisor);
  await assertGone(tlsRecovered.worker);
  tlsStatus = runLauncher(['--status', `--port=${tlsPort}`]);
  assert.equal(tlsStatus.status, 1, tlsStatus.stdout + tlsStatus.stderr);
  assert.match(tlsStatus.stdout, /not running/);
  console.log(`npm smoke passed: ${platform.id}, version ${version}, CLI/static/SPA/proxy/WS/CORS/gzip/cache/shutdown/daemon/TLS`);
} finally {
  if (daemonCli && daemonCli.exitCode === null && daemonCli.signalCode === null) {
    daemonCli.kill('SIGKILL');
    await daemonExit;
  }
  if (child && child.exitCode === null && child.signalCode === null) {
    child.kill('SIGTERM');
    const timer = setTimeout(() => child.kill('SIGKILL'), 7_000);
    await childExit;
    clearTimeout(timer);
  }
  for (const stoppedPort of daemonPorts) {
    spawnSync(process.execPath, [launcherPath, '--stop', `--port=${stoppedPort}`], {
      cwd: temporary, env: { ...process.env, SERVD_INTERNAL_CACHE_DIR: join(temporary, 'cache') }, timeout: 30_000,
    });
  }
  for (const socket of sockets) socket.destroy();
  if (upstream) await new Promise((resolve) => upstream.close(resolve));
  rmSync(temporary, { recursive: true, force: true });
}
