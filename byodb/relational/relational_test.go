package relational

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"byodb/btree"
	"byodb/kv"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// peopleTable is the schema most tests use: a two-column primary key, a
// secondary index, and both supported types in play.
func peopleTable() *TableDef {
	return &TableDef{
		Name:  "people",
		Cols:  []string{"country", "id", "name", "age"},
		Types: []uint32{TypeBytes, TypeInt64, TypeBytes, TypeInt64},
		Indexes: [][]string{
			{"country", "id"}, // primary key
			{"age"},           // secondary index
			{"name", "age"},   // another secondary index
		},
	}
}

func createTable(t *testing.T, db *DB, tdef *TableDef) {
	t.Helper()
	if err := db.Update(func(tx *DBTX) error { return tx.TableNew(tdef) }); err != nil {
		t.Fatalf("TableNew(%s): %v", tdef.Name, err)
	}
}

type person struct {
	country string
	id      int64
	name    string
	age     int64
}

func (p person) record() Record {
	rec := Record{}
	rec.AddStr("country", []byte(p.country)).
		AddInt64("id", p.id).
		AddStr("name", []byte(p.name)).
		AddInt64("age", p.age)
	return rec
}

func upsert(t *testing.T, db *DB, p person) {
	t.Helper()
	if err := db.Update(func(tx *DBTX) error {
		_, err := tx.Upsert("people", p.record())
		return err
	}); err != nil {
		t.Fatalf("upsert %v: %v", p, err)
	}
}

