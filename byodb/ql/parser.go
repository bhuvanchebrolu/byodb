package ql

import (
	"fmt"
	"math"
	"strings"

	"byodb/kv"
	"byodb/relational"
)

// The parser is recursive descent: each grammar rule is a function, and
// deciding which rule applies is a matter of looking at the next token.
//
// Operator precedence — the part that looks like it needs a table — comes out
// of the same recursion. One function per precedence level, each parsing a
// sequence of the next level down joined by its own operators. Because the
// lower levels are consumed first, they bind tighter:
//
//	OR  ->  AND  ->  NOT  ->  comparison  ->  + -  ->  * / %  ->  unary  ->  atom

type parser struct {
	tokens []token
	pos    int
	input  string
}

// Parse turns one statement into a syntax tree. The result is one of the
// statement types in ast.go.
func Parse(input string) (any, error) {
	tokens, err := tokenize(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens, input: input}

	stmt, err := p.statement()
	if err != nil {
		return nil, err
	}
	p.accept(";")
	if !p.atEOF() {
		return nil, p.errorf("unexpected %s after the end of the statement", p.peek().raw)
	}
	return stmt, nil
}

// ---------- token plumbing ----------

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) atEOF() bool { return p.tokens[p.pos].kind == tokEOF }

func (p *parser) errorf(format string, args ...any) error {
	return &ParseError{Msg: fmt.Sprintf(format, args...), Pos: p.peek().pos, Input: p.input}
}

// accept consumes the given sequence of keywords or operators if they all
// match, and reports whether it did. Matching a whole sequence is what lets a
// two-word keyword like "index by" be tested atomically.
func (p *parser) accept(words ...string) bool {
	saved := p.pos
	for _, want := range words {
		tok := p.tokens[p.pos]
		if tok.kind != tokSymbol && tok.kind != tokOperator {
			p.pos = saved
			return false
		}
		if tok.text != want {
			p.pos = saved
			return false
		}
		p.pos++
	}
	return true
}

// peekWord reports whether the next token is the given keyword, without
// consuming it.
func (p *parser) peekWord(word string) bool {
	tok := p.peek()
	return (tok.kind == tokSymbol || tok.kind == tokOperator) && tok.text == word
}

func (p *parser) expect(word string, context string) error {
	if p.accept(word) {
		return nil
	}
	return p.errorf("expected %s %s, found %s", word, context, p.peek().raw)
}

// name reads an identifier, rejecting the keywords that would otherwise be
// swallowed as a table or column name and produce a baffling error later.
func (p *parser) name(what string) (string, error) {
	tok := p.peek()
	if tok.kind != tokSymbol {
		return "", p.errorf("expected %s, found %s", what, tok.raw)
	}
	if reservedWords[tok.text] {
		return "", p.errorf("%s is a reserved word and cannot be used as %s", tok.raw, what)
	}
	p.pos++
	return tok.raw, nil
}

var reservedWords = map[string]bool{
	"select": true, "from": true, "where": true, "insert": true, "into": true,
	"values": true, "update": true, "set": true, "delete": true, "create": true,
	"table": true, "drop": true, "index": true, "by": true, "filter": true,
	"limit": true, "and": true, "or": true, "not": true, "primary": true,
	"key": true, "as": true, "replace": true, "join": true, "on": true,
	"order": true, "asc": true, "desc": true, "is": true, "null": true,
	"group": true, "alter": true, "left": true, "right": true, "inner": true,
	"sum": true, "avg": true, "min": true, "max": true,
	"begin": true, "commit": true, "rollback": true, "transaction": true,
	"default": true, "check": true, "like": true, "between": true, "in": true,
	"distinct": true, "union": true, "intersect": true,
}

// ---------- statements ----------

func (p *parser) statement() (any, error) {
	stmt, err := p.primaryStatement()
	if err != nil {
		return nil, err
	}
	if p.accept("union") {
		all := p.accept("all")
		right, err := p.statement()
		if err != nil {
			return nil, err
		}
		return &UnionStmt{Left: stmt, Right: right, All: all}, nil
	}
	if p.accept("intersect") {
		right, err := p.statement()
		if err != nil {
			return nil, err
		}
		return &IntersectStmt{Left: stmt, Right: right}, nil
	}
	return stmt, nil
}

