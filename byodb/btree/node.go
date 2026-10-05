// Package btree implements a copy-on-write B+tree.
//
// The tree performs no IO of its own. It reaches storage through three
// callbacks supplied by the caller (pageRead, pageAlloc, pageFree), which is
// what allows the whole data structure to be built and tested entirely in
// memory before any file exists.
package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"byodb/internal/assert"
)

// panicf reports a violated invariant. It exists so that the hot accessors can
// check their bounds without paying to format a message that is almost never
// produced.
func panicf(format string, args ...any) {
	panic("byodb invariant violated: " + fmt.Sprintf(format, args...))
}

// Node layout on disk. Every node occupies exactly one page.
//
//	| type | keyCount |   child ptrs   |   kv offsets   | key-values | unused |
//	|  2B  |    2B    | keyCount * 8B  | keyCount * 2B  |    ...     |        |
//
// Each key-value pair is stored as:
//
//	| keyLen | valLen | key | val |
//	|   2B   |   2B   | ... | ... |
//
// The offsets array exists so the nth pair can be located in O(1) despite
// keys and values being variable length, which in turn makes binary search
// inside a node possible.
const (
	// HeaderSize is the fixed node header: node type plus key count.
	HeaderSize = 4

	// PageSize is the size of every node, matching the typical OS page size.
	PageSize = 4096

	// MaxKeySize and MaxValSize bound a single pair so that one pair plus a
	// header always fits inside one page. That guarantee is what lets a split
	// produce at most three nodes.
	MaxKeySize = 1000
	MaxValSize = 3000
)

// Node types.
const (
	NodeInternal = 1 // internal node: child pointers, keys, no values
	NodeLeaf     = 2 // leaf node: keys and values, no child pointers
)

func init() {
	// The worst case single pair: header + one child pointer + one offset +
	// the pair's own length prefixes + a maximum key and value.
	largestSinglePair := HeaderSize + 8 + 2 + 4 + MaxKeySize + MaxValSize
	assert.True(largestSinglePair <= PageSize,
		"a maximum-size key/value pair (%d bytes) must fit in one page (%d)",
		largestSinglePair, PageSize)
}

// Node is a page-sized chunk of bytes interpreted by the layout above.
//
// The in-memory representation is the on-disk representation, so writing a
// node to a page needs no serialization step.
type Node []byte

// nodeType reports whether this is an internal or a leaf node.
func (n Node) nodeType() uint16 {
	return binary.LittleEndian.Uint16(n[0:2])
}

// keyCount is the number of keys held in this node. For an internal node it is
// also the number of children, since each child is labelled by a copy of the
// smallest key in its subtree.
func (n Node) keyCount() uint16 {
	return binary.LittleEndian.Uint16(n[2:4])
}

func (n Node) setHeader(nodeType, keyCount uint16) {
	binary.LittleEndian.PutUint16(n[0:2], nodeType)
	binary.LittleEndian.PutUint16(n[2:4], keyCount)
}

// The bounds checks below are written as explicit ifs rather than as calls to
// a variadic assert helper. These accessors run millions of times per second,
// and passing arguments to a variadic function boxes them on every call —
// whether or not the check fails. Formatting only on the failing path keeps
// the checks free.

// childPtr returns the page number of the idx-th child. Only meaningful for
// internal nodes; leaf nodes carry a zero in every slot.
func (n Node) childPtr(idx uint16) uint64 {
	if idx >= n.keyCount() {
		panicf("child index %d out of range (%d keys)", idx, n.keyCount())
	}
	pos := HeaderSize + 8*uint32(idx)
	return binary.LittleEndian.Uint64(n[pos:])
}

func (n Node) setChildPtr(idx uint16, ptr uint64) {
	if idx >= n.keyCount() {
		panicf("child index %d out of range (%d keys)", idx, n.keyCount())
	}
	pos := HeaderSize + 8*uint32(idx)
	binary.LittleEndian.PutUint64(n[pos:], ptr)
}

// offsetSlotPos is the byte position of the stored offset for pair idx.
// Offset 0 is implicit (always zero) and therefore not stored, so idx counts
// from 1 here.
func (n Node) offsetSlotPos(idx uint16) uint32 {
	if idx < 1 || idx > n.keyCount() {
		panicf("offset index %d out of range (%d keys)", idx, n.keyCount())
	}
	return HeaderSize + 8*uint32(n.keyCount()) + 2*uint32(idx-1)
}

// kvEndOffset returns the end position of pair idx, measured from the start of
// the key-value area. Equivalently: the start of pair idx+1. kvEndOffset(0) is
// zero, and kvEndOffset(keyCount) is the total size of the key-value area.
func (n Node) kvEndOffset(idx uint16) uint16 {
	if idx == 0 {
		return 0
	}
	return binary.LittleEndian.Uint16(n[n.offsetSlotPos(idx):])
}

