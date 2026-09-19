import test from 'node:test';
import assert from 'node:assert/strict';
import {
  copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync,
  readdirSync, rmSync, statSync, writeFileSync,
} from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { buildNpm, createManifests, parseArgs } from './build-npm.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const source = JSON.parse(readFileSync(join(ROOT, 'package.json'), 'utf8'));
const platforms = ['darwin-x64', 'darwin-arm64', 'linux-x64', 'linux-arm64', 'win32-x64', 'win32-arm64'];

test('root package publishes only the launcher with all exact-version optional dependencies', () => {
  assert.equal(source.name, '@qingyu31/servd');
  assert.match(source.version, /^\d+\.\d+\.\d+/);
  assert.deepEqual(source.bin, { servd: 'npm/bin/servd.cjs' });
  assert.deepEqual(source.files, ['npm/bin/servd.cjs']);
  assert.deepEqual(source.engines, { node: '>=18' });
  assert.deepEqual(source.optionalDependencies, Object.fromEntries(
    platforms.map((platform) => [`@qingyu31/servd-${platform}`, source.version]),
  ));
  assert.equal(source.dependencies, undefined);
  assert.equal(source.devDependencies, undefined);
  assert.equal(source.license, 'MIT');
  assert.deepEqual(source.scripts, {
    test: 'go test ./... && npm run test:node',
    'test:node': 'node --test npm/test/*.test.cjs scripts/*.test.mjs',
    'test:smoke': 'node scripts/smoke-npm.mjs',
    build: 'node scripts/build-npm.mjs',
    pack: 'node scripts/build-npm.mjs --pack',
    release: 'node scripts/publish.mjs',
    'release:dry': 'node scripts/publish.mjs --dry-run',
  });
});

test('generates seven same-version manifests with exact platform and file restrictions', () => {
  const manifests = createManifests(source);
  assert.deepEqual(Object.keys(manifests), ['main', ...platforms]);
  assert.equal(manifests.main.name, source.name);
  assert.deepEqual(manifests.main.bin, source.bin);
  assert.deepEqual(manifests.main.files, source.files);
  assert.deepEqual(manifests.main.engines, source.engines);
  assert.deepEqual(manifests.main.optionalDependencies, source.optionalDependencies);
  for (const [key, manifest] of Object.entries(manifests)) {
    assert.equal(manifest.version, source.version);
    assert.equal(manifest.scripts, undefined);
    assert.equal(manifest.license, 'MIT');
    assert.deepEqual(manifest.publishConfig, { access: 'public', registry: 'https://registry.npmjs.org' });
    if (key === 'main') {
      assert.equal(manifest.os, undefined);
      assert.equal(manifest.cpu, undefined);
      continue;
    }
    const [os, cpu] = key.split('-');
    assert.equal(manifest.name, `@qingyu31/servd-${key}`);
    assert.deepEqual(manifest.os, [os]);
    assert.deepEqual(manifest.cpu, [cpu]);
    assert.deepEqual(manifest.files, [os === 'win32' ? 'bin/servd.exe' : 'bin/servd']);
    assert.equal(manifest.bin, undefined);
    assert.equal(manifest.optionalDependencies, undefined);
  }
});

test('every generated version comes from the root manifest', () => {
  const version = '2.3.4-rc.1';
  const updated = {
    ...source, version,
    optionalDependencies: Object.fromEntries(platforms.map((platform) => [`@qingyu31/servd-${platform}`, version])),
  };
  const manifests = createManifests(updated);
  for (const manifest of Object.values(manifests)) assert.equal(manifest.version, version);
  assert.deepEqual(Object.values(manifests.main.optionalDependencies), Array(6).fill(version));
});

test('refuses version drift, missing optional packages, and main-package file leakage', () => {
  assert.throws(() => createManifests({ ...source, version: '1.2.3' }), /exactly match/);
  assert.throws(() => createManifests({ ...source, version: '0.1.0 -X other=value' }), /valid version/);
  assert.throws(() => createManifests({ ...source, optionalDependencies: {} }), /exactly match/);
  assert.throws(() => createManifests({ ...source, files: [...source.files, 'dist'] }), /only the launcher/);
  assert.throws(() => createManifests({ ...source, engines: { node: '>=16' } }), /Node >=18/);
  assert.throws(() => createManifests({ ...source, license: 'Apache-2.0' }), /license "MIT"/);
  assert.throws(() => createManifests({ ...source, license: undefined }), /license "MIT"/);
});

