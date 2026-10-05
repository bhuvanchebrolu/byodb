package ql

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"byodb/btree"
	"byodb/kv"
	"byodb/relational"
)

// A query runs as a chain of iterators, each transforming the stream from the
// one below:
//
//	B+tree iterator   raw keys and values
//	  transaction     merged with this transaction's own writes
//	  Scanner         decoded rows, following secondary indexes
//	  scanIter        FILTER, OFFSET and LIMIT applied
//	  selectIter      output expressions evaluated
//
// Nothing materializes the result set. A query over more rows than fit in
// memory streams, and LIMIT stops the work rather than discarding it after the
// fact.

// RecordIter is a stream of rows.
type RecordIter interface {
	Valid() bool
	Next()
	Deref(*relational.Record) error
}

// Result is what running a statement produces: either a stream of rows, or a
// count of rows changed.
type Result struct {
	Rows     RecordIter // non-nil for SELECT
	Names    []string   // output column names, for SELECT
	Affected int64      // rows changed, for the other statements
}

// Execute runs one parsed statement inside a transaction.
func Execute(tx *relational.DBTX, stmt any) (*Result, error) {
	switch s := stmt.(type) {
	case *CreateTable:
		if err := tx.TableNew(s.Def); err != nil {
			if s.IfNotExists && errors.Is(err, relational.ErrTableExists) {
				return &Result{}, nil
			}
			return nil, err
		}
		return &Result{}, nil
	case *DropTable:
		if err := tx.TableDrop(s.Table); err != nil {
			if s.IfExists && errors.Is(err, relational.ErrTableNotFound) {
				return &Result{}, nil
			}
			return nil, err
		}
		return &Result{}, nil
	case *CreateView:
		if err := tx.ViewNew(s.Name, s.Query); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropView:
		if err := tx.ViewDrop(s.Name); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *AlterTable:
		if err := tx.TableAlterAddCol(s.Table, s.Col, s.Type); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *BeginStmt, *CommitStmt, *RollbackStmt:
		return &Result{}, nil
	case *UnionStmt:
		return executeUnion(tx, s)
	case *IntersectStmt:
		return executeIntersect(tx, s)
	case *Select:
		return executeSelect(tx, s)
	case *Insert:
		return executeInsert(tx, s)
	case *Update:
		return executeUpdate(tx, s)
	case *Delete:
		return executeDelete(tx, s)
	default:
		return nil, fmt.Errorf("ql: cannot execute %T", stmt)
	}
}

// Run parses and executes a statement.
func Run(tx *relational.DBTX, query string) (*Result, error) {
	stmt, err := Parse(query)
	if err != nil {
		return nil, err
	}
	return Execute(tx, stmt)
}

// ---------- SELECT ----------

func resolveSubqueries(tx *relational.DBTX, node *Node) error {
	if node == nil || !node.isSet() {
		return nil
	}
	for i := range node.Kids {
		if err := resolveSubqueries(tx, &node.Kids[i]); err != nil {
			return err
		}
	}
	if node.Sub != nil {
		subStmt, ok := node.Sub.(*Select)
		if !ok {
			return fmt.Errorf("invalid subquery statement %T", node.Sub)
		}
		subRes, err := executeSelect(tx, subStmt)
		if err != nil {
			return err
		}
		var litKids []Node
		for subRes.Rows.Valid() {
			var rec relational.Record
			if err := subRes.Rows.Deref(&rec); err != nil {
				return err
			}
			if len(rec.Vals) > 0 {
				val := rec.Vals[0]
				var n Node
				switch val.Type {
				case relational.TypeInt64:
					n = Node{Type: NodeInt, I64: val.I64}
				case relational.TypeFloat64:
					n = Node{Type: NodeFloat, F64: val.F64}
				case relational.TypeBytes:
					n = Node{Type: NodeStr, Str: val.Str}
				case relational.TypeNull:
					n = Node{Type: NodeNull}
				}
				litKids = append(litKids, n)
			}
			subRes.Rows.Next()
		}
		node.Kids = append(node.Kids, litKids...)
		node.Sub = nil
	}
	return nil
}

