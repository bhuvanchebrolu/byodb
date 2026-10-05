package relational

import (
	"errors"
	"fmt"
	"math/rand"
	"time"

	"byodb/kv"
)

// DB is a relational database: tables and indexes over one key-value store.
type DB struct {
	kv *kv.KV
}

// Open opens or creates a database at path.
func Open(path string) (*DB, error) {
	store, err := kv.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{kv: store}, nil
}

// Close releases the underlying file.
func (db *DB) Close() error { return db.kv.Close() }

// DBTX is a transaction over the relational database.
//
// Every table operation lives here rather than on DB, because one logical row
// change is several key changes once indexes exist: writing a row and updating
// its index entries have to succeed or fail together, and only a transaction
// can promise that.
type DBTX struct {
	db *DB
	kv kv.KVTX

	// schemas caches table definitions for the life of this transaction.
	//
	// Every row operation begins by looking up a schema, which is a key lookup
	// plus a JSON decode — real cost when a statement touches many rows.
	//
	// The cache is per-transaction rather than shared, which makes its
	// correctness trivial: a transaction reads from a fixed snapshot, so a
	// schema cannot change underneath it, and a definition created by a
	// transaction that later aborts is discarded along with the cache. A
	// process-wide cache would have to be invalidated across transactions and
	// could serve an uncommitted schema to a reader.
	schemas map[string]*TableDef
}

// Begin starts a transaction.
func (db *DB) Begin(tx *DBTX) {
	tx.db = db
	tx.schemas = map[string]*TableDef{}
	db.kv.Begin(&tx.kv)
}

// Commit makes the transaction's changes durable, or returns kv.ErrConflict if
// another transaction has changed data this one read.
func (db *DB) Commit(tx *DBTX) error { return db.kv.Commit(&tx.kv) }

// Abort discards the transaction.
func (db *DB) Abort(tx *DBTX) { db.kv.Abort(&tx.kv) }

// MaxRetries is how many times Update re-runs a transaction that conflicted.
const MaxRetries = 32

// Update runs fn inside a transaction, committing it if fn succeeds and
// aborting it otherwise.
//
// It retries on conflict, which is the whole obligation optimistic concurrency
// control places on a caller: a conflicted transaction changed nothing, so
// running it again against the newer state is safe. fn must therefore be
// prepared to run more than once.
//
// Retries back off. Without a delay, every writer that lost a race re-reads
// and re-commits at the same instant and they collide again — contention that
// a moment's wait resolves turns into a burst of failures instead. The delay
// doubles up to a ceiling, with jitter so that writers which conflicted
// together do not come back together.
func (db *DB) Update(fn func(tx *DBTX) error) error {
	backoff := 50 * time.Microsecond
	const maxBackoff = 10 * time.Millisecond

	var lastErr error
	for attempt := 0; attempt < MaxRetries; attempt++ {
		if attempt > 0 {
			jitter := time.Duration(rand.Int63n(int64(backoff) + 1))
			time.Sleep(backoff/2 + jitter)
			if backoff < maxBackoff {
				backoff *= 2
			}
		}

		var tx DBTX
		db.Begin(&tx)
		if err := fn(&tx); err != nil {
			db.Abort(&tx)
			return err
		}
		err := db.Commit(&tx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, kv.ErrConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("relational: gave up after %d conflicting attempts: %w", MaxRetries, lastErr)
}

// View runs fn inside a read-only transaction.
func (db *DB) View(fn func(tx *DBTX) error) error {
	var tx DBTX
	db.Begin(&tx)
	defer db.Abort(&tx)
	return fn(&tx)
}
