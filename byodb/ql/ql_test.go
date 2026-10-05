package ql

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"byodb/relational"
)

// runner drives statements against a real database and collects results.
type runner struct {
	t  *testing.T
	db *relational.DB
}

func newRunner(t *testing.T) *runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := relational.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &runner{t: t, db: db}
}

// exec runs a statement that changes data and returns the number of rows
// affected.
func (r *runner) exec(query string) int64 {
	r.t.Helper()
	var affected int64
	err := r.db.Update(func(tx *relational.DBTX) error {
		result, err := Run(tx, query)
		if err != nil {
			return err
		}
		affected = result.Affected
		return nil
	})
	if err != nil {
		r.t.Fatalf("%s\n  -> %v", query, err)
	}
	return affected
}

// execErr runs a statement expecting it to fail, returning the error.
func (r *runner) execErr(query string) error {
	r.t.Helper()
	err := r.db.Update(func(tx *relational.DBTX) error {
		_, err := Run(tx, query)
		return err
	})
	if err == nil {
		r.t.Fatalf("expected %q to fail, but it succeeded", query)
	}
	return err
}

// query runs a SELECT and renders each row as a comma-separated string, which
// makes expectations easy to read in the tests.
func (r *runner) query(q string) []string {
	r.t.Helper()
	var out []string
	err := r.db.View(func(tx *relational.DBTX) error {
		result, err := Run(tx, q)
		if err != nil {
			return err
		}
		for ; result.Rows.Valid(); result.Rows.Next() {
			var rec relational.Record
			if err := result.Rows.Deref(&rec); err != nil {
				return err
			}
			parts := make([]string, len(rec.Vals))
			for i, v := range rec.Vals {
				parts[i] = v.String()
			}
			out = append(out, strings.Join(parts, ","))
		}
		return nil
	})
	if err != nil {
		r.t.Fatalf("%s\n  -> %v", q, err)
	}
	return out
}

func requireRows(t *testing.T, got, want []string, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows, want %d\n got: %v\nwant: %v", context, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d is %q, want %q\n got: %v\nwant: %v", context, i, got[i], want[i], got, want)
		}
	}
}

const peopleSchema = `create table people (
    country bytes,
    id int64,
    name bytes,
    age int64,
    index (age),
    index (name, age),
    primary key (country, id)
)`

func seedPeople(r *runner) {
	r.exec(peopleSchema)
	r.exec(`insert into people (country, id, name, age) values
	    ('de', 1, 'anna', 30),
	    ('de', 2, 'bruno', 45),
	    ('in', 1, 'chitra', 22),
	    ('in', 2, 'deepak', 45),
	    ('uk', 1, 'eve', 61),
	    ('uk', 2, 'frank', 30)`)
}

// ---------- parsing ----------

func TestParseErrorsAreLocated(t *testing.T) {
	cases := []string{
		"select",
		"select * from",
		"select a from",
		"select a from t index by",
		"select a from t index by a",
		"insert into t (a) values",
		"insert into t (a) values (1,)",
		"create table t (a int64)",       // no primary key
		"create table t (a int64,",       // unterminated
		"update t set",                   // no assignment
		"select a from t limit",          // no number
		"select 'unterminated from t",    // unterminated string
		"select a from t filter a = 'x'", // fine syntactically, but table is missing
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			if err == nil && !strings.Contains(q, "filter") {
				t.Fatalf("parsing %q should have failed", q)
			}
			if err != nil && !strings.Contains(err.Error(), "^") {
				t.Fatalf("the error for %q does not point at a position:\n%v", q, err)
			}
		})
	}
}

