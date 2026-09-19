package server

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.qingyu31.com/servd/internal/config"
)

func TestHTTPProxyRewritesEscapedPathAndPreservesRawQuery(t *testing.T) {
	observed := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observeRequest(r)
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	for _, tc := range []struct {
		name, mount, targetPath, input, wantPath, wantEscaped string
	}{
		{"simple", "/api", "/base", "/api/item", "/base/item", "/base/item"},
		{"exact mount", "/api", "/base", "/api", "/base", "/base"},
		{"trailing slash", "/api", "/base/", "/api/", "/base/", "/base/"},
		{"one joining slash", "/api", "/base/", "/api/item", "/base/item", "/base/item"},
		{"root mount", "/", "/base", "/item", "/base/item", "/base/item"},
		{"no target path", "/api", "", "/api/item", "/item", "/item"},
		{"spaces", "/api", "/base%20path", "/api/hello%20world", "/base path/hello world", "/base%20path/hello%20world"},
		{"Chinese", "/api", "/%E5%9F%BA", "/api/%E4%B8%AD%E6%96%87", "/基/中文", "/%E5%9F%BA/%E4%B8%AD%E6%96%87"},
		{"encoded slash", "/api", "/base%2Fpart", "/api/a%2Fb", "/base/part/a/b", "/base%2Fpart/a%2Fb"},
		{"lowercase encoding", "/api", "/base", "/api/a%2fb", "/base/a/b", "/base/a%2fb"},
		{"escaped mount bytes", "/api", "/base", "/%61pi/a%2Fb", "/base/a/b", "/base/a%2Fb"},
		{"Unicode mount", "/中文", "/base", "/%E4%B8%AD%E6%96%87/hello%20world", "/base/hello world", "/base/hello%20world"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const targetQuery = "key=target&key=again&encoded=%2f+%20"
			const inputQuery = "key=input&key=last&encoded=%2F%20+&bare&empty="
			site := newTestSite(t, config.Config{Proxies: []config.Proxy{{Mount: tc.mount, Target: mustURL(t, upstream.URL+tc.targetPath+"?"+targetQuery)}}})
			resp, _ := site.request(t, "GET", tc.input+"?"+inputQuery, nil, nil)
			requireStatus(t, resp, 204)
			got := <-observed
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.path != tc.wantPath || got.escapedPath != tc.wantEscaped {
				t.Errorf("path/escaped = %q / %q, want %q / %q", got.path, got.escapedPath, tc.wantPath, tc.wantEscaped)
			}
			if got.query != targetQuery+"&"+inputQuery {
				t.Errorf("raw query = %q, want exact concatenation", got.query)
			}
			if value := got.headers.Get("Accept-Encoding"); value != "" {
				t.Errorf("proxy injected Accept-Encoding %q into a request without it", value)
			}
		})
	}
}

func TestHTTPProxyMethodBodyAndForwardingHeaders(t *testing.T) {
	observed := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observeRequest(r)
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	site := newTestSite(t, config.Config{Proxies: []config.Proxy{{Mount: "/api", Target: mustURL(t, upstream.URL)}}})
	payload := gzipBytes(t, []byte("raw body: 中文 \x00 \xff &x=%2F"))
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			resp, body := site.request(t, method, "/api/submit", http.Header{
				"Host": {"client.example:8123"}, "Content-Encoding": {"gzip"}, "Content-Type": {"application/octet-stream"},
				"Accept-Encoding": {"gzip, br"}, "X-Custom": {"preserve-me"},
				"Forwarded": {"for=evil;host=evil;proto=https"}, "X-Forwarded": {"evil"},
				"X-Forwarded-For": {"203.0.113.9", "203.0.113.10"}, "X-Forwarded-Host": {"evil.test"},
				"X-Forwarded-Proto": {"https"}, "X-Forwarded-Port": {"443"}, "X-Forwarded-Prefix": {"/evil"},
				"Connection": {"X-Remove"}, "X-Remove": {"hop-by-hop"},
			}, payload)
			requireStatus(t, resp, 204)
			requireBody(t, body, "")
			got := <-observed
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.method != method || !bytes.Equal(got.body, payload) {
				t.Errorf("method/body changed: method=%q, body=%x", got.method, got.body)
			}
			if got.host != mustURL(t, upstream.URL).Host {
				t.Errorf("upstream Host = %q, want target authority", got.host)
			}
			for name, want := range map[string]string{
				"Content-Encoding": "gzip", "Content-Type": "application/octet-stream", "Accept-Encoding": "gzip, br",
				"X-Custom": "preserve-me", "X-Forwarded-Host": "client.example:8123", "X-Forwarded-Proto": "http",
				"Forwarded": "", "X-Forwarded": "", "X-Forwarded-Port": "", "X-Forwarded-Prefix": "", "X-Remove": "",
			} {
				if got.headers.Get(name) != want {
					t.Errorf("upstream %s = %q, want %q", name, got.headers.Get(name), want)
				}
			}
			if ip := net.ParseIP(got.headers.Get("X-Forwarded-For")); ip == nil || !ip.IsLoopback() {
				t.Errorf("X-Forwarded-For must contain only actual loopback peer: %q", got.headers.Values("X-Forwarded-For"))
			}
		})
	}
}

