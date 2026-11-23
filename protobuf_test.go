package protobuf

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

type wireBytes []byte

type wireEvent struct {
	Detail       *wireDetail   `protobuf:"8,name=detail"`
	Counter      int32         `protobuf:"1,name=counter"`
	Delta        int64         `protobuf:"2,zigzag,name=delta"`
	Active       bool          `protobuf:"3,name=active"`
	Ratio        float32       `protobuf:"4,name=ratio"`
	Offset       float64       `protobuf:"5,name=offset"`
	Counts       []uint64      `protobuf:"6,name=counts"`
	Text         string        `protobuf:"7,name=text"`
	Messages     []*wireDetail `protobuf:"9,name=messages"`
	OptionalText *string       `protobuf:"10,name=optional_text"`
	Tags         [][]byte      `protobuf:"11,name=tags"`
	Empty        wireDetail    `protobuf:"12,name=empty"`
	Present      *wireBytes    `protobuf:"13,name=present"`
	Data         wireBytes     `protobuf:"14,name=data"`
	Omitted      string        `protobuf:"-"`
	Ignored      string
}

type wireDetail struct {
	Code uint32 `protobuf:"1,name=code"`
	Text string `protobuf:"2,name=text"`
}

type recursiveEvent struct {
	Next *recursiveEvent `protobuf:"1"`
}

func TestMarshal(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		t.Run("protoc", func(t *testing.T) {
			want, v := wireFixture(t)
			v.Ignored = "not serialized"
			v.Omitted = "not serialized"
			got, err := Marshal(&v)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("Marshal = %x, want %x", got, want)
			}
		})
		t.Run("multibyte length prefixes", func(t *testing.T) {
			type largeEvent struct {
				Detail wireDetail `protobuf:"1"`
				Counts []int32    `protobuf:"2"`
			}
			b, err := Marshal(largeEvent{Detail: wireDetail{Text: strings.Repeat("x", 128)}, Counts: make([]int32, 130)})
			if err != nil {
				t.Fatal(err)
			}
			// The submessage occupies 131 bytes and packed counts occupy 130.
			want := append([]byte{0x0a, 0x83, 0x01, 0x12, 0x80, 0x01}, bytes.Repeat([]byte("x"), 128)...)
			want = append(want, 0x12, 0x82, 0x01)
			want = append(want, make([]byte, 130)...)
			if !bytes.Equal(b, want) {
				t.Fatalf("Marshal = %x, want %x", b, want)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Run("cyclic message", func(t *testing.T) {
			var v recursiveEvent
			v.Next = &v
			_, err := Marshal(&v)
			checkErrorContains(t, err, "message nesting exceeds 100")
		})
		t.Run("nil repeated message", func(t *testing.T) {
			_, err := Marshal(wireEvent{Messages: []*wireDetail{nil}})
			checkErrorContains(t, err, "messages: nil message at index 0")
		})
	})
}

func TestAppend(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := Append([]byte{0xff}, wireDetail{Code: 42})
		if err != nil || !bytes.Equal(got, []byte{0xff, 0x08, 0x2a}) {
			t.Fatalf("Append = %x, %v", got, err)
		}
	})
	t.Run("error", func(t *testing.T) {
		type labeledEvent struct {
			Labels []string `protobuf:"1"`
		}
		for _, c := range []struct {
			name string
			v    any
			err  string
		}{
			{name: "nil message", v: (*wireDetail)(nil), err: "expected a named struct or non-nil pointer"},
			{name: "non-message value", v: true, err: "not a named struct"},
			{name: "invalid repeated string after a valid element", v: labeledEvent{Labels: []string{"ok", "\xff"}}, err: "Labels: invalid UTF-8"},
		} {
			t.Run(c.name, func(t *testing.T) {
				b := make([]byte, 1, 32)
				b[0] = 0xff
				got, err := Append(b, c.v)
				checkErrorContains(t, err, c.err)
				if !bytes.Equal(got, []byte{0xff}) {
					t.Fatalf("Append returned %x, want the original prefix ff", got)
				}
			})
		}
	})
}

