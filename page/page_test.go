package page

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestWriter(t *testing.T) {
	t.Parallel()
	t.Run("Write", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewWriter(&dst, 4)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := w.Write([]byte{0xff, 0xfe}); n != 2 || err != nil {
				t.Fatalf("Write returned %d, %v, want 2, nil", n, err)
			}
			p := []byte("abcdefghijk")
			if n, err := w.Write(p); n != len(p) || err != nil {
				t.Fatalf("Write returned %d, %v, want %d, nil", n, err, len(p))
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			wire := dst.Bytes()
			for i, rawSize := range []uint32{4, 4, 4, 1} {
				if len(wire) < 12 || string(wire[:4]) != "PBP\x02" || binary.LittleEndian.Uint32(wire[8:12]) != rawSize {
					t.Fatalf("invalid v2 header for page %d in %x", i, wire)
				}
				compressedSize := binary.LittleEndian.Uint32(wire[4:8])
				if compressedSize == 0 || int64(compressedSize) > int64(len(wire)-12) {
					t.Fatalf("invalid compressed size %d for page %d", compressedSize, i)
				}
				wire = wire[12+compressedSize:]
			}
			if len(wire) != 0 {
				t.Fatalf("unexpected trailing %d bytes", len(wire))
			}
			r, err := NewReader(&dst)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			var first [1]byte
			if n, err := r.Read(first[:]); n != 1 || first[0] != 0xff || err != nil {
				t.Fatalf("Read returned %d, %v and wrote %x, want 1, nil and ff", n, err, first)
			}
			got, err := io.ReadAll(r)
			want := append([]byte{0xfe}, p...)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("remaining bytes = %x, %v, want %x", got, err, want)
			}
			if n, err := r.Read(first[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("Read after EOF returned %d, %v, want 0, EOF", n, err)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("reports accepted bytes on short write", func(t *testing.T) {
				t.Parallel()
				var dst shortPageWriter
				w, err := NewWriter(&dst, 3)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := w.Write([]byte("ab")); n != 2 || err != nil {
					t.Fatalf("Write returned %d, %v, want 2, nil", n, err)
				}
				dst.truncateNext = true
				if n, err := w.Write([]byte("cdef")); n != 1 || !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("Write returned %d, %v, want 1, short write", n, err)
				}
				size := dst.Len()
				if n, err := w.Write([]byte("d")); n != 0 || !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("Write after failure returned %d, %v, want 0, short write", n, err)
				}
				if err := w.Close(); !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("Close after failure returned %v, want short write", err)
				}
				if dst.Len() != size {
					t.Fatal("operation after a short write produced more data")
				}
			})
		})
	})
	t.Run("Flush", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewWriter(&dst, 4)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := w.Write(nil); n != 0 || err != nil {
				t.Fatalf("Write(nil) returned %d, %v, want 0, nil", n, err)
			}
			if n, err := w.Write([]byte("ab")); n != 2 || err != nil {
				t.Fatalf("Write returned %d, %v, want 2, nil", n, err)
			}
			if dst.Len() != 0 {
				t.Fatal("Write published an incomplete page before Flush")
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if dst.Len() == 0 {
				t.Fatal("Flush published no bytes")
			}
			if n, err := w.Write([]byte("c")); n != 1 || err != nil {
				t.Fatalf("Write after Flush returned %d, %v, want 1, nil", n, err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := NewReader(&dst)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			b, err := io.ReadAll(r)
			if string(b) != "abc" || err != nil {
				t.Fatalf("stream bytes = %q, %v, want abc, nil", b, err)
			}
		})
	})

	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var b bytes.Buffer
			w, err := NewWriter(&b, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if b.Len() != 0 {
				t.Fatalf("empty stream wrote %d bytes", b.Len())
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			w, err := NewWriter(io.Discard, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if n, err := w.Write([]byte{1}); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Write after Close returned %d, %v, want 0, closed pipe", n, err)
			}
			for _, err := range []error{w.Flush(), w.Close()} {
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("operation on closed writer returned %v, want io.ErrClosedPipe", err)
				}
			}
		})
	})
}

func TestNewWriter(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewWriter(nil, 0)
		checkErrorContains(t, err, "nil page writer destination")
		_, err = NewWriter(io.Discard, -1)
		checkErrorContains(t, err, "page size -1 is outside")
		_, err = NewWriter(io.Discard, (16<<20)+1)
		checkErrorContains(t, err, "page size 16777217 is outside")
	})
}

func TestNewReader(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewReader(nil)
		checkErrorContains(t, err, "nil page reader source")
	})
}