func (p *parser) primaryStatement() (any, error) {
	switch {
	case p.accept("create", "table"):
		return p.createTable()
	case p.accept("create", "view"):
		return p.createView()
	case p.accept("drop", "table"):
		return p.dropTable()
	case p.accept("drop", "view"):
		return p.dropView()
	case p.accept("alter", "table"):
		return p.alterTable()
	case p.accept("begin", "transaction") || p.accept("begin"):
		return &BeginStmt{}, nil
	case p.accept("commit", "transaction") || p.accept("commit"):
		return &CommitStmt{}, nil
	case p.accept("rollback", "transaction") || p.accept("rollback"):
		return &RollbackStmt{}, nil
	case p.accept("select"):
		return p.selectStmt()
	case p.accept("insert", "into"):
		return p.insertStmt(kv.ModeInsertOnly)
	case p.accept("replace", "into"):
		return p.insertStmt(kv.ModeUpsert)
	case p.accept("update"):
		return p.updateStmt()
	case p.accept("delete", "from"):
		return p.deleteStmt()
	default:
		return nil, p.errorf("expected a statement (select, insert, replace, update, "+
			"delete, create table, create view, drop table, drop view, alter table, begin, commit, rollback), found %s", p.peek().raw)
	}
}

func (p *parser) alterTable() (*AlterTable, error) {
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expect("add", "after table name"); err != nil {
		return nil, err
	}
	p.accept("column")
	col, err := p.name("a column name")
	if err != nil {
		return nil, err
	}
	colType, err := p.columnType()
	if err != nil {
		return nil, err
	}
	return &AlterTable{Table: table, Col: col, Type: colType}, nil
}

// createTable parses:
//
//	create table t (a int64, b bytes, index (b, a), primary key (a));
func (p *parser) createTable() (*CreateTable, error) {
	ifNotExists := p.accept("if", "not", "exists")
	name, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expect("(", "after the table name"); err != nil {
		return nil, err
	}

	def := &relational.TableDef{Name: name}
	var primaryKey []string
	var secondary [][]string

	for {
		switch {
		case p.accept("primary", "key"):
			if primaryKey != nil {
				return nil, p.errorf("the table already has a primary key")
			}
			if primaryKey, err = p.columnList(); err != nil {
				return nil, err
			}
		case p.accept("check"):
			if err := p.expect("(", "after CHECK"); err != nil {
				return nil, err
			}
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")", "after CHECK expression"); err != nil {
				return nil, err
			}
			def.Checks = append(def.Checks, nodeName(expr.Type))
		case p.accept("foreign", "key"):
			cols, err := p.columnList()
			if err != nil {
				return nil, err
			}
			if err := p.expect("references", "after FOREIGN KEY columns"); err != nil {
				return nil, err
			}
			refTable, err := p.name("referenced table name")
			if err != nil {
				return nil, err
			}
			refCols, err := p.columnList()
			if err != nil {
				return nil, err
			}
			onDelete := "RESTRICT"
			if p.accept("on", "delete") {
				if p.accept("cascade") {
					onDelete = "CASCADE"
				} else {
					p.accept("restrict")
				}
			}
			def.ForeignKeys = append(def.ForeignKeys, relational.ForeignKeyDef{
				Cols: cols, RefTable: refTable, RefCols: refCols, OnDelete: onDelete,
			})
		case p.accept("unique"):
			cols, err := p.columnList()
			if err != nil {
				return nil, err
			}
			def.Uniques = append(def.Uniques, cols)
			secondary = append(secondary, cols)
		case p.accept("index"):
			cols, err := p.columnList()
			if err != nil {
				return nil, err
			}
			secondary = append(secondary, cols)
		default:
			col, err := p.name("a column name")
			if err != nil {
				return nil, err
			}
			colType, err := p.columnType()
			if err != nil {
				return nil, err
			}
			var defVal relational.Value
			for {
				if p.accept("default") {
					expr, err := p.expression()
					if err != nil {
						return nil, err
					}
					defVal, _ = evalExpr(relational.Record{}, expr)
				} else if p.accept("check") {
					if err := p.expect("(", "after CHECK"); err != nil {
						return nil, err
					}
					expr, err := p.expression()
					if err != nil {
						return nil, err
					}
					if err := p.expect(")", "after CHECK expression"); err != nil {
						return nil, err
					}
					def.Checks = append(def.Checks, nodeName(expr.Type))
				} else if p.accept("auto_increment") || p.accept("autoincrement") || p.accept("auto", "increment") {
					def.AutoIncCol = col
				} else if p.accept("unique") {
					def.Uniques = append(def.Uniques, []string{col})
					secondary = append(secondary, []string{col})
				} else {
					break
				}
			}
			def.Cols = append(def.Cols, col)
			def.Types = append(def.Types, colType)
			def.Defaults = append(def.Defaults, defVal)
		}
		if !p.accept(",") {
			break
		}
		// A trailing comma before the closing paren is allowed, matching the
		// way these definitions get edited.
		if p.peekWord(")") {
			break
		}
	}

	if err := p.expect(")", "to close the table definition"); err != nil {
		return nil, err
	}
	if primaryKey == nil {
		return nil, p.errorf("table %s has no primary key", name)
	}
	def.Indexes = append([][]string{primaryKey}, secondary...)
	return &CreateTable{IfNotExists: ifNotExists, Def: def}, nil
}

