//go:build windows

package manager

import (
	"os"

	"golang.org/x/sys/windows"
)

var errLockBusy = windows.ERROR_LOCK_VIOLATION

// lockFileExclusive locks the first byte of the lock file. The manager release
// targets are Linux, but the command still builds on Windows for developers,
// and Windows must not silently allow a second manager to write the same
// state file.
func lockFileExclusive(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
}

func unlockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
