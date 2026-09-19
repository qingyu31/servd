package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.qingyu31.com/servd/internal/config"
)

func TestCORSOrigins(t *testing.T) {
	root := makeFiles(t, map[string]string{"file.txt": "text"})
	for _, tc := range []struct {
		name    string
		origins []string
		origin  string
		allow   string
		vary    bool
	}{
		{"disabled", nil, "https://one.test", "", false},
		{"first exact", []string{"https://one.test", "https://two.test:8443"}, "https://one.test", "https://one.test", true},
		{"second exact", []string{"https://one.test", "https://two.test:8443"}, "https://two.test:8443", "https://two.test:8443", true},
		{"wrong port", []string{"https://two.test:8443"}, "https://two.test", "", true},
		{"wrong scheme", []string{"https://one.test"}, "http://one.test", "", true},
		{"suffix attack", []string{"https://one.test"}, "https://one.test.attacker.test", "", true},
		{"unlisted", []string{"https://one.test"}, "https://other.test", "", true},
		{"absent", []string{"https://one.test"}, "", "", true},
		{"multiple origins", []string{"https://one.test"}, "https://one.test https://two.test", "", true},
		{"wildcard", []string{"*"}, "https://any.test:1234", "*", false},
		{"wildcard without origin", []string{"*"}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}, Origins: tc.origins})
			resp, body := site.request(t, "GET", "/file.txt", http.Header{"Origin": {tc.origin}}, nil)
			requireStatus(t, resp, 200)
			requireBody(t, body, "text")
			requireHeader(t, resp, "Access-Control-Allow-Origin", tc.allow)
			requireHeader(t, resp, "Access-Control-Allow-Credentials", "")
			if tc.vary {
				requireVary(t, resp.Header, "Origin", "Accept-Encoding")
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	root := makeFiles(t, map[string]string{"file.txt": "text"})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}, Origins: []string{"https://one.test"}})
	for _, tc := range []struct {
		name, origin, method string
		headers              []string
		status               int
	}{
		{"GET", "https://one.test", "GET", []string{"X-Token, Content-Type", "X-Other"}, 204},
		{"HEAD", "https://one.test", "HEAD", nil, 204},
		{"wrong origin", "https://other.test", "GET", nil, 403},
		{"static POST", "https://one.test", "POST", nil, 405},
		{"invalid method", "https://one.test", "GET POST", nil, 400},
		{"invalid header", "https://one.test", "GET", []string{"X-Token, bad header"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{
				"Origin": {tc.origin}, "Access-Control-Request-Method": {tc.method},
				"Access-Control-Request-Headers": tc.headers,
			}
			resp, body := site.request(t, "OPTIONS", "/file.txt", h, nil)
			requireStatus(t, resp, tc.status)
			requireHeader(t, resp, "Cache-Control", "no-store")
			requireHeader(t, resp, "Access-Control-Allow-Credentials", "")
			requireVary(t, resp.Header, "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")
			if tc.status == 204 {
				requireBody(t, body, "")
				requireHeader(t, resp, "Access-Control-Allow-Origin", tc.origin)
				requireHeader(t, resp, "Access-Control-Allow-Methods", tc.method)
				if len(tc.headers) > 0 {
					for _, name := range []string{"X-Token", "Content-Type", "X-Other"} {
						if !headerContains(resp.Header, "Access-Control-Allow-Headers", name) {
							t.Errorf("missing allowed header %q", name)
						}
					}
				}
			}
		})
	}
}

func TestCORSProxyConflictAndPreflight(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "https://upstream.test")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "DELETE")
		w.Header().Set("Access-Control-Expose-Headers", "Secret")
		w.Header().Set("Vary", "User-Agent, Accept-Encoding")
		w.Header().Set("Cache-Control", "public, max-age=123")
		w.Write([]byte("upstream"))
	}))
	defer upstream.Close()
	for _, tc := range []struct {
		name    string
		origins []string
		origin  string
		allow   string
	}{
		{"allowed", []string{"https://one.test"}, "https://one.test", "https://one.test"},
		{"denied", []string{"https://one.test"}, "https://other.test", ""},
		{"wildcard", []string{"*"}, "https://other.test", "*"},
		{"disabled", nil, "https://one.test", "https://upstream.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newTestSite(t, config.Config{Proxies: []config.Proxy{{Mount: "/", Target: mustURL(t, upstream.URL)}}, Origins: tc.origins})
			resp, body := site.request(t, "GET", "/anything", http.Header{"Origin": {tc.origin}}, nil)
			requireStatus(t, resp, 200)
			requireBody(t, body, "upstream")
			requireHeader(t, resp, "Access-Control-Allow-Origin", tc.allow)
			requireHeader(t, resp, "Cache-Control", "public, max-age=123")
			requireVary(t, resp.Header, "User-Agent", "Accept-Encoding")
			if len(resp.Header.Values("Access-Control-Allow-Origin")) > 1 {
				t.Errorf("conflicting allow origins: %v", resp.Header.Values("Access-Control-Allow-Origin"))
			}
			if len(tc.origins) != 0 {
				for _, name := range []string{"Access-Control-Allow-Credentials", "Access-Control-Allow-Methods", "Access-Control-Expose-Headers"} {
					requireHeader(t, resp, name, "")
				}
				if tc.origins[0] != "*" {
					requireVary(t, resp.Header, "Origin")
				}
			} else {
				requireHeader(t, resp, "Access-Control-Allow-Credentials", "true")
			}
			before := calls.Load()
			resp, body = site.request(t, "OPTIONS", "/anything", http.Header{
				"Origin": {tc.origin}, "Access-Control-Request-Method": {"PATCH"},
				"Access-Control-Request-Headers": {"Content-Type, X-Token"},
			}, nil)
			if len(tc.origins) == 0 {
				requireStatus(t, resp, 200)
				requireBody(t, body, "upstream")
				if calls.Load() != before+1 {
					t.Error("CORS-disabled OPTIONS was not proxied")
				}
			} else {
				status := 204
				if tc.allow == "" {
					status = 403
				}
				requireStatus(t, resp, status)
				if calls.Load() != before {
					t.Error("local preflight reached upstream")
				}
				if status == 204 {
					requireBody(t, body, "")
					requireHeader(t, resp, "Access-Control-Allow-Methods", "PATCH")
				}
			}
		})
	}
}

func TestCORSIsAppliedToLocalErrors(t *testing.T) {
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: t.TempDir()}}, Origins: []string{"https://one.test"}})
	resp, _ := site.request(t, "GET", "/missing", http.Header{"Origin": {"https://one.test"}}, nil)
	requireStatus(t, resp, 404)
	requireHeader(t, resp, "Access-Control-Allow-Origin", "https://one.test")
	requireVary(t, resp.Header, "Origin")
}
