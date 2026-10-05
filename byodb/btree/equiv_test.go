package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// linearLookup is the obvious implementation, kept here only as the reference
// the binary search is checked against.
func linearLookup(node Node, key []byte) uint16 {
	found := uint16(0)
	for i := uint16(1); i < node.keyCount(); i++ {
		cmp := bytes.Compare(node.key(i), key)
		if cmp <= 0 {
			found = i
		}
		if cmp >= 0 {
			break
		}
	}
	return found
}

// TestBinarySearchMatchesLinearScan pins the replacement against the version
// it replaced. A search that is merely "close" would return a neighbouring
// child, which reads as missing data rather than as an error.
func TestBinarySearchMatchesLinearScan(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(17))
	for i := 0; i < 5000; i++ {
		h.insert(fmt.Sprintf("key%05d", rng.Intn(50000)), "v")
	}

	checked := 0
	for ptr := range h.pages {
		node := h.pages[ptr]
		for trial := 0; trial < 200; trial++ {
			var probe []byte
			switch trial % 3 {
			case 0: // an existing key
				probe = node.key(uint16(rng.Intn(int(node.keyCount()))))
			case 1: // a key that falls between two others
				probe = []byte(fmt.Sprintf("key%05d", rng.Intn(50000)))
			default: // outside the node's range entirely
				probe = []byte(fmt.Sprintf("%c%05d", 'a'+rng.Intn(26), rng.Intn(99999)))
			}
			want := linearLookup(node, probe)
			got := lookupLessOrEqual(node, probe)
			if got != want {
				t.Fatalf("lookup(%q) in a node of %d keys gave %d, linear scan gives %d",
					probe, node.keyCount(), got, want)
			}
			checked++
		}
	}
	t.Logf("%d lookups across %d nodes agree with the linear scan", checked, len(h.pages))
}
