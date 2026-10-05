// Package kv implements a durable, crash-safe key-value store: a copy-on-write
// B+tree backed by a single file.
//
// The file is an array of fixed-size pages. Page 0 is the meta page and holds
// the pointer to the current root; every other page is a B+tree node. Updates
// append new pages and then move the root pointer, so a reachable page is
// never overwritten and the committed version of the database changes in one
// atomic step.
//
// This stage is append-only — space from superseded pages is not yet reclaimed
// — and assumes a single thread of access.
package kv

import (
	"fmt"
	"os"
	"slices"
	"sync"

	"byodb/btree"
	"byodb/internal/assert"
)

// PageSize is the size of every page in the file.
const PageSize = btree.PageSize

// KV is a durable key-value store.
type KV struct {
	path string
	file *os.File
	io   fileIO
	tree *btree.BTree

	// pages serves committed pages. Which implementation this is depends on
	// the platform: a memory mapping where one is available, explicit reads
	// otherwise. See pages_mmap.go and pages_pread.go.
	pages *pageBackend

	// free hands out pages that copy-on-write has orphaned.
	free freeList

	// page tracks committed pages and the pages staged by the update in
	// progress.
	//
	// Staged pages are a map rather than a list now, because a reused page is
	// written back at its own page number rather than appended at the end.
	page struct {
		flushed  uint64            // pages belonging to the committed version
		appended uint64            // pages staged beyond the committed end
		updates  map[uint64][]byte // staged pages, by page number

		// born tracks pages allocated during the update in progress, and
		// reusable holds those of them that have since been discarded.
		//
		// A multi-key update rewrites the same nodes over and over: inserting
		// the second key copies the path the first key just created, and so
		// on. Those intermediate copies were never part of any committed
		// version, so they can be reclaimed the moment they are dropped
		// instead of accumulating until the commit. Without this, staged
		// memory grows with the amount of *work* an update does rather than
		// the data it leaves behind, and a bulk load exhausts memory.
		born     map[uint64]bool
		reusable []uint64
	}

	// generation is the commit counter of the currently committed version. It
	// decides which meta slot the next commit writes into.
	generation uint64

	// failed records that a previous update left the on-disk meta page in an
	// unknown state, so it must be rewritten before anything is appended.
	failed bool

	// mutex serializes transaction lifecycle steps. Reads and writes inside a
	// transaction do not take it: writes only touch the transaction's own
	// buffers, and reads only touch the page backend, so they can run in
	// parallel — which matters because a read can block on IO.
	mutex sync.Mutex

	// version is the logical timestamp of the committed state. It orders
	// snapshots, and tells the free list which freed pages a live reader
	// might still reach.
	version uint64

	// ongoing holds the base versions of transactions currently in flight.
	ongoing []uint64

	// history records what recently committed transactions wrote, so an
	// in-flight transaction can check whether its reads still hold.
	history []committedTX
}

// Open opens or creates a database at path.
func Open(path string) (*KV, error) { return openWith(path, nil) }

