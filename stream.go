package protobuf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"reflect"
	"slices"
)

// Writer writes a stream of length-delimited protobuf records. Call its methods
// serially.
type Writer struct {
	// Dst receives records. Set it before use and change it only between calls.
	// The caller owns any flushing or closing it requires.
	Dst io.Writer

	schemas schemaCache
	buf     []byte
	prefix  [binary.MaxVarintLen32]byte
}

// NewWriter creates a writer over w, which must be non-nil.
func NewWriter(w io.Writer) *Writer {
	return &Writer{Dst: w}
}

// Write encodes a non-nil pointer to a named struct as one record. It
// validates the struct type and buffers the encoded message. An encoding
// error leaves the destination untouched. A write error can leave a partial
// record in the destination.
func (w *Writer) Write[T any](v *T) error {
	if v == nil {
		return errors.New("protobuf: nil record")
	}
	m, err := w.schemas.lookupFor[T]()
	if err != nil {
		return err
	}
	b, err := m.append(w.buf[:0], reflect.ValueOf(v).Elem(), 0)
	if err != nil {
		return err
	}
	if len(b) > maxRecordSize {
		return fmt.Errorf("protobuf: record size %d exceeds %d bytes", len(b), maxRecordSize)
	}
	w.buf = b
	p := binary.AppendUvarint(w.prefix[:0], uint64(len(b)))
	if err := writeFull(w.Dst, p); err != nil || len(b) == 0 {
		return err
	}
	return writeFull(w.Dst, b)
}

// WriteRawFrom copies size bytes of an already encoded message from src
// as one record. It uses bounded working memory and leaves any trailing bytes
// in src unread. If src ends early, it returns io.ErrUnexpectedEOF. A read or
// write error can leave a partial record in the destination.
func (w *Writer) WriteRawFrom(src io.Reader, size int64) error {
	if src == nil {
		return errors.New("protobuf: nil record source")
	}
	if size < 0 || size > maxRecordSize {
		return fmt.Errorf("protobuf: record size %d is outside 0..%d bytes", size, maxRecordSize)
	}
	p := binary.AppendUvarint(w.prefix[:0], uint64(size))
	if err := writeFull(w.Dst, p); err != nil || size == 0 {
		return err
	}
	_, err := copyRecord(w.Dst, src, size)
	return err
}

// DecodeError reports a failure to decode a record read by Reader.Read.
type DecodeError struct {
	// Raw is the encoded record. The caller owns these bytes.
	Raw RawMessage
	// Err is the underlying decoding error.
	Err error
}

// Error describes the decoding failure.
func (e *DecodeError) Error() string {
	return "protobuf: decode record: " + e.Err.Error()
}

// Unwrap returns the underlying decoding error.
func (e *DecodeError) Unwrap() error {
	return e.Err
}

// Reader reads length-delimited protobuf records and can decode them into
// wire-compatible named struct types. Call its methods serially.
//
// Errors apply to the current call. After a framing or I/O error, continue
// reading from the same position only if you know the call stopped at a record
// boundary. To resume after repositioning the source at a record boundary,
// reset or replace Src to discard any buffered bytes.
type Reader struct {
	// Src is the buffered source of records. Set it to an initialized reader
	// before use. Reset or replace it only between calls.
	Src *bufio.Reader

	schemas schemaCache
	buf     []byte
}

// NewReader creates a reader over r, which must be non-nil. The reader may
// buffer ahead. The caller owns the source and keeps it open for the reader's
// exclusive use.
func NewReader(r io.Reader) *Reader {
	return &Reader{Src: bufio.NewReader(r)}
}

// Read replaces dst with the next decoded record and passes through errors
// from the record source. An error leaves dst unchanged. If decoding fails,
// Read returns a *DecodeError and advances to the following record. Decoded
// values and the bytes in a DecodeError remain valid after subsequent reads.
func (r *Reader) Read[T any](dst *T) error {
	if dst == nil {
		return errors.New("protobuf: nil record destination")
	}
	m, err := r.schemas.lookupFor[T]()
	if err != nil {
		return err
	}
	b, err := r.ReadRaw()
	if err != nil {
		return err
	}
	if err := m.unmarshal(b, reflect.ValueOf(dst).Elem()); err != nil {
		if r.fitsInputBuffer(len(b)) {
			b = slices.Clone(b)
		} else {
			// ReadRaw used r.buf. Transfer it to the caller.
			r.buf = nil
			b = slices.Clip(b)
		}
		return &DecodeError{Raw: b, Err: err}
	}
	return nil
}

