package relational

import (
	"fmt"
	"strings"

	"byodb/btree"
	"byodb/kv"
)

// A row is stored as one key-value pair per index.
//
//	primary key   | prefix | pk columns...            | -> | other columns... |
//	secondary idx | prefix | indexed cols... | pk cols | -> | (empty)          |
//
// A secondary index appends the primary key to its own columns for two
// reasons: it makes otherwise-duplicate index entries unique, so the tree needs
// no support for duplicate keys, and it gives the scan a way back to the full
// row.

// Get looks up a single row by its primary key. rec supplies the key columns
// and receives the whole row.
func (tx *DBTX) Get(table string, rec *Record) (bool, error) {
	tdef, err := tx.TableDef(table)
	if err != nil {
		return false, err
	}
	return tx.get(tdef, rec)
}

func (tx *DBTX) get(tdef *TableDef, rec *Record) (bool, error) {
	values, err := checkRecord(tdef, *rec, tdef.PKeyCount())
	if err != nil {
		return false, err
	}
	key := encodeKey(nil, tdef.Prefixes[0], values[:tdef.PKeyCount()])
	encoded, ok := tx.kv.Get(key)
	if !ok {
		return false, nil
	}
	if err := decodeRow(tdef, values, encoded); err != nil {
		return false, err
	}
	rec.Cols = tdef.Cols
	rec.Vals = values
	return true, nil
}

// decodeRow fills in the non-key columns from a stored value.
func decodeRow(tdef *TableDef, values []Value, encoded []byte) error {
	n := tdef.PKeyCount()
	for i := n; i < len(tdef.Cols); i++ {
		values[i] = Value{Type: tdef.Types[i]}
	}
	return decodeValues(encoded, values[n:])
}

// Insert adds a row, failing if the primary key already exists.
func (tx *DBTX) Insert(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, kv.ModeInsertOnly)
}

// Update modifies an existing row, failing if the primary key is absent.
func (tx *DBTX) Update(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, kv.ModeUpdateOnly)
}

// Upsert adds a row or replaces an existing one.
func (tx *DBTX) Upsert(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, kv.ModeUpsert)
}

// Set writes a whole row according to mode.
func (tx *DBTX) Set(table string, rec Record, mode int) (bool, error) {
	tdef, err := tx.TableDef(table)
	if err != nil {
		return false, err
	}
	return tx.set(tdef, rec, mode)
}

// set writes the row and brings every index into line with it.
func (tx *DBTX) set(tdef *TableDef, rec Record, mode int) (bool, error) {
	values, err := checkRecord(tdef, rec, len(tdef.Cols))
	if err != nil {
		return false, err
	}

	if err := tx.checkUniqueConstraints(tdef, values); err != nil {
		return false, err
	}
	if err := tx.checkForeignKeysOnSet(tdef, values); err != nil {
		return false, err
	}

	n := tdef.PKeyCount()

	req := kv.UpdateReq{
		Key:  encodeKey(nil, tdef.Prefixes[0], values[:n]),
		Val:  encodeValues(nil, values[n:]),
		Mode: mode,
	}
	if err := tx.kv.Update(&req); err != nil {
		return false, err
	}
	if !req.Updated {
		return false, nil // the mode rejected this row; nothing changed
	}
	if len(tdef.Indexes) == 1 {
		return true, nil
	}

	if !req.Added {
		previous := make([]Value, len(values))
		copy(previous, values[:n])
		if err := decodeRow(tdef, previous, req.Old); err != nil {
			return false, err
		}
		if err := tx.updateIndexes(tdef, previous, false); err != nil {
			return false, err
		}
	}
	if err := tx.updateIndexes(tdef, values, true); err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes a row by its primary key.
func (tx *DBTX) Delete(table string, rec Record) (bool, error) {
	tdef, err := tx.TableDef(table)
	if err != nil {
		return false, err
	}
	return tx.deleteRow(tdef, rec)
}

func (tx *DBTX) deleteRow(tdef *TableDef, rec Record) (bool, error) {
	values, err := checkRecord(tdef, rec, tdef.PKeyCount())
	if err != nil {
		return false, err
	}
	n := tdef.PKeyCount()

	// Get full row first to check foreign keys
	fullRow := make([]Value, len(tdef.Cols))
	copy(fullRow, values[:n])
	reqGet := encodeKey(nil, tdef.Prefixes[0], values[:n])
	if encoded, ok := tx.kv.Get(reqGet); ok {
		decodeRow(tdef, fullRow, encoded)
		if err := tx.checkForeignKeysOnDelete(tdef, fullRow); err != nil {
			return false, err
		}
	}

	req := kv.DeleteReq{Key: encodeKey(nil, tdef.Prefixes[0], values[:n])}
	deleted, err := tx.kv.Del(&req)
	if err != nil || !deleted {
		return false, err
	}
	if len(tdef.Indexes) == 1 {
		return true, nil
	}

	if err := decodeRow(tdef, values, req.Old); err != nil {
		return false, err
	}
	if err := tx.updateIndexes(tdef, values, false); err != nil {
		return false, err
	}
	return true, nil
}

func (tx *DBTX) checkUniqueConstraints(tdef *TableDef, values []Value) error {
	for _, uCols := range tdef.Uniques {
		uVals := make([]Value, len(uCols))
		hasNull := false
		for i, col := range uCols {
			val := values[columnIndex(tdef, col)]
			if val.Type == TypeNull {
				hasNull = true
				break
			}
			uVals[i] = val
		}
		if hasNull {
			continue
		}

		scanner := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
		if err := tx.scanAll(tdef, &scanner); err != nil {
			return err
		}
		pkCount := tdef.PKeyCount()
		for ; scanner.Valid(); scanner.Next() {
			var row Record
			if err := scanner.Deref(&row); err != nil {
				return err
			}
			samePK := true
			for i := 0; i < pkCount; i++ {
				if !row.Vals[i].Equal(values[i]) {
					samePK = false
					break
				}
			}
			if samePK {
				continue
			}
			match := true
			for i, col := range uCols {
				if !row.Get(col).Equal(uVals[i]) {
					match = false
					break
				}
			}
			if match {
				return fmt.Errorf("%w: column(s) %v in table %q must be unique", ErrUniqueConstraint, uCols, tdef.Name)
			}
		}
	}
	return nil
}

func (tx *DBTX) checkForeignKeysOnSet(tdef *TableDef, values []Value) error {
	for _, fk := range tdef.ForeignKeys {
		refTdef, err := tx.TableDef(fk.RefTable)
		if err != nil {
			return fmt.Errorf("%w: referenced table %q not found", ErrForeignKeyConstraint, fk.RefTable)
		}
		refRec := Record{Cols: fk.RefCols, Vals: make([]Value, len(fk.RefCols))}
		allNull := true
		for i, c := range fk.Cols {
			val := values[columnIndex(tdef, c)]
			if val.Type != TypeNull {
				allNull = false
			}
			refRec.Vals[i] = val
		}
		if allNull {
			continue
		}
		found, err := tx.get(refTdef, &refRec)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: table %q column(s) %v value(s) not found in referenced table %q", ErrForeignKeyConstraint, tdef.Name, fk.Cols, fk.RefTable)
		}
	}
	return nil
}