func (n Node) setKVEndOffset(idx uint16, offset uint16) {
	if idx == 0 {
		if offset != 0 {
			panicf("offset 0 is implicit and must be zero, got %d", offset)
		}
		return
	}
	binary.LittleEndian.PutUint16(n[n.offsetSlotPos(idx):], offset)
}

// kvPos is the byte position of pair idx within the whole node.
func (n Node) kvPos(idx uint16) uint32 {
	if idx > n.keyCount() {
		panicf("kv index %d out of range (%d keys)", idx, n.keyCount())
	}
	return HeaderSize + 8*uint32(n.keyCount()) + 2*uint32(n.keyCount()) + uint32(n.kvEndOffset(idx))
}

// key returns the idx-th key. The returned slice aliases the node's bytes.
func (n Node) key(idx uint16) []byte {
	if idx >= n.keyCount() {
		panicf("key index %d out of range (%d keys)", idx, n.keyCount())
	}
	pos := n.kvPos(idx)
	keyLen := binary.LittleEndian.Uint16(n[pos:])
	return n[pos+4:][:keyLen]
}

// value returns the idx-th value. Internal nodes store an empty value.
func (n Node) value(idx uint16) []byte {
	if idx >= n.keyCount() {
		panicf("value index %d out of range (%d keys)", idx, n.keyCount())
	}
	pos := n.kvPos(idx)
	keyLen := binary.LittleEndian.Uint16(n[pos+0:])
	valLen := binary.LittleEndian.Uint16(n[pos+2:])
	return n[pos+4+uint32(keyLen):][:valLen]
}

// sizeBytes is the used size of the node. It falls out of an off-by-one
// lookup: the end of the last pair is the end of the node's content.
func (n Node) sizeBytes() uint32 {
	return n.kvPos(n.keyCount())
}

// lookupLessOrEqual finds the last position whose key is <= the target.
//
// For an internal node this is the child whose subtree may contain the key;
// for a leaf it is the candidate position of the key itself. The search never
// falls off the left edge because the tree always holds a sentinel empty key,
// which sorts below every real key.
//
// Position 0 needs no comparison: in an internal node it is a copy of the key
// the parent stored for this subtree, so it is always <= the target. The
// binary search therefore covers 1..keyCount, which is what the offsets array
// in the node format exists to make possible — without it, finding the nth
// pair would mean walking the variable-length pairs one at a time.
func lookupLessOrEqual(node Node, key []byte) uint16 {
	found := uint16(0)
	lo, hi := uint16(1), node.keyCount() // the half-open range still to search
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(node.key(mid), key) <= 0 {
			found = mid // a candidate; anything better is to the right
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return found
}

// appendKV writes one key-value pair (and its child pointer) at position idx.
//
// Pairs must be appended in ascending idx order: each one derives its start
// position from the end offset written by the previous call.
func appendKV(dst Node, idx uint16, childPtr uint64, key, val []byte) {
	dst.setChildPtr(idx, childPtr)

	pos := dst.kvPos(idx)
	binary.LittleEndian.PutUint16(dst[pos+0:], uint16(len(key)))
	binary.LittleEndian.PutUint16(dst[pos+2:], uint16(len(val)))
	copy(dst[pos+4:], key)
	copy(dst[pos+4+uint32(len(key)):], val)

	// Record where the next pair begins.
	dst.setKVEndOffset(idx+1, dst.kvEndOffset(idx)+4+uint16(len(key)+len(val)))
}

// appendKVRange copies n consecutive pairs from src starting at srcIdx into
// dst starting at dstIdx, preserving child pointers.
//
// dst's header must already be set, since the pointer and offset arrays are
// sized from its key count.
func appendKVRange(dst, src Node, dstIdx, srcIdx, n uint16) {
	if srcIdx+n > src.keyCount() {
		panicf("source range [%d,%d) exceeds %d keys", srcIdx, srcIdx+n, src.keyCount())
	}
	if dstIdx+n > dst.keyCount() {
		panicf("destination range [%d,%d) exceeds %d keys", dstIdx, dstIdx+n, dst.keyCount())
	}
	if n == 0 {
		return
	}

	// Child pointers.
	for i := uint16(0); i < n; i++ {
		dst.setChildPtr(dstIdx+i, src.childPtr(srcIdx+i))
	}

	// Offsets, rebased from src's key-value area onto dst's.
	dstBegin := dst.kvEndOffset(dstIdx)
	srcBegin := src.kvEndOffset(srcIdx)
	for i := uint16(1); i <= n; i++ {
		dst.setKVEndOffset(dstIdx+i, dstBegin+src.kvEndOffset(srcIdx+i)-srcBegin)
	}

	// The pair bytes themselves, in one copy.
	copy(dst[dst.kvPos(dstIdx):], src[src.kvPos(srcIdx):src.kvPos(srcIdx+n)])
}