func (p *parser) columnType() (uint32, error) {
	tok := p.peek()
	if tok.kind != tokSymbol {
		return 0, p.errorf("expected a column type, found %s", tok.raw)
	}
	p.pos++
	switch tok.text {
	case "int64", "int", "integer", "bigint":
		return relational.TypeInt64, nil
	case "bytes", "string", "str", "text", "blob":
		return relational.TypeBytes, nil
	case "float64", "float", "double", "real":
		return relational.TypeFloat64, nil
	default:
		return 0, p.errorf("unknown column type %s (use int64, float64 or bytes)", tok.raw)
	}
}

// columnList parses a parenthesized list of column names.
func (p *parser) columnList() ([]string, error) {
	if err := p.expect("(", "before a column list"); err != nil {
		return nil, err
	}
	var cols []string
	for {
		col, err := p.name("a column name")
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
		if !p.accept(",") {
			break
		}
	}
	if err := p.expect(")", "to close the column list"); err != nil {
		return nil, err
	}
	return cols, nil
}

func (p *parser) dropTable() (*DropTable, error) {
	ifExists := p.accept("if", "exists")
	name, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	return &DropTable{IfExists: ifExists, Table: name}, nil
}

func (p *parser) createView() (*CreateView, error) {
	name, err := p.name("a view name")
	if err != nil {
		return nil, err
	}
	if err := p.expect("as", "after view name"); err != nil {
		return nil, err
	}
	queryStartPos := p.pos
	subStmt, err := p.statement()
	if err != nil {
		return nil, err
	}
	_, isSel := subStmt.(*Select)
	_, isUnion := subStmt.(*UnionStmt)
	_, isIntersect := subStmt.(*IntersectStmt)
	if !isSel && !isUnion && !isIntersect {
		return nil, p.errorf("VIEW query must be a SELECT statement")
	}

	var rawTokens []string
	for i := queryStartPos; i < p.pos; i++ {
		rawTokens = append(rawTokens, p.tokens[i].raw)
	}
	return &CreateView{Name: name, Query: strings.Join(rawTokens, " ")}, nil
}

func (p *parser) dropView() (*DropView, error) {
	name, err := p.name("a view name")
	if err != nil {
		return nil, err
	}
	return &DropView{Name: name}, nil
}

