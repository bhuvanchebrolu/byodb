package kv

import (
	"math/rand"
	"testing"
)

// pageStore is a stand-in for KV's page management, with the same
// staged-versus-committed split: pages modified during an update live in
// `staged` and only join `committed` when the update commits.
//
// Reads must consult staged pages first. Reading the committed copy of a page
// the current update has already modified would silently return stale bytes —
// including a stale `next` pointer, which is how a linked list loses its tail.
type pageStore struct {
	t         *testing.T
	committed map[uint64][]byte
	staged    map[uint64][]byte
	nextPage  uint64
}

func newPageStore(t *testing.T) *pageStore {
	s := &pageStore{
		t:         t,
		committed: map[uint64][]byte{},
		staged:    map[uint64][]byte{},
		nextPage:  1,
	}
	s.committed[s.newPageNumber()] = make([]byte, PageSize) // the list's first node
	return s
}

func (s *pageStore) newPageNumber() uint64 {
	ptr := s.nextPage
	s.nextPage++
	return ptr
}

func (s *pageStore) read(ptr uint64) []byte {
	if page, ok := s.staged[ptr]; ok {
		return page
	}
	page, ok := s.committed[ptr]
	if !ok {
		s.t.Fatalf("read of page %d, which was never written", ptr)
	}
	return page
}

func (s *pageStore) write(ptr uint64) []byte {
	if page, ok := s.staged[ptr]; ok {
		return page
	}
	page := make([]byte, PageSize)
	if existing, ok := s.committed[ptr]; ok {
		copy(page, existing)
	}
	s.staged[ptr] = page
	return page
}

func (s *pageStore) append(page []byte) uint64 {
	ptr := s.newPageNumber()
	s.staged[ptr] = page
	return ptr
}

func (s *pageStore) commit() {
	for ptr, page := range s.staged {
		s.committed[ptr] = page
	}
	clear(s.staged)
}

// newFreeList builds a list over a fake store, starting from the single node
// the store seeds at page 1.
func newFreeList(store *pageStore) *freeList {
	return &freeList{
		readPage:   store.read,
		appendPage: store.append,
		writePage:  store.write,
		headPage:   1,
		tailPage:   1,
	}
}

// versionClock models the store's commit counter for tests that do not care
// about reader visibility: every version is immediately reusable because no
// reader is held open.
type versionClock struct{ now uint64 }

func (c *versionClock) next() uint64 { c.now++; return c.now }
func (c *versionClock) max() uint64  { return c.now }

// TestFreeListFIFOWithinNode checks the ordering guarantee at a scale small
// enough that the list never needs a second node.
//
// That restriction is what makes the assertion meaningful: once the list grows
// past one node it starts consuming queued pages to use as nodes and pushing
// emptied nodes back to the tail, so a page's position legitimately changes
// and global push order is no longer the property it holds.
//
// FIFO matters because the oldest freed pages are the safest to hand out: a
// page freed recently may still be referenced by the version a reader is
// looking at. Stage 9 turns that into an explicit version check.
func TestFreeListFIFOWithinNode(t *testing.T) {
	clock := &versionClock{}
	nextVer := clock.next
	maxVer := clock.max
	_ = nextVer
	store := newPageStore(t)
	fl := newFreeList(store)

	// Pin the node capacity: a node must hold exactly listNodeCap page
	// numbers before a new one is linked. Getting this wrong wastes a slot
	// per node silently, and changes the on-disk meaning of a sequence
	// number, so an existing file would be read against the wrong layout.
	func() {
		probeStore := newPageStore(t)
		probe := newFreeList(probeStore)
		for i := 0; i < listNodeCap-1; i++ {
			probe.push(uint64(5000 + i))
		}
		if probe.tailPage != 1 {
			t.Fatalf("the tail node moved after only %d pushes; a node should hold %d",
				listNodeCap-1, listNodeCap)
		}
		probe.push(9999) // fills the last slot, so a fresh tail is linked
		if probe.tailPage == 1 {
			t.Fatalf("the tail node did not move after %d pushes; a node holds only %d",
				listNodeCap, listNodeCap)
		}
	}()

	const batch = 50 // comfortably inside one node
	var expected []uint64
	next := uint64(1000)

	for round := 0; round < 5; round++ {
		for i := 0; i < batch; i++ {
			next++
			fl.push(next)
			// Pushed during this update, so it must not come back yet.
			if got := fl.pop(); got != 0 && got == next {
				t.Fatalf("pop returned page %d in the same update it was pushed", next)
			}
			expected = append(expected, next)
		}
		store.commit()
		fl.beginUpdate(nextVer(), maxVer())

		for i := 0; i < batch; i++ {
			got := fl.pop()
			if got == 0 {
				t.Fatalf("round %d: pop returned nothing with %d pages queued", round, len(expected))
			}
			if got != expected[0] {
				t.Fatalf("round %d: pop returned page %d, want %d (oldest first)",
					round, got, expected[0])
			}
			expected = expected[1:]
		}
		store.commit()
		fl.beginUpdate(nextVer(), maxVer())
	}
}

