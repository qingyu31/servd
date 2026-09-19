package server

import (
	"compress/gzip"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type representation struct {
	file    *os.File
	info    fs.FileInfo
	coding  string
	dynamic bool
}

func serveFile(w http.ResponseWriter, r *http.Request, dir directory, name string, file *os.File, info fs.FileInfo) {
	mediaType := contentType(file, name)
	available := []representation{{file: file, info: info, coding: "identity"}}
	for _, coding := range []string{"gzip", "br"} {
		extension := ".gz"
		if coding == "br" {
			extension = ".br"
		}
		compressed, compressedInfo, err := openFile(dir.root, name+extension)
		if err == nil {
			defer compressed.Close()
			if compressedInfo.Mode().IsRegular() && !compressedInfo.ModTime().Before(info.ModTime()) {
				available = append(available, representation{file: compressed, info: compressedInfo, coding: coding})
				continue
			}
		}
		if coding == "gzip" && compressible(mediaType) {
			available = append(available, representation{file: file, info: info, coding: "gzip", dynamic: true})
		}
	}
	addVary(w.Header(), "Accept-Encoding")
	selected, ok := chooseRepresentation(available, strings.Join(r.Header.Values("Accept-Encoding"), ","))
	if !ok {
		writeError(w, http.StatusNotAcceptable)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	if dir.spa && strings.HasPrefix(mediaType, "text/html") {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("ETag", fmt.Sprintf("W/\"%x-%x-%s-%t\"", selected.info.Size(), selected.info.ModTime().UnixNano(), selected.coding, selected.dynamic))
	if selected.coding != "identity" {
		w.Header().Set("Content-Encoding", selected.coding)
		if !selected.dynamic {
			w.Header().Set("Content-Length", strconv.FormatInt(selected.info.Size(), 10))
		}
	}
	request := r
	if selected.dynamic || (selected.coding != "identity" && strings.Contains(r.Header.Get("Range"), ",")) {
		// Encoded multipart ranges would encode the boundary framing incorrectly.
		request = r.Clone(r.Context())
		request.Header.Del("Range")
		request.Header.Del("If-Range")
	}
	writer := &fileWriter{ResponseWriter: w, dynamic: selected.dynamic, head: r.Method == http.MethodHead}
	http.ServeContent(writer, request, name, selected.info.ModTime(), selected.file)
	if writer.gzip != nil {
		writer.gzip.Close()
	}
}

func compressible(contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return strings.HasPrefix(mediaType, "text/") || strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") ||
		mediaType == "application/json" || mediaType == "application/javascript" || mediaType == "application/xml" ||
		mediaType == "application/wasm" || mediaType == "image/svg+xml"
}

func chooseRepresentation(available []representation, value string) (representation, bool) {
	weights := make(map[string]float64)
	for _, item := range strings.Split(value, ",") {
		coding, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		weight := 1.0
		if q, ok := params["q"]; ok {
			weight, err = strconv.ParseFloat(q, 64)
			if err != nil || !(weight >= 0 && weight <= 1) {
				weight = 0
			}
		}
		weights[strings.ToLower(coding)] = weight
	}
	quality := func(coding string) float64 {
		if q, ok := weights[coding]; ok {
			return q
		}
		if coding == "identity" {
			if q, ok := weights["*"]; ok && q == 0 {
				return 0
			}
			return 1
		}
		return weights["*"]
	}
	var best representation
	bestQuality := 0.0
	priority := map[string]int{"identity": 0, "gzip": 1, "br": 2}
	for _, candidate := range available {
		q := quality(candidate.coding)
		if q > bestQuality || (q > 0 && q == bestQuality && priority[candidate.coding] > priority[best.coding]) {
			best, bestQuality = candidate, q
		}
	}
	return best, bestQuality > 0
}

type fileWriter struct {
	http.ResponseWriter
	dynamic bool
	head    bool
	status  int
	gzip    *gzip.Writer
}

func (w *fileWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status >= 400 {
		w.Header().Set("Cache-Control", "no-store")
		for _, name := range []string{"Content-Encoding", "Content-Length", "ETag", "Last-Modified"} {
			w.Header().Del(name)
		}
	}
	if w.dynamic {
		w.Header().Del("Content-Length")
		w.Header().Del("Accept-Ranges")
	}
	w.ResponseWriter.WriteHeader(status)
	if w.dynamic && status == http.StatusOK && !w.head {
		w.gzip, _ = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
	}
}

func (w *fileWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.head || w.status == http.StatusNotModified {
		return len(data), nil
	}
	if w.dynamic && w.status == http.StatusOK {
		return w.gzip.Write(data)
	}
	return w.ResponseWriter.Write(data)
}
