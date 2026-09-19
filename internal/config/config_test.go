package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first", "second", "dir=with=equals", "with spaces"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	equals := filepath.Join(root, "dir=with=equals")
	tests := []struct {
		name string
		args []string
		want []Directory
	}{
		{"default", nil, []Directory{{"/", root, false}}},
		{"only port", []string{"--port=9000"}, []Directory{{"/", root, false}}},
		{"only hosts", []string{"--host=example.org"}, []Directory{{"/", root, false}}},
		{"only cors", []string{"--cors=*"}, []Directory{{"/", root, false}}},
		{"relative static", []string{"--static=./first"}, []Directory{{"/", first, false}}},
		{"relative spa", []string{"--spa=./second"}, []Directory{{"/", second, true}}},
		{"absolute static", []string{"--static=" + first}, []Directory{{"/", first, false}}},
		{"absolute spa", []string{"--spa=" + second}, []Directory{{"/", second, true}}},
		{"relative mapping", []string{"--static=/assets=./first"}, []Directory{{"/assets", first, false}}},
		{"absolute mapping", []string{"--spa=/app=" + second}, []Directory{{"/app", second, true}}},
		{"root mapping", []string{"--static=/=" + first}, []Directory{{"/", first, false}}},
		{"trailing slash", []string{"--spa=/app/=./second"}, []Directory{{"/app", second, true}}},
		{"relative equals", []string{"--static=./dir=with=equals"}, []Directory{{"/", equals, false}}},
		{"bare relative equals", []string{"--spa=dir=with=equals"}, []Directory{{"/", equals, true}}},
		{"mapped relative equals", []string{"--static=/files=./dir=with=equals"}, []Directory{{"/files", equals, false}}},
		{"mapped absolute equals", []string{"--spa=/app=" + equals}, []Directory{{"/app", equals, true}}},
		{"unambiguous absolute equals", []string{"--static=/=" + equals}, []Directory{{"/", equals, false}}},
		{"clean relative path", []string{"--static=./second/../first/"}, []Directory{{"/", first, false}}},
		{"spaces in directory", []string{"--static=./with spaces"}, []Directory{{"/", filepath.Join(root, "with spaces"), false}}},
		{"separate value", []string{"--static", "./first", "--spa", "/app=./second"}, []Directory{{"/", first, false}, {"/app", second, true}}},
		{"declaration order", []string{"--spa=./second", "--static=/assets=./first", "--proxy=/api=http://example.org", "--spa=/app=./first", "--static=./second"}, []Directory{{"/", second, true}, {"/assets", first, false}, {"/app", first, true}, {"/", second, false}}},
		{"repeated directories", []string{"--static=./first", "--static=./second"}, []Directory{{"/", first, false}, {"/", second, false}}},
		{"proxy suppresses default", []string{"--proxy=/api=http://example.org"}, nil},
		{"websocket suppresses default", []string{"--ws=/ws=wss://example.org/ws"}, nil},
		{"help suppresses default", []string{"--help"}, nil},
		{"version suppresses default", []string{"--version"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Directories, tt.want) {
				t.Fatalf("Directories = %#v, want %#v", cfg.Directories, tt.want)
			}
		})
	}
}

func TestParseDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"static", "spa"} {
		for _, value := range []string{"", "missing", file, "./file", "/assets=", "/assets=missing", "/assets=" + file, "/assets\x00=.", "\x00"} {
			t.Run(option+"/"+value, func(t *testing.T) {
				if _, err := Parse([]string{"--" + option + "=" + value}); err == nil {
					t.Fatalf("accepted invalid directory %q", value)
				}
			})
		}
	}
}

func TestParsePorts(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want int
	}{
		{"default", nil, 8080},
		{"minimum", []string{"--port=1"}, 1},
		{"maximum", []string{"--port=65535"}, 65535},
		{"separate value", []string{"--port", "9000"}, 9000},
		{"leading zeroes", []string{"--port=08080"}, 8080},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(append(tt.args, "--help"))
			if err != nil || cfg.Port != tt.want {
				t.Fatalf("Parse() port = %d, err = %v; want %d", cfg.Port, err, tt.want)
			}
		})
	}
}

func TestParseArgumentErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown"}, {"--help", "--unknown"}, {"--version", "file"},
		{"."}, {"--", "."}, {"-"}, {"", "--help"}, {"--help", ""},
		{"--port"}, {"--static"}, {"--spa"}, {"--host"}, {"--cors"}, {"--proxy"}, {"--ws"},
		{"--port="}, {"--port", ""}, {"--host="}, {"--cors="}, {"--proxy="}, {"--ws="},
		{"--port=0"}, {"--port=65536"}, {"--port=-1"}, {"--port=+80"},
		{"--port=abc"}, {"--port=1.5"}, {"--port=0x50"}, {"--port= 80"}, {"--port=80 "},
		{"--port=999999999999999999999999999999"}, {"--port=８０"},
		{"--port=8080", "--port=8080"}, {"--port", "8080", "-port=9090"},
		{"--help=maybe"}, {"--version="},
		{"--proxy=http://example.org"}, {"--ws=wss://example.org"},
		{"--proxy=/api=wss://example.org"}, {"--ws=/ws=https://example.org"},
		{"--proxy=/api=ftp://example.org"}, {"--ws=/ws=ftp://example.org"},
		{"--host=example.org:80"}, {"--host=127.0.0.1:80"}, {"--host=[::1]:80"},
		{"--host=http://example.org"}, {"--host=*"}, {"--cors=null"},
		{"--cors=*", "--cors=null"}, {"--cors=null", "--cors=*"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cfg, err := Parse(args)
			if err == nil {
				t.Fatalf("accepted invalid arguments %#v", args)
			}
			if !reflect.DeepEqual(cfg, Config{}) {
				t.Fatalf("returned partial configuration after error: %#v", cfg)
			}
		})
	}
}

func TestParseProxies(t *testing.T) {
	type proxySpec struct{ mount, target string }
	for _, tt := range []struct {
		name    string
		args    []string
		http    []proxySpec
		sockets []proxySpec
	}{
		{"http", []string{"--proxy=/api/=http://example.org/api/"}, []proxySpec{{"/api", "http://example.org/api/"}}, nil},
		{"https", []string{"--proxy=/api=https://EXAMPLE.ORG./api?q=a=b&next=%2F"}, []proxySpec{{"/api", "https://example.org/api?q=a=b&next=%2F"}}, nil},
		{"ws unchanged", []string{"--ws=/ws/=ws://example.org/socket?key=a=b"}, nil, []proxySpec{{"/ws", "ws://example.org/socket?key=a=b"}}},
		{"wss unchanged", []string{"--ws=/ws=wss://example.org/ws"}, nil, []proxySpec{{"/ws", "wss://example.org/ws"}}},
		{"root", []string{"--proxy=/=http://example.org"}, []proxySpec{{"/", "http://example.org"}}, nil},
		{"IPv6", []string{"--proxy=/api=http://[2001:0DB8:0:0::1]:8080/api"}, []proxySpec{{"/api", "http://[2001:db8::1]:8080/api"}}, nil},
		{"IPv4", []string{"--ws=/ws=WS://127.0.0.1:08080/ws"}, nil, []proxySpec{{"/ws", "ws://127.0.0.1:8080/ws"}}},
		{"encoded path", []string{"--proxy=/api=https://example.org/a%2Fb?q=%23"}, []proxySpec{{"/api", "https://example.org/a%2Fb?q=%23"}}, nil},
		{"empty query", []string{"--proxy=/api=http://example.org/api?"}, []proxySpec{{"/api", "http://example.org/api?"}}, nil},
		{"cross type same mount", []string{"--proxy=/api/=http://example.org", "--ws=/api=wss://example.org"}, []proxySpec{{"/api", "http://example.org"}}, []proxySpec{{"/api", "wss://example.org"}}},
		{"order", []string{"--proxy=/z=http://z.example", "--ws=/z=wss://z.example", "--proxy=/a=http://a.example", "--ws=/a=ws://a.example"}, []proxySpec{{"/z", "http://z.example"}, {"/a", "http://a.example"}}, []proxySpec{{"/z", "wss://z.example"}, {"/a", "ws://a.example"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Directories) != 0 {
				t.Fatalf("unexpected default directories: %#v", cfg.Directories)
			}
			for _, group := range []struct {
				name string
				got  []Proxy
				want []proxySpec
			}{{"http", cfg.Proxies, tt.http}, {"ws", cfg.WebSockets, tt.sockets}} {
				if len(group.got) != len(group.want) {
					t.Fatalf("%s count = %d, want %d", group.name, len(group.got), len(group.want))
				}
				for i, proxy := range group.got {
					if proxy.Target == nil || proxy.Mount != group.want[i].mount || proxy.Target.String() != group.want[i].target {
						t.Errorf("%s[%d] = %#v, want %#v", group.name, i, proxy, group.want[i])
					}
				}
			}
		})
	}
}