// TestOperatorPrecedence is what the recursive-descent structure exists to get
// right; each case would parse differently under a flat left-to-right reading.
func TestOperatorPrecedence(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (id int64, primary key (id))`)
	r.exec(`insert into t (id) values (1)`)

	cases := map[string]string{
		"1 + 2 * 3":                 "7",
		"(1 + 2) * 3":               "9",
		"2 * 3 + 4 * 5":             "26",
		"10 - 2 - 3":                "5", // left-associative
		"100 / 5 / 2":               "10",
		"1 + 2 = 3":                 "1", // arithmetic binds tighter than comparison
		"1 = 1 and 2 = 2":           "1",
		"1 = 2 or 3 = 3":            "1",
		"1 = 1 and 1 = 2 or 1 = 1":  "1", // AND binds tighter than OR
		"not 1 = 2":                 "1",
		"not 1 = 2 and 1 = 2":       "0", // NOT binds tighter than AND
		"0 - 5":                     "-5",
		"10 % 3":                    "1",
		"2 * -3":                    "-6",
		"'a' + 'b'":                 "ab",
		"1 < 2":                     "1",
		"2 <= 2":                    "1",
		"3 != 4":                    "1",
		"1 = 1 and 2 = 2 and 3 = 3": "1",
		"1 + 2 * 3 - 4 / 2":         "5",
	}
	for expr, want := range cases {
		t.Run(expr, func(t *testing.T) {
			got := r.query(fmt.Sprintf("select %s from t", expr))
			requireRows(t, got, []string{want}, expr)
		})
	}
}

func TestExpressionErrors(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (id int64, name bytes, primary key (id))`)
	r.exec(`insert into t (id, name) values (1, 'x')`)

	for _, q := range []string{
		"select 1 / 0 from t",
		"select 1 % 0 from t",
		"select id + name from t",
		"select -name from t",
		"select id = name from t",
		"select nosuchcolumn from t",
	} {
		t.Run(q, func(t *testing.T) {
			err := r.db.View(func(tx *relational.DBTX) error {
				result, err := Run(tx, q)
				if err != nil {
					return err
				}
				for ; result.Rows.Valid(); result.Rows.Next() {
					var rec relational.Record
					if err := result.Rows.Deref(&rec); err != nil {
						return err
					}
				}
				return nil
			})
			if err == nil {
				t.Fatalf("%q should have failed", q)
			}
		})
	}
}

