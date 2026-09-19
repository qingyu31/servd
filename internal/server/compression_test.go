package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.qingyu31.com/servd/internal/config"
)

func TestDynamicGzip(t *testing.T) {
	text := strings.Repeat("compressible 中文 text\n", 200)
	root := makeFiles(t, map[string]string{"asset.txt": text, "empty.txt": ""})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	for _, tc := range []struct{ path, want string }{{"/asset.txt", text}, {"/empty.txt", ""}} {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := site.request(t, "GET", tc.path, http.Header{"Accept-Encoding": {"gzip, identity;q=0"}}, nil)
			requireStatus(t, resp, 200)
			requireHeader(t, resp, "Content-Encoding", "gzip")
			requireHeader(t, resp, "Cache-Control", "no-cache")
			requireHeader(t, resp, "X-Content-Type-Options", "nosniff")
			requireVary(t, resp.Header, "Accept-Encoding")
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
				t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
			}
			requireBody(t, gunzipBytes(t, body), tc.want)
		})
	}
}

func TestAcceptEncodingNegotiationAndPrecompressedBytes(t *testing.T) {
	plain := []byte("source text for precompressed representations")
	compressed := gzipBytes(t, plain)
	// Opaque .br bytes test passthrough, not Brotli validity or decoding.
	brotli := []byte{0x8b, 0x08, 0x80, 0x62, 0x72, 0x2d, 0x66, 0x69, 0x78, 0x74, 0x75, 0x72, 0x65, 0x03}
	root := makeFiles(t, map[string]string{"asset.txt": string(plain)})
	writeTestFile(t, root, "asset.txt.gz", compressed)
	writeTestFile(t, root, "asset.txt.br", brotli)
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	for _, tc := range []struct {
		name   string
		accept []string
		coding string
		status int
	}{
		{"absent", nil, "", 200},
		{"identity", []string{"identity"}, "", 200},
		{"gzip", []string{"gzip"}, "gzip", 200},
		{"br", []string{"br"}, "br", 200},
		{"gzip disabled", []string{"gzip;q=0"}, "", 200},
		{"identity disabled", []string{"gzip;q=0.5, identity;q=0"}, "gzip", 200},
		{"weighted gzip", []string{"br;q=0.3, gzip;q=0.9, identity;q=0.1"}, "gzip", 200},
		{"weighted br", []string{"br;q=0.9, gzip;q=0.3, identity;q=0.1"}, "br", 200},
		{"weighted identity", []string{"br;q=0.2, gzip;q=0.3, identity;q=0.9"}, "", 200},
		{"wildcard excludes explicit gzip", []string{"gzip;q=0, *;q=0.8, identity;q=0"}, "br", 200},
		{"wildcard explicit override", []string{"*;q=0, gzip;q=1"}, "gzip", 200},
		{"wildcard identity override", []string{"*;q=0, identity;q=1"}, "", 200},
		{"multiline", []string{"gzip;q=0.9", "br;q=0.2, identity;q=0"}, "gzip", 200},
		{"all excluded", []string{"gzip;q=0, br;q=0, identity;q=0"}, "", 406},
		{"wildcard excluded", []string{"*;q=0"}, "", 406},
		{"unavailable encoding", []string{"deflate, identity;q=0"}, "", 406},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := site.request(t, "GET", "/asset.txt", http.Header{"Accept-Encoding": tc.accept}, nil)
			requireStatus(t, resp, tc.status)
			requireVary(t, resp.Header, "Accept-Encoding")
			requireHeader(t, resp, "Content-Encoding", tc.coding)
			if tc.status == 406 {
				requireHeader(t, resp, "Cache-Control", "no-store")
				return
			}
			want := plain
			switch tc.coding {
			case "gzip":
				want = compressed
				requireBody(t, gunzipBytes(t, body), string(plain))
			case "br":
				want = brotli
			}
			if !bytes.Equal(body, want) {
				t.Errorf("%s representation bytes = %x, want %x", tc.coding, body, want)
			}
			requireHeader(t, resp, "Content-Length", strconv.Itoa(len(want)))
			requireHeader(t, resp, "Cache-Control", "no-cache")
		})
	}
}