// selectStmt parses:
//
//	select [distinct] expr [as name], ... from t [index by ...] [filter ...] [limit a, b];
func (p *parser) selectStmt() (*Select, error) {
	stmt := &Select{Scan: newScan()}
	if p.accept("distinct") {
		stmt.Distinct = true
	}

	for {
		if p.accept("*") {
			stmt.Output = append(stmt.Output, Node{Type: NodeStar})
			stmt.Names = append(stmt.Names, "*")
		} else if tok := p.peek(); tok.kind == tokSymbol && strings.HasSuffix(tok.text, ".*") {
			tbl := strings.TrimSuffix(tok.raw, ".*")
			p.pos++
			stmt.Output = append(stmt.Output, Node{Type: NodeTableStar, Str: []byte(tbl)})
			stmt.Names = append(stmt.Names, tbl+".*")
		} else if tok := p.peek(); tok.kind == tokSymbol && strings.HasSuffix(tok.text, ".") && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].text == "*" {
			tbl := strings.TrimSuffix(tok.raw, ".")
			p.pos += 2
			stmt.Output = append(stmt.Output, Node{Type: NodeTableStar, Str: []byte(tbl)})
			stmt.Names = append(stmt.Names, tbl+".*")
		} else {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			name := ""
			if p.accept("as") {
				if name, err = p.name("an output column name"); err != nil {
					return nil, err
				}
			} else if expr.Type == NodeSym {
				name = string(expr.Str) // a bare column keeps its own name
			} else {
				name = fmt.Sprintf("col%d", len(stmt.Output)+1)
			}
			stmt.Output = append(stmt.Output, expr)
			stmt.Names = append(stmt.Names, name)
		}
		if !p.accept(",") {
			break
		}
	}

	if err := p.expect("from", "after the selected expressions"); err != nil {
		return nil, err
	}
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = table
	if err := p.scanClauses(&stmt.Scan); err != nil {
		return nil, err
	}

	if p.accept("group", "by") {
		for {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			stmt.GroupBy = append(stmt.GroupBy, expr)
			if !p.accept(",") {
				break
			}
		}
	}

	if p.accept("order", "by") {
		for {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			desc := false
			if p.accept("desc") {
				desc = true
			} else {
				p.accept("asc")
			}
			stmt.OrderBy = append(stmt.OrderBy, OrderItem{Expr: expr, Desc: desc})
			if !p.accept(",") {
				break
			}
		}
	}

	if p.accept("limit") {
		if err := p.limit(&stmt.Scan); err != nil {
			return nil, err
		}
	}
	if p.accept("offset") {
		off, err := p.integer()
		if err != nil {
			return nil, err
		}
		stmt.Scan.Offset = off
		if stmt.Scan.Limit == math.MaxInt64 && p.accept("limit") {
			lim, err := p.integer()
			if err != nil {
				return nil, err
			}
			stmt.Scan.Limit = lim
		}
	}

	return stmt, nil
}

func (p *parser) insertStmt(mode int) (*Insert, error) {
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	cols, err := p.columnList()
	if err != nil {
		return nil, err
	}
	if err := p.expect("values", "after the column list"); err != nil {
		return nil, err
	}

	stmt := &Insert{Table: table, Mode: mode, Cols: cols}
	for {
		if err := p.expect("(", "before a row of values"); err != nil {
			return nil, err
		}
		var row []Node
		for {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			row = append(row, expr)
			if !p.accept(",") {
				break
			}
		}
		if err := p.expect(")", "to close the row"); err != nil {
			return nil, err
		}
		if len(row) != len(cols) {
			return nil, p.errorf("row %d has %d values but %d columns were named",
				len(stmt.Values)+1, len(row), len(cols))
		}
		stmt.Values = append(stmt.Values, row)
		if !p.accept(",") {
			break
		}
	}
	return stmt, nil
}

