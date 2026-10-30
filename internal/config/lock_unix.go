//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

// withConfigLock runs fn while holding an advisory exclusive flock on
// path+".lock". The lock serializes revision checks and writes between
// processes and goroutines. Callback errors are returned unchanged.
func withConfigLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open config lock %s: %w", lockPath, err)
	}
	defer file.Close()

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock config %s: %w", lockPath, err)
	}
	defer func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}()

	return fn()
}
