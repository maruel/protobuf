package protobuf

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"slices"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const maxMessageDepth = 100

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
			return b, fmt.Errorf("invalid UTF-8")
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
		return protowire.AppendVarint(b, uint64(v.Int()))
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
	prefix := protowire.AppendVarint(buf[:0], uint64(len(b)-start-1))
	b[start] = prefix[0]
	return slices.Insert(b, start+1, prefix[1:]...)
}

func (m *message) unmarshal(data []byte, dst reflect.Value) error {
	v := reflect.New(m.typ).Elem()
	if err := m.consume(data, v, 0); err != nil {
		return err
	}
	dst.Set(v)
	return nil
}

func (m *message) consume(b []byte, v reflect.Value, depth int) error {
	if depth > maxMessageDepth {
		return fmt.Errorf("protobuf: message nesting exceeds %d", maxMessageDepth)
	}
	for len(b) != 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("protobuf: %v: %w", m.typ, protowire.ParseError(n))
		}
		if !num.IsValid() {
			return fmt.Errorf("protobuf: %v: invalid field number %d", m.typ, num)
		}
		b = b[n:]
		i, ok := m.byNumber[num]
		if !ok {
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return fmt.Errorf("protobuf: %v: field %d: %w", m.typ, num, protowire.ParseError(n))
			}
			b = b[n:]
			continue
		}
		f := &m.fields[i]
		n, err := f.consume(b, typ, v.Field(f.index), depth)
		if err != nil {
			return fmt.Errorf("protobuf: %v.%s: %w", m.typ, f.name, err)
		}
		b = b[n:]
	}
	return nil
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
				return 0, fmt.Errorf("invalid UTF-8")
			}
			v.SetString(string(x))
		case protoreflect.BytesKind:
			v.SetBytes(bytes.Clone(x))
		case protoreflect.MessageKind:
			if err := f.message.consume(x, v, depth+1); err != nil {
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
	switch f.kind {
	case protoreflect.BoolKind:
		v.SetBool(protowire.DecodeBool(x))
		return nil
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		if f.kind == protoreflect.Uint32Kind {
			x = uint64(uint32(x))
		}
		if v.OverflowUint(x) {
			return fmt.Errorf("value %d overflows %v", x, v.Type())
		}
		v.SetUint(x)
		return nil
	case protoreflect.Int32Kind:
		x = uint64(int32(x))
	case protoreflect.Sint32Kind:
		x = uint64(int32(protowire.DecodeZigZag(x)))
	case protoreflect.Sint64Kind:
		x = uint64(protowire.DecodeZigZag(x))
	}
	i := int64(x)
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
