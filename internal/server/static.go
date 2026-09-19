package server

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
)

func (m *mount) serveFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed)
		return
	}
	if m.path != "/" && r.URL.Path == m.path {
		redirectDirectory(w, r)
		return
	}
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(m.path, "/")), "/")
	if name == "" {
		name = "."
	}
	if strings.Contains(name, ":") {
		writeError(w, http.StatusBadRequest)
		return
	}
	for _, dir := range m.directories {
		file, info, err := openFile(dir.root, name)
		if err != nil {
			if missingFile(err) {
				continue
			}
			writeError(w, http.StatusForbidden)
			return
		}
		fileName := name
		if info.IsDir() {
			file.Close()
			fileName = path.Join(name, "index.html")
			file, info, err = openFile(dir.root, fileName)
			if err != nil {
				if missingFile(err) {
					continue
				}
				writeError(w, http.StatusForbidden)
				return
			}
			if !strings.HasSuffix(r.URL.Path, "/") {
				file.Close()
				redirectDirectory(w, r)
				return
			}
		}
		if !info.Mode().IsRegular() {
			file.Close()
			writeError(w, http.StatusForbidden)
			return
		}
		defer file.Close()
		serveFile(w, r, dir, fileName, file, info)
		return
	}
	if path.Ext(strings.TrimSuffix(r.URL.Path, "/")) == "" && acceptsHTML(r) {
		for _, dir := range m.directories {
			if !dir.spa {
				continue
			}
			file, info, err := openFile(dir.root, "index.html")
			if err != nil {
				if missingFile(err) {
					continue
				}
				writeError(w, http.StatusForbidden)
				return
			}
			defer file.Close()
			if !info.Mode().IsRegular() {
				writeError(w, http.StatusForbidden)
				return
			}
			serveFile(w, r, dir, "index.html", file, info)
			return
		}
	}
	writeError(w, http.StatusNotFound)
}

func openFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	info, err := root.Stat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil, fs.ErrPermission
	}
	// Nonblocking open prevents a file-to-FIFO race from hanging the request.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func missingFile(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func redirectDirectory(w http.ResponseWriter, r *http.Request) {
	target := url.URL{Path: r.URL.Path + "/", RawQuery: r.URL.RawQuery}
	if r.URL.RawPath != "" {
		target.RawPath = r.URL.RawPath + "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
}

func acceptsHTML(r *http.Request) bool {
	for _, entry := range strings.Split(strings.Join(r.Header.Values("Accept"), ","), ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
		if err != nil || mediaType != "text/html" {
			continue
		}
		if q, ok := params["q"]; ok {
			weight, err := strconv.ParseFloat(q, 64)
			if err != nil || !(weight > 0 && weight <= 1) {
				continue
			}
		}
		return true
	}
	return false
}

func contentType(file *os.File, name string) string {
	if value := mime.TypeByExtension(path.Ext(name)); value != "" {
		return value
	}
	var prefix [512]byte
	n, err := file.ReadAt(prefix[:], 0)
	if err != nil && err != io.EOF {
		return "application/octet-stream"
	}
	return http.DetectContentType(prefix[:n])
}
