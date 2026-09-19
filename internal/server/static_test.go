package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"go.qingyu31.com/servd/internal/config"
)

func TestDirectoryOverlayFilesBeforeSPAFallback(t *testing.T) {
	first := makeFiles(t, map[string]string{
		"index.html": "first SPA", "shared.txt": "first file", "folder/index.html": "first folder",
	})
	second := makeFiles(t, map[string]string{
		"index.html": "second SPA", "shared.txt": "second file", "real": "later real file",
		"folder/index.html": "second folder", "later/index.html": "later directory index",
	})
	for _, reverse := range []bool{false, true} {
		name := "declared"
		dirs := []config.Directory{{Mount: "/", Path: first, SPA: true}, {Mount: "/", Path: second, SPA: true}}
		wantShared, wantSPA, wantFolder := "first file", "first SPA", "first folder"
		if reverse {
			name = "reversed"
			dirs[0], dirs[1] = dirs[1], dirs[0]
			wantShared, wantSPA, wantFolder = "second file", "second SPA", "second folder"
		}
		t.Run(name, func(t *testing.T) {
			site := newTestSite(t, config.Config{Directories: dirs})
			for _, tc := range []struct{ path, body string }{
				{"/shared.txt", wantShared}, {"/missing", wantSPA}, {"/real", "later real file"},
				{"/folder/", wantFolder}, {"/later/", "later directory index"},
			} {
				t.Run(tc.path, func(t *testing.T) {
					resp, body := site.request(t, "GET", tc.path, http.Header{"Accept": {"text/html"}}, nil)
					requireStatus(t, resp, 200)
					requireBody(t, body, tc.body)
				})
			}
		})
	}
}

func TestSPAFallbackRequiresExplicitHTMLAndNoExtension(t *testing.T) {
	root := makeFiles(t, map[string]string{"index.html": "SPA shell", "asset.txt": "asset"})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root, SPA: true}}})
	for _, tc := range []struct {
		name, path string
		accept     []string
		status     int
	}{
		{"no Accept", "/route", nil, 404},
		{"wildcard", "/route", []string{"*/*"}, 404},
		{"text wildcard", "/route", []string{"text/*"}, 404},
		{"JSON", "/route", []string{"application/json"}, 404},
		{"explicit HTML", "/route", []string{"text/html"}, 200},
		{"mixed HTML", "/route", []string{"application/json, text/html;q=0.5"}, 200},
		{"multiline", "/route", []string{"application/json", "text/html"}, 200},
		{"excluded HTML", "/route", []string{"text/html;q=0, */*;q=1"}, 404},
		{"invalid weight", "/route", []string{"text/html;q=banana"}, 404},
		{"extension", "/missing.js", []string{"text/html"}, 404},
		{"extension trailing slash", "/missing.js/", []string{"text/html"}, 404},
		{"nested route", "/some/deep/route?x=.js", []string{"text/html"}, 200},
		{"real asset ignores Accept", "/asset.txt", []string{"application/json"}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := site.request(t, "GET", tc.path, http.Header{"Accept": tc.accept}, nil)
			requireStatus(t, resp, tc.status)
			if tc.status == 200 && tc.path != "/asset.txt" {
				requireBody(t, body, "SPA shell")
				requireHeader(t, resp, "Cache-Control", "no-store")
			}
		})
	}
	for _, path := range []string{"/", "/index.html", "/deep/link"} {
		resp, body := site.request(t, "HEAD", path, http.Header{"Accept": {"text/html"}}, nil)
		requireStatus(t, resp, 200)
		requireBody(t, body, "")
		requireHeader(t, resp, "Cache-Control", "no-store")
	}
	resp, _ := site.request(t, "GET", "/asset.txt", nil, nil)
	requireHeader(t, resp, "Cache-Control", "no-cache")
}

