package kv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// The meta page is page 0. It holds the pointer to the current root, and
// therefore *is* the committed version of the database: everything else in the
// file is either reachable from that root or is garbage left by an interrupted
// update.
//
//	| signature | format | generation | version | root ptr | page count | free head | head seq | free tail | tail seq | checksum |
//	|    16B    |   4B   |     8B     |   8B    |    8B    |     8B     |    8B     |    8B    |    8B     |    8B    |    4B    |
//
// Page 0 holds *two* of these records, in separate sectors, and commits
// alternate between them. A meta write that is torn by a power loss can
// therefore only damage the slot that is not currently live, and the previous
// version remains intact in the other. Without this, a single torn meta write
// would leave the database unopenable — the one failure a checksum can detect
// but not repair.
//
// The free list pointers live here because they must move in the same atomic
// step as the root: a version of the database is the tree *and* the set of
// pages available for reuse, and committing one without the other would let a
// later update hand out a page the committed tree still references.
const (
	metaSignature = "BYODB-GO\x00\x00\x00\x00\x00\x00\x00\x00"
	metaSize      = 88

	// metaSlotOffset separates the two records. 512 bytes is a sector
	// boundary, so a tear in one slot cannot reach the other.
	metaSlotOffset = 512
	metaSlots      = 2

	// formatVersion changes whenever the on-disk layout changes, so an old
	// file fails to open with a clear message instead of being misread.
	formatVersion = 5
)

// Errors reporting a file that cannot be opened.
var (
	ErrCorrupt = errors.New("kv: database file is corrupt")
	ErrFormat  = errors.New("kv: unrecognized database file format")
	// ErrLocked reports that another process has the database open. This
	// design keeps its page cache, free list state and conflict history in
	// memory, so it admits exactly one process at a time.
	ErrLocked = errors.New("kv: database is already open in another process")
)

// meta is the committed state of the database: which page is the root, and how
// many pages the committed version occupies.
//
// pageCount is stored rather than derived from the file size because a crash
// can leave file metadata inconsistent with file data. The count in the meta
// page is authoritative; anything past it is garbage from an update that never
// committed.
type meta struct {
	// generation increases with every commit and decides which of the two
	// meta slots is live: the valid record with the higher generation.
	generation uint64

	// version is the logical timestamp of this committed state. It orders
	// snapshots so the free list can tell which freed pages a live reader
	// might still be looking at.
	version uint64

	root      uint64
	pageCount uint64

	// The free list's persisted position. See freelist.go.
	freeHead    uint64
	freeHeadSeq uint64
	freeTail    uint64
	freeTailSeq uint64
}

func (m meta) encode() []byte {
	var buf [metaSize]byte
	copy(buf[0:16], metaSignature)
	binary.LittleEndian.PutUint32(buf[16:20], formatVersion)
	binary.LittleEndian.PutUint64(buf[20:28], m.generation)
	binary.LittleEndian.PutUint64(buf[28:36], m.version)
	binary.LittleEndian.PutUint64(buf[36:44], m.root)
	binary.LittleEndian.PutUint64(buf[44:52], m.pageCount)
	binary.LittleEndian.PutUint64(buf[52:60], m.freeHead)
	binary.LittleEndian.PutUint64(buf[60:68], m.freeHeadSeq)
	binary.LittleEndian.PutUint64(buf[68:76], m.freeTail)
	binary.LittleEndian.PutUint64(buf[76:84], m.freeTailSeq)
	binary.LittleEndian.PutUint32(buf[84:88], crc32.ChecksumIEEE(buf[0:84]))
	return buf[:]
}

// slot is the meta record this version is written into. Alternating means a
// commit never overwrites the record the database would fall back to.
func (m meta) slot() int { return int(m.generation % metaSlots) }

func metaSlotOffsetFor(slot int) int64 { return int64(slot) * metaSlotOffset }

