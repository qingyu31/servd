'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const { spawn, spawnSync } = require('node:child_process');
const { constants, tmpdir } = require('node:os');
const { join } = require('node:path');
const { PLATFORMS, getPlatform, resolveBinary, launch } = require('../bin/servd.cjs');
const launcherPath = require.resolve('../bin/servd.cjs');

function fixture(platform = 'linux') {
  const parent = new EventEmitter();
  parent.platform = platform;
  parent.pid = 1234;
  parent.messages = '';
  parent.stderr = { write: (text) => { parent.messages += text; } };
  parent.kills = [];
  parent.kill = (...args) => parent.kills.push(args);
  const child = new EventEmitter();
  child.kills = [];
  child.kill = (signal) => { child.kills.push(signal); return true; };
  const calls = [];
  const spawnImpl = (...args) => { calls.push(args); return child; };
  return { parent, child, calls, spawnImpl };
}

test('maps every supported platform to its optional package and binary', () => {
  assert.deepEqual(PLATFORMS, [
    'darwin-x64', 'darwin-arm64', 'linux-x64', 'linux-arm64', 'win32-x64', 'win32-arm64',
  ]);
  for (const id of PLATFORMS) {
    const [platform, arch] = id.split('-');
    const binary = platform === 'win32' ? 'servd.exe' : 'servd';
    assert.deepEqual(getPlatform(platform, arch), { id, packageName: `@qingyu31/servd-${id}`, binary });
    assert.equal(resolveBinary(platform, arch, (specifier) => {
      assert.equal(specifier, `@qingyu31/servd-${id}/bin/${binary}`);
      return `/resolved/${binary}`;
    }), `/resolved/${binary}`);
  }
});

test('rejects unsupported platforms before resolving any package', () => {
  for (const [platform, arch] of [['freebsd', 'x64'], ['linux', 'ia32'], ['linux', 'arm']]) {
    assert.throws(() => resolveBinary(platform, arch, () => assert.fail('must not resolve')), {
      message: new RegExp(`Unsupported platform: ${platform}-${arch}`),
    });
  }
});

test('explains missing optional packages without any fallback', () => {
  const missing = Object.assign(new Error('not found'), { code: 'MODULE_NOT_FOUND' });
  assert.throws(() => resolveBinary('linux', 'arm64', () => { throw missing; }), (error) => {
    assert.match(error.message, /Missing optional package @qingyu31\/servd-linux-arm64/);
    assert.match(error.message, /--include=optional/);
    assert.equal(error.cause, missing);
    return true;
  });
  const unexpected = Object.assign(new Error('access denied'), { code: 'EACCES' });
  assert.throws(() => resolveBinary('linux', 'x64', () => { throw unexpected; }), (error) => error === unexpected);
});

test('forwards arguments unchanged without a shell and preserves the exit code', () => {
  const state = fixture();
  const args = ['--dir', 'a b', '--', '$HOME; exit 9', '中文', ''];
  assert.equal(launch('/some path/servd', args, state), state.child);
  assert.deepEqual(state.calls, [['/some path/servd', args, {
    stdio: 'inherit', shell: false, detached: true,
  }]]);
  assert.equal(state.calls[0][1], args);
  state.child.emit('close', 37, null);
  assert.equal(state.parent.exitCode, 37);
  assert.equal(state.parent.eventNames().length, 0);
  state.parent.emit('exit');
  assert.deepEqual(state.child.kills, []);
});

test('forwards POSIX signals once and waits for child close', () => {
  const state = fixture();
  launch('/servd', [], state);
  const signals = ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGQUIT', 'SIGUSR1', 'SIGUSR2'];
  for (const signal of signals) state.parent.emit(signal);
  assert.deepEqual(state.child.kills, signals);
  assert.equal(state.parent.exitCode, undefined);
  assert.deepEqual(state.parent.kills, []);
  state.child.emit('close', 0, null);
  assert.equal(state.parent.exitCode, 0);
  assert.equal(state.parent.eventNames().length, 0);
});