func TestStalePrecompressedCopiesAreNotServed(t *testing.T) {
	root := makeFiles(t, map[string]string{"asset.txt": "new source"})
	gz := writeTestFile(t, root, "asset.txt.gz", gzipBytes(t, []byte("stale source")))
	br := writeTestFile(t, root, "asset.txt.br", []byte("stale opaque Brotli fixture"))
	old := time.Now().Add(-2 * time.Hour)
	for _, name := range []string{gz, br} {
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	resp, body := site.request(t, "GET", "/asset.txt", http.Header{"Accept-Encoding": {"gzip, identity;q=0"}}, nil)
	requireStatus(t, resp, 200)
	requireHeader(t, resp, "Content-Encoding", "gzip")
	requireBody(t, gunzipBytes(t, body), "new source")
	resp, _ = site.request(t, "GET", "/asset.txt", http.Header{"Accept-Encoding": {"br, gzip;q=0, identity;q=0"}}, nil)
	requireStatus(t, resp, 406)
	requireHeader(t, resp, "Cache-Control", "no-store")
}

func TestNoncompressibleImageUsesIdentityOr406(t *testing.T) {
	image := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0xff, 0x80}
	root := makeFiles(t, map[string]string{"image.png": string(image)})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	resp, body := site.request(t, "GET", "/image.png", http.Header{"Accept-Encoding": {"gzip, br"}}, nil)
	requireStatus(t, resp, 200)
	requireHeader(t, resp, "Content-Encoding", "")
	requireHeader(t, resp, "Content-Type", "image/png")
	if !bytes.Equal(body, image) {
		t.Error("identity image bytes changed")
	}
	for _, method := range []string{"GET", "HEAD"} {
		resp, body := site.request(t, method, "/image.png", http.Header{"Accept-Encoding": {"gzip, identity;q=0"}}, nil)
		requireStatus(t, resp, 406)
		requireHeader(t, resp, "Content-Encoding", "")
		requireHeader(t, resp, "Cache-Control", "no-store")
		if method == "HEAD" {
			requireBody(t, body, "")
		}
	}
}

