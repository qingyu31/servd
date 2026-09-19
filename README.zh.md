# servd

[English](./README.md)

一个轻量、自包含的静态文件服务器，适用于开发、测试和轻量托管 —— 可作为 npm [`http-server`](https://www.npmjs.com/package/http-server) 的替代品。

`servd` 是一个单一的 Go 二进制文件，没有运行时依赖。它可以托管静态文件和 SPA 路由，反向代理 HTTP 与 WebSocket 上游，应用 CORS 和 Host 过滤，终结 TLS，并能以自我监督的后台守护进程方式运行。它通过 npm 分发：一个极小的启动器加上各平台的原生二进制，因此 `npx @qingyu31/servd` 开箱即用。

## 特性

- **静态文件** —— 托管一个或多个目录，每个目录可选择挂载到某个路径前缀。
- **SPA 回退** —— 接受 `text/html` 的无扩展名导航请求会回退到 `index.html`。
- **HTTP 反向代理** —— 将某个挂载前缀转发到上游 URL，并进行路径重写。
- **WebSocket 代理** —— 通过同一端口隧道转发 `ws://` / `wss://` 上游。
- **CORS** —— 允许通配符或显式的来源列表（不携带凭证）。
- **Host 过滤** —— 限制接受哪些 `Host` 请求头。
- **TLS** —— 基于 PEM 证书和密钥提供 TLS 1.2+ 的 HTTPS 与 WSS。
- **压缩** —— 遵循 `Accept-Encoding`，优先提供预压缩的 `.gz` / `.br` 同级文件，并对可压缩响应实时 gzip。
- **合理的缓存** —— 静态文件通过 `ETag` / `Last-Modified` 重新校验；SPA HTML 从不缓存。
- **守护进程模式** —— 以分离方式运行，监督进程使用指数退避重启崩溃的 worker。
- **零依赖** —— 单一静态二进制；托管服务无需 Node 运行时。

## 安装

需要 Node.js >= 18（仅供启动器使用）。正确的原生二进制会作为可选依赖自动拉取。

```sh
npm install --global @qingyu31/servd
# 或者不安装直接运行
npx @qingyu31/servd
```

该包发布在 `@qingyu31` 作用域下，但它安装的命令就是 `servd` —— 下文所有示例都使用 `servd`。

支持的平台：`darwin-x64`、`darwin-arm64`、`linux-x64`、`linux-arm64`、`win32-x64`、`win32-arm64`。

如果你的包管理器跳过了可选依赖，请启用它们后重新安装：

```sh
npm install --include=optional @qingyu31/servd
```

### 从源码构建

需要 Go 1.24+。

```sh
go build -o bin/servd ./cmd/servd
./bin/servd --help
```

## 快速开始

在 8080 端口托管当前目录（未指定任何路由时的默认行为）：

```sh
servd
```

在自定义端口托管指定目录：

```sh
servd --static=./dist --port=3000
```

托管一个 SPA，并配置 API 代理和 WebSocket 代理：

```sh
servd \
  --spa=./frontend \
  --proxy=/api/=http://localhost:9000/api/ \
  --ws=/ws=ws://localhost:9000/ws \
  --port=8080
```

`servd` 监听所有 IPv4 网卡（`0.0.0.0`）。启动时它会打印解析后的路由，方便你确认实际托管的内容。

## CLI 参考

```
servd [options]
  --static=./dir              静态根目录；可重复
  --static=/assets=./dir      挂载在 /assets 的静态根目录
  --spa=./dir                 SPA 根目录；可重复，同样支持 /mount=./dir
  --proxy=/api/=http://example.org/api/
                              带前缀重写的 HTTP 代理；可重复
  --ws=/ws=wss://example.org/ws
                              WebSocket 代理；可重复
  --port=8080                 TCP 端口（1-65535）；只能指定一次
  --host=example.org          允许的请求主机名或 IP；可重复
  --cors=*                    允许的 Origin 或 *；可重复，不含凭证
  --tls-cert=./fullchain.pem  TLS 证书（PEM）；需与 --tls-key 搭配
  --tls-key=./privkey.pem     TLS 私钥（PEM）；提供 HTTPS 和 WSS
  --daemon                    后台运行并在崩溃时重启
  --status                    显示某端口后台实例的状态
  --stop                      停止某端口的后台实例
  --help, -h                  显示帮助
  --version                   显示版本
```

### 路由与挂载

- `--static` 和 `--spa` 接受裸目录（`./dir`，挂载在 `/`）或 `mount=directory` 形式（`/assets=./dir`）。两者都可重复。
- `--proxy` 和 `--ws` 必须使用 `mount=target` 形式。挂载前缀会被剥离，剩余部分追加到目标路径之后。两者都可重复，同一选项的重复挂载会被拒绝。
- **最长挂载优先。** 更具体的前缀会先于更短的前缀匹配。
- 在同一挂载点上，**代理优先于文件**。

### 静态与 SPA 行为

- 未指定任何路由时，`servd` 托管当前工作目录。
- 目录请求会重定向到带尾斜杠的形式，随后在存在时提供 `index.html`。
- SPA 回退需要 `Accept: text/html` 请求头**且**为无扩展名的导航路径；资源请求按正常方式提供。
- 静态文件以 `Cache-Control: no-cache` 提供，并通过 `ETag` / `Last-Modified` 重新校验。SPA HTML 响应使用 `Cache-Control: no-store`。
- 压缩响应：当存在同级 `file.ext.gz` 或 `file.ext.br` 且不早于原文件时优先使用；否则对可压缩的文本响应动态 gzip。上游代理的请求头和内容编码会被保留。

### Host 过滤与 CORS

- `--host` 限制接受的 `Host` 请求头；其他主机的请求返回 `403`。Host 过滤**不**配置 DNS，也**不**提供任何身份认证。
- `--cors=*` 允许任意来源；否则传入一个或多个显式来源。CORS 响应从不包含凭证。启用 CORS 时，来自被代理上游的 `Access-Control-*` 头会被剥离，以确保 `servd` 具有权威性。

### TLS（HTTPS 与 WSS）

```sh
servd --static=./dist --tls-cert=./fullchain.pem --tls-key=./privkey.pem --port=8443
```

- `--tls-cert` 和 `--tls-key` 必须一起使用。证书文件可携带其证书链和多个 SAN。
- 该端口随后通过 TLS 1.2+ 提供 HTTPS 和 WSS（仅 HTTP/1.1）。
- 重启进程（或先 `--stop` 再 `--daemon`）以加载续期后的证书文件。
- **请将密钥保存在所有被托管目录之外** —— 未指定路由时，当前目录会被公开托管。

## 守护进程模式

`servd` 可以以分离方式运行，由监督进程保持 worker 存活。守护进程的作用域为**每用户、每端口**。

```sh
# 后台启动（仅在 worker 真正开始监听后返回）
servd --static=./dist --port=8080 --daemon

# 查看某端口实例的状态
servd --status --port=8080

# 停止某端口的实例
servd --stop --port=8080
```

- `--daemon` 仅在 worker 真正开始监听后才返回。
- 崩溃的 worker 会以指数退避重启，最多**连续 5 次**；持续存活的 worker 会重置该配额。配额耗尽后实例被标记为 `failed`，并保持该状态直到 `--stop`。
- `--status` 和 `--stop` 只接受 `--port`。
- 日志在 5 MiB 时轮转，保留一个备份。日志路径由 `--daemon` 和 `--status` 打印。

## 与 `http-server` 的对比

| 能力 | `http-server` | `servd` |
| --- | --- | --- |
| 静态文件托管 | 是 | 是 |
| SPA 回退 | 基础 | 是（基于内容协商） |
| HTTP 代理 | 仅 `--proxy` 回退 | 一等的挂载代理，支持重写 |
| WebSocket 代理 | 否 | 是 |
| CORS | 是 | 是（通配符或显式来源） |
| Host 过滤 | 否 | 是 |
| TLS / HTTPS | 是 | 是（TLS 1.2+，HTTP/1.1） |
| 预压缩 `.gz` / `.br` | 仅 `.gz` | `.gz` 和 `.br` |
| 带重启的后台守护进程 | 否 | 是 |
| 运行时依赖 | Node.js | 无（单一 Go 二进制） |

## 开发

```sh
# 运行完整测试套件（Go + Node）
npm test

# 仅 Node 测试（启动器和构建脚本）
npm run test:node

# 对打包后的启动器进行冒烟测试
npm run test:smoke

# 为所有平台构建原生二进制到 dist/npm
npm run build

# 构建单个平台
npm run build -- --platform=darwin-arm64

# 构建并生成 npm tarball 到 dist/tarballs
npm run pack
```

npm 包只发布启动器（`npm/bin/servd.cjs`）；每个平台的二进制作为独立的 `@qingyu31/servd-<os>-<arch>` 包发布，并作为可选依赖被引用。启动器会解析当前平台对应的二进制，并透明地转发信号和退出码。

## 发布

发布始终指向官方 npm registry（`https://registry.npmjs.org`），不受你本地默认 registry 影响 —— 用于安装的镜像仓库绝不会收到这些包。

先向官方 registry 登录一次：

```sh
npm login --registry=https://registry.npmjs.org
```

然后用一条命令完成构建、打包并发布全部内容（6 个平台二进制 + 主包）：

```sh
pnpm run release          # 构建、打包并发布
pnpm run release:dry      # 同样流程但使用 npm publish --dry-run（不实际发布）
```

发布脚本会交叉编译并打包每个平台，先发布平台包、最后发布主包，并自动跳过已存在的 `name@version`。常用参数与环境变量：

- `NPM_TOKEN` —— 无交互发布（CI）。脚本会写入一个锁定官方 registry 的临时 npmrc，因此不会使用你本地的镜像 token。
- `--otp=<验证码>` 或 `NPM_OTP` —— 开启了两步验证（2FA）的账号所需的一次性验证码。
- `--tag=<dist-tag>` —— 发布到 `latest` 以外的 dist-tag（例如 `--tag=next`）。
- `--platform=<os-arch>` —— 可重复；只构建并发布部分平台。

命令行 `--registry` 与写入每个生成 manifest 的 `publishConfig.registry` 都指向官方 registry，因此即使手动 `npm publish <tarball>` 也会发布到正确位置。发布新版本前请先在 `package.json` 中修改 `version`；脚本不会改动它。

## 项目结构

```
cmd/servd/          CLI 入口与用法文本
internal/config/      参数解析与校验
internal/app/         服务器生命周期与启动输出
internal/server/      路由、静态/SPA 托管、代理、CORS、压缩
internal/daemon/      监督进程、worker、控制协议、日志
npm/bin/servd.cjs   发布到 npm 的 Node 启动器
scripts/              构建与冒烟测试工具
```

## 许可证

许可证详情请见仓库。