// ReadRaw buffers the next record in full and returns its encoded bytes.
// Use ReadRawTo to copy a record with bounded working memory.
//
// The read-only bytes are valid until the next read, until Src is reset or
// replaced, or until the source closes. A clean end of stream returns io.EOF.
func (r *Reader) ReadRaw() (RawMessage, error) {
	n, err := r.readSize()
	if err != nil {
		return nil, err
	}
	if r.fitsInputBuffer(n) {
		return r.readChunk(n)
	}
	b := r.buf[:0]
	for len(b) < n {
		p, err := r.readChunk(min(n-len(b), r.Src.Size()))
		if err != nil {
			return nil, err
		}
		b = append(b, p...)
	}
	r.buf = b
	return r.buf, nil
}

// ReadRawTo copies the next encoded record to dst and returns the number of
// message bytes written. It uses bounded working memory. Pass io.Discard to
// skip a record. A clean end of stream returns io.EOF.
func (r *Reader) ReadRawTo(dst io.Writer) (int64, error) {
	if dst == nil {
		return 0, errors.New("protobuf: nil record destination")
	}
	n, err := r.readSize()
	if err != nil || n == 0 {
		return 0, err
	}
	c, err := copyRecord(dst, r.Src, int64(n))
	if err != nil {
		return c, fmt.Errorf("protobuf: copy record: %w", err)
	}
	return c, nil
}

// Iter returns a single-use iterator over the remaining records decoded into
// values of T, which must be a named struct. The values remain valid after
// subsequent reads. Restarting the iterator resumes at the reader's current
// position. It stops at a clean EOF. On any other error, it yields the zero
// value of T and the error from Read, then stops.
func (r *Reader) Iter[T any]() iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var v T
		for {
			if err := r.Read(&v); err != nil {
				if err != io.EOF { //nolint:errorlint // A wrapped EOF is an input error and must reach the caller.
					var zero T
					yield(zero, err)
				}
				return
			}
			if !yield(v, nil) {
				return
			}
		}
	}
}

// IterRaw returns a single-use iterator over the remaining encoded records.
// Restarting the iterator resumes at the reader's current position. Each
// record is buffered in full and its bytes have the same lifetime as a result
// of ReadRaw. It stops at a clean EOF. On any other error, it yields a nil
// message and the error, then stops.
func (r *Reader) IterRaw() iter.Seq2[RawMessage, error] {
	return func(yield func(RawMessage, error) bool) {
		for {
			b, err := r.ReadRaw()
			if err == io.EOF || !yield(b, err) || err != nil { //nolint:errorlint // A wrapped EOF is an input error and must reach the caller.
				return
			}
		}
	}
}

const maxRecordSize = math.MaxInt32

func (r *Reader) fitsInputBuffer(n int) bool {
	return n <= r.Src.Size()
}

func (r *Reader) readSize() (int, error) {
	n, err := binary.ReadUvarint(r.Src)
	switch {
	case err == io.EOF: //nolint:errorlint // Only the exact EOF marks a clean end of stream.
		return 0, io.EOF
	case err != nil:
		return 0, fmt.Errorf("protobuf: read record size: %w", err)
	case n > maxRecordSize:
		return 0, fmt.Errorf("protobuf: record size %d exceeds %d bytes", n, maxRecordSize)
	default:
		return int(n), nil
	}
}

func (r *Reader) readChunk(n int) (RawMessage, error) {
	b, err := r.Src.Peek(n)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("protobuf: read record body: %w", err)
	}
	_, _ = r.Src.Discard(n) // Peek ensured that n bytes are already buffered.
	return b, nil
}

type schemaCache struct {
	entries [8]*message
	next    int
}

// lookupFor resolves the schema for T. Hits leave the entries in place.
// A new schema takes slot zero and moves its predecessor into the ring of
// secondary slots, which fill in order and evict the oldest schema.
func (c *schemaCache) lookupFor[T any]() (*message, error) {
	t := reflect.TypeFor[T]()
	if m := c.entries[0]; m != nil && m.typ == t {
		return m, nil
	}
	for _, m := range c.entries[1:] {
		if m == nil {
			break
		}
		if m.typ == t {
			return m, nil
		}
	}
	m, err := messageFor(t)
	if err != nil {
		return nil, err
	}
	if p := c.entries[0]; p != nil {
		c.entries[c.next+1] = p
		c.next = (c.next + 1) % (len(c.entries) - 1)
	}
	c.entries[0] = m
	return m, nil
}

func writeFull(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func copyRecord(dst io.Writer, src io.Reader, size int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
