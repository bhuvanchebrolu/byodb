package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// sortedKeys returns the harness's reference keys in order — the answer the
// iterator has to reproduce.
func (h *harness) sortedKeys() []string {
	keys := make([]string, 0, len(h.ref))
	for k := range h.ref {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// collectForward walks the whole tree with the iterator, skipping the sentinel.
func (h *harness) collectForward() []string {
	var got []string
	for iter := h.tree.SeekLE(nil); iter.Valid(); iter.Next() {
		key, _ := iter.Deref()
		if len(key) > 0 {
			got = append(got, string(key))
		}
	}
	return got
}

func TestIteratorWalksEveryKeyInOrder(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 2000; i++ {
		h.insert(fmt.Sprintf("key%05d", i), fmt.Sprintf("value%d", i))
	}
	h.verify()

	got := h.collectForward()
	want := h.sortedKeys()
	if len(got) != len(want) {
		t.Fatalf("iterator visited %d keys, the tree holds %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: iterator gave %q, want %q", i, got[i], want[i])
		}
	}
}

// TestIteratorWalksBackward matters because stepping backward across a node
// boundary has to enter the sibling at its *last* key, a different case from
// stepping forward.
func TestIteratorWalksBackward(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 2000; i++ {
		h.insert(fmt.Sprintf("key%05d", i), fmt.Sprintf("value%d", i))
	}
	want := h.sortedKeys()

	var got []string
	iter := h.tree.Seek([]byte("key99999"), CmpLE)
	for ; iter.Valid(); iter.Prev() {
		key, _ := iter.Deref()
		if len(key) == 0 {
			break // the sentinel marks the start of the tree
		}
		got = append(got, string(key))
	}

	if len(got) != len(want) {
		t.Fatalf("backward walk visited %d keys, the tree holds %d", len(got), len(want))
	}
	for i := range want {
		if got[len(got)-1-i] != want[i] {
			t.Fatalf("backward position %d: got %q, want %q", i, got[len(got)-1-i], want[i])
		}
	}
}

// TestSeekOperators checks all four operators against a linear scan of the
// reference data, including keys that are present and keys that fall between
// two existing ones.
func TestSeekOperators(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 500; i++ {
		h.insert(fmt.Sprintf("key%04d", i*2), "v") // even keys only
	}
	sorted := h.sortedKeys()

	// Reference implementations over the sorted slice.
	reference := map[int]func(target string) (string, bool){
		CmpGE: func(target string) (string, bool) {
			for _, k := range sorted {
				if k >= target {
					return k, true
				}
			}
			return "", false
		},
		CmpGT: func(target string) (string, bool) {
			for _, k := range sorted {
				if k > target {
					return k, true
				}
			}
			return "", false
		},
		CmpLE: func(target string) (string, bool) {
			for i := len(sorted) - 1; i >= 0; i-- {
				if sorted[i] <= target {
					return sorted[i], true
				}
			}
			return "", false
		},
		CmpLT: func(target string) (string, bool) {
			for i := len(sorted) - 1; i >= 0; i-- {
				if sorted[i] < target {
					return sorted[i], true
				}
			}
			return "", false
		},
	}
	names := map[int]string{CmpGE: ">=", CmpGT: ">", CmpLE: "<=", CmpLT: "<"}

	var targets []string
	for i := 0; i < 1000; i++ {
		targets = append(targets, fmt.Sprintf("key%04d", i)) // even = present, odd = absent
	}
	targets = append(targets, "aaa", "zzz", "key0000", "key0998")

	for cmp, want := range reference {
		for _, target := range targets {
			iter := h.tree.Seek([]byte(target), cmp)

			var got string
			var found bool
			for iter.Valid() {
				key, _ := iter.Deref()
				if len(key) == 0 {
					// The sentinel is not a real key; for a backward search it
					// means nothing smaller exists.
					break
				}
				got, found = string(key), true
				break
			}

			wantKey, wantFound := want(target)
			if found != wantFound || got != wantKey {
				t.Fatalf("Seek(%q, %s) = (%q, %v), want (%q, %v)",
					target, names[cmp], got, found, wantKey, wantFound)
			}
		}
	}
}

// TestIteratorOnEmptyAndSingleKeyTrees covers the degenerate shapes, where an
// off-by-one in Valid would otherwise go unnoticed.
func TestIteratorOnEmptyAndSingleKeyTrees(t *testing.T) {
	h := newHarness(t)
	if iter := h.tree.SeekLE([]byte("anything")); iter.Valid() {
		t.Fatal("an iterator over an empty tree reports a valid position")
	}

	h.insert("only", "value")
	iter := h.tree.Seek([]byte("only"), CmpGE)
	if !iter.Valid() {
		t.Fatal("seeking the only key found nothing")
	}
	key, val := iter.Deref()
	if string(key) != "only" || string(val) != "value" {
		t.Fatalf("got (%q, %q), want (%q, %q)", key, val, "only", "value")
	}
	iter.Next()
	if iter.Valid() {
		t.Fatal("stepping past the only key still reports a valid position")
	}
}

// TestIteratorMatchesReferenceOverRandomRanges is the differential test: many
// random ranges, each compared against a linear scan.
func TestIteratorMatchesReferenceOverRandomRanges(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 1500; i++ {
		h.insert(fmt.Sprintf("key%05d", rng.Intn(30000)), "v")
	}
	sorted := h.sortedKeys()

	for trial := 0; trial < 300; trial++ {
		lo := fmt.Sprintf("key%05d", rng.Intn(30000))
		hi := fmt.Sprintf("key%05d", rng.Intn(30000))
		if lo > hi {
			lo, hi = hi, lo
		}

		var want []string
		for _, k := range sorted {
			if k >= lo && k <= hi {
				want = append(want, k)
			}
		}

		var got []string
		for iter := h.tree.Seek([]byte(lo), CmpGE); iter.Valid(); iter.Next() {
			key, _ := iter.Deref()
			if len(key) == 0 {
				continue
			}
			if string(key) > hi {
				break
			}
			got = append(got, string(key))
		}

		if len(got) != len(want) {
			t.Fatalf("range [%s, %s]: iterator returned %d keys, want %d",
				lo, hi, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("range [%s, %s] position %d: got %q, want %q",
					lo, hi, i, got[i], want[i])
			}
		}
	}
}

// TestIteratorOnTallTree walks a tree deep enough to have three levels.
//
// Two-level trees hide a whole class of iterator bug: when the carry reaches
// an exhausted root, there is no intermediate frame left to unwind into, so a
// mistake in what those frames do on the way back down never shows. At three
// levels it does — and the failure mode is an iterator that wraps around and
// never terminates, not one that returns a wrong answer.
func TestIteratorOnTallTree(t *testing.T) {
	h := newHarness(t)

	// Long keys and values, so the tree gains height with fewer rows.
	padding := string(bytes.Repeat([]byte("p"), 400))
	const count = 3000
	for i := 0; i < count; i++ {
		h.insert(fmt.Sprintf("key%06d", i), padding)
	}
	if height := h.height(); height < 3 {
		t.Fatalf("this test needs a tree of at least three levels, got %d", height)
	}

	// A bounded loop: an iterator that wraps around would otherwise hang the
	// test rather than fail it.
	seen := 0
	for iter := h.tree.SeekLE(nil); iter.Valid(); iter.Next() {
		key, _ := iter.Deref()
		if len(key) == 0 {
			continue
		}
		seen++
		if seen > count*2 {
			t.Fatal("the forward iterator did not terminate: it wrapped around " +
				"instead of stopping past the last key")
		}
	}
	if seen != count {
		t.Fatalf("the forward walk visited %d keys, want %d", seen, count)
	}

	seen = 0
	for iter := h.tree.Seek([]byte("key999999"), CmpLE); iter.Valid(); iter.Prev() {
		key, _ := iter.Deref()
		if len(key) == 0 {
			break
		}
		seen++
		if seen > count*2 {
			t.Fatal("the backward iterator did not terminate: it wrapped around " +
				"instead of stopping before the first key")
		}
	}
	if seen != count {
		t.Fatalf("the backward walk visited %d keys, want %d", seen, count)
	}
}

// TestIteratorTerminatesAtEveryHeight walks trees across the height boundaries,
// since each added level is a fresh opportunity for the carry to go wrong.
func TestIteratorTerminatesAtEveryHeight(t *testing.T) {
	for _, count := range []int{1, 2, 200, 2000, 6000, 20000} {
		t.Run(fmt.Sprintf("%d_keys", count), func(t *testing.T) {
			h := newHarness(t)
			for i := 0; i < count; i++ {
				h.insert(fmt.Sprintf("key%08d", i), "v")
			}

			seen := 0
			for iter := h.tree.SeekLE(nil); iter.Valid(); iter.Next() {
				if key, _ := iter.Deref(); len(key) > 0 {
					seen++
				}
				if seen > count+10 {
					t.Fatalf("the iterator did not terminate at height %d", h.height())
				}
			}
			if seen != count {
				t.Fatalf("visited %d keys at height %d, want %d", seen, h.height(), count)
			}
		})
	}
}