func TestParseDuplicateProxyMounts(t *testing.T) {
	for _, option := range []struct{ name, scheme string }{{"proxy", "http"}, {"ws", "ws"}} {
		for _, mounts := range [][2]string{{"/api", "/api"}, {"/api/", "/api"}, {"/api", "/api/"}, {"/", "/"}} {
			t.Run(option.name+mounts[0]+mounts[1], func(t *testing.T) {
				args := []string{"--" + option.name + "=" + mounts[0] + "=" + option.scheme + "://one.example", "--" + option.name + "=" + mounts[1] + "=" + option.scheme + "://two.example"}
				if _, err := Parse(args); err == nil {
					t.Fatalf("accepted duplicate mounts: %#v", args)
				}
			})
		}
	}
}

func TestParseInvalidMounts(t *testing.T) {
	directory := t.TempDir()
	for _, mount := range []string{"", "api", "//api", "/api//v1", "/api//", "/.", "/..", "/api/../v1", "/api/./", "/api\\v1", "/api\x00", "/api?x", "/api#x", "/%2e", "/a%2Fb", "/%41", "/bad%", "/a b", "/a\tb", "/a\u00a0b"} {
		for _, option := range []struct{ name, target string }{{"proxy", "https://example.org"}, {"ws", "wss://example.org"}, {"static", directory}, {"spa", directory}} {
			t.Run(option.name+mount, func(t *testing.T) {
				if _, err := Parse([]string{"--" + option.name + "=" + mount + "=" + option.target}); err == nil {
					t.Fatalf("accepted invalid mount %q", mount)
				}
			})
		}
	}
}