test('preserves child signal termination after removing forwarding handlers', () => {
  const state = fixture();
  state.parent.kill = (pid, signal) => {
    assert.equal(state.parent.listenerCount(signal), 0);
    state.parent.kills.push([pid, signal]);
  };
  launch('/servd', [], state);
  state.child.emit('close', null, 'SIGTERM');
  assert.deepEqual(state.parent.kills, [[1234, 'SIGTERM']]);
  assert.equal(state.parent.exitCode, 128 + constants.signals.SIGTERM);
});

test('uses numeric signal status when re-signalling is unavailable', () => {
  const state = fixture();
  state.parent.kill = () => { throw new Error('cannot signal'); };
  launch('/servd', [], state);
  state.child.emit('close', null, 'SIGINT');
  assert.equal(state.parent.exitCode, 128 + constants.signals.SIGINT);
});

test('Windows console signals are not sent a second time', () => {
  const state = fixture('win32');
  launch('C:\\servd.exe', [], state);
  assert.equal(state.calls[0][2].detached, false);
  for (const signal of ['SIGINT', 'SIGBREAK', 'SIGHUP']) state.parent.emit(signal);
  assert.deepEqual(state.child.kills, []);
  state.parent.emit('SIGTERM');
  assert.deepEqual(state.child.kills, ['SIGTERM']);
  state.child.emit('close', null, 'SIGTERM');
  assert.deepEqual(state.parent.kills, []);
  assert.equal(state.parent.exitCode, 128 + constants.signals.SIGTERM);
  assert.equal(state.parent.eventNames().length, 0);
});

test('reports spawn errors and cleans up listeners on close', () => {
  const state = fixture();
  launch('/missing/servd', [], state);
  state.child.emit('error', Object.assign(new Error('ENOENT'), { code: 'ENOENT' }));
  state.child.emit('close', -2, null);
  assert.equal(state.parent.exitCode, 1);
  assert.match(state.parent.messages, /Failed to run \/missing\/servd: ENOENT/);
  assert.equal(state.parent.eventNames().length, 0);
});

test('kills an active child on explicit parent exit rather than orphaning it', () => {
  const state = fixture();
  launch('/servd', [], state);
  state.parent.emit('exit');
  assert.deepEqual(state.child.kills, ['SIGKILL']);
  state.child.emit('close', null, 'SIGKILL');
});

test('requiring the launcher does not start a process', () => {
  const result = spawnSync(process.execPath, ['-e', `require(${JSON.stringify(launcherPath)}); console.log('loaded');`], {
    encoding: 'utf8', timeout: 10000,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, 'loaded\n');
  assert.equal(result.stderr, '');
});

function wrapperSource(childArgs) {
  return `require(${JSON.stringify(launcherPath)}).launch(process.execPath, ${JSON.stringify(childArgs)});`;
}

test('launcher exits after a daemon-style handoff and leaves the detached service alone', {
  skip: process.platform === 'win32', timeout: 15000,
}, async () => {
  // The fake CLI mimics servd --daemon: it spawns a detached grandchild,
  // reports readiness, then exits 0 while the grandchild keeps running.
  const fakeCli = `
    const { spawn } = require('node:child_process');
    const grandchild = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000);'], {
      detached: true, stdio: 'ignore',
    });
    grandchild.unref();
    console.log('GRANDCHILD:' + grandchild.pid);
    setTimeout(() => process.exit(0), 150);
  `;
  const result = spawnSync(process.execPath, ['-e', wrapperSource(['-e', fakeCli])], {
    encoding: 'utf8', timeout: 10_000,
  });
  assert.equal(result.status, 0, result.stderr);
  const match = result.stdout.match(/GRANDCHILD:(\d+)\n/);
  assert.ok(match, `fake CLI output: ${result.stdout}${result.stderr}`);
  const grandchild = Number(match[1]);
  try {
    process.kill(grandchild, 0);
  } catch (error) {
    if (error.code === 'ESRCH') {
      assert.fail('launcher killed the detached grandchild after the daemon-style handoff');
    }
    throw error;
  } finally {
    try { process.kill(grandchild, 'SIGKILL'); } catch { /* Already gone. */ }
  }
});

test('real Node child inherits output, receives exact arguments and exits with its code', () => {
  const args = ['a b', '"quoted"', '$HOME; exit 9', '--flag=value', '中文', ''];
  const childSource = "process.stdout.write(JSON.stringify(process.argv.slice(1))); process.stderr.write('child stderr'); process.exitCode = 37;";
  const result = spawnSync(process.execPath, ['-e', wrapperSource(['-e', childSource, '--', ...args])], {
    encoding: 'utf8', timeout: 10000,
  });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 37, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), args);
  assert.equal(result.stderr, 'child stderr');
});

