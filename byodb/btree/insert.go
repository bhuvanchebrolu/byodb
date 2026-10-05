package btree

import (
	"bytes"

	"byodb/internal/assert"
)

// leafInsert produces a copy of old with a new pair inserted at idx.
func leafInsert(dst, old Node, idx uint16, key, val []byte) {
	dst.setHeader(NodeLeaf, old.keyCount()+1)
	appendKVRange(dst, old, 0, 0, idx)
	appendKV(dst, idx, 0, key, val)
	appendKVRange(dst, old, idx+1, idx, old.keyCount()-idx)
}

// leafUpdate produces a copy of old with the pair at idx replaced, rather than
// inserting a duplicate key.
func leafUpdate(dst, old Node, idx uint16, key, val []byte) {
	dst.setHeader(NodeLeaf, old.keyCount())
	appendKVRange(dst, old, 0, 0, idx)
	appendKV(dst, idx, 0, key, val)
	appendKVRange(dst, old, idx+1, idx+1, old.keyCount()-(idx+1))
}

// replaceChildren rebuilds an internal node with the child at idx replaced by
// one or more children — more than one when the child was split.
//
// Each child is allocated here, and labelled by the smallest key in its
// subtree.
func replaceChildren(tree *BTree, dst, old Node, idx uint16, children ...Node) {
	added := uint16(len(children))
	dst.setHeader(NodeInternal, old.keyCount()+added-1)
	appendKVRange(dst, old, 0, 0, idx)
	for i, child := range children {
		appendKV(dst, idx+uint16(i), tree.pageAlloc(child), child.key(0), nil)
	}
	appendKVRange(dst, old, idx+added, idx+1, old.keyCount()-(idx+1))
}

// splitInTwo divides an oversized node so that the right half is guaranteed to
// fit in one page. The left half may still be oversized and need splitting
// again — see splitNode.
func splitInTwo(left, right, old Node) {
	assert.True(old.keyCount() >= 2, "cannot split a node with %d keys", old.keyCount())

	// Size of the left half if it took the first n pairs.
	leftBytes := func(n uint16) uint32 {
		return HeaderSize + 8*uint32(n) + 2*uint32(n) + uint32(old.kvEndOffset(n))
	}
	// Size of the right half if the left took the first n pairs. The old
	// node's size minus the left's, plus the right's own header.
	rightBytes := func(n uint16) uint32 {
		return old.sizeBytes() - leftBytes(n) + HeaderSize
	}

	// Start at the midpoint, shrink the left until it fits, then grow it back
	// until the right fits. The key size limit guarantees this converges with
	// at least one pair on each side.
	nLeft := old.keyCount() / 2
	for leftBytes(nLeft) > PageSize {
		nLeft--
	}
	assert.True(nLeft >= 1, "left half of a split must hold at least one key")
	for rightBytes(nLeft) > PageSize {
		nLeft++
	}
	assert.True(nLeft < old.keyCount(), "right half of a split must hold at least one key")

	nRight := old.keyCount() - nLeft
	left.setHeader(old.nodeType(), nLeft)
	right.setHeader(old.nodeType(), nRight)
	appendKVRange(left, old, 0, 0, nLeft)
	appendKVRange(right, old, 0, nLeft, nRight)

	assert.True(right.sizeBytes() <= PageSize, "right half still oversized after split")
}

// splitNode splits an oversized node into one, two or three page-sized nodes.
//
// Three are needed in the worst case: a single very large pair sitting between
// two others forces the left half to be split a second time.
//
// The returned nodes live in memory only; replaceChildren is what actually
// allocates pages for them.
func splitNode(old Node) (uint16, [3]Node) {
	if old.sizeBytes() <= PageSize {
		return 1, [3]Node{old[:PageSize]}
	}

	left := Node(make([]byte, 2*PageSize)) // may still be oversized
	right := Node(make([]byte, PageSize))
	splitInTwo(left, right, old)
	if left.sizeBytes() <= PageSize {
		return 2, [3]Node{left[:PageSize], right}
	}

	farLeft := Node(make([]byte, PageSize))
	middle := Node(make([]byte, PageSize))
	splitInTwo(farLeft, middle, left)
	assert.True(farLeft.sizeBytes() <= PageSize, "a node split three ways is still oversized")
	return 3, [3]Node{farLeft, middle, right}
}

// treeInsert inserts a pair into the subtree rooted at node and returns the
// updated subtree root.
//
// The result may exceed one page; the caller splits it. The caller is also
// responsible for freeing the input node and allocating the results.
func treeInsert(tree *BTree, node Node, key, val []byte) Node {
	// Allow room for overflow; the caller splits if needed.
	updated := Node(make([]byte, 2*PageSize))

	idx := lookupLessOrEqual(node, key)
	switch node.nodeType() {
	case NodeLeaf:
		if bytes.Equal(key, node.key(idx)) {
			leafUpdate(updated, node, idx, key, val)
		} else {
			leafInsert(updated, node, idx+1, key, val)
		}
	case NodeInternal:
		insertIntoInternal(tree, updated, node, idx, key, val)
	default:
		panic("byodb: unrecognized node type")
	}
	return updated
}

// insertIntoInternal recurses into the child at idx, splits whatever comes
// back, and rewrites the parent's child links.
func insertIntoInternal(tree *BTree, dst, node Node, idx uint16, key, val []byte) {
	childPtr := node.childPtr(idx)
	updatedChild := treeInsert(tree, tree.pageRead(childPtr), key, val)
	splitCount, split := splitNode(updatedChild)
	tree.pageFree(childPtr)
	replaceChildren(tree, dst, node, idx, split[:splitCount]...)
}
