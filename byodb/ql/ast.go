// Package ql is the query language: a parser and an interpreter for a
// SQL-like syntax over the relational layer.
//
// The language deliberately splits what SQL puts in one WHERE clause into two:
//
//	INDEX BY   chooses the index and the range to scan
//	FILTER     tests each row the scan produces
//
// A real database infers that split with a query planner. Making it explicit
// costs a little syntax and buys predictable performance — the query does what
// it says, and cannot silently change plan as the data grows.
package ql

import (
	"fmt"
	"math"

	"byodb/relational"
)

// Node types. An expression is a tree of these.
const (
	NodeUnset = iota

	// Leaves.
	NodeSym   // a column name
	NodeInt   // an integer literal
	NodeFloat // a floating point literal
	NodeStr   // a string literal
	NodeNull  // a NULL literal

	// Boolean operators, lowest precedence first.
	NodeOr
	NodeAnd
	NodeNot

	// Comparisons & NULL checks.
	NodeEq
	NodeNE
	NodeLT
	NodeLE
	NodeGT
	NodeGE
	NodeIsNull
	NodeIsNotNull

	// Arithmetic.
	NodeAdd
	NodeSub
	NodeMul
	NodeDiv
	NodeMod
	NodeNeg

	// A parenthesized list, used for multi-column index bounds.
	NodeTuple

	// Operators & Functions.
	NodeLike
	NodeBetween
	NodeIn
	NodeNotIn
	NodeSubquery

	// Wildcards.
	NodeStar
	NodeTableStar

	// Aggregates.
	NodeCountStar
	NodeCount
	NodeSum
	NodeAvg
	NodeMin
	NodeMax
)

const (
	JoinInner = iota
	JoinLeft
	JoinRight
)

var nodeNames = map[int]string{
	NodeSym: "column", NodeInt: "integer", NodeFloat: "float", NodeStr: "string", NodeNull: "NULL",
	NodeStar: "*", NodeTableStar: "table.*",
	NodeOr: "OR", NodeAnd: "AND", NodeNot: "NOT",
	NodeEq: "=", NodeNE: "!=", NodeLT: "<", NodeLE: "<=", NodeGT: ">", NodeGE: ">=",
	NodeIsNull: "IS NULL", NodeIsNotNull: "IS NOT NULL",
	NodeLike: "LIKE", NodeBetween: "BETWEEN", NodeIn: "IN", NodeNotIn: "NOT IN", NodeSubquery: "SUBQUERY",
	NodeAdd: "+", NodeSub: "-", NodeMul: "*", NodeDiv: "/", NodeMod: "%", NodeNeg: "-",
	NodeTuple: "tuple", NodeCountStar: "COUNT(*)",
	NodeCount: "COUNT", NodeSum: "SUM", NodeAvg: "AVG", NodeMin: "MIN", NodeMax: "MAX",
}

func nodeName(t int) string {
	if name, ok := nodeNames[t]; ok {
		return name
	}
	return fmt.Sprintf("node(%d)", t)
}

// Node is one expression node: a tagged union with operands.
type Node struct {
	Type int
	I64  int64
	F64  float64
	Str  []byte
	Kids []Node
	Sub  any // *Select for subqueries
}

func (n Node) isSet() bool { return n.Type != NodeUnset }

// isComparison reports whether the node is one of the six comparison
// operators, which are the only forms INDEX BY accepts.
func (n Node) isComparison() bool {
	switch n.Type {
	case NodeEq, NodeNE, NodeLT, NodeLE, NodeGT, NodeGE:
		return true
	}
	return false
}

// OrderItem describes an item in an ORDER BY clause.
type OrderItem struct {
	Expr Node
	Desc bool
}

// Join describes a JOIN clause.
type Join struct {
	Type  int    // JoinInner, JoinLeft, JoinRight
	Table string // Right table name
	On    Node   // JOIN ON predicate
}

// Scan is what SELECT, UPDATE and DELETE have in common: which rows to act on.
type Scan struct {
	Table  string
	Joins  []Join // Optional JOIN clauses
	Key1   Node   // the INDEX BY lower or starting bound
	Key2   Node   // the INDEX BY upper or ending bound
	Filter Node   // an arbitrary predicate applied to each row
	Offset int64
	Limit  int64
}

func newScan() Scan { return Scan{Offset: 0, Limit: math.MaxInt64} }

// Statements.
type (
	// Select projects expressions over the rows a scan produces.
	Select struct {
		Scan
		Distinct bool
		Names    []string    // output column names, from AS or derived
		Output   []Node
		GroupBy  []Node      // GROUP BY clause items
		OrderBy  []OrderItem // ORDER BY clause items
	}

	// Set operators.
	UnionStmt struct {
		Left  any
		Right any
		All   bool
	}

	IntersectStmt struct {
		Left  any
		Right any
	}

	// Update assigns expressions to columns of the rows a scan produces.
	Update struct {
		Scan
		Names  []string
		Values []Node
	}

	// Delete removes the rows a scan produces.
	Delete struct {
		Scan
	}

	// Insert adds literal rows. Mode distinguishes INSERT from REPLACE.
	Insert struct {
		Table  string
		Mode   int // relational insert mode
		Cols   []string
		Values [][]Node
	}

	// CreateTable defines a new table.
	CreateTable struct {
		IfNotExists bool
		Def         *relational.TableDef
	}

	// DropTable removes a table and everything in it.
	DropTable struct {
		IfExists bool
		Table    string
	}

	// AlterTable adds a column to an existing table.
	AlterTable struct {
		Table string
		Col   string
		Type  uint32
	}

	// Transaction statements.
	BeginStmt    struct{}
	CommitStmt   struct{}
	RollbackStmt struct{}

	// View statements.
	CreateView struct {
		Name  string
		Query string
	}
	DropView struct {
		Name string
	}
)


