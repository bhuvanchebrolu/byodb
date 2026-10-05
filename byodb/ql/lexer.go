package ql

import (
	"fmt"
	"strconv"
	"strings"
)

// Token kinds.
const (
	tokEOF = iota
	tokSymbol
	tokInt
	tokFloat
	tokString
	tokOperator
)

type token struct {
	kind int
	text string // the literal text, lowercased for symbols
	raw  string // the text as written, for error messages
	i64  int64
	f64  float64
	str  []byte
	pos  int
}

// operators, longest first so that "<=" is not read as "<" then "=".
var operators = []string{"<=", ">=", "!=", "<>", "=", "<", ">", "+", "-", "*", "/", "%", "(", ")", ",", ";"}

// ParseError reports where in the input the problem is, which matters more
// than usual for a language people type by hand.
type ParseError struct {
	Msg   string
	Pos   int
	Input string
}

func (e *ParseError) Error() string {
	// Show the offending position on its own line.
	line := e.Input
	col := e.Pos
	if col > len(line) {
		col = len(line)
	}
	return fmt.Sprintf("%s\n  %s\n  %s^", e.Msg, line, strings.Repeat(" ", col))
}

// tokenize splits the input into tokens.
//
// Whitespace and `--` comments are dropped; string literals use single quotes,
// with a doubled quote standing for one inside them.
func tokenize(input string) ([]token, error) {
	var out []token
	i := 0
	fail := func(pos int, format string, args ...any) ([]token, error) {
		return nil, &ParseError{Msg: fmt.Sprintf(format, args...), Pos: pos, Input: input}
	}

	for i < len(input) {
		c := input[i]

		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '-' && i+1 < len(input) && input[i+1] == '-':
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}

		start := i
		switch {
		case isSymbolStart(c):
			for i < len(input) && isSymbolChar(input[i]) {
				i++
			}
			raw := input[start:i]
			out = append(out, token{kind: tokSymbol, text: strings.ToLower(raw), raw: raw, pos: start})

		case (c >= '0' && c <= '9') || (c == '.' && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9'):
			isFloat := false
			for i < len(input) && ((input[i] >= '0' && input[i] <= '9') || input[i] == '.') {
				if input[i] == '.' {
					isFloat = true
				}
				i++
			}
			raw := input[start:i]
			if isFloat {
				value, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					return fail(start, "the number %s does not fit in a 64-bit float", raw)
				}
				out = append(out, token{kind: tokFloat, text: raw, raw: raw, f64: value, pos: start})
			} else {
				value, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					return fail(start, "the number %s does not fit in a 64-bit integer", raw)
				}
				out = append(out, token{kind: tokInt, text: raw, raw: raw, i64: value, pos: start})
			}

		case c == '\'':
			i++
			var sb strings.Builder
			closed := false
			for i < len(input) {
				if input[i] == '\'' {
					if i+1 < len(input) && input[i+1] == '\'' {
						sb.WriteByte('\'') // '' is an escaped quote
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				sb.WriteByte(input[i])
				i++
			}
			if !closed {
				return fail(start, "unterminated string literal")
			}
			out = append(out, token{kind: tokString, text: sb.String(), raw: input[start:i],
				str: []byte(sb.String()), pos: start})

		default:
			matched := ""
			for _, op := range operators {
				if strings.HasPrefix(input[i:], op) {
					matched = op
					break
				}
			}
			if matched == "" {
				return fail(start, "unexpected character %q", string(c))
			}
			i += len(matched)
			if matched == "<>" {
				matched = "!=" // one spelling from here on
			}
			out = append(out, token{kind: tokOperator, text: matched, raw: matched, pos: start})
		}
	}

	out = append(out, token{kind: tokEOF, text: "", raw: "end of input", pos: len(input)})
	return out, nil
}

func isSymbolStart(c byte) bool {
	return c == '_' || c == '@' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSymbolChar(c byte) bool {
	return isSymbolStart(c) || (c >= '0' && c <= '9') || c == '.'
}
