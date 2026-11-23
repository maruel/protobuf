package page

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestWriter(t *testing.T) {
	t.Parallel()
	t.Run("WriteRecord", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("version one framing", func(t *testing.T) {
				t.Parallel()
				var dst bytes.Buffer
				w, err := NewWriter(&dst, 0)
				if err != nil {
					t.Fatal(err)
				}
				record := bytes.Repeat([]byte{0xff}, 130)
				if err := w.WriteRecord(record); err != nil {
					t.Fatal(err)
				}
				clear(record)
				if err := w.WriteRecord(nil); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				wire := dst.Bytes()
				if len(wire) < 12 || string(wire[:4]) != "PBP\x01" ||
					int(binary.LittleEndian.Uint32(wire[4:8])) != len(wire)-12 ||
					binary.LittleEndian.Uint32(wire[8:12]) != 133 {
					t.Fatalf("unexpected v1 page header in %x", wire)
				}
				dec, err := zstd.NewReader(nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(dec.Close)
				got, err := dec.DecodeAll(wire[12:], nil)
				want := append([]byte{0x82, 0x01}, bytes.Repeat([]byte{0xff}, 130)...)
				want = append(want, 0)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("page payload = %x, %v, want %x", got, err, want)
				}
			})
			t.Run("multi-page file", func(t *testing.T) {
				t.Parallel()
				f, err := os.Create(filepath.Join(t.TempDir(), "events.pbz"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := f.Close(); err != nil {
						t.Error(err)
					}
				})
				w, err := NewWriter(f, 3)
				if err != nil {
					t.Fatal(err)
				}
				want := [][]byte{{0x08, 7}, {}, {0xff, 0x80, 1, 2}}
				for i := range want[:2] {
					if err := w.WriteRecord(want[i]); err != nil {
						t.Fatal(err)
					}
				}
				first, err := f.Seek(0, io.SeekCurrent)
				if err != nil {
					t.Fatal(err)
				}
				if first == 0 {
					t.Fatal("rolling over the page wrote no bytes")
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				second, err := f.Seek(0, io.SeekCurrent)
				if err != nil {
					t.Fatal(err)
				}
				if second <= first {
					t.Fatal("flushing pending records wrote no bytes")
				}
				if err := w.WriteRecord(want[2]); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				r, err := NewReader(f)
				if err != nil {
					t.Fatal(err)
				}
				checkPageRecords(t, r, want)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			})
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("oversized input preserves pending records", func(t *testing.T) {
				t.Parallel()
				var b bytes.Buffer
				w, err := NewWriter(&b, 1)
				if err != nil {
					t.Fatal(err)
				}
				want := [][]byte{{0x08, 7}, {0x08, 9}}
				if err := w.WriteRecord(want[0]); err != nil {
					t.Fatal(err)
				}
				checkErrorContains(t, w.WriteRecord(make([]byte, 16<<20)), "record exceeds the 16777216-byte page limit")
				if b.Len() != 0 {
					t.Fatal("rejected input published the pending page")
				}
				if err := w.WriteRecord(want[1]); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				r, err := NewReader(&b)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				checkPageRecords(t, r, want)
			})
			t.Run("short write during rollover is terminal", func(t *testing.T) {
				t.Parallel()
				var dst shortPageWriter
				w, err := NewWriter(&dst, 3)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.WriteRecord([]byte{0x08, 7}); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				if err := w.WriteRecord([]byte{0x08, 9}); err != nil {
					t.Fatal(err)
				}
				dst.truncateNext = true
				err = w.WriteRecord([]byte{0x08, 11})
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("WriteRecord returned %v, want io.ErrShortWrite", err)
				}
				n := dst.Len()
				for _, got := range []error{w.WriteRecord(nil), w.Flush(), w.Close()} {
					if !errors.Is(got, io.ErrShortWrite) {
						t.Fatalf("operation after a short write returned %v, want io.ErrShortWrite", got)
					}
				}
				if dst.Len() != n {
					t.Fatal("operation after a short write produced more data")
				}
				r, err := NewReader(&dst.Buffer)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				if b, err := r.ReadRecord(); err != nil || !bytes.Equal(b, []byte{0x08, 7}) {
					t.Fatalf("previous page yielded %x, %v, want 0807", b, err)
				}
				if _, err := r.ReadRecord(); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated tail returned %v, want io.ErrUnexpectedEOF", err)
				}
			})
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
			for _, err := range []error{w.WriteRecord([]byte{0x08, 1}), w.Flush(), w.Close()} {
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
	t.Run("ReadRecord", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			r, err := NewReader(bytes.NewReader(pageFixture(t)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			checkPageRecords(t, r, [][]byte{{0x08, 7}, nil, {0x08, 9}})
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
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
				{name: "missing payload", data: b[:12], is: io.ErrUnexpectedEOF, msg: "read page payload"},
				{name: "short payload", data: b[:len(b)-1], is: io.ErrUnexpectedEOF, msg: "read page payload"},
				{name: "unsupported version", data: replace(3, 2), msg: "invalid page magic or version"},
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
					b, first := r.ReadRecord()
					if b != nil {
						t.Fatalf("ReadRecord returned bytes %x with an error", b)
					}
					if tc.is != nil && !errors.Is(first, tc.is) {
						t.Fatalf("ReadRecord returned %v, want %v", first, tc.is)
					}
					checkErrorContains(t, first, tc.msg)
					if _, err := r.ReadRecord(); err != first { //nolint:errorlint // A terminal error is returned unchanged.
						t.Fatalf("ReadRecord after a terminal error returned %v, want %v", err, first)
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
			if _, err := r.ReadRecord(); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReadRecord(); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("ReadRecord after Close returned %v, want io.ErrClosedPipe", err)
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

func checkPageRecords(t *testing.T, r *Reader, want [][]byte) {
	for i, expected := range want {
		got, err := r.ReadRecord()
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("record %d = %x, %v, want %x", i, got, err, expected)
		}
	}
	if _, err := r.ReadRecord(); err != io.EOF { //nolint:errorlint // A clean page boundary returns the exact EOF sentinel.
		t.Fatalf("after %d records, ReadRecord returned %v, want io.EOF", len(want), err)
	}
}

func checkErrorContains(t *testing.T, err error, want string) {
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want an error containing %q", err, want)
	}
}
