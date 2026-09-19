import {
  chmodSync, copyFileSync, existsSync, mkdirSync, readFileSync,
  rmSync, statSync, writeFileSync,
} from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import launcher from '../npm/bin/servd.cjs';

const { PLATFORMS, getPlatform } = launcher;
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const LAUNCHER = 'npm/bin/servd.cjs';

export function parseArgs(args) {
  let platform;
  let pack = false;
  for (const arg of args) {
    if (arg === '--pack' && !pack) {
      pack = true;
    } else if (arg.startsWith('--platform=') && platform === undefined) {
      platform = arg.slice('--platform='.length);
      if (!PLATFORMS.includes(platform)) {
        throw new Error(`Unsupported platform: ${platform}. Choose ${PLATFORMS.join(', ')}.`);
      }
    } else {
      throw new Error(`Unknown or repeated argument: ${arg}. Use --platform=<os-arch> and/or --pack.`);
    }
  }
  return { platforms: platform ? [platform] : [...PLATFORMS], pack };
}

export function createManifests(source) {
  const { name, version } = source;
  if (name !== '@qingyu31/servd' || !/^\d+\.\d+\.\d+(?:-[\da-z.-]+)?(?:\+[\da-z.-]+)?$/i.test(version || '')) {
    throw new Error('Root package.json must specify name @qingyu31/servd and a valid version.');
  }
  if (source.bin?.servd !== LAUNCHER || Object.keys(source.bin).length !== 1 ||
      !Array.isArray(source.files) || source.files.length !== 1 || source.files[0] !== LAUNCHER ||
      source.engines?.node !== '>=18') {
    throw new Error('Root package.json must publish only the launcher and require Node >=18.');
  }
  const optionalDependencies = Object.fromEntries(
    PLATFORMS.map((platform) => [`${name}-${platform}`, version]),
  );
  if (Object.keys(source.optionalDependencies || {}).length !== PLATFORMS.length ||
      Object.entries(optionalDependencies).some(([key, value]) => source.optionalDependencies[key] !== value)) {
    throw new Error('Every platform optionalDependency must exactly match the root version.');
  }
  const manifests = {
    main: {
      name,
      version,
      ...(source.description ? { description: source.description } : {}),
      bin: { servd: LAUNCHER },
      files: [LAUNCHER],
      engines: { node: source.engines.node },
      publishConfig: { access: 'public', registry: 'https://registry.npmjs.org' },
      optionalDependencies,
    },
  };
  for (const platform of PLATFORMS) {
    const [os, cpu] = platform.split('-');
    const { packageName, binary } = getPlatform(os, cpu);
    manifests[platform] = {
      name: packageName,
      version,
      description: `Native binary for servd on ${platform}.`,
      os: [os],
      cpu: [cpu],
      files: [`bin/${binary}`],
      publishConfig: { access: 'public', registry: 'https://registry.npmjs.org' },
    };
  }
  return manifests;
}

function npmInvocation() {
  const npmCli = process.env.npm_execpath;
  if (npmCli && basename(npmCli) === 'npm-cli.js') {
    return [process.execPath, [npmCli]];
  }
  if (process.platform === 'win32') {
    const cli = join(dirname(process.execPath), 'node_modules', 'npm', 'bin', 'npm-cli.js');
    if (!existsSync(cli)) throw new Error('Cannot locate npm-cli.js. Run this script with npm run pack.');
    // .cmd files cannot be spawned without a shell; invoke npm's JS entrypoint.
    return [process.execPath, [cli]];
  }
  return ['npm', []];
}

function readableSize(size) {
  return `${size.toLocaleString('en-US')} bytes (${(size / 1024 / 1024).toFixed(2)} MiB)`;
}

