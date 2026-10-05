# On-disk format

Format version 5 · signature `BYODB-GO`

The database is a single file divided into 4096-byte pages. Page 0 is the meta
page; every other page is either a B+tree node or a free-list node.

```
| meta page | page | page | page | ... |
|     0     |  1   |  2   |  3   |     |
```

## Meta page (page 0)

Page 0 holds **two** meta records, one at offset 0 and one at offset 512. Each
is 88 bytes:

| offset | size | field |
|---|---|---|
| 0 | 16 | signature, `BYODB-GO` padded with NULs |
| 16 | 4 | format version (little-endian uint32) |
| 20 | 8 | generation — commit counter |
| 28 | 8 | version — logical timestamp of this committed state |
| 36 | 8 | root page number of the B+tree; 0 means empty |
| 44 | 8 | committed page count |
| 52 | 8 | free list head page |
| 60 | 8 | free list head sequence |
| 68 | 8 | free list tail page |
| 76 | 8 | free list tail sequence |
| 84 | 4 | CRC-32 (IEEE) of bytes 0–84 |

`version` orders snapshots: a reader holds one,
and the free list uses it to decide which freed pages are still reachable.

All integers are little-endian.

The live record is the one with the higher generation that passes its checksum.
Commits alternate between the two slots, so a torn meta write can only damage
the slot that is not live. Falling back costs the most recent commit — which,
in a real crash, is the commit that was in flight anyway.

The committed page count is authoritative, not the file size: a crash can leave
file metadata inconsistent with file data, and anything past the committed count
is garbage from an update that never finished.

## B+tree node

```
| type | key count |   child ptrs   |   kv offsets   | key-values | unused |
|  2B  |    2B     | keyCount * 8B  | keyCount * 2B  |    ...     |        |
```

Type is 1 for internal, 2 for leaf. Each key-value pair:

```
| key len | val len | key | val |
|   2B    |   2B    | ... | ... |
```

The offsets array holds the *end* position of each pair relative to the start of
the key-value area; the first pair's start offset is implicitly zero and is not
stored. It exists so the nth pair can be found in O(1) despite variable-length
keys.

Both node types share one layout. Leaf nodes waste the child pointer array and
internal nodes store empty values — a deliberate simplification.

Limits: keys ≤ 1000 bytes, values ≤ 3000 bytes. These guarantee that one pair
plus a header always fits in a page, which is what bounds a split to three
nodes.

The tree always contains a sentinel empty key, which sorts below every real key
so that a lookup can never fall off the left edge.

## Free list node

```
| next | (page, version) pairs | unused |
|  8B  |       n * 16B         |  ...   |
```

An unrolled linked list; each node holds up to 255 entries. Head and tail
positions are monotonically increasing sequence numbers stored in the meta page;
the slot within a node is `seq % 255`. The list is FIFO and never empty — it
always has at least one node, which removes a pile of special cases.

Each entry carries the version that freed the page. A page freed at version V
may still be referenced by version V-1, which a reader can be holding, so it is
not handed out until every live transaction has moved past it. Because the list
is FIFO, the oldest — and therefore safest — entries are at the head, so one
check there is enough to stop.

The list manages its own space: a node that empties is pushed back into the list,
and a new tail node is taken from the list before resorting to appending.

## Rows and indexes

Every table and index gets a 4-byte big-endian key prefix, so they share one
B+tree while each occupying a contiguous stretch of it.

```
primary key   | prefix | pk columns...            | -> | remaining columns |
secondary idx | prefix | indexed cols... | pk cols | -> | (empty)           |
```

Columns are encoded so that byte comparison matches value comparison:

- **int64** — big-endian with the sign bit flipped, so negatives sort below
  positives instead of above them.
- **bytes** — null-terminated, with `0x00` escaped to `0x01 0x01` and `0x01` to
  `0x01 0x02`. Without a terminator, `("a","bc")` and `("ab","c")` would encode
  identically.
- Every column is prefixed with its 1-byte type tag. That makes a hexdump
  readable and, since no tag is `0xff`, reserves `0xff` as a usable
  "greater than any column" marker.

A query constraining only the leading columns of an index needs a value for the
rest: `0xff` for +infinity and the empty string for -infinity. Which one depends
on the operator — `a > 1` means `(a, b) > (1, +inf)`, while `a >= 1` means
`(a, b) > (1, -inf)`.

Schemas live in the database itself, in two tables defined in code so there is
something to read the others with:

- `@meta` (prefix 1) — settings, including the counter that hands out prefixes.
- `@table` (prefix 2) — table name to a JSON-encoded definition.

User prefixes start at 3, one per index.

## Update protocol

An update is two phases, and the order is the whole point:

1. Write all new and modified pages. **fsync.**
2. Write the meta record into the non-live slot with generation + 1. **fsync.**

New pages are confirmed on disk before the root pointer that references them
moves. A crash in phase 1 leaves the meta page pointing at the old root, so the
new pages are unreachable garbage. A crash in phase 2 leaves either the old or
the new record live. There is no ordering in which a reader can observe a root
pointing at pages that are not there.

The first fsync is a barrier, not a durability step. Without it the meta write
could reach the device before the pages it references, since write order is not
persistence order.

### After a failed update

The in-memory state reverts to the last committed version, which is safe because
copy-on-write never touched it. The database stays readable.

A failed fsync on the meta page is the subtle case: the record may or may not
have landed, so the on-disk live version is unknown. The database records this
and rewrites the last known-good record before appending anything on the next
update — otherwise it could append over pages that the disk still commits.

## Compatibility

`formatVersion` is bumped whenever the layout changes, and a file written by a
different version is rejected with a clear error rather than misread. Version 3
introduced the paged file, 4 added free-list state and the second meta slot, and
5 added snapshot versions to the meta page and the free list.