func (tx *DBTX) checkForeignKeysOnDelete(tdef *TableDef, values []Value) error {
	tables, err := tx.TableList()
	if err != nil {
		return err
	}
	for _, tbl := range tables {
		childTdef, err := tx.TableDef(tbl)
		if err != nil || childTdef == nil {
			continue
		}
		for _, fk := range childTdef.ForeignKeys {
			if fk.RefTable != tdef.Name {
				continue
			}
			parentVals := make([]Value, len(fk.RefCols))
			for i, rc := range fk.RefCols {
				parentVals[i] = values[columnIndex(tdef, rc)]
			}
			var childRows []Record
			scanner := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
			if err := tx.scanAll(childTdef, &scanner); err != nil {
				return err
			}
			for ; scanner.Valid(); scanner.Next() {
				var rec Record
				if err := scanner.Deref(&rec); err != nil {
					return err
				}
				match := true
				for i, cc := range fk.Cols {
					if !rec.Get(cc).Equal(parentVals[i]) {
						match = false
						break
					}
				}
				if match {
					childRows = append(childRows, rec)
				}
			}
			if len(childRows) > 0 {
				if strings.EqualFold(fk.OnDelete, "CASCADE") {
					for _, cr := range childRows {
						if _, err := tx.deleteRow(childTdef, cr); err != nil {
							return err
						}
					}
				} else {
					return fmt.Errorf("%w: cannot delete row in table %q referenced by table %q", ErrForeignKeyConstraint, tdef.Name, childTdef.Name)
				}
			}
		}
	}
	return nil
}

// updateIndexes adds or removes the secondary index entries for one row.
func (tx *DBTX) updateIndexes(tdef *TableDef, values []Value, add bool) error {
	for i := 1; i < len(tdef.Indexes); i++ {
		key := encodeIndexKey(tdef, i, values)
		if add {
			req := kv.UpdateReq{Key: key, Val: nil, Mode: kv.ModeUpsert}
			if err := tx.kv.Update(&req); err != nil {
				return err
			}
		} else if _, err := tx.kv.Del(&kv.DeleteReq{Key: key}); err != nil {
			return err
		}
	}
	return nil
}

// encodeIndexKey builds one secondary index entry: the indexed columns
// followed by the primary key columns.
func encodeIndexKey(tdef *TableDef, index int, values []Value) []byte {
	cols := make([]Value, 0, len(tdef.Indexes[index])+tdef.PKeyCount())
	for _, name := range tdef.Indexes[index] {
		cols = append(cols, values[columnIndex(tdef, name)])
	}
	// Appending the primary key makes the entry unique and gives a scan its
	// route back to the row.
	for _, name := range tdef.Indexes[0] {
		cols = append(cols, values[columnIndex(tdef, name)])
	}
	return encodeKey(nil, tdef.Prefixes[index], cols)
}

func columnIndex(tdef *TableDef, name string) int {
	for i, col := range tdef.Cols {
		if col == name {
			return i
		}
	}
	panic(fmt.Sprintf("relational: table %q has no column %q", tdef.Name, name))
}
