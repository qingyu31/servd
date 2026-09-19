package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type Directory struct {
	Mount string
	Path  string
	SPA   bool
}

type Proxy struct {
	Mount  string
	Target *url.URL
}

type Action int

const (
	ActionServe Action = iota
	ActionDaemon
	ActionStatus
	ActionStop
)

func (a Action) String() string {
	switch a {
	case ActionDaemon:
		return "daemon"
	case ActionStatus:
		return "status"
	case ActionStop:
		return "stop"
	}
	return "serve"
}

type Config struct {
	Port        int
	Directories []Directory
	Proxies     []Proxy
	WebSockets  []Proxy
	Hosts       []string
	Origins     []string
	TLSCert     string
	TLSKey      string
	Action      Action
	Help        bool
	Version     bool
}

func Parse(args []string) (Config, error) {
	cfg := Config{Port: 8080}
	fs := flag.NewFlagSet("servd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&cfg.Help, "help", false, "show help")
	fs.BoolVar(&cfg.Help, "h", false, "show help")
	fs.BoolVar(&cfg.Version, "version", false, "show version")
	var daemon, status, stop bool
	fs.BoolVar(&daemon, "daemon", false, "run in the background")
	fs.BoolVar(&status, "status", false, "show background status")
	fs.BoolVar(&stop, "stop", false, "stop background instance")

	portSet := false
	fs.Func("port", "listen port", func(value string) error {
		if portSet {
			return fmt.Errorf("port may only be specified once")
		}
		portSet = true
		port, err := parsePort(value)
		if err == nil {
			cfg.Port, _ = strconv.Atoi(port)
		}
		return err
	})
	for _, option := range []struct {
		name string
		spa  bool
	}{{"static", false}, {"spa", true}} {
		fs.Func(option.name, "directory or /mount=directory", func(value string) error {
			directory, err := parseDirectory(value, option.spa)
			if err == nil {
				cfg.Directories = append(cfg.Directories, directory)
			}
			return err
		})
	}
	for _, option := range []struct {
		name string
		dest *[]Proxy
	}{{"proxy", &cfg.Proxies}, {"ws", &cfg.WebSockets}} {
		seen := make(map[string]bool)
		fs.Func(option.name, "/mount=target URL", func(value string) error {
			proxy, err := parseProxy(value, option.name == "ws")
			if err != nil {
				return err
			}
			if seen[proxy.Mount] {
				return fmt.Errorf("duplicate %s mount %q", option.name, proxy.Mount)
			}
			seen[proxy.Mount] = true
			*option.dest = append(*option.dest, proxy)
			return nil
		})
	}
	hosts := make(map[string]bool)
	fs.Func("host", "allowed hostname or IP without a port", func(value string) error {
		host, port, err := parseAuthority(value, true)
		if err != nil {
			return err
		}
		if port != "" {
			return fmt.Errorf("host whitelist must not include a port: %q", value)
		}
		if !hosts[host] {
			hosts[host] = true
			cfg.Hosts = append(cfg.Hosts, host)
		}
		return nil
	})
	origins := make(map[string]bool)
	fs.Func("cors", "allowed Origin or *", func(value string) error {
		origin, err := NormalizeOrigin(value)
		if err != nil {
			return err
		}
		if origin == "*" {
			cfg.Origins = []string{"*"}
		} else if !origins["*"] && !origins[origin] {
			cfg.Origins = append(cfg.Origins, origin)
		}
		origins[origin] = true
		return nil
	})
	for _, option := range []struct {
		name  string
		dest  *string
		isSet *bool
	}{{"tls-cert", &cfg.TLSCert, new(bool)}, {"tls-key", &cfg.TLSKey, new(bool)}} {
		fs.Func(option.name, "PEM file path", func(value string) error {
			if *option.isSet {
				return fmt.Errorf("--%s may only be specified once", option.name)
			}
			*option.isSet = true
			if value == "" {
				return fmt.Errorf("empty --%s path", option.name)
			}
			*option.dest = value
			return nil
		})
	}
	for _, arg := range args {
		if arg == "" {
			return Config{}, fmt.Errorf("empty argument")
		}
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	selected := 0
	for _, flagSet := range []bool{daemon, status, stop} {
		if flagSet {
			selected++
		}
	}
	if selected > 1 {
		return Config{}, fmt.Errorf("--daemon, --status and --stop are mutually exclusive")
	}
	switch {
	case daemon:
		cfg.Action = ActionDaemon
	case status:
		cfg.Action = ActionStatus
	case stop:
		cfg.Action = ActionStop
	}
	if cfg.Action == ActionStatus || cfg.Action == ActionStop {
		if len(cfg.Directories)+len(cfg.Proxies)+len(cfg.WebSockets)+len(cfg.Hosts)+len(cfg.Origins) > 0 ||
			cfg.TLSCert != "" || cfg.TLSKey != "" {
			return Config{}, fmt.Errorf("--%s accepts only --port", cfg.Action)
		}
		return cfg, nil
	}
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			return Config{}, fmt.Errorf("--tls-cert and --tls-key must be used together")
		}
		cert, err := filepath.Abs(cfg.TLSCert)
		if err != nil {
			return Config{}, fmt.Errorf("resolve certificate path: %w", err)
		}
		key, err := filepath.Abs(cfg.TLSKey)
		if err != nil {
			return Config{}, fmt.Errorf("resolve key path: %w", err)
		}
		cfg.TLSCert, cfg.TLSKey = cert, key
	}
	if !cfg.Help && !cfg.Version && len(cfg.Directories)+len(cfg.Proxies)+len(cfg.WebSockets) == 0 {
		directory, err := parseDirectory(".", false)
		if err != nil {
			return Config{}, err
		}
		cfg.Directories = []Directory{directory}
	}
	return cfg, nil
}