// TestShortCircuit checks that AND and OR stop early — without which the
// second half of `a != 0 and 100 / a > 2` would divide by zero.
func TestShortCircuit(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (id int64, primary key (id))`)
	r.exec(`insert into t (id) values (0), (5)`)

	got := r.query(`select id from t filter id != 0 and 100 / id > 2`)
	requireRows(t, got, []string{"5"}, "short-circuiting AND")

	got = r.query(`select id from t filter id = 0 or 100 / id > 2`)
	requireRows(t, got, []string{"0", "5"}, "short-circuiting OR")
}

// ---------- statements ----------

func TestSelectProjectionAndAliases(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)

	got := r.query(`select name, age from people index by country = 'de'`)
	requireRows(t, got, []string{"anna,30", "bruno,45"}, "projection")

	got = r.query(`select name as who, age + 1 as next from people index by country = 'uk'`)
	requireRows(t, got, []string{"eve,62", "frank,31"}, "aliases and expressions")

	// The output column names come from AS, or from the column itself.
	err := r.db.View(func(tx *relational.DBTX) error {
		result, err := Run(tx, `select name as who, age + 1 as next from people`)
		if err != nil {
			return err
		}
		if len(result.Names) != 2 || result.Names[0] != "who" || result.Names[1] != "next" {
			t.Fatalf("output names are %v, want [who next]", result.Names)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

func TestIndexByForms(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)

	// Equality on the leading primary key column.
	got := r.query(`select id from people index by country = 'in'`)
	requireRows(t, got, []string{"1", "2"}, "index by country = 'in'")

	// Both primary key columns as a tuple.
	got = r.query(`select country, id from people index by (country, id) = ('uk', 2)`)
	requireRows(t, got, []string{"uk,2"}, "tuple equality")

	// Open-ended range.
	got = r.query(`select country from people index by country > 'de'`)
	requireRows(t, got, []string{"in", "in", "uk", "uk"}, "country > 'de'")

	// Closed range.
	got = r.query(`select country, id from people index by country > 'de' and country < 'uk'`)
	requireRows(t, got, []string{"in,1", "in,2"}, "de < country < uk")

	// Descending: the bounds are written the other way round.
	got = r.query(`select country, id from people index by country < 'uk' and country > 'de'`)
	requireRows(t, got, []string{"in,2", "in,1"}, "descending range")

	// A secondary index.
	got = r.query(`select name from people index by age = 45`)
	requireRows(t, got, []string{"bruno", "deepak"}, "age = 45")

	got = r.query(`select name, age from people index by age > 30 and age < 61`)
	requireRows(t, got, []string{"bruno,45", "deepak,45"}, "30 < age < 61")
}

func TestFullScanWithoutIndexBy(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)
	got := r.query(`select country, id from people`)
	requireRows(t, got,
		[]string{"de,1", "de,2", "in,1", "in,2", "uk,1", "uk,2"},
		"full scan in primary key order")
}

func TestFilterAndLimit(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)

	got := r.query(`select name from people filter age > 40`)
	requireRows(t, got, []string{"bruno", "deepak", "eve"}, "filter")

	got = r.query(`select name from people filter age = 30 or age = 61`)
	requireRows(t, got, []string{"anna", "eve", "frank"}, "compound filter")

	got = r.query(`select name from people limit 2`)
	requireRows(t, got, []string{"anna", "bruno"}, "limit")

	got = r.query(`select name from people limit 2, 3`)
	requireRows(t, got, []string{"chitra", "deepak", "eve"}, "offset and limit")

	// The filter runs before the limit, so the limit counts matching rows.
	got = r.query(`select name from people filter age > 40 limit 2`)
	requireRows(t, got, []string{"bruno", "deepak"}, "filter then limit")

	// INDEX BY, FILTER and LIMIT together.
	got = r.query(`select name from people index by country > 'de' filter age < 50 limit 1, 5`)
	requireRows(t, got, []string{"deepak", "frank"}, "all three clauses")
}

func TestInsertAndReplace(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (id int64, v bytes, primary key (id))`)

	if n := r.exec(`insert into t (id, v) values (1, 'a'), (2, 'b')`); n != 2 {
		t.Fatalf("insert reported %d rows, want 2", n)
	}
	// A duplicate primary key must be refused, not silently overwritten.
	err := r.execErr(`insert into t (id, v) values (1, 'other')`)
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the duplicate-key error is unhelpful: %v", err)
	}
	requireRows(t, r.query(`select v from t index by id = 1`), []string{"a"}, "after refused insert")

	// REPLACE overwrites.
	r.exec(`replace into t (id, v) values (1, 'replaced')`)
	requireRows(t, r.query(`select v from t index by id = 1`), []string{"replaced"}, "after replace")

	// Expressions are allowed as values, as long as they are constant.
	r.exec(`insert into t (id, v) values (3 + 4, 'seven')`)
	requireRows(t, r.query(`select v from t index by id = 7`), []string{"seven"}, "computed value")

	// A column reference has no meaning here: there is no row yet.
	r.execErr(`insert into t (id, v) values (id, 'x')`)
	// And the types must match the schema.
	r.execErr(`insert into t (id, v) values ('not a number', 'x')`)
}

