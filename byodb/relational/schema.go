package relational

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"byodb/btree"
	"byodb/kv"
)

var (
	ErrUniqueConstraint     = errors.New("relational: unique constraint violation")
	ErrForeignKeyConstraint = errors.New("relational: foreign key constraint violation")
)

// ForeignKeyDef defines a foreign key reference to another table.
type ForeignKeyDef struct {
	Cols     []string `json:"cols"`
	RefTable string   `json:"ref_table"`
	RefCols  []string `json:"ref_cols"`
	OnDelete string   `json:"on_delete"` // "RESTRICT" or "CASCADE"
}

// TableDef is a table's schema.
//
// Indexes[0] is the primary key. Treating the primary key as simply the first
// index removes a whole category of special cases: index selection, encoding
// and maintenance all work the same way whichever index is chosen, and the
// only thing that distinguishes the primary one is that it is unique and its
// entries carry the row's remaining columns as their value.
type TableDef struct {
	Name        string          `json:"name"`
	Cols        []string        `json:"cols"`
	Types       []uint32        `json:"types"`
	Defaults    []Value         `json:"defaults"`
	Checks      []string        `json:"checks"`
	AutoIncCol  string          `json:"auto_inc_col,omitempty"` // Column name that auto-increments
	Uniques     [][]string      `json:"uniques,omitempty"`      // Unique column sets
	ForeignKeys []ForeignKeyDef `json:"foreign_keys,omitempty"` // Foreign key constraints
	Indexes     [][]string      `json:"indexes"`                // Indexes[0] is the primary key
	Prefixes    []uint32        `json:"prefixes"`               // one B+tree key prefix per index
}

// PKeyCount is the number of columns in the primary key. Because TableNew
// reorders columns so the primary key comes first, these are always Cols[:n].
func (tdef *TableDef) PKeyCount() int { return len(tdef.Indexes[0]) }

// Reserved key prefixes. Prefix 0 is left unused so that a zero value is never
// mistaken for a real table.
const (
	prefixMeta      = 1 // the @meta table
	prefixTable     = 2 // the @table table
	firstUserPrefix = 3
)

// Internal tables. The database stores its own schemas the same way it stores
// user data — the only bootstrap needed is that these two definitions live in
// code rather than on disk, so there is something to read the others with.
var (
	tableMeta = &TableDef{
		Name:     "@meta",
		Cols:     []string{"key", "val"},
		Types:    []uint32{TypeBytes, TypeBytes},
		Indexes:  [][]string{{"key"}},
		Prefixes: []uint32{prefixMeta},
	}
	tableSchemas = &TableDef{
		Name:     "@table",
		Cols:     []string{"name", "def"},
		Types:    []uint32{TypeBytes, TypeBytes},
		Indexes:  [][]string{{"name"}},
		Prefixes: []uint32{prefixTable},
	}
)

// metaNextPrefix is the @meta key holding the counter that hands out key
// prefixes to new tables and indexes.
const metaNextPrefix = "next_prefix"

// normalize validates a user-supplied definition and puts it in the canonical
// shape: primary key columns first, in primary key order.
//
// Reordering here means the rest of the code can assume the primary key is
// Cols[:n], so encoding a row splits at a single index instead of gathering
// scattered columns.
func normalize(tdef *TableDef) error {
	switch {
	case tdef.Name == "":
		return fmt.Errorf("%w: the table has no name", ErrBadSchema)
	case len(tdef.Cols) == 0:
		return fmt.Errorf("%w: table %q has no columns", ErrBadSchema, tdef.Name)
	case len(tdef.Cols) != len(tdef.Types):
		return fmt.Errorf("%w: table %q has %d columns but %d types",
			ErrBadSchema, tdef.Name, len(tdef.Cols), len(tdef.Types))
	case len(tdef.Indexes) == 0 || len(tdef.Indexes[0]) == 0:
		return fmt.Errorf("%w: table %q has no primary key", ErrBadSchema, tdef.Name)
	}

	seen := map[string]bool{}
	for i, col := range tdef.Cols {
		if col == "" {
			return fmt.Errorf("%w: table %q has an unnamed column", ErrBadSchema, tdef.Name)
		}
		if seen[col] {
			return fmt.Errorf("%w: table %q declares column %q twice", ErrBadSchema, tdef.Name, col)
		}
		seen[col] = true
		if tdef.Types[i] != TypeBytes && tdef.Types[i] != TypeInt64 && tdef.Types[i] != TypeFloat64 {
			return fmt.Errorf("%w: column %q has unrecognized type %d",
				ErrBadSchema, col, tdef.Types[i])
		}
	}

	for _, index := range tdef.Indexes {
		if len(index) == 0 {
			return fmt.Errorf("%w: table %q has an empty index", ErrBadSchema, tdef.Name)
		}
		inIndex := map[string]bool{}
		for _, col := range index {
			if !seen[col] {
				return fmt.Errorf("%w: index on table %q names unknown column %q",
					ErrBadSchema, tdef.Name, col)
			}
			if inIndex[col] {
				return fmt.Errorf("%w: index on table %q uses column %q twice",
					ErrBadSchema, tdef.Name, col)
			}
			inIndex[col] = true
		}
	}

	// Move the primary key columns to the front, keeping the declared order of
	// everything else.
	pkey := tdef.Indexes[0]
	cols := append([]string(nil), pkey...)
	types := make([]uint32, 0, len(tdef.Cols))
	for _, col := range pkey {
		types = append(types, tdef.Types[slices.Index(tdef.Cols, col)])
	}
	for i, col := range tdef.Cols {
		if !slices.Contains(pkey, col) {
			cols = append(cols, col)
			types = append(types, tdef.Types[i])
		}
	}
	tdef.Cols, tdef.Types = cols, types
	return nil
}

