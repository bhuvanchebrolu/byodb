// Command byodb is a shell for the database.
//
//	byodb path/to/file.db          interactive
//	byodb file.db "select ..."     run one statement
//	byodb file.db < script.sql     run a script
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"byodb/ql"
	"byodb/relational"
	"byodb/web"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--web" {
		port := 8080
		path := "database.db"
		if len(os.Args) >= 3 {
			path = os.Args[2]
		}
		if len(os.Args) >= 4 {
			fmt.Sscanf(os.Args[3], "%d", &port)
		}
		db, err := relational.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
			os.Exit(1)
		}
		defer db.Close()

		srv := web.NewServer(db, path, port)
		if err := srv.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "web server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: byodb <database file> [statement] OR byodb --web [db file] [port]")
		os.Exit(2)
	}
	path := os.Args[1]

	db, err := relational.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer db.Close()

	shell := &shell{db: db, out: os.Stdout}

	if len(os.Args) > 2 {
		if err := shell.run(strings.Join(os.Args[2:], " ")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	info, _ := os.Stdin.Stat()
	interactive := info != nil && (info.Mode()&os.ModeCharDevice) != 0
	if interactive {
		fmt.Printf("byodb — %s\nEnd statements with ';'. Type .help for commands, .quit to exit.\n\n", path)
	}
	shell.repl(os.Stdin, interactive)
}

type shell struct {
	db       *relational.DB
	activeTx *relational.DBTX
	out      io.Writer
}

// run executes one statement and prints the result.
func (s *shell) run(statement string) error {
	stmt, err := ql.Parse(statement)
	if err != nil {
		return err
	}

	switch stmt.(type) {
	case *ql.BeginStmt:
		if s.activeTx != nil {
			return fmt.Errorf("transaction already active")
		}
		s.activeTx = &relational.DBTX{}
		s.db.Begin(s.activeTx)
		fmt.Fprintln(s.out, "BEGIN TRANSACTION")
		return nil

	case *ql.CommitStmt:
		if s.activeTx == nil {
			return fmt.Errorf("no active transaction to commit")
		}
		err := s.db.Commit(s.activeTx)
		s.activeTx = nil
		if err != nil {
			return fmt.Errorf("commit failed: %w", err)
		}
		fmt.Fprintln(s.out, "COMMIT")
		return nil

	case *ql.RollbackStmt:
		if s.activeTx == nil {
			return fmt.Errorf("no active transaction to rollback")
		}
		s.db.Abort(s.activeTx)
		s.activeTx = nil
		fmt.Fprintln(s.out, "ROLLBACK")
		return nil
	}

	if s.activeTx != nil {
		result, err := ql.Execute(s.activeTx, stmt)
		if err != nil {
			return err
		}
		if _, isSelect := stmt.(*ql.Select); isSelect {
			return s.printRows(result)
		}
		if result.Affected > 0 {
			fmt.Fprintf(s.out, "%d row(s) affected\n", result.Affected)
		} else {
			fmt.Fprintln(s.out, "ok")
		}
		return nil
	}

	// Autocommit mode
	if _, isSelect := stmt.(*ql.Select); isSelect {
		return s.db.View(func(tx *relational.DBTX) error {
			result, err := ql.Execute(tx, stmt)
			if err != nil {
				return err
			}
			return s.printRows(result)
		})
	}

	return s.db.Update(func(tx *relational.DBTX) error {
		result, err := ql.Execute(tx, stmt)
		if err != nil {
			return err
		}
		if result.Affected > 0 {
			fmt.Fprintf(s.out, "%d row(s) affected\n", result.Affected)
		} else {
			fmt.Fprintln(s.out, "ok")
		}
		return nil
	})
}

// repl reads statements until end of input. Statements may span lines and end
// at a semicolon, so that a schema can be pasted in as written.
func (s *shell) repl(in io.Reader, interactive bool) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var pending strings.Builder
	prompt := func() {
		if !interactive {
			return
		}
		if pending.Len() == 0 {
			fmt.Fprint(s.out, "byodb> ")
		} else {
			fmt.Fprint(s.out, "  ...> ")
		}
	}

	prompt()
	for scanner.Scan() {
		line := scanner.Text()

		// Dot commands are only recognized at the start of a statement, so a
		// line inside a pending statement is never mistaken for one.
		if pending.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			if done := s.dotCommand(strings.TrimSpace(line)); done {
				return
			}
			prompt()
			continue
		}

		pending.WriteString(line)
		pending.WriteString("\n")
		if !strings.Contains(line, ";") {
			prompt()
			continue
		}

		statement := strings.TrimSpace(pending.String())
		pending.Reset()
		if statement != "" {
			if err := s.run(statement); err != nil {
				fmt.Fprintf(s.out, "error: %v\n", err)
			}
		}
		prompt()
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "reading input: %v\n", err)
	}
}

