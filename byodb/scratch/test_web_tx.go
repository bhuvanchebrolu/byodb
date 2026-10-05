package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"

	"byodb/ql"
	"byodb/relational"
	"byodb/web"
)

type QReq struct {
	Query string `json:"query"`
}

type QResp struct {
	Columns  []string   `json:"columns,omitempty"`
	Rows     [][]string `json:"rows,omitempty"`
	Affected int64      `json:"affected"`
	InTx     bool       `json:"in_tx"`
	Error    string     `json:"error,omitempty"`
}

func main() {
	os.Remove("web_tx_test.db")
	defer os.Remove("web_tx_test.db")

	db, err := relational.Open("web_tx_test.db")
	if err != nil {
		fmt.Printf("❌ Failed to open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Seed table
	var initTx relational.DBTX
	db.Begin(&initTx)
	ql.Run(&initTx, `CREATE TABLE accounts (id int64, holder bytes, balance float64, PRIMARY KEY (id));`)
	ql.Run(&initTx, `INSERT INTO accounts (id, holder, balance) VALUES (1, 'Alice', 1000.0);`)
	db.Commit(&initTx)

	srv := web.NewServer(db, "web_tx_test.db", 8080)
	ts := httptest.Server{
		URL:     "http://localhost:8080",
		Listener: nil,
	}
	_ = ts

	postQuery := func(sql string) QResp {
		body, _ := json.Marshal(QReq{Query: sql})
		req := httptest.NewRequest("POST", "/api/query", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		res := w.Result()
		b, _ := io.ReadAll(res.Body)
		var resp QResp
		json.Unmarshal(b, &resp)
		return resp
	}

	fmt.Println("--- Web Workbench Transaction Test ---")

	// 1. Initial count
	r1 := postQuery("SELECT * FROM accounts;")
	fmt.Printf("Initial row count: %d (in_tx=%v)\n", len(r1.Rows), r1.InTx)
	if len(r1.Rows) != 1 {
		fmt.Printf("❌ Expected 1 row, got %d\n", len(r1.Rows))
		os.Exit(1)
	}

	// 2. BEGIN
	r2 := postQuery("BEGIN;")
	fmt.Printf("After BEGIN: in_tx=%v\n", r2.InTx)
	if !r2.InTx {
		fmt.Printf("❌ Expected in_tx=true after BEGIN\n")
		os.Exit(1)
	}

	// 3. INSERT inside transaction
	r3 := postQuery("INSERT INTO accounts (id, holder, balance) VALUES (99, 'TempUser', 500.0);")
	fmt.Printf("After INSERT in TX: affected=%d, in_tx=%v\n", r3.Affected, r3.InTx)
	if !r3.InTx || r3.Affected != 1 {
		fmt.Printf("❌ Expected affected=1, in_tx=true\n")
		os.Exit(1)
	}

	// 4. SELECT inside transaction (should see TempUser)
	r4 := postQuery("SELECT id, holder FROM accounts ORDER BY id ASC;")
	fmt.Printf("Inside TX SELECT row count: %d (in_tx=%v)\n", len(r4.Rows), r4.InTx)
	if len(r4.Rows) != 2 {
		fmt.Printf("❌ Expected 2 rows inside uncommitted TX, got %d\n", len(r4.Rows))
		os.Exit(1)
	}

	// 5. ROLLBACK
	r5 := postQuery("ROLLBACK;")
	fmt.Printf("After ROLLBACK: in_tx=%v\n", r5.InTx)
	if r5.InTx {
		fmt.Printf("❌ Expected in_tx=false after ROLLBACK\n")
		os.Exit(1)
	}

	// 6. SELECT outside transaction (TempUser MUST be rolled back)
	r6 := postQuery("SELECT id, holder FROM accounts ORDER BY id ASC;")
	fmt.Printf("After ROLLBACK SELECT row count: %d (in_tx=%v)\n", len(r6.Rows), r6.InTx)
	if len(r6.Rows) != 1 {
		fmt.Printf("❌ ROLLBACK FAILED! Expected 1 row, got %d\n", len(r6.Rows))
		os.Exit(1)
	}

	fmt.Println("✅ ALL WEB WORKBENCH TRANSACTION & ROLLBACK TESTS PASSED 100%!")
}
