package protobuf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"reflect"
	"slices"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/encoding/protowire"
)

// Writer buffers a stream of messages of struct type T into Zstandard pages.
// Use NewWriter to construct it and call its methods serially.
type Writer[T any] struct {
	dst    io.Writer
	msg    *message
	enc    *zstd.Encoder
	target int
	raw    []byte
	record []byte
	frame  []byte
	err    error
}

// NewWriter creates a page writer for the named struct type T. The target size
// is in uncompressed bytes including record lengths. Pass zero for 256 KiB.
// A single record can exceed the target if it fits in a 16 MiB page.
func NewWriter[T any](w io.Writer, pageSize int) (*Writer[T], error) {
	if w == nil {
		return nil, errors.New("protobuf: nil page writer destination")
	}
	if pageSize < 0 || pageSize > maxPageSize {
		return nil, fmt.Errorf("protobuf: page size %d is outside 0..%d bytes", pageSize, maxPageSize)
	}
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	m, err := messageFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	return &Writer[T]{dst: w, msg: m, enc: e, target: pageSize}, nil
}

// Write encodes and buffers v. It flushes any pending page before buffering a
// record that would exceed the target size. An invalid record leaves pending
// records intact. An underlying write failure is returned by all subsequent
// operations through Close.
func (w *Writer[T]) Write(v *T) error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	if w.err != nil {
		return w.err
	}
	if v == nil {
		return errors.New("protobuf: nil page record")
	}
	b, err := w.msg.append(w.record[:0], reflect.ValueOf(v).Elem(), 0)
	if err != nil {
		return err
	}
	n := protowire.SizeVarint(uint64(len(b)))
	if len(b) > maxPageSize-n {
		return fmt.Errorf("protobuf: record exceeds the %d-byte page limit", maxPageSize)
	}
	w.record = b
	if len(w.raw) != 0 && len(w.raw)+n+len(b) > w.target {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	w.raw = protowire.AppendBytes(w.raw, b)
	return nil
}

// Flush emits the pending page to the underlying writer. The caller controls
// any buffering and synchronization at that layer.
func (w *Writer[T]) Flush() error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	if w.err != nil || len(w.raw) == 0 {
		return w.err
	}
	w.frame = w.enc.EncodeAll(w.raw, append(w.frame[:0], make([]byte, pageHeaderSize)...))
	copy(w.frame, pageMagic)
	binary.LittleEndian.PutUint32(w.frame[4:8], uint32(len(w.frame)-pageHeaderSize)) //nolint:gosec // Zstandard frame overhead keeps a page below the 17 MiB limit.
	binary.LittleEndian.PutUint32(w.frame[8:12], uint32(len(w.raw)))                 //nolint:gosec // Write bounds the raw page to 16 MiB.
	n, err := w.dst.Write(w.frame)
	if err == nil && n != len(w.frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = fmt.Errorf("protobuf: write page: %w", err)
		return w.err
	}
	w.raw = w.raw[:0]
	return nil
}

// Close flushes the pending page and releases the writer's buffers. The
// caller retains ownership of the underlying writer.
// Subsequent calls to Write, Flush, or Close return io.ErrClosedPipe.
func (w *Writer[T]) Close() error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	err := w.Flush()
	cerr := w.enc.Close()
	*w = Writer[T]{}
	if cerr != nil {
		return errors.Join(err, cerr)
	}
	return err
}

// Reader decodes records from a sequence of Zstandard pages. Available
// records can be read into any wire-compatible named struct type. Use
// NewReader to construct it and call its methods serially.
type Reader struct {
	src     io.Reader
	dec     *zstd.Decoder
	frame   []byte
	raw     []byte
	pending []byte
	err     error
}

// NewReader creates a reader for a sequence of Zstandard pages.
func NewReader(r io.Reader) (*Reader, error) {
	if r == nil {
		return nil, errors.New("protobuf: nil page reader source")
	}
	d, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(maxPageSize),
		zstd.WithDecoderMaxMemory(maxPageSize),
		zstd.WithDecodeAllCapLimit(true),
	)
	if err != nil {
		return nil, err
	}
	return &Reader{src: r, dec: d}, nil
}

