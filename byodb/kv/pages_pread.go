//go:build windows || byodb_pread

package kv

import (
	"fmt"
	"os"
	"sync"
)

// This is the portable page backend: pages are read with pread (ReadAt) rather
// than through a memory mapping.
//
// It exists because Windows has no mmap with the same semantics, and because
// mmap turns a disk read error into SIGBUS, which kills the process instead of
// returning an error. Reading explicitly costs a system call per page miss,
// which is why a small cache sits in front of it.
//
// The build tag means this backend can be selected on any platform with
// `-tags byodb_pread`. That matters: it is how the code Windows will run gets
// exercised by the full test suite on a machine that is not Windows.

// backendName identifies the backend in errors and diagnostics.
const backendName = "pread"

// cachedPages bounds the read cache. Pages are 4 KiB, so this is about 32 MiB.
//
// The cache exists because a B+tree lookup re-reads the same handful of upper
// nodes on every descent: without it, each lookup in a three-level tree costs
// three system calls instead of, typically, one.
const cachedPages = 8192

// pageBackend serves committed pages by reading them from the file.
type pageBackend struct {
	file *os.File

	mu    sync.Mutex
	cache map[uint64][]byte
	// order records insertion order so the cache can be trimmed. It is a
	// plain FIFO rather than an LRU: tree lookups touch upper nodes far more
	// often than leaves, so those stay resident either way, and a FIFO needs
	// no bookkeeping on the read path.
	order []uint64
}

func newPageBackend(file *os.File) *pageBackend {
	return &pageBackend{file: file, cache: make(map[uint64][]byte, cachedPages)}
}

// grow is a no-op: there is no mapping to extend, and the file grows when it
// is written to.
func (b *pageBackend) grow(int) error { return nil }

// ready reports whether any page can be read. Reads go straight to the file,
// so the backend is usable as soon as it exists.
func (b *pageBackend) ready() bool { return true }

// readPage returns the page at ptr.
//
// Unlike the mapped backend, the returned slice is this backend's own memory
// rather than a view of the file. Callers already treat pages as read-only, so
// the distinction does not change how they are used.
//
// A read error panics rather than returning an error, matching what the mapped
// backend does by way of SIGBUS. It keeps the page-read callback signature the
// tree is built around, and an unreadable page mid-descent is not a condition
// the tree can carry on from.
func (b *pageBackend) readPage(ptr uint64) []byte {
	b.mu.Lock()
	if page, ok := b.cache[ptr]; ok {
		b.mu.Unlock()
		return page
	}
	b.mu.Unlock()

	page := make([]byte, PageSize)
	if _, err := b.file.ReadAt(page, int64(ptr)*PageSize); err != nil {
		panic(fmt.Sprintf("byodb: reading page %d: %v", ptr, err))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// Another goroutine may have cached it meanwhile; either copy is correct,
	// so keep the one already there to avoid handing out two.
	if existing, ok := b.cache[ptr]; ok {
		return existing
	}
	if len(b.order) >= cachedPages {
		delete(b.cache, b.order[0])
		b.order = b.order[1:]
	}
	b.cache[ptr] = page
	b.order = append(b.order, ptr)
	return page
}

// invalidate drops a page from the cache. A page that is rewritten in place —
// which happens when the free list hands one out again — must not be served
// from a stale copy.
func (b *pageBackend) invalidate(ptr uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cache[ptr]; !ok {
		return
	}
	delete(b.cache, ptr)
	for i, cached := range b.order {
		if cached == ptr {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
}

// snapshot returns a reader for a transaction.
//
// Committed pages are stable for as long as a transaction can reach them —
// that is what the free list's version stamps guarantee — so a snapshot needs
// no copy of anything and simply reads through.
func (b *pageBackend) snapshot() func(uint64) []byte { return b.readPage }

func (b *pageBackend) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache, b.order = nil, nil
	return nil
}
