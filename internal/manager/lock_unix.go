//go:build !windows

package manager

import (
	"errors"
	"os"
	"syscall"
)

var errLockBusy = syscall.EWOULDBLOCK

// lockFileExclusive takes a non-blocking exclusive flock. flock is advisory,
// which is sufficient here because every manager writer goes through this
// package.
func lockFileExclusive(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	if errors.Is(err, syscall.EBADF) {
		return nil
	}
	return err
}
