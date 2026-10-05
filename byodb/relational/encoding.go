package relational

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"byodb/btree"
)

// Keys are encoded so that comparing the encoded bytes gives the same answer
// as comparing the values they represent. That is what lets a B+tree — which
// knows nothing about types — serve typed range queries: no comparison
// callback, no decoding during a scan, just bytes.Compare.
//
// Three things have to hold:
//
//   - Integers must order as integers, not as their two's-complement bytes.
//   - A multi-column key must order column by column, which means no column's
//     encoding may be a prefix of another's in a way that shifts the boundary.
//   - A query may constrain only the leading columns of an index, so there
//     must be a way to write "everything above this prefix" and "everything
//     below it".

// encodeValues appends the order-preserving encoding of each value.
//
// Every column is tagged with its type first. The tag makes a hexdump
// readable, and — because no tag is 0xff — it also guarantees that no encoded
// column can start with 0xff, which is what reserves 0xff as a usable
// "greater than anything" marker.
func encodeValues(out []byte, vals []Value) []byte {
	for _, v := range vals {
		out = append(out, byte(v.Type))
		switch v.Type {
		case TypeInt64:
			var buf [8]byte
			// Big-endian puts the most significant byte first, so byte-wise
			// comparison matches numeric comparison. Flipping the sign bit
			// then moves the negative half below the positive half, which
			// two's complement would otherwise place above it.
			binary.BigEndian.PutUint64(buf[:], uint64(v.I64)+(1<<63))
			out = append(out, buf[:]...)
		case TypeFloat64:
			var buf [8]byte
			bits := math.Float64bits(v.F64)
			if (bits & (1 << 63)) != 0 {
				bits = ^bits // negative float: flip all bits so magnitude DECREASES uint64 value
			} else {
				bits ^= (1 << 63) // positive float: flip sign bit so it sits above negatives
			}
			binary.BigEndian.PutUint64(buf[:], bits)
			out = append(out, buf[:]...)
		case TypeBytes:
			// Null-terminated so that ("a","bc") and ("ab","c") cannot encode
			// to the same bytes, with nulls inside the data escaped so the
			// terminator stays unambiguous.
			out = append(out, escapeString(v.Str)...)
			out = append(out, 0)
		case TypeNull:
			// Tag only, no additional bytes
		default:
			panic(fmt.Sprintf("relational: cannot encode a value of type %d", v.Type))
		}
	}
	return out
}

// decodeValues reads back what encodeValues wrote, filling in `out` whose
// entries already carry the expected types.
func decodeValues(in []byte, out []Value) error {
	pos := 0
	for i := range out {
		if pos >= len(in) {
			out[i] = Value{Type: TypeNull}
			continue
		}
		tag := uint32(in[pos])
		pos++
		if tag == TypeNull {
			out[i] = Value{Type: TypeNull}
			continue
		}
		if tag != out[i].Type {
			return fmt.Errorf("%w: column %d is encoded as %s but the schema says %s",
				ErrBadRecord, i, TypeName(tag), TypeName(out[i].Type))
		}
		switch tag {
		case TypeInt64:
			if pos+8 > len(in) {
				return fmt.Errorf("%w: truncated integer in column %d", ErrBadRecord, i)
			}
			out[i].I64 = int64(binary.BigEndian.Uint64(in[pos:pos+8]) - (1 << 63))
			pos += 8
		case TypeFloat64:
			if pos+8 > len(in) {
				return fmt.Errorf("%w: truncated float64 in column %d", ErrBadRecord, i)
			}
			bits := binary.BigEndian.Uint64(in[pos : pos+8])
			if (bits & (1 << 63)) != 0 {
				bits ^= (1 << 63)
			} else {
				bits = ^bits
			}
			out[i].F64 = math.Float64frombits(bits)
			pos += 8
		case TypeBytes:
			end := bytes.IndexByte(in[pos:], 0)
			if end < 0 {
				return fmt.Errorf("%w: unterminated string in column %d", ErrBadRecord, i)
			}
			out[i].Str = unescapeString(in[pos : pos+end])
			pos += end + 1
		default:
			return fmt.Errorf("%w: unrecognized type tag %d", ErrBadRecord, tag)
		}
	}
	return nil
}

// escapeString removes null bytes from the data so a null can terminate it.
//
// The substitutions keep sort order: 0x00 and 0x01 map to two-byte sequences
// starting with 0x01, which still sort below every other byte and below each
// other in the same relative order.
func escapeString(in []byte) []byte {
	if bytes.IndexByte(in, 0x00) < 0 && bytes.IndexByte(in, 0x01) < 0 {
		return in
	}
	out := make([]byte, 0, len(in)+8)
	for _, b := range in {
		switch b {
		case 0x00:
			out = append(out, 0x01, 0x01)
		case 0x01:
			out = append(out, 0x01, 0x02)
		default:
			out = append(out, b)
		}
	}
	return out
}

func unescapeString(in []byte) []byte {
	if !bytes.Contains(in, []byte{0x01}) {
		return append([]byte(nil), in...)
	}
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == 0x01 && i+1 < len(in) {
			i++
			out = append(out, in[i]-1)
			continue
		}
		out = append(out, in[i])
	}
	return out
}

// encodeKey builds a B+tree key: the index's prefix followed by its columns.
//
// The prefix is big-endian so that prefixes themselves sort numerically, which
// keeps each table's and index's keys in one contiguous stretch of the tree.
func encodeKey(out []byte, prefix uint32, vals []Value) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], prefix)
	out = append(out, buf[:]...)
	return encodeValues(out, vals)
}

// encodeKeyPartial builds a range boundary from a query that constrains only
// the leading columns of an index.
//
// A query `a > 1` against an index on (a, b) means "(a, b) > (1, +∞)": every
// row with a == 1 is excluded whatever its b. Conversely `a >= 1` means
// "(a, b) > (1, -∞)". So the missing columns become an infinity, and which
// infinity depends on the operator:
//
//	a >  1  ->  (a, b) >  (1, +inf)
//	a >= 1  ->  (a, b) >  (1, -inf)
//	a <= 1  ->  (a, b) <  (1, +inf)
//	a <  1  ->  (a, b) <  (1, -inf)
//
// -∞ is the empty string, since no column encodes to nothing; +∞ is 0xff,
// which no encoded column can begin with because every column starts with its
// type tag.
func encodeKeyPartial(out []byte, prefix uint32, vals []Value, cmp int) []byte {
	out = encodeKey(out, prefix, vals)
	if cmp == btree.CmpGT || cmp == btree.CmpLE {
		out = append(out, 0xff)
	}
	return out
}
