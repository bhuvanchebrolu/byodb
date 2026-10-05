package kv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"byodb/btree"
)

func openTemp(t *testing.T) (*KV, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func mustSet(t *testing.T, db *KV, key, val string) {
	t.Helper()
	if err := db.Set([]byte(key), []byte(val)); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

// TestAbortLeavesFileByteIdentical is the transaction guarantee stated as
// strictly as it can be: a rolled-back transaction must leave no trace at all.
//
// Copy-on-write is what makes this achievable — the aborted work only ever
// touched pages the committed version does not reference — but "no trace"
// deserves to be checked against the bytes rather than assumed.
func TestAbortLeavesFileByteIdentical(t *testing.T) {
	db, path := openTemp(t)
	for i := 0; i < 200; i++ {
		mustSet(t, db, fmt.Sprintf("key%03d", i), fmt.Sprintf("value%03d", i))
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}

	var tx KVTX
	db.Begin(&tx)
	for i := 0; i < 200; i++ {
		req := UpdateReq{Key: []byte(fmt.Sprintf("key%03d", i)), Val: []byte("changed"), Mode: ModeUpsert}
		if err := tx.Update(&req); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	for i := 0; i < 50; i++ {
		if _, err := tx.Del(&DeleteReq{Key: []byte(fmt.Sprintf("key%03d", i))}); err != nil {
			t.Fatalf("Del: %v", err)
		}
	}
	db.Abort(&tx)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("aborting changed the file: %d bytes before, %d after", len(before), len(after))
	}

	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%03d", i)
		got, ok := db.Get([]byte(key))
		if !ok || string(got) != fmt.Sprintf("value%03d", i) {
			t.Fatalf("after abort, %q is %q (present=%v)", key, got, ok)
		}
	}
}

// TestMultiKeyCommitIsAllOrNothing checks the property secondary indexes will
// depend on: a transaction touching many keys either lands entirely or not at
// all.
func TestMultiKeyCommitIsAllOrNothing(t *testing.T) {
	db, _ := openTemp(t)

	var tx KVTX
	db.Begin(&tx)
	for i := 0; i < 500; i++ {
		req := UpdateReq{Key: []byte(fmt.Sprintf("k%03d", i)), Val: []byte("v"), Mode: ModeUpsert}
		if err := tx.Update(&req); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	// Not yet committed, so nothing is visible outside the transaction.
	if _, ok := db.Get([]byte("k000")); ok {
		t.Fatal("an uncommitted write is visible outside its transaction")
	}
	if err := db.Commit(&tx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, ok := db.Get([]byte(fmt.Sprintf("k%03d", i))); !ok {
			t.Fatalf("key k%03d missing after commit", i)
		}
	}
}

func TestReadYourOwnWrites(t *testing.T) {
	db, _ := openTemp(t)
	mustSet(t, db, "existing", "original")

	var tx KVTX
	db.Begin(&tx)
	defer db.Abort(&tx)

	if err := tx.Update(&UpdateReq{Key: []byte("existing"), Val: []byte("changed"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := tx.Update(&UpdateReq{Key: []byte("fresh"), Val: []byte("new"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := tx.Del(&DeleteReq{Key: []byte("existing")}); err != nil {
		t.Fatalf("Del: %v", err)
	}

	if _, ok := tx.Get([]byte("existing")); ok {
		t.Fatal("a key deleted in this transaction is still readable inside it")
	}
	got, ok := tx.Get([]byte("fresh"))
	if !ok || string(got) != "new" {
		t.Fatalf("a key written in this transaction reads back as %q (present=%v)", got, ok)
	}
}

func TestUpdateModes(t *testing.T) {
	db, _ := openTemp(t)
	mustSet(t, db, "existing", "original")

	var tx KVTX
	db.Begin(&tx)
	defer db.Abort(&tx)

	// InsertOnly must refuse an existing key and leave it alone.
	req := UpdateReq{Key: []byte("existing"), Val: []byte("nope"), Mode: ModeInsertOnly}
	if err := tx.Update(&req); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if req.Updated {
		t.Fatal("insert-only mode overwrote an existing key")
	}
	if got, _ := tx.Get([]byte("existing")); string(got) != "original" {
		t.Fatalf("insert-only mode changed the value to %q", got)
	}

	// UpdateOnly must refuse a missing key.
	req = UpdateReq{Key: []byte("missing"), Val: []byte("nope"), Mode: ModeUpdateOnly}
	if err := tx.Update(&req); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if req.Updated {
		t.Fatal("update-only mode created a key that did not exist")
	}

	// Upsert reports the previous value, which is what index maintenance needs.
	req = UpdateReq{Key: []byte("existing"), Val: []byte("replaced"), Mode: ModeUpsert}
	if err := tx.Update(&req); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !req.Updated || req.Added {
		t.Fatalf("upsert on an existing key reported Updated=%v Added=%v", req.Updated, req.Added)
	}
	if string(req.Old) != "original" {
		t.Fatalf("upsert reported the old value as %q, want %q", req.Old, "original")
	}

	req = UpdateReq{Key: []byte("brand-new"), Val: []byte("v"), Mode: ModeUpsert}
	if err := tx.Update(&req); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !req.Added {
		t.Fatal("upsert on a new key did not report Added")
	}
}

// TestSnapshotIsolation is the reader guarantee: a transaction sees the
// database exactly as it was when it began, however much is committed
// afterwards.
func TestSnapshotIsolation(t *testing.T) {
	db, _ := openTemp(t)
	for i := 0; i < 100; i++ {
		mustSet(t, db, fmt.Sprintf("key%03d", i), "original")
	}

	var reader KVTX
	db.Begin(&reader)

	// Commit several more transactions while the reader is open.
	for round := 0; round < 20; round++ {
		var writer KVTX
		db.Begin(&writer)
		for i := 0; i < 100; i++ {
			req := UpdateReq{
				Key:  []byte(fmt.Sprintf("key%03d", i)),
				Val:  []byte(fmt.Sprintf("round%d", round)),
				Mode: ModeUpsert,
			}
			if err := writer.Update(&req); err != nil {
				t.Fatalf("Update: %v", err)
			}
		}
		if err := db.Commit(&writer); err != nil {
			t.Fatalf("writer commit: %v", err)
		}
	}

	// The reader still sees its own snapshot, not a mixture.
	for i := 0; i < 100; i++ {
		got, ok := reader.Get([]byte(fmt.Sprintf("key%03d", i)))
		if !ok {
			t.Fatalf("key%03d vanished from the reader's snapshot", i)
		}
		if string(got) != "original" {
			t.Fatalf("the reader observed %q, but its snapshot holds %q", got, "original")
		}
	}
	db.Abort(&reader)

	// And after the reader leaves, the latest version is what everyone sees.
	if got, _ := db.Get([]byte("key000")); string(got) != "round19" {
		t.Fatalf("after the reader left, key000 is %q, want %q", got, "round19")
	}
}

// TestFreeListHoldsPagesForLiveReaders checks the mechanism snapshot isolation
// depends on. A long-lived reader means recently freed pages must not be
// recycled, because that reader can still follow pointers into them.
//
// Without the version check the reader would silently read a page that has
// been overwritten with unrelated data.
func TestFreeListHoldsPagesForLiveReaders(t *testing.T) {
	db, _ := openTemp(t)
	for i := 0; i < 400; i++ {
		mustSet(t, db, fmt.Sprintf("key%04d", i), "original")
	}

	var reader KVTX
	db.Begin(&reader)

	// Churn hard enough that, without the version check, the reader's pages
	// would certainly be handed out and overwritten.
	for round := 0; round < 30; round++ {
		for i := 0; i < 400; i++ {
			mustSet(t, db, fmt.Sprintf("key%04d", i), fmt.Sprintf("churn%d", round))
		}
	}

	for i := 0; i < 400; i++ {
		got, ok := reader.Get([]byte(fmt.Sprintf("key%04d", i)))
		if !ok {
			t.Fatalf("key%04d vanished from the reader's snapshot after page churn", i)
		}
		if string(got) != "original" {
			t.Fatalf("the reader's snapshot was corrupted: key%04d reads %q, want %q",
				i, got, "original")
		}
	}
	db.Abort(&reader)
}

// TestWriteWriteConflictIsDetected covers the case optimistic concurrency
// control exists for: two transactions that overlap must not both succeed if
// one invalidates the other's reads.
func TestWriteWriteConflictIsDetected(t *testing.T) {
	db, _ := openTemp(t)
	mustSet(t, db, "shared", "0")

	var first, second KVTX
	db.Begin(&first)
	db.Begin(&second)

	// Both read the same key, then both try to write based on what they read.
	if _, ok := first.Get([]byte("shared")); !ok {
		t.Fatal("first transaction could not read the key")
	}
	if _, ok := second.Get([]byte("shared")); !ok {
		t.Fatal("second transaction could not read the key")
	}
	if err := first.Update(&UpdateReq{Key: []byte("shared"), Val: []byte("1"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := second.Update(&UpdateReq{Key: []byte("shared"), Val: []byte("2"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := db.Commit(&first); err != nil {
		t.Fatalf("the first commit should succeed: %v", err)
	}
	if err := db.Commit(&second); !errors.Is(err, ErrConflict) {
		t.Fatalf("the second commit returned %v, want ErrConflict — its read of "+
			"\"shared\" was invalidated by the first", err)
	}

	// The loser left nothing behind.
	if got, _ := db.Get([]byte("shared")); string(got) != "1" {
		t.Fatalf("after the conflict, the value is %q, want %q", got, "1")
	}
}

// TestDisjointTransactionsBothCommit is the other half: optimistic control
// must not reject transactions that never actually overlap, or it degenerates
// into a lock.
func TestDisjointTransactionsBothCommit(t *testing.T) {
	db, _ := openTemp(t)

	var first, second KVTX
	db.Begin(&first)
	db.Begin(&second)

	if err := first.Update(&UpdateReq{Key: []byte("alpha"), Val: []byte("1"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := second.Update(&UpdateReq{Key: []byte("beta"), Val: []byte("2"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Commit(&first); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := db.Commit(&second); err != nil {
		t.Fatalf("second commit on a disjoint key was rejected: %v", err)
	}
	if _, ok := db.Get([]byte("alpha")); !ok {
		t.Fatal("alpha missing")
	}
	if _, ok := db.Get([]byte("beta")); !ok {
		t.Fatal("beta missing")
	}
}

// TestRangeScanDependencyIsDetected checks that a range query records what it
// read. A transaction that scanned a range and then acted on the result must
// conflict with a write that lands inside that range — otherwise it committed
// on a premise that is no longer true.
func TestRangeScanDependencyIsDetected(t *testing.T) {
	db, _ := openTemp(t)
	for i := 0; i < 20; i++ {
		mustSet(t, db, fmt.Sprintf("row%02d", i), "v")
	}

	var scanner KVTX
	db.Begin(&scanner)
	count := 0
	for iter := scanner.Seek([]byte("row"), btree.CmpGE); iter.Valid(); iter.Next() {
		key, _ := iter.Deref()
		if !bytes.HasPrefix(key, []byte("row")) {
			break
		}
		count++
	}
	if count != 20 {
		t.Fatalf("the scan saw %d rows, want 20", count)
	}

	// Another transaction inserts into the scanned range and commits first.
	var writer KVTX
	db.Begin(&writer)
	if err := writer.Update(&UpdateReq{Key: []byte("row05x"), Val: []byte("v"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Commit(&writer); err != nil {
		t.Fatalf("writer commit: %v", err)
	}

	// The scanner now writes based on a count that is stale.
	if err := scanner.Update(&UpdateReq{Key: []byte("summary"), Val: []byte("20"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Commit(&scanner); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit returned %v, want ErrConflict — a row was inserted into the "+
			"range this transaction scanned", err)
	}
}

// TestTransactionIteratorSeesOwnWrites checks the merged view: a range scan
// inside a transaction must show that transaction's inserts, hide its
// deletions, and prefer its updates over the snapshot.
func TestTransactionIteratorSeesOwnWrites(t *testing.T) {
	db, _ := openTemp(t)
	for _, k := range []string{"a", "c", "e", "g"} {
		mustSet(t, db, k, "old")
	}

	var tx KVTX
	db.Begin(&tx)
	defer db.Abort(&tx)

	if err := tx.Update(&UpdateReq{Key: []byte("b"), Val: []byte("new"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := tx.Update(&UpdateReq{Key: []byte("c"), Val: []byte("changed"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := tx.Del(&DeleteReq{Key: []byte("e")}); err != nil {
		t.Fatalf("Del: %v", err)
	}

	type pair struct{ k, v string }
	var got []pair
	for iter := tx.Seek([]byte("a"), btree.CmpGE); iter.Valid(); iter.Next() {
		k, v := iter.Deref()
		got = append(got, pair{string(k), string(v)})
	}
	want := []pair{{"a", "old"}, {"b", "new"}, {"c", "changed"}, {"g", "old"}}
	if len(got) != len(want) {
		t.Fatalf("forward scan returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forward scan position %d: got %v, want %v", i, got[i], want[i])
		}
	}

	// Backward, the merge has to work in the other direction too.
	var backward []pair
	for iter := tx.Seek([]byte("z"), btree.CmpLE); iter.Valid(); iter.Next() {
		k, v := iter.Deref()
		backward = append(backward, pair{string(k), string(v)})
	}
	if len(backward) != len(want) {
		t.Fatalf("backward scan returned %v, want %d entries", backward, len(want))
	}
	for i := range want {
		if backward[len(backward)-1-i] != want[i] {
			t.Fatalf("backward scan position %d: got %v, want %v",
				i, backward[len(backward)-1-i], want[i])
		}
	}
}

// TestConcurrentReadersAndWriter runs real goroutines under the race detector.
// Readers must never observe a partially applied transaction.
func TestConcurrentReadersAndWriter(t *testing.T) {
	db, _ := openTemp(t)

	// Every row carries the same generation label, so a reader that sees two
	// different labels has caught a transaction half-applied.
	const rows = 50
	writeGeneration := func(label string) error {
		var tx KVTX
		db.Begin(&tx)
		for i := 0; i < rows; i++ {
			req := UpdateReq{Key: []byte(fmt.Sprintf("row%02d", i)), Val: []byte(label), Mode: ModeUpsert}
			if err := tx.Update(&req); err != nil {
				db.Abort(&tx)
				return err
			}
		}
		return db.Commit(&tx)
	}
	if err := writeGeneration("gen000"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 16)

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var tx KVTX
				db.Begin(&tx)
				var label string
				for i := 0; i < rows; i++ {
					val, ok := tx.Get([]byte(fmt.Sprintf("row%02d", i)))
					if !ok {
						db.Abort(&tx)
						errs <- fmt.Errorf("row%02d missing from a snapshot", i)
						return
					}
					if label == "" {
						label = string(val)
					} else if string(val) != label {
						db.Abort(&tx)
						errs <- fmt.Errorf("snapshot mixes generations %q and %q: "+
							"a reader observed a partially applied transaction", label, val)
						return
					}
				}
				db.Abort(&tx)
			}
		}()
	}

	for g := 1; g <= 40; g++ {
		if err := writeGeneration(fmt.Sprintf("gen%03d", g)); err != nil {
			t.Fatalf("writing generation %d: %v", g, err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestReadDependencyOnADifferentKeyConflicts isolates the read half of
// conflict detection.
//
// The write-write test does not actually prove reads are tracked: Update
// itself reads the key it writes, so the dependency would be recorded even if
// Get tracked nothing. Here the transaction reads one key and writes a
// different one — the classic "write b := a" — so only a recorded read
// dependency on `a` can catch the conflict.
func TestReadDependencyOnADifferentKeyConflicts(t *testing.T) {
	db, _ := openTemp(t)
	mustSet(t, db, "source", "1")

	var derived KVTX
	db.Begin(&derived)

	got, ok := derived.Get([]byte("source"))
	if !ok {
		t.Fatal("could not read the source key")
	}

	// Another transaction changes the value this one is about to depend on.
	var writer KVTX
	db.Begin(&writer)
	if err := writer.Update(&UpdateReq{Key: []byte("source"), Val: []byte("999"), Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Commit(&writer); err != nil {
		t.Fatalf("writer commit: %v", err)
	}

	// Now derive a different key from the stale read.
	if err := derived.Update(&UpdateReq{Key: []byte("derived"), Val: got, Mode: ModeUpsert}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Commit(&derived); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit returned %v, want ErrConflict — this transaction wrote a value "+
			"derived from a key another transaction has since changed", err)
	}

	if got, _ := db.Get([]byte("source")); string(got) != "999" {
		t.Fatalf("source is %q, want %q", got, "999")
	}
	if _, ok := db.Get([]byte("derived")); ok {
		t.Fatal("the conflicting transaction left a key behind")
	}
}
