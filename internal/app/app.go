package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.qingyu31.com/servd/internal/config"
	"go.qingyu31.com/servd/internal/server"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

// Run serves cfg until ctx is cancelled or serving fails, then shuts down
// gracefully. onReady is called once the listener is bound and startup output
// has been written; it may be nil.
func Run(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, onReady func()) error {
	handler, err := server.New(cfg)
	if err != nil {
		return err
	}
	defer handler.Close()
	var tlsConfig *tls.Config
	if cfg.TLSCert != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		}
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	if tlsConfig != nil {
		// Serving a *tls.Conn directly lets net/http run the handshake,
		// bound by ReadHeaderTimeout, and populate r.TLS.
		listener = tls.NewListener(listener, tlsConfig)
	}
	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	printStartup(stdout, cfg)
	if onReady != nil {
		onReady()
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		handler.CloseTunnels()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			httpServer.Close()
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func printStartup(stdout io.Writer, cfg config.Config) {
	suffix := ""
	if cfg.TLSCert != "" {
		suffix = ", https"
	}
	fmt.Fprintf(stdout, "servd %s listening on 0.0.0.0:%d (all IPv4 interfaces%s)\n", Version, cfg.Port, suffix)
	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(stdout, "Hosts: unrestricted")
	} else {
		fmt.Fprintln(stdout, "Hosts:", strings.Join(cfg.Hosts, ", "))
	}
	for _, dir := range cfg.Directories {
		kind := "static"
		if dir.SPA {
			kind = "spa"
		}
		fmt.Fprintf(stdout, "  %s %s -> %s\n", kind, dir.Mount, dir.Path)
	}
	for _, proxy := range append(cfg.Proxies, cfg.WebSockets...) {
		fmt.Fprintf(stdout, "  proxy %s -> %s://%s%s\n", proxy.Mount, proxy.Target.Scheme, proxy.Target.Host, proxy.Target.EscapedPath())
	}
}