func TestDirectoryRedirectPreservesQueryAndEscaping(t *testing.T) {
	root := makeFiles(t, map[string]string{
		"index.html": "root", "folder/index.html": "folder", "space dir/index.html": "space",
	})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/app", Path: root}}})
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{"/app", "/app/folder", "/app/space%20dir"} {
			t.Run(method+path, func(t *testing.T) {
				query := "?x=1&x=2&encoded=%2F+a"
				resp, body := site.request(t, method, path+query, nil, nil)
				requireStatus(t, resp, 308)
				requireHeader(t, resp, "Location", path+"/"+query)
				requireHeader(t, resp, "Cache-Control", "no-store")
				if method == "HEAD" {
					requireBody(t, body, "")
				}
			})
		}
	}
	resp, body := site.request(t, "GET", "/app/folder/", nil, nil)
	requireStatus(t, resp, 200)
	requireBody(t, body, "folder")
}

func TestStaticErrorsAndNoDirectoryListing(t *testing.T) {
	root := makeFiles(t, map[string]string{"file.txt": "content", "unlisted/private.txt": "private"})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/missing", 404}, {"HEAD", "/missing", 404},
		{"GET", "/unlisted/", 404}, {"POST", "/file.txt", 405},
		{"PUT", "/file.txt", 405}, {"DELETE", "/missing", 405},
		{"OPTIONS", "/file.txt", 405},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			resp, body := site.request(t, tc.method, tc.path, nil, nil)
			requireStatus(t, resp, tc.status)
			requireHeader(t, resp, "Cache-Control", "no-store")
			if tc.status == 405 {
				for _, method := range []string{"GET", "HEAD"} {
					if !headerContains(resp.Header, "Allow", method) {
						t.Errorf("Allow = %q, missing %s", resp.Header.Values("Allow"), method)
					}
				}
			}
			if tc.method == "HEAD" {
				requireBody(t, body, "")
			}
		})
	}
}

func TestUnsafePathsAreRejectedWithoutCleaningOrSPAFallback(t *testing.T) {
	root := makeFiles(t, map[string]string{"index.html": "SPA must not leak", "secret.txt": "secret"})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root, SPA: true}}})
	for _, path := range []string{
		"/../secret.txt", "/./secret.txt", "/a/../../secret.txt", "/%2e%2e/secret.txt",
		"/%2E/secret.txt", "/a/%2e%2E/secret.txt", "/a%2f..%2fsecret.txt",
		"/a%5cb", "/%5C..%5Csecret.txt", "/null%00.txt", "/nul%00/route",
	} {
		t.Run(path, func(t *testing.T) {
			resp, _ := site.request(t, "GET", path, http.Header{"Accept": {"text/html"}}, nil)
			requireStatus(t, resp, 400)
			requireHeader(t, resp, "Cache-Control", "no-store")
		})
	}
}

func TestSymlinksStayInsideRoot(t *testing.T) {
	outside := makeFiles(t, map[string]string{"secret.txt": "outside secret"})
	root := makeFiles(t, map[string]string{"index.html": "SPA", "inside/real.txt": "inside file"})
	links := map[string]string{
		"escape.txt":          filepath.Join(outside, "secret.txt"),
		"escape-dir":          outside,
		"relative-escape.txt": filepath.Join("..", filepath.Base(outside), "secret.txt"),
		"relative.txt":        "inside/real.txt", "inside/parent.txt": "../inside/real.txt",
		"inside-dir": "inside",
	}
	// Use an actual relative path, independent of t.TempDir's parent layout.
	relative, err := filepath.Rel(root, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	links["relative-escape.txt"] = relative
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root, SPA: true}}})
	for _, path := range []string{"/escape.txt", "/escape-dir/secret.txt", "/relative-escape.txt", "/escape-dir/navigation"} {
		t.Run(path, func(t *testing.T) {
			resp, _ := site.request(t, "GET", path, http.Header{"Accept": {"text/html"}}, nil)
			if resp.StatusCode != 403 && resp.StatusCode != 404 {
				t.Fatalf("outside symlink status = %d, want rejection", resp.StatusCode)
			}
			requireHeader(t, resp, "Cache-Control", "no-store")
		})
	}
	for _, path := range []string{"/relative.txt", "/inside/parent.txt", "/inside-dir/real.txt"} {
		t.Run(path, func(t *testing.T) {
			resp, body := site.request(t, "GET", path, nil, nil)
			requireStatus(t, resp, 200)
			requireBody(t, body, "inside file")
		})
	}
}