func executeSelect(tx *relational.DBTX, stmt *Select) (*Result, error) {
	if err := resolveSubqueries(tx, &stmt.Scan.Filter); err != nil {
		return nil, err
	}
	// Check if stmt.Table is a View
	if viewQuery, viewErr := tx.ViewGet(stmt.Table); viewErr == nil {
		viewRes, err := Run(tx, viewQuery)
		if err != nil {
			return nil, fmt.Errorf("error executing view %q: %w", stmt.Table, err)
		}
		// Expand wildcards if needed against view columns
		if len(stmt.Output) == 1 && stmt.Output[0].Type == NodeStar {
			stmt.Output = nil
			stmt.Names = nil
			for _, colName := range viewRes.Names {
				stmt.Output = append(stmt.Output, Node{Type: NodeSym, Str: []byte(colName)})
				stmt.Names = append(stmt.Names, colName)
			}
		}
		var rows RecordIter = viewRes.Rows
		if stmt.Scan.Filter.isSet() {
			var filtered []relational.Record
			for rows.Valid() {
				var rec relational.Record
				if err := rows.Deref(&rec); err != nil {
					return nil, err
				}
				verdict, err := evalExpr(rec, stmt.Scan.Filter)
				if err == nil && truthy(verdict) {
					filtered = append(filtered, rec)
				}
				rows.Next()
			}
			rows = &sliceRecordIter{rows: filtered, pos: 0, valid: len(filtered) > 0}
		}
		if len(stmt.OrderBy) > 0 {
			orderIt, err := applyOrderBy(rows, stmt.OrderBy)
			if err != nil {
				return nil, err
			}
			rows = orderIt
		}
		resIter := RecordIter(&selectIter{input: rows, names: stmt.Names, exprs: stmt.Output})
		if stmt.Distinct {
			distIt, err := applyDistinct(resIter)
			if err != nil {
				return nil, err
			}
			resIter = distIt
		}
		if stmt.Scan.Offset > 0 || stmt.Scan.Limit < math.MaxInt64 {
			resIter, _ = applyLimitOffset(resIter, stmt.Scan.Offset, stmt.Scan.Limit)
		}
		return &Result{
			Rows:  resIter,
			Names: stmt.Names,
		}, nil
	}

	if err := expandWildcards(tx, stmt); err != nil {
		return nil, err
	}

	scanCopy := stmt.Scan
	hasPostProcessing := len(stmt.OrderBy) > 0 || stmt.Distinct || len(stmt.GroupBy) > 0
	if hasPostProcessing {
		scanCopy.Offset = 0
		scanCopy.Limit = math.MaxInt64
	}

	scan, err := newScanIter(tx, &scanCopy)
	if err != nil {
		return nil, err
	}
	var rows RecordIter = scan

	if len(stmt.Scan.Joins) > 0 {
		joinIt, err := newJoinIter(tx, rows, stmt.Table, stmt.Scan.Joins)
		if err != nil {
			return nil, err
		}
		rows = joinIt
	}

	if len(stmt.GroupBy) > 0 || hasAggregates(stmt.Output) {
		groupIt, err := executeGroupBy(rows, stmt.GroupBy, stmt.Output, stmt.Names)
		if err != nil {
			return nil, err
		}
		rows = groupIt
	}

	if len(stmt.OrderBy) > 0 {
		orderIt, err := applyOrderBy(rows, stmt.OrderBy)
		if err != nil {
			return nil, err
		}
		rows = orderIt
	}

	resIter := RecordIter(&selectIter{input: rows, names: stmt.Names, exprs: stmt.Output})
	if len(stmt.GroupBy) > 0 || hasAggregates(stmt.Output) {
		resIter = rows
	}

	if stmt.Distinct {
		distIt, err := applyDistinct(resIter)
		if err != nil {
			return nil, err
		}
		resIter = distIt
	}

	if hasPostProcessing && (stmt.Scan.Offset > 0 || stmt.Scan.Limit < math.MaxInt64) {
		limIt, err := applyLimitOffset(resIter, stmt.Scan.Offset, stmt.Scan.Limit)
		if err != nil {
			return nil, err
		}
		resIter = limIt
	}

	return &Result{
		Rows:  resIter,
		Names: stmt.Names,
	}, nil
}

func applyLimitOffset(input RecordIter, offset, limit int64) (RecordIter, error) {
	var rows []relational.Record
	var matched int64
	for input.Valid() {
		var rec relational.Record
		if err := input.Deref(&rec); err != nil {
			return nil, err
		}
		matched++
		if matched > offset {
			rows = append(rows, rec)
			if limit > 0 && int64(len(rows)) >= limit {
				break
			}
		}
		input.Next()
	}
	return &sliceRecordIter{rows: rows, pos: 0, valid: len(rows) > 0}, nil
}

func applyDistinct(input RecordIter) (RecordIter, error) {
	var rows []relational.Record
	seen := make(map[string]bool)
	for input.Valid() {
		var rec relational.Record
		if err := input.Deref(&rec); err != nil {
			return nil, err
		}
		var keyBuf []byte
		for _, v := range rec.Vals {
			keyBuf = append(keyBuf, []byte(v.String())...)
			keyBuf = append(keyBuf, 0x00)
		}
		k := string(keyBuf)
		if !seen[k] {
			seen[k] = true
			rows = append(rows, rec)
		}
		input.Next()
	}
	return &sliceRecordIter{rows: rows, pos: 0, valid: len(rows) > 0}, nil
}