func TestUpdate(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)

	if n := r.exec(`update people set age = age + 1 index by country = 'de'`); n != 2 {
		t.Fatalf("update reported %d rows, want 2", n)
	}
	requireRows(t, r.query(`select name, age from people index by country = 'de'`),
		[]string{"anna,31", "bruno,46"}, "after update")

	// The secondary index must have followed the change.
	requireRows(t, r.query(`select name from people index by age = 46`),
		[]string{"bruno"}, "index updated")
	requireRows(t, r.query(`select name from people index by age = 45`),
		[]string{"deepak"}, "stale index entry removed")

	// Updating with a filter.
	r.exec(`update people set name = name + '!' filter age > 60`)
	requireRows(t, r.query(`select name from people index by country = 'uk'`),
		[]string{"eve!", "frank"}, "update with filter")

	// The primary key cannot be updated in place.
	err := r.execErr(`update people set id = 99 index by country = 'de'`)
	if !strings.Contains(err.Error(), "primary key") {
		t.Fatalf("the error should explain the primary key restriction: %v", err)
	}
	// Type mismatches are caught.
	r.execErr(`update people set age = 'old' index by country = 'de'`)
}

func TestDelete(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)

	if n := r.exec(`delete from people index by country = 'in'`); n != 2 {
		t.Fatalf("delete reported %d rows, want 2", n)
	}
	requireRows(t, r.query(`select country, id from people`),
		[]string{"de,1", "de,2", "uk,1", "uk,2"}, "after delete")

	// Index entries for the deleted rows must be gone.
	requireRows(t, r.query(`select name from people index by age = 22`), nil, "index entry removed")

	r.exec(`delete from people filter age > 40`)
	requireRows(t, r.query(`select name from people`), []string{"anna", "frank"}, "delete with filter")

	r.exec(`delete from people`)
	requireRows(t, r.query(`select name from people`), nil, "delete everything")
}

func TestCreateAndDropTable(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (a int64, b bytes, index (b), primary key (a))`)
	r.exec(`insert into t (a, b) values (1, 'x')`)
	requireRows(t, r.query(`select b from t index by b = 'x'`), []string{"x"}, "secondary index works")

	r.execErr(`create table t (a int64, primary key (a))`) // duplicate
	r.exec(`drop table t`)
	r.execErr(`select a from t`) // gone
}

// ---------- differential test ----------

// TestAgainstReferenceModel runs random queries against both the database and
// a plain Go slice, which is the only way to be confident about the
// interaction of index selection, filtering, limits and ordering.
func TestAgainstReferenceModel(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table items (
	    grp bytes,
	    id int64,
	    score int64,
	    label bytes,
	    index (score),
	    primary key (grp, id)
	)`)

	type item struct {
		grp   string
		id    int64
		score int64
		label string
	}
	var model []item
	rng := rand.New(rand.NewSource(21))
	groups := []string{"a", "b", "c", "d"}

	for i := 0; i < 240; i++ {
		it := item{
			grp:   groups[rng.Intn(len(groups))],
			id:    int64(rng.Intn(60)),
			score: int64(rng.Intn(100)),
			label: fmt.Sprintf("l%02d", rng.Intn(20)),
		}
		r.exec(fmt.Sprintf(`replace into items (grp, id, score, label) values ('%s', %d, %d, '%s')`,
			it.grp, it.id, it.score, it.label))
		replaced := false
		for j := range model {
			if model[j].grp == it.grp && model[j].id == it.id {
				model[j] = it
				replaced = true
				break
			}
		}
		if !replaced {
			model = append(model, it)
		}
	}

	byPrimaryKey := func() []item {
		out := append([]item(nil), model...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].grp != out[j].grp {
				return out[i].grp < out[j].grp
			}
			return out[i].id < out[j].id
		})
		return out
	}
	byScore := func() []item {
		out := byPrimaryKey()
		sort.SliceStable(out, func(i, j int) bool { return out[i].score < out[j].score })
		return out
	}

	// Full scan.
	var want []string
	for _, it := range byPrimaryKey() {
		want = append(want, fmt.Sprintf("%s,%d", it.grp, it.id))
	}
	requireRows(t, r.query(`select grp, id from items`), want, "full scan")

	// Every group, by equality on the leading primary key column.
	for _, g := range groups {
		want = nil
		for _, it := range byPrimaryKey() {
			if it.grp == g {
				want = append(want, fmt.Sprintf("%d", it.id))
			}
		}
		requireRows(t, r.query(fmt.Sprintf(`select id from items index by grp = '%s'`, g)),
			want, "grp = "+g)
	}

	// Random score ranges on the secondary index, with and without a filter.
	for trial := 0; trial < 60; trial++ {
		lo := int64(rng.Intn(100))
		hi := lo + int64(rng.Intn(40))
		threshold := int64(rng.Intn(60))

		want = nil
		for _, it := range byScore() {
			if it.score >= lo && it.score <= hi && it.id > threshold {
				want = append(want, fmt.Sprintf("%s,%d,%d", it.grp, it.id, it.score))
			}
		}
		query := fmt.Sprintf(
			`select grp, id, score from items index by score >= %d and score <= %d filter id > %d`,
			lo, hi, threshold)
		requireRows(t, r.query(query), want, query)
	}

	// Limits and offsets against the same model.
	for trial := 0; trial < 30; trial++ {
		offset := int64(rng.Intn(20))
		limit := int64(rng.Intn(15))
		all := byPrimaryKey()

		want = nil
		for i := offset; i < int64(len(all)) && i < offset+limit; i++ {
			want = append(want, fmt.Sprintf("%s,%d", all[i].grp, all[i].id))
		}
		query := fmt.Sprintf(`select grp, id from items limit %d, %d`, offset, limit)
		requireRows(t, r.query(query), want, query)
	}
}

