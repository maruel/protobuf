package protobuf

import (
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
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
