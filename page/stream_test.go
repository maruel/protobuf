package page

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestStreamWriter(t *testing.T) {
	t.Parallel()
	t.Run("Write", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewStreamWriter(&dst, 4)
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
			r, err := NewStreamReader(&dst)
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
				w, err := NewStreamWriter(&dst, 3)
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
			w, err := NewStreamWriter(&dst, 4)
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
			r, err := NewStreamReader(&dst)
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
			w, err := NewStreamWriter(&b, 0)
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
			w, err := NewStreamWriter(io.Discard, 0)
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

func TestNewStreamWriter(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewStreamWriter(nil, 0)
		checkErrorContains(t, err, "nil page writer destination")
		_, err = NewStreamWriter(io.Discard, -1)
		checkErrorContains(t, err, "page size -1 is outside")
		_, err = NewStreamWriter(io.Discard, (16<<20)+1)
		checkErrorContains(t, err, "page size 16777217 is outside")
	})
}

func TestNewStreamReader(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewStreamReader(nil)
		checkErrorContains(t, err, "nil page reader source")
	})
}

func TestStreamReader(t *testing.T) {
	t.Parallel()
	t.Run("Read", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var src bytes.Buffer
			r, err := NewStreamReader(&src)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			if n, err := r.Read(nil); n != 0 || err != nil {
				t.Fatalf("Read(nil) returned %d, %v, want 0, nil", n, err)
			}
			if got, err := io.ReadAll(r); len(got) != 0 || err != nil {
				t.Fatalf("empty stream returned %x, %v", got, err)
			}
			src.Write(pageFixture(t))
			got, err := io.ReadAll(r)
			want := []byte{2, 8, 7, 0, 2, 8, 9}
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("stream after EOF = %x, %v, want %x", got, err, want)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			p := pageFixture(t)
			r, err := NewStreamReader(bytes.NewReader(append(bytes.Clone(p), p[:len(p)-1]...)))
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
	})
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			r, err := NewStreamReader(bytes.NewReader(pageFixture(t)))
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