func TestHTTPProxyPreservesResponseStatusEncodingAndCache(t *testing.T) {
	payload := gzipBytes(t, []byte("upstream's exact encoded response"))
	for _, code := range []int{200, 400, 404, 418, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Type", "application/custom")
				w.Header().Set("Cache-Control", "public, max-age=3600")
				w.Header().Set("ETag", `"upstream-etag"`)
				w.Header().Set("Vary", "Accept-Language")
				w.Header().Add("Set-Cookie", "one=1")
				w.Header().Add("Set-Cookie", "two=2")
				w.WriteHeader(code)
				w.Write(payload)
			}))
			defer upstream.Close()
			root := makeFiles(t, map[string]string{"index.html": "SPA must not replace errors"})
			site := newTestSite(t, config.Config{
				Proxies:     []config.Proxy{{Mount: "/api", Target: mustURL(t, upstream.URL)}},
				Directories: []config.Directory{{Mount: "/", Path: root, SPA: true}, {Mount: "/api", Path: root, SPA: true}},
			})
			resp, body := site.request(t, "GET", "/api/navigation", http.Header{"Accept": {"text/html"}, "Accept-Encoding": {"identity"}}, nil)
			requireStatus(t, resp, code)
			if !bytes.Equal(body, payload) {
				t.Errorf("encoded upstream bytes changed: got %x, want %x", body, payload)
			}
			requireHeader(t, resp, "Content-Encoding", "gzip")
			requireHeader(t, resp, "Content-Type", "application/custom")
			requireHeader(t, resp, "Cache-Control", "public, max-age=3600")
			requireHeader(t, resp, "ETag", `"upstream-etag"`)
			requireVary(t, resp.Header, "Accept-Language")
			if len(resp.Header.Values("Set-Cookie")) != 2 {
				t.Error("multiple Set-Cookie headers were not preserved")
			}
		})
	}
}

func TestHTTPProxyDoesNotFollowRedirectOrAutoCompress(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere?x=%2F")
		w.WriteHeader(307)
		w.Write([]byte("redirect body"))
	}))
	defer upstream.Close()
	site := newTestSite(t, config.Config{Proxies: []config.Proxy{{Mount: "/", Target: mustURL(t, upstream.URL)}}})
	resp, body := site.request(t, "GET", "/", http.Header{"Accept-Encoding": {"gzip"}}, nil)
	requireStatus(t, resp, 307)
	requireHeader(t, resp, "Location", "/elsewhere?x=%2F")
	requireHeader(t, resp, "Content-Encoding", "")
	requireHeader(t, resp, "Cache-Control", "")
	requireBody(t, body, "redirect body")
}

func TestHTTPProxyFailureIs502NotSPA(t *testing.T) {
	// Keep the port owned but close accepted sockets: deterministic failure,
	// without assuming a just-closed ephemeral port stays unallocated.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	root := makeFiles(t, map[string]string{"index.html": "SPA must not be used"})
	site := newTestSite(t, config.Config{
		Proxies:     []config.Proxy{{Mount: "/api", Target: mustURL(t, "http://"+listener.Addr().String())}},
		Directories: []config.Directory{{Mount: "/", Path: root, SPA: true}, {Mount: "/api", Path: root, SPA: true}},
	})
	resp, body := site.request(t, "GET", "/api/route", http.Header{"Accept": {"text/html"}}, nil)
	requireStatus(t, resp, 502)
	requireHeader(t, resp, "Cache-Control", "no-store")
	if strings.Contains(string(body), "SPA") {
		t.Error("proxy failure fell back to SPA")
	}
}

func TestHTTPProxyRejectsUnsafePathsAndMalformedQueries(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	site := newTestSite(t, config.Config{Proxies: []config.Proxy{{Mount: "/", Target: mustURL(t, upstream.URL)}}})
	for _, path := range []string{"/a/%2e%2e/private", "/a%5Cb", "/null%00", "/ok?bad=%zz"} {
		resp, _ := site.request(t, "GET", path, nil, nil)
		requireStatus(t, resp, 400)
		requireHeader(t, resp, "Cache-Control", "no-store")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}
