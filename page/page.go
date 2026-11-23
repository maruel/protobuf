// Package page stores byte streams in independently compressed Zstandard pages.
package page

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
)

// Writer buffers data into independently compressed Zstandard pages.
// Use NewWriter to construct it and call its methods serially.
type Writer struct {
	dst    io.Writer
	enc    *zstd.Encoder
	target int
	raw    []byte
	frame  []byte
	err    error
}

// NewWriter creates a page writer. The target size is in uncompressed bytes.
// Pass zero for 256 KiB.
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

// Write buffers p as a byte stream in v2 pages, splitting writes at the
// configured page size. It returns the number of bytes accepted. A write
// failure from the underlying stream is terminal through Close.
func (w *Writer) Write(p []byte) (int, error) {
	if w.dst == nil {
		return 0, io.ErrClosedPipe
	}
	if w.err != nil || len(p) == 0 {
		return 0, w.err
	}
	n := 0
	for n < len(p) {
		if len(w.raw) == w.target {
			if err := w.Flush(); err != nil {
				return n, err
			}
		}
		k := min(w.target-len(w.raw), len(p)-n)
		w.raw = append(w.raw, p[n:n+k]...)
		n += k
	}
	return n, nil
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
	w.frame[3] = byte(streamVersion)
	binary.LittleEndian.PutUint32(w.frame[4:8], uint32(len(w.frame)-pageHeaderSize)) //nolint:gosec // Zstandard frame overhead keeps a page below the 17 MiB limit.
	binary.LittleEndian.PutUint32(w.frame[8:12], uint32(len(w.raw)))                 //nolint:gosec // Write bounds the raw page to 16 MiB.
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
// Subsequent calls to Write, Flush or Close return io.ErrClosedPipe.
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

// Reader reads data from a sequence of Zstandard pages. Use NewReader to
// construct it and call its methods serially.
type Reader struct {
	src     io.Reader
	dec     *zstd.Decoder
	frame   []byte
	raw     []byte
	pending []byte
	err     error
}

// NewReader creates a reader for a sequence of v1 or v2 Zstandard pages.
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

// Read returns the decompressed byte stream. For legacy v1 pages, Read
// validates the record framing and returns the payload including all length
// prefixes. Read returns io.EOF at a clean end of stream. End of stream and
// page read errors are terminal.
func (r *Reader) Read(p []byte) (int, error) {
	if r.src == nil {
		return 0, io.ErrClosedPipe
	}
	if r.err != nil || len(p) == 0 {
		return 0, r.err
	}
	if len(r.pending) == 0 {
		r.err = r.readPage()
		if r.err != nil {
			return 0, r.err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Close releases the decoder and its buffers. The caller retains ownership
// of the underlying reader.
// Subsequent calls to Read or Close return io.ErrClosedPipe.
func (r *Reader) Close() error {
	if r.src == nil {
		return io.ErrClosedPipe
	}
	r.dec.Close()
	*r = Reader{}
	return nil
}

// An empty stream has no bytes. Each page starts with PBP and one version
// byte, then unsigned 32-bit little-endian compressed and raw lengths.
// The compressed length counts the following Zstandard payload. The
// writer emits one checksummed frame per page with no dictionary.
//
// A v1 payload contains one or more complete records, each an unsigned
// base-128 varint length followed by that many bytes. Zero-length records
// are valid. A v2 payload is an arbitrary chunk of a byte stream.
// The compressed and raw lengths are limited to 17 MiB and 16 MiB respectively.
type pageVersion byte

const (
	legacyVersion pageVersion = 1
	streamVersion pageVersion = 2
)

const (
	pageMagic             = "PBP"
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
	version := pageVersion(hdr[3])
	if string(hdr[:3]) != pageMagic || (version != legacyVersion && version != streamVersion) {
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
	if version == legacyVersion {
		for rest := b; len(rest) != 0; {
			size, n := binary.Uvarint(rest)
			if n < 0 || (n == 0 && len(rest) >= binary.MaxVarintLen64) {
				return errors.New("page: read record length: varint overflow")
			}
			rest = rest[n:]
			if n == 0 || size > uint64(len(rest)) {
				return fmt.Errorf("page: read record length: %w", io.ErrUnexpectedEOF)
			}
			rest = rest[size:]
		}
	}
	r.pending = b
	return nil
}
