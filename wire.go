package protobuf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const maxMessageDepth = 100

type message struct {
	// Immutable after construction of the message graph.
	typ      reflect.Type
	fields   []field
	byNumber map[protowire.Number]int
}

func (m *message) append(dst []byte, v reflect.Value, depth int) ([]byte, error) {
	if depth > maxMessageDepth {
		return dst, fmt.Errorf("protobuf: message nesting exceeds %d", maxMessageDepth)
	}
	b := dst
	for i := range m.fields {
		f := &m.fields[i]
		var err error
		b, err = f.append(b, v.Field(f.index), depth)
		if err != nil {
			return dst, fmt.Errorf("protobuf: %v.%s: %w", m.typ, f.name, err)
		}
	}
	return b, nil
}

func (m *message) unmarshal(data []byte, dst reflect.Value) error {
	v := reflect.New(m.typ).Elem()
	if _, err := m.consume(data, v, 0, nil); err != nil {
		return err
	}
	dst.Set(v)
	return nil
}

// consume decodes into a struct. When only is set, v holds a single field
// and the result reports whether the selected tag appeared.
func (m *message) consume(b []byte, v reflect.Value, depth int, only *field) (bool, error) {
	if depth > maxMessageDepth {
		return false, fmt.Errorf("protobuf: message nesting exceeds %d", maxMessageDepth)
	}
	var present bool
	for len(b) != 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false, fmt.Errorf("protobuf: %v: %w", m.typ, protowire.ParseError(n))
		}
		if !num.IsValid() {
			return false, fmt.Errorf("protobuf: %v: invalid field number %d", m.typ, num)
		}
		b = b[n:]
		var f *field
		dst := v
		if only == nil {
			if i, ok := m.byNumber[num]; ok {
				f = &m.fields[i]
				dst = v.Field(f.index)
			}
		} else if num == only.number {
			f = only
		}
		if f == nil {
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return false, fmt.Errorf("protobuf: %v: field %d: %w", m.typ, num, protowire.ParseError(n))
			}
			b = b[n:]
			continue
		}
		n, err := f.consume(b, typ, dst, depth)
		if err != nil {
			return false, fmt.Errorf("protobuf: %v.%s: %w", m.typ, f.name, err)
		}
		present = true
		b = b[n:]
	}
	return present, nil
}

func (m *message) writeSchema(b *strings.Builder, seen map[*message]struct{}) {
	if _, ok := seen[m]; ok {
		return
	}
	seen[m] = struct{}{}
	fmt.Fprintf(b, "\nmessage %s {\n", m.typ.Name())
	for _, f := range m.fields {
		b.WriteString("  ")
		switch {
		case f.repeated:
			b.WriteString("repeated ")
		case f.optional:
			b.WriteString("optional ")
		}
		typ := f.kind.String()
		if f.message != nil {
			typ = "." + f.message.typ.Name()
		}
		fmt.Fprintf(b, "%s %s = %d;\n", typ, f.name, f.number)
	}
	b.WriteString("}\n")
	for _, f := range m.fields {
		if f.message != nil {
			f.message.writeSchema(b, seen)
		}
	}
}

type field struct {
	name     string
	number   protowire.Number
	index    int
	kind     protoreflect.Kind
	message  *message
	optional bool
	repeated bool
}

func (f *field) append(b []byte, v reflect.Value, depth int) ([]byte, error) {
	wire := f.wireType()
	if f.repeated {
		if v.Len() == 0 {
			return b, nil
		}
		if wire != protowire.BytesType {
			b = protowire.AppendTag(b, f.number, protowire.BytesType)
			start := len(b)
			b = append(b, 0)
			for i := range v.Len() {
				b = f.appendNumeric(b, v.Index(i))
			}
			return finishBytes(b, start), nil
		}
		for i := range v.Len() {
			e := v.Index(i)
			if e.Kind() == reflect.Pointer {
				if e.IsNil() {
					return b, fmt.Errorf("nil message at index %d", i)
				}
				e = e.Elem()
			}
			b = protowire.AppendTag(b, f.number, wire)
			var err error
			b, err = f.appendValue(b, e, depth)
			if err != nil {
				return b, err
			}
		}
		return b, nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return b, nil
		}
		v = v.Elem()
	} else if f.kind != protoreflect.MessageKind && isDefault(v) {
		return b, nil
	}
	b = protowire.AppendTag(b, f.number, wire)
	return f.appendValue(b, v, depth)
}

func (f *field) appendValue(b []byte, v reflect.Value, depth int) ([]byte, error) {
	switch f.kind {
	case protoreflect.StringKind:
		s := v.String()
		if !utf8.ValidString(s) {
			return b, errors.New("invalid UTF-8")
		}
		return protowire.AppendString(b, s), nil
	case protoreflect.BytesKind:
		return protowire.AppendBytes(b, v.Bytes()), nil
	case protoreflect.MessageKind:
		start := len(b)
		b, err := f.message.append(append(b, 0), v, depth+1)
		if err != nil {
			return b, err
		}
		return finishBytes(b, start), nil
	default:
		return f.appendNumeric(b, v), nil
	}
}

