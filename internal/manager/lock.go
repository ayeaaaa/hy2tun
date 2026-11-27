package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrStateLocked reports that another process holds the manager state lock.
// The running manager holds it for its entire lifetime; reset-admin requires
// the service to be stopped first.
var ErrStateLocked = errors.New("manager state is locked by a running manager")

// StateLock is an exclusive advisory lock on state.json's sibling lock file.
// It is held by serveManager from startup until shutdown and briefly by
// reset-admin while it rewrites the administrator hash.
type StateLock struct {
	file *os.File
	path string
}

// AcquireStateLock takes a non-blocking exclusive lock next to statePath. A
// second process, including a reset-admin invocation while the service runs,
// fails immediately instead of writing a state file the live manager would
// later overwrite from its stale in-memory snapshot.
func AcquireStateLock(statePath string) (*StateLock, error) {
	if strings.TrimSpace(statePath) == "" {
		return nil, fmt.Errorf("manager state path is empty")
	}
	lockPath := statePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create state lock directory for %s: %w", lockPath, err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open manager state lock %s: %w", lockPath, err)
	}
	if err := lockFileExclusive(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("%w: stop the hy2tun-manager service (or any other manager using %s), run the command again, then start the service", ErrStateLocked, statePath)
		}
		return nil, fmt.Errorf("lock manager state %s: %w", statePath, err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
		_ = file.Sync()
	}
	return &StateLock{file: file, path: lockPath}, nil
}

// Close releases the state lock. It is safe to call on a nil or already
// closed lock.
func (l *StateLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
