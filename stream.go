package protobuf

import (
	"errors"
	"fmt"
	"io"
	"iter"
	"reflect"
)

// RecordWriter accepts complete encoded messages. WriteRecord consumes or
// copies the bytes during the call and leaves the input slice unchanged.
// An empty slice represents an empty message.
type RecordWriter interface {
	WriteRecord(b []byte) error
}

// Writer encodes messages of one named struct type and sends them to a
// RecordWriter. Use NewWriter to construct it and call Write serially.
type Writer[T any] struct {
	dst    RecordWriter
	msg    *message
	record []byte
}

// NewWriter creates a writer for the named struct type T. The caller owns
// the record destination, including any flushing or closing it requires.
func NewWriter[T any](w RecordWriter) (*Writer[T], error) {
	if w == nil {
		return nil, errors.New("protobuf: nil record writer")
	}
	m, err := messageFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	return &Writer[T]{dst: w, msg: m}, nil
}

// Write encodes v as one record and returns any error from the destination.
// An encoding error leaves the destination untouched.
func (w *Writer[T]) Write(v *T) error {
	if v == nil {
		return errors.New("protobuf: nil record")
	}
	b, err := w.msg.append(w.record[:0], reflect.ValueOf(v).Elem(), 0)
	if err != nil {
		return err
	}
	if err := w.dst.WriteRecord(b); err != nil {
		return err
	}
	w.record = b
	return nil
}

// RecordReader supplies complete encoded messages. ReadRecord returns a
// read-only slice valid until the next call and io.EOF at a clean end of
// stream. An empty slice with a nil error represents an empty message.
type RecordReader interface {
	ReadRecord() ([]byte, error)
}

// Reader decodes records into wire-compatible named struct types. Use
// NewReader to construct it and call its methods serially.
type Reader struct {
	src    RecordReader
	record []byte
}

// NewReader creates a reader over a record source. The caller owns the
// source and keeps it open for the reader's exclusive use.
func NewReader(r RecordReader) (*Reader, error) {
	if r == nil {
		return nil, errors.New("protobuf: nil record reader")
	}
	return &Reader{src: r}, nil
}

// Read replaces dst with the next decoded record and passes through errors
// from the record source. An error leaves dst unchanged. If decoding fails,
// the same record stays pending for retry with a compatible type. Decoded
// values remain valid after subsequent reads.
func (r *Reader) Read[T any](dst *T) error {
	if dst == nil {
		return errors.New("protobuf: nil record destination")
	}
	m, err := messageFor(reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	if r.record == nil {
		b, err := r.src.ReadRecord()
		if err != nil {
			return err
		}
		r.record = b
	}
	if err := m.unmarshal(r.record, reflect.ValueOf(dst).Elem()); err != nil {
		return fmt.Errorf("protobuf: decode record: %w", err)
	}
	r.record = nil
	return nil
}

// Events iterates over the remaining records. Each iteration yields any read
// error other than io.EOF once with the zero value of T and stops. Breaking
// after a record leaves the reader positioned at the next record.
func (r *Reader) Events[T any]() iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for {
			var v T
			err := r.Read(&v)
			if err == io.EOF || !yield(v, err) || err != nil { //nolint:errorlint // A wrapped EOF is an input error and must reach the caller.
				return
			}
		}
	}
}
