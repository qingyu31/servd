//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

var errLocked = errors.New("locked")

// acquireLock takes an exclusive non-blocking advisory lock on path. The lock
// is held until the returned file is closed; the OS releases it if the
// process dies.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

// detachAttr starts the child in its own session so terminal signals and
// process-group kills cannot reach it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
