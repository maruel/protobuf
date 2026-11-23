package protobuf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/maruel/protobuf/page"
)

type streamEvent struct {
	Seq  uint64 `protobuf:"1"`
	Text string `protobuf:"2"`
	Data []byte `protobuf:"3"`
}

func TestNewWriter(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewWriter[streamEvent](nil)
		checkErrorContains(t, err, "nil record writer")
		_, err = NewWriter[*streamEvent](&recordBuffer{})
		checkErrorContains(t, err, "not a named struct")
	})
}

func TestWriter(t *testing.T) {
	t.Parallel()
	t.Run("Write", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst recordBuffer
			w, err := NewWriter[streamEvent](&dst)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range []streamEvent{{Seq: 7, Text: "ok"}, {Seq: 9}, {}} {
				if err := w.Write(&e); err != nil {
					t.Fatal(err)
				}
			}
			want := [][]byte{{0x08, 7, 0x12, 2, 'o', 'k'}, {0x08, 9}, nil}
			if !slices.EqualFunc(dst.records, want, bytes.Equal) {
				t.Fatalf("written records = %x, want %x", dst.records, want)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			var dst recordBuffer
			w, err := NewWriter[streamEvent](&dst)
			if err != nil {
				t.Fatal(err)
			}
			checkErrorContains(t, w.Write(nil), "nil record")
			checkErrorContains(t, w.Write(&streamEvent{Seq: 7, Text: "\xff"}), "invalid UTF-8")
			dst.err = io.ErrShortWrite
			if err := w.Write(&streamEvent{Seq: 9}); !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("Write returned %v, want io.ErrShortWrite", err)
			}
			if len(dst.records) != 0 {
				t.Fatalf("rejected writes stored %x", dst.records)
			}
		})
	})
}

func TestNewReader(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewReader(nil)
		checkErrorContains(t, err, "nil record reader")
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
				r, err := NewReader(&recordBuffer{records: [][]byte{{0x08, 7}, nil, {0x08, 9}}})
				if err != nil {
					t.Fatal(err)
				}
				checkErrorContains(t, r.Read[streamEvent](nil), "nil record destination")
				var dst string
				checkErrorContains(t, r.Read(&dst), "not a named struct")
				type incompatibleEvent struct {
					Seq string `protobuf:"1"`
				}
				p := incompatibleEvent{Seq: "previous"}
				checkErrorContains(t, r.Read(&p), "Seq: wire type 0, expected 2")
				if p.Seq != "previous" {
					t.Fatalf("destination changed to %#v after failure", p)
				}
				for i, want := range []uint64{7, 0, 9} {
					e := streamEvent{Seq: 99}
					if err := r.Read(&e); err != nil || e.Seq != want {
						t.Fatalf("record %d yielded %#v, %v, want sequence %d", i, e, err, want)
					}
				}
			})
			t.Run("malformed message stays pending", func(t *testing.T) {
				t.Parallel()
				// The first record contains only a field tag and omits its value.
				r, err := NewReader(&recordBuffer{records: [][]byte{{0x08}, {0x08, 9}}})
				if err != nil {
					t.Fatal(err)
				}
				e := streamEvent{Seq: 77}
				for range 2 {
					err := r.Read(&e)
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("Read returned %v, want io.ErrUnexpectedEOF", err)
					}
					checkErrorContains(t, err, "decode record")
					if e.Seq != 77 {
						t.Fatalf("destination changed to %#v after failure", e)
					}
				}
			})
		})
	})

	t.Run("Events", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			r, err := NewReader(&recordBuffer{records: [][]byte{{0x08, 7}, nil, {0x08, 9}}})
			if err != nil {
				t.Fatal(err)
			}
			var first uint64
			for e, err := range r.Events[streamEvent]() {
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
			wrapped := fmt.Errorf("reading record from storage: %w", io.EOF)
			r, err := NewReader(&recordBuffer{err: wrapped})
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for e, err := range r.Events[streamEvent]() {
				if !errors.Is(err, wrapped) || !reflect.DeepEqual(e, streamEvent{}) {
					t.Fatalf("Events yielded %#v, %v, want a zero event and %v", e, err, wrapped)
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
	events, n := benchmarkEvents(b)
	b.Run("Write", func(b *testing.B) {
		for _, size := range []int{16 << 10, 256 << 10} {
			b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
				pw, err := page.NewWriter(io.Discard, size)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := pw.Close(); err != nil {
						b.Error(err)
					}
				})
				w, err := NewWriter[streamEvent](pw)
				if err != nil {
					b.Fatal(err)
				}
				writeBatch := func() {
					for i := range events {
						if err := w.Write(&events[i]); err != nil {
							b.Fatal(err)
						}
					}
					if err := pw.Flush(); err != nil {
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
	events, n := benchmarkEvents(b)
	b.Run("Read", func(b *testing.B) {
		for _, size := range []int{16 << 10, 256 << 10} {
			b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
				var dst bytes.Buffer
				pw, err := page.NewWriter(&dst, size)
				if err != nil {
					b.Fatal(err)
				}
				w, err := NewWriter[streamEvent](pw)
				if err != nil {
					b.Fatal(err)
				}
				for i := range events {
					if err := w.Write(&events[i]); err != nil {
						b.Fatal(err)
					}
				}
				if err := pw.Close(); err != nil {
					b.Fatal(err)
				}
				data := dst.Bytes()
				src := bytes.NewReader(data)
				pr, err := page.NewReader(src)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := pr.Close(); err != nil {
						b.Error(err)
					}
				})
				r, err := NewReader(pr)
				if err != nil {
					b.Fatal(err)
				}
				var got streamEvent
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

// benchmarkEvents returns a batch and its uncompressed protobuf payload
// size. Recurring event types and distinct request IDs model a request log.
// Each batch is flushed, so its last page can be smaller than the target.
func benchmarkEvents(b *testing.B) (events []streamEvent, payloadBytes int64) {
	names := [...]string{"request started", "request completed", "request retried"}
	events = make([]streamEvent, 2048)
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

type recordBuffer struct {
	records [][]byte
	err     error
}

func (b *recordBuffer) ReadRecord() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if len(b.records) == 0 {
		return nil, io.EOF
	}
	p := b.records[0]
	b.records = b.records[1:]
	return p, nil
}

func (b *recordBuffer) WriteRecord(p []byte) error {
	if b.err != nil {
		return b.err
	}
	b.records = append(b.records, bytes.Clone(p))
	return nil
}
