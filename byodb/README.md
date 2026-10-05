# byodb

A relational database in Go, built from the B+tree up: durable, crash-safe,
transactional, with secondary indexes and a SQL-like query language.

Built following *Build Your Own Database From Scratch in Go* (James Smith).

Runs on Linux, macOS and Windows.

## Try it

```
go build -o byodb ./cmd/byodb     # byodb.exe on Windows
./byodb my.db
```

Or run the bundled exercise of every statement:

```
./byodb smoke.db < smoke.sql
```

```sql
create table people (
    country bytes,
    id int64,
    name bytes,
    age int64,
    index (age),
    primary key (country, id)
);

insert into people (country, id, name, age) values
    ('uk', 1, 'alice', 30),
    ('in', 2, 'bruno', 45);

select name, age from people index by age > 25 filter country != 'uk';
update people set age = age + 1 index by (country, id) = ('uk', 1);
delete from people filter age > 60;
```

`.tables`, `.schema`, `.help` and `.quit` are also available.

## The language

Where SQL has one `WHERE` clause, this has two:

- **`INDEX BY`** chooses the index and the range to scan.
- **`FILTER`** tests each row the scan produces.

A real database infers that split with a query planner. Making it explicit costs
a little syntax and buys predictable performance: the query does what it says,
and cannot silently change plan as the data grows.

`INDEX BY` takes one of three forms — `a = v`, `a > v`, or `a > v and a < w` —
and reversing the bounds (`a < w and a > v`) scans descending.

## Layers

```
  ql/          parser and interpreter
  ─────────────────────────────────────────────────────
  relational/  tables, rows, schemas, secondary indexes
  ─────────────────────────────────────────────────────
  kv/          durable paged file, transactions, MVCC
  ─────────────────────────────────────────────────────
  btree/       copy-on-write B+tree, no IO of its own
```

`btree` reaches storage through three callbacks, so the whole data structure
runs and is tested in memory before any file exists. That is what makes the
layering real rather than decorative.

## How it stays correct

**Copy-on-write** is the single load-bearing decision. A modification never
overwrites a reachable page: the path from the root to the changed leaf is
copied, and one pointer in the meta page moves. Crash recovery, transaction
rollback and snapshot isolation all fall out of that.

A commit is two phases, and the order is the point:

1. Write the new pages. `fsync`.
2. Write the meta record. `fsync`.

New pages are on disk before the root that references them moves. There is no
ordering in which a reader can observe a root pointing at pages that are not
there. Page 0 holds two meta records in separate sectors and commits alternate
between them, so a torn meta write can only damage the one that is not live.

**Transactions** read from an immutable snapshot and buffer writes in memory, so
readers and writers never block each other. Conflicts are detected at commit
rather than prevented; a conflicted transaction changed nothing, so `DB.Update`
retries it with backoff.

**The free list** reclaims pages copy-on-write orphans, stamping each with the
version that freed it so a page a live reader can still reach is never handed
out.

## Documentation

- **`READING_GUIDE.md`** — how to read the codebase, bottom-up in four sittings
- **`WINDOWS.md`** — building and testing on Windows, and what was and was not verified
- **`FORMAT.md`** — the on-disk layout, enough to write an independent reader
- **`CHAPTER_AUDIT.md`** — what each book chapter asked for, and what is outstanding
- **`LIMITATIONS.md`** — what it does not do, and what fixing each would cost

## Testing

```
make test           # everything
make test-portable  # against the page backend Windows uses
make race           # under the race detector
make check          # all of the above, plus cross-platform vet
make fuzz           # fuzz the parser
make bench
```

Page reads go through a memory mapping where one is available and explicit
reads otherwise. The portable backend is selectable anywhere with
`-tags byodb_pread`, so the code Windows runs is covered by the whole suite on
a machine that is not Windows.

Every layer is checked against a reference model — a `map`, a sorted slice, a
linear search — so correctness is decided by comparison rather than by
hand-written expectations. On top of that:

- **Crash injection** fails or tears every write and `fsync` in an update, under
  five failure modes including write reordering, and requires recovery to a
  consistent version each time.
- **A real `SIGKILL`** against a live writer in a separate process: every row it
  acknowledged must survive, with indexes intact.
- **Invariant checks** after every mutation: keys sorted, leaves at equal depth,
  no page leaked or double-referenced.
- **Mutation testing** — deliberately breaking the implementation to confirm the
  suite fails. This found several tests that were passing for the wrong reason,
  and one real bug the tests could not have caught as written.
