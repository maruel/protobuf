package protobuf

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type pageEvent struct {
	Seq  uint64 `protobuf:"1"`
	Text string `protobuf:"2"`
	Data []byte `protobuf:"3"`
}

func TestWriter(t *testing.T) {
	t.Parallel()
	t.Run("Write", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
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
				w, err := NewWriter[pageEvent](f, 3)
				if err != nil {
					t.Fatal(err)
				}
				want := []pageEvent{{Seq: 7}, {}, {Seq: 9, Text: "larger than target", Data: []byte{1, 2}}}
				for i := range want[:2] {
					if err := w.Write(&want[i]); err != nil {
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
				if err := w.Write(&want[2]); err != nil {
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
			t.Run("invalid input preserves pending events", func(t *testing.T) {
				t.Parallel()
				var b bytes.Buffer
				w, err := NewWriter[pageEvent](&b, 1)
				if err != nil {
					t.Fatal(err)
				}
				want := []pageEvent{{Seq: 7}, {Seq: 9}}
				if err := w.Write(&want[0]); err != nil {
					t.Fatal(err)
				}
				checkErrorContains(t, w.Write(nil), "nil page record")
				checkErrorContains(t, w.Write(&pageEvent{Text: "\xff"}), "invalid UTF-8")
				checkErrorContains(t, w.Write(&pageEvent{Data: make([]byte, 16<<20)}), "record exceeds the 16777216-byte page limit")
				if b.Len() != 0 {
					t.Fatal("rejected input published the pending page")
				}
				if err := w.Write(&want[1]); err != nil {
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
				w, err := NewWriter[pageEvent](&dst, 3)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.Write(&pageEvent{Seq: 7}); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				if err := w.Write(&pageEvent{Seq: 9}); err != nil {
					t.Fatal(err)
				}
				dst.truncateNext = true
				err = w.Write(&pageEvent{Seq: 11})
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("Write returned %v, want io.ErrShortWrite", err)
				}
				n := dst.Len()
				for _, got := range []error{w.Write(&pageEvent{}), w.Flush(), w.Close()} {
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
				var e pageEvent
				if err := r.Read(&e); err != nil || e.Seq != 7 {
					t.Fatalf("previous page yielded %#v, %v, want Seq 7", e, err)
				}
				if err := r.Read(&e); !errors.Is(err, io.ErrUnexpectedEOF) {
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
			w, err := NewWriter[pageEvent](&b, 0)
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
			w, err := NewWriter[pageEvent](io.Discard, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			for _, err := range []error{w.Write(&pageEvent{Seq: 1}), w.Flush(), w.Close()} {
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
		_, err := NewWriter[pageEvent](nil, 0)
		checkErrorContains(t, err, "nil page writer destination")
		_, err = NewWriter[pageEvent](io.Discard, -1)
		checkErrorContains(t, err, "page size -1 is outside")
		_, err = NewWriter[pageEvent](io.Discard, (16<<20)+1)
		checkErrorContains(t, err, "page size 16777217 is outside")
		_, err = NewWriter[*pageEvent](io.Discard, 0)
		checkErrorContains(t, err, "not a named struct")
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
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("invalid destinations can be retried", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(bytes.NewReader(pageFixture(t)))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				checkErrorContains(t, r.Read[pageEvent](nil), "nil page record destination")
				var dst string
				checkErrorContains(t, r.Read(&dst), "not a named struct")
				type incompatibleEvent struct {
					Seq string `protobuf:"1"`
				}
				pr := incompatibleEvent{Seq: "previous"}
				checkErrorContains(t, r.Read(&pr), "Seq: wire type 0, expected 2")
				if pr.Seq != "previous" {
					t.Fatalf("destination changed to %#v after failure", pr)
				}
				checkPageRecords(t, r, []pageEvent{{Seq: 7}, {}, {Seq: 9}})
			})
			t.Run("malformed message stays pending", func(t *testing.T) {
				t.Parallel()
				// The record contains only its field tag and omits the value.
				record := decodePageFixture(t, "504250010f0000000200000028b52ffd045811000001082bca2162")
				r, err := NewReader(bytes.NewReader(record))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close() })
				e := pageEvent{Seq: 77}
				for range 2 {
					err := r.Read(&e)
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("Read returned %v, want io.ErrUnexpectedEOF", err)
					}
					checkErrorContains(t, err, "decode page record")
					if e.Seq != 77 {
						t.Fatalf("destination changed to %#v after failure", e)
					}
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
				{name: "malformed length", data: length, is: io.ErrUnexpectedEOF, msg: "read record length"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					r, err := NewReader(bytes.NewReader(tc.data))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = r.Close() })
					e := pageEvent{Seq: 77}
					first := r.Read(&e)
					if tc.is != nil && !errors.Is(first, tc.is) {
						t.Fatalf("Read returned %v, want %v", first, tc.is)
					}
					checkErrorContains(t, first, tc.msg)
					if err := r.Read(&e); err != first { //nolint:errorlint // A terminal error is returned unchanged.
						t.Fatalf("Read after a terminal error returned %v, want %v", err, first)
					}
					if e.Seq != 77 {
						t.Fatalf("destination changed to %#v after failure", e)
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
			var got pageEvent
			if err := r.Read(&got); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := r.Read(&got); !errors.Is(err, io.ErrClosedPipe) || got.Seq != 7 {
				t.Fatalf("Read after Close returned %#v, %v, want the previous record and io.ErrClosedPipe", got, err)
			}
			if err := r.Close(); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Close returned %v, want io.ErrClosedPipe", err)
			}
		})
	})

	t.Run("Events", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			r, err := NewReader(bytes.NewReader(pageFixture(t)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			var first uint64
			for e, err := range r.Events[pageEvent]() {
				if err != nil {
					t.Fatal(err)
				}
				first = e.Seq
				break
			}
			if first != 7 {
				t.Fatalf("first event sequence = %d, want 7", first)
			}
			type projection struct {
				Sequence uint32 `protobuf:"1"`
			}
			next := projection{Sequence: 99}
			if err := r.Read(&next); err != nil || next.Sequence != 0 {
				t.Fatalf("resumed Read returned %#v, %v, want a zero value", next, err)
			}
			var got []uint32
			for e, err := range r.Events[projection]() {
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, e.Sequence)
			}
			if !reflect.DeepEqual(got, []uint32{9}) {
				t.Fatalf("remaining sequences = %v, want [9]", got)
			}
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			r, err := NewReader(bytes.NewReader(pageFixture(t)[:4]))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			n := 0
			for e, err := range r.Events[pageEvent]() {
				if !errors.Is(err, io.ErrUnexpectedEOF) || !reflect.DeepEqual(e, pageEvent{}) {
					t.Fatalf("Events yielded %#v, %v, want a zero event and io.ErrUnexpectedEOF", e, err)
				}
				n++
			}
			if n != 1 {
				t.Fatalf("Events yielded %d results, want one", n)
			}
		})
	})
}

func BenchmarkWriter(b *testing.B) {
	events, n := pageBenchmarkEvents(b)
	b.Run("Write", func(b *testing.B) {
		for _, size := range []int{16 << 10, 256 << 10} {
			b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
				w, err := NewWriter[pageEvent](io.Discard, size)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := w.Close(); err != nil {
						b.Error(err)
					}
				})
				writeBatch := func() {
					for i := range events {
						if err := w.Write(&events[i]); err != nil {
							b.Fatal(err)
						}
					}
					if err := w.Flush(); err != nil {
						b.Fatal(err)
					}
				}
				writeBatch()
				b.SetBytes(n)
				b.ReportAllocs()
				for b.Loop() {
					writeBatch()
				}
				b.ReportMetric(float64(len(events)), "events/op")
			})
		}
	})
}

