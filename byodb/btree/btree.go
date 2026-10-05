package btree

import (
	"bytes"
	"errors"
	"fmt"
)

// Page management callbacks. These are the tree's entire interface to storage.
type (
	// PageReader dereferences a page number, returning the page's bytes.
	PageReader func(ptr uint64) []byte
	// PageAllocator writes a new page and returns its page number. Under
	// copy-on-write every modified node becomes a new page.
	PageAllocator func(page []byte) uint64
	// PageDeallocator releases a page number that is no longer reachable.
	PageDeallocator func(ptr uint64)
)

// BTree is a copy-on-write B+tree over a paged store.
//
// A modification never overwrites a reachable page: the path from the root
// down to the changed leaf is copied, and the tree's root pointer moves to the
// new copy. That single property is what later gives us crash recovery,
// transaction rollback and snapshot isolation almost for free.
type BTree struct {
	root uint64 // page number of the root node; 0 means empty

	pageRead  PageReader
	pageAlloc PageAllocator
	pageFree  PageDeallocator
}

// New builds a tree over the given page callbacks. A root of 0 starts an
// empty tree; otherwise it is the root recovered from storage.
func New(root uint64, read PageReader, alloc PageAllocator, free PageDeallocator) *BTree {
	return &BTree{root: root, pageRead: read, pageAlloc: alloc, pageFree: free}
}

// Root returns the current root page number, which is what a storage layer
// persists to make an update durable.
func (tree *BTree) Root() uint64 { return tree.root }

// SetRoot points the tree at a different root, used when recovering from disk
// or rolling a transaction back.
func (tree *BTree) SetRoot(root uint64) { tree.root = root }

// ErrKeyTooLarge and ErrValueTooLarge report input that exceeds the size
// limits the node format imposes.
var (
	ErrKeyTooLarge   = errors.New("btree: key exceeds the maximum size")
	ErrValueTooLarge = errors.New("btree: value exceeds the maximum size")
	ErrEmptyKey      = errors.New("btree: key must not be empty")
)

func checkLimits(key, val []byte) error {
	switch {
	case len(key) == 0:
		// The empty key is reserved for the sentinel.
		return ErrEmptyKey
	case len(key) > MaxKeySize:
		return fmt.Errorf("%w: %d > %d", ErrKeyTooLarge, len(key), MaxKeySize)
	case len(val) > MaxValSize:
		return fmt.Errorf("%w: %d > %d", ErrValueTooLarge, len(val), MaxValSize)
	}
	return nil
}

// Get looks up a key, returning its value and whether it was present.
//
// The returned slice aliases a page and is only valid until the next
// modification; callers that keep it must copy.
func (tree *BTree) Get(key []byte) ([]byte, bool) {
	if tree.root == 0 {
		return nil, false
	}
	node := Node(tree.pageRead(tree.root))
	for {
		idx := lookupLessOrEqual(node, key)
		switch node.nodeType() {
		case NodeLeaf:
			if bytes.Equal(key, node.key(idx)) {
				return node.value(idx), true
			}
			return nil, false
		case NodeInternal:
			node = Node(tree.pageRead(node.childPtr(idx)))
		default:
			panic("byodb: unrecognized node type")
		}
	}
}

// Insert adds a key or replaces the value of an existing one.
func (tree *BTree) Insert(key, val []byte) error {
	if err := checkLimits(key, val); err != nil {
		return err
	}

	if tree.root == 0 {
		// The first node. It holds a sentinel empty key alongside the real
		// one: the sentinel sorts below every possible key, so a lookup can
		// never fall off the left edge of the tree and no search has to
		// special-case "smaller than everything".
		root := Node(make([]byte, PageSize))
		root.setHeader(NodeLeaf, 2)
		appendKV(root, 0, 0, nil, nil)
		appendKV(root, 1, 0, key, val)
		tree.root = tree.pageAlloc(root)
		return nil
	}

	updated := treeInsert(tree, Node(tree.pageRead(tree.root)), key, val)
	splitCount, split := splitNode(updated)
	tree.pageFree(tree.root)

	if splitCount == 1 {
		tree.root = tree.pageAlloc(split[0])
		return nil
	}

	// The root was split, so the tree gains a level.
	root := Node(make([]byte, PageSize))
	root.setHeader(NodeInternal, splitCount)
	for i, child := range split[:splitCount] {
		appendKV(root, uint16(i), tree.pageAlloc(child), child.key(0), nil)
	}
	tree.root = tree.pageAlloc(root)
	return nil
}

// Delete removes a key, reporting whether it was present.
func (tree *BTree) Delete(key []byte) (bool, error) {
	if err := checkLimits(key, nil); err != nil {
		return false, err
	}
	if tree.root == 0 {
		return false, nil
	}

	updated := treeDelete(tree, Node(tree.pageRead(tree.root)), key)
	if len(updated) == 0 {
		return false, nil
	}
	tree.pageFree(tree.root)

	if updated.nodeType() == NodeInternal && updated.keyCount() == 1 {
		// The root has collapsed to a single child; drop a level.
		tree.root = updated.childPtr(0)
	} else {
		tree.root = tree.pageAlloc(updated)
	}
	return true, nil
}
