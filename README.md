# servd

[中文文档](./README.zh.md)

A small, self-contained static file server for development, testing, and lightweight hosting — a drop-in alternative to npm's [`http-server`](https://www.npmjs.com/package/http-server).

`servd` is a single Go binary with no runtime dependencies. It serves static files and SPA routes, reverse-proxies HTTP and WebSocket upstreams, applies CORS and host filters, terminates TLS, and can run as a self-supervising background daemon. It ships through npm as a tiny launcher plus per-platform native binaries, so `npx @qingyu31/servd` just works.

## Features

- **Static files** — serve one or more directories, each optionally mounted at a path prefix.
- **SPA fallback** — extensionless navigations that accept `text/html` fall back to `index.html`.
- **HTTP reverse proxy** — forward a mount prefix to an upstream URL with path rewriting.
- **WebSocket proxy** — tunnel `ws://` / `wss://` upstreams through the same port.
- **CORS** — allow a wildcard or an explicit list of origins (no credentials).
- **Host filtering** — restrict which `Host` headers are accepted.
- **TLS** — serve HTTPS and WSS over TLS 1.2+ from a PEM certificate and key.
- **Compression** — honor `Accept-Encoding`, serve pre-compressed `.gz` / `.br` siblings, and gzip compressible responses on the fly.
- **Sane caching** — static files revalidate with `ETag` / `Last-Modified`; SPA HTML is never stored.
- **Daemon mode** — run detached with a supervisor that restarts a crashed worker using exponential backoff.
- **Zero dependencies** — one static binary; no Node runtime required to serve.

## Install

Requires Node.js >= 18 (used only by the launcher). The correct native binary is pulled in automatically as an optional dependency.

```sh
npm install --global @qingyu31/servd
# or run it without installing
npx @qingyu31/servd
```

The package is published under the `@qingyu31` scope, but the command it installs is simply `servd` — every example below uses `servd`.

Supported platforms: `darwin-x64`, `darwin-arm64`, `linux-x64`, `linux-arm64`, `win32-x64`, `win32-arm64`.

If your package manager skips optional dependencies, reinstall with them enabled:

```sh
npm install --include=optional @qingyu31/servd
```

### Build from source

Requires Go 1.24+.

```sh
go build -o bin/servd ./cmd/servd
./bin/servd --help
```

## Quick start

Serve the current directory on port 8080 (the default when no routes are given):

```sh
servd
```

Serve a specific directory on a custom port:

```sh
servd --static=./dist --port=3000
```

Serve an SPA with an API proxy and a WebSocket proxy:

```sh
servd \
  --spa=./frontend \
  --proxy=/api/=http://localhost:9000/api/ \
  --ws=/ws=ws://localhost:9000/ws \
  --port=8080
```

`servd` listens on all IPv4 interfaces (`0.0.0.0`). On startup it prints the resolved routes so you can confirm what is being served.

## CLI reference

```
servd [options]
  --static=./dir              Static root; repeatable
  --static=/assets=./dir      Static root mounted at /assets
  --spa=./dir                 SPA root; repeatable, also supports /mount=./dir
  --proxy=/api/=http://example.org/api/
                              HTTP proxy with prefix rewriting; repeatable
  --ws=/ws=wss://example.org/ws
                              WebSocket proxy; repeatable
  --port=8080                 TCP port (1-65535); specify once
  --host=example.org          Allowed request hostname or IP; repeatable
  --cors=*                    Allowed Origin or *; repeatable, no credentials
  --tls-cert=./fullchain.pem  TLS certificate (PEM); pair with --tls-key
  --tls-key=./privkey.pem     TLS private key (PEM); serves HTTPS and WSS
  --daemon                    Run in the background and restart on crashes
  --status                    Show the background instance status for a port
  --stop                      Stop the background instance for a port
  --help, -h                  Show help
  --version                   Show version
```

### Routes and mounting

- `--static` and `--spa` accept either a bare directory (`./dir`, mounted at `/`) or a `mount=directory` pair (`/assets=./dir`). Both are repeatable.
- `--proxy` and `--ws` require the `mount=target` form. The mount prefix is stripped and the remainder is appended to the target path. Both are repeatable, and duplicate mounts for the same option are rejected.
- **Longest mount wins.** More specific prefixes are matched before shorter ones.
- **Proxies take precedence over files** at the same mount point.

### Static and SPA behavior

- With no routes, `servd` serves the current working directory.
- Directory requests redirect to a trailing slash and then serve `index.html` when present.
- SPA fallback requires an `Accept: text/html` header **and** an extensionless navigation path; asset requests are served normally.
- Static files are served with `Cache-Control: no-cache` and revalidate via `ETag` / `Last-Modified`. SPA HTML responses use `Cache-Control: no-store`.
- Compressed responses: a sibling `file.ext.gz` or `file.ext.br` is preferred when present and at least as new as the original; otherwise compressible text responses are gzipped dynamically. Upstream proxy headers and content encodings are preserved.