// TableNew creates a table.
//
// Four keys change: the schema row, and the prefix counter — plus one prefix
// allocation per index. They all land in this transaction, so a half-created
// table is not a state the database can be left in.
func (tx *DBTX) TableNew(tdef *TableDef) error {
	if err := normalize(tdef); err != nil {
		return err
	}
	if existing, err := tx.getTableDef(tdef.Name); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("%w: %s", ErrTableExists, tdef.Name)
	}

	// Allocate one key prefix per index from the shared counter.
	next, err := tx.nextPrefix()
	if err != nil {
		return err
	}
	tdef.Prefixes = nil
	for range tdef.Indexes {
		tdef.Prefixes = append(tdef.Prefixes, next)
		next++
	}
	if err := tx.setNextPrefix(next); err != nil {
		return err
	}

	encoded, err := json.Marshal(tdef)
	if err != nil {
		return fmt.Errorf("relational: encoding the schema of %q: %w", tdef.Name, err)
	}
	row := (&Record{}).AddStr("name", []byte(tdef.Name)).AddStr("def", encoded)
	added, err := tx.set(tableSchemas, *row, kv.ModeInsertOnly)
	if err != nil {
		return err
	}
	if !added {
		return fmt.Errorf("%w: %s", ErrTableExists, tdef.Name)
	}
	// The cache recorded the absence of this table a moment ago; the rest of
	// this transaction must see it now.
	tx.schemas[tdef.Name] = tdef
	return nil
}

// TableDrop removes a table's schema and every row and index entry it owns.
func (tx *DBTX) TableDrop(name string) error {
	tdef, err := tx.getTableDef(name)
	if err != nil {
		return err
	}
	if tdef == nil {
		return fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}

	// Collect the primary keys first: deleting rows while scanning them would
	// mutate the range being walked.
	var rows []Record
	scanner := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
	if err := tx.scanAll(tdef, &scanner); err != nil {
		return err
	}
	for ; scanner.Valid(); scanner.Next() {
		var rec Record
		if err := scanner.Deref(&rec); err != nil {
			return err
		}
		rows = append(rows, rec)
	}
	for _, rec := range rows {
		if _, err := tx.deleteRow(tdef, rec); err != nil {
			return err
		}
	}

	key := (&Record{}).AddStr("name", []byte(name))
	if _, err := tx.deleteRow(tableSchemas, *key); err != nil {
		return err
	}
	tx.schemas[name] = nil
	return nil
}

// TableList returns the names of the user tables, in order.
func (tx *DBTX) TableList() ([]string, error) {
	var names []string
	scanner := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
	if err := tx.scanAll(tableSchemas, &scanner); err != nil {
		return nil, err
	}
	for ; scanner.Valid(); scanner.Next() {
		var rec Record
		if err := scanner.Deref(&rec); err != nil {
			return nil, err
		}
		names = append(names, string(rec.Get("name").Str))
	}
	return names, nil
}

// TableDef returns a table's schema, or ErrTableNotFound.
func (tx *DBTX) TableDef(name string) (*TableDef, error) {
	tdef, err := tx.getTableDef(name)
	if err != nil {
		return nil, err
	}
	if tdef == nil {
		return nil, fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}
	return tdef, nil
}

// getTableDef reads a schema, returning nil when the table does not exist.
func (tx *DBTX) getTableDef(name string) (*TableDef, error) {
	switch name {
	case tableMeta.Name:
		return tableMeta, nil
	case tableSchemas.Name:
		return tableSchemas, nil
	}
	if cached, ok := tx.schemas[name]; ok {
		return cached, nil // nil is cached too: "this table does not exist"
	}

	rec := (&Record{}).AddStr("name", []byte(name))
	found, err := tx.get(tableSchemas, rec)
	if err != nil {
		return nil, err
	}
	if !found {
		tx.schemas[name] = nil
		return nil, nil
	}
	tdef := &TableDef{}
	if err := json.Unmarshal(rec.Get("def").Str, tdef); err != nil {
		return nil, fmt.Errorf("relational: schema of %q is corrupt: %w", name, err)
	}
	tx.schemas[name] = tdef
	return tdef, nil
}

func (tx *DBTX) nextPrefix() (uint32, error) {
	rec := (&Record{}).AddStr("key", []byte(metaNextPrefix))
	found, err := tx.get(tableMeta, rec)
	if err != nil {
		return 0, err
	}
	if !found {
		return firstUserPrefix, nil
	}
	var value uint32
	if _, err := fmt.Sscanf(string(rec.Get("val").Str), "%d", &value); err != nil {
		return 0, fmt.Errorf("relational: the table prefix counter is corrupt: %w", err)
	}
	if value < firstUserPrefix {
		return firstUserPrefix, nil
	}
	return value, nil
}

