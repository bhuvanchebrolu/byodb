package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// harness pairs the tree with an in-memory page store and a reference map.
//
// Every mutation is applied to both, then the two are compared. The reference
// is obviously correct, so any divergence is a bug in the tree — this catches
// far more than hand-written cases because it explores states nobody would
// think to write down.
type harness struct {
	t     *testing.T
	tree  *BTree
	pages map[uint64]Node   // the simulated disk
	ref   map[string]string // the reference model
	next  uint64            // page number allocator
	freed int               // pages released, for accounting
}

func newHarness(t *testing.T) *harness {
	h := &harness{
		t:     t,
		pages: map[uint64]Node{},
		ref:   map[string]string{},
		next:  1, // page 0 is reserved, mirroring the on-disk meta page
	}
	h.tree = New(0,
		func(ptr uint64) []byte {
			node, ok := h.pages[ptr]
			if !ok {
				h.t.Fatalf("read of unallocated page %d", ptr)
			}
			return node
		},
		func(page []byte) uint64 {
			if size := Node(page).sizeBytes(); size > PageSize {
				h.t.Fatalf("allocating an oversized node: %d > %d", size, PageSize)
			}
			ptr := h.next
			h.next++
			// Copy so the store owns its bytes, as a real page cache would.
			owned := make(Node, PageSize)
			copy(owned, page)
			h.pages[ptr] = owned
			return ptr
		},
		func(ptr uint64) {
			if _, ok := h.pages[ptr]; !ok {
				h.t.Fatalf("double free or free of unallocated page %d", ptr)
			}
			delete(h.pages, ptr)
			h.freed++
		},
	)
	return h
}

func (h *harness) insert(key, val string) {
	if err := h.tree.Insert([]byte(key), []byte(val)); err != nil {
		h.t.Fatalf("Insert(%q): %v", key, err)
	}
	h.ref[key] = val
}

func (h *harness) delete(key string) bool {
	deleted, err := h.tree.Delete([]byte(key))
	if err != nil {
		h.t.Fatalf("Delete(%q): %v", key, err)
	}
	_, expected := h.ref[key]
	if deleted != expected {
		h.t.Fatalf("Delete(%q) reported %v, reference says the key was present: %v",
			key, deleted, expected)
	}
	delete(h.ref, key)
	return deleted
}

// verify checks the tree against the reference model and against its own
// structural invariants. Run after every mutation in the randomized tests.
func (h *harness) verify() {
	h.t.Helper()

	// Every reference key is present with the right value.
	for key, want := range h.ref {
		got, ok := h.tree.Get([]byte(key))
		if !ok {
			h.t.Fatalf("key %q missing from the tree", key)
		}
		if string(got) != want {
			h.t.Fatalf("key %q: tree has %q, reference has %q", key, got, want)
		}
	}

	// The tree holds nothing the reference does not, and holds it in order.
	keys := h.collectKeys()
	if len(keys) != len(h.ref) {
		h.t.Fatalf("tree holds %d keys, reference holds %d", len(keys), len(h.ref))
	}
	if !sort.SliceIsSorted(keys, func(i, j int) bool { return keys[i] < keys[j] }) {
		h.t.Fatal("keys are not in sorted order across leaves")
	}
	for _, key := range keys {
		if _, ok := h.ref[key]; !ok {
			h.t.Fatalf("tree holds key %q that the reference does not", key)
		}
	}

	h.checkStructure()
	h.checkNoLeakedPages()
}

// collectKeys walks the leaves left to right, skipping the sentinel.
func (h *harness) collectKeys() []string {
	var keys []string
	if h.tree.root == 0 {
		return keys
	}
	var walk func(ptr uint64)
	walk = func(ptr uint64) {
		node := Node(h.pages[ptr])
		for i := uint16(0); i < node.keyCount(); i++ {
			switch node.nodeType() {
			case NodeLeaf:
				if len(node.key(i)) > 0 { // skip the sentinel empty key
					keys = append(keys, string(node.key(i)))
				}
			case NodeInternal:
				walk(node.childPtr(i))
			}
		}
	}
	walk(h.tree.root)
	return keys
}

