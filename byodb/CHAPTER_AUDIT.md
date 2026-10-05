# Chapter-by-chapter audit

**Short answer: yes.** Every chapter's core objective is implemented, and every
component the book names by function exists in the code (renamed, but
one-for-one — see the mapping at the end).

What is missing is a specific, short list: **five things the book itself marks
as an exercise or an optional optimization**, plus two illustrative code samples
from the early chapters that were never part of the finished database. Nothing
on that list affects correctness. They are listed here with what each would
cost.

## Summary

| Ch | Objective | Status | Outstanding |
|---|---|---|---|
| 00 | Introduction | — | LoC budget exceeded (see note) |
| 01 | Files → databases | ✅ concepts applied | Illustrative samples not written |
| 02 | Indexing data structures | ✅ conceptual | Hashtable exercise not done |
| 03 | B-tree & crash recovery | ✅ complete | — |
| 04 | B+tree node & insertion | ✅ complete | Large KVs outside nodes (book's own simplification) |
| 05 | B+tree deletion & testing | ✅ complete, exercise done | — |
| 06 | Append-only KV store | ✅ complete + hardened | — |
| 07 | Free list | ✅ complete | — |
| 08 | Tables on KV | ✅ complete | — |
| 09 | Range queries | ✅ complete | **Float encoding exercise** |
| 10 | Secondary indexes | ✅ complete | — |
| 11 | Atomic transactions | ✅ complete | **§11.3: range delete, prefix compression; copy-once partial** |
| 12 | Concurrency control | ✅ complete | **Lock held across commit IO (book's closing exercise)** |
| 13 | SQL parser | ✅ complete | — |
| 14 | Query language | ✅ complete | — |

---

## Chapter 00 — Introduction

The book budgets **3000 lines**; this is **5394**. The difference is not extra
features. It is:

- Platform support (~250 lines): a second page backend and four file-open paths,
  so it runs on Windows and macOS as well as Linux.
- Error handling the book elides (~400 lines): the book panics or writes
  `// code omitted`; every such path here returns a described error.
- Comments (~1200 lines): the book carries its explanation in prose around the
  code, and this carries it in the code.
- The CLI (280 lines), which the book does not build at all.

Strip those and the engine is close to the book's figure. Worth knowing if you
are comparing line counts, not worth "fixing".

## Chapter 01 — From Files To Databases

**Objective:** understand why in-place updates are unsafe; atomic rename;
append-only logs; `fsync` gotchas.

**Status: concepts fully applied.** The directory `fsync` after file creation is
implemented (`kv/open_unix.go`) — the specific gotcha this chapter exists to
teach. Copy-on-write, which the whole design rests on, is the answer to the
problem this chapter poses.

**Outstanding — cosmetic:** `SaveData1`, `SaveData2` and the checksummed
append-only log are not in the tree. These are the chapter's *illustrations of
what fails*, not components of the database; the book writes them to show why
they are inadequate. My original plan listed them as an `internal/fsutil`
package and they were never written.

*Worth doing?* Only as teaching material. ~80 lines, half an hour. It would give
a runnable demonstration of what goes wrong without copy-on-write — which is
genuinely useful if you want to show someone the problem before the solution.

## Chapter 02 — Indexing Data Structures

**Objective:** understand why B-trees and LSM-trees are viable on disk and
hashtables are not; the LSM trade-off.

**Status: conceptual chapter, no code required.** The B-tree branch is built.

**Outstanding:** the book calls coding a hashtable "a valuable exercise". Not
done. Purely pedagogical — it would not be part of this database.

## Chapter 03 — B-Tree & Crash Recovery

**Objective:** B-tree as a balanced n-ary tree; block-based allocation;
copy-on-write for safe updates; the crash-recovery principle.

**Status: complete.** Copy-on-write is the load-bearing decision of the whole
codebase. The alternative the chapter discusses (in-place update with
double-write) is deliberately not taken, which is the book's own choice.

## Chapter 04 — B+Tree Node and Insertion

**Objective:** node format; decode it; insert into leaves; update internal
nodes; split into up to three nodes; tree insertion.

**Status: complete.** All of it, including the three-way split.

**Outstanding — inherited from the book:** keys are capped at 1000 bytes and
values at 3000, so a pair always fits in a page. The book explicitly skips
storing large values outside nodes: *"This problem is solvable, but not
fundamental. So we'll skip it by limiting the KV size."*

*Worth doing?* Only if you need large values. It means overflow pages and a
pointer-to-overflow value type: ~2 days, and it touches the node format.

## Chapter 05 — B+Tree Deletion and Testing

**Objective:** high-level interfaces; keep the root; the sentinel; merge nodes;
tree deletion; **test the B+tree**.

**Status: complete, including the exercise.** The book leaves the test cases as
an exercise; they are written — a reference-model harness, an invariant checker
after every mutation, and a page audit. Those tests are what caught the
iterator bug later.

## Chapter 06 — Append-Only KV Store

**Objective:** two-phase update; the file layout; `fsync` on the directory;
`mmap`; the meta page; error handling after IO failures.

**Status: complete, and hardened beyond the chapter.**

Two deliberate departures, both upward:

- **The meta page is double-buffered.** The book writes one record and relies on
  a single-sector write being power-loss atomic, mentioning double buffering as
  an option. A torn-write test showed one record is genuinely unrecoverable, so
  page 0 now holds two in separate sectors and commits alternate.
- **The meta page is checksummed and versioned**, so a damaged file is reported
  rather than misread into a wrong root pointer.

## Chapter 07 — Free List

**Objective:** an unrolled linked list of reusable pages that manages its own
space; FIFO with sequence numbers; `maxSeq` so an update cannot consume what it
just freed.

**Status: complete.** Plus version stamps from chapter 12.

## Chapter 08 — Tables on KV

**Objective:** rows as KV pairs; the table prefix; `Value`/`Record`/`TableDef`;
the `@meta` and `@table` internal tables; get, update, insert, delete, create.

**Status: complete.** The book's schema-in-the-database bootstrap is implemented
exactly.

The chapter's *alternative* design — an auto-generated row ID as the true
primary key, with the user's primary key as another index — is not taken. The
book presents it as an alternative, not a requirement. Its advantage (shorter
keys in internal nodes, smaller secondary indexes) is real but it is a different
database.

## Chapter 09 — Range Queries

**Objective:** the B+tree iterator; order-preserving encoding for integers,
strings and tuples; `Scanner`.

**Status: complete.** All of it, and the encoding is checked pairwise against
real ordering across every value pair in the test set.

### ▲ Outstanding: float encoding

> *"Exercise for the reader: Apply this to floats (sign + magnitude +
> exponent)."*

**Not done.** Only `int64` and `bytes` exist.

**What it takes:** a third type tag, and an order-preserving encoding in
`relational/encoding.go`. For IEEE-754 doubles the trick is a cousin of the
integer sign-bit flip: take the bits, and if the sign bit is set flip *all*
bits, otherwise flip only the sign bit. That maps the float ordering onto
unsigned ordering, negatives included. Then a `float64` column type in the
parser and evaluator.

**Effort:** ~4 hours including tests. The test already written for integers and
strings (`checkOrder`) takes floats unchanged, which is most of the work.

## Chapter 10 — Secondary Indexes

**Objective:** indexes as extra key prefixes; index selection by matching
columns; infinity markers for unconstrained columns; keeping indexes in sync,
and the atomicity problem that follows.

**Status: complete.** Index consistency is checked differentially — after
arbitrary insert/update/delete sequences, every index is walked and compared
against the primary data.

## Chapter 11 — Atomic Transactions

**Objective:** commit and rollback via copy-on-write; move tree operations onto
the transaction; `KVTX` and `DBTX`.

**Status: core complete.** An aborted transaction is checked to leave the file
*byte-identical*, which is the strongest form of the claim.

### §11.3 "Optional optimizations" — the book's own list of three

**1. Reduce copying on multi-key updates — partially done.**

> *"copy a node only once within a transaction and use in-place updates on
> copied nodes."*

What is implemented is the *memory* half: a page created and discarded within
the same update is reclaimed immediately instead of accumulating. That was not
optional — without it a 20,000-row transaction exhausted memory.

What is **not** implemented is the copying itself. Inserting the second key
still copies the path the first key just created, it just reuses the page
number. So the work is still O(keys × height) node copies.

*Effort:* ~1 day. Track which pages this transaction already copied, and mutate
them in place rather than copying again. The subtlety is that the tree currently
treats every node as immutable, so the in-place path needs to be clearly
separated from the copy-on-write one.

**2. Range delete — not done. ▲**

> *"The naive approach to dropping a table is to iterate and delete keys one by
> one. This reads the entire table into memory and does useless work as nodes
> are updated repeatedly before being deleted."*

`TableDrop` does exactly that — it collects every row into a slice, then deletes
them one at a time. For a large table it reads the whole thing into memory and
rewrites nodes repeatedly on the way to discarding them.

*Effort:* ~2 days. Free whole subtrees whose key range falls entirely inside the
deleted range, without reading their leaves. It is the most worthwhile of the
three, because dropping a large table is a normal operation that currently
behaves badly.

**3. Compress common prefixes — not done.**

Neighbouring keys share long prefixes, especially multi-column ones and
secondary index entries (which begin with the same 4-byte table prefix). Storing
each in full wastes space and lowers fanout.

*Effort:* ~3 days, and the book warns why: node size stops being easy to predict,
which makes splitting and merging harder to get right. The existing invariant
checker would help.

## Chapter 12 — Concurrency Control

**Objective:** snapshot isolation for readers; version numbers in the free list;
read-your-own-writes; optimistic conflict detection; serialize the shared
structures.

**Status: complete.** All five. Verified under the race detector with concurrent
readers, disjoint writers and contended writers.

### ▲ Outstanding: the chapter's closing exercise

> *"considering that Commit involves IO, we can go further by releasing the lock
> while waiting for IO to allow other TXs to enter and read-only TXs to exit.
> The commit step should still be serialized with other commit steps via another
> lock. This is left as an exercise."*

**Not done.** `Commit` holds the database-wide mutex across both `fsync`s, so a
reader trying to *begin* waits for the writer's disk IO.

**Measured here:**

| Reader `Begin()` latency while a writer commits | |
|---|---|
| median | 1.4 ms |
| p95 | 1.8 ms |
| max | 6.4 ms |

On slower storage this gets worse in proportion to `fsync` latency. The database
is still correct — readers are never blocked *during* a read, only when
starting one — but it undercuts the chapter's own claim that readers and writers
do not block each other.

**What it takes:** two locks instead of one. A short `mutex` protecting the
shared in-memory state (`ongoing`, `history`, the version counter), and a
separate `commitMutex` serializing commits against each other. `Commit` takes
the short lock to verify conflicts, releases it, takes the commit lock for the
IO, then retakes the short lock to publish the new version. The ordering has to
be right or two commits could interleave their meta writes.

**Effort:** ~1 day including a test that asserts reader latency stays low while
a writer commits — the measurement above is the test.

## Chapter 13 — SQL Parser

**Objective:** recursive descent; the statement grammar; operator precedence by
recursion; `INDEX BY` / `FILTER` instead of `WHERE`.

**Status: complete.** All five statement forms, all three `INDEX BY` shapes,
seven precedence levels. Errors point at the offending position. Fuzzed for
650,000 executions with no panics.

Two additions beyond the book: `REPLACE INTO` (upsert) and `DROP TABLE`.

## Chapter 14 — Query Language

**Objective:** expression evaluation; translating `INDEX BY` into a range; the
`RecordIter` chain; executing every statement.

**Status: complete.** The iterator pipeline is exactly the book's: scan →
filter/offset/limit → projection, with nothing materialized.

---

## Suggested order of work

1. **Release the lock during commit IO** (ch12 exercise) — ~1 day. The only item
   with a measured cost today, and it is the chapter's own stated goal.
2. **Range delete** (ch11 §11.3) — ~2 days. Dropping a large table currently
   behaves badly in a way users would notice.
3. **Float encoding** (ch09 exercise) — ~4 hours. Cheapest of the real gaps, and
   the most visible as a feature.
4. **Copy-node-once within a transaction** (ch11 §11.3) — ~1 day. Speeds up
   every multi-key write.
5. **Prefix compression** (ch11 §11.3) — ~3 days. Most work, least visible
   effect, highest risk to the node format.
6. **`SaveData1`/`SaveData2` and the checksummed log** (ch01) — half an hour, if
   you want the teaching artifacts.

Items 1–5 are all in the book. Everything else on the wish list —
`SELECT *`, aggregates, `JOIN`, `ORDER BY`, `NULL` — is beyond it; see
`LIMITATIONS.md`.

## Component mapping

Every function the book names, and where it lives here. Names were expanded for
readability; the structure is one-for-one.

| Book | Here |
|---|---|
| `nodeLookupLE` | `btree.lookupLessOrEqual` (now a binary search) |
| `nodeSplit2` / `nodeSplit3` | `btree.splitInTwo` / `btree.splitNode` |
| `nodeReplaceKidN` | `btree.replaceChildren` |
| `nodeReplace2Kid` | `btree.replaceTwoChildren` |
| `shouldMerge` | `btree.mergeTarget` |
| `treeInsert` / `nodeInsert` | `btree.treeInsert` / `insertIntoInternal` |
| `treeDelete` / `nodeDelete` | `btree.treeDelete` / `deleteFromInternal` |
| `BIter` / `iterNext` | `btree.Iter` / `iterNext` |
| `saveMeta` / `loadMeta` | `kv.meta.encode` / `decodeMeta` |
| `updateFile` / `updateOrRevert` | `kv.commit` / `updateOrRevert` |
| `seq2idx` / `SetMaxSeq` | `kv.seqToIndex` / `freeList.beginUpdate` |
| `FreeList.PopHead` / `PushTail` | `kv.freeList.pop` / `push` |
| `CombinedIter` | `kv.TxIter` |
| `detectConflicts` | `kv.detectConflicts` |
| `TDEF_TABLE` / `TDEF_META` | `relational.tableSchemas` / `tableMeta` |
| `checkRecord` / `encodeKeyPartial` | same names |
| `dbScan` index selection | `relational.selectIndex` |
| `pStmt` / `pSelect` / `pScan` | `parser.statement` / `selectStmt` / `scanClauses` |
| `pExprOr` … `pExprUnop` | `parser.exprOr` … `exprUnary` |
| `qlEval` | `ql.evalExpr` |
| `qlScanInit` | `ql.setUpScan` |
| `qlScanIter` / `qlSelectIter` | `ql.scanIter` / `selectIter` |
| `RecordIter` | same name |
