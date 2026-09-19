package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.qingyu31.com/servd/internal/config"
)

func TestWebSocketHandshakeFramesRewriteAndCloseTunnels(t *testing.T) {
	observed := make(chan observedRequest, 1)
	finished := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observeRequest(r)
		key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
		if err != nil || len(key) != 16 || r.Header.Get("Sec-WebSocket-Version") != "13" ||
			!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerContains(r.Header, "Connection", "upgrade") {
			http.Error(w, "invalid WebSocket handshake", 400)
			finished <- fmt.Errorf("upstream received invalid WebSocket handshake")
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			finished <- err
			return
		}
		_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", websocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		if err == nil {
			err = rw.Flush()
		}
		if err != nil {
			finished <- err
			return
		}
		text, err := readTextFrame(rw, true)
		if err != nil || text != "hello from client 中文" {
			finished <- fmt.Errorf("masked client frame: text=%q, error=%v", text, err)
			return
		}
		if err := writeTextFrame(rw, "hello from upstream", false); err != nil {
			finished <- err
			return
		}
		if err := rw.Flush(); err != nil {
			finished <- err
			return
		}
		var next [1]byte
		_, err = rw.Read(next[:])
		if err == nil {
			finished <- fmt.Errorf("upstream connection remained open after CloseTunnels")
		} else if network, ok := err.(net.Error); ok && network.Timeout() {
			finished <- fmt.Errorf("upstream timed out instead of closing: %w", err)
		} else {
			finished <- nil
		}
	}))
	defer upstream.Close()
	// HTTP and WebSocket routes coexist, but the WS upgrade must select ws.
	httpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ordinary HTTP"))
	}))
	defer httpUpstream.Close()
	site := newTestSite(t, config.Config{
		WebSockets: []config.Proxy{{Mount: "/socket", Target: mustURL(t, "ws"+strings.TrimPrefix(upstream.URL, "http")+"/base%20path?key=target&encoded=%2f")}},
		Proxies:    []config.Proxy{{Mount: "/socket", Target: mustURL(t, httpUpstream.URL)}},
		Hosts:      []string{"allowed.test"},
	})
	resp, body := site.request(t, "GET", "/socket", http.Header{"Host": {"allowed.test"}}, nil)
	requireStatus(t, resp, 200)
	requireBody(t, body, "ordinary HTTP")

	conn, err := net.DialTimeout("tcp", mustURL(t, site.url).Host, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "GET /socket/room%%20%%E4%%B8%%AD%%E6%%96%%87/a%%2Fb?key=input&encoded=%%2F+ HTTP/1.1\r\nHost: ALLOWED.TEST.:8080\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nX-Forwarded-For: 203.0.113.99\r\nX-Forwarded-Proto: https\r\nForwarded: for=evil\r\n\r\n", testWSKey)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	handshake, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, handshake, 101)
	requireHeader(t, handshake, "Sec-WebSocket-Accept", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=")
	if !headerContains(handshake.Header, "Connection", "upgrade") || !strings.EqualFold(handshake.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("invalid upgrade response: %v", handshake.Header)
	}
	got := <-observed
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.path != "/base path/room 中文/a/b" || got.escapedPath != "/base%20path/room%20%E4%B8%AD%E6%96%87/a%2Fb" {
		t.Errorf("WS rewrite: path=%q, escaped=%q", got.path, got.escapedPath)
	}
	if got.query != "key=target&encoded=%2f&key=input&encoded=%2F+" {
		t.Errorf("WS raw query changed: %q", got.query)
	}
	if got.host != mustURL(t, upstream.URL).Host || got.headers.Get("Forwarded") != "" || got.headers.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("WS host/forwarding headers: host=%q, headers=%v", got.host, got.headers)
	}
	if ip := net.ParseIP(got.headers.Get("X-Forwarded-For")); ip == nil || !ip.IsLoopback() {
		t.Errorf("WS spoofed X-Forwarded-For survived: %q", got.headers.Get("X-Forwarded-For"))
	}
	if err := writeTextFrame(conn, "hello from client 中文", true); err != nil {
		t.Fatal(err)
	}
	text, err := readTextFrame(reader, false)
	if err != nil || text != "hello from upstream" {
		t.Fatalf("unmasked server frame: text=%q, error=%v", text, err)
	}
	site.server.CloseTunnels()
	site.server.CloseTunnels()
	var next [1]byte
	if _, err := reader.Read(next[:]); err == nil {
		t.Error("client connection remained open after CloseTunnels")
	} else if network, ok := err.(net.Error); ok && network.Timeout() {
		t.Errorf("client timed out instead of closing: %v", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("upstream tunnel did not terminate")
	}
}

func TestWSSProxyPreservesTLSVerification(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", websocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		if err := rw.Flush(); err != nil {
			return
		}
		text, err := readTextFrame(rw, true)
		if err == nil {
			writeTextFrame(rw, text, false)
			rw.Flush()
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%t", trusted), func(t *testing.T) {
			site := newTestSite(t, config.Config{WebSockets: []config.Proxy{{
				Mount: "/socket", Target: mustURL(t, "wss"+strings.TrimPrefix(upstream.URL, "https")+"/secure"),
			}}})
			if trusted {
				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				site.server.transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.url+"/socket", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = http.Header{
				"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
				"Sec-Websocket-Key": {testWSKey}, "Sec-Websocket-Version": {"13"},
			}
			// Client.Timeout would hide the upgraded body's io.Writer interface.
			resp, err := site.client.Transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if !trusted {
				requireStatus(t, resp, http.StatusBadGateway)
				return
			}
			requireStatus(t, resp, http.StatusSwitchingProtocols)
			stream, ok := resp.Body.(io.ReadWriteCloser)
			if !ok {
				t.Fatal("upgraded response is not bidirectional")
			}
			if err := writeTextFrame(stream, "secure echo", true); err != nil {
				t.Fatal(err)
			}
			text, err := readTextFrame(stream, false)
			if err != nil || text != "secure echo" {
				t.Fatalf("WSS echo = %q, error = %v", text, err)
			}
		})
	}
}