// TestFreeListAcrossNodes drives the list past its node boundaries, which is
// where its self-management happens: a node that empties is recycled into the
// list, and a node that fills is linked to a fresh one taken from the list
// itself.
//
// Small updates never reach that code — one insert frees a handful of pages
// while a node holds several hundred page numbers — so it is exercised here
// directly rather than through Set and Del.
func TestFreeListAcrossNodes(t *testing.T) {
	clock := &versionClock{}
	nextVer := clock.next
	maxVer := clock.max
	_ = nextVer
	store := newPageStore(t)
	fl := newFreeList(store)

	// At this scale the list recycles its own nodes, so a page's queue
	// position legitimately changes and push order is not preserved. What
	// must hold regardless is safety and liveness: nothing is handed out
	// while the committed version still needs it, nothing is handed out
	// twice, and pages do come back rather than leaking.
	pushedThisUpdate := map[uint64]bool{}
	outstanding := map[uint64]bool{}
	everPushed := map[uint64]bool{}
	recovered := map[uint64]bool{}
	pushes, pops := 0, 0

	rng := rand.New(rand.NewSource(11))
	nextValue := uint64(1000)

	// Enough traffic to fill, empty and recycle many nodes.
	for update := 0; update < 60; update++ {
		clear(pushedThisUpdate)
		for i := rng.Intn(3 * listNodeCap); i > 0; i-- {
			nextValue++
			pushedThisUpdate[nextValue] = true
			everPushed[nextValue] = true
			delete(outstanding, nextValue)
			fl.push(nextValue)
			pushes++
		}

		for i := rng.Intn(3 * listNodeCap); i > 0; i-- {
			got := fl.pop()
			if got == 0 {
				continue
			}
			pops++
			if pushedThisUpdate[got] {
				t.Fatalf("update %d: pop returned page %d, which was pushed during this "+
					"same update and is still referenced by the committed version", update, got)
			}
			if outstanding[got] {
				t.Fatalf("update %d: page %d was handed out while already in use", update, got)
			}
			outstanding[got] = true
			recovered[got] = true
		}

		store.commit()
		fl.beginUpdate(nextVer(), maxVer())

		if fl.headPage == 0 || fl.tailPage == 0 {
			t.Fatalf("update %d: the free list lost a node (head=%d tail=%d)",
				update, fl.headPage, fl.tailPage)
		}
		if fl.headSeq > fl.tailSeq {
			t.Fatalf("update %d: head sequence %d overran tail %d",
				update, fl.headSeq, fl.tailSeq)
		}
	}

	// The list must actually be recycling, not just accepting pushes.
	if pops < pushes/2 {
		t.Fatalf("only %d of %d pushed pages were handed back out; "+
			"the list is not recycling", pops, pushes)
	}

	// Conservation: drain the list and require that every page ever pushed is
	// accounted for — either handed back out, or currently serving as one of
	// the list's own nodes. A page that is neither has been leaked, and a
	// leaked page is space the database can never reclaim. Nothing else in
	// the suite would notice.
	const drainLimit = 1 << 20
	drained := 0
	for {
		if drained++; drained > drainLimit {
			t.Fatal("draining the free list did not terminate")
		}
		got := fl.pop()
		if got == 0 {
			// maxSeq may simply be blocking; commit and try once more before
			// concluding the list is empty.
			store.commit()
			fl.beginUpdate(nextVer(), maxVer())
			if got = fl.pop(); got == 0 {
				break
			}
		}
		recovered[got] = true
	}

	inChain := map[uint64]bool{}
	for page := fl.headPage; page != 0; page = listNode(store.read(page)).next() {
		if inChain[page] {
			break
		}
		inChain[page] = true
		if page == fl.tailPage {
			break
		}
	}

	leaked := 0
	var example uint64
	for page := range everPushed {
		if !recovered[page] && !inChain[page] {
			leaked++
			example = page
		}
	}
	if leaked > 0 {
		t.Fatalf("%d of %d pushed pages were lost — for example page %d is neither "+
			"queued, handed out, nor a list node", leaked, len(everPushed), example)
	}

	// Everything still queued must be reachable by walking the list, which
	// only holds if every `next` pointer was read back correctly as nodes
	// were linked and recycled.
	seen := map[uint64]bool{}
	nodes := 0
	for page := fl.headPage; page != 0; page = listNode(store.read(page)).next() {
		if seen[page] {
			t.Fatalf("the free list node chain loops back to page %d", page)
		}
		seen[page] = true
		nodes++
		if page == fl.tailPage {
			break
		}
	}
	if !seen[fl.tailPage] {
		t.Fatalf("walking from head page %d never reached tail page %d: the chain is broken",
			fl.headPage, fl.tailPage)
	}

	t.Logf("%d pushes, %d pops, %d pages still queued across %d list nodes, "+
		"%d pages allocated in total",
		pushes, pops, fl.tailSeq-fl.headSeq, nodes, store.nextPage-1)
}