func TestEncodingSpecificValidatorsHEADAnd304(t *testing.T) {
	plain := strings.Repeat("source for conditional requests\n", 50)
	root := makeFiles(t, map[string]string{"dynamic.txt": plain, "pre.txt": plain})
	writeTestFile(t, root, "pre.txt.gz", gzipBytes(t, []byte(plain)))
	writeTestFile(t, root, "pre.txt.br", []byte("opaque .br passthrough fixture"))
	// HTTP dates have one-second resolution; avoid timing assumptions.
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, name := range []string{"dynamic.txt", "pre.txt", "pre.txt.gz", "pre.txt.br"} {
		if err := os.Chtimes(filepath.Join(root, name), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	for _, path := range []string{"/dynamic.txt", "/pre.txt"} {
		t.Run(path, func(t *testing.T) {
			codings := []string{"identity", "gzip"}
			if path == "/pre.txt" {
				codings = append(codings, "br")
			}
			etags := make(map[string]string)
			for _, coding := range codings {
				t.Run(coding, func(t *testing.T) {
					h := http.Header{"Accept-Encoding": {coding}}
					resp, _ := site.request(t, "GET", path, h, nil)
					requireStatus(t, resp, 200)
					etag := resp.Header.Get("ETag")
					if etag == "" {
						t.Fatal("missing ETag")
					}
					for otherCoding, otherTag := range etags {
						if etag == otherTag {
							t.Errorf("ETag for %s equals %s: %s", coding, otherCoding, etag)
						}
					}
					etags[coding] = etag
					modified := resp.Header.Get("Last-Modified")
					if modified == "" {
						t.Fatal("missing Last-Modified")
					}
					head, body := site.request(t, "HEAD", path, h, nil)
					requireStatus(t, head, 200)
					requireBody(t, body, "")
					for _, name := range []string{"ETag", "Last-Modified", "Content-Encoding", "Content-Type", "Cache-Control"} {
						requireHeader(t, head, name, resp.Header.Get(name))
					}
					for _, condition := range []http.Header{
						{"If-None-Match": {etag}},
						{"If-None-Match": {`"other", ` + etag}},
						{"If-None-Match": {"*"}},
						{"If-Modified-Since": {modified}},
					} {
						for _, method := range []string{"GET", "HEAD"} {
							condition.Set("Accept-Encoding", coding)
							notModified, body := site.request(t, method, path, condition, nil)
							requireStatus(t, notModified, 304)
							requireBody(t, body, "")
							requireHeader(t, notModified, "ETag", etag)
							requireHeader(t, notModified, "Cache-Control", "no-cache")
							requireVary(t, notModified.Header, "Accept-Encoding")
						}
					}
					// If-None-Match takes precedence over a matching modification date.
					resp, _ = site.request(t, "GET", path, http.Header{
						"Accept-Encoding": {coding}, "If-None-Match": {`"not-this-etag"`}, "If-Modified-Since": {modified},
					}, nil)
					requireStatus(t, resp, 200)
				})
			}
			if etags["identity"] != "" {
				resp, body := site.request(t, "GET", path, http.Header{
					"Accept-Encoding": {"gzip"}, "If-None-Match": {etags["identity"]},
				}, nil)
				requireStatus(t, resp, 200)
				requireBody(t, gunzipBytes(t, body), plain)
			}
		})
	}
}

func TestIdentityRangeIfRangeAndConditionalPrecedence(t *testing.T) {
	const text = "0123456789abcdefghijklmnopqrstuvwxyz"
	root := makeFiles(t, map[string]string{"asset.txt": text})
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(root, "asset.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	base, _ := site.request(t, "GET", "/asset.txt", nil, nil)
	requireStatus(t, base, 200)
	for _, tc := range []struct {
		name, rangeValue, ifRange string
		status                    int
		body                      string
	}{
		{"simple", "bytes=2-5", "", 206, "2345"},
		{"suffix", "bytes=-3", "", 206, "xyz"},
		{"open ended", "bytes=33-", "", 206, "xyz"},
		{"matching date", "bytes=2-5", base.Header.Get("Last-Modified"), 206, "2345"},
		{"old date", "bytes=2-5", stamp.Add(-time.Hour).Format(http.TimeFormat), 200, text},
		{"different etag", "bytes=2-5", `"wrong"`, 200, text},
		{"unsatisfiable", "bytes=999-", "", 416, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				h := http.Header{"Range": {tc.rangeValue}, "Accept-Encoding": {"identity"}}
				if tc.ifRange != "" {
					h.Set("If-Range", tc.ifRange)
				}
				resp, body := site.request(t, method, "/asset.txt", h, nil)
				requireStatus(t, resp, tc.status)
				if method == "HEAD" {
					requireBody(t, body, "")
				} else if tc.status != 416 {
					requireBody(t, body, tc.body)
				}
				if tc.status == 416 {
					requireHeader(t, resp, "Cache-Control", "no-store")
					requireHeader(t, resp, "Content-Range", "bytes */36")
				} else {
					requireHeader(t, resp, "Cache-Control", "no-cache")
					requireHeader(t, resp, "Content-Length", strconv.Itoa(len(tc.body)))
				}
			}
		})
	}
	resp, body := site.request(t, "GET", "/asset.txt", http.Header{"Range": {"bytes=2-5"}, "If-None-Match": {base.Header.Get("ETag")}}, nil)
	requireStatus(t, resp, 304)
	requireBody(t, body, "")
	// RFC If-Range requires a strong validator. A weak ETag must not enable 206.
	if etag := base.Header.Get("ETag"); strings.HasPrefix(etag, "W/") {
		resp, body := site.request(t, "GET", "/asset.txt", http.Header{"Range": {"bytes=2-5"}, "If-Range": {etag}}, nil)
		requireStatus(t, resp, 200)
		requireBody(t, body, text)
	}
}

func TestDynamicGzipIgnoresRangeAndHEADWritesNoFooter(t *testing.T) {
	text := strings.Repeat("dynamic range text\n", 100)
	root := makeFiles(t, map[string]string{"asset.txt": text, "empty.txt": ""})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	for _, rangeValue := range []string{"bytes=1-4", "bytes=999999-", "bytes=0-1,4-5"} {
		t.Run(rangeValue, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				resp, body := site.request(t, method, "/asset.txt", http.Header{
					"Accept-Encoding": {"gzip"}, "Range": {rangeValue},
				}, nil)
				requireStatus(t, resp, 200)
				requireHeader(t, resp, "Content-Encoding", "gzip")
				requireHeader(t, resp, "Content-Range", "")
				if method == "HEAD" {
					requireBody(t, body, "")
				} else {
					requireBody(t, gunzipBytes(t, body), text)
				}
			}
		})
	}
	resp, body := site.request(t, "HEAD", "/empty.txt", http.Header{"Accept-Encoding": {"gzip"}}, nil)
	requireStatus(t, resp, 200)
	requireBody(t, body, "")
}