// TestStatementAtomicity checks that a statement failing part-way leaves
// nothing behind, which matters most for multi-row inserts.
func TestStatementAtomicity(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (id int64, v bytes, index (v), primary key (id))`)
	r.exec(`insert into t (id, v) values (1, 'a')`)

	// The third row duplicates an existing key, so the whole statement fails.
	r.execErr(`insert into t (id, v) values (2, 'b'), (3, 'c'), (1, 'clash')`)

	requireRows(t, r.query(`select id from t`), []string{"1"},
		"a failed multi-row insert left rows behind")
	requireRows(t, r.query(`select v from t index by v = 'b'`), nil,
		"a failed multi-row insert left index entries behind")
}

// TestOpenEndedDescendingRange covers `index by a < v` with no lower bound.
//
// An open range has to invent its far end, and which end depends on the
// direction: an ascending scan runs to +infinity, a descending one to
// -infinity. Getting that backwards yields an empty result for descending
// queries — which a test that always supplies both bounds never notices.
func TestOpenEndedDescendingRange(t *testing.T) {
	r := newRunner(t)
	seedPeople(r)

	got := r.query(`select country, id from people index by country < 'uk'`)
	requireRows(t, got, []string{"in,2", "in,1", "de,2", "de,1"},
		"open-ended descending range")

	got = r.query(`select country, id from people index by country <= 'in'`)
	requireRows(t, got, []string{"in,2", "in,1", "de,2", "de,1"},
		"open-ended descending range, inclusive")

	// The ascending counterpart, for symmetry.
	got = r.query(`select country, id from people index by country >= 'in'`)
	requireRows(t, got, []string{"in,1", "in,2", "uk,1", "uk,2"},
		"open-ended ascending range")

	// On a secondary index too.
	got = r.query(`select name from people index by age < 30`)
	requireRows(t, got, []string{"chitra"}, "descending range on a secondary index")
}

// TestUpdateAssignmentsSeeTheOldRow pins the semantics of multiple assignments
// in one statement: every expression is evaluated against the row as it was
// before the statement, so assignments cannot observe each other.
//
// Without that, `set a = b, b = a` would set both columns to the same value
// instead of swapping them, and the result would depend on assignment order.
func TestUpdateAssignmentsSeeTheOldRow(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table t (id int64, a int64, b int64, primary key (id))`)
	r.exec(`insert into t (id, a, b) values (1, 10, 20)`)

	r.exec(`update t set a = b, b = a index by id = 1`)
	requireRows(t, r.query(`select a, b from t index by id = 1`), []string{"20,10"},
		"the two assignments should have swapped the columns")

	// The same point with arithmetic.
	r.exec(`replace into t (id, a, b) values (2, 5, 0)`)
	r.exec(`update t set a = a + 1, b = a + 100 index by id = 2`)
	requireRows(t, r.query(`select a, b from t index by id = 2`), []string{"6,105"},
		"b should be computed from the old a, not the newly assigned one")
}