// readMetaPage picks the live record out of page 0.
//
// A slot that fails to decode is not fatal on its own — that is precisely the
// case double buffering exists to survive — so the newest record that verifies
// wins, and only a page with no valid record at all is an error.
func readMetaPage(page []byte) (m meta, fresh bool, err error) {
	var best meta
	var found bool
	var firstErr error
	allFresh := true

	for slot := 0; slot < metaSlots; slot++ {
		start := int(metaSlotOffsetFor(slot))
		if start+metaSize > len(page) {
			break
		}
		candidate, slotFresh, slotErr := decodeMeta(page[start : start+metaSize])
		if slotErr != nil {
			allFresh = false
			if firstErr == nil {
				firstErr = slotErr
			}
			continue
		}
		if slotFresh {
			continue // never written
		}
		allFresh = false
		if !found || candidate.generation > best.generation {
			best, found = candidate, true
		}
	}

	switch {
	case found:
		return best, false, nil
	case allFresh:
		return meta{}, true, nil // no commit has ever happened
	default:
		return meta{}, false, firstErr
	}
}

// decodeMeta reads the meta page.
//
// The three outcomes are distinct on purpose. An all-zero page means no update
// has ever committed — the expected state after a crash between creating the
// file and the first successful commit — and starts a fresh database. A bad
// checksum means a torn write, which we refuse rather than silently misread.
func decodeMeta(page []byte) (m meta, fresh bool, err error) {
	if len(page) < metaSize {
		return meta{}, false, fmt.Errorf("%w: meta page is only %d bytes", ErrCorrupt, len(page))
	}
	buf := page[:metaSize]

	if allZero(buf) {
		return meta{}, true, nil
	}
	if !bytes.Equal(buf[0:16], []byte(metaSignature)) {
		return meta{}, false, fmt.Errorf("%w: not a byodb database file", ErrFormat)
	}
	if version := binary.LittleEndian.Uint32(buf[16:20]); version != formatVersion {
		return meta{}, false, fmt.Errorf("%w: file uses format version %d, this build writes version %d",
			ErrFormat, version, formatVersion)
	}
	want := binary.LittleEndian.Uint32(buf[84:88])
	if got := crc32.ChecksumIEEE(buf[0:84]); got != want {
		// A torn record. The caller falls back to the other slot; only if
		// both are damaged does this become fatal.
		return meta{}, false, fmt.Errorf("%w: meta page checksum mismatch (got %08x, want %08x)",
			ErrCorrupt, got, want)
	}

	m = meta{
		generation:  binary.LittleEndian.Uint64(buf[20:28]),
		version:     binary.LittleEndian.Uint64(buf[28:36]),
		root:        binary.LittleEndian.Uint64(buf[36:44]),
		pageCount:   binary.LittleEndian.Uint64(buf[44:52]),
		freeHead:    binary.LittleEndian.Uint64(buf[52:60]),
		freeHeadSeq: binary.LittleEndian.Uint64(buf[60:68]),
		freeTail:    binary.LittleEndian.Uint64(buf[68:76]),
		freeTailSeq: binary.LittleEndian.Uint64(buf[76:84]),
	}
	if m.pageCount < 2 {
		return meta{}, false, fmt.Errorf("%w: page count is %d, must include the meta page "+
			"and at least one free list node", ErrCorrupt, m.pageCount)
	}
	if m.root >= m.pageCount {
		return meta{}, false, fmt.Errorf("%w: root page %d is beyond the %d committed pages",
			ErrCorrupt, m.root, m.pageCount)
	}
	for _, check := range []struct {
		name string
		page uint64
	}{{"head", m.freeHead}, {"tail", m.freeTail}} {
		if check.page < 1 || check.page >= m.pageCount {
			return meta{}, false, fmt.Errorf("%w: free list %s page %d is outside the %d committed pages",
				ErrCorrupt, check.name, check.page, m.pageCount)
		}
	}
	if m.freeHeadSeq > m.freeTailSeq {
		return meta{}, false, fmt.Errorf("%w: free list head sequence %d is ahead of tail %d",
			ErrCorrupt, m.freeHeadSeq, m.freeTailSeq)
	}
	return m, false, nil
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}
