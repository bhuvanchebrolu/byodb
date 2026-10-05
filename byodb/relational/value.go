// Package relational puts tables, rows and indexes on top of the key-value
// store.
//
// Nothing here is fundamentally new: a row is a key-value pair, an index is a
// second key-value pair pointing back at the first, and a schema is a row in a
// table the database keeps for itself. The work is in the encoding — arranging
// bytes so that sorting them sorts the data they represent.
package relational

import (
	"errors"
	"fmt"
	"slices"
)

// Column types. Two is enough to show what a type system costs a storage
// engine; more would be more of the same.
const (
	TypeError   = 0 // the zero value, meaning "unset"
	TypeBytes   = 1 // a string of arbitrary bytes
	TypeInt64   = 2 // a signed 64-bit integer
	TypeFloat64 = 3 // a IEEE 754 64-bit floating point number
	TypeNull    = 4 // a SQL NULL value
)

// TypeName renders a column type for error messages and schema output.
func TypeName(t uint32) string {
	switch t {
	case TypeBytes:
		return "bytes"
	case TypeInt64:
		return "int64"
	case TypeFloat64:
		return "float64"
	case TypeNull:
		return "null"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}

// Value is one table cell: a tagged union over the supported types.
type Value struct {
	Type uint32
	I64  int64
	F64  float64
	Str  []byte
}

// Bytes, Int64, Float64, and Null construct values of each type.
func Bytes(v []byte) Value    { return Value{Type: TypeBytes, Str: v} }
func Int64(v int64) Value     { return Value{Type: TypeInt64, I64: v} }
func Float64(v float64) Value { return Value{Type: TypeFloat64, F64: v} }
func Null() Value             { return Value{Type: TypeNull} }

// String renders a value for display.
func (v Value) String() string {
	switch v.Type {
	case TypeBytes:
		return string(v.Str)
	case TypeInt64:
		return fmt.Sprintf("%d", v.I64)
	case TypeFloat64:
		return fmt.Sprintf("%g", v.F64)
	case TypeNull:
		return "NULL"
	default:
		return "NULL"
	}
}

// Equal compares two values, including their types.
func (v Value) Equal(other Value) bool {
	if v.Type != other.Type {
		return false
	}
	if v.Type == TypeNull {
		return true
	}
	if v.Type == TypeInt64 {
		return v.I64 == other.I64
	}
	if v.Type == TypeFloat64 {
		return v.F64 == other.F64
	}
	return string(v.Str) == string(other.Str)
}

// Record is a row: parallel lists of column names and values.
//
// Names travel with the values because a record is also how callers describe
// *partial* rows — a primary key on its own, or the columns a query filters
// by — and those need naming to be interpreted against a schema.
type Record struct {
	Cols []string
	Vals []Value
}

func (rec *Record) AddStr(col string, val []byte) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Bytes(val))
	return rec
}

func (rec *Record) AddInt64(col string, val int64) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Int64(val))
	return rec
}

func (rec *Record) AddFloat64(col string, val float64) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Float64(val))
	return rec
}

func (rec *Record) AddNull(col string) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Null())
	return rec
}

func (rec *Record) Add(col string, val Value) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, val)
	return rec
}

// Get returns a pointer to the named column's value, or nil if the record does
// not carry that column.
func (rec *Record) Get(col string) *Value {
	for i, name := range rec.Cols {
		if name == col {
			return &rec.Vals[i]
		}
	}
	return nil
}

// ErrNotFound and friends are the errors callers are expected to distinguish.
var (
	ErrTableNotFound = errors.New("relational: table not found")
	ErrTableExists   = errors.New("relational: table already exists")
	ErrBadSchema     = errors.New("relational: invalid table definition")
	ErrBadRecord     = errors.New("relational: record does not match the schema")
)

// checkRecord reorders a record into schema order and verifies its types.
//
// n says how much of the row must be present: the primary key columns alone
// (for a lookup or delete), or every column (for an insert). Columns beyond n
// may be omitted and come back as zero values.
func checkRecord(tdef *TableDef, rec Record, n int) ([]Value, error) {
	if len(rec.Cols) != len(rec.Vals) {
		return nil, fmt.Errorf("%w: %d column names but %d values",
			ErrBadRecord, len(rec.Cols), len(rec.Vals))
	}

	values := make([]Value, len(tdef.Cols))
	for i, name := range rec.Cols {
		idx := slices.Index(tdef.Cols, name)
		if idx < 0 {
			return nil, fmt.Errorf("%w: table %q has no column %q", ErrBadRecord, tdef.Name, name)
		}
		if values[idx].Type != TypeError {
			return nil, fmt.Errorf("%w: column %q given twice", ErrBadRecord, name)
		}
		if rec.Vals[i].Type != TypeNull && rec.Vals[i].Type != tdef.Types[idx] {
			return nil, fmt.Errorf("%w: column %q is %s, got %s", ErrBadRecord,
				name, TypeName(tdef.Types[idx]), TypeName(rec.Vals[i].Type))
		}
		if idx < tdef.PKeyCount() && rec.Vals[i].Type == TypeNull {
			return nil, fmt.Errorf("%w: primary key column %q cannot be NULL", ErrBadRecord, name)
		}
		values[idx] = rec.Vals[i]
	}

	for i := 0; i < len(values); i++ {
		if values[i].Type == TypeError {
			if i < tdef.PKeyCount() {
				return nil, fmt.Errorf("%w: missing primary key column %q", ErrBadRecord, tdef.Cols[i])
			}
			if i < len(tdef.Defaults) && tdef.Defaults[i].Type != TypeError {
				values[i] = tdef.Defaults[i]
			} else {
				values[i] = Value{Type: TypeNull}
			}
		}
	}
	return values, nil
}