func executeUnion(tx *relational.DBTX, s *UnionStmt) (*Result, error) {
	resLeft, err := Execute(tx, s.Left)
	if err != nil {
		return nil, err
	}
	resRight, err := Execute(tx, s.Right)
	if err != nil {
		return nil, err
	}

	var rows []relational.Record
	seen := make(map[string]bool)

	collect := func(res *Result) error {
		for res.Rows.Valid() {
			var rec relational.Record
			if err := res.Rows.Deref(&rec); err != nil {
				return err
			}
			var keyBuf []byte
			for _, v := range rec.Vals {
				keyBuf = append(keyBuf, []byte(v.String())...)
				keyBuf = append(keyBuf, 0x00)
			}
			k := string(keyBuf)
			if s.All || !seen[k] {
				seen[k] = true
				rows = append(rows, rec)
			}
			res.Rows.Next()
		}
		return nil
	}

	if err := collect(resLeft); err != nil {
		return nil, err
	}
	if err := collect(resRight); err != nil {
		return nil, err
	}

	return &Result{
		Rows:  &sliceRecordIter{rows: rows, pos: 0, valid: len(rows) > 0},
		Names: resLeft.Names,
	}, nil
}

func executeIntersect(tx *relational.DBTX, s *IntersectStmt) (*Result, error) {
	resLeft, err := Execute(tx, s.Left)
	if err != nil {
		return nil, err
	}
	resRight, err := Execute(tx, s.Right)
	if err != nil {
		return nil, err
	}

	rightSet := make(map[string]bool)
	for resRight.Rows.Valid() {
		var rec relational.Record
		if err := resRight.Rows.Deref(&rec); err != nil {
			return nil, err
		}
		var keyBuf []byte
		for _, v := range rec.Vals {
			keyBuf = append(keyBuf, []byte(v.String())...)
			keyBuf = append(keyBuf, 0x00)
		}
		rightSet[string(keyBuf)] = true
		resRight.Rows.Next()
	}

	var rows []relational.Record
	seen := make(map[string]bool)
	for resLeft.Rows.Valid() {
		var rec relational.Record
		if err := resLeft.Rows.Deref(&rec); err != nil {
			return nil, err
		}
		var keyBuf []byte
		for _, v := range rec.Vals {
			keyBuf = append(keyBuf, []byte(v.String())...)
			keyBuf = append(keyBuf, 0x00)
		}
		k := string(keyBuf)
		if rightSet[k] && !seen[k] {
			seen[k] = true
			rows = append(rows, rec)
		}
		resLeft.Rows.Next()
	}

	return &Result{
		Rows:  &sliceRecordIter{rows: rows, pos: 0, valid: len(rows) > 0},
		Names: resLeft.Names,
	}, nil
}

func expandWildcards(tx *relational.DBTX, stmt *Select) error {
	var newOutput []Node
	var newNames []string

	for i, expr := range stmt.Output {
		switch expr.Type {
		case NodeStar:
			tdef, err := tx.TableDef(stmt.Table)
			if err != nil {
				return err
			}
			for _, col := range tdef.Cols {
				colName := col
				if len(stmt.Scan.Joins) > 0 {
					colName = stmt.Table + "." + col
				}
				newOutput = append(newOutput, Node{Type: NodeSym, Str: []byte(colName)})
				newNames = append(newNames, colName)
			}
			for _, j := range stmt.Scan.Joins {
				jtdef, err := tx.TableDef(j.Table)
				if err != nil {
					return err
				}
				for _, col := range jtdef.Cols {
					colName := j.Table + "." + col
					newOutput = append(newOutput, Node{Type: NodeSym, Str: []byte(colName)})
					newNames = append(newNames, colName)
				}
			}
		case NodeTableStar:
			tbl := string(expr.Str)
			tdef, err := tx.TableDef(tbl)
			if err != nil {
				return err
			}
			for _, col := range tdef.Cols {
				colName := col
				if len(stmt.Scan.Joins) > 0 {
					colName = tbl + "." + col
				}
				newOutput = append(newOutput, Node{Type: NodeSym, Str: []byte(colName)})
				newNames = append(newNames, colName)
			}
		default:
			newOutput = append(newOutput, expr)
			newNames = append(newNames, stmt.Names[i])
		}
	}
	stmt.Output = newOutput
	stmt.Names = newNames
	return nil
}

type singleRowIter struct {
	rec   relational.Record
	valid bool
}

func (it *singleRowIter) Valid() bool { return it.valid }
func (it *singleRowIter) Next()       { it.valid = false }
func (it *singleRowIter) Deref(rec *relational.Record) error {
	*rec = it.rec
	return nil
}

// ---------- JOIN & ORDER BY ----------

