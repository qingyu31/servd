package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogWriterRotatesAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servd.log")
	w, err := openLog(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("a", 80) + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("b", 80) + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != strings.Repeat("a", 80)+"\n" {
		t.Fatalf("backup = %q", backup)
	}
	if string(current) != strings.Repeat("b", 80)+"\n" {
		t.Fatalf("current = %q", current)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("rotated file perms: %v %v", info.Mode().Perm(), err)
	}
}

func TestLogWriterReopensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servd.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := openLog(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.logf("appended %d", 1)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "existing\n") || !strings.HasSuffix(string(data), "appended 1\n") {
		t.Fatalf("unexpected log content: %q", data)
	}
}

func TestLogWriterClosesIdempotently(t *testing.T) {
	w, err := openLog(filepath.Join(t.TempDir(), "servd.log"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after close succeeded")
	}
}
