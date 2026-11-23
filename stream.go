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

// Writer writes a stream of length-delimited protobuf records. Use NewWriter
// to construct it and call its methods serially.
type Writer struct {
	dst     io.Writer
	schemas schemaCache
	buf     []byte
	prefix  [binary.MaxVarintLen32]byte
}

// NewWriter creates a writer over w. The caller owns the destination,
// including any flushing or closing it requires.
func NewWriter(w io.Writer) (*Writer, error) {
	if w == nil {
		return nil, errors.New("protobuf: nil record writer")
	}
	return &Writer{dst: w}, nil
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
	if err := writeFull(w.dst, p); err != nil || len(b) == 0 {
		return err
	}
	return writeFull(w.dst, b)
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
	if err := writeFull(w.dst, p); err != nil || size == 0 {
		return err
	}
	_, err := copyRecord(w.dst, src, size)
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
// wire-compatible named struct types. Use NewReader to construct it and call
// its methods serially.
type Reader struct {
	src     *bufio.Reader
	schemas schemaCache
	buf     []byte
	err     error
}

// NewReader creates a reader over r. The reader may buffer ahead. The caller
// owns the source and keeps it open for the reader's exclusive use.
func NewReader(r io.Reader) (*Reader, error) {
	if r == nil {
		return nil, errors.New("protobuf: nil record reader")
	}
	return &Reader{src: bufio.NewReader(r)}, nil
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

// ReadRaw returns the next encoded record. Its read-only bytes are valid
// until the next read or until the source closes. It buffers the whole record.
// A clean end of stream returns io.EOF. Framing and source errors are terminal.
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
		p, err := r.readChunk(min(n-len(b), r.src.Size()))
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
// skip a record. A clean end of stream returns io.EOF. Framing, source and
// destination errors are terminal.
func (r *Reader) ReadRawTo(dst io.Writer) (int64, error) {
	if dst == nil {
		return 0, errors.New("protobuf: nil record destination")
	}
	n, err := r.readSize()
	if err != nil || n == 0 {
		return 0, err
	}
	c, err := copyRecord(dst, r.src, int64(n))
	if err != nil {
		r.err = fmt.Errorf("protobuf: copy record: %w", err)
	}
	return c, r.err
}

// RawMessages iterates over the remaining encoded messages. It shares the
// cursor and the borrowed byte lifetime of ReadRaw. Each iteration yields a
// non-EOF error once with a nil message and stops.
func (r *Reader) RawMessages() iter.Seq2[RawMessage, error] {
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
	return n <= r.src.Size()
}

func (r *Reader) readSize() (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := binary.ReadUvarint(r.src)
	switch {
	case err == io.EOF: //nolint:errorlint // Only the exact EOF marks a clean end of stream.
		r.err = err
	case err != nil:
		r.err = fmt.Errorf("protobuf: read record size: %w", err)
	case n > maxRecordSize:
		r.err = fmt.Errorf("protobuf: record size %d exceeds %d bytes", n, maxRecordSize)
	default:
		return int(n), nil
	}
	return 0, r.err
}

func (r *Reader) readChunk(n int) (RawMessage, error) {
	b, err := r.src.Peek(n)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.err = fmt.Errorf("protobuf: read record body: %w", err)
		return nil, r.err
	}
	_, _ = r.src.Discard(n) // Peek ensured that n bytes are already buffered.
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