func TestUnmarshal(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		t.Run("protoc", func(t *testing.T) {
			b, want := wireFixture(t)
			got := wireEvent{Ignored: "old", Omitted: "old"}
			if err := Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			clear(b)
			if !reflect.DeepEqual(got, want) || !math.Signbit(got.Offset) {
				t.Fatalf("Unmarshal = %+v, want %+v", got, want)
			}
		})
		t.Run("merged messages and mixed repeated encodings", func(t *testing.T) {
			type oldEvent struct {
				Detail *wireDetail `protobuf:"1"`
				Counts []int32     `protobuf:"2"`
			}
			b := []byte{
				0x0a, 0x02, 0x08, 0x05, // Detail code = 5
				0x0a, 0x03, 0x12, 0x01, 'x', // Detail text = x
				0x10, 0x01, // Counts = 1, unpacked
				0x12, 0x03, 0x00, 0x96, 0x01, // Counts = 0, 150, packed
				0x4b, 0x08, 0x07, 0x4c, // Unknown group
				0x10, 0x02, // Counts = 2, unpacked
				0x50, 0x01, // Unknown varint
			}
			var got oldEvent
			if err := Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			want := oldEvent{Detail: &wireDetail{Code: 5, Text: "x"}, Counts: []int32{1, 0, 150, 2}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Unmarshal = %+v, want %+v", got, want)
			}
		})
		t.Run("added field reads as zero", func(t *testing.T) {
			got := wireDetail{Code: 99, Text: "old"}
			if err := Unmarshal([]byte{0x08, 0x05}, &got); err != nil || got != (wireDetail{Code: 5}) {
				t.Fatalf("Unmarshal = %+v, %v", got, err)
			}
		})
		t.Run("minimum zigzag int32", func(t *testing.T) {
			type zigzagEvent struct {
				Delta int32 `protobuf:"1,zigzag"`
			}
			var got zigzagEvent
			if err := Unmarshal([]byte{0x08, 0xff, 0xff, 0xff, 0xff, 0x0f}, &got); err != nil || got.Delta != math.MinInt32 {
				t.Fatalf("Unmarshal = %+v, %v, want delta %d", got, err, math.MinInt32)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Run("invalid destination", func(t *testing.T) {
			checkErrorContains(t, Unmarshal(nil, (*wireDetail)(nil)), "expected a non-nil pointer")
			var dst bool
			checkErrorContains(t, Unmarshal(nil, &dst), "not a named struct")
		})
		t.Run("truncated wire data leaves destination intact", func(t *testing.T) {
			for _, c := range []struct {
				name string
				wire []byte
			}{
				{name: "field tag", wire: []byte{0x80}},
				{name: "unknown field value", wire: []byte{0x78}},
				{name: "packed field body", wire: []byte{0x32, 2, 0}},
				{name: "packed field element", wire: []byte{0x32, 2, 0, 0x80}},
				{name: "float32 value", wire: []byte{0x25, 0}},
				{name: "float64 value", wire: []byte{0x29, 0}},
				{name: "submessage body", wire: []byte{0x42, 2, 0x08}},
				{name: "submessage field value", wire: []byte{0x42, 1, 0x08}},
			} {
				t.Run(c.name, func(t *testing.T) {
					b := append([]byte{0x08, 1}, c.wire...)
					got := wireEvent{Counter: 7, Detail: &wireDetail{Code: 99, Text: "old"}}
					want := wireEvent{Counter: 7, Detail: &wireDetail{Code: 99, Text: "old"}}
					if err := Unmarshal(b, &got); !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("Unmarshal returned %v, want io.ErrUnexpectedEOF", err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("Unmarshal changed the destination to %+v, want %+v", got, want)
					}
				})
			}
		})
		t.Run("field number exceeds protobuf limit", func(t *testing.T) {
			var got wireDetail
			// Field number 2^29, one above the wire limit, followed by value 1.
			err := Unmarshal([]byte{0x80, 0x80, 0x80, 0x80, 0x10, 1}, &got)
			checkErrorContains(t, err, "invalid field number 536870912")
		})
		t.Run("invalid UTF-8", func(t *testing.T) {
			var got wireDetail
			err := Unmarshal([]byte{0x12, 0x01, 0xff}, &got)
			checkErrorContains(t, err, "text: invalid UTF-8")
		})
		t.Run("narrow integer overflow", func(t *testing.T) {
			type narrowEvent struct {
				Code  int8  `protobuf:"1"`
				Count uint8 `protobuf:"2"`
			}
			var got narrowEvent
			err := Unmarshal([]byte{0x08, 0x80, 0x01}, &got)
			checkErrorContains(t, err, "Code: value 128 overflows int8")
			err = Unmarshal([]byte{0x10, 0x80, 0x02}, &got)
			checkErrorContains(t, err, "Count: value 256 overflows uint8")
		})
		t.Run("excessive nesting", func(t *testing.T) {
			var b []byte
			for range 120 {
				b = protowire.AppendBytes([]byte{0x0a}, b)
			}
			var got recursiveEvent
			checkErrorContains(t, Unmarshal(b, &got), "message nesting exceeds 100")
		})
		t.Run("wrong wire types leave destination intact", func(t *testing.T) {
			type mismatchEvent struct {
				Code    uint32        `protobuf:"1"`
				Details []*wireDetail `protobuf:"3"`
				Text    string        `protobuf:"4"`
			}
			cases := []struct {
				name string
				wire []byte
				err  string
			}{
				{name: "packed for singular scalar", wire: []byte{0x0a, 1, 1}, err: "Code: wire type 2, expected 0"},
				{name: "varint for repeated message", wire: []byte{0x18, 1}, err: "Details: wire type 0, expected 2"},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					b := append([]byte{0x22, 3, 'n', 'e', 'w'}, c.wire...)
					want := mismatchEvent{Code: 7, Text: "old"}
					got := want
					checkErrorContains(t, Unmarshal(b, &got), c.err)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("Unmarshal changed the destination to %+v, want %+v", got, want)
					}
				})
			}
		})
	})
}

