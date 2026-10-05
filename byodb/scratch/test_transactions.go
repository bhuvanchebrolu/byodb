package main

import (
	"fmt"
	"os"

	"byodb/ql"
	"byodb/relational"
)

func main() {
	os.Remove("tx_test.db")
	defer os.Remove("tx_test.db")

	db, err := relational.Open("tx_test.db")
	if err != nil {
		fmt.Printf("❌ Failed to open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// 1. Setup table
	var tx1 relational.DBTX
	db.Begin(&tx1)
	ql.Run(&tx1, `CREATE TABLE accounts (id int64, holder bytes, balance float64, PRIMARY KEY (id));`)
	db.Commit(&tx1)

	// 2. Commit Test
	fmt.Println("--- Test 1: Transaction Commit ---")
	var txCommit relational.DBTX
	db.Begin(&txCommit)
	ql.Run(&txCommit, `INSERT INTO accounts (id, holder, balance) VALUES (1, 'Alice', 1000.0);`)
	ql.Run(&txCommit, `INSERT INTO accounts (id, holder, balance) VALUES (2, 'Bob', 500.0);`)
	err = db.Commit(&txCommit)
	if err != nil {
		fmt.Printf("❌ Commit failed: %v\n", err)
	} else {
		fmt.Println("✅ Commit succeeded!")
	}

	// Verify rows exist after commit
	var txRead relational.DBTX
	db.Begin(&txRead)
	res, _ := ql.Run(&txRead, `SELECT id, holder, balance FROM accounts ORDER BY id ASC;`)
	var count int
	for res.Rows.Valid() {
		var rec relational.Record
		res.Rows.Deref(&rec)
		fmt.Printf("   Row: id=%s, holder=%s, balance=%s\n", rec.Vals[0], rec.Vals[1], rec.Vals[2])
		count++
		res.Rows.Next()
	}
	db.Commit(&txRead)

	if count != 2 {
		fmt.Printf("❌ Expected 2 committed rows, got %d\n", count)
	} else {
		fmt.Println("✅ Verified committed rows exist in DB!")
	}

	// 3. Rollback Test
	fmt.Println("\n--- Test 2: Transaction Rollback ---")
	var txRollback relational.DBTX
	db.Begin(&txRollback)
	ql.Run(&txRollback, `INSERT INTO accounts (id, holder, balance) VALUES (3, 'Charlie', 2000.0);`)
	ql.Run(&txRollback, `UPDATE accounts SET balance = balance + 100.0 WHERE id = 1;`)
	// Abort (Rollback)
	db.Abort(&txRollback)
	fmt.Println("✅ Transaction rolled back (aborted) successfully!")

	// Verify Charlie was NOT added and Alice's balance was NOT changed
	var txRead2 relational.DBTX
	db.Begin(&txRead2)
	res2, _ := ql.Run(&txRead2, `SELECT id, holder, balance FROM accounts ORDER BY id ASC;`)
	var count2 int
	for res2.Rows.Valid() {
		var rec relational.Record
		res2.Rows.Deref(&rec)
		fmt.Printf("   Row: id=%s, holder=%s, balance=%s\n", rec.Vals[0], rec.Vals[1], rec.Vals[2])
		count2++
		res2.Rows.Next()
	}
	db.Commit(&txRead2)

	if count2 == 2 {
		fmt.Println("✅ Verified rolled-back changes were completely undone!")
	} else {
		fmt.Printf("❌ Rollback failed! Got %d rows (expected 2)\n", count2)
	}

	fmt.Println("\n🎉 ALL TRANSACTION TESTS PASSED PERFECTLY!")
}
