package kv

import (
	"encoding/binary"
	"fmt"

	"byodb/internal/assert"
)

// The free list holds page numbers that copy-on-write has orphaned, so they
// can be handed out again instead of the file growing forever.
//
// It is an unrolled linked list of pages, which is what lets it manage its own
// space: when a list node empties it is fed back into the list, and when a new
// node is needed the list tries to take one from itself before appending.
//
// Each entry carries the version at which the page was freed. A page freed by
// version V may still be referenced by version V-1, which a reader can still
// be holding, so it cannot be reused until every live reader has moved past
// it. That check is what makes concurrent readers safe.
//
// Node layout:
//
//	| next | (page, version) pairs | unused |
//	|  8B  |      n * 16B          |  ...   |
const (
	listHeaderSize = 8
	listEntrySize  = 16
	// listNodeCap is how many entries one node holds.
	listNodeCap = (PageSize - listHeaderSize) / listEntrySize
)

type listNode []byte

func (n listNode) next() uint64       { return binary.LittleEndian.Uint64(n[0:8]) }
func (n listNode) setNext(ptr uint64) { binary.LittleEndian.PutUint64(n[0:8], ptr) }

func (n listNode) entryAt(idx int) (page, version uint64) {
	if idx < 0 || idx >= listNodeCap {
		panic(fmt.Sprintf("byodb: free list index %d out of range", idx))
	}
	pos := listHeaderSize + listEntrySize*idx
	return binary.LittleEndian.Uint64(n[pos:]), binary.LittleEndian.Uint64(n[pos+8:])
}

func (n listNode) setEntryAt(idx int, page, version uint64) {
	if idx < 0 || idx >= listNodeCap {
		panic(fmt.Sprintf("byodb: free list index %d out of range", idx))
	}
	pos := listHeaderSize + listEntrySize*idx
	binary.LittleEndian.PutUint64(n[pos:], page)
	binary.LittleEndian.PutUint64(n[pos+8:], version)
}

// freeList is a FIFO queue of reusable page numbers.
//
// FIFO rather than LIFO on purpose: the oldest freed pages are the ones no
// live reader can still need, so they are the safest to hand out — and the
// version stamps at the head of the list are the oldest, so a single check
// there is enough to stop.
type freeList struct {
	// Page callbacks. There is no delete callback: the list is what pages are
	// deleted *into*.
	readPage   func(ptr uint64) []byte  // read a page
	appendPage func(page []byte) uint64 // append a brand new page
	writePage  func(ptr uint64) []byte  // get a writable copy of a page

	// Persisted in the meta page.
	headPage uint64 // node entries are consumed from
	headSeq  uint64 // monotonic position within the head node
	tailPage uint64 // node entries are appended to
	tailSeq  uint64 // monotonic position within the tail node

	// In-memory only.
	//
	// maxSeq freezes the tail as it stood when the update began, so an update
	// cannot consume the pages it just freed. Those pages still belong to the
	// version currently committed on disk.
	maxSeq uint64
	// maxVer is the version of the oldest live reader. A page freed at a
	// later version is still visible to that reader and must not be reused.
	maxVer uint64
	// curVer is the version being created, stamped onto pages freed now.
	curVer uint64
}

// seqToIndex maps a monotonic sequence number onto a slot within a node.
//
// The sequence numbers keep increasing rather than wrapping so that they
// double as unique positions in the list: comparing head against tail says
// directly whether the list is exhausted, with no ambiguity about a wrapped
// index meaning "full" or "empty".
func seqToIndex(seq uint64) int { return int(seq % listNodeCap) }

// beginUpdate releases the pages freed by the previous update for reuse, and
// records the versions that bound reuse for the update about to run.
func (fl *freeList) beginUpdate(curVer, maxVer uint64) {
	fl.maxSeq = fl.tailSeq
	fl.curVer = curVer
	fl.maxVer = maxVer
}

// takeFromHead removes one page number from the head node.
//
// It also reports the head node itself when that node has just been emptied,
// so the caller can recycle it. It is not pushed back here because pushing can
// itself consume from the head, and the recursion has to be broken somewhere.
func (fl *freeList) takeFromHead() (page uint64, emptiedNode uint64) {
	if fl.headSeq == fl.maxSeq {
		return 0, 0 // nothing available to this update
	}
	node := listNode(fl.readPage(fl.headPage))
	page, version := node.entryAt(seqToIndex(fl.headSeq))
	if version > fl.maxVer {
		// This page, and everything after it, was freed too recently: a
		// reader is still looking at a version that references it.
		return 0, 0
	}
	fl.headSeq++

	if seqToIndex(fl.headSeq) == 0 {
		// The head node is used up; advance to the next one and hand the
		// empty node back to the caller.
		emptiedNode, fl.headPage = fl.headPage, node.next()
		assert.True(fl.headPage != 0, "the free list must never become empty")
	}
	return page, emptiedNode
}

// pop returns a reusable page number, or 0 if none is available.
func (fl *freeList) pop() uint64 {
	page, emptied := fl.takeFromHead()
	if emptied != 0 {
		fl.push(emptied) // the list recycles its own nodes
	}
	return page
}

// push adds a page number to the tail for reuse by a later update.
func (fl *freeList) push(page uint64) {
	listNode(fl.writePage(fl.tailPage)).setEntryAt(seqToIndex(fl.tailSeq), page, fl.curVer)
	fl.tailSeq++

	if seqToIndex(fl.tailSeq) != 0 {
		return
	}
	// The tail node is full. A new one is linked immediately rather than
	// lazily, so the list always has at least one node — a list that can
	// become empty needs special cases everywhere.
	next, emptied := fl.takeFromHead() // prefer a page from the list itself
	if next == 0 {
		next = fl.appendPage(make([]byte, PageSize))
	}
	listNode(fl.writePage(fl.tailPage)).setNext(next)
	fl.tailPage = next

	if emptied != 0 {
		// takeFromHead emptied a node on the way; it belongs in the list too,
		// and the fresh tail has room for it.
		listNode(fl.writePage(fl.tailPage)).setEntryAt(0, emptied, fl.curVer)
		fl.tailSeq++
	}
}
