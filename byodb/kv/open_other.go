//go:build !windows && !linux

package kv

import "os"

// openInDirectory opens the file by path. The directory handle is unused here:
// openat is not portable across the BSDs and macOS in the standard library, so
// these systems give up the (minor) protection against the directory being
// swapped mid-open. See open_linux.go.
func openInDirectory(_ *os.File, path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}
