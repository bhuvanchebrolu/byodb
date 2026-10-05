// Package assert provides internal invariant checks.
//
// These guard conditions that the code itself must maintain — a failure is a
// bug in the database, never bad input from a caller. Bad input is reported as
// an error; a violated invariant panics, because continuing would corrupt data.
package assert

import "fmt"

// True panics if cond is false.
func True(cond bool, format string, args ...any) {
	if !cond {
		panic("byodb invariant violated: " + fmt.Sprintf(format, args...))
	}
}
