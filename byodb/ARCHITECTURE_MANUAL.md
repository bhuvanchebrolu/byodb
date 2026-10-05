# BYODB — Architecture & Codebase Manual

This document provides a comprehensive, file-by-file manual of the **BYODB** (Build Your Own Database) engine. It details the responsibilities of every file, the 4-layer architecture, data flow paths, on-disk binary layouts, and instructions on how to extend the codebase by adding new features or files.

---

## 1. Architectural Overview & Layering

The database is built strictly bottom-up in four decoupled layers:

```
  ┌────────────────────────────────────────────────────────┐
  │  ql/          SQL Parser, AST, Evaluator & Execution   │
  ├────────────────────────────────────────────────────────┤
  │  relational/  Tables, Rows, Schemas, Secondary Indexes │
  ├────────────────────────────────────────────────────────┤
  │  kv/          Durable Paged File, Transactions, MVCC   │
  ├────────────────────────────────────────────────────────┤
  │  btree/       Copy-On-Write B+Tree (Pure In-Memory)   │
  └────────────────────────────────────────────────────────┘
```

### Key Layering Rules:
1. **`btree` performs NO disk I/O**: It interacts with storage solely through three page management callbacks (`get`, `new`, `del`). This allows the entire data structure to be built and tested in memory before a file exists.
2. **Copy-On-Write (CoW)**: Updating a B+tree node never overwrites a reachable page in place. The path from the root to the modified leaf is copied, and the root pointer moves in an atomic commit step.
3. **Transaction-First Architecture**: Transactions (`kv/tx.go`) sit *below* the relational layer (`relational/`). Tables and secondary index updates occur inside transactions to ensure row updates and index maintenance remain atomic.

---

## 2. Package-by-Package File Inventory

