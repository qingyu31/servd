import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { buildNpm } from './build-npm.mjs';
import launcher from '../npm/bin/servd.cjs';

const { PLATFORMS } = launcher;
const REGISTRY = 'https://registry.npmjs.org';
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');

// parseFlags reads the release CLI: --dry-run, --otp=<code>, --tag=<dist-tag>
// and repeatable --platform=<os-arch>. NPM_OTP seeds the one-time code.
export function parseFlags(argv, env = process.env) {
  const options = { dryRun: false, otp: env.NPM_OTP || '', tag: 'latest', platforms: [] };
  let tagSet = false;
  for (const arg of argv) {
    if (arg === '--dry-run') {
      if (options.dryRun) throw new Error('duplicate --dry-run');
      options.dryRun = true;
    } else if (arg.startsWith('--otp=')) {
      const otp = arg.slice('--otp='.length);
      if (!/^\d+$/.test(otp)) throw new Error('--otp must be numeric');
      options.otp = otp;
    } else if (arg.startsWith('--tag=')) {
      const tag = arg.slice('--tag='.length);
      if (tagSet) throw new Error('duplicate --tag');
      if (!tag) throw new Error('empty --tag');
      options.tag = tag;
      tagSet = true;
    } else if (arg.startsWith('--platform=')) {
      const platform = arg.slice('--platform='.length);
      if (!PLATFORMS.includes(platform)) {
        throw new Error(`Unsupported platform: ${platform}. Choose ${PLATFORMS.join(', ')}.`);
      }
      if (options.platforms.includes(platform)) throw new Error(`duplicate --platform=${platform}`);
      options.platforms.push(platform);
    } else {
      throw new Error(`Unknown argument: ${arg}. Use --dry-run, --otp=<code>, --tag=<tag> or --platform=<os-arch>.`);
    }
  }
  return options;
}

// npm runs one npm CLI command pinned to the official registry. A userconfig
// path (from NPM_TOKEN) isolates auth from any local mirror configuration.
function npm(args, { userconfig = '', stdio = 'inherit', encoding } = {}) {
  const full = [...args, `--registry=${REGISTRY}`];
  if (userconfig) full.push(`--userconfig=${userconfig}`);
  const result = spawnSync('npm', full, { stdio, encoding, shell: false });
  if (result.error) throw new Error(`Cannot execute npm: ${result.error.message}`, { cause: result.error });
  return result;
}

// isPublished reports whether name@version already exists on the registry so a
// re-run skips it instead of aborting the whole release on a conflict.
function isPublished(name, version, userconfig) {
  const result = npm(['view', `${name}@${version}`, 'version'], {
    userconfig, stdio: ['ignore', 'pipe', 'pipe'], encoding: 'utf8',
  });
  if (result.status === 0) return true;
  const detail = `${result.stderr || ''}${result.stdout || ''}`;
  if (/E404|\b404\b|not found/i.test(detail)) return false;
  throw new Error(`Cannot check publish state for ${name}@${version}: ${detail.trim()}`);
}

function publishOne(tarball, version, options, userconfig) {
  const args = ['publish', tarball.path, '--access', 'public', '--tag', options.tag];
  if (options.otp) args.push('--otp', options.otp);
  if (options.dryRun) args.push('--dry-run');
  const result = npm(args, { userconfig });
  if (result.status !== 0) {
    throw new Error(`Failed to publish ${tarball.name}@${version} (npm exited ${result.signal || result.status})`);
  }
}

function main() {
  const options = parseFlags(process.argv.slice(2));
  const token = process.env.NPM_TOKEN || '';
  const npmVersion = spawnSync('npm', ['--version'], { encoding: 'utf8', shell: false });
  if (npmVersion.error) throw new Error(`npm CLI not available: ${npmVersion.error.message}`, { cause: npmVersion.error });
  if (npmVersion.status !== 0) throw new Error('npm --version failed; the npm CLI is required to pack and publish');

  let tmpDir = '';
  let userconfig = '';
  try {
    if (token) {
      // A throwaway userconfig keeps the mirror token in ~/.npmrc out of the
      // publish path and works headlessly in CI.
      tmpDir = mkdtempSync(join(tmpdir(), 'servd-publish-'));
      userconfig = join(tmpDir, '.npmrc');
      writeFileSync(userconfig, `registry=${REGISTRY}\n//registry.npmjs.org/:_authToken=${token}\n`, { mode: 0o600 });
    }
    const who = npm(['whoami'], { userconfig, stdio: ['ignore', 'pipe', 'pipe'], encoding: 'utf8' });
    if (who.status !== 0) {
      const hint = token ? 'NPM_TOKEN was rejected' : `run: npm login --registry=${REGISTRY} (or set NPM_TOKEN)`;
      throw new Error(`Not authenticated to ${REGISTRY}; ${hint}. npm said: ${(who.stderr || '').trim()}`);
    }
    const user = (who.stdout || '').trim();
    console.error(`servd release: user ${user} -> ${REGISTRY}${options.dryRun ? ' (dry-run)' : ''}`);

    const platforms = options.platforms.length ? options.platforms : [...PLATFORMS];
    console.error(`building and packing ${platforms.length} platform(s)...`);
    const summary = buildNpm({ options: { platforms, pack: true }, stdout: { write() {} } });

    const rootName = JSON.parse(readFileSync(join(ROOT, 'package.json'), 'utf8')).name;
    const mainTarball = summary.tarballs.find((entry) => entry.name === rootName);
    if (!mainTarball) throw new Error(`main package tarball (${rootName}) missing from the build summary`);
    // Platform packages go first so the main package's optionalDependencies
    // resolve for anyone installing while the release is in progress.
    const ordered = [...summary.tarballs.filter((entry) => entry.name !== rootName), mainTarball];

    const published = [];
    const skipped = [];
    for (const tarball of ordered) {
      if (!options.dryRun && isPublished(tarball.name, summary.version, userconfig)) {
        console.error(`skip    ${tarball.name}@${summary.version} (already published)`);
        skipped.push(tarball.name);
        continue;
      }
      console.error(`publish ${tarball.name}@${summary.version}${options.dryRun ? ' (dry-run)' : ''}`);
      publishOne(tarball, summary.version, options, userconfig);
      published.push(tarball.name);
    }
    const suffix = options.dryRun ? ' (dry-run, nothing was published)' : '';
    console.error(`\nservd release done${suffix}: published ${published.length}, skipped ${skipped.length}`);
  } finally {
    if (tmpDir) rmSync(tmpDir, { recursive: true, force: true });
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (error) {
    process.stderr.write(`servd release: ${error.message}\n`);
    process.exitCode = 1;
  }
}
