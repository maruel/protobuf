package protobuf

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
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
		_, err := NewWriter(nil)
		checkErrorContains(t, err, "nil record writer")
	})
}

func TestWriter(t *testing.T) {
	t.Parallel()
	t.Run("Write", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewWriter(&dst)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Write(&streamEvent{Seq: 7, Text: "ok"}); err != nil {
				t.Fatal(err)
			}
			type sequence struct {
				Number uint64 `protobuf:"1"`
			}
			if err := w.Write(&sequence{Number: 9}); err != nil {
				t.Fatal(err)
			}
			if err := w.Write(&streamEvent{}); err != nil {
				t.Fatal(err)
			}
			want := []byte{6, 0x08, 7, 0x12, 2, 'o', 'k', 2, 0x08, 9, 0}
			if !bytes.Equal(dst.Bytes(), want) {
				t.Fatalf("written stream = %x, want %x", dst.Bytes(), want)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewWriter(&dst)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Write(&streamEvent{Seq: 7}); err != nil {
				t.Fatal(err)
			}
			checkErrorContains(t, w.Write[streamEvent](nil), "nil record")
			var unsupported string
			checkErrorContains(t, w.Write(&unsupported), "not a named struct")
			checkErrorContains(t, w.Write(&streamEvent{Seq: 7, Text: "\xff"}), "invalid UTF-8")
			if want := []byte{2, 0x08, 7}; !bytes.Equal(dst.Bytes(), want) {
				t.Fatalf("stream after rejected writes = %x, want %x", dst.Bytes(), want)
			}
			if err := w.Write(&streamEvent{Seq: 9}); err != nil || !bytes.Equal(dst.Bytes(), []byte{2, 0x08, 7, 2, 0x08, 9}) {
				t.Fatalf("Write after rejected messages yielded %x, %v", dst.Bytes(), err)
			}
		})
	})
	t.Run("WriteRawFrom", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var dst bytes.Buffer
			w, err := NewWriter(&dst)
			if err != nil {
				t.Fatal(err)
			}
			src := bytes.NewReader([]byte{0x08, 7, 0xff})
			if err := w.WriteRawFrom(src, 2); err != nil {
				t.Fatal(err)
			}
			if err := w.WriteRawFrom(src, 0); err != nil {
				t.Fatal(err)
			}
			p, err := src.ReadByte()
			if p != 0xff || err != nil || !bytes.Equal(dst.Bytes(), []byte{2, 0x08, 7, 0}) {
				t.Fatalf("stream = %x, trailing byte = %x, error = %v", dst.Bytes(), p, err)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("invalid arguments", func(t *testing.T) {
				t.Parallel()
				var dst bytes.Buffer
				w, err := NewWriter(&dst)
				if err != nil {
					t.Fatal(err)
				}
				src := strings.NewReader("\x08\x07")
				checkErrorContains(t, w.WriteRawFrom(nil, 2), "nil record source")
				checkErrorContains(t, w.WriteRawFrom(src, -1), "record size -1 is outside")
				checkErrorContains(t, w.WriteRawFrom(src, 1<<31), "record size 2147483648 is outside")
				if dst.Len() != 0 || src.Len() != 2 {
					t.Fatalf("rejected writes changed destination or source: %x, %d unread", dst.Bytes(), src.Len())
				}
				if err := w.WriteRawFrom(src, 2); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("maximum size with truncated body", func(t *testing.T) {
				t.Parallel()
				var dst bytes.Buffer
				w, err := NewWriter(&dst)
				if err != nil {
					t.Fatal(err)
				}
				err = w.WriteRawFrom(strings.NewReader("\x08"), 1<<31-1)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("WriteRawFrom = %v, want io.ErrUnexpectedEOF", err)
				}
				if want := []byte{0xff, 0xff, 0xff, 0xff, 0x07, 0x08}; !bytes.Equal(dst.Bytes(), want) {
					t.Fatalf("partial stream = %x, want %x", dst.Bytes(), want)
				}
				dst.Reset()
				if err := w.Write(&streamEvent{Seq: 7}); err != nil || dst.String() != "\x02\x08\x07" {
					t.Fatalf("Write after resetting the destination = %x, %v", dst.Bytes(), err)
				}
			})
			t.Run("short prefix write", func(t *testing.T) {
				t.Parallel()
				w := Writer{Dst: writerFunc(func([]byte) (int, error) { return 0, nil })}
				if err := w.Write(&streamEvent{Seq: 7}); !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("Write = %v, want io.ErrShortWrite", err)
				}
				src := strings.NewReader("\x08\x07")
				if err := w.WriteRawFrom(src, 2); !errors.Is(err, io.ErrShortWrite) || src.Len() != 2 {
					t.Fatalf("WriteRawFrom with a short prefix write = %v with %d unread", err, src.Len())
				}
				var dst bytes.Buffer
				w.Dst = &dst
				if err := w.WriteRawFrom(src, 2); err != nil || src.Len() != 0 || dst.String() != "\x02\x08\x07" {
					t.Fatalf("WriteRawFrom retry = %x, %v with %d unread", dst.Bytes(), err, src.Len())
				}
			})
		})
	})
}

