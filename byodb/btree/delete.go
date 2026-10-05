package btree

import (
	"bytes"

	"byodb/internal/assert"
)

// mergeThreshold is the soft minimum node size. A node smaller than this is
// merged with a sibling if one will fit.
//
// Merging only when a node becomes completely empty would let a mostly-deleted
// tree keep a large number of near-empty nodes alive, so we merge earlier.
const mergeThreshold = PageSize / 4

// leafDelete produces a copy of old with the pair at idx removed.
func leafDelete(dst, old Node, idx uint16) {
	dst.setHeader(NodeLeaf, old.keyCount()-1)
	appendKVRange(dst, old, 0, 0, idx)
	appendKVRange(dst, old, idx, idx+1, old.keyCount()-(idx+1))
}

// mergeNodes concatenates two sibling nodes into one.
func mergeNodes(dst, left, right Node) {
	dst.setHeader(left.nodeType(), left.keyCount()+right.keyCount())
	appendKVRange(dst, left, 0, 0, left.keyCount())
	appendKVRange(dst, right, left.keyCount(), 0, right.keyCount())
}

// replaceTwoChildren rewrites an internal node, collapsing the two adjacent
// children at idx and idx+1 into the single merged child described by ptr/key.
func replaceTwoChildren(dst, old Node, idx uint16, ptr uint64, key []byte) {
	assert.True(idx+1 < old.keyCount(), "no adjacent pair at index %d", idx)
	dst.setHeader(NodeInternal, old.keyCount()-1)
	appendKVRange(dst, old, 0, 0, idx)
	appendKV(dst, idx, ptr, key, nil)
	appendKVRange(dst, old, idx+1, idx+2, old.keyCount()-(idx+2))
}

// mergeTarget decides whether the updated child at idx should be merged with a
// sibling, and if so which one.
//
// It returns -1 and the left sibling, +1 and the right sibling, or 0 and no
// node when the child should stand alone.
func mergeTarget(tree *BTree, parent Node, idx uint16, updated Node) (int, Node) {
	if updated.sizeBytes() > mergeThreshold {
		return 0, nil
	}
	// A merged node absorbs the sibling's content but only one header.
	fits := func(sibling Node) bool {
		return sibling.sizeBytes()+updated.sizeBytes()-HeaderSize <= PageSize
	}
	if idx > 0 {
		if sibling := Node(tree.pageRead(parent.childPtr(idx - 1))); fits(sibling) {
			return -1, sibling
		}
	}
	if idx+1 < parent.keyCount() {
		if sibling := Node(tree.pageRead(parent.childPtr(idx + 1))); fits(sibling) {
			return +1, sibling
		}
	}
	return 0, nil
}

// treeDelete removes a key from the subtree rooted at node and returns the
// updated subtree root, or nil if the key was not present.
func treeDelete(tree *BTree, node Node, key []byte) Node {
	idx := lookupLessOrEqual(node, key)
	switch node.nodeType() {
	case NodeLeaf:
		if !bytes.Equal(key, node.key(idx)) {
			return nil // not found
		}
		updated := Node(make([]byte, PageSize))
		leafDelete(updated, node, idx)
		return updated
	case NodeInternal:
		return deleteFromInternal(tree, node, idx, key)
	default:
		panic("byodb: unrecognized node type")
	}
}

// deleteFromInternal recurses into the child at idx and, on the way back up,
// merges the result with a sibling if it has shrunk too far.
func deleteFromInternal(tree *BTree, node Node, idx uint16, key []byte) Node {
	childPtr := node.childPtr(idx)
	updatedChild := treeDelete(tree, tree.pageRead(childPtr), key)
	if len(updatedChild) == 0 {
		return nil // not found; nothing changed
	}
	tree.pageFree(childPtr)

	dst := Node(make([]byte, PageSize))
	direction, sibling := mergeTarget(tree, node, idx, updatedChild)
	switch {
	case direction < 0: // merge into the left sibling
		merged := Node(make([]byte, PageSize))
		mergeNodes(merged, sibling, updatedChild)
		tree.pageFree(node.childPtr(idx - 1))
		replaceTwoChildren(dst, node, idx-1, tree.pageAlloc(merged), merged.key(0))
	case direction > 0: // merge into the right sibling
		merged := Node(make([]byte, PageSize))
		mergeNodes(merged, updatedChild, sibling)
		tree.pageFree(node.childPtr(idx + 1))
		replaceTwoChildren(dst, node, idx, tree.pageAlloc(merged), merged.key(0))
	case updatedChild.keyCount() == 0:
		// An empty child with no sibling to absorb it. The emptiness
		// propagates up to be resolved at a higher level.
		assert.True(node.keyCount() == 1 && idx == 0,
			"an empty child without siblings must be an only child")
		dst.setHeader(NodeInternal, 0)
	default: // no merge needed
		replaceChildren(tree, dst, node, idx, updatedChild)
	}
	return dst
}
