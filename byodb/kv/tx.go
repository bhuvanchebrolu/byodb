package kv

import (
	"bytes"
	"errors"
	"fmt"

	"byodb/btree"
)

// Update modes, mirroring the three SQL statements that differ only in how
// they treat an existing row.
const (
	ModeUpsert     = 0 // insert a new key or replace an existing one
	ModeUpdateOnly = 1 // only modify keys that already exist
	ModeInsertOnly = 2 // only add keys that do not exist
)

// Flags prefixed to values held in a transaction's pending tree. Deletions
// have to be recorded rather than applied, so the pending tree needs a way to
// say "this key is gone" that is distinct from "this key is untouched".
const (
	flagDeleted = byte(1)
	flagUpdated = byte(2)
)

// MaxValSize is the largest value a transaction can store. It is one byte
// below the tree's limit because the pending tree prefixes each value with a
// flag byte.
const MaxValSize = btree.MaxValSize - 1

// ErrConflict reports that a transaction read data another transaction has
// since changed. The transaction has been rolled back; the caller may retry.
var ErrConflict = errors.New("kv: transaction conflicts with a committed transaction")

// UpdateReq describes one key update and reports what happened to it.
//
// Old is what makes secondary indexes maintainable: to remove a row's stale
// index entries, the caller needs the values the row had before the update.
type UpdateReq struct {
	Key  []byte
	Val  []byte
	Mode int

	Added   bool   // the key did not exist before
	Updated bool   // the update was applied
	Old     []byte // the previous value, when the key existed
}

// DeleteReq describes one key removal and reports the value removed.
type DeleteReq struct {
	Key []byte
	Old []byte
}

// KeyRange is an inclusive range of keys a transaction read or wrote. It is
// the unit of conflict detection.
type KeyRange struct {
	Start []byte
	Stop  []byte
}

func (r KeyRange) overlaps(other KeyRange) bool {
	return bytes.Compare(r.Start, other.Stop) <= 0 && bytes.Compare(other.Start, r.Stop) <= 0
}

// committedTX records what a committed transaction wrote, so that later
// transactions can check whether their reads have been invalidated.
type committedTX struct {
	version uint64
	writes  []KeyRange
}

// KVTX is a transaction over the key-value store.
//
// It reads from an immutable snapshot and buffers its writes in memory, so it
// neither blocks nor is blocked by other transactions. Conflicts are detected
// at commit time rather than prevented — the optimistic assumption being that
// two transactions rarely touch the same keys.
type KVTX struct {
	db *KV

	// snapshot is the committed tree as it stood when the transaction began.
	// Copy-on-write makes this free: the old root and everything under it
	// stay untouched no matter what other transactions do.
	snapshot *btree.BTree
	version  uint64
	baseMeta meta

	// pending holds this transaction's own writes, in an in-memory tree so
	// they can be read back in order alongside the snapshot.
	pending     *btree.BTree
	pendingPage [][]byte
	pendingFree []uint64 // in-memory pages the pending tree has released

	// reads and writes are the dependency and effect sets used to detect
	// conflicts at commit.
	reads  []KeyRange
	writes []KeyRange

	finished bool
}

