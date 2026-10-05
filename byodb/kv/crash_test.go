package kv

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// crashFile simulates a storage device losing power.
//
// The essential detail is that a write is not durable until an fsync confirms
// it. So writes are held back and only reach the real file on Sync — a
// simulated crash then loses exactly the writes a real crash would lose.
// Without this, an in-process test proves nothing: the OS page cache would
// happily serve back writes that never reached the disk.
type crashFile struct {
	inner   fileIO
	pending []stagedWrite

	// Writes and syncs are targeted separately, by 1-based index; 0 disables
	// injection. Counting them separately keeps a test's intent stable even
	// when an update's number of write calls changes.
	failWriteAt int
	failSyncAt  int
	mode        failureMode

	writes int
	syncs  int
}

// arm resets the counters and targets a specific write or sync.
func (f *crashFile) arm(writeAt, syncAt int, mode failureMode) {
	f.writes, f.syncs = 0, 0
	f.failWriteAt, f.failSyncAt, f.mode = writeAt, syncAt, mode
}

type stagedWrite struct {
	buf    []byte
	offset int64
}

type failureMode int

const (
	// failWrite: the write returns an error and nothing reaches the device.
	failWrite failureMode = iota
	// failTornWrite: the write returns an error having persisted only part of
	// its bytes — the torn write that checksums exist to detect.
	failTornWrite
	// failSyncLose: fsync fails and the pending writes are lost.
	failSyncLose
	// failSyncKeep: fsync reports failure but the data did reach the device
	// anyway. This is the case that makes error handling subtle: the on-disk
	// state now disagrees with what the database believes it committed.
	failSyncKeep
	// failSyncPartial: only the most recent pending write reaches the device;
	// earlier ones are lost.
	//
	// This models write reordering, which real devices and page caches do
	// freely. It is the mode that proves the fsync between phase 1 and phase
	// 2 is load-bearing: without that barrier, a single flush could persist
	// the new root pointer while the pages it references are still in
	// volatile memory, leaving a meta page that points at nothing.
	failSyncPartial
)

var errInjected = errors.New("injected IO failure")

func (f *crashFile) WriteAt(buf []byte, offset int64) error {
	f.writes++
	owned := append([]byte(nil), buf...)
	if f.writes == f.failWriteAt {
		switch f.mode {
		case failWrite:
			return errInjected
		case failTornWrite:
			// Persist a prefix, as an interrupted write would.
			torn := owned[:len(owned)/2]
			if len(torn) > 0 {
				_ = f.inner.WriteAt(torn, offset)
				_ = f.inner.Sync()
			}
			return errInjected
		}
	}
	f.pending = append(f.pending, stagedWrite{buf: owned, offset: offset})
	return nil
}

func (f *crashFile) Sync() error {
	f.syncs++
	failing := f.syncs == f.failSyncAt

	if failing && f.mode == failSyncLose {
		f.pending = nil // the writes never made it
		return errInjected
	}
	if failing && f.mode == failSyncPartial && len(f.pending) > 0 {
		// Persist only the newest write, dropping everything queued before
		// it — the reordering a missing barrier would expose.
		f.pending = f.pending[len(f.pending)-1:]
	}
	for _, w := range f.pending {
		if err := f.inner.WriteAt(w.buf, w.offset); err != nil {
			return err
		}
	}
	f.pending = nil
	if err := f.inner.Sync(); err != nil {
		return err
	}
	if failing && f.mode != failSyncLose {
		return errInjected
	}
	return nil
}

// crash discards everything not yet made durable, exactly as a power loss
// would, leaving the real file holding only fsync'ed data.
func (f *crashFile) crash() { f.pending = nil }

// TestCrashAtEveryStep is the test this whole design exists to pass.
//
// It takes a database with known contents, applies one more update, and
// injects a failure at every write and every fsync in that update, under
// every failure mode. After each simulated crash it reopens the file and
// requires that the database is at either the old version or the new one —
// never a mixture, never unreadable.
func TestCrashAtEveryStep(t *testing.T) {
	writeModes := map[failureMode]string{
		failWrite:     "write fails",
		failTornWrite: "write is torn",
	}
	syncModes := map[failureMode]string{
		failSyncLose:    "fsync fails, data lost",
		failSyncKeep:    "fsync fails, data persisted",
		failSyncPartial: "writes reordered",
	}

	for mode, modeName := range writeModes {
		for n := 1; n <= 4; n++ {
			t.Run(fmt.Sprintf("%s/write_%d", modeName, n), func(t *testing.T) {
				checkCrashDuringUpdate(t, n, 0, mode)
			})
		}
	}
	for mode, modeName := range syncModes {
		for n := 1; n <= 3; n++ {
			t.Run(fmt.Sprintf("%s/sync_%d", modeName, n), func(t *testing.T) {
				checkCrashDuringUpdate(t, 0, n, mode)
			})
		}
	}
}