### Layer 1: `btree/` — Copy-on-Write B+Tree Engine
*Location:* [`btree/`](file:///d:/DBMS_Scratch/byodb/btree)

| File | Primary Responsibilities & Key Functions |
|---|---|
| [`btree.go`](file:///d:/DBMS_Scratch/byodb/btree/btree.go) | Defines the `BTree` struct with storage callbacks (`get`, `new`, `del`). Implements root creation, sentinel key initialization (lowest possible empty key), `Get`, `Insert`, `Delete`, `SeekLE`, and `Seek`. |
| [`node.go`](file:///d:/DBMS_Scratch/byodb/btree/node.go) | Defines the `BNode` slice byte type and binary layout helpers (`btype`, `nkeys`, `getPtr`, `setPtr`, `getOffset`, `setOffset`, `getKey`, `getVal`, `nbytes`). |
| [`insert.go`](file:///d:/DBMS_Scratch/byodb/btree/insert.go) | Handles node copy-on-write insertions (`leafInsert`, `leafUpdate`, `nodeAppendKV`, `nodeAppendRange`), multi-child link replacement (`nodeReplaceKidN`), and 2-way/3-way node splits (`nodeSplit2`, `nodeSplit3`). |
| [`delete.go`](file:///d:/DBMS_Scratch/byodb/btree/delete.go) | Implements leaf key deletion (`leafDelete`), node merging (`nodeMerge`), sibling selection (`mergeTarget`/`shouldMerge`), child link replacement (`nodeReplace2Kid`), and recursive tree deletion (`treeDelete`). |
| [`iter.go`](file:///d:/DBMS_Scratch/byodb/btree/iter.go) | Provides the stateful B+tree iterator `BIter` with root-to-leaf path tracking (`path []BNode`, `pos []uint16`), `SeekLE`, `Next`, `Prev`, `Deref`, and binary search key lookup (`lookupLessOrEqual`). |
| [`btree_test.go`](file:///d:/DBMS_Scratch/byodb/btree/btree_test.go) | Differential testing harness against `map[string]string`, randomized operation suites (1,000,000+ ops), structural invariant checks (key ordering, equal leaf height, page size bounds), and page audit leak checks. |

---

### Layer 2: `kv/` — Durable Paged File, Transactions & MVCC
*Location:* [`kv/`](file:///d:/DBMS_Scratch/byodb/kv)

| File | Primary Responsibilities & Key Functions |
|---|---|
| [`kv.go`](file:///d:/DBMS_Scratch/byodb/kv/kv.go) | Main `KV` database file structure. Manages 4096-byte page files, page allocation (`pageAlloc`), pending updates map, 2-phase commit (`writePages` → `fsync` → `updateRoot` → `fsync`), and IO error recovery. |
| [`meta.go`](file:///d:/DBMS_Scratch/byodb/kv/meta.go) | Implements double-buffered, CRC32-checksummed, versioned meta page reading and writing (`saveMeta`, `loadMeta`, `readMetaSlot`, `writeMetaSlot`), alternating between slot 0 (offset 0) and slot 1 (offset 512). |
| [`freelist.go`](file:///d:/DBMS_Scratch/byodb/kv/freelist.go) | Intrusive unrolled linked list of reusable pages (`freeList`, `LNode`). Manages monotonic sequence numbers (`headSeq`, `tailSeq`), `maxSeq` protection (preventing an update from reusing pages freed during the same update), and `maxVer` version stamps for MVCC snapshots. |
| [`tx.go`](file:///d:/DBMS_Scratch/byodb/kv/tx.go) | Implements read-only snapshots and read-write transactions (`KVTX`). Provides local update buffering (`pending` in-memory B+tree), combined iterator (`TxIter`), and optimistic concurrency control (OCC) conflict detection (`detectConflicts`, `KeyRange` history tracking). |
| [`pages_mmap.go`](file:///d:/DBMS_Scratch/byodb/kv/pages_mmap.go) | Memory-mapped page backend for Unix/macOS (`mmap`, `extendMmap`). |
| [`pages_pread.go`](file:///d:/DBMS_Scratch/byodb/kv/pages_pread.go) | Portable page backend using explicit `pread` calls with a FIFO page cache (used on Windows and selectable via `-tags byodb_pread`). |
| [`open_windows.go`](file:///d:/DBMS_Scratch/byodb/kv/open_windows.go) | Windows file creation with `CreateFile` and `FILE_SHARE_READ` for exclusive writer locking. |
| [`open_unix.go`](file:///d:/DBMS_Scratch/byodb/kv/open_unix.go) | Unix file creation using `openat` and parent directory `fsync`. |
| [`open_other.go`](file:///d:/DBMS_Scratch/byodb/kv/open_other.go) | Standard OS file opening for macOS and BSD systems. |
| [`kv_test.go`](file:///d:/DBMS_Scratch/byodb/kv/kv_test.go) | Differential KV tests against a reference map, reopen durability checks, and free list plateau growth assertions. |
| [`tx_test.go`](file:///d:/DBMS_Scratch/byodb/kv/tx_test.go) | Transaction tests verifying byte-identical file aborts, snapshot isolation, read-your-own-writes, and conflict detection. |
| [`crash_test.go`](file:///d:/DBMS_Scratch/byodb/kv/crash_test.go) | Crash fault injection suite (injecting failures at every write and `fsync`), single/double meta page corruption recovery, and transient write error recovery tests. |

---

### Layer 3: `relational/` — Tables, Schemas & Secondary Indexes
*Location:* [`relational/`](file:///d:/DBMS_Scratch/byodb/relational)

| File | Primary Responsibilities & Key Functions |
|---|---|
| [`value.go`](file:///d:/DBMS_Scratch/byodb/relational/value.go) | Tagged union `Value` type supporting `TypeBytes` (1), `TypeInt64` (2), `TypeFloat64` (3). Defines `Record` row struct, value constructors (`Bytes`, `Int64`, `Float64`), string formatters, and equality helpers. |
| [`encoding.go`](file:///d:/DBMS_Scratch/byodb/relational/encoding.go) | Order-preserving binary byte encoder/decoder (`encodeValues`, `decodeValues`). Encodes integers (sign bit flip), double-precision floats (IEEE 754 sign/bit flipping), strings (null-termination & `0x01` escaping), and range partial bounds (`encodeKeyPartial`, `0xff` infinity marker). |
| [`schema.go`](file:///d:/DBMS_Scratch/byodb/relational/schema.go) | Schema definition struct `TableDef` (columns, types, primary key, secondary indexes, 32-bit table prefix). Manages system internal tables (`@table` for table definitions, `@meta` for prefix counter), `TableNew`, `TableDrop`, `TableList`, and `TableDef` lookup. |
| [`table.go`](file:///d:/DBMS_Scratch/byodb/relational/table.go) | Primary table row CRUD operations (`Get`, `Insert`, `Update`, `Upsert`/`Replace`, `Delete`). Encodes row keys and payloads, and maintains secondary index synchronization (`updateIndexes`, `encodeIndexKey`). |
| [`scanner.go`](file:///d:/DBMS_Scratch/byodb/relational/scanner.go) | Table range query scanner (`Scanner`). Picks the matching primary or secondary index (`selectIndex`), extracts boundary values (`boundaryValues`), and dereferences secondary index entries back to primary rows. |
| [`db.go`](file:///d:/DBMS_Scratch/byodb/relational/db.go) | High-level database wrapper (`DB`), transaction wrapper (`DBTX`), read-only views (`View`), and auto-retrying write transaction manager (`Update`) with exponential backoff and randomized jitter. |
| [`relational_test.go`](file:///d:/DBMS_Scratch/byodb/relational/relational_test.go) | Differential relational tests against linear slice of structs, secondary index consistency validation, float64 sort order tests, and table drop/create tests. |

---

### Layer 4: `ql/` — SQL Parser, Evaluator & Execution Engine
*Location:* [`ql/`](file:///d:/DBMS_Scratch/byodb/ql)

| File | Primary Responsibilities & Key Functions |
|---|---|
| [`ast.go`](file:///d:/DBMS_Scratch/byodb/ql/ast.go) | AST node definitions (`NodeSym`, `NodeInt`, `NodeFloat`, `NodeStr`, `NodeOr`, `NodeAnd`, `NodeEq`, `NodeAdd`, `NodeCountStar`, etc.). Defines `Scan` range struct and statement structs (`Select`, `Insert`, `Update`, `Delete`, `CreateTable`, `DropTable`). |
| [`lexer.go`](file:///d:/DBMS_Scratch/byodb/ql/lexer.go) | Tokenizer (`tokenize`). Converts SQL string into tokens (`tokSymbol`, `tokInt`, `tokFloat`, `tokString`, `tokOperator`), strips `--` comments, handles single-quote escaping, and formats line/column error locations. |
| [`parser.go`](file:///d:/DBMS_Scratch/byodb/ql/parser.go) | Top-down recursive-descent parser (`Parse`, `statement`). Enforces operator precedence levels (`exprOr` → `exprAnd` → `exprNot` → `exprCompare` → `exprAdd` → `exprMul` → `exprUnary` → `exprAtom`). Parses `INDEX BY`, `FILTER`, `LIMIT`, and `COUNT(*)`. |
| [`eval.go`](file:///d:/DBMS_Scratch/byodb/ql/eval.go) | Expression evaluator (`evalExpr`). Computes values for literals, columns, unary operations, boolean logic, comparisons (`evalComparison`), arithmetic (`evalArithmetic`), float operations (`toFloat`), and truthiness (`truthy`). |
| [`exec.go`](file:///d:/DBMS_Scratch/byodb/ql/exec.go) | Execution engine (`Execute`, `Run`). Constructs streaming iterator pipelines (`selectIter`, `scanIter`, `singleRowIter`), sets up index bounds (`setUpScan`), and handles `COUNT(*)` aggregations. |
| [`ql_test.go`](file:///d:/DBMS_Scratch/byodb/ql/ql_test.go) | End-to-end SQL statement tests, expression evaluation tests, float range query tests, and aggregate count tests. |
| [`fuzz_test.go`](file:///d:/DBMS_Scratch/byodb/ql/fuzz_test.go) | Parser fuzzing suite (650,000+ executions) ensuring invalid input never panics the parser. |

---

### Auxiliary Packages & User Interfaces

| Package / File | Description & Purpose |
|---|---|
| [`web/server.go`](file:///d:/DBMS_Scratch/byodb/web/server.go) | Embedded REST API web server (`NewServer`, `Start`). Serves endpoints (`/`, `/api/tables`, `/api/query`, `/api/stats`), manages JSON request decoding and transaction-safe row extraction. |
| [`web/app.html`](file:///d:/DBMS_Scratch/byodb/web/app.html) | Single-Page Application (SPA) Web Workbench UI. Built with modern glassmorphic dark mode CSS, SQL editor, quick snippet loader, data grid table renderer, sidebar schema tree, and B+tree telemetry cards. |
| [`cmd/byodb/main.go`](file:///d:/DBMS_Scratch/byodb/cmd/byodb/main.go) | CLI entry point. Handles arguments (`byodb file.db`, `byodb file.db "SQL"`, `byodb --web file.db port`), interactive REPL (`byodb>`), and dot-commands (`.tables`, `.schema`, `.help`, `.quit`). |
| [`integration/integration_test.go`](file:///d:/DBMS_Scratch/byodb/integration/integration_test.go) | Full-stack end-to-end integration tests, multi-table transactions, index consistency across aborts, and `SIGKILL` fault recovery tests (`TestKillMidWrite`). |
| [`integration/bench_test.go`](file:///d:/DBMS_Scratch/byodb/integration/bench_test.go) | Performance benchmarks for point queries and batched insertions. |
| [`internal/assert/assert.go`](file:///d:/DBMS_Scratch/byodb/internal/assert/assert.go) | Zero-allocation internal invariant assertion helper (`Assert`). |

---

## 3. Data Flow & Execution Walkthrough

Here is how data flows through the four layers when executing a SQL query:

```
[ User Query / Web API ]
          │
          ▼
    ql.Parse(sql)  ───► Lexer (lexer.go) ───► Token Stream ───► Parser (parser.go) ───► AST Statement
          │
          ▼
   ql.Execute(tx, stmt)
          │
          ▼  (relational/scanner.go & relational/table.go)
  1. Select Index (selectIndex)
  2. Encode Range Bounds (encodeKeyPartial)
  3. Start Range Scan (tx.Scan)
          │
          ▼  (kv/tx.go)
  4. Seek B+Tree Snapshot + Pending Writes (TxIter)
          │
          ▼  (btree/iter.go & btree/node.go)
  5. Traverse Nodes & Read 4096B Pages (pageRead callback)
          │
          ▼  (relational/encoding.go)
  6. Decode Raw Bytes to Typed Values (decodeValues)
          │
          ▼  (ql/eval.go & ql/exec.go)
  7. Filter & Evaluate Expressions (evalExpr)
          │
          ▼
[ Result Grid / CLI Output ]
```

---

## 4. How to Extend & Create New Files

When adding a new feature (such as a new data type, SQL statement, or storage backend), follow this exact flow:

```
Step 1: Define Constants & Types     └─► relational/value.go
Step 2: Add Order-Preserving Codec   └─► relational/encoding.go
Step 3: Update Schema Validation     └─► relational/schema.go
Step 4: Update Lexer & AST           └─► ql/lexer.go & ql/ast.go
Step 5: Extend Parser & Evaluator    └─► ql/parser.go & ql/eval.go
Step 6: Update Executor              └─► ql/exec.go
Step 7: Add Unit Tests               └─► relational/relational_test.go & ql/ql_test.go
```

### Example Flow: Adding a New Data Type (e.g., `TIMESTAMP` or `FLOAT64`)
1. **Define Type Constant** in [`relational/value.go`](file:///d:/DBMS_Scratch/byodb/relational/value.go):
   Add `TypeTimestamp = 4` to `const (...)`, update `TypeName()`, `Value` struct fields, `String()` formatting, and `Equal()`.
2. **Add Order-Preserving Encoding** in [`relational/encoding.go`](file:///d:/DBMS_Scratch/byodb/relational/encoding.go):
   Add encoding case in `encodeValues()` and decoding case in `decodeValues()`. Ensure the byte encoding preserves natural sort order.
3. **Allow in Schema Validation** in [`relational/schema.go`](file:///d:/DBMS_Scratch/byodb/relational/schema.go):
   Update column type validation in `createTable()` logic.
4. **Update Lexer & Parser** in [`ql/lexer.go`](file:///d:/DBMS_Scratch/byodb/ql/lexer.go) & [`ql/parser.go`](file:///d:/DBMS_Scratch/byodb/ql/parser.go):
   Add keyword token to lexer, and update `columnType()` in `parser.go`.
5. **Update Evaluator** in [`ql/eval.go`](file:///d:/DBMS_Scratch/byodb/ql/eval.go):
   Update `evalExpr()`, `evalComparison()`, and `evalArithmetic()` to handle operations on the new type.
6. **Verify with Tests**:
   Add test cases in `relational/relational_test.go` and `ql/ql_test.go`, then run `go test ./...` and `go vet ./...`.

---

## 5. Binary On-Disk Page Format

Every page in the database file is exactly **4096 bytes**:

```
Page 0 (Meta Page):
┌──────────────────┬──────────────────┬──────────────────┬──────────────────┐
│   Slot 0 (256B)  │   Slot 1 (256B)  │      Unused      │    Slot 0/1 Alt. │
│ Sig|Root|Flushed │ Sig|Root|Flushed │                  │    CRC32 Checksum│
└──────────────────┴──────────────────┴──────────────────┴──────────────────┘

Pages 1..N (B+Tree Node Format):
┌──────────┬──────────┬──────────────────┬──────────────────┬─────────────────────┐
│ Type(2B) │NKeys(2B) │ Pointers (N*8B)  │ Offsets (N*2B)   │ Packed Key-Values   │
│ 1=Node   │          │ (Internal Nodes) │ (KV byte offsets)│ klen|vlen|key|val   │
│ 2=Leaf   │          │                  │                  │ ...                 │
└──────────┴──────────┴──────────────────┴──────────────────┴─────────────────────┘
```

---

## 6. Build, Test & Run Commands

```powershell
# Build CLI executable & Web Workbench
go build -o byodb.exe ./cmd/byodb

# Run Unit & Integration Test Suites
go test ./...

# Run Portable Pread Backend Test Suite
go test -tags byodb_pread ./...

# Run Static Analysis & Linter
go vet ./...

# Launch Web Workbench GUI (http://localhost:8080)
.\byodb.exe --web mydatabase.db 8080

# Launch CLI Interactive Shell
.\byodb.exe mydatabase.db
```