// Begin starts a transaction.
func (db *KV) Begin(tx *KVTX) {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	tx.db = db
	tx.version = db.version
	tx.baseMeta = db.currentMeta()
	tx.finished = false
	tx.reads, tx.writes = nil, nil

	// The snapshot reads through the backend's own stable view, so a
	// committing writer cannot disturb it.
	readPage := db.pages.snapshot()
	tx.snapshot = btree.New(db.tree.Root(),
		readPage,
		func([]byte) uint64 { panic("byodb: a snapshot is read-only") },
		func(uint64) { panic("byodb: a snapshot is read-only") },
	)

	// The pending tree lives entirely in memory and is thrown away on abort.
	//
	// Its pages are recycled rather than left to the garbage collector.
	// Copy-on-write replaces a whole root-to-leaf path on every write, so a
	// transaction that writes n keys into a tree of height h abandons about
	// n*h pages; without reuse a bulk load would allocate memory in
	// proportion to the work done rather than the data held.
	tx.pendingPage, tx.pendingFree = nil, nil
	tx.pending = btree.New(0,
		func(ptr uint64) []byte { return tx.pendingPage[ptr-1] },
		func(page []byte) uint64 {
			// Reuse is safe because the tree never reads a page after freeing
			// it: the content it needs is copied into a new buffer first.
			if n := len(tx.pendingFree); n > 0 {
				ptr := tx.pendingFree[n-1]
				tx.pendingFree = tx.pendingFree[:n-1]
				copy(tx.pendingPage[ptr-1], page)
				return ptr
			}
			owned := make([]byte, btree.PageSize)
			copy(owned, page)
			tx.pendingPage = append(tx.pendingPage, owned)
			return uint64(len(tx.pendingPage))
		},
		func(ptr uint64) { tx.pendingFree = append(tx.pendingFree, ptr) },
	)

	db.ongoing = append(db.ongoing, tx.version)
}

// Abort discards the transaction. Nothing was written, so there is nothing to
// undo beyond dropping the buffered state.
func (db *KV) Abort(tx *KVTX) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	db.finishTX(tx)
}

// Commit applies the transaction's writes, or returns ErrConflict if another
// transaction has changed data this one read.
//
// Verification and commit are one atomic step: the lock is held across both so
// that no transaction can commit in between and invalidate the check.
func (db *KV) Commit(tx *KVTX) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	if tx.finished {
		return errors.New("kv: transaction has already finished")
	}
	if len(tx.writes) == 0 {
		db.finishTX(tx) // read-only: nothing to write, nothing to conflict over
		return nil
	}
	if db.detectConflicts(tx) {
		db.finishTX(tx)
		return ErrConflict
	}

	// Replay the buffered writes onto the live tree. Only now does any of
	// this touch pages that a commit will make durable.
	lastGood := db.currentMeta()
	next := db.version + 1
	// The free list must know the versions before any page is freed: each
	// freed page is stamped with the version that freed it.
	db.free.beginUpdate(next, db.oldestLiveVersion())

	if err := tx.applyPending(); err != nil {
		db.restoreMeta(lastGood)
		db.discardStaged()
		db.finishTX(tx)
		return err
	}
	db.reclaimUnusedPages()

	if err := db.updateOrRevert(lastGood, next); err != nil {
		db.finishTX(tx)
		return err
	}

	db.history = append(db.history, committedTX{version: next, writes: tx.writes})
	db.finishTX(tx)
	return nil
}

// applyPending walks the pending tree and replays each buffered write onto the
// live tree.
func (tx *KVTX) applyPending() error {
	for iter := tx.pending.SeekLE(nil); iter.Valid(); iter.Next() {
		key, val := iter.Deref()
		if len(key) == 0 {
			continue // the pending tree's sentinel
		}
		switch val[0] {
		case flagUpdated:
			if err := tx.db.tree.Insert(key, val[1:]); err != nil {
				return err
			}
		case flagDeleted:
			if _, err := tx.db.tree.Delete(key); err != nil {
				return err
			}
		default:
			return fmt.Errorf("kv: pending value for key %q has an unrecognized flag %d", key, val[0])
		}
	}
	return nil
}

// finishTX retires a transaction and reclaims what its departure allows: the
// free list can hand out newer pages, and history older than every live
// transaction can be dropped. The caller must hold the mutex.
func (db *KV) finishTX(tx *KVTX) {
	if tx.finished {
		return
	}
	tx.finished = true
	tx.pending, tx.pendingPage, tx.pendingFree = nil, nil, nil
	tx.snapshot = nil

	for i, v := range db.ongoing {
		if v == tx.version {
			db.ongoing = append(db.ongoing[:i], db.ongoing[i+1:]...)
			break
		}
	}
	db.trimHistory()
}

// oldestLiveVersion is the version of the longest-running transaction, or the
// current version when none is running. Pages freed after it must stay
// reserved, because that transaction can still reach them.
func (db *KV) oldestLiveVersion() uint64 {
	oldest := db.version
	for _, v := range db.ongoing {
		if v < oldest {
			oldest = v
		}
	}
	return oldest
}