func (p *parser) updateStmt() (*Update, error) {
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expect("set", "after the table name"); err != nil {
		return nil, err
	}

	stmt := &Update{Scan: newScan()}
	stmt.Table = table
	for {
		col, err := p.name("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expect("=", "in an assignment"); err != nil {
			return nil, err
		}
		expr, err := p.expression()
		if err != nil {
			return nil, err
		}
		stmt.Names = append(stmt.Names, col)
		stmt.Values = append(stmt.Values, expr)
		if !p.accept(",") {
			break
		}
	}
	if err := p.scanClauses(&stmt.Scan); err != nil {
		return nil, err
	}
	return stmt, nil
}

func (p *parser) deleteStmt() (*Delete, error) {
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	stmt := &Delete{Scan: newScan()}
	stmt.Table = table
	if err := p.scanClauses(&stmt.Scan); err != nil {
		return nil, err
	}
	return stmt, nil
}

// scanClauses parses the optional INDEX BY, FILTER and LIMIT clauses shared by
// SELECT, UPDATE and DELETE.
func (p *parser) scanClauses(scan *Scan) error {
	for {
		joinType := JoinInner
		if p.accept("left", "outer", "join") || p.accept("left", "join") {
			joinType = JoinLeft
		} else if p.accept("right", "outer", "join") || p.accept("right", "join") {
			joinType = JoinRight
		} else if p.accept("inner", "join") || p.accept("join") {
			joinType = JoinInner
		} else {
			break
		}
		rightTable, err := p.name("a table name to join")
		if err != nil {
			return err
		}
		if err := p.expect("on", "after JOIN table name"); err != nil {
			return err
		}
		onCond, err := p.expression()
		if err != nil {
			return err
		}
		scan.Joins = append(scan.Joins, Join{Type: joinType, Table: rightTable, On: onCond})
	}
	if p.accept("index", "by") {
		if err := p.indexBy(scan); err != nil {
			return err
		}
	}
	if p.accept("filter") || p.accept("where") {
		filter, err := p.expression()
		if err != nil {
			return err
		}
		scan.Filter = filter
	}
	if p.accept("limit") {
		if err := p.limit(scan); err != nil {
			return err
		}
	}
	if p.accept("offset") {
		off, err := p.integer()
		if err != nil {
			return err
		}
		scan.Offset = off
		if scan.Limit == math.MaxInt64 && p.accept("limit") {
			lim, err := p.integer()
			if err != nil {
				return err
			}
			scan.Limit = lim
		}
	}
	return nil
}

// indexBy parses one or two comparisons naming the same index.
//
//	index by a = 1
//	index by a > 1
//	index by a > 1 and a < 5     -- ascending
//	index by a < 5 and a > 1     -- descending
func (p *parser) indexBy(scan *Scan) error {
	// Parsed at the comparison level, not the full expression level: `and`
	// separates the two bounds here, so it must not be consumed as a boolean
	// operator the way it would be in FILTER.
	first, err := p.exprCompare()
	if err != nil {
		return err
	}
	if !first.isComparison() {
		return p.errorf("INDEX BY needs a comparison such as `a > 1`, found %s",
			nodeName(first.Type))
	}
	scan.Key1 = first

	if p.accept("and") {
		second, err := p.exprCompare()
		if err != nil {
			return err
		}
		if !second.isComparison() {
			return p.errorf("the second half of INDEX BY needs a comparison, found %s",
				nodeName(second.Type))
		}
		scan.Key2 = second
	}
	return nil
}

// limit parses `limit count`, `limit offset, count`, or `limit count offset offset`.
func (p *parser) limit(scan *Scan) error {
	first, err := p.integer()
	if err != nil {
		return err
	}
	if p.accept("offset") {
		second, err := p.integer()
		if err != nil {
			return err
		}
		scan.Limit, scan.Offset = first, second
		return nil
	}
	if p.accept(",") {
		second, err := p.integer()
		if err != nil {
			return err
		}
		scan.Offset, scan.Limit = first, second
		return nil
	}
	scan.Offset, scan.Limit = 0, first
	return nil
}

func (p *parser) integer() (int64, error) {
	tok := p.peek()
	if tok.kind != tokInt {
		return 0, p.errorf("expected a number, found %s", tok.raw)
	}
	p.pos++
	return tok.i64, nil
}