func BenchmarkMarshal(b *testing.B) {
	v := benchmarkWireEvent()
	data, err := Marshal(&v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(&v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppend(b *testing.B) {
	v := benchmarkWireEvent()
	buf, err := Append(nil, &v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		buf, err = Append(buf[:0], &v)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	v := benchmarkWireEvent()
	data, err := Marshal(&v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	var dst wireEvent
	for b.Loop() {
		if err := Unmarshal(data, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkWireEvent() wireEvent {
	return wireEvent{
		Counter:  4164,
		Delta:    -21,
		Active:   true,
		Ratio:    0.98,
		Counts:   []uint64{12, 17, 53, 420},
		Text:     "request completed",
		Detail:   &wireDetail{Code: 200, Text: "ok"},
		Messages: []*wireDetail{{Code: 206, Text: "upstream cache hit"}},
		Data:     []byte(`{"path":"/v1/events","method":"GET","region":"us-east-1","request_id":"9f305f","duration_ms":37}`),
	}
}

func checkErrorContains(t *testing.T, err error, want string) {
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want an error containing %q", err, want)
	}
}

func wireFixture(t *testing.T) ([]byte, wireEvent) {
	// Frozen bytes from protoc --encode=wireEvent using the corresponding
	// proto3 scalar types, packed counts, and explicit optional fields.
	const fixture = "08e5ffffffffffffffff0110211801250000c03f290000000000000080320d009601ffffffffffffffffff013a05636166c3a9420508081201784a0208034a0052005a005a0200ff62006a007203010203"
	b, err := hex.DecodeString(fixture)
	if err != nil {
		t.Fatal(err)
	}
	s := ""
	empty := wireBytes{}
	return b, wireEvent{
		Counter:      -27,
		Delta:        -17,
		Active:       true,
		Ratio:        1.5,
		Offset:       math.Copysign(0, -1),
		Counts:       []uint64{0, 150, math.MaxUint64},
		Text:         "café",
		Detail:       &wireDetail{Code: 8, Text: "x"},
		Messages:     []*wireDetail{{Code: 3}, {}},
		OptionalText: &s,
		Tags:         [][]byte{{}, {0, 255}},
		Present:      &empty,
		Data:         wireBytes{1, 2, 3},
	}
}