// TestUpdateAndDeleteOverManyRows makes the write phase churn enough pages
// that any row still pointing into the tree's storage would read corrupted
// data.
//
// A scan hands back values that point into pages. Those pages are reused as
// the updates proceed, so the rows have to be copied out before the first
// write — at small scale nothing moves and the bug stays invisible.
func TestUpdateAndDeleteOverManyRows(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table big (id int64, tag bytes, n int64, index (n), primary key (id))`)

	const rows = 3000
	err := r.db.Update(func(tx *relational.DBTX) error {
		for i := 0; i < rows; i++ {
			rec := relational.Record{}
			rec.AddInt64("id", int64(i)).
				AddStr("tag", []byte(fmt.Sprintf("tag-%06d", i))).
				AddInt64("n", int64(i))
			if _, err := tx.Insert("big", rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Update every row, deriving the new value from the old one. If the rows
	// were not copied out first, the derived values come from pages that have
	// since been overwritten.
	if n := r.exec(`update big set n = n + 1000000`); n != rows {
		t.Fatalf("update affected %d rows, want %d", n, rows)
	}

	var got []string
	err = r.db.View(func(tx *relational.DBTX) error {
		result, err := Run(tx, `select id, tag, n from big`)
		if err != nil {
			return err
		}
		for ; result.Rows.Valid(); result.Rows.Next() {
			var rec relational.Record
			if err := result.Rows.Deref(&rec); err != nil {
				return err
			}
			id := rec.Get("id").I64
			wantTag := fmt.Sprintf("tag-%06d", id)
			if tag := string(rec.Get("tag").Str); tag != wantTag {
				t.Fatalf("row %d has tag %q, want %q — a stale page reference", id, tag, wantTag)
			}
			if n := rec.Get("n").I64; n != id+1000000 {
				t.Fatalf("row %d has n=%d, want %d — the update read a stale value",
					id, n, id+1000000)
			}
			got = append(got, "ok")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(got) != rows {
		t.Fatalf("after the update the table holds %d rows, want %d", len(got), rows)
	}

	// The secondary index must have followed every one of those changes.
	shifted := r.query(`select id from big index by n = 1000042`)
	requireRows(t, shifted, []string{"42"}, "index entry after a bulk update")
	stale := r.query(`select id from big index by n = 42`)
	requireRows(t, stale, nil, "stale index entry after a bulk update")

	// Now delete most of them, which churns pages the other way.
	deleted := r.exec(`delete from big filter n < 1001500`)
	if deleted != 1500 {
		t.Fatalf("delete affected %d rows, want 1500", deleted)
	}
	remaining := r.query(`select id from big limit 3`)
	requireRows(t, remaining, []string{"1500", "1501", "1502"}, "after the bulk delete")
}

func TestFloat64AndCountStar(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table products (id int64, name bytes, price float64, index (price), primary key (id))`)
	r.exec(`insert into products (id, name, price) values (1, 'apple', 1.50), (2, 'banana', 0.75), (3, 'cherry', 3.25)`)

	cnt := r.query(`select count(*) from products`)
	requireRows(t, cnt, []string{"3"}, "count(*)")

	p := r.query(`select name from products index by price > 1.0`)
	requireRows(t, p, []string{"apple", "cherry"}, "float range scan")
}