type joinIter struct {
	buffered []relational.Record
	pos      int
	valid    bool
}

func newJoinIter(tx *relational.DBTX, left RecordIter, leftTable string, joins []Join) (*joinIter, error) {
	var leftRows []relational.Record
	for left.Valid() {
		var r relational.Record
		if err := left.Deref(&r); err != nil {
			return nil, err
		}
		prefixed := relational.Record{}
		for i, c := range r.Cols {
			prefixed.Add(c, r.Vals[i])
			if !stringsContainsDot(c) {
				prefixed.Add(leftTable+"."+c, r.Vals[i])
			}
		}
		leftRows = append(leftRows, prefixed)
		left.Next()
	}

	for _, j := range joins {
		var rightRows []relational.Record
		var scanner relational.Scanner
		scanner.Cmp1 = btree.CmpGE
		scanner.Cmp2 = btree.CmpLE
		if err := tx.Scan(j.Table, &scanner); err != nil {
			return nil, err
		}
		for scanner.Valid() {
			var rr relational.Record
			if err := scanner.Deref(&rr); err != nil {
				return nil, err
			}
			prefixed := relational.Record{}
			for i, c := range rr.Cols {
				prefixed.Add(c, rr.Vals[i])
				if !stringsContainsDot(c) {
					prefixed.Add(j.Table+"."+c, rr.Vals[i])
				}
			}
			rightRows = append(rightRows, prefixed)
			scanner.Next()
		}

		var joined []relational.Record
		if j.Type == JoinLeft {
			for _, lRow := range leftRows {
				matched := false
				for _, rRow := range rightRows {
					combined := relational.Record{}
					for i, c := range lRow.Cols {
						combined.Add(c, lRow.Vals[i])
					}
					for i, c := range rRow.Cols {
						combined.Add(c, rRow.Vals[i])
					}
					verdict, err := evalExpr(combined, j.On)
					if err == nil && truthy(verdict) {
						joined = append(joined, combined)
						matched = true
					}
				}
				if !matched {
					combined := relational.Record{}
					for i, c := range lRow.Cols {
						combined.Add(c, lRow.Vals[i])
					}
					if len(rightRows) > 0 {
						for _, c := range rightRows[0].Cols {
							combined.Add(c, relational.Null())
						}
					}
					joined = append(joined, combined)
				}
			}
		} else if j.Type == JoinRight {
			for _, rRow := range rightRows {
				matched := false
				for _, lRow := range leftRows {
					combined := relational.Record{}
					for i, c := range lRow.Cols {
						combined.Add(c, lRow.Vals[i])
					}
					for i, c := range rRow.Cols {
						combined.Add(c, rRow.Vals[i])
					}
					verdict, err := evalExpr(combined, j.On)
					if err == nil && truthy(verdict) {
						joined = append(joined, combined)
						matched = true
					}
				}
				if !matched {
					combined := relational.Record{}
					if len(leftRows) > 0 {
						for _, c := range leftRows[0].Cols {
							combined.Add(c, relational.Null())
						}
					}
					for i, c := range rRow.Cols {
						combined.Add(c, rRow.Vals[i])
					}
					joined = append(joined, combined)
				}
			}
		} else {
			for _, lRow := range leftRows {
				for _, rRow := range rightRows {
					combined := relational.Record{}
					for i, c := range lRow.Cols {
						combined.Add(c, lRow.Vals[i])
					}
					for i, c := range rRow.Cols {
						combined.Add(c, rRow.Vals[i])
					}
					verdict, err := evalExpr(combined, j.On)
					if err == nil && truthy(verdict) {
						joined = append(joined, combined)
					}
				}
			}
		}
		leftRows = joined
	}

	it := &joinIter{buffered: leftRows, pos: 0, valid: len(leftRows) > 0}
	return it, nil
}

func stringsContainsDot(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}

func (it *joinIter) Valid() bool { return it.valid }
func (it *joinIter) Next() {
	it.pos++
	if it.pos < len(it.buffered) {
		it.valid = true
	} else {
		it.valid = false
	}
}
func (it *joinIter) Deref(rec *relational.Record) error {
	*rec = it.buffered[it.pos]
	return nil
}

