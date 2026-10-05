     # Layer 3 — Transactions & Query Engine (SQL Parser, Execution Pipeline & Workbench)

     > **Module Responsibility**: Parses raw SQL strings into Abstract Syntax Trees (AST), evaluates expressions, executes streaming query pipelines, manages Snapshot Isolation transactions, and serves the Web Workbench interface.

     ---

     ## 📂 Directory Structure & File Map

     ```text
     byodb/
     ├── ql/                   # Query Language engine (Lexer, Parser, AST, Evaluator, Executor)
     │   ├── lexer.go          # Lexical scanner & tokenization
     │   ├── parser.go         # Recursive descent parser (converts tokens to AST)
     │   ├── ast.go            # AST struct definitions (Select, Insert, Update, CreateTable, Node)
     │   ├── eval.go           # Expression evaluator (math, comparisons, LIKE, BETWEEN, IN)
     │   ├── exec.go           # Query execution pipeline, streaming iterators & subquery resolution
     │   └── ql_test.go        # Comprehensive SQL unit & integration tests
     ├── web/                  # Web Workbench server & UI
     │   ├── server.go         # Go HTTP server (/api/query, /api/tables, /api/stats)
     │   └── app.html          # Embedded single-page Web Workbench GUI
     └── cmd/byodb/            # CLI binary entry point
     └── main.go           # Interactive REPL CLI & --web server flag launcher
     ```

     ---

     ## 🏗️ Architecture & Component Flow

     ```mermaid
     graph TD
     A["Raw SQL String"] -->|ql/lexer.go| B["Token Stream"]
     B -->|ql/parser.go| C["Abstract Syntax Tree (AST)"]
     C -->|ql/exec.go Execute()| D["Subquery Resolver (resolveSubqueries)"]
     D -->|Streaming Iterators| E["ScanIter -> JoinIter -> GroupBy -> OrderBy -> Limit"]
     E -->|Evaluate Expressions| F["ql/eval.go (evalExpr)"]
     F -->|Read/Write Rows| G["Relational Layer (Layer 2)"]
     G -->|ACID Tx Lock/Commit| H["kv.DBTX Snapshot Transaction"]
     ```

     ---

     ## 🔑 Key Concepts & Mechanisms

     ### 1. Lexical Analysis & Tokenization (`ql/lexer.go`)
     Converts raw SQL text into a list of structured tokens (`tokSymbol`, `tokInt`, `tokFloat`, `tokStr`, `tokOp`).
     - Handles SQL keywords (`SELECT`, `FROM`, `WHERE`, `FILTER`, `JOIN`, `LIKE`, `BETWEEN`, `IN`, `UNION`, `CREATE VIEW`).
     - Preserves strings, numeric literals, and punctuation.

     ---

     ### 2. Recursive Descent Parsing & AST (`ql/parser.go` & `ql/ast.go`)
     Parses token streams into strongly typed AST structs:
     - **`Select`**: Fields for output expressions, source table/view, joins, filter, group-by, order-by, distinct, limit, and offset.
     - **`CreateTable`**: Schema def, `IfNotExists`, `AutoIncCol`, `Uniques`, `ForeignKeys`.
     - **`CreateView` / `DropView`**: Virtual view queries.
     - **`Node`**: Expression tree node representing operators (`+`, `-`, `=`, `LIKE`, `BETWEEN`, `IN`, `AND`, `OR`).

     #### Operator Precedence Hierarchy (Low to High)
     1. `OR`
     2. `AND`
     3. `NOT`
     4. Comparisons (`=`, `!=`, `<`, `<=`, `>`, `>=`, `LIKE`, `BETWEEN`, `IN`)
     5. Arithmetic (`+`, `-`, `*`, `/`, `%`)

     ---

     ### 3. Subquery Resolution & Expression Evaluation (`ql/eval.go` & `ql/exec.go`)

     - **Subquery Resolution (`resolveSubqueries`)**:
     Before executing the outer query scan, any subquery in `WHERE col IN (SELECT ...)` is executed against `tx`, its result values are gathered into a literal set, and the AST node is dynamically rewritten to `WHERE col IN (val1, val2)`.
     - **Pattern Matching (`evalLike`)**: Converts `%` to `.*` and `_` to `.`, compiling a case-insensitive regular expression to evaluate string matches.
     - **Range Check (`evalBetween`)**: Evaluates `low <= val <= high`.

     ---

     ### 4. Streaming Query Execution Pipeline (`ql/exec.go`)

     Queries stream rows through a chain of **iterators** (`RecordIter`), avoiding loading full datasets into memory:

     ```text
     [B+Tree Scanner] 
          │
          ▼
     [scanIter] (Applies FILTER / WHERE predicates)
          │
          ▼
     [joinIter] (Performs INNER / LEFT / RIGHT joins)
          │
          ▼
     [executeGroupBy] (Computes COUNT, SUM, AVG, MIN, MAX)
          │
          ▼
     [applyOrderBy] (Sorts rows by expressions)
          │
          ▼
     [selectIter] (Projects final output columns & aliases)
          │
          ▼
     [applyDistinct] (Deduplicates output tuples)
          │
          ▼
     [applyLimitOffset] (Applies final LIMIT & OFFSET pagination)
     ```

     ---

     ### 5. Snapshot Isolation & Concurrency Control (`kv/kv.go` & `web/server.go`)
     - **`db.Begin(&tx)`**: Holds a read snapshot pinned to a specific B-Tree root version.
     - **`db.Commit(&tx)`**: Validates optimistic concurrency conflict checks. If no other transaction modified the same pages, dirty pages write atomically; otherwise, `ErrConflict` triggers a rollback.

     ---

     ### 6. Web Workbench Server (`web/server.go`)
     Provides a web-based GUI running on port 8080 (`http://localhost:8080`).
     - **`/api/query`**: Executes semi-colon separated SQL strings inside a transaction.
     - **`/api/tables`**: Returns JSON list of tables, columns, indexes, constraints, and virtual views.
     - **`/api/stats`**: Displays database page usage and storage stats.

     ---

     ## 🛠️ Data Structures Summary

     ### `Select` AST (`ql/ast.go`)
     ```go
     type Select struct {
     Scan
     Distinct bool
     Names    []string
     Output   []Node
     GroupBy  []Node
     OrderBy  []OrderItem
     }
     ```

     ### Expression `Node` (`ql/ast.go`)
     ```go
     type Node struct {
     Type int       // NodeEq, NodeLike, NodeBetween, NodeIn, NodeAnd, etc.
     I64  int64
     F64  float64
     Str  []byte
     Kids []Node
     Sub  any       // Subquery *Select AST
     }
     ```

     ---

     ## 🧪 Verification & Unit Tests
     Run the query engine test suite:
     ```bash
     go test ./ql ./integration
     ```