// Read replaces dst, a pointer to a named struct, with the next record. It
// returns io.EOF at a clean end of stream. An incomplete page returns an
// error wrapping io.ErrUnexpectedEOF. End of stream, page read errors, and
// invalid record lengths are terminal.
//
// An error decoding the record leaves it available for retry with a
// compatible type. Read leaves dst unchanged on error. Decoded values
// remain valid after subsequent reads.
func (r *Reader) Read[T any](dst *T) error {
	if r.src == nil {
		return io.ErrClosedPipe
	}
	if r.err != nil {
		return r.err
	}
	if dst == nil {
		return errors.New("protobuf: nil page record destination")
	}
	m, err := messageFor(reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	if len(r.pending) == 0 {
		if err := r.readPage(); err != nil {
			r.err = err
			return err
		}
	}
	b, n := protowire.ConsumeBytes(r.pending)
	if n < 0 {
		r.err = fmt.Errorf("protobuf: read record length: %w", protowire.ParseError(n))
		return r.err
	}
	if err := m.unmarshal(b, reflect.ValueOf(dst).Elem()); err != nil {
		return fmt.Errorf("protobuf: decode page record: %w", err)
	}
	r.pending = r.pending[n:]
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

// Close releases the decoder and its buffers. The caller retains ownership
// of the underlying reader.
// Subsequent reads and calls to Close return io.ErrClosedPipe.
func (r *Reader) Close() error {
	if r.src == nil {
		return io.ErrClosedPipe
	}
	r.dec.Close()
	*r = Reader{}
	return nil
}

// An empty stream has no bytes. Each v1 page starts with PBP followed by 0x01,
// then two unsigned 32-bit little-endian lengths for the compressed and raw
// bytes, respectively. The compressed length counts the following Zstandard
// payload. The writer emits one checksummed frame per page with no dictionary.
// A raw page contains one or more records, each an unsigned protobuf varint
// length followed by that many proto3 bytes. Zero-length messages are valid.
// The compressed and raw lengths are limited to 17 MiB and 16 MiB respectively.
const (
	pageMagic             = "PBP\x01"
	pageHeaderSize        = 12
	defaultPageSize       = 256 << 10
	maxPageSize           = 16 << 20
	maxCompressedPageSize = 17 << 20
)

func (r *Reader) readPage() error {
	var hdr [pageHeaderSize]byte
	if _, err := io.ReadFull(r.src, hdr[:]); err != nil {
		if err == io.EOF {
			return err
		}
		return fmt.Errorf("protobuf: read page header: %w", err)
	}
	if string(hdr[:4]) != pageMagic {
		return fmt.Errorf("protobuf: invalid page magic or version %x", hdr[:4])
	}
	cl := binary.LittleEndian.Uint32(hdr[4:8])
	rl := binary.LittleEndian.Uint32(hdr[8:12])
	if cl == 0 || cl > maxCompressedPageSize {
		return fmt.Errorf("protobuf: invalid compressed page size %d", cl)
	}
	if rl == 0 || rl > maxPageSize {
		return fmt.Errorf("protobuf: invalid raw page size %d", rl)
	}
	r.frame = slices.Grow(r.frame[:0], int(cl))[:cl]
	if _, err := io.ReadFull(r.src, r.frame); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("protobuf: read page payload: %w", err)
	}
	r.raw = slices.Grow(r.raw[:0], int(rl))
	b, err := r.dec.DecodeAll(r.frame, r.raw[:0:rl])
	if err != nil {
		return fmt.Errorf("protobuf: decompress page: %w", err)
	}
	if len(b) != int(rl) {
		return fmt.Errorf("protobuf: raw page size %d does not match %d decoded bytes", rl, len(b))
	}
	r.pending = b
	return nil
}