func BenchmarkReader(b *testing.B) {
	events, n := pageBenchmarkEvents(b)
	b.Run("Read", func(b *testing.B) {
		for _, size := range []int{16 << 10, 256 << 10} {
			b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
				var dst bytes.Buffer
				w, err := NewWriter[pageEvent](&dst, size)
				if err != nil {
					b.Fatal(err)
				}
				for i := range events {
					if err := w.Write(&events[i]); err != nil {
						b.Fatal(err)
					}
				}
				if err := w.Close(); err != nil {
					b.Fatal(err)
				}
				data := dst.Bytes()
				src := bytes.NewReader(data)
				r, err := NewReader(src)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := r.Close(); err != nil {
						b.Error(err)
					}
				})
				var got pageEvent
				readBatch := func() {
					// Rewind at a page boundary before Read observes EOF.
					src.Reset(data)
					for range events {
						if err := r.Read(&got); err != nil {
							b.Fatal(err)
						}
					}
				}
				readBatch()
				b.SetBytes(n)
				b.ReportAllocs()
				for b.Loop() {
					readBatch()
				}
				b.ReportMetric(float64(len(events)), "events/op")
			})
		}
	})
}

// pageBenchmarkEvents returns a batch and its uncompressed protobuf payload
// size. Recurring event types and distinct request IDs model a request log.
// Each batch is flushed, so its last page can be smaller than the target.
func pageBenchmarkEvents(b *testing.B) (events []pageEvent, payloadBytes int64) {
	names := [...]string{"request started", "request completed", "request retried"}
	events = make([]pageEvent, 2048)
	for i := range events {
		e := &events[i]
		e.Seq = uint64(i + 1)
		e.Text = names[i%len(names)]
		e.Data = fmt.Appendf(nil, `{"service":"ingest","path":"/v1/events","method":"POST","region":"us-east-1","host":"ingest-%02d","request_id":"%08x","duration_ms":%d}`,
			i%32, uint32(i)*0x9e3779b1, i%1000)
		data, err := Marshal(e)
		if err != nil {
			b.Fatal(err)
		}
		payloadBytes += int64(len(data))
	}
	return events, payloadBytes
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

func checkPageRecords(t *testing.T, r *Reader, want []pageEvent) {
	for i := range want {
		var got pageEvent
		if err := r.Read(&got); err != nil {
			t.Fatalf("Read record %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("record %d is %#v, want %#v", i, got, want[i])
		}
	}
	var last pageEvent
	if err := r.Read(&last); err != io.EOF { //nolint:errorlint // A clean page boundary returns the exact EOF sentinel.
		t.Fatalf("after reading %d records, Read returned %v, want io.EOF", len(want), err)
	}
}
