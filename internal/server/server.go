package server

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sort"
	"sync"
	"time"

	"go.qingyu31.com/servd/internal/config"
)

type directory struct {
	root *os.Root
	spa  bool
}

type mount struct {
	path        string
	directories []directory
	proxy       *httputil.ReverseProxy
	ws          *httputil.ReverseProxy
}

type Server struct {
	mounts    []*mount
	hosts     map[string]bool
	origins   map[string]bool
	transport *http.Transport
	mu        sync.Mutex
	tunnels   map[*tunnelConn]struct{}
	closing   bool
	closeOnce sync.Once
	closeErr  error
}

func New(cfg config.Config) (*Server, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 30 * time.Second
	s := &Server{
		hosts: make(map[string]bool), origins: make(map[string]bool),
		transport: transport, tunnels: make(map[*tunnelConn]struct{}),
	}
	for _, host := range cfg.Hosts {
		s.hosts[host] = true
	}
	for _, origin := range cfg.Origins {
		s.origins[origin] = true
	}
	mounts := make(map[string]*mount)
	getMount := func(path string) *mount {
		if m := mounts[path]; m != nil {
			return m
		}
		m := &mount{path: path}
		mounts[path] = m
		s.mounts = append(s.mounts, m)
		return m
	}
	for _, dir := range cfg.Directories {
		root, err := os.OpenRoot(dir.Path)
		if err != nil {
			s.Close()
			return nil, err
		}
		m := getMount(dir.Mount)
		m.directories = append(m.directories, directory{root: root, spa: dir.SPA})
	}
	for _, proxy := range cfg.Proxies {
		getMount(proxy.Mount).proxy = s.reverseProxy(proxy)
	}
	for _, proxy := range cfg.WebSockets {
		getMount(proxy.Mount).ws = s.reverseProxy(proxy)
	}
	sort.Slice(s.mounts, func(i, j int) bool { return len(s.mounts[i].path) > len(s.mounts[j].path) })
	return s, nil
}

func (s *Server) CloseTunnels() {
	s.mu.Lock()
	s.closing = true
	conns := make([]*tunnelConn, 0, len(s.tunnels))
	for conn := range s.tunnels {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.CloseTunnels()
		s.transport.CloseIdleConnections()
		for _, m := range s.mounts {
			for _, dir := range m.directories {
				s.closeErr = errors.Join(s.closeErr, dir.root.Close())
			}
		}
	})
	return s.closeErr
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}
