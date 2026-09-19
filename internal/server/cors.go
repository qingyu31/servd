package server

import (
	"net/http"
	"strings"

	"go.qingyu31.com/servd/internal/config"
)

func (s *Server) applyCORS(h http.Header, r *http.Request) bool {
	if len(s.origins) == 0 {
		return false
	}
	if !s.origins["*"] {
		addVary(h, "Origin")
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "*" || len(r.Header.Values("Origin")) != 1 {
		return false
	}
	normalized, err := config.NormalizeOrigin(origin)
	if err != nil && !(s.origins["*"] && origin == "null") {
		return false
	}
	if !s.origins["*"] && !s.origins[normalized] {
		return false
	}
	if s.origins["*"] {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	return true
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request, m *mount) bool {
	if len(s.origins) == 0 || r.Method != http.MethodOptions || r.Header.Get("Origin") == "" || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	addVary(w.Header(), "Access-Control-Request-Method")
	addVary(w.Header(), "Access-Control-Request-Headers")
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		writeError(w, http.StatusForbidden)
		return true
	}
	method := r.Header.Get("Access-Control-Request-Method")
	if !httpToken(method) {
		writeError(w, http.StatusBadRequest)
		return true
	}
	if m.proxy == nil && (len(m.directories) == 0 || (method != http.MethodGet && method != http.MethodHead)) {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed)
		return true
	}
	requested := strings.Join(r.Header.Values("Access-Control-Request-Headers"), ",")
	var headers []string
	if requested != "" {
		for _, name := range strings.Split(requested, ",") {
			name = strings.TrimSpace(name)
			if !httpToken(name) {
				writeError(w, http.StatusBadRequest)
				return true
			}
			headers = append(headers, name)
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", method)
	if len(headers) > 0 {
		w.Header().Set("Access-Control-Allow-Headers", strings.Join(headers, ", "))
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

func httpToken(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}