func TestNewReader(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		first := []byte{0x08, 7}
		r, err := NewReader(framedRecords(first))
		if err != nil {
			t.Fatal(err)
		}
		m, err := r.ReadRaw()
		if err != nil || !bytes.Equal(m, first) {
			t.Fatalf("ReadRaw yielded %x, %v, want the record %x", m, err, first)
		}
		var got streamEvent
		if err := m.Decode(&got); err != nil || got.Seq != 7 {
			t.Fatalf("Decode yielded %#v, %v, want sequence 7", got, err)
		}
		if _, err := r.ReadRaw(); err != io.EOF { //nolint:errorlint // A clean end of stream returns the exact EOF sentinel.
			t.Fatalf("ReadRaw returned %v, want io.EOF", err)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := NewReader(nil)
		checkErrorContains(t, err, "nil record reader")
	})
}

func TestReader(t *testing.T) {
	t.Parallel()
	t.Run("ReadRaw", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("continues after EOF", func(t *testing.T) {
				t.Parallel()
				var src bytes.Buffer
				r, err := NewReader(&src)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.ReadRaw(); err != io.EOF { //nolint:errorlint // A clean end of stream returns the exact EOF sentinel.
					t.Fatalf("ReadRaw returned %v, want io.EOF", err)
				}
				src.Write([]byte{2, 0x08, 7})
				if got, err := r.ReadRaw(); err != nil || !bytes.Equal(got, []byte{0x08, 7}) {
					t.Fatalf("ReadRaw after EOF = %x, %v, want 0807, nil", got, err)
				}
			})
			t.Run("reset source after a framing error", func(t *testing.T) {
				t.Parallel()
				r := Reader{Src: bufio.NewReader(strings.NewReader("\x03\x08\x07"))}
				if _, err := r.ReadRaw(); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("ReadRaw = %v, want io.ErrUnexpectedEOF", err)
				}
				r.Src.Reset(framedRecords([]byte{0x08, 9}))
				if got, err := r.ReadRaw(); err != nil || !bytes.Equal(got, []byte{0x08, 9}) {
					t.Fatalf("ReadRaw after resetting Src = %x, %v, want 0809, nil", got, err)
				}
			})
			t.Run("large buffered message", func(t *testing.T) {
				t.Parallel()
				b := bytes.Repeat([]byte{0x08, 7}, 4369)
				r, err := NewReader(framedRecords(b, []byte{0x08, 9}))
				if err != nil {
					t.Fatal(err)
				}
				got, err := r.ReadRaw()
				if err != nil || !bytes.Equal(got, b) {
					t.Fatalf("ReadRaw with %d bytes = %d bytes, %v", len(b), len(got), err)
				}
				if got, err := r.ReadRaw(); err != nil || !bytes.Equal(got, []byte{0x08, 9}) {
					t.Fatalf("following record = %x, %v", got, err)
				}
			})
			t.Run("legacy page", func(t *testing.T) {
				t.Parallel()
				b, err := hex.DecodeString("50425001140000000700000028b52ffd2407390000020807000208092e67ee6c")
				if err != nil {
					t.Fatal(err)
				}
				p, err := page.NewReader(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := p.Close(); err != nil {
						t.Error(err)
					}
				})
				r, err := NewReader(p)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []uint64{7, 0, 9} {
					var e streamEvent
					if err := r.Read(&e); err != nil || e.Seq != want {
						t.Fatalf("legacy sequence = %d, %v, want %d", e.Seq, err, want)
					}
				}
				if _, err := r.ReadRaw(); err != io.EOF { //nolint:errorlint // Require a clean end of stream.
					t.Fatalf("end of legacy stream = %v, want io.EOF", err)
				}
			})
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, tt := range []struct {
				name string
				data []byte
				want string
			}{
				{"truncated size", []byte{0x80}, "read record size: unexpected EOF"},
				{"overflowing size", bytes.Repeat([]byte{0xff}, 10), "overflows a 64-bit integer"},
				{"oversized record", binary.AppendUvarint(nil, 1<<31), "record size 2147483648 exceeds 2147483647 bytes"},
				{"truncated small body", []byte{3, 0x08, 7}, "read record body: unexpected EOF"},
				{"truncated maximum body", binary.AppendUvarint(nil, 1<<31-1), "read record body: unexpected EOF"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					r, err := NewReader(bytes.NewReader(tt.data))
					if err != nil {
						t.Fatal(err)
					}
					m, err := r.ReadRaw()
					checkErrorContains(t, err, tt.want)
					if m != nil {
						t.Fatalf("ReadRaw returned a partial message %x", m)
					}
				})
			}
		})
	})
	t.Run("ReadRawTo", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("empty record", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(framedRecords(nil, []byte{0x08, 9}))
				if err != nil {
					t.Fatal(err)
				}
				var dst bytes.Buffer
				if n, err := r.ReadRawTo(&dst); n != 0 || err != nil || dst.Len() != 0 {
					t.Fatalf("empty record = %d, %v, destination %x", n, err, dst.Bytes())
				}
				if n, err := r.ReadRawTo(&dst); n != 2 || err != nil || dst.String() != "\x08\x09" {
					t.Fatalf("following record = %d, %v, bytes %x", n, err, dst.Bytes())
				}
			})
			t.Run("message across compressed pages", func(t *testing.T) {
				t.Parallel()
				const size = 17<<20 + 37
				h := binary.AppendUvarint([]byte{0x1a}, size)
				src := io.MultiReader(bytes.NewReader(h), readerFunc(func(p []byte) (int, error) {
					for i := range p {
						p[i] = 'a'
					}
					return len(p), nil
				}))
				want := sha256.New()
				n := int64(size + len(h))
				var dst bytes.Buffer
				pw, err := page.NewWriter(&dst, 256<<10)
				if err != nil {
					t.Fatal(err)
				}
				w, err := NewWriter(pw)
				if err != nil {
					t.Fatal(errors.Join(err, pw.Close()))
				}
				if err := w.WriteRawFrom(io.TeeReader(src, want), n); err != nil {
					t.Fatal(errors.Join(err, pw.Close()))
				}
				if err := errors.Join(w.Write(&streamEvent{Seq: 9}), pw.Close()); err != nil {
					t.Fatal(err)
				}
				pr, err := page.NewReader(&dst)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := pr.Close(); err != nil {
						t.Error(err)
					}
				})
				r, err := NewReader(pr)
				if err != nil {
					t.Fatal(err)
				}
				got := sha256.New()
				if copied, err := r.ReadRawTo(got); err != nil || copied != n || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
					t.Fatalf("ReadRawTo = %d, %v, digest %x, want %d bytes, digest %x", copied, err, got.Sum(nil), n, want.Sum(nil))
				}
				var e streamEvent
				if err := r.Read(&e); err != nil || e.Seq != 9 {
					t.Fatalf("following record = %#v, %v", e, err)
				}
			})
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("nil destination", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(framedRecords([]byte{0x08, 7}))
				if err != nil {
					t.Fatal(err)
				}
				n, err := r.ReadRawTo(nil)
				checkErrorContains(t, err, "nil record destination")
				if got, err := r.ReadRaw(); n != 0 || err != nil || !bytes.Equal(got, []byte{0x08, 7}) {
					t.Fatalf("invalid copy returned %d bytes; subsequent record = %x, %v", n, got, err)
				}
			})
			t.Run("truncated body", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(strings.NewReader("\x03\x08\x07"))
				if err != nil {
					t.Fatal(err)
				}
				var dst bytes.Buffer
				n, err := r.ReadRawTo(&dst)
				if n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || dst.String() != "\x08\x07" {
					t.Fatalf("truncated record = %d, %v, bytes %x", n, err, dst.Bytes())
				}
			})
			t.Run("destination error with full count", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(framedRecords([]byte{0x08, 7}, []byte{0x08, 9}))
				if err != nil {
					t.Fatal(err)
				}
				w := writerFunc(func(b []byte) (int, error) { return len(b), io.EOF })
				n, err := r.ReadRawTo(w)
				if n != 2 || !errors.Is(err, io.EOF) || err == io.EOF { //nolint:errorlint // A destination EOF cannot end iteration cleanly.
					t.Fatalf("ReadRawTo = %d, %v, want two bytes and wrapped EOF", n, err)
				}
				if got, err := r.ReadRaw(); err != nil || !bytes.Equal(got, []byte{0x08, 9}) {
					t.Fatalf("ReadRaw after destination error = %x, %v, want 0809, nil", got, err)
				}
			})
			t.Run("short destination write", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(framedRecords([]byte{0x08, 7}))
				if err != nil {
					t.Fatal(err)
				}
				w := writerFunc(func([]byte) (int, error) { return 1, nil })
				n, err := r.ReadRawTo(w)
				if n != 1 || !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("ReadRawTo = %d, %v, want one byte and io.ErrShortWrite", n, err)
				}
			})
		})
	})
	t.Run("RawMessages", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			first := []byte{0x08, 7}
			r, err := NewReader(framedRecords(first, []byte{0x08, 9}, nil, []byte{0x80}))
			if err != nil {
				t.Fatal(err)
			}
			var got streamEvent
			for m, err := range r.RawMessages() {
				if err != nil || !bytes.Equal(m, first) {
					t.Fatalf("RawMessages yielded %x, %v, want first record %x", m, err, first)
				}
				present, err := m.DecodeField(1, &got)
				if err != nil || !present {
					t.Fatalf("DecodeField = %v, %v", present, err)
				}
				break
			}
			if got.Seq != 7 {
				t.Fatalf("decoded sequence = %d, want 7", got.Seq)
			}
			if err := r.Read(&got); err != nil || got.Seq != 9 {
				t.Fatalf("Read yielded %#v, %v, want sequence 9", got, err)
			}
			if b, err := r.ReadRaw(); err != nil || len(b) != 0 {
				t.Fatalf("ReadRaw yielded %x, %v, want an empty message", b, err)
			}
			n := 0
			for m, err := range r.RawMessages() {
				if err != nil || !bytes.Equal(m, []byte{0x80}) {
					t.Fatalf("RawMessages yielded %x, %v, want the unparsed bytes 80", m, err)
				}
				n++
			}
			if n != 1 {
				t.Fatalf("RawMessages yielded %d messages, want one", n)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("reading record from storage: %w", io.EOF)
			src := framedRecords([]byte{0x08, 7})
			fail := true
			r, err := NewReader(readerFunc(func(p []byte) (int, error) {
				if fail {
					fail = false
					return 0, wrapped
				}
				return src.Read(p)
			}))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for m, err := range r.RawMessages() {
				if !errors.Is(err, wrapped) || m != nil {
					t.Fatalf("RawMessages yielded %x, %v, want a nil message and %v", m, err, wrapped)
				}
				n++
			}
			if n != 1 {
				t.Fatalf("RawMessages yielded %d results, want one", n)
			}
			if got, err := r.ReadRaw(); err != nil || !bytes.Equal(got, []byte{0x08, 7}) {
				t.Fatalf("ReadRaw after source error = %x, %v, want 0807, nil", got, err)
			}
		})
	})

	t.Run("Read", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("invalid destinations can be retried", func(t *testing.T) {
				t.Parallel()
				r, err := NewReader(framedRecords([]byte{0x08, 4}, []byte{0x08, 7}, nil, []byte{0x08, 9}))
				if err != nil {
					t.Fatal(err)
				}
				var first streamEvent
				if err := r.Read(&first); err != nil || first.Seq != 4 {
					t.Fatalf("first record yielded %#v, %v, want sequence 4", first, err)
				}
				checkErrorContains(t, r.Read[streamEvent](nil), "nil record destination")
				var dst string
				checkErrorContains(t, r.Read(&dst), "not a named struct")
				for i, want := range []uint64{7, 0, 9} {
					e := streamEvent{Seq: 99}
					if err := r.Read(&e); err != nil || e.Seq != want {
						t.Fatalf("record %d yielded %#v, %v, want sequence %d", i, e, err, want)
					}
				}
				if err := r.Read(&streamEvent{}); err != io.EOF { //nolint:errorlint // A clean end of stream returns the exact EOF sentinel.
					t.Fatalf("Read returned %v, want io.EOF", err)
				}
			})
			t.Run("recover a decode error after continuing", func(t *testing.T) {
				t.Parallel()
				b := []byte{0x08, 7}
				for _, tc := range []struct {
					name string
					raw  []byte
				}{
					{"small record", b},
					{"large record", bytes.Repeat(b, 3<<10)},
				} {
					t.Run(tc.name, func(t *testing.T) {
						t.Parallel()
						// Reading the following record would overwrite borrowed bytes.
						r, err := NewReader(framedRecords(tc.raw, bytes.Repeat([]byte{0x08, 9}, 3<<10)))
						if err != nil {
							t.Fatal(err)
						}
						type incompatibleEvent struct {
							Seq string `protobuf:"1"`
						}
						p := incompatibleEvent{Seq: "previous"}
						err = r.Read(&p)
						de, ok := errors.AsType[*DecodeError](err)
						if !ok {
							t.Fatalf("Read returned %T %v, want *DecodeError", err, err)
						}
						checkErrorContains(t, err, "Seq: wire type 0, expected 2")
						if p.Seq != "previous" {
							t.Fatalf("destination changed to %#v after failure", p)
						}
						var e streamEvent
						if err := r.Read(&e); err != nil || e.Seq != 9 {
							t.Fatalf("following record = %#v, %v, want sequence 9", e, err)
						}
						if !bytes.Equal(de.Raw, tc.raw) {
							t.Fatal("DecodeError.Raw changed after the next read")
						}
						if err := de.Raw.Decode(&e); err != nil || e.Seq != 7 {
							t.Fatalf("recovered record = %#v, %v, want sequence 7", e, err)
						}
					})
				}
			})
			t.Run("malformed message unwraps", func(t *testing.T) {
				t.Parallel()
				// The first record contains only a field tag and omits its value.
				r, err := NewReader(framedRecords([]byte{0x08}))
				if err != nil {
					t.Fatal(err)
				}
				e := streamEvent{Seq: 77}
				err = r.Read(&e)
				de, ok := errors.AsType[*DecodeError](err)
				if !ok || !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(de.Raw, []byte{0x08}) {
					t.Fatalf("Read returned %v, want *DecodeError wrapping io.ErrUnexpectedEOF with raw byte 08", err)
				}
				if e.Seq != 77 {
					t.Fatalf("destination changed to %#v after failure", e)
				}
			})
		})
	})
}