// checkStructure asserts the B+tree invariants: sorted keys, bounded node
// sizes, correct separator keys, and every leaf at the same depth.
func (h *harness) checkStructure() {
	h.t.Helper()
	if h.tree.root == 0 {
		return
	}

	// The root, if internal, must have at least two children. A root with one
	// child means a level that should have been collapsed on the way out of a
	// delete — the tree would still answer every query correctly while
	// silently growing taller and never shrinking.
	if root := Node(h.pages[h.tree.root]); root.nodeType() == NodeInternal && root.keyCount() < 2 {
		h.t.Fatalf("root is internal with only %d child: a level was not collapsed",
			root.keyCount())
	}

	leafDepths := map[int]bool{}
	var walk func(ptr uint64, depth int, lowerBound []byte)
	walk = func(ptr uint64, depth int, lowerBound []byte) {
		node, ok := h.pages[ptr]
		if !ok {
			h.t.Fatalf("page %d referenced but not allocated", ptr)
		}
		if size := node.sizeBytes(); size > PageSize {
			h.t.Fatalf("page %d is oversized: %d > %d", ptr, size, PageSize)
		}
		if node.keyCount() == 0 {
			h.t.Fatalf("page %d holds no keys", ptr)
		}
		// A node's first key must match the separator its parent stored.
		if lowerBound != nil && !bytes.Equal(node.key(0), lowerBound) {
			h.t.Fatalf("page %d starts with key %q, parent's separator is %q",
				ptr, node.key(0), lowerBound)
		}
		// Keys strictly ascending within the node.
		for i := uint16(1); i < node.keyCount(); i++ {
			if bytes.Compare(node.key(i-1), node.key(i)) >= 0 {
				h.t.Fatalf("page %d: key %d (%q) is not less than key %d (%q)",
					ptr, i-1, node.key(i-1), i, node.key(i))
			}
		}

		if node.nodeType() == NodeLeaf {
			leafDepths[depth] = true
			return
		}
		for i := uint16(0); i < node.keyCount(); i++ {
			walk(node.childPtr(i), depth+1, node.key(i))
		}
	}
	walk(h.tree.root, 0, nil)

	if len(leafDepths) > 1 {
		h.t.Fatalf("tree is unbalanced: leaves found at depths %v", leafDepths)
	}
}

// checkNoLeakedPages asserts that every allocated page is reachable from the
// root and that no page is referenced twice. A leak here would, on disk, mean
// a file that grows forever.
func (h *harness) checkNoLeakedPages() {
	h.t.Helper()
	reachable := map[uint64]bool{}
	var walk func(ptr uint64)
	walk = func(ptr uint64) {
		if reachable[ptr] {
			h.t.Fatalf("page %d is referenced more than once", ptr)
		}
		reachable[ptr] = true
		node := Node(h.pages[ptr])
		if node.nodeType() == NodeInternal {
			for i := uint16(0); i < node.keyCount(); i++ {
				walk(node.childPtr(i))
			}
		}
	}
	if h.tree.root != 0 {
		walk(h.tree.root)
	}
	if len(reachable) != len(h.pages) {
		h.t.Fatalf("%d pages allocated but only %d reachable from the root: %d leaked",
			len(h.pages), len(reachable), len(h.pages)-len(reachable))
	}
}

// height returns the number of levels in the tree; 0 for an empty tree.
func (h *harness) height() int {
	if h.tree.root == 0 {
		return 0
	}
	levels := 1
	node := Node(h.pages[h.tree.root])
	for node.nodeType() == NodeInternal {
		levels++
		node = Node(h.pages[node.childPtr(0)])
	}
	return levels
}

// TestTreeHeightShrinks guards the property that structural checks alone miss:
// a tree that grows tall and never collapses answers every query correctly
// while degrading toward a linked list.
func TestTreeHeightShrinks(t *testing.T) {
	h := newHarness(t)
	const count = 4000
	for i := 0; i < count; i++ {
		h.insert(fmt.Sprintf("key%06d", i), fmt.Sprintf("value%d", i))
	}
	h.verify()

	// A 4 KiB node holds on the order of a hundred small pairs, so a few
	// thousand keys is comfortably more than one level but not necessarily
	// three. Two levels is enough to make the collapse below meaningful.
	tall := h.height()
	if tall < 2 {
		t.Fatalf("expected a multi-level tree from %d keys, got height %d", count, tall)
	}

	for i := 0; i < count; i++ {
		h.delete(fmt.Sprintf("key%06d", i))
	}
	h.verify()

	if short := h.height(); short != 1 {
		t.Fatalf("after deleting every key the tree should collapse to a single leaf, "+
			"height is %d (was %d when full)", short, tall)
	}
}

// TestTreeHeightIsLogarithmic is the complexity sanity check: ten times the
// data must not mean ten times the depth.
func TestTreeHeightIsLogarithmic(t *testing.T) {
	heightAt := func(count int) int {
		h := newHarness(t)
		val := string(bytes.Repeat([]byte("v"), 100))
		for i := 0; i < count; i++ {
			h.insert(fmt.Sprintf("key%08d", i), val)
		}
		return h.height()
	}
	small, large := heightAt(1000), heightAt(10000)
	if large > small+2 {
		t.Fatalf("height grew from %d to %d for a 10x larger dataset; "+
			"that is not logarithmic", small, large)
	}
	t.Logf("height %d at 1k keys, %d at 10k keys", small, large)
}

func TestInsertAndGet(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 500; i++ {
		h.insert(fmt.Sprintf("key%06d", i), fmt.Sprintf("value%d", i))
	}
	h.verify()

	if _, ok := h.tree.Get([]byte("nonexistent")); ok {
		t.Fatal("Get returned a key that was never inserted")
	}
}

