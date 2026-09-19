package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
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

type testSite struct {
	server *Server
	url    string
	client *http.Client
}

func newTestSite(t *testing.T, cfg config.Config) *testSite {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.Proxy = nil
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(func() {
		s.CloseTunnels()
		transport.CloseIdleConnections()
		ts.Close()
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &testSite{server: s, url: ts.URL, client: client}
}

func (site *testSite) request(t *testing.T, method, path string, headers http.Header, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, site.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
		req.Header.Del("Host")
	}
	resp, err := site.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp, data
}

func makeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, value := range files {
		writeTestFile(t, root, name, []byte(value))
	}
	return root
}

func writeTestFile(t *testing.T, root, name string, value []byte) string {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, value, 0644); err != nil {
		t.Fatal(err)
	}
	return file
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	u, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d; headers = %v", resp.StatusCode, want, resp.Header)
	}
}

func requireHeader(t *testing.T, resp *http.Response, name, want string) {
	t.Helper()
	if got := resp.Header.Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func requireBody(t *testing.T, got []byte, want string) {
	t.Helper()
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func requireVary(t *testing.T, h http.Header, names ...string) {
	t.Helper()
	for _, name := range names {
		found := false
		for _, line := range h.Values("Vary") {
			for _, token := range strings.Split(line, ",") {
				if strings.EqualFold(strings.TrimSpace(token), name) || strings.TrimSpace(token) == "*" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("Vary = %q, missing %s", h.Values("Vary"), name)
		}
	}
}

func gzipBytes(t *testing.T, value []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := gzip.NewWriter(&buffer)
	if _, err := w.Write(value); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func gunzipBytes(t *testing.T, value []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(value))
	if err != nil {
		t.Fatalf("invalid gzip representation (%d bytes): %v", len(value), err)
	}
	defer r.Close()
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

type observedRequest struct {
	method, path, escapedPath, query, host string
	headers                                http.Header
	body                                   []byte
	err                                    error
}

func observeRequest(r *http.Request) observedRequest {
	body, err := io.ReadAll(r.Body)
	return observedRequest{
		method: r.Method, path: r.URL.Path, escapedPath: r.URL.EscapedPath(),
		query: r.URL.RawQuery, host: r.Host, headers: r.Header.Clone(), body: body, err: err,
	}
}

const testWSKey = "dGhlIHNhbXBsZSBub25jZQ=="

func websocketAccept(key string) string {
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(digest[:])
}

// Validate real WS masking instead of accepting arbitrary TCP echo bytes.
func readTextFrame(r io.Reader, wantMasked bool) (string, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", err
	}
	if header[0] != 0x81 || (header[1]&0x80 != 0) != wantMasked || header[1]&0x7f >= 126 {
		return "", fmt.Errorf("invalid short text frame header %x", header)
	}
	var mask [4]byte
	if wantMasked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return "", err
		}
	}
	payload := make([]byte, int(header[1]&0x7f))
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", err
	}
	if wantMasked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return string(payload), nil
}

func writeTextFrame(w io.Writer, text string, masked bool) error {
	payload := []byte(text)
	if len(payload) >= 126 {
		return fmt.Errorf("test frame is too long")
	}
	frame := []byte{0x81, byte(len(payload))}
	if masked {
		frame[1] |= 0x80
		mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
		frame = append(frame, mask[:]...)
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	frame = append(frame, payload...)
	_, err := w.Write(frame)
	return err
}

func TestNewRejectsUnavailableDirectory(t *testing.T) {
	root := makeFiles(t, map[string]string{"file": "not a directory"})
	for _, path := range []string{filepath.Join(root, "missing"), filepath.Join(root, "file")} {
		s, err := New(config.Config{Directories: []config.Directory{{Mount: "/", Path: root}, {Mount: "/bad", Path: path}}})
		if err == nil {
			s.Close()
			t.Errorf("New accepted unavailable directory %q", path)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s, err := New(config.Config{Directories: []config.Directory{{Mount: "/", Path: t.TempDir()}}})
	if err != nil {
		t.Fatal(err)
	}
	s.CloseTunnels()
	s.CloseTunnels()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