test('defaults to six platforms and accepts one-platform pack smoke builds', () => {
  assert.deepEqual(parseArgs([]), { platforms, pack: false });
  assert.deepEqual(parseArgs(['--pack']), { platforms, pack: true });
  for (const platform of platforms) {
    assert.deepEqual(parseArgs([`--platform=${platform}`, '--pack']), { platforms: [platform], pack: true });
  }
  assert.deepEqual(parseArgs(['--pack', '--platform=linux-arm64']), { platforms: ['linux-arm64'], pack: true });
  for (const args of [
    ['--platform=linux-ia32'], ['--platform='], ['--platform', 'linux-x64'], ['--publish'],
    ['--pack', '--pack'], ['--platform=linux-x64', '--platform=darwin-x64'],
  ]) assert.throws(() => parseArgs(args));
});

function fixture(t) {
  const rootDir = mkdtempSync(join(tmpdir(), 'servd-npm-test-'));
  t.after(() => rmSync(rootDir, { recursive: true, force: true }));
  writeFileSync(join(rootDir, 'package.json'), JSON.stringify(source));
  mkdirSync(join(rootDir, 'npm', 'bin'), { recursive: true });
  copyFileSync(join(ROOT, 'npm', 'bin', 'servd.cjs'), join(rootDir, 'npm', 'bin', 'servd.cjs'));
  const calls = [];
  let json = '';
  let log = '';
  // No Go compiler or npm process is executed in these tests.
  function run(command, args, options) {
    calls.push({ command, args, options });
    if (command === 'go') {
      writeFileSync(args[args.indexOf('-o') + 1], `mock binary ${options.env.GOOS}/${options.env.GOARCH}`);
      return { status: 0 };
    }
    assert.ok(args.includes('pack'), 'Only local npm pack is permitted');
    const manifest = JSON.parse(readFileSync(join(options.cwd, 'package.json'), 'utf8'));
    const filename = `${manifest.name.replace('@', '').replace('/', '-')}-${manifest.version}.tgz`;
    const destination = args[args.indexOf('--pack-destination') + 1];
    const unpackedSize = manifest.files.reduce((sum, file) => sum + statSync(join(options.cwd, file)).size, 0);
    writeFileSync(join(destination, filename), Buffer.alloc(53));
    return { status: 0, stdout: JSON.stringify([{ filename, size: 99999, unpackedSize }]), stderr: '' };
  }
  return {
    rootDir, run, calls,
    stdout: { write: (text) => { json += text; } },
    stderr: { write: (text) => { log += text; } },
    json: () => JSON.parse(json),
    log: () => log,
  };
}

test('mock build targets all six Go platforms with static, trimmed, versioned binaries', (t) => {
  const state = fixture(t);
  const summary = buildNpm({ ...state, options: parseArgs([]) });
  assert.equal(state.calls.length, 6);
  assert.equal(summary.version, source.version);
  assert.deepEqual(summary.platforms, platforms);
  assert.deepEqual(summary.tarballs, []);
  for (const [index, call] of state.calls.entries()) {
    const [os, cpu] = platforms[index].split('-');
    assert.equal(call.command, 'go');
    assert.equal(call.options.cwd, state.rootDir);
    assert.equal(call.options.shell, false);
    assert.equal(call.options.env.CGO_ENABLED, '0');
    assert.equal(call.options.env.GOOS, os === 'win32' ? 'windows' : os);
    assert.equal(call.options.env.GOARCH, cpu === 'x64' ? 'amd64' : 'arm64');
    const artifact = summary.binaries[index];
    assert.deepEqual(call.args, [
      'build', '-trimpath', '-ldflags', `-s -w -X go.qingyu31.com/servd/internal/app.Version=${source.version}`,
      '-o', artifact.path, './cmd/servd',
    ]);
    assert.equal(artifact.size, statSync(artifact.path).size);
    if (process.platform !== 'win32') assert.equal(statSync(artifact.path).mode & 0o777, 0o755);
    const packageDir = join(state.rootDir, 'dist', 'npm', `servd-${platforms[index]}`);
    assert.deepEqual(readdirSync(packageDir).sort(), ['bin', 'package.json']);
    assert.deepEqual(readdirSync(join(packageDir, 'bin')), [os === 'win32' ? 'servd.exe' : 'servd']);
  }
  const main = join(state.rootDir, 'dist', 'npm', 'main');
  assert.deepEqual(readdirSync(main).sort(), ['npm', 'package.json']);
  assert.equal(existsSync(join(main, 'bin')), false);
  assert.equal(readFileSync(join(main, source.files[0]), 'utf8'), readFileSync(join(ROOT, source.files[0]), 'utf8'));
  assert.deepEqual(state.json(), summary);
  assert.match(state.log(), /darwin-x64: [\d,]+ bytes \([\d.]+ MiB\)/);
});

