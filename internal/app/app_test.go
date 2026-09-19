package app

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.qingyu31.com/servd/internal/config"
)

// writeTestCert generates a self-signed certificate valid for localhost and
// 127.0.0.1 and stores it as a PEM pair on disk.
func writeTestCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "servd test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// tlsTestSite runs app.Run with a TLS configuration on a free port and
// returns a client that trusts the generated certificate.
type tlsTestSite struct {
	t          *testing.T
	port       int
	client     *http.Client
	tlsClient  *tls.Config
	cancel     context.CancelFunc
	done       chan error
	stdout     strings.Builder
	certPath   string
	keyPath    string
	staticPath string
}

func newTLSTestSite(t *testing.T, cfg config.Config) *tlsTestSite {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := writeTestCert(t, t.TempDir())
	cfg.Port = port
	cfg.TLSCert, cfg.TLSKey = certPath, keyPath

	pool := x509.NewCertPool()
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("test certificate could not be added to the pool")
	}
	site := &tlsTestSite{
		t: t, port: port, certPath: certPath, keyPath: keyPath,
		tlsClient: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{"http/1.1"}},
		done:      make(chan error, 1),
	}
	site.client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: site.tlsClient.Clone()},
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-site.done:
		case <-time.After(10 * time.Second):
			t.Error("app.Run did not return after context cancellation")
		}
	})
	go func() {
		site.done <- Run(ctx, cfg, &site.stdout, io.Discard, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-site.done:
		t.Fatalf("app.Run exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ready")
	}
	return site
}

func (s *tlsTestSite) get(path string, header http.Header) (*http.Response, string, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d%s", s.port, path), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = header
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, string(body), err
}

func TestRunHTTPSServesStaticContent(t *testing.T) {
	static := t.TempDir()
	if err := os.WriteFile(filepath.Join(static, "index.html"), []byte("hello tls"), 0o644); err != nil {
		t.Fatal(err)
	}
	site := newTLSTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: static}}})

	resp, body, err := site.get("/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || body != "hello tls" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("unexpected TLS state: %+v", resp.TLS)
	}
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("negotiated protocol = %q", resp.Proto)
	}
	if !strings.Contains(site.stdout.String(), "https") {
		t.Fatalf("startup output missing https marker: %q", site.stdout.String())
	}

	// A plaintext request against the TLS port must not serve site content.
	plain := &http.Client{Timeout: 5 * time.Second}
	httpResp, err := plain.Get(fmt.Sprintf("http://127.0.0.1:%d/", site.port))
	if err == nil {
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if httpResp.StatusCode == 200 && strings.Contains(string(body), "hello tls") {
			t.Fatal("plaintext request served protected content")
		}
	}
}

func TestRunTLSHandshakeLimits(t *testing.T) {
	static := t.TempDir()
	site := newTLSTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: static}}})

	// TLS 1.1 is refused.
	old := site.tlsClient.Clone()
	old.MaxVersion = tls.VersionTLS11
	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", site.port), old)
	if err == nil {
		conn.Close()
		t.Fatal("TLS 1.1 handshake unexpectedly succeeded")
	}

	// A client offering only h2 cannot negotiate with our http/1.1-only ALPN.
	h2Only := site.tlsClient.Clone()
	h2Only.NextProtos = []string{"h2"}
	conn, err = tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", site.port), h2Only)
	if err == nil {
		proto := conn.ConnectionState().NegotiatedProtocol
		conn.Close()
		if proto == "h2" {
			t.Fatal("server negotiated h2")
		}
	}

	// An idle connection that never completes the handshake is dropped by the
	// ReadHeaderTimeout-derived deadline instead of being held forever.
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", site.port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	deadline := time.Now().Add(15 * time.Second)
	if err := raw.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	// Send only a partial ClientHello: one record byte of a bogus content type.
	if _, err := raw.Write([]byte{0x16}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Read(make([]byte, 1)); err == nil {
		t.Fatal("server kept a stalled handshake connection open")
	}
}

