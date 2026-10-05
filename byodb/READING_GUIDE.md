# Reading guide

35 Go files, 10,302 lines: **5,394 implementation, 4,908 tests.**

| Package | Files | Impl | Tests | What lives here |
|---|---|---|---|---|
| `btree/` | 5 + 3 | 843 | 1,008 | Copy-on-write B+tree and its iterator |
| `kv/` | 5 + 4 | 1,515 | 1,690 | Paged file, crash safety, free list, transactions |
| `relational/` | 6 + 1 | 1,161 | 913 | Rows, schemas, encoding, secondary indexes |
| `ql/` | 5 + 3 | 1,565 | 738 | Parser, evaluator, execution pipeline |
| `cmd/byodb/` | 1 | 280 | — | The shell |
| `internal/assert/` | 1 | 15 | — | Invariant checks |
| `integration/` | — + 2 | — | 730 | Whole-stack, crash and concurrency tests |

Tests are roughly as large as the implementation. That is deliberate: nearly
every test is a differential test against a reference model, so the reference
models and harnesses account for much of the bulk.

## Read it bottom-up, in four sittings

Each layer is built entirely on the one below, so reading in dependency order
means never encountering something undefined. Each sitting has **one idea** —
if you take only that away, the rest follows.

### 1. The B+tree — `btree/` (~2 hours)

> **The idea: the in-memory representation *is* the on-disk representation, and
> the tree never overwrites a page it can still reach.**

Read in this order:

1. **`node.go`** — start with the layout comment at the top. Everything else is
   arithmetic on those offsets. The one non-obvious piece is the offsets array:
   it stores the *end* of each pair, and exists so the nth pair can be found in
   O(1) despite variable-length keys. `lookupLessOrEqual` is the binary search
   that array makes possible.
2. **`insert.go`** — `leafInsert` first (copy the old node, splice in a pair),
   then `splitNode`. The surprise is that a split can produce **three** nodes,
   not two: one oversized pair in the middle forces a second split.
3. **`delete.go`** — the mirror image. `mergeTarget` decides whether a shrunken
   node joins a sibling.
4. **`btree.go`** — `Insert` and `Delete` handle the root: creating it,
   growing a level when it splits, dropping one when it collapses. Note the
   **sentinel empty key** inserted at creation — it removes the "smaller than
   everything" edge case from every lookup in the codebase.
5. **`iter.go`** — the path stack. Read `iterNext` carefully and note why it
   returns a bool; the comment explains the bug that return value prevents.

Then read **`btree_test.go`'s `harness`**. It is the pattern used everywhere
else: apply each operation to both the tree and a plain Go `map`, then compare.

*Check yourself:* why is `MaxKeySize + MaxValSize` less than a page, and what
would break if it weren't?

### 2. Durability — `kv/` (~3 hours, the hardest part)

> **The idea: new pages reach disk before the pointer that references them.**

1. **`meta.go`** — the layout comment, then `decodeMeta`. Three outcomes, not
   two: valid, all-zero (never committed), and corrupt.
2. **`kv.go`** — `recover()`, then `commit()`. `commit` is fifteen lines and is
   the heart of the whole project; the comment explains why the first `fsync`
   is a barrier rather than a durability step. Then `updateOrRevert` and the
   `failed` flag, which handles the genuinely nasty case: an `fsync` that fails
   on the meta page leaves the on-disk version *unknown*.
3. **`freelist.go`** — an unrolled linked list that stores itself in the pages
   it manages. Read `push` and note that it links a new tail *immediately* when
   one fills, so the list is never empty.
4. **`tx.go`** — snapshot plus a pending in-memory tree. `TxIter.sync` merges
   the two streams, and `detectConflicts` is the whole of optimistic
   concurrency control in twelve lines.

Then read **`crash_test.go`**. The `crashFile` comment explains why writes are
held back until `Sync`: without that, an in-process crash test proves nothing,
because the OS page cache would serve back writes that never reached disk.

*Check yourself:* a crash between the two `fsync`s — what does the next open
see, and why is it consistent?

### 3. Rows and indexes — `relational/` (~2 hours)

> **The idea: sorting encoded bytes must sort the values they encode.**

1. **`encoding.go`** — the top comment states the three properties. The
   sign-bit flip for integers and the null-escaping for strings each fix one
   specific failure; the tests in `TestOrderPreservingEncoding` show what breaks
   without them.
2. **`schema.go`** — `@meta` and `@table` are ordinary tables, defined in code
   only so there is something to read the others with. `normalize` moves primary
   key columns to the front, which is why every row splits at a single index.
3. **`table.go`** — the layout comment shows why a secondary index appends the
   primary key: it makes duplicate entries unique *and* gives a scan its route
   back to the row. Then `set`, where the order matters — the row is written
   first because its result says which stale index entries to remove.
4. **`scanner.go`** — `selectIndex` and `encodeKeyPartial`'s infinity markers.

*Check yourself:* `index by a > 1` on an index over `(a, b)` — why does the
missing `b` become +infinity here but −infinity for `a >= 1`?

### 4. The query language — `ql/` (~2 hours, the easiest)

> **The idea: precedence needs no table, only one function per level.**

1. **`ast.go`** then **`lexer.go`** — short, and they fix the vocabulary.
2. **`parser.go`** — read the top comment, then `exprOr` down to `exprAtom`.
   Seven functions, each calling the next; that chain *is* the precedence
   table. Note `indexBy` calling `exprCompare` rather than `expression`, and
   why.
3. **`eval.go`** — a tree walk. Nothing surprising, which is the point.
4. **`exec.go`** — the iterator chain in the top comment, then `scanIter`.
   Nothing materializes the result set, so `LIMIT` stops the work instead of
   discarding it afterwards.

*Check yourself:* trace `select name from t index by age > 30 filter name != 'x' limit 5`
through all four layers.

## Shortcuts

- **Just want to use it?** `README.md`, then run `./byodb test.db` and type.
- **Just want the file format?** `FORMAT.md` alone is enough to write a reader.
- **Want the design decisions?** The package-level comment at the top of each
  package, in the order above. About twenty minutes for the shape of the whole
  thing.
- **Reviewing for correctness?** Start with the tests. `integration_test.go`
  states what the database claims; `crash_test.go` states what it survives.

## The comments are the documentation

Comments explain *why*, not *what* — why a split can make three nodes, why the
first `fsync` is a barrier, why the free list is FIFO. If a comment seems to
state the obvious, it is usually guarding a non-obvious failure. Read the ones
above functions before the code inside them.