test('mock single-platform packing writes seven manifests but packs only two packages', (t) => {
  const state = fixture(t);
  const summary = buildNpm({ ...state, options: parseArgs(['--platform=linux-arm64', '--pack']) });
  assert.equal(state.calls.length, 3);
  assert.equal(summary.binaries.length, 1);
  assert.deepEqual(summary.tarballs.map((entry) => entry.name), ['@qingyu31/servd', '@qingyu31/servd-linux-arm64']);
  for (const call of state.calls.slice(1)) {
    for (const arg of ['pack', '--json', '--offline', '--ignore-scripts']) assert.ok(call.args.includes(arg));
    assert.equal(call.options.shell, false);
    assert.equal(call.args[call.args.indexOf('--pack-destination') + 1], join(state.rootDir, 'dist', 'tarballs'));
  }
  for (const entry of summary.tarballs) {
    assert.equal(entry.size, 53, 'Report actual file size rather than trusting npm JSON size');
    assert.equal(entry.size, statSync(entry.path).size);
    assert.ok(entry.unpackedSize > 0);
  }
  for (const platform of platforms) {
    const packageDir = join(state.rootDir, 'dist', 'npm', `servd-${platform}`);
    const manifest = JSON.parse(readFileSync(join(packageDir, 'package.json'), 'utf8'));
    assert.equal(manifest.version, source.version);
    assert.equal(existsSync(join(packageDir, manifest.files[0])), platform === 'linux-arm64');
  }
  assert.deepEqual(state.json(), summary);
  assert.match(state.log(), /\.tgz: 53 bytes/);
});

test('mock full packing includes the main package and all six platform packages', (t) => {
  const state = fixture(t);
  const summary = buildNpm({ ...state, options: parseArgs(['--pack']) });
  assert.equal(summary.tarballs.length, 7);
  assert.equal(state.calls.filter((call) => call.command === 'go').length, 6);
  assert.deepEqual(summary.tarballs.map((entry) => entry.name), ['@qingyu31/servd', ...platforms.map((p) => `@qingyu31/servd-${p}`)]);
});

test('only removes known stale binaries when preparing a new build', (t) => {
  const state = fixture(t);
  const packageDir = join(state.rootDir, 'dist', 'npm', 'servd-linux-x64');
  mkdirSync(join(packageDir, 'bin'), { recursive: true });
  writeFileSync(join(packageDir, 'bin', 'servd'), 'stale binary');
  writeFileSync(join(state.rootDir, 'dist', 'keep.txt'), 'unrelated output');
  buildNpm({ ...state, options: parseArgs(['--platform=darwin-arm64']) });
  assert.equal(existsSync(join(packageDir, 'bin', 'servd')), false);
  assert.equal(readFileSync(join(state.rootDir, 'dist', 'keep.txt'), 'utf8'), 'unrelated output');
});

test('does not attempt npm pack after a failed compiler invocation', (t) => {
  const state = fixture(t);
  let calls = 0;
  assert.throws(() => buildNpm({
    ...state, options: parseArgs(['--pack']),
    run: () => { calls++; return { status: 1, stderr: 'compiler failed' }; },
  }), /go failed \(1\): compiler failed/);
  assert.equal(calls, 1);
  assert.equal(existsSync(join(state.rootDir, 'dist', 'tarballs')), false);
});

test('reports missing tools and failed npm pack commands', (t) => {
  const missing = fixture(t);
  assert.throws(() => buildNpm({
    ...missing, options: parseArgs(['--platform=linux-x64']),
    run: () => ({ error: new Error('ENOENT') }),
  }), /Cannot execute go: ENOENT/);
  const state = fixture(t);
  assert.throws(() => buildNpm({
    ...state, options: parseArgs(['--platform=linux-x64', '--pack']),
    run: (command, args, options) => command === 'go'
      ? state.run(command, args, options)
      : { status: 1, stderr: 'pack failed' },
  }), /failed \(1\): pack failed/);
});
