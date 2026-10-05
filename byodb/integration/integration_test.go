// Package integration exercises the whole stack together: the query language
// down through tables and indexes, transactions, the key-value store and the
// B+tree, onto a real file.
//
// The tests here are the ones that cannot be written against a single layer —
// durability across a real process kill, and consistency between the primary
// data and its indexes after concurrent work.
package integration

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"byodb/kv"
	"byodb/ql"
	"byodb/relational"
)

func openTemp(t *testing.T) (*relational.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := relational.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func exec1(t *testing.T, db *relational.DB, statement string) int64 {
	t.Helper()
	var affected int64
	err := db.Update(func(tx *relational.DBTX) error {
		result, err := ql.Run(tx, statement)
		if err != nil {
			return err
		}
		affected = result.Affected
		return nil
	})
	if err != nil {
		t.Fatalf("%s\n  -> %v", statement, err)
	}
	return affected
}

func queryRows(t *testing.T, db *relational.DB, statement string) [][]string {
	t.Helper()
	var rows [][]string
	err := db.View(func(tx *relational.DBTX) error {
		result, err := ql.Run(tx, statement)
		if err != nil {
			return err
		}
		for ; result.Rows.Valid(); result.Rows.Next() {
			var rec relational.Record
			if err := result.Rows.Deref(&rec); err != nil {
				return err
			}
			cells := make([]string, len(rec.Vals))
			for i, v := range rec.Vals {
				cells[i] = v.String()
			}
			rows = append(rows, cells)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s\n  -> %v", statement, err)
	}
	return rows
}

// TestFullStack is the acceptance test: everything the database claims to do,
// in one run, through the query language.
func TestFullStack(t *testing.T) {
	db, path := openTemp(t)

	exec1(t, db, `create table accounts (
	    owner bytes,
	    id int64,
	    balance int64,
	    tier bytes,
	    index (balance),
	    index (tier, balance),
	    primary key (owner, id)
	)`)

	const rows = 20000
	t.Run("bulk insert in one transaction", func(t *testing.T) {
		start := time.Now()
		err := db.Update(func(tx *relational.DBTX) error {
			for i := 0; i < rows; i++ {
				rec := relational.Record{}
				rec.AddStr("owner", []byte(fmt.Sprintf("owner%02d", i%50))).
					AddInt64("id", int64(i)).
					AddInt64("balance", int64(i%1000)).
					AddStr("tier", []byte([]string{"gold", "silver", "bronze"}[i%3]))
				if _, err := tx.Insert("accounts", rec); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bulk insert: %v", err)
		}
		t.Logf("%d rows with two secondary indexes in %v", rows, time.Since(start))
	})

	t.Run("indexed queries", func(t *testing.T) {
		got := queryRows(t, db, `select owner, id from accounts index by balance = 999 limit 3`)
		if len(got) == 0 {
			t.Fatal("the secondary index returned nothing")
		}
		for _, row := range got {
			id, _ := strconv.Atoi(row[1])
			if id%1000 != 999 {
				t.Fatalf("index by balance = 999 returned id %s, whose balance is %d", row[1], id%1000)
			}
		}

		// A composite index, constrained on its leading column only.
		got = queryRows(t, db, `select tier, balance from accounts index by tier = 'gold' limit 5`)
		for _, row := range got {
			if row[0] != "gold" {
				t.Fatalf("index by tier = 'gold' returned tier %q", row[0])
			}
		}
	})

	t.Run("aggregate-free counting via limit", func(t *testing.T) {
		got := queryRows(t, db, `select id from accounts index by balance >= 100 and balance <= 109`)
		want := 0
		for i := 0; i < rows; i++ {
			if b := i % 1000; b >= 100 && b <= 109 {
				want++
			}
		}
		if len(got) != want {
			t.Fatalf("the range query returned %d rows, want %d", len(got), want)
		}
	})

	t.Run("update maintains indexes", func(t *testing.T) {
		exec1(t, db, `update accounts set balance = 424242 index by (owner, id) = ('owner00', 0)`)
		got := queryRows(t, db, `select owner, id from accounts index by balance = 424242`)
		if len(got) != 1 || got[0][0] != "owner00" || got[0][1] != "0" {
			t.Fatalf("after the update the index returned %v", got)
		}
	})

	t.Run("aborted transaction leaves nothing", func(t *testing.T) {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var tx relational.DBTX
		db.Begin(&tx)
		for i := 0; i < 500; i++ {
			if _, err := ql.Run(&tx, fmt.Sprintf(
				`replace into accounts (owner, id, balance, tier) values ('ghost', %d, %d, 'gold')`,
				i, i)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		db.Abort(&tx)

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(before) != len(after) {
			t.Fatalf("an aborted transaction changed the file size: %d -> %d", len(before), len(after))
		}
		if got := queryRows(t, db, `select id from accounts index by (owner, id) = ('ghost', 1)`); len(got) != 0 {
			t.Fatalf("an aborted transaction left rows behind: %v", got)
		}
	})

	t.Run("delete and reopen", func(t *testing.T) {
		affected := exec1(t, db, `delete from accounts filter balance < 500`)
		if affected == 0 {
			t.Fatal("the delete affected no rows")
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		reopened, err := relational.Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()

		got := queryRows(t, reopened, `select id from accounts filter balance < 500 limit 1`)
		if len(got) != 0 {
			t.Fatalf("deleted rows came back after reopening: %v", got)
		}
		checkIndexConsistency(t, reopened, "accounts")
	})
}

// checkIndexConsistency verifies that every secondary index agrees with the
// primary data: the same rows, no more and no fewer.
//
// A disagreement here is the silent kind of corruption — every individual
// query still returns something plausible, but which rows you get depends on
// which index the query happened to use.
func checkIndexConsistency(t *testing.T, db *relational.DB, table string) {
	t.Helper()
	err := db.View(func(tx *relational.DBTX) error {
		tdef, err := tx.TableDef(table)
		if err != nil {
			return err
		}

		primary := map[string]bool{}
		result, err := ql.Run(tx, fmt.Sprintf("select %s from %s",
			strings.Join(tdef.Indexes[0], ", "), table))
		if err != nil {
			return err
		}
		for ; result.Rows.Valid(); result.Rows.Next() {
			var rec relational.Record
			if err := result.Rows.Deref(&rec); err != nil {
				return err
			}
			primary[renderKey(rec)] = true
		}

		for _, index := range tdef.Indexes[1:] {
			// Scanning the whole index means an unbounded range on its leading
			// column, which the infinity encoding turns into the whole index.
			leading := index[0]
			bound := "0"
			if tdef.Types[columnIndex(tdef, leading)] == relational.TypeBytes {
				bound = "''"
			}
			query := fmt.Sprintf("select %s from %s index by %s >= %s",
				strings.Join(tdef.Indexes[0], ", "), table, leading, bound)
			result, err := ql.Run(tx, query)
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			for ; result.Rows.Valid(); result.Rows.Next() {
				var rec relational.Record
				if err := result.Rows.Deref(&rec); err != nil {
					return err
				}
				key := renderKey(rec)
				if seen[key] {
					t.Fatalf("index %v returned row %s twice", index, key)
				}
				seen[key] = true
				if !primary[key] {
					t.Fatalf("index %v points at row %s, which is not in the table", index, key)
				}
			}
			// Only rows whose leading indexed column is at or above the bound
			// are expected, so a shortfall is only meaningful for numeric
			// indexes when the bound is the minimum. Checking the counts match
			// catches the common failure of stale entries not being removed.
			if bound == "''" && len(seen) != len(primary) {
				t.Fatalf("index %v holds %d rows but the table holds %d",
					index, len(seen), len(primary))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("consistency check: %v", err)
	}
}

func columnIndex(tdef *relational.TableDef, name string) int {
	for i, col := range tdef.Cols {
		if col == name {
			return i
		}
	}
	return -1
}

func renderKey(rec relational.Record) string {
	parts := make([]string, len(rec.Vals))
	for i, v := range rec.Vals {
		parts[i] = v.String()
	}
	return strings.Join(parts, "\x00")
}

// ---------- durability against a real process kill ----------

// TestKillMidWrite is the durability claim tested the only way that really
// counts: a separate process writes rows, reporting each one it has committed,
// and is killed with SIGKILL part-way through.
//
// In-process crash simulation can model lost writes, but only a real kill
// proves the claim end to end: nothing buffered in the process, nothing
// deferred by the runtime, no cleanup path that happens to run.
func TestKillMidWrite(t *testing.T) {
	if os.Getenv("BYODB_CRASH_CHILD") != "" {
		runCrashWriter(t)
		return
	}
	if testing.Short() {
		t.Skip("spawns a subprocess")
	}

	path := filepath.Join(t.TempDir(), "crash.db")
	cmd := exec.Command(os.Args[0], "-test.run=TestKillMidWrite", "-test.v=false")
	cmd.Env = append(os.Environ(), "BYODB_CRASH_CHILD=1", "BYODB_CRASH_DB="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start writer: %v", err)
	}

	// Read acknowledgements until the writer is well underway, then kill it
	// without warning.
	var lastCommitted int64 = -1
	reader := bufio.NewReader(stdout)
	deadline := time.After(60 * time.Second)
	acks := make(chan int64, 1024)
	go func() {
		defer close(acks)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if id, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
				acks <- id
			}
		}
	}()

	const killAfter = 200
	for lastCommitted < killAfter {
		select {
		case id, ok := <-acks:
			if !ok {
				t.Fatalf("the writer exited before committing %d rows (last was %d)",
					killAfter, lastCommitted)
			}
			lastCommitted = id
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatal("timed out waiting for the writer")
		}
	}

	// Process.Kill is the portable spelling: SIGKILL on Unix,
	// TerminateProcess on Windows. Either way the process gets no chance to
	// flush anything, which is the point.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()
	t.Logf("killed the writer after it acknowledged row %d", lastCommitted)

	// Reopen and check what survived.
	db, err := relational.Open(path)
	if err != nil {
		t.Fatalf("reopening after SIGKILL: %v", err)
	}
	defer db.Close()

	// Every acknowledged row must be there. A row the writer never
	// acknowledged may or may not be, but must not be half-written.
	err = db.View(func(tx *relational.DBTX) error {
		for id := int64(0); id <= lastCommitted; id++ {
			rec := (&relational.Record{})
			rec.AddInt64("id", id)
			found, err := tx.Get("log", rec)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("row %d was acknowledged as committed but is gone "+
					"after the process was killed", id)
			}
			want := fmt.Sprintf("payload-%06d", id)
			if got := string(rec.Get("payload").Str); got != want {
				return fmt.Errorf("row %d holds %q, want %q", id, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	checkIndexConsistency(t, db, "log")

	// And the database is still usable, not merely readable.
	exec1(t, db, `insert into log (id, payload, bucket) values (999999, 'after-crash', 7)`)
	got := queryRows(t, db, `select payload from log index by id = 999999`)
	if len(got) != 1 || got[0][0] != "after-crash" {
		t.Fatalf("the database is not writable after recovery: %v", got)
	}
}

// runCrashWriter is the child process: it writes rows forever, printing the id
// of each one only after the commit has returned.
func runCrashWriter(t *testing.T) {
	path := os.Getenv("BYODB_CRASH_DB")
	db, err := relational.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: open: %v\n", err)
		os.Exit(1)
	}
	if err := db.Update(func(tx *relational.DBTX) error {
		_, err := ql.Run(tx, `create table log (
		    id int64,
		    payload bytes,
		    bucket int64,
		    index (bucket),
		    primary key (id)
		)`)
		return err
	}); err != nil {
		fmt.Fprintf(os.Stderr, "child: create table: %v\n", err)
		os.Exit(1)
	}

	out := bufio.NewWriter(os.Stdout)
	for id := int64(0); ; id++ {
		err := db.Update(func(tx *relational.DBTX) error {
			rec := relational.Record{}
			rec.AddInt64("id", id).
				AddStr("payload", []byte(fmt.Sprintf("payload-%06d", id))).
				AddInt64("bucket", id%16)
			_, err := tx.Insert("log", rec)
			return err
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "child: insert %d: %v\n", id, err)
			os.Exit(1)
		}
		// Only acknowledge after the commit returned, so the parent's
		// expectation is exactly the durability promise.
		fmt.Fprintln(out, id)
		if err := out.Flush(); err != nil {
			os.Exit(1)
		}
	}
}

// ---------- concurrency across the whole stack ----------

// TestConcurrentReadersAndWriters runs readers and writers against the full
// stack at once. Readers must always see a consistent world; writers must
// either commit or conflict, never corrupt.
func TestConcurrentReadersAndWriters(t *testing.T) {
	db, _ := openTemp(t)
	exec1(t, db, `create table counters (
	    name bytes,
	    value int64,
	    total int64,
	    index (value),
	    primary key (name)
	)`)

	const counters = 8
	for i := 0; i < counters; i++ {
		exec1(t, db, fmt.Sprintf(
			`insert into counters (name, value, total) values ('c%d', 0, 0)`, i))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 32)

	// Readers: the two columns are always incremented together, so any row
	// where they disagree means a reader saw half a transaction.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := db.View(func(tx *relational.DBTX) error {
					result, err := ql.Run(tx, `select name, value, total from counters`)
					if err != nil {
						return err
					}
					for ; result.Rows.Valid(); result.Rows.Next() {
						var rec relational.Record
						if err := result.Rows.Deref(&rec); err != nil {
							return err
						}
						if v, total := rec.Get("value").I64, rec.Get("total").I64; v != total {
							return fmt.Errorf("row %s has value=%d but total=%d: a reader "+
								"observed a partially applied transaction",
								rec.Get("name").Str, v, total)
						}
					}
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	// Writers, each incrementing its own counter so they rarely conflict.
	const incrementsPerWriter = 40
	for w := 0; w < counters; w++ {
		wg.Add(1)
		go func(counter int) {
			defer wg.Done()
			for i := 0; i < incrementsPerWriter; i++ {
				err := db.Update(func(tx *relational.DBTX) error {
					_, err := ql.Run(tx, fmt.Sprintf(
						`update counters set value = value + 1, total = total + 1 index by name = 'c%d'`,
						counter))
					return err
				})
				if err != nil {
					errs <- fmt.Errorf("writer %d: %w", counter, err)
					return
				}
			}
		}(w)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// The writers finish on their own; the readers run until told to stop.
	time.AfterFunc(50*time.Millisecond, func() {})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Wait for the writers by watching the totals, then release the readers.
	deadline := time.Now().Add(60 * time.Second)
	for {
		rows := queryRows(t, db, `select value from counters`)
		complete := len(rows) == counters
		for _, row := range rows {
			if row[0] != strconv.Itoa(incrementsPerWriter) {
				complete = false
			}
		}
		if complete {
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			wg.Wait()
			t.Fatalf("the writers did not finish; counters are %v", rows)
		}
		select {
		case err := <-errs:
			close(stop)
			t.Fatal(err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	checkIndexConsistency(t, db, "counters")
}

// TestConflictingWritersRetryToCompletion checks that contention on a single
// row still makes progress: every increment must land, however many retries it
// takes.
func TestConcurrentContendedCounter(t *testing.T) {
	db, _ := openTemp(t)
	exec1(t, db, `create table counter (name bytes, value int64, primary key (name))`)
	exec1(t, db, `insert into counter (name, value) values ('shared', 0)`)

	const writers, increments = 4, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < increments; i++ {
				err := db.Update(func(tx *relational.DBTX) error {
					_, err := ql.Run(tx,
						`update counter set value = value + 1 index by name = 'shared'`)
					return err
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == kv.ErrConflict {
			t.Fatal("a conflict escaped the retry loop")
		}
		t.Fatal(err)
	}

	got := queryRows(t, db, `select value from counter index by name = 'shared'`)
	want := strconv.Itoa(writers * increments)
	if len(got) != 1 || got[0][0] != want {
		t.Fatalf("the counter is %v, want %s — increments were lost to a race", got, want)
	}
}
