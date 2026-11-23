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
// Writer and Reader store streams of one message type in independently
// compressed Zstandard pages. Each page contains length-prefixed protobuf
// messages, and the caller supplies the message type when writing or reading
// a stream.
package protobuf

import (
	"fmt"
	"reflect"
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