func applyOrderBy(input RecordIter, orderBy []OrderItem) (RecordIter, error) {
	var rows []relational.Record
	for input.Valid() {
		var rec relational.Record
		if err := input.Deref(&rec); err != nil {
			return nil, err
		}
		rows = append(rows, rec)
		input.Next()
	}

	var sortErr error
	sort.SliceStable(rows, func(i, j int) bool {
		if sortErr != nil {
			return false
		}
		for _, item := range orderBy {
			v1, err1 := evalExpr(rows[i], item.Expr)
			v2, err2 := evalExpr(rows[j], item.Expr)
			if err1 != nil {
				sortErr = err1
				return false
			}
			if err2 != nil {
				sortErr = err2
				return false
			}
			cmp := compareValues(v1, v2)
			if cmp != 0 {
				if item.Desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})

	if sortErr != nil {
		return nil, sortErr
	}

	return &sliceRecordIter{rows: rows, pos: 0, valid: len(rows) > 0}, nil
}

func compareValues(v1, v2 relational.Value) int {
	if v1.Type == relational.TypeNull && v2.Type == relational.TypeNull {
		return 0
	}
	if v1.Type == relational.TypeNull {
		return -1
	}
	if v2.Type == relational.TypeNull {
		return 1
	}
	if v1.Type == relational.TypeInt64 && v2.Type == relational.TypeInt64 {
		switch {
		case v1.I64 < v2.I64:
			return -1
		case v1.I64 > v2.I64:
			return 1
		default:
			return 0
		}
	}
	if v1.Type == relational.TypeFloat64 && v2.Type == relational.TypeFloat64 {
		switch {
		case v1.F64 < v2.F64:
			return -1
		case v1.F64 > v2.F64:
			return 1
		default:
			return 0
		}
	}
	s1, s2 := string(v1.Str), string(v2.Str)
	switch {
	case s1 < s2:
		return -1
	case s1 > s2:
		return 1
	default:
		return 0
	}
}

type sliceRecordIter struct {
	rows  []relational.Record
	pos   int
	valid bool
}

func (it *sliceRecordIter) Valid() bool { return it.valid }
func (it *sliceRecordIter) Next() {
	it.pos++
	if it.pos < len(it.rows) {
		it.valid = true
	} else {
		it.valid = false
	}
}
func (it *sliceRecordIter) Deref(rec *relational.Record) error {
	*rec = it.rows[it.pos]
	return nil
}

// selectIter evaluates the output expressions for each row.
type selectIter struct {
	input RecordIter
	names []string
	exprs []Node
}

func (it *selectIter) Valid() bool { return it.input.Valid() }
func (it *selectIter) Next()       { it.input.Next() }

func (it *selectIter) Deref(rec *relational.Record) error {
	var row relational.Record
	if err := it.input.Deref(&row); err != nil {
		return err
	}
	values := make([]relational.Value, len(it.exprs))
	for i, expr := range it.exprs {
		value, err := evalExpr(row, expr)
		if err != nil {
			return err
		}
		values[i] = value
	}
	rec.Cols, rec.Vals = it.names, values
	return nil
}

// ---------- the scan, filter and limit stage ----------

// scanIter applies FILTER, OFFSET and LIMIT to the rows a Scanner produces.
//
// It runs one row ahead: the next result is found and cached during Next, so
// Valid can answer without the caller having to distinguish "no more rows in
// the range" from "no more rows that pass the filter".
type scanIter struct {
	scanner relational.Scanner
	filter  Node
	offset  int64
	limit   int64

	matched int64 // rows that passed the filter, including skipped ones
	current relational.Record
	valid   bool
	err     error
}

func newScanIter(tx *relational.DBTX, scan *Scan) (*scanIter, error) {
	it := &scanIter{filter: scan.Filter, offset: scan.Offset, limit: scan.Limit}
	if err := setUpScan(tx, scan, &it.scanner); err != nil {
		return nil, err
	}
	it.advance()
	return it, it.err
}

func (it *scanIter) Valid() bool { return it.valid && it.err == nil }

func (it *scanIter) Next() {
	if it.valid {
		it.advance()
	}
}

func (it *scanIter) Deref(rec *relational.Record) error {
	if it.err != nil {
		return it.err
	}
	if !it.valid {
		return fmt.Errorf("ql: Deref called on a finished result")
	}
	*rec = it.current
	return nil
}

// Err reports a failure that ended the scan early, which Valid alone cannot
// distinguish from a scan that simply finished.
func (it *scanIter) Err() error { return it.err }

func (it *scanIter) advance() {
	for it.scanner.Valid() {
		var row relational.Record
		if err := it.scanner.Deref(&row); err != nil {
			it.err, it.valid = err, false
			return
		}
		it.scanner.Next()

		if it.filter.isSet() {
			verdict, err := evalExpr(row, it.filter)
			if err != nil {
				it.err, it.valid = err, false
				return
			}
			if !truthy(verdict) {
				continue
			}
		}

		it.matched++
		if it.matched <= it.offset {
			continue // inside OFFSET
		}
		if it.matched-it.offset > it.limit {
			it.valid = false // past LIMIT; stop scanning
			return
		}
		it.current, it.valid = row, true
		return
	}
	it.valid = false
}

// setUpScan turns the INDEX BY clause into a range over a chosen index.
func setUpScan(tx *relational.DBTX, scan *Scan, out *relational.Scanner) error {
	var err error
	if out.Key1, out.Cmp1, err = scanBound(scan.Key1); err != nil {
		return err
	}
	if out.Key2, out.Cmp2, err = scanBound(scan.Key2); err != nil {
		return err
	}

	// A clause that names only one end has to be completed, and what completes
	// it depends on the operator. See the infinity encoding in the relational
	// layer: an unconstrained end is the empty tuple, which every index covers.
	switch {
	case !scan.Key1.isSet() && !scan.Key2.isSet():
		// No INDEX BY: walk the whole table in primary key order.
		out.Cmp1, out.Cmp2 = btree.CmpGE, btree.CmpLE

	case scan.Key1.Type == NodeEq && !scan.Key2.isSet():
		// `index by a = 1` is the prefix of the index where a is 1, which is
		// the closed interval from (1, -inf) to (1, +inf).
		out.Key2 = out.Key1
		out.Cmp1, out.Cmp2 = btree.CmpGE, btree.CmpLE

	case scan.Key1.isSet() && !scan.Key2.isSet():
		// `index by a > 1` runs to the end of the index in that direction.
		if out.Cmp1 > 0 {
			out.Cmp2 = btree.CmpLE
		} else {
			out.Cmp2 = btree.CmpGE
		}

	case scan.Key1.Type == NodeEq || scan.Key2.Type == NodeEq:
		return evalErrorf("`=` in INDEX BY already covers a whole range and cannot be " +
			"combined with another bound")
	}

	return tx.Scan(scan.Table, out)
}

// scanBound converts one comparison from INDEX BY into a record of column
// values and a comparison operator.
func scanBound(node Node) (relational.Record, int, error) {
	if !node.isSet() {
		return relational.Record{}, 0, nil
	}
	if !node.isComparison() {
		return relational.Record{}, 0, evalErrorf("INDEX BY needs a comparison, found %s",
			nodeName(node.Type))
	}

	cmp, ok := map[int]int{
		NodeEq: btree.CmpGE, // refined by the caller into a closed interval
		NodeGE: btree.CmpGE,
		NodeGT: btree.CmpGT,
		NodeLE: btree.CmpLE,
		NodeLT: btree.CmpLT,
	}[node.Type]
	if !ok {
		return relational.Record{}, 0, evalErrorf("INDEX BY cannot use %s", nodeName(node.Type))
	}

	columns := tupleItems(node.Kids[0])
	values := tupleItems(node.Kids[1])
	if len(columns) != len(values) {
		return relational.Record{}, 0, evalErrorf(
			"INDEX BY compares %d columns with %d values", len(columns), len(values))
	}

	var rec relational.Record
	for i, col := range columns {
		if col.Type != NodeSym {
			return relational.Record{}, 0, evalErrorf(
				"the left side of an INDEX BY comparison must be a column name, found %s",
				nodeName(col.Type))
		}
		// The bound must be a constant: it is evaluated once, before any row
		// exists, to locate where the scan starts.
		value, err := evalExpr(relational.Record{}, values[i])
		if err != nil {
			return relational.Record{}, 0, evalErrorf(
				"the INDEX BY bound for %s must be a constant: %s", col.Str, err)
		}
		rec.Add(string(col.Str), value)
	}
	return rec, cmp, nil
}

// tupleItems flattens a tuple node into its items, treating a single
// expression as a one-item tuple.
func tupleItems(node Node) []Node {
	if node.Type == NodeTuple {
		return node.Kids
	}
	return []Node{node}
}

// ---------- INSERT ----------

func executeInsert(tx *relational.DBTX, stmt *Insert) (*Result, error) {
	tdef, err := tx.TableDef(stmt.Table)
	if err != nil {
		return nil, err
	}
	for _, col := range stmt.Cols {
		if !slices.Contains(tdef.Cols, col) {
			return nil, fmt.Errorf("%w: table %s has no column %s",
				relational.ErrBadRecord, stmt.Table, col)
		}
	}

	var affected int64
	for rowNumber, row := range stmt.Values {
		var rec relational.Record
		for i, expr := range row {
			// An empty environment: the values must be constants, since there
			// is no row yet for a column reference to mean anything.
			value, err := evalExpr(relational.Record{}, expr)
			if err != nil {
				return nil, fmt.Errorf("row %d, column %s: %w", rowNumber+1, stmt.Cols[i], err)
			}
			if stmt.Cols[i] == tdef.AutoIncCol {
				if value.Type == relational.TypeNull || (value.Type == relational.TypeInt64 && value.I64 == 0) {
					seq, err := tx.NextSeq(stmt.Table)
					if err != nil {
						return nil, err
					}
					value = relational.Int64(seq)
				} else if value.Type == relational.TypeInt64 {
					tx.UpdateSeqIfHigher(stmt.Table, value.I64)
				}
			}
			rec.Add(stmt.Cols[i], value)
		}
		if tdef.AutoIncCol != "" && !slices.Contains(stmt.Cols, tdef.AutoIncCol) {
			seq, err := tx.NextSeq(stmt.Table)
			if err != nil {
				return nil, err
			}
			rec.Add(tdef.AutoIncCol, relational.Int64(seq))
		}
		added, err := tx.Set(stmt.Table, rec, stmt.Mode)
		if err != nil {
			return nil, err
		}
		if !added && stmt.Mode == kv.ModeInsertOnly {
			return nil, fmt.Errorf("row %d: a row with this primary key already exists "+
				"(use REPLACE INTO to overwrite)", rowNumber+1)
		}
		if added {
			affected++
		}
	}
	return &Result{Affected: affected}, nil
}

// ---------- UPDATE and DELETE ----------

// collectRows reads a scan to completion before anything is modified.
//
// Modifying rows while iterating over them would change the range being
// walked underneath the iterator; reading first keeps the two apart.
func collectRows(tx *relational.DBTX, scan *Scan) ([]relational.Record, error) {
	iter, err := newScanIter(tx, scan)
	if err != nil {
		return nil, err
	}
	var rows []relational.Record
	for ; iter.Valid(); iter.Next() {
		var rec relational.Record
		if err := iter.Deref(&rec); err != nil {
			return nil, err
		}
		// Copied defensively. Decoding a row already allocates its strings,
		// so today this is redundant — but the rows outlive the writes below,
		// which move pages around, and relying on the decoder's allocation
		// behavior from up here would be a trap for anyone optimizing it.
		// relational.TestDerefReturnsIndependentMemory pins that behavior.
		rows = append(rows, copyRecord(rec))
	}
	return rows, iter.Err()
}

func copyRecord(rec relational.Record) relational.Record {
	out := relational.Record{
		Cols: append([]string(nil), rec.Cols...),
		Vals: append([]relational.Value(nil), rec.Vals...),
	}
	for i := range out.Vals {
		if out.Vals[i].Type == relational.TypeBytes {
			out.Vals[i].Str = append([]byte(nil), out.Vals[i].Str...)
		}
	}
	return out
}

func executeUpdate(tx *relational.DBTX, stmt *Update) (*Result, error) {
	tdef, err := tx.TableDef(stmt.Table)
	if err != nil {
		return nil, err
	}
	primaryKey := tdef.Indexes[0]
	for _, col := range stmt.Names {
		if !slices.Contains(tdef.Cols, col) {
			return nil, fmt.Errorf("%w: table %s has no column %s",
				relational.ErrBadRecord, stmt.Table, col)
		}
		if slices.Contains(primaryKey, col) {
			// Changing a primary key column moves the row to a different key,
			// which is a delete plus an insert rather than an update. Refusing
			// is clearer than quietly doing one of the two.
			return nil, fmt.Errorf("%w: %s is part of the primary key of %s and cannot "+
				"be updated; delete the row and insert it again",
				relational.ErrBadRecord, col, stmt.Table)
		}
	}

	rows, err := collectRows(tx, &stmt.Scan)
	if err != nil {
		return nil, err
	}

	var affected int64
	for _, row := range rows {
		updated := copyRecord(row)
		for i, col := range stmt.Names {
			// Expressions see the row as it was before this statement, so
			// `set a = a + 1` means the old a, and two assignments in one
			// statement cannot depend on each other's results.
			value, err := evalExpr(row, stmt.Values[i])
			if err != nil {
				return nil, err
			}
			want := tdef.Types[slices.Index(tdef.Cols, col)]
			if value.Type != want {
				return nil, fmt.Errorf("%w: column %s is %s, but the new value is %s",
					relational.ErrBadRecord, col, relational.TypeName(want),
					relational.TypeName(value.Type))
			}
			*updated.Get(col) = value
		}
		changed, err := tx.Update(stmt.Table, updated)
		if err != nil {
			return nil, err
		}
		if changed {
			affected++
		}
	}
	return &Result{Affected: affected}, nil
}

func executeDelete(tx *relational.DBTX, stmt *Delete) (*Result, error) {
	tdef, err := tx.TableDef(stmt.Table)
	if err != nil {
		return nil, err
	}
	rows, err := collectRows(tx, &stmt.Scan)
	if err != nil {
		return nil, err
	}

	var affected int64
	for _, row := range rows {
		// Only the primary key is needed to identify the row.
		var key relational.Record
		for _, col := range tdef.Indexes[0] {
			key.Add(col, *row.Get(col))
		}
		deleted, err := tx.Delete(stmt.Table, key)
		if err != nil {
			return nil, err
		}
		if deleted {
			affected++
		}
	}
	return &Result{Affected: affected}, nil
}

// ---------- GROUP BY & AGGREGATES ----------

func hasAggregates(exprs []Node) bool {
	for _, e := range exprs {
		if containsAggregate(e) {
			return true
		}
	}
	return false
}

func containsAggregate(n Node) bool {
	switch n.Type {
	case NodeCountStar, NodeCount, NodeSum, NodeAvg, NodeMin, NodeMax:
		return true
	}
	for _, kid := range n.Kids {
		if containsAggregate(kid) {
			return true
		}
	}
	return false
}

func executeGroupBy(input RecordIter, groupBy []Node, output []Node, names []string) (RecordIter, error) {
	var rows []relational.Record
	for input.Valid() {
		var rec relational.Record
		if err := input.Deref(&rec); err != nil {
			return nil, err
		}
		rows = append(rows, rec)
		input.Next()
	}

	type group struct {
		key  string
		rows []relational.Record
	}

	var groups []group
	groupMap := make(map[string]int)

	for _, row := range rows {
		var keyBuf []byte
		for _, gExpr := range groupBy {
			val, err := evalExpr(row, gExpr)
			if err != nil {
				return nil, err
			}
			keyBuf = append(keyBuf, []byte(val.String())...)
			keyBuf = append(keyBuf, 0x00)
		}
		k := string(keyBuf)
		if idx, ok := groupMap[k]; ok {
			groups[idx].rows = append(groups[idx].rows, row)
		} else {
			groupMap[k] = len(groups)
			groups = append(groups, group{key: k, rows: []relational.Record{row}})
		}
	}

	if len(groups) == 0 && len(groupBy) == 0 && hasAggregates(output) {
		groups = append(groups, group{key: "", rows: nil})
	}

	var outRows []relational.Record
	for _, g := range groups {
		var rec relational.Record
		rec.Cols = names
		rec.Vals = make([]relational.Value, len(output))
		for i, expr := range output {
			val, err := evalAggregateInGroup(g.rows, expr)
			if err != nil {
				return nil, err
			}
			rec.Vals[i] = val
		}
		outRows = append(outRows, rec)
	}

	return &sliceRecordIter{rows: outRows, pos: 0, valid: len(outRows) > 0}, nil
}

func evalAggregateInGroup(rows []relational.Record, expr Node) (relational.Value, error) {
	switch expr.Type {
	case NodeCountStar:
		return relational.Int64(int64(len(rows))), nil

	case NodeCount:
		var count int64
		for _, r := range rows {
			val, err := evalExpr(r, expr.Kids[0])
			if err == nil && val.Type != relational.TypeNull {
				count++
			}
		}
		return relational.Int64(count), nil

	case NodeSum:
		var sumInt int64
		var sumFloat float64
		isFloat := false
		hasVal := false
		for _, r := range rows {
			val, err := evalExpr(r, expr.Kids[0])
			if err == nil && val.Type != relational.TypeNull {
				hasVal = true
				if val.Type == relational.TypeFloat64 {
					isFloat = true
					sumFloat += val.F64
				} else if val.Type == relational.TypeInt64 {
					sumInt += val.I64
					sumFloat += float64(val.I64)
				}
			}
		}
		if !hasVal {
			return relational.Null(), nil
		}
		if isFloat {
			return relational.Float64(sumFloat), nil
		}
		return relational.Int64(sumInt), nil

	case NodeAvg:
		var sumFloat float64
		var count int64
		for _, r := range rows {
			val, err := evalExpr(r, expr.Kids[0])
			if err == nil && val.Type != relational.TypeNull {
				count++
				sumFloat += toFloat(val)
			}
		}
		if count == 0 {
			return relational.Null(), nil
		}
		return relational.Float64(sumFloat / float64(count)), nil

	case NodeMin:
		var minVal *relational.Value
		for _, r := range rows {
			val, err := evalExpr(r, expr.Kids[0])
			if err == nil && val.Type != relational.TypeNull {
				if minVal == nil || compareValues(val, *minVal) < 0 {
					v := val
					minVal = &v
				}
			}
		}
		if minVal == nil {
			return relational.Null(), nil
		}
		return *minVal, nil

	case NodeMax:
		var maxVal *relational.Value
		for _, r := range rows {
			val, err := evalExpr(r, expr.Kids[0])
			if err == nil && val.Type != relational.TypeNull {
				if maxVal == nil || compareValues(val, *maxVal) > 0 {
					v := val
					maxVal = &v
				}
			}
		}
		if maxVal == nil {
			return relational.Null(), nil
		}
		return *maxVal, nil

	default:
		if len(rows) > 0 {
			return evalExpr(rows[0], expr)
		}
		return relational.Null(), nil
	}
}