func TestParseInvalidTargets(t *testing.T) {
	for _, target := range []string{
		"", "example.org", "http:example.org", "http:///api", "//example.org/api", "http://",
		"http://user@example.org", "http://user:pass@example.org", "http://@example.org",
		"http://example.org/#fragment", "http://example.org/#", "http://example.org/?x=%zz", "http://example.org/?a=1;b=2",
		"http://example.org/%zz", "http://example.org/?x=hello world", "http://example.org/\n",
		"http://example.org:0", "http://example.org:65536", "http://example.org:", "http://example.org:abc", "http://example.org:+80",
		"http://2001:db8::1", "http://[::1", "http://[::1]junk", "http://[127.0.0.1]", "http://[fe80::1%25eth0]",
		"http://bad_host", "http://bad..host", "http://-bad.host", "http://host..", "http://256.1.1.1", "http://127.01.0.1",
		"http://例子.test", "http://K.example", "http://host\\evil", "http://a b", "http://%65xample.org", "http://example.org\x00",
	} {
		for _, option := range []string{"proxy", "ws"} {
			value := target
			if option == "ws" {
				value = strings.Replace(target, "http", "ws", 1)
			}
			t.Run(option+"/"+target, func(t *testing.T) {
				if _, err := Parse([]string{"--" + option + "=/route=" + value}); err == nil {
					t.Fatalf("accepted invalid target %q", value)
				}
			})
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	maxHost := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, tt := range []struct{ value, want string }{
		{"Example.COM", "example.com"}, {"Example.COM.", "example.com"}, {"Example.COM.:8080", "example.com"},
		{"localhost", "localhost"}, {"LOCALHOST:80", "localhost"}, {"a-b.example:00080", "a-b.example"},
		{"127.0.0.1", "127.0.0.1"}, {"127.0.0.1:65535", "127.0.0.1"}, {"127.0.0.1.", "127.0.0.1"},
		{"::1", "::1"}, {"[::1]", "::1"}, {"[::1]:80", "::1"},
		{"2001:0DB8:0000:0:0:0:0:1", "2001:db8::1"}, {"[2001:0DB8::1]:443", "2001:db8::1"},
		{"2001:db8::1:80", "2001:db8::1:80"}, {"[::ffff:192.0.2.1]:80", "192.0.2.1"},
		{"XN--BCHER-KVA.Example.", "xn--bcher-kva.example"}, {maxHost + ".:80", maxHost},
	} {
		t.Run(tt.value, func(t *testing.T) {
			got, err := NormalizeHost(tt.value)
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeHost(%q) = %q, %v; want %q", tt.value, got, err, tt.want)
			}
			if again, err := NormalizeHost(got); err != nil || again != got {
				t.Fatalf("normalization is not idempotent: %q, %v", again, err)
			}
		})
	}
}

func TestNormalizeHostErrors(t *testing.T) {
	for _, value := range []string{
		"", ".", "..", "example..", ".example", "bad..example", "-bad.example", "bad-.example", "bad_host",
		"example.org:", ":80", "example.org:0", "example.org:65536", "example.org:-1", "example.org:+80", "example.org:http", "example.org:1.5", "example.org:999999999999999999999999",
		"[::1]:", "[::1]:x", "[::1]:80:90", "[::1]extra", "[::1", "::1]", "[example.org]", "[127.0.0.1]", "[]",
		":::1", "::1.", "2001:db8::1::2", "example.org:80:90", "fe80::1%en0", "[fe80::1%en0]",
		"127.1", "127.0.0.256", "0127.0.0.1", "127.0.00.1", "2130706433", "1.2.3.4.5",
		" example.org", "example.org ", "a\tb", "a\nb", "example.org\r\n", "a\x00b", "a\x7fb", "a\u00a0b",
		"http://example.org", "example.org/path", "example.org?x", "example.org#x", "user@example.org", "example\\org", "*.example.org", "example%2eorg",
		"例子.test", "bücher.example", "K.example", "İ.example", "example。org", "\xff.example",
		strings.Repeat("a", 64) + ".example", strings.Repeat("a.", 127) + "aa",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := NormalizeHost(value); err == nil || got != "" {
				t.Fatalf("NormalizeHost(%q) = %q, %v; want an error and empty result", value, got, err)
			}
			if _, err := Parse([]string{"--host=" + value, "--help"}); err == nil {
				t.Fatalf("host whitelist accepted %q", value)
			}
		})
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for _, tt := range []struct{ value, want string }{
		{"*", "*"}, {"http://Example.ORG", "http://example.org"}, {"HTTPS://EXAMPLE.ORG.", "https://example.org"},
		{"http://example.org:80", "http://example.org"}, {"https://example.org:443", "https://example.org"},
		{"http://example.org:443", "http://example.org:443"}, {"https://example.org:80", "https://example.org:80"},
		{"https://example.org:0443", "https://example.org"}, {"https://example.org:08443", "https://example.org:8443"},
		{"http://127.0.0.1:80", "http://127.0.0.1"}, {"http://[::1]:80", "http://[::1]"},
		{"https://[2001:0DB8:0:0::1]:443", "https://[2001:db8::1]"}, {"https://[2001:DB8::1]:8443", "https://[2001:db8::1]:8443"},
		{"http://[::ffff:192.0.2.1]:80", "http://192.0.2.1"}, {"https://XN--BCHER-KVA.example.", "https://xn--bcher-kva.example"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			got, err := NormalizeOrigin(tt.value)
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeOrigin(%q) = %q, %v; want %q", tt.value, got, err, tt.want)
			}
			if again, err := NormalizeOrigin(got); err != nil || again != got {
				t.Fatalf("normalization is not idempotent: %q, %v", again, err)
			}
		})
	}
}

func TestNormalizeOriginErrors(t *testing.T) {
	for _, value := range []string{
		"", "null", "NULL", " *", "* ", "example.org", "//example.org", "http:example.org", "http://", "https:///",
		"ws://example.org", "wss://example.org", "ftp://example.org", "file:///tmp", "https://*.example.org",
		"https://example.org/", "https://example.org/path", "https://example.org/%2f", "https://example.org?", "https://example.org?x=1",
		"https://example.org#", "https://example.org#fragment", "https://user@example.org", "https://user:pass@example.org", "https://@example.org",
		"https://example.org:", "https://example.org:0", "https://example.org:65536", "https://example.org:no", "https://example.org:-443", "https://example.org:+443",
		"https://[::1]:", "https://[::1", "https://::1", "https://[127.0.0.1]", "https://[fe80::1%25en0]",
		"https://example..org", "https://bad_host", "https://-host.example", "https://127.0.0.256", "https://127.01.0.1",
		" https://example.org", "https://example.org ", "https://a\tb", "https://example.org\x00", "https://例子.test", "https://K.example",
		"https://one.example https://two.example", "https://one.example,https://two.example",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := NormalizeOrigin(value); err == nil || got != "" {
				t.Fatalf("NormalizeOrigin(%q) = %q, %v; want an error and empty result", value, got, err)
			}
			if _, err := Parse([]string{"--cors=" + value, "--help"}); err == nil {
				t.Fatalf("cors accepted %q", value)
			}
		})
	}
}