func (s *shell) dotCommand(line string) (quit bool) {
	fields := strings.Fields(line)
	switch fields[0] {
	case ".quit", ".exit":
		return true

	case ".help":
		fmt.Fprint(s.out, `Statements:
  create table t (a int64, b bytes, index (b), primary key (a));
  insert into t (a, b) values (1, 'x'), (2, 'y');
  replace into t (a, b) values (1, 'z');
  select a, b from t index by a > 1 filter b != 'x' limit 10;
  update t set b = b + '!' index by a = 1;
  delete from t filter a > 5;
  drop table t;

INDEX BY chooses the index and the range; FILTER tests each row.

Commands:
  .tables            list tables
  .schema [table]    show column and index definitions
  .help              this text
  .quit              exit
`)

	case ".tables":
		err := s.db.View(func(tx *relational.DBTX) error {
			names, err := tx.TableList()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Fprintln(s.out, "(no tables)")
				return nil
			}
			for _, name := range names {
				fmt.Fprintln(s.out, name)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(s.out, "error: %v\n", err)
		}

	case ".schema":
		err := s.db.View(func(tx *relational.DBTX) error {
			names := fields[1:]
			if len(names) == 0 {
				var err error
				if names, err = tx.TableList(); err != nil {
					return err
				}
			}
			for _, name := range names {
				tdef, err := tx.TableDef(name)
				if err != nil {
					return err
				}
				fmt.Fprintln(s.out, formatSchema(tdef))
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(s.out, "error: %v\n", err)
		}

	default:
		fmt.Fprintf(s.out, "unknown command %s (try .help)\n", fields[0])
	}
	return false
}

// formatSchema renders a table definition as the statement that would create
// it, so what comes out can be fed back in.
func formatSchema(tdef *relational.TableDef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "create table %s (\n", tdef.Name)
	for i, col := range tdef.Cols {
		fmt.Fprintf(&b, "    %s %s,\n", col, relational.TypeName(tdef.Types[i]))
	}
	for _, index := range tdef.Indexes[1:] {
		fmt.Fprintf(&b, "    index (%s),\n", strings.Join(index, ", "))
	}
	fmt.Fprintf(&b, "    primary key (%s)\n);", strings.Join(tdef.Indexes[0], ", "))
	return b.String()
}



// printRows renders a result set as an aligned table.
//
// Rows are buffered here because column widths cannot be known until every
// value has been seen. That is a deliberate exception to streaming, confined
// to display.
func (s *shell) printRows(result *ql.Result) error {
	widths := make([]int, len(result.Names))
	for i, name := range result.Names {
		widths[i] = len(name)
	}

	var rows [][]string
	for ; result.Rows.Valid(); result.Rows.Next() {
		var rec relational.Record
		if err := result.Rows.Deref(&rec); err != nil {
			return err
		}
		cells := make([]string, len(rec.Vals))
		for i, v := range rec.Vals {
			cells[i] = v.String()
			if i < len(widths) && len(cells[i]) > widths[i] {
				widths[i] = len(cells[i])
			}
		}
		rows = append(rows, cells)
	}

	line := func(cells []string) {
		parts := make([]string, len(cells))
		for i, cell := range cells {
			width := 0
			if i < len(widths) {
				width = widths[i]
			}
			parts[i] = cell + strings.Repeat(" ", max(0, width-len(cell)))
		}
		fmt.Fprintln(s.out, strings.TrimRight(strings.Join(parts, "  "), " "))
	}

	line(result.Names)
	separators := make([]string, len(result.Names))
	for i := range separators {
		separators[i] = strings.Repeat("-", widths[i])
	}
	line(separators)
	for _, row := range rows {
		line(row)
	}
	fmt.Fprintf(s.out, "(%d rows)\n", len(rows))
	return nil
}
