package relational

import (
	"fmt"
	"slices"

	"byodb/btree"
	"byodb/kv"
)

// Scanner is a range query over a table.
//
// The range is given as two record boundaries with a comparison operator each:
// Key1/Cmp1 is where the scan starts and which way it runs, Key2/Cmp2 is where
// it stops. Both records name the columns they constrain, and those names are
// what selects which index the scan uses.
type Scanner struct {
	Cmp1 int    // btree.CmpGE / CmpGT to scan forward, CmpLE / CmpLT to scan back
	Cmp2 int    // the operator bounding the far end
	Key1 Record // the starting boundary
	Key2 Record // the stopping boundary

	tx     *DBTX
	tdef   *TableDef
	index  int // which of tdef.Indexes is being walked
	iter   *kv.TxIter
	keyEnd []byte
}

// Scan starts a range query.
func (tx *DBTX) Scan(table string, req *Scanner) error {
	tdef, err := tx.TableDef(table)
	if err != nil {
		return err
	}
	return tx.scan(tdef, req)
}

// scanAll walks a whole table in primary key order.
func (tx *DBTX) scanAll(tdef *TableDef, req *Scanner) error {
	req.Cmp1, req.Cmp2 = btree.CmpGE, btree.CmpLE
	req.Key1, req.Key2 = Record{}, Record{}
	return tx.scan(tdef, req)
}

func (tx *DBTX) scan(tdef *TableDef, req *Scanner) error {
	if err := checkScanOperators(req); err != nil {
		return err
	}

	index, err := selectIndex(tdef, req.Key1.Cols, req.Key2.Cols)
	if err != nil {
		return err
	}
	req.tx, req.tdef, req.index = tx, tdef, index

	// The boundary records name only the columns the query constrains, so
	// their values are gathered in index order rather than schema order.
	start, err := boundaryValues(tdef, index, req.Key1)
	if err != nil {
		return err
	}
	stop, err := boundaryValues(tdef, index, req.Key2)
	if err != nil {
		return err
	}

	keyStart := encodeKeyPartial(nil, tdef.Prefixes[index], start, req.Cmp1)
	req.keyEnd = encodeKeyPartial(nil, tdef.Prefixes[index], stop, req.Cmp2)
	req.iter = tx.kv.Seek(keyStart, req.Cmp1)
	return nil
}

// checkScanOperators rejects ranges that do not describe an interval — the two
// operators must point at each other, or the scan either returns nothing or
// runs away from its own end.
func checkScanOperators(req *Scanner) error {
	forward := req.Cmp1 == btree.CmpGE || req.Cmp1 == btree.CmpGT
	backward := req.Cmp1 == btree.CmpLE || req.Cmp1 == btree.CmpLT
	endForward := req.Cmp2 == btree.CmpLE || req.Cmp2 == btree.CmpLT
	endBackward := req.Cmp2 == btree.CmpGE || req.Cmp2 == btree.CmpGT

	switch {
	case forward && endForward, backward && endBackward:
		return nil
	case !forward && !backward:
		return fmt.Errorf("%w: %d is not a range start operator", ErrBadRecord, req.Cmp1)
	case !endForward && !endBackward:
		return fmt.Errorf("%w: %d is not a range end operator", ErrBadRecord, req.Cmp2)
	default:
		return fmt.Errorf("%w: the range starts and ends in opposite directions", ErrBadRecord)
	}
}

// selectIndex picks an index whose leading columns cover both ends of the
// range.
//
// An index on (a, b) can serve a query on (a) — that is just (a, b) with b
// unconstrained — but it cannot serve a query on (b) alone, because b is not
// what the index is sorted by.
//
// The two ends may constrain different columns. An open-ended query such as
// `a > 1` becomes "a > 1 and () <= ()": one end names a column, the other
// names none, and the empty end is covered by every index. A query naming no
// columns at either end scans the whole table in primary key order.
func selectIndex(tdef *TableDef, cols1, cols2 []string) (int, error) {
	covered := func(cols, index []string) bool {
		return len(index) >= len(cols) && slices.Equal(index[:len(cols)], cols)
	}
	if len(cols1) == 0 && len(cols2) == 0 {
		return 0, nil
	}
	for i, index := range tdef.Indexes {
		if covered(cols1, index) && covered(cols2, index) {
			return i, nil
		}
	}
	wanted := cols1
	if len(cols2) > len(wanted) {
		wanted = cols2
	}
	return -1, fmt.Errorf("%w: table %q has no index whose leading columns are %v",
		ErrTableNotFound, tdef.Name, wanted)
}