func TestParseWhitelists(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		hosts   []string
		origins []string
	}{
		{"multiple hosts", []string{"--host=EXAMPLE.org.", "--host=example.org", "--host=127.0.0.1", "--host=[::ffff:127.0.0.1]", "--host=2001:0DB8:0:0::1", "--host=[2001:db8::1]", "--host=XN--BCHER-KVA.example", "--host=localhost"}, []string{"example.org", "127.0.0.1", "2001:db8::1", "xn--bcher-kva.example", "localhost"}, nil},
		{"origin union", []string{"--cors=https://EXAMPLE.org:443", "--cors=https://example.org.", "--cors=http://example.org:80", "--cors=http://[2001:0DB8::1]:80", "--cors=http://[2001:db8::1]", "--cors=https://example.org:8443"}, nil, []string{"https://example.org", "http://example.org", "http://[2001:db8::1]", "https://example.org:8443"}},
		{"wildcard first", []string{"--cors=*", "--cors=https://example.org"}, nil, []string{"*"}},
		{"wildcard last", []string{"--cors=https://example.org", "--cors=*"}, nil, []string{"*"}},
		{"wildcard middle", []string{"--cors=https://example.org", "--cors=*", "--cors=http://example.org", "--cors=*"}, nil, []string{"*"}},
		{"separate values", []string{"--host", "EXAMPLE.org", "--cors", "https://EXAMPLE.org:443"}, []string{"example.org"}, []string{"https://example.org"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(append(tt.args, "--help"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Hosts, tt.hosts) || !reflect.DeepEqual(cfg.Origins, tt.origins) {
				t.Fatalf("whitelists = %#v, %#v; want %#v, %#v", cfg.Hosts, cfg.Origins, tt.hosts, tt.origins)
			}
		})
	}
}

func TestParseHelpVersionWithoutWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args          []string
		help, version bool
	}{
		{[]string{"--help"}, true, false}, {[]string{"-h"}, true, false},
		{[]string{"--version"}, false, true}, {[]string{"--help", "--version"}, true, true},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cfg, err := Parse(tt.args)
			if err != nil || cfg.Help != tt.help || cfg.Version != tt.version || len(cfg.Directories) != 0 || cfg.Port != 8080 {
				t.Fatalf("Parse(%q) = %#v, %v", tt.args, cfg, err)
			}
		})
	}
	if _, err := Parse(nil); err == nil {
		t.Fatal("default directory unexpectedly resolved in a deleted working directory")
	}
	if _, err := Parse([]string{"--static=.", "--help"}); err == nil {
		t.Fatal("help bypassed validation of an explicit directory")
	}
	if _, err := Parse([]string{"--proxy=/api=http://example.org"}); err != nil {
		t.Fatalf("proxy-only configuration needs no working directory: %v", err)
	}
}

func TestParseIndependentCalls(t *testing.T) {
	args := []string{"--help", "--port=9000", "--host=example.org", "--cors=*", "--proxy=/api=http://example.org", "--ws=/ws=wss://example.org"}
	first, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse(args)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("Parse calls share state: %#v, %v", second, err)
	}
	empty, err := Parse([]string{"--help"})
	if err != nil || !reflect.DeepEqual(empty, Config{Port: 8080, Help: true}) {
		t.Fatalf("Parse leaked previous options: %#v, %v", empty, err)
	}
}

func TestParseActions(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		action Action
		port   int
	}{
		{"serve", []string{"--static=."}, ActionServe, 8080},
		{"daemon with routes", []string{"--daemon", "--static=."}, ActionDaemon, 8080},
		{"daemon custom port", []string{"--daemon", "--port=9", "--static=."}, ActionDaemon, 9},
		{"status default port", []string{"--status"}, ActionStatus, 8080},
		{"stop custom port", []string{"--stop", "--port=9001"}, ActionStop, 9001},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(tc.args)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.args, err)
			}
			if cfg.Action != tc.action || cfg.Port != tc.port {
				t.Fatalf("Parse(%q) = action %v port %d; want %v port %d", tc.args, cfg.Action, cfg.Port, tc.action, tc.port)
			}
		})
	}
}