func TestRunTLSFailurePaths(t *testing.T) {
	static := t.TempDir()
	tests := []struct {
		name    string
		mutate  func(t *testing.T, cert, key string)
		certArg func(cert string) string
	}{
		{
			name:    "missing cert",
			certArg: func(cert string) string { return cert + ".missing" },
		},
		{
			name: "invalid PEM",
			mutate: func(t *testing.T, cert, key string) {
				if err := os.WriteFile(cert, []byte("not a pem"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mismatched key",
			mutate: func(t *testing.T, cert, key string) {
				_, replacement := writeTestCert(t, t.TempDir())
				data, err := os.ReadFile(replacement)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(key, data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			certPath, keyPath := writeTestCert(t, t.TempDir())
			if tc.mutate != nil {
				tc.mutate(t, certPath, keyPath)
			}
			cert := certPath
			if tc.certArg != nil {
				cert = tc.certArg(certPath)
			}
			ready := make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, config.Config{
					Port: 80, Directories: []config.Directory{{Mount: "/", Path: static}},
					TLSCert: cert, TLSKey: keyPath,
				}, io.Discard, io.Discard, func() { close(ready) })
			}()
			select {
			case <-ready:
				t.Fatal("ready reported despite certificate failure")
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "load TLS certificate") {
					t.Fatalf("unexpected error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run hung on a broken certificate")
			}
		})
	}
}

func TestRunTLSProxyForwardedProto(t *testing.T) {
	observed := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case observed <- r.Header.Get("X-Forwarded-Proto"):
		default:
		}
		w.Write([]byte("upstream ok"))
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	site := newTLSTestSite(t, config.Config{
		Proxies: []config.Proxy{{Mount: "/api", Target: target}},
	})
	resp, body, err := site.get("/api/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || body != "upstream ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	select {
	case proto := <-observed:
		if proto != "https" {
			t.Fatalf("X-Forwarded-Proto = %q, want https", proto)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed the proxied request")
	}
}

func TestRunTLSWebSocketTunnel(t *testing.T) {
	handshakes := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		accept := websocketAcceptValue(key)
		_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err != nil {
			t.Error(err)
			return
		}
		rw.Flush()
		select {
		case handshakes <- struct{}{}:
		default:
		}
		// Echo one masked text frame back unmasked.
		header := make([]byte, 2)
		if _, err := io.ReadFull(rw, header); err != nil {
			t.Error(err)
			return
		}
		length := int(header[1] & 0x7f)
		masked := make([]byte, length+4)
		if _, err := io.ReadFull(rw, masked); err != nil {
			t.Error(err)
			return
		}
		mask, payload := masked[:4], masked[4:]
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		if _, err := rw.Write(append([]byte{0x81, byte(len(payload))}, payload...)); err != nil {
			t.Error(err)
			return
		}
		rw.Flush()
		// Hold the tunnel open until the client side closes it.
		_, _ = rw.Read(make([]byte, 1))
	}))
	defer upstream.Close()
	target, err := url.Parse("ws" + strings.TrimPrefix(upstream.URL, "http") + "/socket")
	if err != nil {
		t.Fatal(err)
	}
	site := newTLSTestSite(t, config.Config{
		WebSockets: []config.Proxy{{Mount: "/ws", Target: target}},
	})

	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", site.port), site.tlsClient.Clone())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	_, err = fmt.Fprintf(conn, "GET /ws/room HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", site.port, key)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 {
		t.Fatalf("wss handshake status = %d", response.StatusCode)
	}
	if !conn.ConnectionState().HandshakeComplete {
		t.Fatal("wss handshake response carries no TLS state")
	}
	select {
	case <-handshakes:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the tunneled handshake")
	}

	payload := []byte("hello wss")
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 2+len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatal(err)
	}
	if string(echo[2:]) != "hello wss" {
		t.Fatalf("echo = %q", echo[2:])
	}
}

func websocketAcceptValue(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func TestRunPlainHTTPUnchanged(t *testing.T) {
	static := t.TempDir()
	if err := os.WriteFile(filepath.Join(static, "index.html"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- Run(ctx, config.Config{
			Port: port, Directories: []config.Directory{{Mount: "/", Path: static}},
		}, io.Discard, io.Discard, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("ready timeout")
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "plain" {
		t.Fatalf("plain HTTP regression: %d %q", resp.StatusCode, body)
	}
	if resp.TLS != nil {
		t.Fatal("plain listener unexpectedly negotiated TLS")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down")
	}
}
