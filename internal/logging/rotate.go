package logging

import (
	"fmt"
	"os"
	"sync"
)

const (
	defaultRotateSize    = 10 << 20 // 10 MiB
	defaultRotateBackups = 5
)

// rotatingWriter is a small size-based log writer. Rotation keeps the current
// file plus N numbered backups. All writes are serialized so concurrent slog
// records cannot interleave a rotation.
type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	file       *os.File
	size       int64
	maxSize    int64
	maxBackups int
}

func newRotatingWriter(path string, maxSize int64, maxBackups int) (*rotatingWriter, error) {
	if path == "" {
		return nil, fmt.Errorf("log file path is empty")
	}
	if maxSize <= 0 {
		maxSize = defaultRotateSize
	}
	if maxBackups <= 0 {
		maxBackups = defaultRotateBackups
	}
	w := &rotatingWriter{path: path, maxSize: maxSize, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	for i := w.maxBackups - 1; i >= 1; i-- {
		oldPath := fmt.Sprintf("%s.%d", w.path, i)
		newPath := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Remove(newPath)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = os.Remove(w.path + ".1")
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	w.size = 0
	return w.open()
}

// Close closes the current log file.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
