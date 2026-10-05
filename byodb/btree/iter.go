package btree

import "bytes"

// Comparison operators for Seek. The values are ordered and signed so that the
// sign says which way to step when the landing position is wrong: positive
// operators move forward, negative ones move backward.
const (
	CmpGE = +3 // >=
	CmpGT = +2 // >
	CmpLT = -2 // <
	CmpLE = -3 // <=
)

// Iter is a position within the tree.
//
// It holds the whole path from the root down to a leaf, because nodes carry no
// parent pointers: stepping past the end of a leaf means climbing back up to
// find the next sibling, and there is no other way back up.
type Iter struct {
	tree *BTree
	path []Node   // root to leaf
	pos  []uint16 // the index taken at each level
}

// invalidPos marks a position that has stepped off the start of the tree.
// Being larger than any possible key count, it also reads as invalid to
// Valid, which is what stepping off the end produces.
const invalidPos = ^uint16(0)

// Valid reports whether the iterator points at a key. It is the precondition
// for Deref.
func (iter *Iter) Valid() bool {
	if iter == nil || len(iter.path) == 0 {
		return false
	}
	leaf := len(iter.path) - 1
	return iter.pos[leaf] < iter.path[leaf].keyCount()
}

// Deref returns the key and value at the current position. The slices point
// into a page and are only valid until the next update.
func (iter *Iter) Deref() ([]byte, []byte) {
	leaf := len(iter.path) - 1
	node, idx := iter.path[leaf], iter.pos[leaf]
	return node.key(idx), node.value(idx)
}

// Next advances to the following key in sorted order.
func (iter *Iter) Next() {
	if len(iter.path) > 0 {
		iterNext(iter, len(iter.path)-1)
	}
}

// iterNext advances the position at one level, carrying up to the parent when
// this level is exhausted — the same shape as incrementing a number digit by
// digit, with a carry into the next column.
//
// It reports whether it moved. That return value is load-bearing: when the
// carry reaches the root and finds it exhausted, the frames unwinding beneath
// it must not go on to rebuild their child paths, or they would overwrite the
// past-the-end marker with position 0 and send the iterator round again. The
// bug only appears at three levels or more, since with two there is no frame
// left to unwind into.
func iterNext(iter *Iter, level int) bool {
	switch {
	case iter.pos[level]+1 < iter.path[level].keyCount():
		iter.pos[level]++ // room in this node
	case level > 0:
		if !iterNext(iter, level-1) { // exhausted; carry into the parent
			return false
		}
	default:
		// The root is exhausted, so the iterator steps past the last key.
		iter.pos[len(iter.pos)-1] = iter.path[len(iter.path)-1].keyCount()
		return false
	}
	// Levels below the one that moved must be rebuilt: a different parent
	// index means a different child node, entered at its first key.
	if level+1 < len(iter.pos) {
		child := Node(iter.tree.pageRead(iter.path[level].childPtr(iter.pos[level])))
		iter.path[level+1] = child
		iter.pos[level+1] = 0
	}
	return true
}

// Prev steps back to the preceding key in sorted order.
func (iter *Iter) Prev() {
	if len(iter.path) > 0 {
		iterPrev(iter, len(iter.path)-1)
	}
}

// iterPrev mirrors iterNext, including the reason for the return value.
func iterPrev(iter *Iter, level int) bool {
	switch {
	case iter.pos[level] > 0:
		iter.pos[level]--
	case level > 0:
		if !iterPrev(iter, level-1) {
			return false
		}
	default:
		iter.pos[len(iter.pos)-1] = invalidPos // before the first key
		return false
	}
	// Entering a child from the right means starting at its last key.
	if level+1 < len(iter.pos) {
		child := Node(iter.tree.pageRead(iter.path[level].childPtr(iter.pos[level])))
		iter.path[level+1] = child
		iter.pos[level+1] = child.keyCount() - 1
	}
	return true
}

// SeekLE positions the iterator at the greatest key less than or equal to the
// target.
//
// It is a point lookup that records the path on the way down. The search can
// never fail to land somewhere, because the tree always holds a sentinel empty
// key below every real key.
func (tree *BTree) SeekLE(key []byte) *Iter {
	iter := &Iter{tree: tree}
	for ptr := tree.root; ptr != 0; {
		node := Node(tree.pageRead(ptr))
		idx := lookupLessOrEqual(node, key)
		iter.path = append(iter.path, node)
		iter.pos = append(iter.pos, idx)
		if node.nodeType() == NodeLeaf {
			break
		}
		ptr = node.childPtr(idx)
	}
	return iter
}

// Seek positions the iterator at the first key satisfying `key <op> target`,
// where op is one of CmpGE, CmpGT, CmpLT, CmpLE.
//
// It lands with SeekLE and then takes at most one step. The direction of that
// step is the sign of the operator: a `>=` or `>` query that landed too low
// steps forward, a `<` query that landed too high steps back.
func (tree *BTree) Seek(key []byte, cmp int) *Iter {
	iter := tree.SeekLE(key)
	if !iter.Valid() {
		return iter
	}
	found, _ := iter.Deref()
	if compareOK(found, cmp, key) {
		return iter
	}
	if cmp > 0 {
		iter.Next()
	} else {
		iter.Prev()
	}
	return iter
}

// compareOK reports whether `key <op> ref` holds.
func compareOK(key []byte, cmp int, ref []byte) bool {
	r := bytes.Compare(key, ref)
	switch cmp {
	case CmpGE:
		return r >= 0
	case CmpGT:
		return r > 0
	case CmpLT:
		return r < 0
	case CmpLE:
		return r <= 0
	default:
		panic("byodb: unrecognized comparison operator")
	}
}

// CompareOK exposes the operator semantics so callers can test a key against a
// range boundary without duplicating the mapping.
func CompareOK(key []byte, cmp int, ref []byte) bool { return compareOK(key, cmp, ref) }
