package daemon

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// logWriter appends to path, rotating to path+".1" once the file exceeds max
// bytes. All writes go through a single mutex so worker pipes and supervisor
// events serialize safely.
type logWriter struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	size    int64
	maxSize int64
}

func openLog(path string, maxSize int64) (*logWriter, error) {
	if maxSize <= 0 {
		maxSize = 5 << 20
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &logWriter{path: path, file: file, size: info.Size(), maxSize: maxSize}, nil
}

func (w *logWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(data)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(data)
	w.size += int64(n)
	return n, err
}

func (w *logWriter) rotateLocked() error {
	w.file.Close()
	_ = os.Remove(w.path + ".1")
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		w.file = nil
		return fmt.Errorf("rotate %s: %w", w.path, err)
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		w.file = nil
		return err
	}
	w.file = file
	w.size = 0
	return nil
}

func (w *logWriter) logf(format string, args ...any) {
	_, _ = w.Write([]byte(time.Now().UTC().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n"))
}

func (w *logWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