test('real failed spawn returns an actionable error and status 1', () => {
  const nonexistent = join(tmpdir(), `servd-nonexistent-${process.pid}`);
  const source = `require(${JSON.stringify(launcherPath)}).launch(${JSON.stringify(nonexistent)}, []);`;
  const result = spawnSync(process.execPath, ['-e', source], { encoding: 'utf8', timeout: 10000 });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /servd: Failed to run .*ENOENT/);
});

function signalFixture(t, childSource) {
  const wrapper = spawn(process.execPath, ['-e', wrapperSource(['-e', childSource])], {
    stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  });
  let stdout = '';
  let stderr = '';
  let childPid;
  wrapper.stderr.on('data', (data) => { stderr += data; });
  const done = new Promise((resolve) => wrapper.once('close', (code, signal) => resolve({ code, signal })));
  const ready = new Promise((resolve, reject) => {
    wrapper.once('error', reject);
    wrapper.stdout.on('data', (data) => {
      stdout += data;
      const match = stdout.match(/READY:(\d+)\n/);
      if (match && !childPid) {
        childPid = Number(match[1]);
        resolve(childPid);
      }
    });
    wrapper.once('close', () => { if (!childPid) reject(new Error(`Wrapper exited before READY: ${stderr}`)); });
  });
  t.after(() => {
    if (wrapper.exitCode === null && wrapper.signalCode === null) {
      try { process.kill(-wrapper.pid, 'SIGKILL'); } catch { /* Already exited. */ }
    }
    if (childPid) {
      try { process.kill(childPid, 'SIGKILL'); } catch { /* Already reaped. */ }
    }
  });
  return { wrapper, ready, done, output: () => stdout, errors: () => stderr };
}

test('real POSIX SIGTERM reaches child and wrapper exits by the same signal', {
  skip: process.platform === 'win32', timeout: 10000,
}, async (t) => {
  const state = signalFixture(t, "console.log('READY:' + process.pid); setInterval(() => {}, 1000);");
  const pid = await state.ready;
  state.wrapper.kill('SIGTERM');
  assert.deepEqual(await state.done, { code: null, signal: 'SIGTERM' }, state.errors());
  assert.throws(() => process.kill(pid, 0), { code: 'ESRCH' });
});

test('a POSIX foreground-group Ctrl+C is delivered only once and graceful exit is awaited', {
  skip: process.platform === 'win32', timeout: 10000,
}, async (t) => {
  const source = `
    let count = 0;
    process.on('SIGINT', () => {
      count++;
      if (count === 1) setTimeout(() => { console.log('COUNT:' + count); process.exit(23); }, 120);
    });
    console.log('READY:' + process.pid);
    setInterval(() => {}, 1000);
  `;
  const state = signalFixture(t, source);
  const pid = await state.ready;
  process.kill(-state.wrapper.pid, 'SIGINT');
  assert.deepEqual(await state.done, { code: 23, signal: null }, state.errors());
  assert.match(state.output(), /COUNT:1\n/);
  assert.throws(() => process.kill(pid, 0), { code: 'ESRCH' });
});
