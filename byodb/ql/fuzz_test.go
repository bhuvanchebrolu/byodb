package ql

import (
	"strings"
	"testing"
)

// FuzzParse checks that the parser only ever returns errors, never panics.
//
// A parser is the part of a database that faces whatever anyone types, so its
// failure mode matters: a syntax error must be reported, and a malformed
// statement must not take the process down with it.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"select a from t",
		"select a, b as c from t index by a > 1 and a < 5 filter b != 'x' limit 2, 3",
		"create table t (a int64, b bytes, index (b), primary key (a))",
		"insert into t (a, b) values (1, 'x'), (2, 'y')",
		"replace into t (a) values (-9223372036854775808)",
		"update t set a = a + 1 index by a = 1",
		"delete from t filter a > 5",
		"drop table t",
		"select (1 + 2) * 3 from t",
		"select (a, b) from t index by (a, b) > (1, 2)",
		"-- just a comment",
		"select 'it''s quoted' from t",
		";",
		"",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		// Very long inputs only slow the fuzzer down; the interesting shapes
		// are short.
		if len(input) > 4096 {
			return
		}
		stmt, err := Parse(input)
		if err != nil {
			// An error must say where the problem is, so it is usable.
			if !strings.Contains(err.Error(), "^") {
				t.Fatalf("the error for %q does not point at a position: %v", input, err)
			}
			return
		}
		if stmt == nil {
			t.Fatalf("Parse(%q) returned no statement and no error", input)
		}
	})
}
