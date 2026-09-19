package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.qingyu31.com/servd/internal/config"
)

func TestLongestSegmentMountAndNoRootFallback(t *testing.T) {
	root := makeFiles(t, map[string]string{"index.html": "root SPA", "app/root-only.txt": "must not leak"})
	app := makeFiles(t, map[string]string{"index.html": "app index", "item.txt": "app item"})
	deep := makeFiles(t, map[string]string{"item.txt": "deep item"})
	site := newTestSite(t, config.Config{Directories: []config.Directory{
		{Mount: "/", Path: root, SPA: true},
		{Mount: "/app", Path: app},
		{Mount: "/app/deep", Path: deep},
	}})
	for _, tc := range []struct {
		path string
		code int
		body string
	}{
		{"/app/item.txt", 200, "app item"},
		{"/app/deep/item.txt", 200, "deep item"},
		{"/application", 200, "root SPA"},
		{"/app/deeply", 404, ""},
		{"/app/missing", 404, ""},
		{"/app/deep/missing", 404, ""},
		{"/app/root-only.txt", 404, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := site.request(t, "GET", tc.path, http.Header{"Accept": {"text/html"}}, nil)
			requireStatus(t, resp, tc.code)
			if tc.code == 200 {
				requireBody(t, body, tc.body)
			} else {
				requireHeader(t, resp, "Cache-Control", "no-store")
			}
		})
	}
}

func TestProxyPrecedesFilesAtSameMount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("proxy wins"))
	}))
	defer upstream.Close()
	root := makeFiles(t, map[string]string{"index.html": "SPA", "item": "file"})
	site := newTestSite(t, config.Config{
		Directories: []config.Directory{{Mount: "/api", Path: root, SPA: true}},
		Proxies:     []config.Proxy{{Mount: "/api", Target: mustURL(t, upstream.URL)}},
	})
	resp, body := site.request(t, "GET", "/api/item", http.Header{"Accept": {"text/html"}}, nil)
	requireStatus(t, resp, 200)
	requireBody(t, body, "proxy wins")
}

func TestHostAllowlistAppliesBeforeEveryRoute(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	root := makeFiles(t, map[string]string{"file.txt": "local"})
	cfg, err := config.Parse([]string{
		"--static=" + root,
		"--proxy=/api=" + upstream.URL,
		"--ws=/socket=ws" + upstream.URL[len("http"):],
		"--host=EXAMPLE.Test.", "--host=[::1]", "--host=127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	site := newTestSite(t, cfg)
	for _, host := range []string{"example.test", "EXAMPLE.TEST.:54321", "[0:0:0:0:0:0:0:1]:80", "127.0.0.1:8080"} {
		t.Run("allowed/"+host, func(t *testing.T) {
			resp, body := site.request(t, "GET", "/file.txt", http.Header{"Host": {host}}, nil)
			requireStatus(t, resp, 200)
			requireBody(t, body, "local")
		})
	}
	for _, path := range []string{"/file.txt", "/api/item", "/socket"} {
		t.Run("denied"+path, func(t *testing.T) {
			h := http.Header{"Host": {"attacker.test"}}
			if path == "/socket" {
				h.Set("Connection", "Upgrade")
				h.Set("Upgrade", "websocket")
				h.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
				h.Set("Sec-WebSocket-Version", "13")
			}
			resp, _ := site.request(t, "GET", path, h, nil)
			requireStatus(t, resp, 403)
			requireHeader(t, resp, "Cache-Control", "no-store")
		})
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("denied proxy/WS requests reached upstream %d times", got)
	}
	resp, _ := site.request(t, "GET", "/api/allowed", http.Header{"Host": {"ExAmPlE.TeSt:1234"}}, nil)
	requireStatus(t, resp, 204)
	if got := calls.Load(); got != 1 {
		t.Errorf("allowed proxy request: calls = %d, want 1", got)
	}
	resp, _ = site.request(t, "GET", "/file.txt", http.Header{"Host": {"example.test:invalid"}}, nil)
	requireStatus(t, resp, 400)
}

func TestWebSocketOnlyRouteAndUpgradeSelection(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(418)
	}))
	defer upstream.Close()
	site := newTestSite(t, config.Config{
		WebSockets: []config.Proxy{{Mount: "/socket", Target: mustURL(t, upstream.URL)}},
		Directories: []config.Directory{{Mount: "/", Path: makeFiles(t, map[string]string{
			"index.html": "must not swallow WS",
		}), SPA: true}},
	})
	for _, tc := range []struct {
		path, upgrade, connection string
		status                    int
	}{
		{"/socket", "", "", 426},
		{"/socket", "h2c", "upgrade", 400},
		{"/socket", "websocket", "", 400},
		{"/socket", "", "upgrade", 400},
		{"/other", "websocket", "Upgrade", 404},
	} {
		t.Run(tc.path+"/"+tc.upgrade+"/"+tc.connection, func(t *testing.T) {
			resp, _ := site.request(t, "GET", tc.path, http.Header{
				"Upgrade": {tc.upgrade}, "Connection": {tc.connection}, "Accept": {"text/html"},
			}, nil)
			requireStatus(t, resp, tc.status)
			requireHeader(t, resp, "Cache-Control", "no-store")
			if tc.status == 426 {
				requireHeader(t, resp, "Upgrade", "websocket")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid upgrade reached upstream")
	}
}
