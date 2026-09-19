package server

import (
	"net/http"
	"net/url"
	"strings"

	"go.qingyu31.com/servd/internal/config"
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, err := config.NormalizeHost(r.Host)
	if err != nil {
		writeError(w, http.StatusBadRequest)
		return
	}
	if len(s.hosts) > 0 && !s.hosts[host] {
		writeError(w, http.StatusForbidden)
		return
	}
	s.applyCORS(w.Header(), r)
	if !safePath(r.URL.Path) {
		writeError(w, http.StatusBadRequest)
		return
	}
	var selected *mount
	for _, m := range s.mounts {
		if m.path == "/" || r.URL.Path == m.path || strings.HasPrefix(r.URL.Path, m.path+"/") {
			selected = m
			break
		}
	}
	if selected == nil {
		writeError(w, http.StatusNotFound)
		return
	}
	if s.preflight(w, r, selected) {
		return
	}
	if r.Header.Get("Upgrade") != "" || headerContains(r.Header, "Connection", "upgrade") {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerContains(r.Header, "Connection", "upgrade") {
			writeError(w, http.StatusBadRequest)
			return
		}
		if selected.ws == nil {
			writeError(w, http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet || !validQuery(r) {
			writeError(w, http.StatusBadRequest)
			return
		}
		selected.ws.ServeHTTP(&upgradeWriter{ResponseWriter: w, server: s}, r)
		return
	}
	if selected.proxy != nil {
		if !validQuery(r) {
			writeError(w, http.StatusBadRequest)
			return
		}
		selected.proxy.ServeHTTP(w, r)
		return
	}
	if len(selected.directories) == 0 {
		w.Header().Set("Upgrade", "websocket")
		writeError(w, http.StatusUpgradeRequired)
		return
	}
	selected.serveFiles(w, r)
}

func safePath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") || strings.Contains(path, "//") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validQuery(r *http.Request) bool {
	_, err := url.ParseQuery(r.URL.RawQuery)
	return err == nil
}

func headerContains(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func addVary(h http.Header, token string) {
	if !headerContains(h, "Vary", "*") && !headerContains(h, "Vary", token) {
		h.Add("Vary", token)
	}
}