export function buildNpm({
  rootDir = ROOT,
  options = parseArgs(process.argv.slice(2)),
  run = spawnSync,
  stdout = process.stdout,
  stderr = process.stderr,
} = {}) {
  const root = resolve(rootDir);
  const source = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
  const manifests = createManifests(source);
  if (!options.platforms.length || options.platforms.some((platform) => !PLATFORMS.includes(platform))) {
    throw new Error('No valid build platforms selected.');
  }
  const output = join(root, 'dist', 'npm');
  const directories = {};
  for (const [key, manifest] of Object.entries(manifests)) {
    const directory = join(output, key === 'main' ? 'main' : manifest.name.replace(/^@[^/]+\//, ''));
    directories[key] = directory;
    mkdirSync(directory, { recursive: true });
    writeFileSync(join(directory, 'package.json'), `${JSON.stringify(manifest, null, 2)}\n`);
    if (key !== 'main') {
      mkdirSync(join(directory, 'bin'), { recursive: true });
      // A new manifest must not label an unselected platform's stale binary.
      rmSync(join(directory, manifest.files[0]), { force: true });
    }
  }
  const mainLauncher = join(directories.main, LAUNCHER);
  mkdirSync(dirname(mainLauncher), { recursive: true });
  copyFileSync(join(root, LAUNCHER), mainLauncher);
  chmodSync(mainLauncher, 0o755);

  function checkedRun(command, args, config) {
    const result = run(command, args, { shell: false, ...config });
    if (result.error) throw new Error(`Cannot execute ${command}: ${result.error.message}`, { cause: result.error });
    if (result.status !== 0) {
      throw new Error(`${command} failed (${result.signal || result.status}): ${result.stderr || ''}`);
    }
    return result;
  }

  const summary = { version: source.version, platforms: [...options.platforms], binaries: [], tarballs: [] };
  for (const platform of options.platforms) {
    const [os, cpu] = platform.split('-');
    const binaryPath = join(directories[platform], manifests[platform].files[0]);
    checkedRun('go', [
      'build', '-trimpath', '-ldflags', `-s -w -X go.qingyu31.com/servd/internal/app.Version=${source.version}`,
      '-o', binaryPath, './cmd/servd',
    ], {
      cwd: root,
      env: {
        ...process.env,
        CGO_ENABLED: '0',
        GOOS: os === 'win32' ? 'windows' : os,
        GOARCH: cpu === 'x64' ? 'amd64' : 'arm64',
      },
      stdio: 'inherit',
    });
    if (!statSync(binaryPath).isFile()) throw new Error(`Build did not produce a binary: ${binaryPath}`);
    chmodSync(binaryPath, 0o755);
    const size = statSync(binaryPath).size;
    summary.binaries.push({ platform, path: binaryPath, size });
    stderr.write(`${platform}: ${readableSize(size)}\n`);
  }

  if (options.pack) {
    const tarballDirectory = join(root, 'dist', 'tarballs');
    mkdirSync(tarballDirectory, { recursive: true });
    const [command, prefix] = npmInvocation();
    // Unbuilt platforms have manifests but must never be packed.
    for (const key of ['main', ...options.platforms]) {
      const result = checkedRun(command, [
        ...prefix, 'pack', '--json', '--offline', '--ignore-scripts', '--pack-destination', tarballDirectory,
      ], {
        cwd: directories[key],
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      const packed = JSON.parse(result.stdout);
      if (!Array.isArray(packed) || packed.length !== 1 || !packed[0].filename ||
          basename(packed[0].filename) !== packed[0].filename) {
        throw new Error(`Unexpected npm pack result for ${manifests[key].name}.`);
      }
      const tarball = join(tarballDirectory, packed[0].filename);
      const size = statSync(tarball).size;
      summary.tarballs.push({
        name: manifests[key].name,
        path: tarball,
        size,
        unpackedSize: packed[0].unpackedSize,
      });
      stderr.write(`${basename(tarball)}: ${readableSize(size)} (unpacked ${readableSize(packed[0].unpackedSize)})\n`);
    }
  }
  stdout.write(`${JSON.stringify(summary, null, 2)}\n`);
  return summary;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    buildNpm();
  } catch (error) {
    process.stderr.write(`servd npm build: ${error.message}\n`);
    process.exitCode = 1;
  }
}
