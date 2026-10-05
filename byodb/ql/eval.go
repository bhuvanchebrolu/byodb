package ql

import (
	"fmt"
	"regexp"

	"byodb/relational"
)

// Evaluating an expression is a walk over its tree: each node asks its
// operands for values and combines them. Because the tree already encodes
// precedence and associativity, nothing here needs to know either.

// EvalError reports a problem in an expression, as opposed to a problem in the
// syntax.
type EvalError struct{ Msg string }

func (e *EvalError) Error() string { return e.Msg }

func evalErrorf(format string, args ...any) error {
	return &EvalError{Msg: fmt.Sprintf(format, args...)}
}

// evalExpr computes a value from an expression and a row.
//
// The row supplies column values; an empty row means the expression must be
// made of literals, which is what INSERT requires.
func evalExpr(env relational.Record, node Node) (relational.Value, error) {
	switch node.Type {
	case NodeUnset:
		return relational.Value{}, evalErrorf("empty expression")

	case NodeInt:
		return relational.Int64(node.I64), nil
	case NodeFloat:
		return relational.Float64(node.F64), nil
	case NodeStr:
		return relational.Bytes(node.Str), nil
	case NodeNull:
		return relational.Null(), nil

	case NodeIsNull:
		val, err := evalExpr(env, node.Kids[0])
		if err != nil {
			return relational.Value{}, err
		}
		return boolValue(val.Type == relational.TypeNull), nil

	case NodeIsNotNull:
		val, err := evalExpr(env, node.Kids[0])
		if err != nil {
			return relational.Value{}, err
		}
		return boolValue(val.Type != relational.TypeNull), nil

	case NodeSym:
		value := env.Get(string(node.Str))
		if value == nil {
			return relational.Value{}, evalErrorf("unknown column: %s", node.Str)
		}
		return *value, nil

	case NodeNeg:
		operand, err := evalExpr(env, node.Kids[0])
		if err != nil {
			return relational.Value{}, err
		}
		if operand.Type == relational.TypeInt64 {
			return relational.Int64(-operand.I64), nil
		}
		if operand.Type == relational.TypeFloat64 {
			return relational.Float64(-operand.F64), nil
		}
		return relational.Value{}, evalErrorf("cannot negate a %s value",
			relational.TypeName(operand.Type))

	case NodeNot:
		operand, err := evalExpr(env, node.Kids[0])
		if err != nil {
			return relational.Value{}, err
		}
		return boolValue(!truthy(operand)), nil

	case NodeAnd, NodeOr:
		left, err := evalExpr(env, node.Kids[0])
		if err != nil {
			return relational.Value{}, err
		}
		// Short-circuit, so `a != 0 and 100 / a > 2` is safe to write.
		if node.Type == NodeAnd && !truthy(left) {
			return boolValue(false), nil
		}
		if node.Type == NodeOr && truthy(left) {
			return boolValue(true), nil
		}
		right, err := evalExpr(env, node.Kids[1])
		if err != nil {
			return relational.Value{}, err
		}
		return boolValue(truthy(right)), nil

	case NodeLike:
		return evalLike(env, node)
	case NodeBetween:
		return evalBetween(env, node)
	case NodeIn, NodeNotIn:
		return evalIn(env, node)

	case NodeEq, NodeNE, NodeLT, NodeLE, NodeGT, NodeGE:
		return evalComparison(env, node)

	case NodeAdd, NodeSub, NodeMul, NodeDiv, NodeMod:
		return evalArithmetic(env, node)

	case NodeTuple:
		return relational.Value{}, evalErrorf(
			"a tuple can only appear as an INDEX BY bound, not in an expression")

	default:
		return relational.Value{}, evalErrorf("cannot evaluate %s", nodeName(node.Type))
	}
}

func evalComparison(env relational.Record, node Node) (relational.Value, error) {
	left, err := evalExpr(env, node.Kids[0])
	if err != nil {
		return relational.Value{}, err
	}
	right, err := evalExpr(env, node.Kids[1])
	if err != nil {
		return relational.Value{}, err
	}

	if left.Type == relational.TypeNull || right.Type == relational.TypeNull {
		if node.Type == NodeEq {
			return boolValue(left.Type == right.Type), nil
		}
		if node.Type == NodeNE {
			return boolValue(left.Type != right.Type), nil
		}
		return boolValue(false), nil
	}

	if left.Type != right.Type {
		return relational.Value{}, evalErrorf("cannot compare %s with %s",
			relational.TypeName(left.Type), relational.TypeName(right.Type))
	}

	var sign int
	if left.Type == relational.TypeInt64 {
		switch {
		case left.I64 < right.I64:
			sign = -1
		case left.I64 > right.I64:
			sign = +1
		}
	} else if left.Type == relational.TypeFloat64 {
		switch {
		case left.F64 < right.F64:
			sign = -1
		case left.F64 > right.F64:
			sign = +1
		}
	} else {
		switch a, b := string(left.Str), string(right.Str); {
		case a < b:
			sign = -1
		case a > b:
			sign = +1
		}
	}

	switch node.Type {
	case NodeEq:
		return boolValue(sign == 0), nil
	case NodeNE:
		return boolValue(sign != 0), nil
	case NodeLT:
		return boolValue(sign < 0), nil
	case NodeLE:
		return boolValue(sign <= 0), nil
	case NodeGT:
		return boolValue(sign > 0), nil
	default: // NodeGE
		return boolValue(sign >= 0), nil
	}
}