func TestGzipHEADAnd304DoNotCorruptPersistentHTTPConnection(t *testing.T) {
	root := makeFiles(t, map[string]string{"asset.txt": "identity response after bodyless responses"})
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	base, _ := site.request(t, "GET", "/asset.txt", http.Header{"Accept-Encoding": {"gzip"}}, nil)
	requireStatus(t, base, 200)
	address := mustURL(t, site.url).Host
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Pipelining exposes illegal gzip bytes after a bodyless HEAD or 304.
	_, err = fmt.Fprintf(conn,
		"HEAD /asset.txt HTTP/1.1\r\nHost: %s\r\nAccept-Encoding: gzip\r\n\r\n"+
			"GET /asset.txt HTTP/1.1\r\nHost: %s\r\nAccept-Encoding: gzip\r\nIf-None-Match: %s\r\n\r\n"+
			"GET /asset.txt HTTP/1.1\r\nHost: %s\r\nAccept-Encoding: identity\r\nConnection: close\r\n\r\n",
		address, address, base.Header.Get("ETag"), address)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for _, tc := range []struct {
		method string
		status int
		body   string
	}{{"HEAD", 200, ""}, {"GET", 304, ""}, {"GET", 200, "identity response after bodyless responses"}} {
		resp, err := http.ReadResponse(reader, &http.Request{Method: tc.method})
		if err != nil {
			t.Fatalf("response framing after HEAD/304: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		requireStatus(t, resp, tc.status)
		requireBody(t, body, tc.body)
	}
}

func TestHTMLCachePolicySurvivesCompressionAndRevalidation(t *testing.T) {
	root := makeFiles(t, map[string]string{"index.html": "<!doctype html><title>shell</title>"})
	for _, tc := range []struct {
		name, path, cache string
		spa               bool
	}{
		{"static HTML", "/index.html", "no-cache", false},
		{"SPA HTML", "/index.html", "no-store", true},
		{"SPA fallback", "/deep/route", "no-store", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root, SPA: tc.spa}}})
			h := http.Header{"Accept": {"text/html"}, "Accept-Encoding": {"gzip"}}
			resp, body := site.request(t, "GET", tc.path, h, nil)
			requireStatus(t, resp, 200)
			requireHeader(t, resp, "Cache-Control", tc.cache)
			requireHeader(t, resp, "Content-Encoding", "gzip")
			requireBody(t, gunzipBytes(t, body), "<!doctype html><title>shell</title>")
			h.Set("If-None-Match", resp.Header.Get("ETag"))
			resp, body = site.request(t, "GET", tc.path, h, nil)
			requireStatus(t, resp, 304)
			requireHeader(t, resp, "Cache-Control", tc.cache)
			requireBody(t, body, "")
		})
	}
}

func TestPrecompressedRangesReferToEncodedBytes(t *testing.T) {
	plain := []byte(strings.Repeat("precompressed source\n", 50))
	gz := gzipBytes(t, plain)
	br := []byte("opaque br fixture used only for encoded-byte range checks")
	root := makeFiles(t, map[string]string{"asset.txt": string(plain)})
	writeTestFile(t, root, "asset.txt.gz", gz)
	writeTestFile(t, root, "asset.txt.br", br)
	site := newTestSite(t, config.Config{Directories: []config.Directory{{Mount: "/", Path: root}}})
	for _, tc := range []struct {
		coding string
		data   []byte
	}{{"gzip", gz}, {"br", br}} {
		t.Run(tc.coding, func(t *testing.T) {
			base, _ := site.request(t, "GET", "/asset.txt", http.Header{"Accept-Encoding": {tc.coding}}, nil)
			requireStatus(t, base, 200)
			for _, method := range []string{"GET", "HEAD"} {
				resp, body := site.request(t, method, "/asset.txt", http.Header{
					"Accept-Encoding": {tc.coding}, "Range": {"bytes=2-7"}, "If-Range": {base.Header.Get("Last-Modified")},
				}, nil)
				requireStatus(t, resp, 206)
				requireHeader(t, resp, "Content-Encoding", tc.coding)
				requireHeader(t, resp, "Content-Range", fmt.Sprintf("bytes 2-7/%d", len(tc.data)))
				requireHeader(t, resp, "Content-Length", "6")
				requireHeader(t, resp, "ETag", base.Header.Get("ETag"))
				if method == "HEAD" {
					requireBody(t, body, "")
				} else if !bytes.Equal(body, tc.data[2:8]) {
					t.Errorf("encoded range = %x, want %x", body, tc.data[2:8])
				}
			}
			resp, _ := site.request(t, "GET", "/asset.txt", http.Header{
				"Accept-Encoding": {tc.coding}, "Range": {"bytes=999999-"},
			}, nil)
			requireStatus(t, resp, 416)
			requireHeader(t, resp, "Content-Encoding", "")
			requireHeader(t, resp, "Cache-Control", "no-store")
		})
	}
}
