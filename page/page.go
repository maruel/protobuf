// Package page stores sequences of byte records in independently compressed Zstandard pages.
package page

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
)

// Writer buffers byte records into independently compressed Zstandard pages.
// Use NewWriter to construct it and call its methods serially.
type Writer struct {
	dst    io.Writer
	enc    *zstd.Encoder
	target int
	raw    []byte
	frame  []byte
	err    error
}

// NewWriter creates a page writer. The target size is in uncompressed bytes
// including record lengths. Pass zero for 256 KiB.
// A single record can exceed the target if it fits in a 16 MiB page.
func NewWriter(w io.Writer, pageSize int) (*Writer, error) {
	if w == nil {
		return nil, errors.New("page: nil page writer destination")
	}
	if pageSize < 0 || pageSize > maxPageSize {
		return nil, fmt.Errorf("page: page size %d is outside 0..%d bytes", pageSize, maxPageSize)
	}
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	return &Writer{dst: w, enc: e, target: pageSize}, nil
}

// WriteRecord copies b into the pending page. A record can be empty. If a
// record would exceed the target size, the writer flushes the pending page
// before buffering it. An oversized record leaves the pending page intact.
// An underlying write failure is returned by all subsequent operations
// through Close.
func (w *Writer) WriteRecord(b []byte) error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	if w.err != nil {
		return w.err
	}
	var hdr [binary.MaxVarintLen64]byte
	prefix := binary.AppendUvarint(hdr[:0], uint64(len(b)))
	if len(b) > maxPageSize-len(prefix) {
		return fmt.Errorf("page: record exceeds the %d-byte page limit", maxPageSize)
	}
	if len(w.raw) != 0 && len(w.raw)+len(prefix)+len(b) > w.target {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	w.raw = append(w.raw, prefix...)
	w.raw = append(w.raw, b...)
	return nil
}

// Flush emits the pending page to the underlying writer. The caller controls
// any buffering and synchronization at that layer.
func (w *Writer) Flush() error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	if w.err != nil || len(w.raw) == 0 {
		return w.err
	}
	w.frame = w.enc.EncodeAll(w.raw, append(w.frame[:0], make([]byte, pageHeaderSize)...))
	copy(w.frame, pageMagic)
	binary.LittleEndian.PutUint32(w.frame[4:8], uint32(len(w.frame)-pageHeaderSize)) //nolint:gosec // Zstandard frame overhead keeps a page below the 17 MiB limit.
	binary.LittleEndian.PutUint32(w.frame[8:12], uint32(len(w.raw)))                 //nolint:gosec // WriteRecord bounds the raw page to 16 MiB.
	n, err := w.dst.Write(w.frame)
	if err == nil && n != len(w.frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = fmt.Errorf("page: write page: %w", err)
		return w.err
	}
	w.raw = w.raw[:0]
	return nil
}

// Close flushes the pending page and releases the writer's buffers. The
// caller retains ownership of the underlying writer.
// Subsequent calls to WriteRecord, Flush, or Close return io.ErrClosedPipe.
func (w *Writer) Close() error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	err := w.Flush()
	cerr := w.enc.Close()
	*w = Writer{}
	if cerr != nil {
		return errors.Join(err, cerr)
	}
	return err
}

// Reader reads byte records from a sequence of Zstandard pages. Use
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
		return nil, errors.New("page: nil page reader source")
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

// ReadRecord returns the next record as a read-only slice valid until the
// next call to ReadRecord or Close. Copy the bytes to keep them
// longer. ReadRecord returns io.EOF at a clean end of stream, and wraps
// io.ErrUnexpectedEOF for an incomplete page or record. End of stream, page
// read errors, and invalid record lengths are terminal.
func (r *Reader) ReadRecord() ([]byte, error) {
	if r.src == nil {
		return nil, io.ErrClosedPipe
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.pending) == 0 {
		if err := r.readPage(); err != nil {
			r.err = err
			return nil, err
		}
	}
	size, n := binary.Uvarint(r.pending)
	if n < 0 || (n == 0 && len(r.pending) >= binary.MaxVarintLen64) {
		r.err = errors.New("page: read record length: varint overflow")
		return nil, r.err
	}
	b := r.pending[n:]
	if n == 0 || size > uint64(len(b)) {
		r.err = fmt.Errorf("page: read record length: %w", io.ErrUnexpectedEOF)
		return nil, r.err
	}
	end := int(size) //nolint:gosec // The size fits in the slice.
	r.pending = b[end:]
	return b[:end:end], nil
}

// Close releases the decoder and its buffers. The caller retains ownership
// of the underlying reader.
// Subsequent calls to ReadRecord or Close return io.ErrClosedPipe.
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
// A raw page contains one or more records, each an unsigned base-128 varint
// length followed by that many bytes. Zero-length records are valid.
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
		return fmt.Errorf("page: read page header: %w", err)
	}
	if string(hdr[:4]) != pageMagic {
		return fmt.Errorf("page: invalid page magic or version %x", hdr[:4])
	}
	cl := binary.LittleEndian.Uint32(hdr[4:8])
	rl := binary.LittleEndian.Uint32(hdr[8:12])
	if cl == 0 || cl > maxCompressedPageSize {
		return fmt.Errorf("page: invalid compressed page size %d", cl)
	}
	if rl == 0 || rl > maxPageSize {
		return fmt.Errorf("page: invalid raw page size %d", rl)
	}
	r.frame = slices.Grow(r.frame[:0], int(cl))[:cl]
	if _, err := io.ReadFull(r.src, r.frame); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("page: read page payload: %w", err)
	}
	r.raw = slices.Grow(r.raw[:0], int(rl))
	b, err := r.dec.DecodeAll(r.frame, r.raw[:0:rl])
	if err != nil {
		return fmt.Errorf("page: decompress page: %w", err)
	}
	if len(b) != int(rl) {
		return fmt.Errorf("page: raw page size %d does not match %d decoded bytes", rl, len(b))
	}
	r.pending = b
	return nil
}
