//go:build windows

package kv

import (
	"fmt"
	"os"
	"syscall"
)

// errSharingViolation is what Windows returns when a file is already open
// without sharing. The standard library does not name it.
const errSharingViolation = syscall.Errno(32)

// createFileSync opens (creating if necessary) the database file and claims it
// for this process.
//
// Windows does both in one step: opening with a share mode of zero means no
// other process can open the file at all, which is exactly the exclusivity
// this design needs. Its page cache, its view of which versions are live and
// its conflict history are all in memory, so a second process would commit
// against state the first knows nothing about and silently destroy its data.
// The claim is released by the OS when the handle closes, so a crash never
// leaves it stuck.
//
// There is no parent-directory fsync here, unlike the Unix path. Windows has
// no equivalent operation on a directory handle, and NTFS journals the
// metadata that creates the directory entry, so flushing the file (which
// os.File.Sync does, via FlushFileBuffers) is what durability requires.
func createFileSync(path string) (*os.File, error) {
	wide, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("open database file: %w", err)
	}

	handle, err := syscall.CreateFile(
		wide,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ, // allow read-only handles (e.g. os.ReadFile), block write access
		nil,                     // default security
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if err == errSharingViolation || err == syscall.ERROR_ACCESS_DENIED {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("open database file: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}