// ---------- expressions ----------
//
// One function per precedence level. Each parses a run of the next level down
// joined by its own operators, folding them left-associatively into a tree.

func (p *parser) expression() (Node, error) { return p.exprOr() }

// binaryLevel builds the parser for one precedence level from the operators it
// handles and the parser for the level below.
func (p *parser) binaryLevel(next func() (Node, error), ops map[string]int) (Node, error) {
	node, err := next()
	if err != nil {
		return Node{}, err
	}
	for {
		matched := 0
		for word, kind := range ops {
			if p.accept(word) {
				matched = kind
				break
			}
		}
		if matched == 0 {
			return node, nil
		}
		right, err := next()
		if err != nil {
			return Node{}, err
		}
		// Left-associative: the tree built so far becomes the left operand.
		node = Node{Type: matched, Kids: []Node{node, right}}
	}
}

func (p *parser) exprOr() (Node, error) {
	return p.binaryLevel(p.exprAnd, map[string]int{"or": NodeOr})
}

func (p *parser) exprAnd() (Node, error) {
	return p.binaryLevel(p.exprNot, map[string]int{"and": NodeAnd})
}

func (p *parser) exprNot() (Node, error) {
	if p.accept("not") {
		operand, err := p.exprNot()
		if err != nil {
			return Node{}, err
		}
		return Node{Type: NodeNot, Kids: []Node{operand}}, nil
	}
	return p.exprCompare()
}

func (p *parser) exprCompare() (Node, error) {
	left, err := p.exprAdd()
	if err != nil {
		return Node{}, err
	}
	if p.accept("is", "not", "null") {
		return Node{Type: NodeIsNotNull, Kids: []Node{left}}, nil
	}
	if p.accept("is", "null") {
		return Node{Type: NodeIsNull, Kids: []Node{left}}, nil
	}
	if p.accept("like") {
		pattern, err := p.exprAdd()
		if err != nil {
			return Node{}, err
		}
		return Node{Type: NodeLike, Kids: []Node{left, pattern}}, nil
	}
	if p.accept("between") {
		low, err := p.exprAdd()
		if err != nil {
			return Node{}, err
		}
		if err := p.expect("and", "in BETWEEN expression"); err != nil {
			return Node{}, err
		}
		high, err := p.exprAdd()
		if err != nil {
			return Node{}, err
		}
		return Node{Type: NodeBetween, Kids: []Node{left, low, high}}, nil
	}
	isNotIn := p.accept("not", "in")
	isIn := !isNotIn && p.accept("in")
	if isIn || isNotIn {
		if err := p.expect("(", "before IN list"); err != nil {
			return Node{}, err
		}
		var kids []Node
		kids = append(kids, left)
		kind := NodeIn
		if isNotIn {
			kind = NodeNotIn
		}
		if p.accept("select") {
			subSel, err := p.selectStmt()
			if err != nil {
				return Node{}, err
			}
			if err := p.expect(")", "to close subquery IN list"); err != nil {
				return Node{}, err
			}
			return Node{Type: kind, Kids: kids, Sub: subSel}, nil
		}
		for {
			item, err := p.expression()
			if err != nil {
				return Node{}, err
			}
			kids = append(kids, item)
			if !p.accept(",") {
				break
			}
		}
		if err := p.expect(")", "after IN list"); err != nil {
			return Node{}, err
		}
		return Node{Type: kind, Kids: kids}, nil
	}
	ops := map[string]int{"=": NodeEq, "!=": NodeNE, "<": NodeLT, "<=": NodeLE, ">": NodeGT, ">=": NodeGE}
	for word, kind := range ops {
		if p.accept(word) {
			right, err := p.exprAdd()
			if err != nil {
				return Node{}, err
			}
			return Node{Type: kind, Kids: []Node{left, right}}, nil
		}
	}
	return left, nil
}

func (p *parser) exprAdd() (Node, error) {
	return p.binaryLevel(p.exprMul, map[string]int{"+": NodeAdd, "-": NodeSub})
}