func evalArithmetic(env relational.Record, node Node) (relational.Value, error) {
	left, err := evalExpr(env, node.Kids[0])
	if err != nil {
		return relational.Value{}, err
	}
	right, err := evalExpr(env, node.Kids[1])
	if err != nil {
		return relational.Value{}, err
	}

	// Concatenation is the one arithmetic operator that makes sense on
	// strings; the rest are integers only.
	if node.Type == NodeAdd && left.Type == relational.TypeBytes && right.Type == relational.TypeBytes {
		joined := make([]byte, 0, len(left.Str)+len(right.Str))
		joined = append(joined, left.Str...)
		joined = append(joined, right.Str...)
		return relational.Bytes(joined), nil
	}

	if left.Type == relational.TypeFloat64 || right.Type == relational.TypeFloat64 {
		lVal, rVal := toFloat(left), toFloat(right)
		switch node.Type {
		case NodeAdd:
			return relational.Float64(lVal + rVal), nil
		case NodeSub:
			return relational.Float64(lVal - rVal), nil
		case NodeMul:
			return relational.Float64(lVal * rVal), nil
		case NodeDiv:
			if rVal == 0 {
				return relational.Value{}, evalErrorf("division by zero")
			}
			return relational.Float64(lVal / rVal), nil
		default:
			return relational.Value{}, evalErrorf("cannot apply %s to float64", nodeName(node.Type))
		}
	}

	if left.Type != relational.TypeInt64 || right.Type != relational.TypeInt64 {
		return relational.Value{}, evalErrorf("cannot apply %s to %s and %s",
			nodeName(node.Type), relational.TypeName(left.Type), relational.TypeName(right.Type))
	}

	switch node.Type {
	case NodeAdd:
		return relational.Int64(left.I64 + right.I64), nil
	case NodeSub:
		return relational.Int64(left.I64 - right.I64), nil
	case NodeMul:
		return relational.Int64(left.I64 * right.I64), nil
	case NodeDiv, NodeMod:
		if right.I64 == 0 {
			return relational.Value{}, evalErrorf("division by zero")
		}
		// Go panics on MinInt64 / -1 rather than wrapping, so it is reported
		// as the overflow it is.
		if left.I64 == -1<<63 && right.I64 == -1 {
			return relational.Value{}, evalErrorf("integer overflow in %s", nodeName(node.Type))
		}
		if node.Type == NodeDiv {
			return relational.Int64(left.I64 / right.I64), nil
		}
		return relational.Int64(left.I64 % right.I64), nil
	default:
		return relational.Value{}, evalErrorf("cannot apply %s", nodeName(node.Type))
	}
}

func toFloat(v relational.Value) float64 {
	if v.Type == relational.TypeFloat64 {
		return v.F64
	}
	if v.Type == relational.TypeInt64 {
		return float64(v.I64)
	}
	return 0
}

// truthy defines what counts as true. There is no boolean type, so a
// comparison yields 0 or 1 and any non-zero integer or non-empty string is
// true.
func truthy(v relational.Value) bool {
	if v.Type == relational.TypeInt64 {
		return v.I64 != 0
	}
	if v.Type == relational.TypeFloat64 {
		return v.F64 != 0
	}
	return len(v.Str) > 0
}

func boolValue(b bool) relational.Value {
	if b {
		return relational.Int64(1)
	}
	return relational.Int64(0)
}

func evalLike(env relational.Record, node Node) (relational.Value, error) {
	val, err := evalExpr(env, node.Kids[0])
	if err != nil {
		return relational.Value{}, err
	}
	pattern, err := evalExpr(env, node.Kids[1])
	if err != nil {
		return relational.Value{}, err
	}
	if val.Type == relational.TypeNull || pattern.Type == relational.TypeNull {
		return boolValue(false), nil
	}
	s := string(val.Str)
	p := string(pattern.Str)
	matched := matchLikePattern(s, p)
	return boolValue(matched), nil
}

func matchLikePattern(s, pattern string) bool {
	var regexPattern string
	for _, ch := range pattern {
		switch ch {
		case '%':
			regexPattern += ".*"
		case '_':
			regexPattern += "."
		case '.', '+', '*', '?', '^', '$', '(', ')', '[', ']', '{', '}', '|', '\\':
			regexPattern += "\\" + string(ch)
		default:
			regexPattern += string(ch)
		}
	}
	re, err := regexp.Compile("(?i)^" + regexPattern + "$")
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func evalBetween(env relational.Record, node Node) (relational.Value, error) {
	val, err := evalExpr(env, node.Kids[0])
	if err != nil {
		return relational.Value{}, err
	}
	low, err := evalExpr(env, node.Kids[1])
	if err != nil {
		return relational.Value{}, err
	}
	high, err := evalExpr(env, node.Kids[2])
	if err != nil {
		return relational.Value{}, err
	}
	if val.Type == relational.TypeNull || low.Type == relational.TypeNull || high.Type == relational.TypeNull {
		return boolValue(false), nil
	}
	ge := compareValues(val, low) >= 0
	le := compareValues(val, high) <= 0
	return boolValue(ge && le), nil
}

func evalIn(env relational.Record, node Node) (relational.Value, error) {
	val, err := evalExpr(env, node.Kids[0])
	if err != nil {
		return relational.Value{}, err
	}
	if val.Type == relational.TypeNull {
		return boolValue(false), nil
	}

	found := false
	for _, kid := range node.Kids[1:] {
		target, err := evalExpr(env, kid)
		if err == nil && target.Type != relational.TypeNull && compareValues(val, target) == 0 {
			found = true
			break
		}
	}

	if node.Type == NodeNotIn {
		return boolValue(!found), nil
	}
	return boolValue(found), nil
}

