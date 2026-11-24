package page

import (
	"errors"
	"fmt"
	"io"
)

// StreamWriter buffers a byte stream into independently compressed pages.
// Use NewStreamWriter to construct it and call its methods serially.
type StreamWriter struct {
	w      *Writer
	raw    []byte
	target int
	err    error
}

// NewStreamWriter creates a stream writer over dst. The target page size is
// in uncompressed bytes. Pass zero for 256 KiB. The caller owns dst.
func NewStreamWriter(dst io.Writer, pageSize int) (*StreamWriter, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return nil, fmt.Errorf("page: page size %d is outside 0..%d bytes", pageSize, maxPageSize)
	}
	if pageSize == 0 {
		pageSize = 256 << 10
	}
	w, err := NewWriter(dst)
	if err != nil {
		return nil, err
	}
	return &StreamWriter{w: w, target: pageSize}, nil
}

// Write accepts bytes into pages of the configured size and returns the number
// of bytes accepted. A write failure is terminal through Close.
func (w *StreamWriter) Write(p []byte) (int, error) {
	if w.w == nil {
		return 0, io.ErrClosedPipe
	}
	if w.err != nil {
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

// Flush writes the pending page. The caller controls any buffering and
// synchronization at the underlying writer.
func (w *StreamWriter) Flush() error {
	if w.w == nil {
		return io.ErrClosedPipe
	}
	if w.err != nil || len(w.raw) == 0 {
		return w.err
	}
	if err := w.w.WritePage(w.raw); err != nil {
		w.err = err
		return err
	}
	w.raw = w.raw[:0]
	return nil
}

// Close flushes the pending page and releases the writer's resources. The
// caller retains ownership of the underlying writer.
func (w *StreamWriter) Close() error {
	if w.w == nil {
		return io.ErrClosedPipe
	}
	err := errors.Join(w.Flush(), w.w.Close())
	*w = StreamWriter{}
	return err
}

// StreamReader joins decompressed pages into a byte stream. Use
// NewStreamReader to construct it and call its methods serially.
type StreamReader struct {
	r       *Reader
	pending []byte
}

// NewStreamReader creates a stream reader over src. The caller owns src.
func NewStreamReader(src io.Reader) (*StreamReader, error) {
	r, err := NewReader(src)
	if err != nil {
		return nil, err
	}
	return &StreamReader{r: r}, nil
}

// Read reads bytes from the decompressed stream. A clean end of stream
// returns io.EOF. After an error, resume at a known page boundary.
func (r *StreamReader) Read(p []byte) (int, error) {
	if r.r == nil {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		b, err := r.r.ReadPage()
		if err != nil {
			return 0, err
		}
		r.pending = b
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Close releases the reader's resources. The caller retains ownership of the
// underlying reader.
func (r *StreamReader) Close() error {
	if r.r == nil {
		return io.ErrClosedPipe
	}
	err := r.r.Close()
	*r = StreamReader{}
	return err
}