func TestUpdateExistingKey(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 200; i++ {
		h.insert(fmt.Sprintf("key%03d", i), "original")
	}
	for i := 0; i < 200; i++ {
		h.insert(fmt.Sprintf("key%03d", i), "replaced")
	}
	h.verify()
	if len(h.collectKeys()) != 200 {
		t.Fatalf("updating existing keys changed the key count to %d", len(h.collectKeys()))
	}
}

func TestDeleteEverything(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 1000; i++ {
		h.insert(fmt.Sprintf("key%06d", i), fmt.Sprintf("value%d", i))
	}
	h.verify()
	for i := 0; i < 1000; i++ {
		h.delete(fmt.Sprintf("key%06d", i))
	}
	h.verify()
	if got := len(h.collectKeys()); got != 0 {
		t.Fatalf("tree still holds %d keys after deleting everything", got)
	}
}

// TestLargeKeyValues exercises the three-way split: a maximum-size pair cannot
// share a page with much else, so the splitter has to produce three nodes.
func TestLargeKeyValues(t *testing.T) {
	h := newHarness(t)
	big := string(bytes.Repeat([]byte("v"), MaxValSize))
	for i := 0; i < 60; i++ {
		h.insert(fmt.Sprintf("key%03d", i), big)
	}
	h.verify()

	// Interleave small pairs so oversized ones end up with neighbours.
	for i := 0; i < 60; i++ {
		h.insert(fmt.Sprintf("key%03d.small", i), "s")
	}
	h.verify()
}

func TestSizeLimitsRejected(t *testing.T) {
	h := newHarness(t)
	if err := h.tree.Insert(nil, []byte("v")); err == nil {
		t.Fatal("an empty key should be rejected; it is reserved for the sentinel")
	}
	if err := h.tree.Insert(bytes.Repeat([]byte("k"), MaxKeySize+1), []byte("v")); err == nil {
		t.Fatal("an oversized key should be rejected")
	}
	if err := h.tree.Insert([]byte("k"), bytes.Repeat([]byte("v"), MaxValSize+1)); err == nil {
		t.Fatal("an oversized value should be rejected")
	}
}

func TestDeleteMissingKey(t *testing.T) {
	h := newHarness(t)
	h.insert("present", "1")
	if h.delete("absent") {
		t.Fatal("deleting a missing key reported success")
	}
	h.verify()
}

// TestRandomizedOperations is the main correctness test: a long random walk
// through insert, update and delete, verified against the reference model
// after every single operation.
func TestRandomizedOperations(t *testing.T) {
	const operations = 20000
	h := newHarness(t)
	rng := rand.New(rand.NewSource(1))

	// A small key space so keys are frequently reused, which is what forces
	// updates, merges and re-splits rather than a monotonic build-up.
	keySpace := 400
	for op := 0; op < operations; op++ {
		key := fmt.Sprintf("key%04d", rng.Intn(keySpace))
		switch {
		case rng.Intn(100) < 60: // insert or update
			valLen := rng.Intn(200)
			if rng.Intn(50) == 0 { // occasionally a very large value
				valLen = MaxValSize - rng.Intn(100)
			}
			h.insert(key, string(bytes.Repeat([]byte("v"), valLen)))
		default:
			h.delete(key)
		}

		// Full verification is O(n); run it often enough to localize a bug
		// but not so often the test takes minutes.
		if op%97 == 0 {
			h.verify()
		}
	}
	h.verify()
	t.Logf("%d operations, %d live keys, %d pages live, %d pages freed",
		operations, len(h.ref), len(h.pages), h.freed)
}

// TestRandomizedSeeded runs several independent seeds, since a single random
// walk can miss a structural edge case entirely.
func TestRandomizedSeeded(t *testing.T) {
	for seed := int64(0); seed < 8; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			h := newHarness(t)
			rng := rand.New(rand.NewSource(seed))
			for op := 0; op < 3000; op++ {
				key := fmt.Sprintf("k%03d", rng.Intn(150))
				if rng.Intn(2) == 0 {
					h.insert(key, string(bytes.Repeat([]byte("x"), rng.Intn(600))))
				} else {
					h.delete(key)
				}
			}
			h.verify()
		})
	}
}

func BenchmarkInsert(b *testing.B) {
	h := &harness{pages: map[uint64]Node{}, ref: map[string]string{}, next: 1}
	h.tree = New(0,
		func(ptr uint64) []byte { return h.pages[ptr] },
		func(page []byte) uint64 {
			ptr := h.next
			h.next++
			owned := make(Node, PageSize)
			copy(owned, page)
			h.pages[ptr] = owned
			return ptr
		},
		func(ptr uint64) { delete(h.pages, ptr) },
	)
	val := bytes.Repeat([]byte("v"), 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = h.tree.Insert([]byte(fmt.Sprintf("key%08d", i)), val)
	}
}