func TestReader(t *testing.T) {
	t.Parallel()
	t.Run("Read", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			legacy := pageFixture(t)
			stream := pageFixture(t)
			stream[3] = 2
			r, err := NewReader(bytes.NewReader(append(legacy, stream...)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			if n, err := r.Read(nil); n != 0 || err != nil {
				t.Fatalf("Read(nil) returned %d, %v, want 0, nil", n, err)
			}
			got, err := io.ReadAll(r)
			want := bytes.Repeat([]byte{2, 8, 7, 0, 2, 8, 9}, 2)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("stream bytes = %x, %v, want %x", got, err, want)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("truncated v2 tail", func(t *testing.T) {
				t.Parallel()
				page := pageFixture(t)
				page[3] = 2
				r, err := NewReader(bytes.NewReader(append(bytes.Clone(page), page[:len(page)-1]...)))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				got, err := io.ReadAll(r)
				want := []byte{2, 8, 7, 0, 2, 8, 9}
				if !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(got, want) {
					t.Fatalf("stream bytes = %x, %v, want %x and unexpected EOF", got, err, want)
				}
			})
			b := pageFixture(t)
			replace := func(i int, v byte) []byte {
				p := bytes.Clone(b)
				p[i] = v
				return p
			}
			// The compressed bytes decode to 03 08 01, a length prefix
			// for a 3-byte record followed by only 2 bytes.
			length := decodePageFixture(t, "50425001100000000300000028b52ffd240319000003080111145868")
			// These payloads contain an incomplete varint and ten continuation bytes.
			truncated := decodePageFixture(t, "504250010e0000000100000028b52ffd0458090000809f846070")
			overflow := decodePageFixture(t, "50425001170000000a00000028b52ffd0458510000808080808080808080803ca37159")
			for _, tc := range []struct {
				name string
				data []byte
				is   error
				msg  string
			}{
				{name: "short header", data: b[:3], is: io.ErrUnexpectedEOF, msg: "read page header"},
				{name: "missing payload", data: b[:12], is: io.ErrUnexpectedEOF, msg: "read page payload"},
				{name: "short payload", data: b[:len(b)-1], is: io.ErrUnexpectedEOF, msg: "read page payload"},
				{name: "unsupported version", data: replace(3, 3), msg: "invalid page magic or version"},
				{name: "zero compressed size", data: replace(4, 0), msg: "invalid compressed page size"},
				{name: "huge compressed size", data: replace(7, 2), msg: "invalid compressed page size"},
				{name: "zero raw size", data: replace(8, 0), msg: "invalid raw page size"},
				{name: "huge raw size", data: replace(11, 2), msg: "invalid raw page size"},
				{name: "understated raw size", data: replace(8, 1), is: zstd.ErrDecoderSizeExceeded, msg: "decompress page"},
				{name: "overstated raw size", data: replace(8, 8), msg: "raw page size 8 does not match 7 decoded bytes"},
				{name: "checksum damage", data: replace(len(b)-1, b[len(b)-1]^1), is: zstd.ErrCRCMismatch, msg: "decompress page"},
				{name: "record extends into next page", data: append(length, b...), is: io.ErrUnexpectedEOF, msg: "read record length"},
				{name: "truncated length", data: append(truncated, b...), is: io.ErrUnexpectedEOF, msg: "read record length"},
				{name: "overflowing length", data: overflow, msg: "read record length: varint overflow"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					r, err := NewReader(bytes.NewReader(tc.data))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = r.Close() })
					var p [1]byte
					n, first := r.Read(p[:])
					if n != 0 {
						t.Fatalf("Read returned %d bytes with an error", n)
					}
					if tc.is != nil && !errors.Is(first, tc.is) {
						t.Fatalf("Read returned %v, want %v", first, tc.is)
					}
					checkErrorContains(t, first, tc.msg)
					if n, err := r.Read(p[:]); n != 0 || err != first { //nolint:errorlint // A terminal error is returned unchanged.
						t.Fatalf("Read after a terminal error returned %d, %v, want 0, %v", n, err, first)
					}
				})
			}
		})
	})

	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			r, err := NewReader(bytes.NewReader(pageFixture(t)))
			if err != nil {
				t.Fatal(err)
			}
			var p [1]byte
			if n, err := r.Read(p[:]); n != 1 || err != nil {
				t.Fatalf("Read returned %d, %v before Close", n, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if n, err := r.Read(p[:]); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Read after Close returned %d, %v, want 0, closed pipe", n, err)
			}
			if err := r.Close(); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Close returned %v, want io.ErrClosedPipe", err)
			}
		})
	})
}

type shortPageWriter struct {
	bytes.Buffer

	truncateNext bool
}

func (w *shortPageWriter) Write(b []byte) (int, error) {
	if w.truncateNext {
		w.truncateNext = false
		b = b[:5]
	}
	return w.Buffer.Write(b)
}

func pageFixture(t *testing.T) []byte {
	// The Zstandard CLI compressed 02 08 07 00 02 08 09 with a checksum.
	return decodePageFixture(t, "50425001140000000700000028b52ffd2407390000020807000208092e67ee6c")
}

func decodePageFixture(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkErrorContains(t *testing.T, err error, want string) {
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want an error containing %q", err, want)
	}
}
