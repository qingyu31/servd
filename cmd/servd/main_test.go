package main

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"go.qingyu31.com/servd/internal/app"
)

func TestRunHelpAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"help", []string{"--help"}, "Usage: servd"},
		{"short help", []string{"-h"}, "Usage: servd"},
		{"version", []string{"--version"}, app.Version + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Errorf("stdout = %q, missing %q", stdout.String(), tc.want)
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected stderr = %q", stderr.String())
			}
		})
	}
}

func TestRunReportsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown-option"}, {"--port=0"}, {"--port=65536"}, {"--port=abc"}, {"unexpected-positional"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Errorf("stdout = %q, stderr = %q; expected error on stderr only", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunReportsListenFailure(t *testing.T) {
	// Reserve the wildcard listener to avoid ephemeral-port allocation races.
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--port=" + port, "--static=" + t.TempDir()}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 || stderr.Len() == 0 {
		t.Errorf("stdout = %q, stderr = %q; expected bind error on stderr only", stdout.String(), stderr.String())
	}
}
