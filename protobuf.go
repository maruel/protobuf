// Package protobuf encodes tagged Go structs as proto3 messages.
//
// A field tag starts with its protobuf field number, such as protobuf:"1".
// The name defaults to the Go field name and can be set with
// protobuf:"1,name=event_id". The zigzag option uses sint32 or sint64 for
// signed integers. Untagged fields and fields tagged protobuf:"-" are omitted.
// A message struct with Go fields must declare at least one protobuf field.
//
// Supported values include booleans, integers, floats, UTF-8 strings, byte
// slices, named structs, and slices of those types. Signed and unsigned Go
// integers use the corresponding 32-bit protobuf types through width 32 and
// the 64-bit types for int, uint, int64, and uint64. Decoded integers must
// fit in the destination Go type, including narrower types such as int8 and
// uint16. A byte slice is one bytes field, and a slice of byte slices is
// repeated bytes. Repeated numeric and Boolean fields use packed encoding.
//
// Pointers to scalars and byte slices track explicit presence. A struct field
// always encodes a submessage. A pointer to a struct omits the submessage when
// nil. Repeated message elements may be values or non-nil pointers. Keep field
// numbers and their wire types stable across revisions.
//
// Writer and Reader encode and decode streams of complete protobuf records.
// Their source and destination can be provided by the page subpackage for
// independently compressed Zstandard storage.
package protobuf

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Marshal returns the proto3 encoding of a named struct or a non-nil pointer
// to one.
func Marshal(v any) ([]byte, error) {
	return Append(nil, v)
}

// Append appends a struct's proto3 encoding to dst. On error it returns dst at
// its original length. The message can be a named struct or a non-nil pointer
// to one. Keep the appended region disjoint from the message's byte fields.
func Append(dst []byte, v any) ([]byte, error) {
	rv := reflect.Indirect(reflect.ValueOf(v))
	if !rv.IsValid() {
		return dst, fmt.Errorf("protobuf: expected a named struct or non-nil pointer, got %T", v)
	}
	m, err := messageFor(rv.Type())
	if err != nil {
		return dst, err
	}
	return m.append(dst, rv, 0)
}

// Unmarshal decodes a proto3 message into a non-nil pointer to a named struct.
// On success it replaces the destination, zeroing untagged fields and fields
// absent from the message. The decoded strings and bytes are independent of
// data. It discards unknown fields and rejects mismatched wire types for known
// fields and decoded integers outside the Go field's range. On error it
// leaves the destination unchanged.
func Unmarshal(data []byte, dst any) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("protobuf: expected a non-nil pointer to a named struct, got %T", dst)
	}
	rv = rv.Elem()
	m, err := messageFor(rv.Type())
	if err != nil {
		return err
	}
	return m.unmarshal(data, rv)
}

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

// RawMessage holds the encoded bytes of one protobuf message.
type RawMessage []byte

// Decode decodes the complete message into dst using Unmarshal.
func (m RawMessage) Decode[T any](dst *T) error {
	return Unmarshal(m, dst)
}

// DecodeField decodes the top-level field with the given protobuf number
// into its field in dst, a non-nil pointer to a named struct. It uses the
// field's tags and Go type, and reports whether its tag appeared on the wire.
// An absent field or an error leaves dst unchanged. A present field replaces
// its previous value. Other fields in dst are preserved.
//
// DecodeField checks wire framing for the whole message and skips the
// contents of unrelated fields. Decoded strings and bytes own their data.
func (m RawMessage) DecodeField[T any](number int, dst *T) (bool, error) {
	if dst == nil {
		return false, fmt.Errorf("protobuf: expected a non-nil pointer to a named struct, got %T", dst)
	}
	if number < int(protowire.MinValidNumber) || number > int(protowire.MaxValidNumber) {
		return false, fmt.Errorf("protobuf: invalid field number %d", number)
	}
	msg, err := messageFor(reflect.TypeFor[T]())
	if err != nil {
		return false, err
	}
	i, ok := msg.byNumber[protowire.Number(number)]
	if !ok {
		return false, fmt.Errorf("protobuf: %v has no field %d", msg.typ, number)
	}
	f := &msg.fields[i]
	v := reflect.ValueOf(dst).Elem().Field(f.index)
	value := reflect.New(v.Type()).Elem()
	present, err := msg.consume(m, value, 0, f)
	if err != nil || !present {
		return false, err
	}
	v.Set(value)
	return true, nil
}

var messageCache sync.Map // map[reflect.Type]*message

func messageFor(t reflect.Type) (*message, error) {
	cached, ok := messageCache.Load(t)
	if !ok {
		m, err := buildMessage(t, make(map[string]*message))
		if err != nil {
			return nil, err
		}
		cached, _ = messageCache.LoadOrStore(t, m)
	}
	return cached.(*message), nil //nolint:forcetypeassert // The cache only stores *message values.
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
		f, err := buildField(&sf, i, tag, seen)
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

func buildField(sf *reflect.StructField, index int, tag string, seen map[string]*message) (field, error) {
	if !sf.IsExported() {
		return field{}, errors.New("tagged field must be exported")
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
			return field{}, errors.New("zigzag requires a signed integer")
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
		return 0
	default:
		return 0
	}
}