func checkCrashDuringUpdate(t *testing.T, writeAt, syncAt int, mode failureMode) {
	t.Helper()

	// Enough baseline keys to span several pages, so an update rewrites a
	// path through a multi-level tree rather than touching a single node.
	baseline := map[string]string{}
	for i := 0; i < 400; i++ {
		baseline[fmt.Sprintf("key%04d", i)] = fmt.Sprintf("value%04d", i)
	}

	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for k, v := range baseline {
		if err := db.Set([]byte(k), []byte(v)); err != nil {
			t.Fatalf("baseline Set(%q): %v", k, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen with faults armed and attempt one more update.
	var fault *crashFile
	db, err = openWith(path, func(inner fileIO) fileIO {
		fault = &crashFile{inner: inner, failWriteAt: writeAt, failSyncAt: syncAt, mode: mode}
		return fault
	})
	if err != nil {
		t.Fatalf("reopen with fault injection: %v", err)
	}
	updateErr := db.Set([]byte("newkey"), []byte("newvalue"))
	fault.crash()
	_ = db.Close()

	// Reopen as a fresh process would after a crash.
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen after crash (update returned %v): %v", updateErr, err)
	}
	defer db.Close()

	// Every previously committed key must still be intact. This is the real
	// assertion: an interrupted update must not damage the version that was
	// already committed.
	for k, want := range baseline {
		got, ok := db.Get([]byte(k))
		if !ok {
			t.Fatalf("committed key %q lost after crash (update error: %v)", k, updateErr)
		}
		if string(got) != want {
			t.Fatalf("committed key %q became %q, want %q", k, got, want)
		}
	}

	// The in-flight update is all-or-nothing.
	got, present := db.Get([]byte("newkey"))
	if present && string(got) != "newvalue" {
		t.Fatalf("in-flight key recovered with a corrupt value %q", got)
	}
	if updateErr == nil && !present {
		t.Fatal("update reported success but the key did not survive the crash")
	}

	// The recovered database must still be usable, not merely readable.
	if err := db.Set([]byte("after-recovery"), []byte("ok")); err != nil {
		t.Fatalf("update after crash recovery: %v", err)
	}
}

// TestUsableAfterFailedUpdate covers the read-after-failure path: a failed
// update must leave the database readable at its previous version rather than
// poisoned.
func TestUsableAfterFailedUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	var fault *crashFile
	db, err := openWith(path, func(inner fileIO) fileIO {
		fault = &crashFile{inner: inner}
		return fault
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for i := 0; i < 50; i++ {
		if err := db.Set([]byte(fmt.Sprintf("key%02d", i)), []byte("original")); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Arm a failure on the very next write.
	fault.arm(1, 0, failWrite)
	if err := db.Set([]byte("key00"), []byte("doomed")); err == nil {
		t.Fatal("expected the injected failure to surface")
	}

	// Reads continue to work, at the last good version.
	got, ok := db.Get([]byte("key00"))
	if !ok || string(got) != "original" {
		t.Fatalf("after a failed update the value is %q (present=%v), want %q", got, ok, "original")
	}

	// A subsequent successful update recovers. This exercises the rewrite of
	// the last known-good meta page: after a failure the on-disk meta page
	// may hold either version, so appending over the newer version's pages
	// without restoring it first could corrupt what the file points at.
	fault.arm(0, 0, failWrite)
	if err := db.Set([]byte("key00"), []byte("recovered")); err != nil {
		t.Fatalf("update after a transient failure: %v", err)
	}
	if got, ok := db.Get([]byte("key00")); !ok || string(got) != "recovered" {
		t.Fatalf("value is %q (present=%v), want %q", got, ok, "recovered")
	}

	// And it is durable.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get([]byte("key00")); !ok || string(got) != "recovered" {
		t.Fatalf("after reopen the value is %q (present=%v), want %q", got, ok, "recovered")
	}
}

// TestRejectsCorruptMetaPage checks that a damaged meta page is reported
// rather than silently misread into a wrong root pointer.
//
// The corruption is deliberately subtle: the root pointer is changed to a
// page number that is still within range, so the sanity checks cannot catch
// it and only the checksum can. A blatant value would pass this test even
// with checksum verification removed.
func TestRejectsCorruptMetaPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 300; i++ {
		if err := db.Set([]byte(fmt.Sprintf("key%03d", i)), []byte("value")); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Byte 28 is the low byte of the root pointer within slot 0. Setting it
	// to 1 leaves a plausible, in-range page number that points at the wrong
	// node — the kind of damage only a checksum catches.
	corrupt(t, path, 28, 0x01)

	// One damaged slot is survivable: that is what the second slot is for.
	// Falling back costs the most recent commit — the deliberate trade-off of
	// double buffering, and far better than refusing to open at all. In a real
	// crash the lost commit is the one that was in flight anyway.
	db, err = Open(path)
	if err != nil {
		t.Fatalf("a database with one damaged meta slot should still open: %v", err)
	}
	for i := 0; i < 299; i++ {
		key := fmt.Sprintf("key%03d", i)
		if _, ok := db.Get([]byte(key)); !ok {
			t.Fatalf("key %q lost after falling back to the intact meta slot; "+
				"only the final commit should be affected", key)
		}
	}
	// And the recovered database is writable, not just readable.
	if err := db.Set([]byte("after-fallback"), []byte("ok")); err != nil {
		t.Fatalf("update after falling back to the intact meta slot: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Damaging both slots leaves nothing to fall back to, and must be
	// reported rather than guessed at. Slot 0 needs damaging again: the
	// update above committed a fresh, valid record into it.
	corrupt(t, path, 28, 0x01)
	corrupt(t, path, metaSlotOffset+28, 0x01)
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("opening a database with both meta slots damaged returned %v, want ErrCorrupt", err)
	}
}

// TestRecoveryAfterAmbiguousMetaPage covers the subtlest failure in the whole
// stage.
//
// When fsync fails on the meta page, the on-disk meta page may hold either the
// old version or the new one — the database cannot tell which. It reverts its
// in-memory state to the old version, so the pages the *new* version occupies
// now look free for reuse. If the next update appends over them while the disk
// meta page still points at that new version, a crash leaves a committed root
// referencing pages that have been overwritten: silent corruption, from two
// individually "handled" failures.
//
// The fix is to rewrite the last known-good meta page before appending
// anything. This test fails without it.
// The two updates are swept across value sizes because the damage only shows
// when they occupy different numbers of pages. If both write the same number,
// the second update's pages happen to land exactly on the first's and the
// result is accidentally self-consistent — so a single size pair can pass
// even with the protection removed.
func TestRecoveryAfterAmbiguousMetaPage(t *testing.T) {
	sizes := []int{8, 800, 3000}
	for _, sizeA := range sizes {
		for _, sizeB := range sizes {
			t.Run(fmt.Sprintf("A%d_B%d", sizeA, sizeB), func(t *testing.T) {
				checkAmbiguousMetaRecovery(t, sizeA, sizeB)
			})
		}
	}
}

func checkAmbiguousMetaRecovery(t *testing.T, sizeA, sizeB int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")

	baseline := map[string]string{}
	for i := 0; i < 300; i++ {
		baseline[fmt.Sprintf("key%03d", i)] = fmt.Sprintf("value%03d", i)
	}
	valueA := string(bytes.Repeat([]byte("A"), sizeA))
	valueB := string(bytes.Repeat([]byte("B"), sizeB))

	var fault *crashFile
	db, err := openWith(path, func(inner fileIO) fileIO {
		fault = &crashFile{inner: inner}
		return fault
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for k, v := range baseline {
		if err := db.Set([]byte(k), []byte(v)); err != nil {
			t.Fatalf("baseline Set: %v", err)
		}
	}

	// Update A fails on the final fsync, but its meta write did reach the
	// device. The disk now commits a version the database has disowned.
	fault.arm(0, 2, failSyncKeep)
	if err := db.Set([]byte("keyA"), []byte(valueA)); err == nil {
		t.Fatal("expected the injected fsync failure to surface")
	}

	// Update B fails while appending its pages. Without the meta rewrite,
	// those appends land on top of the pages the disk meta page still points
	// at.
	fault.arm(0, 2, failSyncLose)
	if err := db.Set([]byte("keyB"), []byte(valueB)); err == nil {
		t.Fatal("expected the injected write failure to surface")
	}

	fault.crash()
	_ = db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after two failed updates: %v", err)
	}
	defer reopened.Close()

	for k, want := range baseline {
		got, ok := reopened.Get([]byte(k))
		if !ok {
			t.Fatalf("committed key %q lost after two failed updates", k)
		}
		if string(got) != want {
			t.Fatalf("committed key %q became %q, want %q", k, got, want)
		}
	}
	// Whichever version survived, the two failed updates must not have left a
	// half-written one.
	if got, ok := reopened.Get([]byte("keyA")); ok && string(got) != valueA {
		t.Fatalf("keyA recovered with a corrupt value (%d bytes, want %d)", len(got), len(valueA))
	}
	if got, ok := reopened.Get([]byte("keyB")); ok && string(got) != valueB {
		t.Fatalf("keyB recovered with a corrupt value (%d bytes, want %d)", len(got), len(valueB))
	}
}