// TestFreeListRecyclesNodeEmptiedWhileLinking targets one exact coincidence:
// the tail node filling up at the same moment that taking a page from the head
// empties the head node.
//
// Both events are handled separately and correctly; it is their overlap that
// is delicate, because the page taken from the head becomes the new tail while
// the node it came from also needs recycling. Dropping that node leaks a page
// permanently, and random traffic almost never lines the two boundaries up, so
// the situation is built here by hand.
func TestFreeListRecyclesNodeEmptiedWhileLinking(t *testing.T) {
	clock := &versionClock{}
	nextVer := clock.next
	maxVer := clock.max
	_ = nextVer
	store := newPageStore(t)
	fl := newFreeList(store)

	everPushed := map[uint64]bool{}
	next := uint64(100000)
	push := func(n int) {
		for i := 0; i < n; i++ {
			next++
			everPushed[next] = true
			fl.push(next)
		}
	}

	// Fill two whole nodes and make them available for reuse.
	push(2 * listNodeCap)
	store.commit()
	fl.beginUpdate(nextVer(), maxVer())

	// Walk the head to one page short of a node boundary.
	recovered := map[uint64]bool{}
	for i := 0; i < listNodeCap-1; i++ {
		got := fl.pop()
		if got == 0 {
			t.Fatalf("pop %d returned nothing while pages were queued", i)
		}
		recovered[got] = true
	}

	// Now fill the tail node. The push that fills its last slot links a new
	// tail, which it takes from the head — and that take is the one that
	// empties the head node.
	headBefore := fl.headPage
	push(listNodeCap)
	if fl.headPage == headBefore {
		t.Fatalf("the head node did not advance; the overlapping case was not reached "+
			"(head=%d headSeq=%d tailSeq=%d)", fl.headPage, fl.headSeq, fl.tailSeq)
	}

	// Drain everything and require that no page vanished.
	store.commit()
	fl.beginUpdate(nextVer(), maxVer())
	for drained := 0; ; drained++ {
		if drained > 1<<20 {
			t.Fatal("draining the free list did not terminate")
		}
		got := fl.pop()
		if got == 0 {
			store.commit()
			fl.beginUpdate(nextVer(), maxVer())
			if got = fl.pop(); got == 0 {
				break
			}
		}
		recovered[got] = true
	}

	inChain := map[uint64]bool{}
	for page := fl.headPage; page != 0; page = listNode(store.read(page)).next() {
		if inChain[page] {
			break
		}
		inChain[page] = true
		if page == fl.tailPage {
			break
		}
	}

	for page := range everPushed {
		if !recovered[page] && !inChain[page] {
			t.Fatalf("page %d was lost when the head node emptied while a new tail "+
				"was being linked", page)
		}
	}
}
