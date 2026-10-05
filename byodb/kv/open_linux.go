//go:build linux

package kv

import (
	"os"
	"path/filepath"
	"syscall"
)

// openInDirectory opens the file relative to an already-open directory handle,
// so the open cannot be redirected by the directory being replaced in between.
//
// openat is Linux-specific in the standard library; see open_other.go for the
// plain version other systems use.
func openInDirectory(dir *os.File, path string) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), filepath.Base(path), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