func TestTrialUsersSequence(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table trial_users (id int64, name bytes, score float64, role bytes, index (score), index (role), primary key (id))`)
	r.exec(`insert into trial_users (id, name, score, role) values (1, 'Alice', 95.5, 'admin'), (2, 'Bob', 82.0, 'user'), (3, 'Charlie', 67.5, 'user'), (4, 'David', 91.0, 'manager')`)
	r.exec(`replace into trial_users (id, name, score, role) values (3, 'Charlie Updated', 74.0, 'user')`)
	res := r.query(`select name, score from trial_users index by score > 80.0 filter role = 'user'`)
	requireRows(t, res, []string{"Bob,82"}, "score > 80.0 filter role = user")
}

func TestNullSupport(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table items (id int64, val bytes, score int64, primary key (id))`)
	r.exec(`insert into items (id, val, score) values (1, 'apple', 100), (2, null, null), (3, 'cherry', null)`)

	// Select NULL values
	got := r.query(`select id, val from items filter val is null`)
	requireRows(t, got, []string{"2,NULL"}, "is null filter")

	got = r.query(`select id, val from items filter val is not null`)
	requireRows(t, got, []string{"1,apple", "3,cherry"}, "is not null filter")

	got = r.query(`select id, score from items filter score is null`)
	requireRows(t, got, []string{"2,NULL", "3,NULL"}, "score is null filter")

	// NULL in primary key must fail
	r.execErr(`insert into items (id, val) values (null, 'test')`)
}

func TestOrderByClause(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table employees (id int64, dept bytes, salary int64, primary key (id))`)
	r.exec(`insert into employees (id, dept, salary) values (1, 'eng', 100), (2, 'sales', 80), (3, 'eng', 120), (4, 'sales', 90)`)

	got := r.query(`select id, salary from employees order by salary asc`)
	requireRows(t, got, []string{"2,80", "4,90", "1,100", "3,120"}, "order by salary asc")

	got = r.query(`select id, salary from employees order by salary desc`)
	requireRows(t, got, []string{"3,120", "1,100", "4,90", "2,80"}, "order by salary desc")

	got = r.query(`select id, dept, salary from employees order by dept asc, salary desc`)
	requireRows(t, got, []string{"3,eng,120", "1,eng,100", "4,sales,90", "2,sales,80"}, "multi-column order by")
}

func TestJoinClause(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table users (id int64, name bytes, primary key (id))`)
	r.exec(`create table orders (id int64, user_id int64, amount int64, primary key (id))`)

	r.exec(`insert into users (id, name) values (1, 'alice'), (2, 'bob'), (3, 'charlie')`)
	r.exec(`insert into orders (id, user_id, amount) values (10, 1, 50), (20, 1, 150), (30, 2, 200)`)

	got := r.query(`select users.name, orders.amount from users join orders on users.id = orders.user_id order by orders.amount asc`)
	requireRows(t, got, []string{"alice,50", "alice,150", "bob,200"}, "join query with order by")
}

func TestSelectStar(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table users (id int64, name bytes, primary key (id))`)
	r.exec(`insert into users (id, name) values (1, 'alice'), (2, 'bob')`)

	got := r.query(`select * from users order by id asc`)
	requireRows(t, got, []string{"1,alice", "2,bob"}, "select *")

	r.exec(`create table orders (id int64, user_id int64, amount int64, primary key (id))`)
	r.exec(`insert into orders (id, user_id, amount) values (10, 1, 50)`)

	got = r.query(`select users.* from users join orders on users.id = orders.user_id`)
	requireRows(t, got, []string{"1,alice"}, "select table.*")
}

func TestAggregatesAndGroupBy(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table sales (id int64, dept bytes, val float64, primary key (id))`)
	r.exec(`insert into sales (id, dept, val) values (1, 'eng', 100.0), (2, 'eng', 200.0), (3, 'sales', 50.0), (4, 'sales', 150.0)`)

	got := r.query(`select dept, count(val), sum(val), avg(val), min(val), max(val) from sales group by dept order by dept asc`)
	requireRows(t, got, []string{"eng,2,300,150,100,200", "sales,2,200,100,50,150"}, "group by with aggregates")
}