// openWith allows tests to wrap the write path for fault injection. wrap
// receives the real write path and returns the one the database should use.
func openWith(path string, wrap func(fileIO) fileIO) (*KV, error) {
	file, err := createFileSync(path)
	if err != nil {
		return nil, err
	}
	db := &KV{path: path, file: file, io: osFile{file: file}}
	db.pages = newPageBackend(file)
	db.page.updates = map[uint64][]byte{}
	db.page.born = map[uint64]bool{}
	if wrap != nil {
		db.io = wrap(db.io)
	}

	if err := db.recover(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// recover reads the committed state from the meta page and rebuilds the tree
// over it. Everything past the committed page count is garbage from an update
// that never finished, and is simply ignored — the next append overwrites it.
func (db *KV) recover() error {
	info, err := db.file.Stat()
	if err != nil {
		return fmt.Errorf("stat database file: %w", err)
	}
	size := info.Size()

	if err := db.pages.grow(int(size)); err != nil {
		return err
	}

	// A fresh database reserves page 0 for the meta page and page 1 for the
	// free list's first node. The list is never allowed to be empty, so it
	// starts with one node rather than none.
	committed := meta{root: 0, pageCount: 2, freeHead: 1, freeTail: 1}
	freshDatabase := true
	if size >= metaSize && db.pages.ready() {
		parsed, fresh, err := readMetaPage(db.pages.readPage(0))
		if err != nil {
			return err
		}
		if !fresh {
			if want := int64(parsed.pageCount) * PageSize; size < want {
				return fmt.Errorf("%w: meta page commits %d pages (%d bytes) "+
					"but the file is only %d bytes", ErrCorrupt, parsed.pageCount, want, size)
			}
			committed = parsed
			freshDatabase = false
		}
	}

	db.page.flushed = committed.pageCount
	db.version = committed.version
	if err := db.pages.grow(int(committed.pageCount) * PageSize); err != nil {
		return err
	}

	db.free = freeList{
		readPage:   db.pageRead,
		appendPage: db.pageAppend,
		writePage:  db.pageWrite,
	}
	db.restoreMeta(committed)

	if freshDatabase {
		// Stage the free list's first node so it is written by the first
		// commit. Without this, page 1 would only ever exist as a hole in a
		// sparse file — readable, but only by accident.
		db.page.updates[committed.freeHead] = make([]byte, PageSize)
	}

	// The tree allocates through the free list and frees into it. Those two
	// callbacks are the whole change from the append-only stage.
	db.tree = btree.New(committed.root, db.pageRead, db.pageAlloc, db.pageFree)
	return nil
}

// Close releases the file and any mapping of it.
func (db *KV) Close() error {
	if err := db.pages.close(); err != nil {
		return err
	}
	return db.file.Close()
}

// Get looks up a key. It is shorthand for a read-only transaction.
func (db *KV) Get(key []byte) ([]byte, bool) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	val, ok := db.tree.Get(key)
	if ok {
		// The value may point into storage the backend owns, which a later
		// update can invalidate, so callers get their own copy.
		val = append([]byte(nil), val...)
	}
	return val, ok
}

// Set inserts or replaces a key, and returns only once the change is durable.
// It is shorthand for a single-key transaction.
func (db *KV) Set(key, val []byte) error {
	var tx KVTX
	db.Begin(&tx)
	if err := tx.Update(&UpdateReq{Key: key, Val: val, Mode: ModeUpsert}); err != nil {
		db.Abort(&tx)
		return err
	}
	return db.Commit(&tx)
}

// Del removes a key, reporting whether it was present. The removal is durable
// on return.
func (db *KV) Del(key []byte) (bool, error) {
	var tx KVTX
	db.Begin(&tx)
	deleted, err := tx.Del(&DeleteReq{Key: key})
	if err != nil {
		db.Abort(&tx)
		return false, err
	}
	if !deleted {
		db.Abort(&tx) // nothing changed, so nothing to persist
		return false, nil
	}
	return true, db.Commit(&tx)
}

func (db *KV) currentMeta() meta {
	return meta{
		generation:  db.generation,
		version:     db.version,
		root:        db.tree.Root(),
		pageCount:   db.page.flushed,
		freeHead:    db.free.headPage,
		freeHeadSeq: db.free.headSeq,
		freeTail:    db.free.tailPage,
		freeTailSeq: db.free.tailSeq,
	}
}

// restoreMeta puts the in-memory state back to a committed version. The free
// list has to move with the root: they are two halves of one version.
func (db *KV) restoreMeta(m meta) {
	if db.tree != nil {
		db.tree.SetRoot(m.root)
	}
	db.page.flushed = m.pageCount
	db.free.headPage = m.freeHead
	db.free.headSeq = m.freeHeadSeq
	db.free.tailPage = m.freeTail
	db.free.tailSeq = m.freeTailSeq
	db.free.maxSeq = m.freeTailSeq
	db.generation = m.generation
	db.version = m.version
}

func (db *KV) discardStaged() {
	clear(db.page.updates)
	clear(db.page.born)
	db.page.reusable = db.page.reusable[:0]
	db.page.appended = 0
}

// updateOrRevert commits the staged pages, or puts the in-memory state back
// the way it was if anything fails.
//
// Reverting is safe precisely because we are copy-on-write: the old root and
// everything under it were never touched, so abandoning the new pages
// abandons the whole failed update. It also sidesteps a nasty property of
// failed fsyncs — on some filesystems, reading back after a failed fsync
// returns the data that never reached disk — since we simply never look at
// those pages again.
func (db *KV) updateOrRevert(lastGood meta, nextVersion uint64) error {
	// A previous failure left the on-disk meta page in an unknown state: it
	// may hold the old version or the new one. Appending over the pages of a
	// version the meta page might still point at would corrupt it, so restore
	// a known meta page first.
	if db.failed {
		if err := db.commitMeta(lastGood); err != nil {
			return err
		}
		if err := db.io.Sync(); err != nil {
			return err
		}
		db.failed = false
	}

	db.version = nextVersion
	if err := db.commit(); err != nil {
		db.failed = true
		db.restoreMeta(lastGood)
		db.discardStaged()
		return err
	}
	return nil
}

// commit is the two-phase update that makes the database crash-safe.
//
// The order is the entire point. New pages reach disk and are confirmed there
// before the root pointer that references them moves. A crash in phase 1
// leaves the meta page pointing at the old root, so the appended pages are
// unreachable garbage. A crash in phase 2 leaves either the old or the new
// root. There is no ordering in which a reader can observe a root pointing at
// pages that are not there.
func (db *KV) commit() error {
	// Phase 1: append the new pages.
	if err := db.writeStagedPages(); err != nil {
		return err
	}
	// The barrier: without this fsync the meta write below could reach disk
	// first, since write order is not persistence order.
	if err := db.io.Sync(); err != nil {
		return err
	}
	// Phase 2: move the root pointer. This single small write is what makes
	// the update atomic.
	if err := db.commitMeta(db.currentMeta()); err != nil {
		return err
	}
	return db.io.Sync()
}

// commitMeta writes a new meta record into the slot that is not currently
// live, and adopts its generation. Bumping the generation is what makes the
// new record win on the next open.
func (db *KV) commitMeta(m meta) error {
	m.generation = db.generation + 1
	if err := db.io.WriteAt(m.encode(), metaSlotOffsetFor(m.slot())); err != nil {
		return fmt.Errorf("write meta page: %w", err)
	}
	db.pages.invalidate(0) // page 0 now differs from any cached copy
	db.generation = m.generation
	return nil
}

// writeStagedPages writes every page staged by the current update, then marks
// them committed.
//
// Staged pages are no longer a contiguous run at the end of the file — reused
// pages sit anywhere — so they are sorted and written as contiguous batches to
// keep the number of write calls down.
func (db *KV) writeStagedPages() error {
	if len(db.page.updates) == 0 {
		return nil
	}
	end := int(db.page.flushed+db.page.appended) * PageSize
	if err := db.pages.grow(end); err != nil {
		return err
	}

	ptrs := make([]uint64, 0, len(db.page.updates))
	for ptr := range db.page.updates {
		ptrs = append(ptrs, ptr)
	}
	slices.Sort(ptrs)

	for start := 0; start < len(ptrs); {
		stop := start + 1
		for stop < len(ptrs) && ptrs[stop] == ptrs[stop-1]+1 {
			stop++
		}
		batch := make([]byte, 0, (stop-start)*PageSize)
		for _, ptr := range ptrs[start:stop] {
			page := db.page.updates[ptr]
			assert.True(len(page) == PageSize,
				"staged page %d is %d bytes, expected %d", ptr, len(page), PageSize)
			batch = append(batch, page...)
		}
		if err := db.io.WriteAt(batch, int64(ptrs[start])*PageSize); err != nil {
			return fmt.Errorf("write pages %d..%d: %w", ptrs[start], ptrs[stop-1], err)
		}
		// A page written in place — a reused page, or a free list node — may
		// be sitting in the backend's cache from before the write.
		for _, ptr := range ptrs[start:stop] {
			db.pages.invalidate(ptr)
		}
		start = stop
	}

	db.page.flushed += db.page.appended
	db.page.appended = 0
	clear(db.page.updates)
	clear(db.page.born)
	return nil
}

// pageRead implements the tree's read callback.
//
// Staged pages are consulted first: with pages being reused in place, a page
// can be read back after it has been modified within the same update, and the
// file still holds the old contents.
func (db *KV) pageRead(ptr uint64) []byte {
	if page, staged := db.page.updates[ptr]; staged {
		return page
	}
	return db.pageReadFile(ptr)
}

// pageAlloc implements the tree's allocate callback.
//
// Pages discarded earlier in this same update come first: they cost nothing to
// take back, and taking them keeps the update's memory bounded. Then the free
// list, and only then the end of the file.
func (db *KV) pageAlloc(page []byte) uint64 {
	if n := len(db.page.reusable); n > 0 {
		ptr := db.page.reusable[n-1]
		db.page.reusable = db.page.reusable[:n-1]
		db.page.updates[ptr] = page
		db.page.born[ptr] = true
		return ptr
	}
	if ptr := db.free.pop(); ptr != 0 {
		db.page.updates[ptr] = page
		db.page.born[ptr] = true
		return ptr
	}
	return db.pageAppend(page)
}

// pageFree implements the tree's deallocate callback.
//
// A page created and dropped within this same update was never referenced by a
// committed version, so it can be reclaimed immediately. Anything older goes
// to the free list, where the version stamp keeps it from being handed out
// while a reader can still reach it.
func (db *KV) pageFree(ptr uint64) {
	if db.page.born[ptr] {
		delete(db.page.born, ptr)
		delete(db.page.updates, ptr)
		db.page.reusable = append(db.page.reusable, ptr)
		return
	}
	db.free.push(ptr)
}

// reclaimUnusedPages hands back any page numbers the update took from the end
// of the file but did not end up using.
//
// They cannot simply be forgotten: the committed page count will include them,
// so a page left neither referenced nor free would be lost space. Giving them
// to the free list keeps every page accounted for.
func (db *KV) reclaimUnusedPages() {
	for len(db.page.reusable) > 0 {
		ptr := db.page.reusable[len(db.page.reusable)-1]
		db.page.reusable = db.page.reusable[:len(db.page.reusable)-1]
		// The page must still be written, or the committed file would have a
		// hole where the free list says a page lives.
		db.page.updates[ptr] = make([]byte, PageSize)
		delete(db.page.born, ptr)
		db.free.push(ptr)
	}
}

// pageWrite returns a writable copy of an existing page, staging it.
//
// The free list needs this because its nodes are updated in place — the one
// place besides the meta page where that happens. It is safe because a list
// node only ever has previously-unused bytes filled in, so an interrupted
// update leaves the bytes the committed version reads untouched.
func (db *KV) pageWrite(ptr uint64) []byte {
	if page, staged := db.page.updates[ptr]; staged {
		return page
	}
	page := make([]byte, PageSize)
	copy(page, db.pageReadFile(ptr))
	db.page.updates[ptr] = page
	return page
}

// pageReadFile reads a committed page through the platform's backend.
func (db *KV) pageReadFile(ptr uint64) []byte {
	return db.pages.readPage(ptr)
}

// pageAppend stages a brand new page at the end of the file.
func (db *KV) pageAppend(page []byte) uint64 {
	ptr := db.page.flushed + db.page.appended
	db.page.appended++
	db.page.updates[ptr] = page
	db.page.born[ptr] = true
	return ptr
}