func TestCreateAndPointQuery(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())

	alice := person{"uk", 1, "alice", 30}
	upsert(t, db, alice)

	err := db.View(func(tx *DBTX) error {
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		found, err := tx.Get("people", rec)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("the row just inserted was not found")
		}
		if got := string(rec.Get("name").Str); got != "alice" {
			t.Fatalf("name is %q, want %q", got, "alice")
		}
		if got := rec.Get("age").I64; got != 30 {
			t.Fatalf("age is %d, want 30", got)
		}

		missing := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 999)
		found, err = tx.Get("people", missing)
		if err != nil {
			return err
		}
		if found {
			t.Fatal("a row that was never inserted was found")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestSchemaSurvivesReopen(t *testing.T) {
	db, path := openTemp(t)
	createTable(t, db, peopleTable())
	upsert(t, db, person{"uk", 1, "alice", 30})
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if err := reopened.View(func(tx *DBTX) error {
		names, err := tx.TableList()
		if err != nil {
			return err
		}
		if len(names) != 1 || names[0] != "people" {
			t.Fatalf("after reopening, the tables are %v, want [people]", names)
		}
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		found, err := tx.Get("people", rec)
		if err != nil {
			return err
		}
		if !found || string(rec.Get("name").Str) != "alice" {
			t.Fatal("the row did not survive reopening")
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestInsertUpdateUpsertSemantics(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())

	if err := db.Update(func(tx *DBTX) error {
		p := person{"uk", 1, "alice", 30}
		added, err := tx.Insert("people", p.record())
		if err != nil || !added {
			t.Fatalf("first insert reported added=%v err=%v", added, err)
		}
		// A second insert of the same primary key must be refused.
		added, err = tx.Insert("people", person{"uk", 1, "impostor", 99}.record())
		if err != nil {
			return err
		}
		if added {
			t.Fatal("insert overwrote an existing primary key")
		}
		// Update requires the row to exist.
		updated, err := tx.Update("people", person{"uk", 2, "nobody", 1}.record())
		if err != nil {
			return err
		}
		if updated {
			t.Fatal("update created a row that did not exist")
		}
		// Upsert does both.
		if _, err := tx.Upsert("people", person{"uk", 1, "alice2", 31}.record()); err != nil {
			return err
		}
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		if _, err := tx.Get("people", rec); err != nil {
			return err
		}
		if got := string(rec.Get("name").Str); got != "alice2" {
			t.Fatalf("after upsert the name is %q, want %q", got, "alice2")
		}
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestSchemaValidation(t *testing.T) {
	db, _ := openTemp(t)

	cases := map[string]*TableDef{
		"no primary key": {
			Name: "t", Cols: []string{"a"}, Types: []uint32{TypeInt64},
		},
		"unknown index column": {
			Name: "t", Cols: []string{"a"}, Types: []uint32{TypeInt64},
			Indexes: [][]string{{"a"}, {"nope"}},
		},
		"duplicate column": {
			Name: "t", Cols: []string{"a", "a"}, Types: []uint32{TypeInt64, TypeInt64},
			Indexes: [][]string{{"a"}},
		},
		"type count mismatch": {
			Name: "t", Cols: []string{"a", "b"}, Types: []uint32{TypeInt64},
			Indexes: [][]string{{"a"}},
		},
	}
	for name, tdef := range cases {
		t.Run(name, func(t *testing.T) {
			err := db.Update(func(tx *DBTX) error { return tx.TableNew(tdef) })
			if !errors.Is(err, ErrBadSchema) {
				t.Fatalf("TableNew returned %v, want ErrBadSchema", err)
			}
		})
	}

	// A duplicate table name is a different error.
	valid := &TableDef{Name: "t", Cols: []string{"a"}, Types: []uint32{TypeInt64},
		Indexes: [][]string{{"a"}}}
	createTable(t, db, valid)
	again := &TableDef{Name: "t", Cols: []string{"a"}, Types: []uint32{TypeInt64},
		Indexes: [][]string{{"a"}}}
	if err := db.Update(func(tx *DBTX) error { return tx.TableNew(again) }); !errors.Is(err, ErrTableExists) {
		t.Fatalf("creating a duplicate table returned %v, want ErrTableExists", err)
	}
}

func TestPrimaryKeyReordering(t *testing.T) {
	// The primary key is declared last, so normalize has to move those columns
	// to the front. If it did not, encoding would split the row in the wrong
	// place and reads would return nonsense.
	db, _ := openTemp(t)
	tdef := &TableDef{
		Name:    "events",
		Cols:    []string{"payload", "seq"},
		Types:   []uint32{TypeBytes, TypeInt64},
		Indexes: [][]string{{"seq"}},
	}
	createTable(t, db, tdef)

	if err := db.Update(func(tx *DBTX) error {
		rec := (&Record{}).AddStr("payload", []byte("hello")).AddInt64("seq", 7)
		_, err := tx.Insert("events", *rec)
		return err
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.View(func(tx *DBTX) error {
		rec := (&Record{}).AddInt64("seq", 7)
		found, err := tx.Get("events", rec)
		if err != nil || !found {
			t.Fatalf("lookup by the reordered primary key: found=%v err=%v", found, err)
		}
		if got := string(rec.Get("payload").Str); got != "hello" {
			t.Fatalf("payload is %q, want %q", got, "hello")
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// ---------- encoding ----------

// TestOrderPreservingEncoding is the property the whole range-query layer
// rests on: sorting encoded keys must sort the values they represent.
func TestOrderPreservingEncoding(t *testing.T) {
	t.Run("integers", func(t *testing.T) {
		values := []int64{
			math.MinInt64, math.MinInt64 + 1, -1 << 40, -70000, -256, -2, -1,
			0, 1, 2, 255, 256, 70000, 1 << 40, math.MaxInt64 - 1, math.MaxInt64,
		}
		checkOrder(t, values, func(v int64) []byte { return encodeValues(nil, []Value{Int64(v)}) },
			func(a, b int64) bool { return a < b })
	})

	t.Run("strings", func(t *testing.T) {
		values := []string{
			"", "\x00", "\x00\x00", "\x01", "\x01\x01", "\x02",
			"a", "a\x00", "a\x00b", "ab", "b", "bc", "z", "\xff", "\xff\xff",
		}
		checkOrder(t, values, func(v string) []byte { return encodeValues(nil, []Value{Bytes([]byte(v))}) },
			func(a, b string) bool { return a < b })
	})

	// Tuples are where a careless string encoding shows up: ("a","bc") and
	// ("ab","c") concatenate to the same bytes without a terminator.
	t.Run("tuples", func(t *testing.T) {
		type tuple struct {
			a string
			b string
		}
		values := []tuple{
			{"a", "bc"}, {"a", "c"}, {"ab", "c"}, {"ab", ""}, {"a", ""},
			{"a\x00", "b"}, {"a", "\x00b"},
		}
		checkOrder(t, values, func(v tuple) []byte {
			return encodeValues(nil, []Value{Bytes([]byte(v.a)), Bytes([]byte(v.b))})
		}, func(x, y tuple) bool {
			if x.a != y.a {
				return x.a < y.a
			}
			return x.b < y.b
		})
	})
}

// checkOrder asserts that encoding preserves the given ordering across every
// pair of values.
func checkOrder[T any](t *testing.T, values []T, encode func(T) []byte, less func(a, b T) bool) {
	t.Helper()
	for i := range values {
		for j := range values {
			a, b := encode(values[i]), encode(values[j])
			wantLess := less(values[i], values[j])
			gotLess := string(a) < string(b)
			if wantLess != gotLess {
				t.Fatalf("values %v and %v: encoded order says less=%v, real order says less=%v\n"+
					"  %v -> %x\n  %v -> %x",
					values[i], values[j], gotLess, wantLess, values[i], a, values[j], b)
			}
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for trial := 0; trial < 2000; trial++ {
		vals := []Value{
			Int64(rng.Int63() - rng.Int63()),
			Bytes(randomBytes(rng, rng.Intn(40))),
			Int64(0),
			Bytes(nil),
		}
		encoded := encodeValues(nil, vals)

		out := []Value{{Type: TypeInt64}, {Type: TypeBytes}, {Type: TypeInt64}, {Type: TypeBytes}}
		if err := decodeValues(encoded, out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for i := range vals {
			if !vals[i].Equal(out[i]) {
				t.Fatalf("column %d: encoded %v, decoded %v", i, vals[i], out[i])
			}
		}
	}
}

// randomBytes produces values including the bytes the escaping has to handle.
func randomBytes(rng *rand.Rand, n int) []byte {
	alphabet := []byte{0x00, 0x01, 0x02, 'a', 'b', 0xfe, 0xff}
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return out
}

// ---------- range queries and indexes ----------

// reference is a linear-search model of the table: obviously correct, and the
// answer every query is checked against.
type reference struct {
	rows map[string]person
}

func newReference() *reference { return &reference{rows: map[string]person{}} }

func (r *reference) key(p person) string { return fmt.Sprintf("%s\x00%d", p.country, p.id) }

func (r *reference) put(p person)    { r.rows[r.key(p)] = p }
func (r *reference) remove(p person) { delete(r.rows, r.key(p)) }

// byPrimaryKey returns the rows sorted the way the primary index stores them.
func (r *reference) byPrimaryKey() []person {
	out := make([]person, 0, len(r.rows))
	for _, p := range r.rows {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].country != out[j].country {
			return out[i].country < out[j].country
		}
		return out[i].id < out[j].id
	})
	return out
}

// byAge returns the rows sorted the way the (age) index stores them: by age,
// then by primary key, since that is what the index appends.
func (r *reference) byAge() []person {
	out := r.byPrimaryKey()
	sort.SliceStable(out, func(i, j int) bool { return out[i].age < out[j].age })
	return out
}

func collectScan(t *testing.T, tx *DBTX, req *Scanner) []person {
	t.Helper()
	var got []person
	for ; req.Valid(); req.Next() {
		var rec Record
		if err := req.Deref(&rec); err != nil {
			t.Fatalf("Deref: %v", err)
		}
		got = append(got, person{
			country: string(rec.Get("country").Str),
			id:      rec.Get("id").I64,
			name:    string(rec.Get("name").Str),
			age:     rec.Get("age").I64,
		})
	}
	return got
}

func requireSamePeople(t *testing.T, got, want []person, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: scan returned %d rows, want %d\n got: %v\nwant: %v",
			context, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d is %v, want %v", context, i, got[i], want[i])
		}
	}
}

func TestFullTableScan(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	ref := newReference()

	for i := 0; i < 300; i++ {
		p := person{
			country: []string{"uk", "in", "us", "de"}[i%4],
			id:      int64(i),
			name:    fmt.Sprintf("person%03d", i),
			age:     int64(20 + i%50),
		}
		upsert(t, db, p)
		ref.put(p)
	}

	if err := db.View(func(tx *DBTX) error {
		req := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		requireSamePeople(t, collectScan(t, tx, &req), ref.byPrimaryKey(), "full scan")
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestRangeScanOnPrimaryKey checks the partial-key case: the query constrains
// only the first primary key column, so the second has to be encoded as an
// infinity in the right direction.
func TestRangeScanOnPrimaryKey(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	ref := newReference()

	for i := 0; i < 200; i++ {
		p := person{
			country: []string{"de", "in", "uk", "us"}[i%4],
			id:      int64(i),
			name:    fmt.Sprintf("p%03d", i),
			age:     int64(20 + i%50),
		}
		upsert(t, db, p)
		ref.put(p)
	}

	if err := db.View(func(tx *DBTX) error {
		// Everything in "uk", using only the leading primary key column.
		req := Scanner{
			Cmp1: btree.CmpGE, Key1: *(&Record{}).AddStr("country", []byte("uk")),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddStr("country", []byte("uk")),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		var want []person
		for _, p := range ref.byPrimaryKey() {
			if p.country == "uk" {
				want = append(want, p)
			}
		}
		requireSamePeople(t, collectScan(t, tx, &req), want, "country = uk")

		// A strict lower bound must exclude the whole of "in".
		req = Scanner{
			Cmp1: btree.CmpGT, Key1: *(&Record{}).AddStr("country", []byte("in")),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddStr("country", []byte("uk")),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		want = nil
		for _, p := range ref.byPrimaryKey() {
			if p.country > "in" && p.country <= "uk" {
				want = append(want, p)
			}
		}
		requireSamePeople(t, collectScan(t, tx, &req), want, "in < country <= uk")
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestBackwardScan(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	ref := newReference()
	for i := 0; i < 100; i++ {
		p := person{"uk", int64(i), fmt.Sprintf("p%03d", i), int64(20 + i)}
		upsert(t, db, p)
		ref.put(p)
	}

	if err := db.View(func(tx *DBTX) error {
		req := Scanner{
			Cmp1: btree.CmpLE, Key1: *(&Record{}).AddStr("country", []byte("uk")),
			Cmp2: btree.CmpGE, Key2: *(&Record{}).AddStr("country", []byte("uk")),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		got := collectScan(t, tx, &req)
		want := ref.byPrimaryKey()
		// Reverse the reference.
		for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
			want[i], want[j] = want[j], want[i]
		}
		requireSamePeople(t, got, want, "descending scan")
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestSecondaryIndexScan checks that a query on an indexed column returns rows
// in that index's order, having followed each entry back to the full row.
func TestSecondaryIndexScan(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	ref := newReference()

	for i := 0; i < 200; i++ {
		p := person{
			country: []string{"de", "uk"}[i%2],
			id:      int64(i),
			name:    fmt.Sprintf("p%03d", i),
			age:     int64(20 + i%40),
		}
		upsert(t, db, p)
		ref.put(p)
	}

	if err := db.View(func(tx *DBTX) error {
		req := Scanner{
			Cmp1: btree.CmpGE, Key1: *(&Record{}).AddInt64("age", 30),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddInt64("age", 39),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		if req.index == 0 {
			t.Fatal("a query on the indexed column fell back to the primary key")
		}
		var want []person
		for _, p := range ref.byAge() {
			if p.age >= 30 && p.age <= 39 {
				want = append(want, p)
			}
		}
		requireSamePeople(t, collectScan(t, tx, &req), want, "30 <= age <= 39")
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestScanRejectsUnindexedColumn(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	err := db.View(func(tx *DBTX) error {
		// "id" is the *second* primary key column, so no index is sorted by
		// it. Real SQL would fall back to a full scan with a filter; here the
		// choice is explicit, so this must be reported rather than silently
		// turning into a table scan.
		req := Scanner{
			Cmp1: btree.CmpGE, Key1: *(&Record{}).AddInt64("id", 1),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddInt64("id", 5),
		}
		return tx.Scan("people", &req)
	})
	if err == nil {
		t.Fatal("scanning by a column no index leads with was accepted")
	}
}

// TestIndexesStayConsistent is the differential test for index maintenance:
// after arbitrary inserts, updates and deletes, every index must agree with
// the primary data.
func TestIndexesStayConsistent(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	ref := newReference()
	rng := rand.New(rand.NewSource(9))

	countries := []string{"de", "in", "uk", "us"}
	for op := 0; op < 1500; op++ {
		p := person{
			country: countries[rng.Intn(len(countries))],
			id:      int64(rng.Intn(60)),
			name:    fmt.Sprintf("name%02d", rng.Intn(30)),
			age:     int64(rng.Intn(80)),
		}
		if rng.Intn(4) == 0 {
			if err := db.Update(func(tx *DBTX) error {
				_, err := tx.Delete("people", p.record())
				return err
			}); err != nil {
				t.Fatalf("delete: %v", err)
			}
			ref.remove(p)
		} else {
			upsert(t, db, p)
			ref.put(p)
		}

		if op%97 != 0 {
			continue
		}
		if err := db.View(func(tx *DBTX) error {
			// The primary data matches the reference.
			req := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
			if err := tx.Scan("people", &req); err != nil {
				return err
			}
			requireSamePeople(t, collectScan(t, tx, &req), ref.byPrimaryKey(), "primary data")

			// And so does every secondary index: walking it must return
			// exactly the same rows, in that index's order.
			ageScan := Scanner{
				Cmp1: btree.CmpGE, Key1: *(&Record{}).AddInt64("age", math.MinInt64),
				Cmp2: btree.CmpLE, Key2: *(&Record{}).AddInt64("age", math.MaxInt64),
			}
			if err := tx.Scan("people", &ageScan); err != nil {
				return err
			}
			requireSamePeople(t, collectScan(t, tx, &ageScan), ref.byAge(), "age index")
			return nil
		}); err != nil {
			t.Fatalf("view at op %d: %v", op, err)
		}
	}
}

// TestIndexEntriesRemovedOnUpdate is the specific failure index maintenance
// exists to prevent: changing an indexed column must retire the old entry, not
// merely add a new one.
func TestIndexEntriesRemovedOnUpdate(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())

	upsert(t, db, person{"uk", 1, "alice", 30})
	upsert(t, db, person{"uk", 1, "alice", 50}) // same row, different age

	if err := db.View(func(tx *DBTX) error {
		// The old age must no longer be indexed.
		req := Scanner{
			Cmp1: btree.CmpGE, Key1: *(&Record{}).AddInt64("age", 30),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddInt64("age", 30),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		if got := collectScan(t, tx, &req); len(got) != 0 {
			t.Fatalf("the stale index entry for age 30 is still there: %v", got)
		}

		req = Scanner{
			Cmp1: btree.CmpGE, Key1: *(&Record{}).AddInt64("age", 50),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddInt64("age", 50),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		got := collectScan(t, tx, &req)
		if len(got) != 1 || got[0].age != 50 {
			t.Fatalf("the new index entry for age 50 is missing: %v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestRowAndIndexesCommitTogether is the reason the relational layer sits on
// transactions at all.
func TestRowAndIndexesCommitTogether(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	upsert(t, db, person{"uk", 1, "alice", 30})

	// Abort after changing an indexed column.
	var tx DBTX
	db.Begin(&tx)
	if _, err := tx.Upsert("people", person{"uk", 1, "alice", 77}.record()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	db.Abort(&tx)

	if err := db.View(func(tx *DBTX) error {
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		if _, err := tx.Get("people", rec); err != nil {
			return err
		}
		if got := rec.Get("age").I64; got != 30 {
			t.Fatalf("after abort the row's age is %d, want 30", got)
		}
		// The index must have been rolled back too — this is the half that a
		// non-transactional implementation gets wrong.
		req := Scanner{
			Cmp1: btree.CmpGE, Key1: *(&Record{}).AddInt64("age", 77),
			Cmp2: btree.CmpLE, Key2: *(&Record{}).AddInt64("age", 77),
		}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		if got := collectScan(t, tx, &req); len(got) != 0 {
			t.Fatalf("an aborted transaction left an index entry behind: %v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestTableDrop(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	for i := 0; i < 50; i++ {
		upsert(t, db, person{"uk", int64(i), fmt.Sprintf("p%d", i), int64(20 + i)})
	}
	if err := db.Update(func(tx *DBTX) error { return tx.TableDrop("people") }); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := db.View(func(tx *DBTX) error {
		names, err := tx.TableList()
		if err != nil {
			return err
		}
		if len(names) != 0 {
			t.Fatalf("after dropping, the tables are %v", names)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestConflictRetryInUpdate(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	upsert(t, db, person{"uk", 1, "alice", 30})

	// A transaction that conflicts once should be retried transparently.
	attempts := 0
	err := db.Update(func(tx *DBTX) error {
		attempts++
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		if _, err := tx.Get("people", rec); err != nil {
			return err
		}
		if attempts == 1 {
			// Slip a conflicting commit in from outside on the first attempt.
			upsert(t, db, person{"uk", 1, "alice", 44})
		}
		_, err := tx.Upsert("people", person{"uk", 1, "alice", rec.Get("age").I64 + 1}.record())
		return err
	})
	if err != nil {
		t.Fatalf("update with retry: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("the conflicting transaction was not retried (attempts=%d)", attempts)
	}
	if err := db.View(func(tx *DBTX) error {
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		if _, err := tx.Get("people", rec); err != nil {
			return err
		}
		if got := rec.Get("age").I64; got != 45 {
			t.Fatalf("age is %d, want 45 — the retry should have read the newer value", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	_ = kv.ErrConflict
}

// TestDerefReturnsIndependentMemory pins an invariant other layers rely on.
//
// A scan reads values out of pages, and those pages get rewritten as soon as
// anything is updated. Deref therefore has to hand back memory that does not
// alias the tree's storage — which it does because decoding a string always
// allocates. That is easy to lose in a future optimization that returns a
// sub-slice of the page instead, so the property is asserted here rather than
// left as a comment.
func TestDerefReturnsIndependentMemory(t *testing.T) {
	db, _ := openTemp(t)
	createTable(t, db, peopleTable())
	for i := 0; i < 500; i++ {
		upsert(t, db, person{"uk", int64(i), fmt.Sprintf("name%04d", i), int64(i)})
	}

	// Read a row, then churn the database hard enough to reuse its pages.
	var captured Record
	if err := db.View(func(tx *DBTX) error {
		req := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
		if err := tx.Scan("people", &req); err != nil {
			return err
		}
		if !req.Valid() {
			t.Fatal("the scan returned nothing")
		}
		return req.Deref(&captured)
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	wantName := string(captured.Get("name").Str)

	for round := 0; round < 20; round++ {
		for i := 0; i < 500; i++ {
			upsert(t, db, person{"uk", int64(i), fmt.Sprintf("churn%d-%04d", round, i), int64(i)})
		}
	}

	if got := string(captured.Get("name").Str); got != wantName {
		t.Fatalf("a value read before the churn now reads %q, want %q: "+
			"Deref returned memory that aliases the tree's pages", got, wantName)
	}
}

// TestSchemaCacheDoesNotLeakUncommittedTables checks the correctness condition
// the per-transaction schema cache depends on.
//
// A transaction must see a table it just created, and a transaction whose
// CREATE was rolled back must leave no trace — neither on disk nor in a cache
// that outlives it.
func TestSchemaCacheDoesNotLeakUncommittedTables(t *testing.T) {
	db, _ := openTemp(t)

	// A table created inside a transaction is visible to the rest of it.
	var tx DBTX
	db.Begin(&tx)
	if err := tx.TableNew(peopleTable()); err != nil {
		t.Fatalf("TableNew: %v", err)
	}
	if _, err := tx.Insert("people", person{"uk", 1, "alice", 30}.record()); err != nil {
		t.Fatalf("inserting into a table created in the same transaction: %v", err)
	}
	db.Abort(&tx)

	// After the abort, the table is gone.
	if err := db.View(func(tx *DBTX) error {
		if _, err := tx.TableDef("people"); !errors.Is(err, ErrTableNotFound) {
			t.Fatalf("after aborting the CREATE, TableDef returned %v, want ErrTableNotFound", err)
		}
		names, err := tx.TableList()
		if err != nil {
			return err
		}
		if len(names) != 0 {
			t.Fatalf("an aborted CREATE left tables behind: %v", names)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}

	// Creating it for real afterwards still works — the negative cache entry
	// from the earlier lookup must not outlive its transaction.
	createTable(t, db, peopleTable())
	upsert(t, db, person{"uk", 1, "alice", 30})
	if err := db.View(func(tx *DBTX) error {
		rec := (&Record{}).AddStr("country", []byte("uk")).AddInt64("id", 1)
		found, err := tx.Get("people", rec)
		if err != nil || !found {
			t.Fatalf("the row is missing after recreating the table: found=%v err=%v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}

	// A dropped table disappears from the transaction that dropped it.
	if err := db.Update(func(tx *DBTX) error {
		if err := tx.TableDrop("people"); err != nil {
			return err
		}
		if _, err := tx.TableDef("people"); !errors.Is(err, ErrTableNotFound) {
			t.Fatalf("after DROP the same transaction still sees the table: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("drop: %v", err)
	}
}

func TestFloat64Encoding(t *testing.T) {
	floats := []float64{-999.99, -100.5, -1.0, -0.001, 0.0, 0.001, 1.0, 42.5, 999.99}
	encoded := make([][]byte, len(floats))
	for i, f := range floats {
		encoded[i] = encodeValues(nil, []Value{Float64(f)})
	}
	for i := 0; i < len(floats)-1; i++ {
		cmp := bytes.Compare(encoded[i], encoded[i+1])
		if cmp >= 0 {
			t.Fatalf("float encoding order failed: %g vs %g returned %d", floats[i], floats[i+1], cmp)
		}
	}
	for i, f := range floats {
		out := []Value{{Type: TypeFloat64}}
		if err := decodeValues(encoded[i], out); err != nil {
			t.Fatalf("decode float %g failed: %v", f, err)
		}
		if out[0].F64 != f {
			t.Fatalf("decoded float %g got %g", f, out[0].F64)
		}
	}
}

func TestNullEncodingAndValidation(t *testing.T) {
	n := Null()
	if n.Type != TypeNull || n.String() != "NULL" {
		t.Fatalf("Null() returned %v", n)
	}

	// Roundtrip
	vals := []Value{Int64(10), Null(), Bytes([]byte("test")), Null()}
	enc := encodeValues(nil, vals)
	out := []Value{{Type: TypeInt64}, {Type: TypeNull}, {Type: TypeBytes}, {Type: TypeNull}}
	if err := decodeValues(enc, out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range vals {
		if !vals[i].Equal(out[i]) {
			t.Fatalf("column %d: want %v, got %v", i, vals[i], out[i])
		}
	}

	// Schema validation
	tdef := &TableDef{
		Name:  "items",
		Cols:  []string{"id", "val"},
		Types: []uint32{TypeInt64, TypeBytes},
		Indexes: [][]string{{"id"}},
	}
	recValid := Record{Cols: []string{"id", "val"}, Vals: []Value{Int64(1), Null()}}
	if _, err := checkRecord(tdef, recValid, len(tdef.Cols)); err != nil {
		t.Fatalf("valid record with NULL non-pk rejected: %v", err)
	}

	recInvalid := Record{Cols: []string{"id", "val"}, Vals: []Value{Null(), Bytes([]byte("x"))}}
	if _, err := checkRecord(tdef, recInvalid, len(tdef.Cols)); err == nil {
		t.Fatal("record with NULL primary key accepted, expected error")
	}
}

