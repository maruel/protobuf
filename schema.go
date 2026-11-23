package protobuf

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Schema returns a standalone proto3 definition for the named struct type T
// and the message types it references. Definitions use the Go type names.
func Schema[T any]() (string, error) {
	m, err := messageFor(reflect.TypeFor[T]())
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n")
	m.writeSchema(&b, make(map[*message]struct{}))
	return b.String(), nil
}

// Immutable after construction of the message graph.
type message struct {
	typ      reflect.Type
	fields   []field
	byNumber map[protowire.Number]int
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

var messageCache sync.Map // map[reflect.Type]*message

func messageFor(t reflect.Type) (*message, error) {
	if cached, ok := messageCache.Load(t); ok {
		return cached.(*message), nil
	}
	m, err := buildMessage(t, make(map[string]*message))
	if err != nil {
		return nil, err
	}
	cached, _ := messageCache.LoadOrStore(t, m)
	return cached.(*message), nil
}

func buildMessage(t reflect.Type, seen map[string]*message) (*message, error) {
	name := t.Name()
	if t.Kind() != reflect.Struct || !protoreflect.Name(name).IsValid() {
		return nil, fmt.Errorf("protobuf: %v is not a named struct with a valid protobuf identifier", t)
	}
	if m := seen[name]; m != nil {
		if m.typ != t {
			return nil, fmt.Errorf("protobuf: %v and %v share message name %q", m.typ, t, name)
		}
		return m, nil
	}
	m := &message{typ: t, byNumber: make(map[protowire.Number]int)}
	seen[name] = m
	names := make(map[string]struct{})
	for i := range t.NumField() {
		sf := t.Field(i)
		tag, ok := sf.Tag.Lookup("protobuf")
		if !ok || tag == "-" {
			continue
		}
		f, err := buildField(sf, i, tag, seen)
		if err != nil {
			return nil, fmt.Errorf("protobuf: %v.%s: %w", t, sf.Name, err)
		}
		if _, ok := m.byNumber[f.number]; ok {
			return nil, fmt.Errorf("protobuf: %v has duplicate field number %d", t, f.number)
		}
		// protoc checks JSON names case-insensitively after camel-casing.
		key := strings.ToLower(strings.ReplaceAll(f.name, "_", ""))
		if _, ok := names[key]; ok {
			return nil, fmt.Errorf("protobuf: %v has conflicting JSON names for field %q", t, f.name)
		}
		names[key] = struct{}{}
		m.byNumber[f.number] = len(m.fields)
		m.fields = append(m.fields, f)
	}
	if len(m.fields) == 0 && t.NumField() != 0 {
		return nil, fmt.Errorf("protobuf: %v has Go fields but no protobuf fields", t)
	}
	slices.SortFunc(m.fields, func(a, b field) int {
		return cmp.Compare(a.number, b.number)
	})
	for i := range m.fields {
		m.byNumber[m.fields[i].number] = i
	}
	return m, nil
}

func buildField(sf reflect.StructField, index int, tag string, seen map[string]*message) (field, error) {
	if !sf.IsExported() {
		return field{}, fmt.Errorf("tagged field must be exported")
	}
	parts := strings.Split(tag, ",")
	n, err := strconv.ParseUint(parts[0], 10, 29)
	num := protowire.Number(n)
	if err != nil || !num.IsValid() || (num >= protowire.FirstReservedNumber && num <= protowire.LastReservedNumber) {
		return field{}, fmt.Errorf("invalid protobuf field number %q", parts[0])
	}
	f := field{name: sf.Name, number: num, index: index}
	var named, zigzag bool
	for _, option := range parts[1:] {
		switch {
		case option == "zigzag" && !zigzag:
			zigzag = true
		case strings.HasPrefix(option, "name=") && !named:
			named = true
			f.name = strings.TrimPrefix(option, "name=")
		default:
			return field{}, fmt.Errorf("invalid or duplicate protobuf option %q", option)
		}
	}
	if !protoreflect.Name(f.name).IsValid() {
		return field{}, fmt.Errorf("invalid protobuf field name %q", f.name)
	}

	t := sf.Type
	if t.Kind() == reflect.Slice && t.Elem() != reflect.TypeFor[byte]() {
		f.repeated = true
		t = t.Elem()
	}
	pointer := t.Kind() == reflect.Pointer
	if pointer {
		t = t.Elem()
	}
	f.kind = scalarKind(t)
	switch {
	case t.Kind() == reflect.Struct:
		f.kind = protoreflect.MessageKind
		f.message, err = buildMessage(t, seen)
		if err != nil {
			return field{}, err
		}
	case f.kind == 0 || (pointer && f.repeated):
		return field{}, fmt.Errorf("unsupported field type %v", sf.Type)
	default:
		f.optional = pointer
	}

	if zigzag {
		switch f.kind {
		case protoreflect.Int32Kind:
			f.kind = protoreflect.Sint32Kind
		case protoreflect.Int64Kind:
			f.kind = protoreflect.Sint64Kind
		default:
			return field{}, fmt.Errorf("zigzag requires a signed integer")
		}
	}
	return f, nil
}

func scalarKind(t reflect.Type) protoreflect.Kind {
	switch t.Kind() {
	case reflect.Bool:
		return protoreflect.BoolKind
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return protoreflect.Int32Kind
	case reflect.Int, reflect.Int64:
		return protoreflect.Int64Kind
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return protoreflect.Uint32Kind
	case reflect.Uint, reflect.Uint64:
		return protoreflect.Uint64Kind
	case reflect.Float32:
		return protoreflect.FloatKind
	case reflect.Float64:
		return protoreflect.DoubleKind
	case reflect.String:
		return protoreflect.StringKind
	case reflect.Slice:
		if t.Elem() == reflect.TypeFor[byte]() {
			return protoreflect.BytesKind
		}
	}
	return 0
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
