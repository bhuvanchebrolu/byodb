package kv

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// corrupt overwrites one byte of the database file, for tests that need to
// damage it deliberately.
func corrupt(t *testing.T, path string, offset int64, value byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte{value}, offset); err != nil {
		t.Fatalf("corrupt byte at %d: %v", offset, err)
	}
}

// store mirrors the database against a reference map, the same differential
// approach used for the tree, now with durability in the mix.
type store struct {
	t    *testing.T
	db   *KV
	ref  map[string]string
	path string
}

func newStore(t *testing.T) *store {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := &store{t: t, db: db, ref: map[string]string{}, path: path}
	t.Cleanup(func() { _ = s.db.Close() })
	return s
}

func (s *store) set(key, val string) {
	if err := s.db.Set([]byte(key), []byte(val)); err != nil {
		s.t.Fatalf("Set(%q): %v", key, err)
	}
	s.ref[key] = val
}

func (s *store) del(key string) {
	if _, err := s.db.Del([]byte(key)); err != nil {
		s.t.Fatalf("Del(%q): %v", key, err)
	}
	delete(s.ref, key)
}

func (s *store) verify() {
	s.t.Helper()
	for key, want := range s.ref {
		got, ok := s.db.Get([]byte(key))
		if !ok {
			s.t.Fatalf("key %q missing", key)
		}
		if string(got) != want {
			s.t.Fatalf("key %q is %q, want %q", key, got, want)
		}
	}
}

// reopen closes and reopens the database, which is the only real test of
// durability: everything held in memory is discarded.
func (s *store) reopen() {
	s.t.Helper()
	if err := s.db.Close(); err != nil {
		s.t.Fatalf("close: %v", err)
	}
	db, err := Open(s.path)
	if err != nil {
		s.t.Fatalf("reopen: %v", err)
	}
	s.db = db
}

func TestSetGetDelete(t *testing.T) {
	s := newStore(t)
	s.set("alpha", "1")
	s.set("beta", "2")
	s.set("gamma", "3")
	s.verify()

	s.del("beta")
	s.verify()
	if _, ok := s.db.Get([]byte("beta")); ok {
		t.Fatal("deleted key is still readable")
	}

	// Deleting a key that was never there is not an error and changes nothing.
	deleted, err := s.db.Del([]byte("absent"))
	if err != nil {
		t.Fatalf("Del of a missing key: %v", err)
	}
	if deleted {
		t.Fatal("Del reported deleting a key that was never inserted")
	}
}

func TestDurabilityAcrossReopen(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 2000; i++ {
		s.set(fmt.Sprintf("key%05d", i), fmt.Sprintf("value%05d", i))
	}
	s.verify()
	s.reopen()
	s.verify()

	// Updates after reopening land on the recovered tree.
	for i := 0; i < 500; i++ {
		s.set(fmt.Sprintf("key%05d", i), "updated")
	}
	s.reopen()
	s.verify()
}

func TestEmptyDatabaseReopen(t *testing.T) {
	s := newStore(t)
	s.reopen()
	if _, ok := s.db.Get([]byte("anything")); ok {
		t.Fatal("a fresh database returned a value")
	}
	s.set("first", "value")
	s.reopen()
	s.verify()
}

func TestLargeValues(t *testing.T) {
	s := newStore(t)
	// MaxValSize, not the tree's limit: a transaction prefixes each buffered
	// value with a flag byte, so one byte of the tree's capacity is spent.
	big := string(bytes.Repeat([]byte("v"), MaxValSize))
	for i := 0; i < 200; i++ {
		s.set(fmt.Sprintf("key%03d", i), big)
	}
	s.reopen()
	s.verify()
}

// TestRandomizedWithReopens mixes updates with reopens, so a bug that only
// shows up in recovered state has somewhere to surface.
func TestRandomizedWithReopens(t *testing.T) {
	s := newStore(t)
	rng := rand.New(rand.NewSource(7))
	for op := 0; op < 3000; op++ {
		key := fmt.Sprintf("key%03d", rng.Intn(300))
		if rng.Intn(3) == 0 {
			s.del(key)
		} else {
			s.set(key, string(bytes.Repeat([]byte("x"), rng.Intn(400))))
		}
		if op%500 == 0 {
			s.verify()
			s.reopen()
			s.verify()
		}
	}
	s.verify()
	s.reopen()
	s.verify()

	// One byte more must be refused rather than silently truncated.
	if err := s.db.Set([]byte("toobig"), bytes.Repeat([]byte("v"), MaxValSize+1)); err == nil {
		t.Fatal("a value one byte over the limit was accepted")
	}
}

// TestFreeListBoundsFileGrowth is the free list's reason for existing.
//
// Copy-on-write orphans the old copy of every page it touches, so without
// reclamation the file would grow in proportion to the number of *updates*
// rather than the amount of data. Correctness tests would never notice; the
// file would just grow forever.
func TestFreeListBoundsFileGrowth(t *testing.T) {
	s := newStore(t)
	const (
		keysPerRound = 200
		rounds       = 40
		// Growth settles over the first few rounds while the tree reaches its
		// shape and the free list fills; measure the steady state after that.
		settleRound = 10
	)

	fileSize := func() int64 {
		info, err := os.Stat(s.path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		return info.Size()
	}

	var settled, final int64
	for round := 0; round < rounds; round++ {
		for i := 0; i < keysPerRound; i++ {
			s.set(fmt.Sprintf("key%03d", i), "value")
		}
		for i := 0; i < keysPerRound; i++ {
			s.del(fmt.Sprintf("key%03d", i))
		}
		if round == settleRound {
			settled = fileSize()
		}
	}
	final = fileSize()

	if final > settled {
		t.Fatalf("the file kept growing across identical cycles: %d bytes at round %d, "+
			"%d at round %d — pages are not being reclaimed", settled, settleRound, final, rounds)
	}

	// The stronger claim: after tens of thousands of updates the file is
	// still a handful of pages, so its size tracks the data rather than the
	// update count.
	const updates = rounds * keysPerRound * 2
	if pages := final / PageSize; pages > 32 {
		t.Fatalf("file is %d pages after %d updates; size is tracking updates, not data",
			pages, updates)
	}
	t.Logf("steady state: %d pages after %d updates", final/PageSize, updates)

	// Reclaimed pages must still hold correct data afterwards.
	for i := 0; i < keysPerRound; i++ {
		s.set(fmt.Sprintf("final%03d", i), fmt.Sprintf("value%03d", i))
	}
	s.reopen()
	s.verify()
}

// TestSecondOpenIsRefused covers the assumption the whole design rests on:
// one process at a time.
//
// The page cache, the free list's view of which versions are live and the
// conflict history are all in memory. A second process would commit against
// state the first knows nothing about, and the failure is silent — its meta
// page overwrites the first's, and rows already reported as committed simply
// disappear. Refusing to open is the only safe answer.
func TestSecondOpenIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Set([]byte("key"), []byte("value")); err != nil {
		t.Fatalf("set: %v", err)
	}

	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("opening an already-open database returned %v, want ErrLocked", err)
	}

	// Closing releases the claim, so the next process can take it.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening after close: %v", err)
	}
	defer second.Close()
	if got, ok := second.Get([]byte("key")); !ok || string(got) != "value" {
		t.Fatalf("value is %q (present=%v) after reopening", got, ok)
	}
}
