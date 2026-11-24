// Package page reads and writes independently compressed Zstandard pages.
package page

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"

	"github.com/klauspost/compress/zstd"
)

// Writer compresses and writes one page at a time. Use NewWriter to construct
// it and call its methods serially.
type Writer struct {
	dst   io.Writer
	enc   *zstd.Encoder
	frame []byte
}

// NewWriter creates a page writer over dst. The caller owns dst.
func NewWriter(dst io.Writer) (*Writer, error) {
	if dst == nil {
		return nil, errors.New("page: nil page writer destination")
	}
	e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	return &Writer{dst: dst, enc: e}, nil
}

// WritePage compresses and writes one page. The uncompressed payload must be
// nonempty and at most 16 MiB. A write error can leave a partial page in the
// destination. Resume at a known page boundary.
func (w *Writer) WritePage(p []byte) error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	if len(p) == 0 || len(p) > maxPageSize {
		return fmt.Errorf("page: raw page size %d is outside 1..%d bytes", len(p), maxPageSize)
	}
	w.frame = w.enc.EncodeAll(p, slices.Grow(w.frame[:0], pageHeaderSize)[:pageHeaderSize])
	copy(w.frame, pageMagic)
	binary.LittleEndian.PutUint32(w.frame[4:8], uint32(len(w.frame)-pageHeaderSize)) //nolint:gosec // Zstandard frame overhead keeps a page below the 17 MiB limit.
	binary.LittleEndian.PutUint32(w.frame[8:12], uint32(len(p)))                     //nolint:gosec // WritePage bounds the uncompressed length to 16 MiB.
	n, err := w.dst.Write(w.frame)
	if err == nil && n != len(w.frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("page: write page: %w", err)
	}
	return nil
}

// Close releases the writer's resources. The caller retains ownership of the
// underlying writer.
func (w *Writer) Close() error {
	if w.dst == nil {
		return io.ErrClosedPipe
	}
	err := w.enc.Close()
	*w = Writer{}
	return err
}

// Reader reads and decompresses one page at a time. Use NewReader to
// construct it and call its methods serially.
type Reader struct {
	src   io.Reader
	dec   *zstd.Decoder
	hdr   [pageHeaderSize]byte
	frame []byte
	raw   []byte
}

// NewReader creates a page reader over src. The caller owns src.
func NewReader(src io.Reader) (*Reader, error) {
	if src == nil {
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
	return &Reader{src: src, dec: d}, nil
}

// ReadPage returns the next decompressed page. Its read-only bytes are valid
// until the next ReadPage call or Close. A clean end of stream returns io.EOF.
// ReadPage reads only the current page. A decompression or decoded-size error
// leaves the source at the next page. After an earlier failure, resume at a
// known page boundary.
func (r *Reader) ReadPage() ([]byte, error) {
	if r.src == nil {
		return nil, io.ErrClosedPipe
	}
	h := r.hdr[:]
	if _, err := io.ReadFull(r.src, h); err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("page: read page header: %w", err)
	}
	if string(h[:4]) != pageMagic {
		return nil, fmt.Errorf("page: invalid page magic or version %x", h[:4])
	}
	cl := binary.LittleEndian.Uint32(h[4:8])
	rl := binary.LittleEndian.Uint32(h[8:12])
	if cl == 0 || cl > maxCompressedPageSize {
		return nil, fmt.Errorf("page: invalid compressed page size %d", cl)
	}
	if rl == 0 || rl > maxPageSize {
		return nil, fmt.Errorf("page: invalid raw page size %d", rl)
	}
	r.frame = slices.Grow(r.frame[:0], int(cl))[:cl]
	if _, err := io.ReadFull(r.src, r.frame); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("page: read page payload: %w", err)
	}
	r.raw = slices.Grow(r.raw[:0], int(rl))
	b, err := r.dec.DecodeAll(r.frame, r.raw[:0:rl])
	if err != nil {
		return nil, fmt.Errorf("page: decompress page: %w", err)
	}
	if len(b) != int(rl) {
		return nil, fmt.Errorf("page: raw page size %d does not match %d decoded bytes", rl, len(b))
	}
	return b, nil
}

// Iter returns a single-use iterator over the remaining decompressed pages.
// Restarting the iterator resumes at the reader's current position. It
// shares the borrowed byte lifetime of ReadPage. A clean EOF ends iteration.
// Any other error is yielded with a nil page and ends iteration.
func (r *Reader) Iter() iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for {
			b, err := r.ReadPage()
			if err == io.EOF || !yield(b, err) || err != nil { //nolint:errorlint // Wrapped EOF is an input error.
				return
			}
		}
	}
}

// Close releases the decoder and its buffers. The caller retains ownership
// of the underlying reader.
func (r *Reader) Close() error {
	if r.src == nil {
		return io.ErrClosedPipe
	}
	r.dec.Close()
	*r = Reader{}
	return nil
}

// An empty stream has no bytes. Each page starts with PBP and the version
// byte, then unsigned 32-bit little-endian compressed and raw lengths.
// The compressed length counts the following Zstandard payload. The
// writer emits one checksummed frame per page with no dictionary.
const (
	pageMagic             = "PBP\x02"
	pageHeaderSize        = 12
	maxPageSize           = 16 << 20
	maxCompressedPageSize = 17 << 20
)