func parseDirectory(value string, spa bool) (Directory, error) {
	mount, directory := "/", value
	if strings.HasPrefix(value, "/") && strings.Contains(value, "=") {
		mount, directory, _ = strings.Cut(value, "=")
	}
	mount, err := normalizeMount(mount)
	if err != nil {
		return Directory{}, err
	}
	if directory == "" {
		return Directory{}, fmt.Errorf("empty directory")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return Directory{}, fmt.Errorf("resolve directory %q: %w", directory, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Directory{}, fmt.Errorf("directory %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return Directory{}, fmt.Errorf("not a directory: %q", absolute)
	}
	return Directory{Mount: mount, Path: absolute, SPA: spa}, nil
}

func normalizeMount(value string) (string, error) {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\%?#") || strings.Contains(value, "//") || hasWhitespaceOrControl(value) {
		return "", fmt.Errorf("invalid mount %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("invalid mount %q", value)
		}
	}
	if value != "/" {
		value = strings.TrimSuffix(value, "/")
	}
	return value, nil
}

func parseProxy(value string, websocket bool) (Proxy, error) {
	mount, target, ok := strings.Cut(value, "=")
	if !ok {
		return Proxy{}, fmt.Errorf("proxy must be /mount=URL")
	}
	mount, err := normalizeMount(mount)
	if err != nil {
		return Proxy{}, err
	}
	u, err := parseTarget(target)
	if err != nil {
		return Proxy{}, err
	}
	if (!websocket && u.Scheme != "http" && u.Scheme != "https") || (websocket && u.Scheme != "ws" && u.Scheme != "wss") {
		return Proxy{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return Proxy{Mount: mount, Target: u}, nil
}

func parseTarget(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.Opaque != "" || u.User != nil || strings.Contains(value, "#") || hasWhitespaceOrControl(value) {
		return nil, fmt.Errorf("invalid target URL %q", value)
	}
	host, port, err := parseAuthority(u.Host, false)
	if err != nil {
		return nil, err
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return nil, fmt.Errorf("invalid URL query: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = formatAuthority(host, port)
	return u, nil
}

func NormalizeHost(value string) (string, error) {
	host, _, err := parseAuthority(value, true)
	return host, err
}

func NormalizeOrigin(value string) (string, error) {
	if value == "*" {
		return value, nil
	}
	u, err := parseTarget(value)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("invalid Origin %q", value)
	}
	host, port, err := parseAuthority(u.Host, false)
	if err != nil {
		return "", err
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	return u.Scheme + "://" + formatAuthority(host, port), nil
}

func parseAuthority(value string, allowBareIPv6 bool) (string, string, error) {
	invalid := func() (string, string, error) {
		return "", "", fmt.Errorf("invalid host %q", value)
	}
	if value == "" || strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r >= unicode.MaxASCII }) {
		return invalid()
	}
	host, port := value, ""
	if strings.HasPrefix(value, "[") {
		end := strings.IndexByte(value, ']')
		if end < 0 {
			return invalid()
		}
		host = value[1:end]
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return invalid()
		}
		suffix := value[end+1:]
		if suffix != "" {
			if !strings.HasPrefix(suffix, ":") {
				return invalid()
			}
			port = suffix[1:]
			if port == "" {
				return invalid()
			}
		}
	} else {
		switch strings.Count(value, ":") {
		case 0:
		case 1:
			host, port, _ = strings.Cut(value, ":")
			if port == "" {
				return invalid()
			}
		default:
			ip, err := netip.ParseAddr(value)
			if !allowBareIPv6 || err != nil || !ip.Is6() || ip.Zone() != "" {
				return invalid()
			}
		}
	}
	if port != "" {
		var err error
		port, err = parsePort(port)
		if err != nil {
			return invalid()
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return invalid()
		}
		return ip.Unmap().String(), port, nil
	}
	if host == "" || len(host) > 253 || strings.Trim(host, "0123456789.") == "" {
		return invalid()
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid()
		}
		for _, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return invalid()
			}
		}
	}
	return host, port, nil
}

func parsePort(value string) (string, error) {
	for _, c := range value {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("invalid port %q", value)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port %q", value)
	}
	return strconv.Itoa(port), nil
}

func formatAuthority(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func hasWhitespaceOrControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}