func (p *parser) exprMul() (Node, error) {
	return p.binaryLevel(p.exprUnary, map[string]int{"*": NodeMul, "/": NodeDiv, "%": NodeMod})
}

func (p *parser) exprUnary() (Node, error) {
	if p.accept("-") {
		operand, err := p.exprUnary()
		if err != nil {
			return Node{}, err
		}
		// Fold a negated literal now, so `-9223372036854775808` is
		// representable rather than overflowing as a positive literal first.
		if operand.Type == NodeInt {
			return Node{Type: NodeInt, I64: -operand.I64}, nil
		}
		return Node{Type: NodeNeg, Kids: []Node{operand}}, nil
	}
	if p.accept("+") {
		return p.exprUnary()
	}
	return p.exprAtom()
}

// exprAtom parses the indivisible pieces: literals, column names, and
// parenthesized expressions or tuples.
func (p *parser) exprAtom() (Node, error) {
	tok := p.peek()
	switch {
	case tok.kind == tokInt:
		p.pos++
		return Node{Type: NodeInt, I64: tok.i64}, nil

	case tok.kind == tokFloat:
		p.pos++
		return Node{Type: NodeFloat, F64: tok.f64}, nil

	case tok.kind == tokString:
		p.pos++
		return Node{Type: NodeStr, Str: tok.str}, nil

	case p.accept("null"):
		return Node{Type: NodeNull}, nil

	case p.accept("count", "(", "*", ")"):
		return Node{Type: NodeCountStar}, nil

	case p.accept("count", "("):
		arg, err := p.expression()
		if err != nil {
			return Node{}, err
		}
		if err := p.expect(")", "to close COUNT(...)"); err != nil {
			return Node{}, err
		}
		return Node{Type: NodeCount, Kids: []Node{arg}}, nil

	case p.accept("sum", "("):
		arg, err := p.expression()
		if err != nil {
			return Node{}, err
		}
		if err := p.expect(")", "to close SUM(...)"); err != nil {
			return Node{}, err
		}
		return Node{Type: NodeSum, Kids: []Node{arg}}, nil

	case p.accept("avg", "("):
		arg, err := p.expression()
		if err != nil {
			return Node{}, err
		}
		if err := p.expect(")", "to close AVG(...)"); err != nil {
			return Node{}, err
		}
		return Node{Type: NodeAvg, Kids: []Node{arg}}, nil

	case p.accept("min", "("):
		arg, err := p.expression()
		if err != nil {
			return Node{}, err
		}
		if err := p.expect(")", "to close MIN(...)"); err != nil {
			return Node{}, err
		}
		return Node{Type: NodeMin, Kids: []Node{arg}}, nil

	case p.accept("max", "("):
		arg, err := p.expression()
		if err != nil {
			return Node{}, err
		}
		if err := p.expect(")", "to close MAX(...)"); err != nil {
			return Node{}, err
		}
		return Node{Type: NodeMax, Kids: []Node{arg}}, nil

	case p.accept("("):
		first, err := p.expression()
		if err != nil {
			return Node{}, err
		}
		if !p.peekWord(",") {
			if err := p.expect(")", "to close a parenthesized expression"); err != nil {
				return Node{}, err
			}
			return first, nil
		}
		// A comma makes it a tuple, which is how a multi-column index bound is
		// written: `index by (a, b) > (1, 2)`.
		items := []Node{first}
		for p.accept(",") {
			item, err := p.expression()
			if err != nil {
				return Node{}, err
			}
			items = append(items, item)
		}
		if err := p.expect(")", "to close a tuple"); err != nil {
			return Node{}, err
		}
		return Node{Type: NodeTuple, Kids: items}, nil

	case tok.kind == tokSymbol && !reservedWords[tok.text]:
		p.pos++
		return Node{Type: NodeSym, Str: []byte(tok.raw)}, nil

	default:
		return Node{}, p.errorf("expected a value, a column name or `(`, found %s", tok.raw)
	}
}

// MaxLimit is the limit used when a statement gives none.
const MaxLimit = math.MaxInt64
