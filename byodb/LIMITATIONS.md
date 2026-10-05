# Limitations and how to fix them

An honest inventory. The distinction that matters: **is it wrong, or is it
missing?** Something wrong gives incorrect answers or loses data, and must be
fixed. Something missing is a feature the book never set out to build.

## Fixed in this pass

Three real defects, found by looking for them rather than by tests failing.

### 1. Two processes could silently destroy each other's data ▲ critical

Opening the same file from two processes was accepted, and the second writer's
meta page simply overwrote the first's. Reproduced directly: two concurrent
`insert` commands, and three committed rows vanished with no error.

Every part of this design keeps its state in memory — the page cache, which
versions are live, the conflict history — so a second process commits against
state the first knows nothing about.

**Fixed** with an advisory `flock` taken at open. A second open now fails with
`ErrLocked` naming the file. The lock is released by the OS when the process
exits, so a crash never leaves it stuck. Covered by
`kv.TestSecondOpenIsRefused`.

### 2. Point queries decoded the schema on every row ▲ performance

Each row operation looked up its table definition and JSON-decoded it: 9.3% of
a point query's time, and most of its allocations.

**Fixed** with a schema cache scoped to a **transaction**, not to the process.
That makes correctness trivial — a transaction reads from a fixed snapshot, so
a schema cannot change under it, and a definition created by a transaction that
later aborts dies with the cache. A process-wide cache would need cross-
transaction invalidation and could hand an uncommitted schema to a reader.
Covered by `relational.TestSchemaCacheDoesNotLeakUncommittedTables`.

### 3. In-node search was a linear scan ▲ performance

The offsets array exists to make binary search possible, and the code carried a
TODO saying so. It was 18% of a point query.

**Fixed.** `btree.TestBinarySearchMatchesLinearScan` checks the replacement
against the implementation it replaced across thousands of probes — an
in-node search that is merely *close* returns a neighbouring child, which reads
as missing data rather than as an error.

**Measured effect of 2 and 3 together:**

| | Before | After |
|---|---|---|
| Point query | 3,212 ns, 27 allocs | **618 ns, 6 allocs** |
| Batched insert | 24,378 ns, 35 allocs | **17,740 ns, 14 allocs** |
| Range scan (1000 rows) | 260 µs | 265 µs (unchanged — schema read once) |

## Known limits that are correct, not broken

These are honest boundaries, not bugs. Inside them the database is correct.

### Deliberate restrictions

**`UPDATE` cannot change primary key columns.** Changing one moves the row to a
different key — a delete plus an insert, not an update. It is refused with an
explaining error rather than quietly doing one of the two. *Fix if you want it:*
make `executeUpdate` detect a primary key change and issue delete + insert
inside the same transaction. About 30 lines. The reason it is not done is that
the semantics need a decision — what should happen if the new key already
exists?

**One process at a time.** Now enforced. *Fix:* a client/server process, which
is the real answer, and the book's suggested next step.

### Missing SQL

None of this is built. Each is additive — the layers below do not change.

| Missing | Effort | Where it would go |
|---|---|---|
| `SELECT *` | ~1 hour | Parser: expand the star against the schema at parse time |
| `ORDER BY` | days | Needs a sort operator, and spilling to disk for large results. Today, order comes from the chosen index |
| `COUNT`/`SUM`/`GROUP BY` | ~2 days | A new aggregating `RecordIter` stage |
| `JOIN` | ~1 week | Nested-loop is a day; anything faster needs a planner |
| `NULL` | ~3 days | Invasive: a third state in every comparison, and a change to the value encoding |
| `float64` | ~4 hours | A type tag plus an order-preserving encoding (sign + exponent + mantissa, like the int64 trick) |
| `ALTER TABLE` | ~1 day | Add-column is easy; anything else rewrites every row and index |
| `BEGIN`/`COMMIT` in the shell | ~2 hours | The engine already supports it; only the REPL is missing it |

### Performance headroom

Measured, in rough order of value:

1. **Two `fsync`s per commit.** A write-ahead log would make it one, and let
   the database return success before the data reaches its final home. Days of
   work, and it trades away the simplicity that makes recovery easy to reason
   about.
2. **A whole transaction is buffered in memory.** Bulk-loading a million rows
   needs the memory to hold a million pending keys. *Fix:* spill the pending
   tree to a temporary file past a threshold.
3. **Conflict detection is O(reads × writes).** Fine for short transactions;
   quadratic for a long one. *Fix:* sort the write ranges and binary-search.
   A few hours.
4. **No prefix compression in nodes.** Neighbouring keys share long prefixes,
   especially multi-column ones. The book names this as an exercise, and warns
   it makes splitting and merging harder to predict.
5. **The file never shrinks.** Freed pages are reused, so it stops *growing* —
   but deleting a large table does not give the space back. *Fix:* a compaction
   pass that copies live pages into a fresh file and renames it.

### Operational gaps

- **No network protocol.** Embedded only, like SQLite.
- **No backup command.** Copying the file while the database is open is unsafe;
  a proper `.backup` would hold a read transaction open and copy the pages its
  snapshot reaches.
- **A long-running reader pins freed pages.** Correct — that reader can still
  reach them — but a reader held open for hours prevents reuse and the file
  grows meanwhile. Real databases report this as "oldest transaction" pressure.
- **`mmap` turns a disk read error into `SIGBUS`**, which kills the process
  rather than returning an error. The book notes this as the cost of `mmap`.
  *Fix:* read through `pread` instead, losing the automatic page cache.

## What to do next, in order

1. **`SELECT *` and `BEGIN`/`COMMIT` in the shell.** A few hours for both,
   and they make the thing markedly nicer to use.
2. **Aggregates.** The single biggest gap between this and something people
   would reach for.
3. **A compaction command**, so deleting data returns disk space.
4. **`float64`**, if the data you care about has non-integers in it.
5. **A server**, if more than one process needs access. This is the fork in the
   road: everything above keeps it an embedded library.

`ORDER BY`, `JOIN` and `NULL` are each a genuine project, and each pulls in a
query planner sooner or later. That is the point at which this stops being a
database you can read in a weekend.
