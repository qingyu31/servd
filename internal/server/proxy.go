package server

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"go.qingyu31.com/servd/internal/config"
)

func (s *Server) reverseProxy(cfg config.Proxy) *httputil.ReverseProxy {
	target := *cfg.Target
	if target.Scheme == "ws" {
		target.Scheme = "http"
	} else if target.Scheme == "wss" {
		target.Scheme = "https"
	}
	return &httputil.ReverseProxy{
		Transport: s.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			prefix := cfg.Mount
			if prefix == "/" {
				prefix = ""
			}
			suffix := strings.TrimPrefix(pr.In.URL.Path, prefix)
			rawSuffix := pr.In.URL.EscapedPath()[escapedOffset(pr.In.URL.EscapedPath(), len(prefix)):]
			pr.SetURL(&target)
			pr.Out.URL.Path, pr.Out.URL.RawPath = joinPath(target.Path, target.EscapedPath(), suffix, rawSuffix)
			for name := range pr.Out.Header {
				lower := strings.ToLower(name)
				if lower == "forwarded" || lower == "x-forwarded" || strings.HasPrefix(lower, "x-forwarded-") {
					pr.Out.Header.Del(name)
				}
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(response *http.Response) error {
			if len(s.origins) > 0 {
				for name := range response.Header {
					if strings.HasPrefix(strings.ToLower(name), "access-control-") {
						response.Header.Del(name)
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				status = http.StatusGatewayTimeout
			}
			writeError(w, status)
		},
	}
}

func escapedOffset(raw string, decodedBytes int) int {
	i := 0
	for n := 0; n < decodedBytes && i < len(raw); n++ {
		if raw[i] == '%' {
			i += 3
		} else {
			i++
		}
	}
	return i
}

func joinPath(base, rawBase, suffix, rawSuffix string) (string, string) {
	baseSlash, suffixSlash := strings.HasSuffix(base, "/"), strings.HasPrefix(suffix, "/")
	switch {
	case baseSlash && suffixSlash:
		return base + suffix[1:], rawBase + rawSuffix[escapedOffset(rawSuffix, 1):]
	case !baseSlash && !suffixSlash && suffix != "":
		return base + "/" + suffix, rawBase + "/" + rawSuffix
	default:
		return base + suffix, rawBase + rawSuffix
	}
}

type upgradeWriter struct {
	http.ResponseWriter
	server *Server
}

func (w *upgradeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *upgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	tracked := &tunnelConn{Conn: conn, server: w.server}
	w.server.mu.Lock()
	if w.server.closing {
		w.server.mu.Unlock()
		conn.Close()
		return nil, nil, net.ErrClosed
	}
	w.server.tunnels[tracked] = struct{}{}
	w.server.mu.Unlock()
	return tracked, buffer, nil
}

type tunnelConn struct {
	net.Conn
	server *Server
	once   sync.Once
	err    error
}

func (c *tunnelConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.server.mu.Lock()
		delete(c.server.tunnels, c)
		c.server.mu.Unlock()
	})
	return c.err
}
