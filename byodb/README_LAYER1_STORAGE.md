# Layer 1 — Storage Engine (B+Tree, Disk Pages & Page Cache)

> **Module Responsibility**: Manages low-level disk I/O, 4KB page serialization, B+Tree indexing structures, Copy-On-Write (COW) page allocation, and file locking.

---

## 📂 Directory Structure & File Map

```text
byodb/
├── btree/                # Pure in-memory B+Tree node manipulation & byte encoding
│   ├── btree.go          # B+Tree traversal algorithms (Get, Seek, Insert, Delete)
│   ├── node.go           # 4KB page binary format, node split/merge, binary search
│   └── btree_test.go     # Unit tests verifying B+Tree invariants
└── kv/                   # Key-Value page manager, file I/O & page cache
    ├── kv.go             # Transactional page cache, Free List, atomic commits
    ├── open_windows.go   # Windows OS file locking (flock) implementation
    └── kv_test.go        # Storage engine concurrency & recovery tests
```

---

## 🏗️ Architecture & Component Flow

```mermaid
graph TD
    A["Relational Layer / Queries"] -->|Key-Value Ops| B["kv.KV Store"]
    B -->|Fetch / Flush Pages| C["kv.PageCache (Memory)"]
    C -->|B+Tree Node Decoding| D["btree.Node (4KB Page)"]
    B -->|Recycle Freed Pages| E["Free List"]
    B -->|File I/O (flock protection)| F[".db Disk File"]
```

---

## 🔑 Key Concepts & Mechanisms

### 1. 4KB Disk Page Layout (`btree/node.go`)
The storage engine divides the database file into fixed **4096-byte pages**. Every page represents one node in a B+Tree.

#### Page Binary Header
| Bytes | Field | Description |
| :--- | :--- | :--- |
| `0-1` | `Type` | Node type (`1` = Internal Node, `2` = Leaf Node) |
| `2-3` | `KeysCount` | Number of key-value pairs stored in this node |
| `4-5` | `FreeOffset` | Offset pointer where unallocated byte space begins |
| `6+`  | `Offsets` | Array of 2-byte pointers pointing to cell byte positions |

```text
+-----------------------------------------------------------------------+
| Type (2B) | Count (2B) | FreePtr (2B) | Offset[0] | Offset[1] | ...   |
+-----------------------------------------------------------------------+
|                            <-- Free Space -->                         |
+-----------------------------------------------------------------------+
| ... | Cell 1 (Key/Val) | Cell 0 (Key/Val)                             |
+-----------------------------------------------------------------------+
```

### 2. In-Node Binary Search (`btree/node.go`)
Instead of scanning key-value pairs sequentially, nodes maintain an array of 2-byte offsets. Searching for a key within a node performs a **binary search over the offset pointers**:
- **Time Complexity**: $O(\log K)$ per node, where $K$ is the number of keys in the page.

### 3. Node Splitting & Merging (`btree/node.go` & `btree/btree.go`)
- **Node Split**: When a 4KB node fills up during an `INSERT`, it is split into two half-full nodes. The median key is promoted up to the parent internal node.
- **Node Merge**: When a node drops below capacity after a `DELETE`, it merges with a sibling node to maintain B+Tree density.

### 4. Copy-On-Write (COW) & Page Cache (`kv/kv.go`)
- Pages are **never overwritten in place**.
- When a page is modified, a new page ID is allocated from the **Free List** or appended to the file.
- Changes remain buffered in the `PageCache` in memory.
- During `Commit()`, all dirty pages are written sequentially, and the root meta page pointer is updated atomically.

### 5. Multi-Process File Locking (`kv/open_windows.go`)
To prevent two processes from opening the same `.db` file simultaneously and corrupting B+Tree state, an OS advisory lock (`flock`) is acquired when `kv.Open()` is invoked.

---

## 🛠️ Data Structures Summary

### `btree.Node` (`btree/node.go`)
```go
type Node struct {
    data []byte // 4096-byte slice backing the page
}
```

### `kv.KV` (`kv/kv.go`)
```go
type KV struct {
    fp       *os.File            // Database disk file handle
    pageMap  map[uint64][]byte   // Active in-memory page cache
    freeList []uint64            // Recycled page IDs available for write
    meta     Meta                // Metadata page holding B+Tree root pointer
}
```

---

## 🧪 Verification & Unit Tests
Run the storage engine test suite:
```bash
go test ./btree ./kv
```