// trimHistory drops committed-write records no live transaction could still
// need to check against.
func (db *KV) trimHistory() {
	oldest := db.oldestLiveVersion()
	keep := 0
	for keep < len(db.history) && db.history[keep].version <= oldest {
		keep++
	}
	db.history = db.history[keep:]
}

// detectConflicts reports whether anything this transaction read has been
// written by a transaction that committed after it began.
//
// Walking backwards lets it stop as soon as it reaches the transaction's own
// base version, since history is ordered.
func (db *KV) detectConflicts(tx *KVTX) bool {
	for i := len(db.history) - 1; i >= 0; i-- {
		if db.history[i].version <= tx.version {
			break // everything older is already reflected in the snapshot
		}
		for _, read := range tx.reads {
			for _, write := range db.history[i].writes {
				if read.overlaps(write) {
					return true
				}
			}
		}
	}
	return false
}

// Get reads a key, consulting this transaction's own uncommitted writes first
// so that a transaction can read back what it just wrote.
func (tx *KVTX) Get(key []byte) ([]byte, bool) {
	tx.reads = append(tx.reads, KeyRange{Start: key, Stop: key})
	return tx.get(key)
}

// get is Get without recording a dependency, for internal reads that are
// already covered by the caller's own read record.
func (tx *KVTX) get(key []byte) ([]byte, bool) {
	if val, ok := tx.pending.Get(key); ok {
		if val[0] == flagUpdated {
			return val[1:], true
		}
		return nil, false // deleted in this transaction
	}
	return tx.snapshot.Get(key)
}

// Update applies one key update according to req.Mode.
func (tx *KVTX) Update(req *UpdateReq) error {
	if len(req.Val) > MaxValSize {
		return fmt.Errorf("kv: value of %d bytes exceeds the maximum of %d", len(req.Val), MaxValSize)
	}
	tx.reads = append(tx.reads, KeyRange{Start: req.Key, Stop: req.Key})
	old, exists := tx.get(req.Key)

	switch req.Mode {
	case ModeInsertOnly:
		if exists {
			req.Updated = false
			return nil
		}
	case ModeUpdateOnly:
		if !exists {
			req.Updated = false
			return nil
		}
	case ModeUpsert:
	default:
		return fmt.Errorf("kv: unrecognized update mode %d", req.Mode)
	}

	if exists {
		req.Old = append([]byte(nil), old...)
	}
	staged := make([]byte, 0, len(req.Val)+1)
	staged = append(staged, flagUpdated)
	staged = append(staged, req.Val...)
	if err := tx.pending.Insert(req.Key, staged); err != nil {
		return err
	}

	tx.writes = append(tx.writes, KeyRange{Start: req.Key, Stop: req.Key})
	req.Added = !exists
	req.Updated = true
	return nil
}

// Del removes a key, reporting whether it was present.
func (tx *KVTX) Del(req *DeleteReq) (bool, error) {
	tx.reads = append(tx.reads, KeyRange{Start: req.Key, Stop: req.Key})
	old, exists := tx.get(req.Key)
	if !exists {
		return false, nil
	}
	req.Old = append([]byte(nil), old...)
	if err := tx.pending.Insert(req.Key, []byte{flagDeleted}); err != nil {
		return false, err
	}
	tx.writes = append(tx.writes, KeyRange{Start: req.Key, Stop: req.Key})
	return true, nil
}

// Seek returns an iterator over the transaction's view: the snapshot with this
// transaction's own writes layered on top.
func (tx *KVTX) Seek(key []byte, cmp int) *TxIter {
	// A range query depends on everything it might read, so the dependency is
	// recorded as a range rather than a point. The exact extent is not known
	// until the scan finishes, so the boundary is widened as the iterator
	// moves; see TxIter.record.
	iter := &TxIter{
		tx:      tx,
		forward: cmp > 0,
		pending: tx.pending.Seek(key, cmp),
		snap:    tx.snapshot.Seek(key, cmp),
		from:    append([]byte(nil), key...),
		to:      append([]byte(nil), key...),
	}
	iter.sync()
	iter.record()
	return iter
}