### Host filtering and CORS

- `--host` restricts the accepted `Host` header; requests for other hosts get `403`. Host filters do **not** configure DNS and provide **no authentication**.
- `--cors=*` allows any origin; otherwise pass one or more explicit origins. CORS responses never include credentials. When CORS is enabled, `Access-Control-*` headers from a proxied upstream are stripped so `servd` stays authoritative.

### TLS (HTTPS and WSS)

```sh
servd --static=./dist --tls-cert=./fullchain.pem --tls-key=./privkey.pem --port=8443
```

- `--tls-cert` and `--tls-key` must be used together. The certificate file may carry its chain and multiple SANs.
- The port then serves HTTPS and WSS over TLS 1.2+ (HTTP/1.1 only).
- Restart the process (or `--stop` then `--daemon`) to pick up renewed certificate files.
- **Keep keys outside every served directory** — with no routes the current directory is served publicly.

## Daemon mode

`servd` can run detached with a supervisor that keeps a worker alive. Daemons are scoped **per user and per port**.

```sh
# Start in the background (returns only once the worker is listening)
servd --static=./dist --port=8080 --daemon

# Inspect the instance for a port
servd --status --port=8080

# Stop the instance for a port
servd --stop --port=8080
```

- `--daemon` returns only after the worker is actually listening.
- A crashed worker restarts with exponential backoff, up to **five consecutive** times; a worker that stays up resets the budget. Once the budget is exhausted the instance is marked `failed` and holds until `--stop`.
- `--status` and `--stop` accept only `--port`.
- Logs rotate at 5 MiB with one backup. The log path is printed by `--daemon` and `--status`.

## Comparison with `http-server`

| Capability | `http-server` | `servd` |
| --- | --- | --- |
| Static file serving | Yes | Yes |
| SPA fallback | Basic | Yes (content-negotiated) |
| HTTP proxy | `--proxy` fallback only | First-class mount proxies with rewriting |
| WebSocket proxy | No | Yes |
| CORS | Yes | Yes (wildcard or explicit origins) |
| Host filtering | No | Yes |
| TLS / HTTPS | Yes | Yes (TLS 1.2+, HTTP/1.1) |
| Pre-compressed `.gz` / `.br` | `.gz` only | `.gz` and `.br` |
| Background daemon with restart | No | Yes |
| Runtime dependencies | Node.js | None (single Go binary) |

## Development

```sh
# Run the full test suite (Go + Node)
npm test

# Node-only tests (launcher and build scripts)
npm run test:node

# Smoke-test the packaged launcher
npm run test:smoke

# Build native binaries for all platforms into dist/npm
npm run build

# Build a single platform
npm run build -- --platform=darwin-arm64

# Build and produce npm tarballs into dist/tarballs
npm run pack
```

The npm package publishes only the launcher (`npm/bin/servd.cjs`); each platform's binary is published as a separate `@qingyu31/servd-<os>-<arch>` package and referenced as an optional dependency. The launcher resolves the binary for the current platform and forwards signals and exit codes transparently.

## Publishing

Releases always go to the official npm registry (`https://registry.npmjs.org`), regardless of your local default registry — a mirror used for installs never receives the packages.

Authenticate once against the official registry:

```sh
npm login --registry=https://registry.npmjs.org
```

Then build, pack, and publish everything (all six platform binaries plus the main package) with one command:

```sh
pnpm run release          # build, pack, and publish
pnpm run release:dry      # same flow with npm publish --dry-run (publishes nothing)
```

The release script cross-compiles and packs every platform, publishes the platform packages first and the main package last, and skips any `name@version` that already exists. Useful flags and environment variables:

- `NPM_TOKEN` — publish headlessly (CI). The script writes a throwaway npmrc pinned to the official registry, so your local mirror token is never used.
- `--otp=<code>` or `NPM_OTP` — one-time password for accounts with 2FA enabled.
- `--tag=<dist-tag>` — publish under a dist-tag other than `latest` (for example `--tag=next`).
- `--platform=<os-arch>` — repeatable; build and publish only a subset of platforms.

Both the CLI `--registry` flag and a `publishConfig.registry` baked into every generated manifest point at the official registry, so even a manual `npm publish <tarball>` lands in the right place. Bump `version` in `package.json` before a new release; the script never changes it.

## Project layout

```
cmd/servd/          CLI entrypoint and usage text
internal/config/      Flag parsing and validation
internal/app/         Server lifecycle and startup output
internal/server/      Routing, static/SPA serving, proxy, CORS, compression
internal/daemon/      Supervisor, worker, control protocol, logging
npm/bin/servd.cjs   Node launcher shipped to npm
scripts/              Build and smoke-test tooling
```

## License

See the repository for license details.
