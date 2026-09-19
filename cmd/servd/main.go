package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"go.qingyu31.com/servd/internal/app"
	"go.qingyu31.com/servd/internal/config"
	"go.qingyu31.com/servd/internal/daemon"
)

const usage = `servd - a small static, SPA, HTTP and WebSocket server

Usage: servd [options]
  --static=./dir              Static root; repeatable
  --static=/assets=./dir      Static root mounted at /assets
  --spa=./dir                 SPA root; repeatable, also supports /mount=./dir
  --proxy=/api/=http://example.org/api/
                             HTTP proxy with prefix rewriting; repeatable
  --ws=/ws=wss://example.org/ws
                             WebSocket proxy; repeatable
  --port=8080                 TCP port (1-65535); specify once
  --host=example.org          Allowed request hostname or IP; repeatable
  --cors=*                   Allowed Origin or *; repeatable, no credentials
  --tls-cert=./fullchain.pem TLS certificate (PEM); pair with --tls-key
  --tls-key=./privkey.pem    TLS private key (PEM); serves HTTPS and WSS
  --daemon                    Run in the background and restart on crashes
  --status                    Show the background instance status for a port
  --stop                      Stop the background instance for a port
  --help, -h                 Show help
  --version                  Show version

Listens on all IPv4 interfaces (0.0.0.0). With no routes, serves the current
working directory. Host filters do not configure DNS or provide authentication.
Longest mount wins; HTTP proxies take precedence over files at the same mount.
SPA fallback requires Accept: text/html and an extensionless navigation path.
Static files revalidate caches; SPA HTML is not stored. Upstream headers and
content encodings are preserved. Local responses support gzip and .gz/.br files.

Background instances are per user and per port: start with --daemon, inspect
with --status, and terminate with --stop. --daemon returns only after the
worker is actually listening; a crashed worker restarts with backoff up to
five consecutive times. Logs rotate at 5 MiB with one backup and their path is
printed by --daemon and --status.

With --tls-cert/--tls-key the port serves HTTPS and WSS over TLS 1.2+
(HTTP/1.1 only); a certificate file may carry its chain and multiple SANs.
Restart (or --stop then --daemon) to load renewed files. Keep keys outside
every served directory - with no routes the current directory is served.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if code, internal := daemon.RunInternal(stdout, stderr); internal {
		return code
	}
	cfg, err := config.Parse(args)
	if err != nil {
		fmt.Fprintln(stderr, "servd:", err)
		fmt.Fprintln(stderr, "Run servd --help for usage.")
		return 2
	}
	if cfg.Help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if cfg.Version {
		fmt.Fprintln(stdout, app.Version)
		return 0
	}
	switch cfg.Action {
	case config.ActionStatus:
		return daemon.Status(cfg.Port, stdout, stderr)
	case config.ActionStop:
		return daemon.Stop(cfg.Port, stdout, stderr)
	case config.ActionDaemon:
		if err := daemon.Start(cfg, stdout); err != nil {
			fmt.Fprintln(stderr, "servd:", err)
			return 1
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, cfg, stdout, stderr, nil); err != nil {
		fmt.Fprintln(stderr, "servd:", err)
		return 1
	}
	return 0
}
