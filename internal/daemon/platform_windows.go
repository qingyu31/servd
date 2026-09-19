//go:build windows

package daemon

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var errLocked = errors.New("locked")

// kernel32 LockFileEx provides byte-range locks that the OS releases when the
// owning handle closes, even after process death; the syscall package does
// not wrap it, so bind it directly without external dependencies.
var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	errorLockViolation      = syscall.Errno(33)
)

// acquireLock takes an exclusive non-blocking lock on the first byte of path.
// The lock is held until the returned file is closed; the OS releases it if
// the process dies.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped syscall.Overlapped
	r1, _, callErr := procLockFileEx.Call(
		uintptr(f.Fd()),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		f.Close()
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorLockViolation {
			return nil, errLocked
		}
		if callErr == nil {
			callErr = errors.New("LockFileEx failed")
		}
		return nil, callErr
	}
	return f, nil
}

// detachAttr starts the child detached from the console in its own process
// group so Ctrl+C and console close events cannot reach it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		CreationFlags: 0x00000008 | 0x00000200,
	}
}
