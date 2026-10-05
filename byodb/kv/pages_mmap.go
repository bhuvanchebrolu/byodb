//go:build !windows && !byodb_pread

package kv

import (
	"fmt"
	"os"
	"syscall"
)

// This is the memory-mapped page backend, used everywhere except Windows.
//
// Reading through a mapping means the OS handles the IO and the caching, and a
// page read costs a pointer dereference rather than a system call. The
// trade-off is that a read error arrives as SIGBUS and kills the process
// instead of returning an error. See pages_pread.go for the portable backend,
// which makes the opposite trade.
//
// Build with `-tags byodb_pread` to use that backend here, which is how its
// behavior is tested on a platform that has mmap.

// initialMapSize is the first mapping. Mapping generously up front, and
// doubling after that, keeps the number of mappings small — the OS has to
// track each one — and costs nothing while the address space is untouched.
const initialMapSize = 64 << 20

// backendName identifies the backend in errors and diagnostics.
const backendName = "mmap"

// pageBackend serves committed pages out of one or more file mappings.
type pageBackend struct {
	file   *os.File
	total  int      // bytes mapped, across all chunks
	chunks [][]byte // separate mappings, in page order
}

func newPageBackend(file *os.File) *pageBackend {
	return &pageBackend{file: file}
}

// grow extends the mapping to cover at least size bytes.
//
// Rather than remapping — which can move the base address and would disturb
// readers holding pointers into it — each extension adds a new chunk. The
// chunks double in size so their number stays small, and a mapping is allowed
// to extend past the end of the file, since untouched address space costs
// nothing.
func (b *pageBackend) grow(size int) error {
	if size <= b.total {
		return nil
	}
	alloc := max(b.total, initialMapSize)
	for b.total+alloc < size {
		alloc *= 2
	}
	chunk, err := syscall.Mmap(
		int(b.file.Fd()), int64(b.total), alloc,
		syscall.PROT_READ, syscall.MAP_SHARED,
	)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}
	b.total += alloc
	b.chunks = append(b.chunks, chunk)
	return nil
}

// readPage returns the page at ptr. The slice points into the mapping, so it
// stays valid until the page is reused.
func (b *pageBackend) readPage(ptr uint64) []byte {
	return readFromChunks(b.chunks, ptr)
}

// ready reports whether any page can be read yet. A mapping of a zero-length
// file is not possible, so an empty database has no chunks.
func (b *pageBackend) ready() bool { return len(b.chunks) > 0 }

// snapshot returns a reader that keeps working while the backend grows.
//
// The chunk list is append-only, so a shallow copy is enough: a committing
// writer appending to the live list cannot disturb a reader holding this one.
func (b *pageBackend) snapshot() func(uint64) []byte {
	chunks := make([][]byte, len(b.chunks))
	copy(chunks, b.chunks)
	return func(ptr uint64) []byte { return readFromChunks(chunks, ptr) }
}

// invalidate is a no-op: writes go through the same page cache the mapping
// reads from, so the mapping already reflects them.
func (b *pageBackend) invalidate(uint64) {}

func (b *pageBackend) close() error {
	for _, chunk := range b.chunks {
		if err := syscall.Munmap(chunk); err != nil {
			return fmt.Errorf("munmap: %w", err)
		}
	}
	b.chunks, b.total = nil, 0
	return nil
}

// readFromChunks locates a page across the mapping chunks.
func readFromChunks(chunks [][]byte, ptr uint64) []byte {
	start := uint64(0)
	for _, chunk := range chunks {
		end := start + uint64(len(chunk))/PageSize
		if ptr < end {
			offset := PageSize * (ptr - start)
			return chunk[offset : offset+PageSize]
		}
		start = end
	}
	panic(fmt.Sprintf("byodb: page %d is outside the memory mapping", ptr))
}