func TestParseActionMutualExclusion(t *testing.T) {
	for _, args := range [][]string{
		{"--daemon", "--status"}, {"--daemon", "--stop"}, {"--status", "--stop"},
		{"--daemon", "--status", "--stop"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := Parse(args); err == nil {
				t.Fatalf("Parse(%q) unexpectedly succeeded", args)
			}
		})
	}
}

func TestParseQueryActionsRejectRoutes(t *testing.T) {
	for _, tc := range []struct{ name, arg string }{
		{"static", "--static=."}, {"spa", "--spa=."},
		{"proxy", "--proxy=/api=http://example.org"}, {"ws", "--ws=/ws=wss://example.org"},
		{"host", "--host=example.org"}, {"cors", "--cors=*"},
	} {
		for _, action := range []string{"--status", "--stop"} {
			t.Run(action+" "+tc.name, func(t *testing.T) {
				if _, err := Parse([]string{action, tc.arg}); err == nil {
					t.Fatalf("Parse(%q %q) unexpectedly succeeded", action, tc.arg)
				}
			})
		}
	}
}

func TestParseTLSOptions(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(cwd, "certs", "fullchain.pem")
	keyPath := filepath.Join(cwd, "certs", "privkey.pem")

	cfg, err := Parse([]string{"--static=.", "--tls-cert=certs/fullchain.pem", "--tls-key=certs/privkey.pem"})
	if err != nil {
		t.Fatalf("paired TLS options: %v", err)
	}
	if cfg.TLSCert != certPath || cfg.TLSKey != keyPath {
		t.Fatalf("TLS paths not resolved to absolute: cert=%q key=%q", cfg.TLSCert, cfg.TLSKey)
	}

	cfg, err = Parse([]string{"--daemon", "--static=.", "--tls-cert=" + certPath, "--tls-key=" + keyPath})
	if err != nil || cfg.Action != ActionDaemon || cfg.TLSCert != certPath || cfg.TLSKey != keyPath {
		t.Fatalf("daemon with TLS: %#v, %v", cfg, err)
	}

	// The files themselves are only opened at serve time, so missing files parse fine.
	cfg, err = Parse([]string{"--static=.", "--tls-cert=" + filepath.Join(cwd, "missing.pem"), "--tls-key=" + keyPath})
	if err != nil || cfg.TLSCert != filepath.Join(cwd, "missing.pem") {
		t.Fatalf("missing certificate file must fail at startup, not parse: %#v, %v", cfg, err)
	}

	if _, err := Parse([]string{"--static=.", "--tls-cert=" + certPath}); err == nil {
		t.Fatal("certificate without key accepted")
	}
	if _, err := Parse([]string{"--static=.", "--tls-key=" + keyPath}); err == nil {
		t.Fatal("key without certificate accepted")
	}
	for _, args := range [][]string{
		{"--tls-cert=a.pem", "--tls-cert=b.pem"},
		{"--tls-key=a.pem", "--tls-key=b.pem"},
		{"--tls-cert="},
		{"--tls-key="},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", args)
		}
	}
}

func TestParseQueryActionsRejectTLSOptions(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--status", "--tls-cert=cert.pem", "--tls-key=key.pem"},
		{"--stop", "--tls-cert=cert.pem"},
		{"--status", "--tls-key=key.pem"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", args)
		}
	}
	// Rejection happens before any path resolution, so a missing working
	// directory cannot break status/stop.
	if _, err := Parse([]string{"--status"}); err != nil {
		t.Fatalf("status without TLS in a deleted working directory: %v", err)
	}
}

func TestParseQueryActionsWithoutWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--status"}, {"--stop", "--port=1"}} {
		cfg, err := Parse(args)
		if err != nil {
			t.Fatalf("Parse(%q): %v", args, err)
		}
		if len(cfg.Directories) != 0 {
			t.Fatalf("Parse(%q) resolved a default directory: %#v", args, cfg.Directories)
		}
	}
	if _, err := Parse([]string{"--daemon"}); err == nil {
		t.Fatal("daemon without routes must still require a usable working directory")
	}
}