func (tx *DBTX) setNextPrefix(value uint32) error {
	rec := (&Record{}).
		AddStr("key", []byte(metaNextPrefix)).
		AddStr("val", []byte(fmt.Sprintf("%d", value)))
	_, err := tx.set(tableMeta, *rec, kv.ModeUpsert)
	return err
}

// TableAlterAddCol adds a new column to an existing table definition.
func (tx *DBTX) TableAlterAddCol(table string, col string, colType uint32) error {
	tdef, err := tx.getTableDef(table)
	if err != nil {
		return err
	}
	if tdef == nil {
		return fmt.Errorf("%w: %s", ErrTableNotFound, table)
	}
	if slices.Contains(tdef.Cols, col) {
		return fmt.Errorf("%w: column %q already exists in table %q", ErrBadSchema, col, table)
	}

	tdef.Cols = append(tdef.Cols, col)
	tdef.Types = append(tdef.Types, colType)

	encoded, err := json.Marshal(tdef)
	if err != nil {
		return fmt.Errorf("relational: encoding updated schema of %q: %w", table, err)
	}
	row := (&Record{}).AddStr("name", []byte(table)).AddStr("def", encoded)
	if _, err := tx.set(tableSchemas, *row, kv.ModeUpsert); err != nil {
		return err
	}
	tx.schemas[table] = tdef
	return nil
}

// ViewNew creates a named view definition in the database metadata.
func (tx *DBTX) ViewNew(name string, query string) error {
	tdef, _ := tx.getTableDef(name)
	if tdef != nil {
		return fmt.Errorf("%w: %s (table exists with same name)", ErrTableExists, name)
	}
	key := "view:" + name
	rec := (&Record{}).
		AddStr("key", []byte(key)).
		AddStr("val", []byte(query))
	added, err := tx.set(tableMeta, *rec, kv.ModeInsertOnly)
	if err != nil {
		return err
	}
	if !added {
		return fmt.Errorf("%w: view %s already exists", ErrTableExists, name)
	}
	return nil
}

// ViewDrop removes a view definition.
func (tx *DBTX) ViewDrop(name string) error {
	key := "view:" + name
	rec := (&Record{}).AddStr("key", []byte(key))
	deleted, err := tx.deleteRow(tableMeta, *rec)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("%w: view %s not found", ErrTableNotFound, name)
	}
	return nil
}

// ViewGet retrieves the SQL query string for a view.
func (tx *DBTX) ViewGet(name string) (string, error) {
	key := "view:" + name
	rec := (&Record{}).AddStr("key", []byte(key))
	found, err := tx.get(tableMeta, rec)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: view %s not found", ErrTableNotFound, name)
	}
	return string(rec.Get("val").Str), nil
}

// ViewList returns the names of all views in the database.
func (tx *DBTX) ViewList() ([]string, error) {
	var views []string
	scanner := Scanner{Cmp1: btree.CmpGE, Cmp2: btree.CmpLE}
	if err := tx.scanAll(tableMeta, &scanner); err != nil {
		return nil, err
	}
	for ; scanner.Valid(); scanner.Next() {
		var rec Record
		if err := scanner.Deref(&rec); err != nil {
			return nil, err
		}
		k := string(rec.Get("key").Str)
		if strings.HasPrefix(k, "view:") {
			views = append(views, strings.TrimPrefix(k, "view:"))
		}
	}
	return views, nil
}

// NextSeq returns and increments the next auto-increment integer for a table.
func (tx *DBTX) NextSeq(table string) (int64, error) {
	key := "seq:" + table
	rec := (&Record{}).AddStr("key", []byte(key))
	found, err := tx.get(tableMeta, rec)
	if err != nil {
		return 0, err
	}
	var current int64 = 0
	if found {
		fmt.Sscanf(string(rec.Get("val").Str), "%d", &current)
	}
	next := current + 1
	recSet := (&Record{}).
		AddStr("key", []byte(key)).
		AddStr("val", []byte(fmt.Sprintf("%d", next)))
	if _, err := tx.set(tableMeta, *recSet, kv.ModeUpsert); err != nil {
		return 0, err
	}
	return next, nil
}

// UpdateSeqIfHigher ensures the sequence counter is at least val.
func (tx *DBTX) UpdateSeqIfHigher(table string, val int64) error {
	key := "seq:" + table
	rec := (&Record{}).AddStr("key", []byte(key))
	found, err := tx.get(tableMeta, rec)
	if err != nil {
		return err
	}
	var current int64 = 0
	if found {
		fmt.Sscanf(string(rec.Get("val").Str), "%d", &current)
	}
	if val > current {
		recSet := (&Record{}).
			AddStr("key", []byte(key)).
			AddStr("val", []byte(fmt.Sprintf("%d", val)))
		_, err := tx.set(tableMeta, *recSet, kv.ModeUpsert)
		return err
	}
	return nil
}


