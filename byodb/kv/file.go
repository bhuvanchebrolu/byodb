package kv

import (
	"fmt"
	"os"
)

// fileIO is the database's entire write path.
//
// It exists as an interface for one reason: crash testing. A test can
// substitute an implementation that fails or truncates at the Nth write or the
// Nth fsync, which is how we verify that a crash at *any* point in the update
// still leaves a recoverable database. Reads bypass this and go through the
// memory mapping.
type fileIO interface {
	// WriteAt writes buf at the given byte offset, as pwrite(2) does.
	WriteAt(buf []byte, offset int64) error
	// Sync flushes pending writes to the storage device and waits.
	Sync() error
}

// osFile is the real implementation, backed by a file descriptor.
type osFile struct{ file *os.File }

func (f osFile) WriteAt(buf []byte, offset int64) error {
	written, err := f.file.WriteAt(buf, offset)
	if err != nil {
		return err
	}
	if written != len(buf) {
		// WriteAt promises to return an error on a short write, so this is a
		// belt-and-braces check rather than an expected path.
		return fmt.Errorf("short write: %d of %d bytes", written, len(buf))
	}
	return nil
}

func (f osFile) Sync() error { return f.file.Sync() }