// TxIter walks the union of a transaction's pending writes and its snapshot,
// in key order, hiding keys the transaction has deleted.
//
// Two sorted sequences are merged: where both hold a key, the pending value
// wins, because a transaction must see its own writes.
type TxIter struct {
	tx      *KVTX
	forward bool

	pending *btree.Iter
	snap    *btree.Iter

	key   []byte
	val   []byte
	valid bool

	// from and to bound the keys this scan has touched, recorded as a read
	// dependency so that a concurrent write inside the range is a conflict.
	from []byte
	to   []byte
}

func (it *TxIter) Valid() bool { return it.valid }

// Deref returns the current key and value.
func (it *TxIter) Deref() ([]byte, []byte) { return it.key, it.val }

// Next advances past the current key in the direction the iterator was
// created with.
func (it *TxIter) Next() {
	if !it.valid {
		return
	}
	current := it.key
	// Step every source that produced the current key, so a key present in
	// both is not visited twice.
	if key, _, ok := it.peek(it.pending); ok && bytes.Equal(key, current) {
		it.step(it.pending)
	}
	if key, _, ok := it.peek(it.snap); ok && bytes.Equal(key, current) {
		it.step(it.snap)
	}
	it.sync()
	it.record()
}

// step moves one underlying iterator in the direction of travel.
func (it *TxIter) step(iter *btree.Iter) {
	if it.forward {
		iter.Next()
	} else {
		iter.Prev()
	}
}

// peek returns an iterator's current entry, skipping the tree's sentinel empty
// key, which is structural rather than data.
func (it *TxIter) peek(iter *btree.Iter) ([]byte, []byte, bool) {
	for iter.Valid() {
		key, val := iter.Deref()
		if len(key) > 0 {
			return key, val, true
		}
		it.step(iter)
	}
	return nil, nil, false
}

// before reports whether a comes before b in the direction of travel.
func (it *TxIter) before(a, b []byte) bool {
	if it.forward {
		return bytes.Compare(a, b) < 0
	}
	return bytes.Compare(a, b) > 0
}

// sync selects the next visible entry from the two sources, skipping keys the
// transaction deleted.
func (it *TxIter) sync() {
	for {
		pendingKey, pendingVal, hasPending := it.peek(it.pending)
		snapKey, snapVal, hasSnap := it.peek(it.snap)

		switch {
		case !hasPending && !hasSnap:
			it.valid = false
			return

		case hasPending && (!hasSnap || it.before(pendingKey, snapKey)):
			if pendingVal[0] == flagDeleted {
				it.step(it.pending)
				continue
			}
			it.key, it.val, it.valid = pendingKey, pendingVal[1:], true
			return

		case hasSnap && (!hasPending || it.before(snapKey, pendingKey)):
			it.key, it.val, it.valid = snapKey, snapVal, true
			return

		default: // the same key in both: this transaction's write wins
			if pendingVal[0] == flagDeleted {
				it.step(it.pending)
				it.step(it.snap)
				continue
			}
			it.key, it.val, it.valid = pendingKey, pendingVal[1:], true
			return
		}
	}
}

// record widens the transaction's read dependency to cover the key the scan
// has now reached.
func (it *TxIter) record() {
	if !it.valid {
		return
	}
	if bytes.Compare(it.key, it.from) < 0 {
		it.from = append([]byte(nil), it.key...)
	}
	if bytes.Compare(it.key, it.to) > 0 {
		it.to = append([]byte(nil), it.key...)
	}
	// Replace rather than append: one range per scan, grown in place.
	if n := len(it.tx.reads); n > 0 && it.tx.reads[n-1].Start != nil &&
		bytes.Equal(it.tx.reads[n-1].Start, it.from) && bytes.Equal(it.tx.reads[n-1].Stop, it.to) {
		return
	}
	it.tx.reads = append(it.tx.reads, KeyRange{Start: it.from, Stop: it.to})
}