// boundaryValues extracts the constrained columns in index order and checks
// their types.
func boundaryValues(tdef *TableDef, index int, key Record) ([]Value, error) {
	if len(key.Cols) != len(key.Vals) {
		return nil, fmt.Errorf("%w: %d column names but %d values",
			ErrBadRecord, len(key.Cols), len(key.Vals))
	}
	values := make([]Value, 0, len(key.Cols))
	for i, name := range tdef.Indexes[index] {
		if i >= len(key.Cols) {
			break // the remaining columns are unconstrained
		}
		if key.Cols[i] != name {
			return nil, fmt.Errorf("%w: the range constrains %q where the index has %q",
				ErrBadRecord, key.Cols[i], name)
		}
		want := tdef.Types[columnIndex(tdef, name)]
		if key.Vals[i].Type != want {
			return nil, fmt.Errorf("%w: column %q is %s, got %s",
				ErrBadRecord, name, TypeName(want), TypeName(key.Vals[i].Type))
		}
		values = append(values, key.Vals[i])
	}
	return values, nil
}

// Valid reports whether the scan is still inside its range.
func (sc *Scanner) Valid() bool {
	if sc.iter == nil || !sc.iter.Valid() {
		return false
	}
	key, _ := sc.iter.Deref()
	return btree.CompareOK(key, sc.Cmp2, sc.keyEnd)
}

// Next moves to the following row, in the direction the scan was started with.
func (sc *Scanner) Next() {
	if sc.iter != nil {
		sc.iter.Next()
	}
}

// Deref decodes the current row.
//
// A primary key scan already has the whole row in hand. A secondary index scan
// has only the indexed columns and the primary key, so it follows that primary
// key back to the row — the extra lookup that secondary indexes cost.
func (sc *Scanner) Deref(rec *Record) error {
	if !sc.Valid() {
		return fmt.Errorf("relational: Deref called on a finished scan")
	}
	key, val := sc.iter.Deref()
	tdef := sc.tdef
	n := tdef.PKeyCount()

	if sc.index == 0 {
		values := make([]Value, len(tdef.Cols))
		for i := range values {
			values[i] = Value{Type: tdef.Types[i]}
		}
		if err := decodeValues(key[4:], values[:n]); err != nil {
			return err
		}
		if err := decodeValues(val, values[n:]); err != nil {
			return err
		}
		rec.Cols, rec.Vals = tdef.Cols, values
		return nil
	}

	// Decode the index entry: the indexed columns followed by the primary key.
	indexCols := tdef.Indexes[sc.index]
	entry := make([]Value, 0, len(indexCols)+n)
	for _, name := range indexCols {
		entry = append(entry, Value{Type: tdef.Types[columnIndex(tdef, name)]})
	}
	for _, name := range tdef.Indexes[0] {
		entry = append(entry, Value{Type: tdef.Types[columnIndex(tdef, name)]})
	}
	if err := decodeValues(key[4:], entry); err != nil {
		return err
	}

	lookup := &Record{}
	for i, name := range tdef.Indexes[0] {
		lookup.Add(name, entry[len(indexCols)+i])
	}
	found, err := sc.tx.get(tdef, lookup)
	if err != nil {
		return err
	}
	if !found {
		// The index and the primary data disagree, which transactions are
		// supposed to make impossible.
		return fmt.Errorf("relational: index %v on table %q points at a row that does "+
			"not exist", indexCols, tdef.Name)
	}
	*rec = *lookup
	return nil
}
