//go:build !windows

package kv

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// createFileSync opens (creating if necessary) the database file, makes the
// directory entry itself durable, and claims the file for this process.
//
// A file's data and the directory that names it are two separate things to
// persist. Without the fsync on the parent directory, a crash just after
// creation can leave a durable file with no name pointing at it — the same
// gotcha that makes atomic rename insufficient on its own.
func createFileSync(path string) (*os.File, error) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open parent directory: %w", err)
	}
	defer dir.Close()

	file, err := openInDirectory(dir, path)
	if err != nil {
		return nil, fmt.Errorf("open database file: %w", err)
	}

	if err := dir.Sync(); err != nil {
		_ = file.Close() // may leave an empty file behind, which is harmless
		return nil, fmt.Errorf("fsync parent directory: %w", err)
	}

	// Claim the file for this process before anything reads it.
	//
	// Everything about this design assumes one process: the page cache, the
	// free list's notion of which versions are live, and the conflict history
	// are all in memory, so a second process would commit against state the
	// first one knows nothing about. The result is not a crash but silent
	// data loss — the second writer's meta page simply overwrites the first's,
	// and rows that were reported as committed are gone.
	//
	// The lock is advisory, and released automatically when the process exits
	// or the descriptor closes, so a crash never leaves a stale lock behind.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock database file: %w", err)
	}
	return file, nil
}