func TestLeftAndRightJoin(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table users (id int64, name bytes, primary key (id))`)
	r.exec(`create table orders (id int64, user_id int64, amount int64, primary key (id))`)

	r.exec(`insert into users (id, name) values (1, 'alice'), (2, 'bob'), (3, 'charlie')`)
	r.exec(`insert into orders (id, user_id, amount) values (10, 1, 50)`)

	got := r.query(`select users.name, orders.amount from users left join orders on users.id = orders.user_id order by users.id asc`)
	requireRows(t, got, []string{"alice,50", "bob,NULL", "charlie,NULL"}, "left join")

	got = r.query(`select users.name, orders.amount from users right join orders on users.id = orders.user_id`)
	requireRows(t, got, []string{"alice,50"}, "right join")
}

func TestAlterTable(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table items (id int64, name bytes, primary key (id))`)
	r.exec(`insert into items (id, name) values (1, 'apple')`)

	r.exec(`alter table items add category bytes`)

	got := r.query(`select id, name, category from items`)
	requireRows(t, got, []string{"1,apple,NULL"}, "alter table add column default NULL")

	r.exec(`insert into items (id, name, category) values (2, 'banana', 'fruit')`)
	got = r.query(`select id, name, category from items index by id = 2`)
	requireRows(t, got, []string{"2,banana,fruit"}, "insert into altered table")
}

func TestTransactionStatements(t *testing.T) {
	r := newRunner(t)
	r.exec(`begin`)
	r.exec(`commit`)
	r.exec(`rollback`)
}

func TestViews(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table employees (id int64, name bytes, salary float64, primary key (id))`)
	r.exec(`insert into employees (id, name, salary) values (1, 'Alice', 75000.0), (2, 'Bob', 48000.0), (3, 'Charlie', 92000.0)`)

	r.exec(`create view high_earners as select id, name, salary from employees filter salary > 50000.0 order by salary desc`)

	got := r.query(`select name, salary from high_earners`)
	requireRows(t, got, []string{"Charlie,92000", "Alice,75000"}, "querying a view")

	r.exec(`drop view high_earners`)
}

func TestLimitOffsetVariants(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table items (id int64, name bytes, primary key (id))`)
	r.exec(`insert into items (id, name) values (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd'), (5, 'e')`)

	got := r.query(`select name from items limit 2 offset 1`)
	requireRows(t, got, []string{"b", "c"}, "limit 2 offset 1")

	got = r.query(`select name from items offset 3 limit 2`)
	requireRows(t, got, []string{"d", "e"}, "offset 3 limit 2")
}

func TestAutoIncrement(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table tickets (id int64 auto_increment, title bytes, primary key (id))`)
	r.exec(`insert into tickets (title) values ('first'), ('second')`)

	got := r.query(`select id, title from tickets order by id asc`)
	requireRows(t, got, []string{"1,first", "2,second"}, "auto increment ids")
}

func TestUniqueConstraint(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table users (id int64, email bytes unique, primary key (id))`)
	r.exec(`insert into users (id, email) values (1, 'a@b.com')`)

	err := r.db.Update(func(tx *relational.DBTX) error {
		_, err := Run(tx, `insert into users (id, email) values (2, 'a@b.com')`)
		return err
	})
	if err == nil {
		t.Fatalf("expected unique constraint violation error on duplicate email")
	}
}

func TestForeignKeyCascade(t *testing.T) {
	r := newRunner(t)
	r.exec(`create table depts (id int64, name bytes, primary key (id))`)
	r.exec(`create table emps (id int64, dept_id int64, name bytes, primary key (id), foreign key (dept_id) references depts (id) on delete cascade)`)

	r.exec(`insert into depts (id, name) values (10, 'Engineering')`)
	r.exec(`insert into emps (id, dept_id, name) values (1, 10, 'Alice')`)

	r.exec(`delete from depts index by id = 10`)

	got := r.query(`select id, name from emps`)
	requireRows(t, got, nil, "on delete cascade should delete child rows")
}


