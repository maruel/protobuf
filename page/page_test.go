package page

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestWriter(t *testing.T) {
	t.Parallel()
	t.Run("WritePage", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewWriter(&dst)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Close() })
			if err := w.WritePage([]byte("first")); err != nil {
				t.Fatal(err)
			}
			if dst.Len() == 0 {
				t.Fatal("WritePage did not publish the page")
			}
			if err := w.WritePage([]byte("second")); err != nil {
				t.Fatal(err)
			}
			r, err := NewReader(&dst)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			for _, want := range []string{"first", "second"} {
				got, err := r.ReadPage()
				if err != nil || string(got) != want {
					t.Fatalf("ReadPage = %q, %v, want %q", got, err, want)
				}
			}
			if got, err := r.ReadPage(); err != io.EOF || got != nil { //nolint:errorlint // Clean EOF is the exact sentinel.
				t.Fatalf("ReadPage = %x, %v, want nil, EOF", got, err)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("invalid sizes", func(t *testing.T) {
				t.Parallel()
				var dst bytes.Buffer
				w, err := NewWriter(&dst)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = w.Close() })
				checkErrorContains(t, w.WritePage(nil), "raw page size 0 is outside")
				checkErrorContains(t, w.WritePage(make([]byte, (16<<20)+1)), "raw page size 16777217 is outside")
				if dst.Len() != 0 {
					t.Fatal("rejected page reached the destination")
				}
			})
			t.Run("retry at a page boundary", func(t *testing.T) {
				t.Parallel()
				dst := shortPageWriter{truncateNext: true}
				w, err := NewWriter(&dst)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = w.Close() })
				if err := w.WritePage([]byte("first")); !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("WritePage = %v, want io.ErrShortWrite", err)
				}
				dst.Reset()
				if err := w.WritePage([]byte("second")); err != nil {
					t.Fatal(err)
				}
				r, err := NewReader(&dst)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				if got, err := r.ReadPage(); err != nil || string(got) != "second" {
					t.Fatalf("ReadPage = %q, %v, want second", got, err)
				}
			})
		})
	})
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			w, err := NewWriter(io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := w.WritePage([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("WritePage after Close = %v, want io.ErrClosedPipe", err)
			}
			if err := w.Close(); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Close after Close = %v, want io.ErrClosedPipe", err)
			}
		})
	})
}

func TestNewWriter(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewWriter(nil)
		checkErrorContains(t, err, "nil page writer destination")
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
	t.Run("ReadPage", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			b := pageFixture(t)
			replace := func(i int, v byte) []byte {
				p := bytes.Clone(b)
				p[i] = v
				return p
			}
			for _, tc := range []struct {
				name string
				data []byte
				is   error
				msg  string
			}{
				{name: "short header", data: b[:3], is: io.ErrUnexpectedEOF, msg: "read page header"},
				{name: "missing payload", data: b[:12], is: io.ErrUnexpectedEOF, msg: "read page payload"},
				{name: "short payload", data: b[:len(b)-1], is: io.ErrUnexpectedEOF, msg: "read page payload"},
				{name: "old version", data: replace(3, 1), msg: "invalid page magic or version"},
				{name: "zero compressed size", data: replace(4, 0), msg: "invalid compressed page size"},
				{name: "huge compressed size", data: replace(7, 2), msg: "invalid compressed page size"},
				{name: "zero raw size", data: replace(8, 0), msg: "invalid raw page size"},
				{name: "huge raw size", data: replace(11, 2), msg: "invalid raw page size"},
				{name: "understated raw size", data: replace(8, 1), is: zstd.ErrDecoderSizeExceeded, msg: "decompress page"},
				{name: "overstated raw size", data: replace(8, 8), msg: "raw page size 8 does not match 7 decoded bytes"},
				{name: "checksum damage", data: replace(len(b)-1, b[len(b)-1]^1), is: zstd.ErrCRCMismatch, msg: "decompress page"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					r, err := NewReader(bytes.NewReader(tc.data))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = r.Close() })
					p, err := r.ReadPage()
					if p != nil || (tc.is != nil && !errors.Is(err, tc.is)) {
						t.Fatalf("ReadPage returned %x, %v, want nil and %v", p, err, tc.is)
					}
					checkErrorContains(t, err, tc.msg)
				})
			}
		})
	})
	t.Run("Iter", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			b := pageFixture(t)
			r, err := NewReader(bytes.NewReader(bytes.Repeat(b, 2)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			want := []byte{2, 8, 7, 0, 2, 8, 9}
			pages := r.Iter()
			for p, err := range pages {
				if err != nil || !bytes.Equal(p, want) {
					t.Fatalf("first page = %x, %v, want %x, nil", p, err, want)
				}
				break
			}
			n := 0
			for p, err := range pages {
				if err != nil || !bytes.Equal(p, want) {
					t.Fatalf("next page = %x, %v, want %x, nil", p, err, want)
				}
				n++
			}
			if n != 1 {
				t.Fatalf("Iter yielded %d pages after early stop, want one", n)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			b := pageFixture(t)
			bad := bytes.Clone(b)
			bad[len(bad)-1] ^= 1
			r, err := NewReader(bytes.NewReader(append(bad, b...)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			n := 0
			for p, err := range r.Iter() {
				if p != nil || !errors.Is(err, zstd.ErrCRCMismatch) {
					t.Fatalf("Iter yielded %x, %v, want nil and checksum error", p, err)
				}
				n++
			}
			if n != 1 {
				t.Fatalf("Iter yielded %d results, want one error", n)
			}
			want := []byte{2, 8, 7, 0, 2, 8, 9}
			if p, err := r.ReadPage(); err != nil || !bytes.Equal(p, want) {
				t.Fatalf("ReadPage after checksum error = %x, %v, want %x", p, err, want)
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
			if _, err := r.ReadPage(); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if p, err := r.ReadPage(); p != nil || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("ReadPage after Close = %x, %v, want nil, closed pipe", p, err)
			}
			if err := r.Close(); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Close after Close = %v, want io.ErrClosedPipe", err)
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
	b, err := hex.DecodeString("50425002140000000700000028b52ffd2407390000020807000208092e67ee6c")
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
