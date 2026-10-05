package integration

import (
	"fmt"
	"testing"

	"byodb/relational"
)

func benchDB(b *testing.B) *relational.DB {
	db, err := relational.Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	err = db.Update(func(tx *relational.DBTX) error {
		return tx.TableNew(&relational.TableDef{
			Name: "t", Cols: []string{"id", "v"},
			Types:   []uint32{relational.TypeInt64, relational.TypeBytes},
			Indexes: [][]string{{"id"}},
		})
	})
	if err != nil {
		b.Fatal(err)
	}
	return db
}

func seed(b *testing.B, db *relational.DB, n int) {
	err := db.Update(func(tx *relational.DBTX) error {
		for i := 0; i < n; i++ {
			rec := relational.Record{}
			rec.AddInt64("id", int64(i)).AddStr("v", []byte(fmt.Sprintf("value-%08d", i)))
			if _, err := tx.Insert("t", rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

// BenchmarkPointQuery measures a single-row lookup, which is dominated by the
// in-node search and the schema lookup that precedes it.
func BenchmarkPointQuery(b *testing.B) {
	db := benchDB(b)
	seed(b, db, 100000)
	b.ResetTimer()
	err := db.View(func(tx *relational.DBTX) error {
		for i := 0; i < b.N; i++ {
			rec := (&relational.Record{}).AddInt64("id", int64(i%100000))
			if _, err := tx.Get("t", rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

// BenchmarkScan measures sequential reads, where the schema is looked up once.
func BenchmarkScan(b *testing.B) {
	db := benchDB(b)
	seed(b, db, 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := db.View(func(tx *relational.DBTX) error {
			req := relational.Scanner{Cmp1: 3, Cmp2: -3}
			if err := tx.Scan("t", &req); err != nil {
				return err
			}
			n := 0
			for ; req.Valid() && n < 1000; req.Next() {
				var rec relational.Record
				if err := req.Deref(&rec); err != nil {
					return err
				}
				n++
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInsertBatch measures buffered writes inside one transaction.
func BenchmarkInsertBatch(b *testing.B) {
	db := benchDB(b)
	b.ResetTimer()
	err := db.Update(func(tx *relational.DBTX) error {
		for i := 0; i < b.N; i++ {
			rec := relational.Record{}
			rec.AddInt64("id", int64(i)).AddStr("v", []byte("value"))
			if _, err := tx.Insert("t", rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}