func TestSchemaCache(t *testing.T) {
	t.Parallel()
	type (
		a struct{}
		b struct{}
		c struct{}
		d struct{}
		e struct{}
		f struct{}
		g struct{}
		h struct{}
		i struct{}
	)
	var cache schemaCache
	types := [...]struct {
		typ    reflect.Type
		lookup func() (*message, error)
	}{
		{reflect.TypeFor[a](), cache.lookupFor[a]},
		{reflect.TypeFor[b](), cache.lookupFor[b]},
		{reflect.TypeFor[c](), cache.lookupFor[c]},
		{reflect.TypeFor[d](), cache.lookupFor[d]},
		{reflect.TypeFor[e](), cache.lookupFor[e]},
		{reflect.TypeFor[f](), cache.lookupFor[f]},
		{reflect.TypeFor[g](), cache.lookupFor[g]},
		{reflect.TypeFor[h](), cache.lookupFor[h]},
		{reflect.TypeFor[i](), cache.lookupFor[i]},
	}
	for n, tc := range types {
		m, err := tc.lookup()
		if err != nil || m == nil || m.typ != tc.typ {
			t.Fatalf("lookupFor[%v]() = %v, %v", tc.typ, m, err)
		}
		if cache.entries[0] != m {
			t.Fatalf("lookupFor[%v]() did not fill the primary slot", tc.typ)
		}
		if n < 7 {
			continue
		}
		before := cache
		for _, hit := range types[n-7 : n] {
			if m, err := hit.lookup(); err != nil || m == nil || m.typ != hit.typ {
				t.Fatalf("lookupFor[%v]() = %v, %v", hit.typ, m, err)
			}
		}
		if cache != before {
			t.Fatalf("secondary hits changed the cache after %d insertions", n+1)
		}
	}
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
				w, err := NewWriter(pw)
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
				w, err := NewWriter(pw)
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
		data, err := Append(nil, e)
		if err != nil {
			b.Fatal(err)
		}
		payloadBytes += int64(len(data))
	}
	return events, payloadBytes
}

func framedRecords(records ...[]byte) *bytes.Reader {
	var b []byte
	for _, p := range records {
		b = binary.AppendUvarint(b, uint64(len(p)))
		b = append(b, p...)
	}
	return bytes.NewReader(b)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) {
	return f(p)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}