func (f *field) appendNumeric(b []byte, v reflect.Value) []byte {
	switch f.kind {
	case protoreflect.BoolKind:
		return protowire.AppendVarint(b, protowire.EncodeBool(v.Bool()))
	case protoreflect.Int32Kind, protoreflect.Int64Kind:
		return protowire.AppendVarint(b, uint64(v.Int())) //nolint:gosec // Protobuf signed varints encode the two's-complement bits.
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return protowire.AppendVarint(b, v.Uint())
	case protoreflect.Sint32Kind, protoreflect.Sint64Kind:
		return protowire.AppendVarint(b, protowire.EncodeZigZag(v.Int()))
	case protoreflect.FloatKind:
		return protowire.AppendFixed32(b, math.Float32bits(float32(v.Float())))
	case protoreflect.DoubleKind:
		return protowire.AppendFixed64(b, math.Float64bits(v.Float()))
	default:
		panic("protobuf: invalid numeric field kind")
	}
}

func isDefault(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		x := v.Float()
		return x == 0 && !math.Signbit(x)
	case reflect.Slice:
		return v.Len() == 0
	default:
		return v.IsZero()
	}
}

// finishBytes fills the reserved length byte, expanding it for larger values.
func finishBytes(b []byte, start int) []byte {
	var buf [binary.MaxVarintLen64]byte
	prefix := protowire.AppendVarint(buf[:0], uint64(len(b)-start-1)) //nolint:gosec // The reserved byte at start precedes the payload.
	b[start] = prefix[0]
	return slices.Insert(b, start+1, prefix[1:]...)
}

func (f *field) consume(b []byte, actual protowire.Type, v reflect.Value, depth int) (int, error) {
	wire := f.wireType()
	if f.repeated && actual == protowire.BytesType && wire != protowire.BytesType {
		packed, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		for len(packed) != 0 {
			n, err := f.consumeValue(packed, appendElement(v), depth)
			if err != nil {
				return 0, err
			}
			packed = packed[n:]
		}
		return n, nil
	}
	if actual != wire {
		return 0, fmt.Errorf("wire type %d, expected %d", actual, wire)
	}
	if f.repeated {
		v = appendElement(v)
	}
	return f.consumeValue(b, v, depth)
}

func appendElement(v reflect.Value) reflect.Value {
	i := v.Len()
	v.Grow(1)
	v.SetLen(i + 1)
	return v.Index(i)
}

func (f *field) consumeValue(b []byte, v reflect.Value, depth int) (int, error) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	switch f.kind {
	case protoreflect.FloatKind:
		x, n := protowire.ConsumeFixed32(b)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		v.SetFloat(float64(math.Float32frombits(x)))
		return n, nil
	case protoreflect.DoubleKind:
		x, n := protowire.ConsumeFixed64(b)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		v.SetFloat(math.Float64frombits(x))
		return n, nil
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind:
		x, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		switch f.kind {
		case protoreflect.StringKind:
			if !utf8.Valid(x) {
				return 0, errors.New("invalid UTF-8")
			}
			v.SetString(string(x))
		case protoreflect.BytesKind:
			v.SetBytes(bytes.Clone(x))
		default: // MessageKind is the remaining length-delimited kind.
			if _, err := f.message.consume(x, v, depth+1, nil); err != nil {
				return 0, err
			}
		}
		return n, nil
	default:
		x, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		return n, f.setVarint(v, x)
	}
}

func (f *field) setVarint(v reflect.Value, x uint64) error {
	var i int64
	switch f.kind {
	case protoreflect.BoolKind:
		v.SetBool(protowire.DecodeBool(x))
		return nil
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		if f.kind == protoreflect.Uint32Kind {
			x = uint64(uint32(x)) //nolint:gosec // Decode uint32 from the low 32 bits of the wire varint.
		}
		if v.OverflowUint(x) {
			return fmt.Errorf("value %d overflows %v", x, v.Type())
		}
		v.SetUint(x)
		return nil
	case protoreflect.Int32Kind:
		i = int64(int32(x)) //nolint:gosec // Decode int32 from the low 32 bits, with sign extension.
	case protoreflect.Sint32Kind:
		i = int64(int32(protowire.DecodeZigZag(x))) //nolint:gosec // Interpret the decoded low 32 bits as a signed integer.
	case protoreflect.Sint64Kind:
		i = protowire.DecodeZigZag(x)
	default: // Int64Kind uses the full wire width.
		i = int64(x) //nolint:gosec // Interpret the wire bits as a two's-complement int64.
	}
	if v.OverflowInt(i) {
		return fmt.Errorf("value %d overflows %v", i, v.Type())
	}
	v.SetInt(i)
	return nil
}

func (f *field) wireType() protowire.Type {
	switch f.kind {
	case protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.DoubleKind:
		return protowire.Fixed64Type
	case protoreflect.BytesKind, protoreflect.StringKind, protoreflect.MessageKind:
		return protowire.BytesType
	default:
		return protowire.VarintType
	}
}
