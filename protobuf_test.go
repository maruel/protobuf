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
	"time"

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

func TestAppend(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("protoc", func(t *testing.T) {
			t.Parallel()
			want, v := wireFixture(t)
			v.Ignored = "not serialized"
			v.Omitted = "not serialized"
			got, err := Append(nil, &v)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("Append = %x, want %x", got, want)
			}
		})
		t.Run("multibyte length prefixes", func(t *testing.T) {
			t.Parallel()
			type largeEvent struct {
				Detail wireDetail `protobuf:"1"`
				Counts []int32    `protobuf:"2"`
			}
			b, err := Append(nil, largeEvent{Detail: wireDetail{Text: strings.Repeat("x", 128)}, Counts: make([]int32, 130)})
			if err != nil {
				t.Fatal(err)
			}
			// The submessage occupies 131 bytes and packed counts occupy 130.
			want := append([]byte{0x0a, 0x83, 0x01, 0x12, 0x80, 0x01}, bytes.Repeat([]byte("x"), 128)...)
			want = append(want, 0x12, 0x82, 0x01)
			want = append(want, make([]byte, 130)...)
			if !bytes.Equal(b, want) {
				t.Fatalf("Append = %x, want %x", b, want)
			}
		})
		t.Run("non-empty destination", func(t *testing.T) {
			t.Parallel()
			got, err := Append([]byte{0xff}, wireDetail{Code: 42})
			if err != nil || !bytes.Equal(got, []byte{0xff, 0x08, 0x2a}) {
				t.Fatalf("Append = %x, %v", got, err)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		type labeledEvent struct {
			Labels []string `protobuf:"1"`
		}
		var cycle recursiveEvent
		cycle.Next = &cycle
		for _, c := range []struct {
			name string
			v    any
			err  string
		}{
			{name: "nil message", v: (*wireDetail)(nil), err: "expected a named struct or non-nil pointer"},
			{name: "non-message value", v: true, err: "not a named struct"},
			{name: "cyclic message", v: &cycle, err: "message nesting exceeds 100"},
			{name: "nil repeated message", v: wireEvent{Messages: []*wireDetail{nil}}, err: "messages: nil message at index 0"},
			{name: "invalid repeated string after a valid element", v: labeledEvent{Labels: []string{"ok", "\xff"}}, err: "Labels: invalid UTF-8"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
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
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("protoc", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
			got := wireDetail{Code: 99, Text: "old"}
			if err := Unmarshal([]byte{0x08, 0x05}, &got); err != nil || got != (wireDetail{Code: 5}) {
				t.Fatalf("Unmarshal = %+v, %v", got, err)
			}
		})
		t.Run("minimum signed widths", func(t *testing.T) {
			t.Parallel()
			type signedEvent struct {
				Delta   int32 `protobuf:"1,zigzag"`
				Counter int64 `protobuf:"2"`
			}
			data := []byte{
				0x08, 0xff, 0xff, 0xff, 0xff, 0x0f, // sint32 minimum
				0x10, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01, // int64 minimum
			}
			var got signedEvent
			want := signedEvent{Delta: math.MinInt32, Counter: math.MinInt64}
			if err := Unmarshal(data, &got); err != nil || got != want {
				t.Fatalf("Unmarshal = %+v, %v, want %+v", got, err, want)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("invalid destination", func(t *testing.T) {
			t.Parallel()
			checkErrorContains(t, Unmarshal(nil, (*wireDetail)(nil)), "expected a non-nil pointer")
			var dst bool
			checkErrorContains(t, Unmarshal(nil, &dst), "not a named struct")
		})
		t.Run("truncated wire data leaves destination intact", func(t *testing.T) {
			t.Parallel()
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
					t.Parallel()
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
			t.Parallel()
			var got wireDetail
			// Field number 2^29, one above the wire limit, followed by value 1.
			err := Unmarshal([]byte{0x80, 0x80, 0x80, 0x80, 0x10, 1}, &got)
			checkErrorContains(t, err, "invalid field number 536870912")
		})
		t.Run("invalid UTF-8", func(t *testing.T) {
			t.Parallel()
			var got wireDetail
			err := Unmarshal([]byte{0x12, 0x01, 0xff}, &got)
			checkErrorContains(t, err, "text: invalid UTF-8")
		})
		t.Run("narrow integer overflow", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
			var b []byte
			for range 120 {
				b = protowire.AppendBytes([]byte{0x0a}, b)
			}
			var got recursiveEvent
			checkErrorContains(t, Unmarshal(b, &got), "message nesting exceeds 100")
		})
		t.Run("wrong wire types leave destination intact", func(t *testing.T) {
			t.Parallel()
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
					t.Parallel()
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

func TestSchema(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		type schemaChild struct {
			Valid bool `protobuf:"1,name=valid"`
		}
		type schemaRoot struct {
			Children []schemaChild `protobuf:"12,name=children"`
			Parent   *schemaRoot   `protobuf:"11,name=parent"`
			Delta    *int32        `protobuf:"5,name=delta,zigzag"`
			IDs      []uint64      `protobuf:"1"`
			Cache    string        `protobuf:"-"`
			Local    string
		}
		const want = `syntax = "proto3";

message schemaRoot {
  repeated uint64 IDs = 1;
  optional sint32 delta = 5;
  .schemaRoot parent = 11;
  repeated .schemaChild children = 12;
}

message schemaChild {
  bool valid = 1;
}
`
		got, err := Schema[schemaRoot]()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Schema:\n%s\nwant:\n%s", got, want)
		}
		t.Run("empty message", func(t *testing.T) {
			t.Parallel()
			type emptyEvent struct{}
			const want = "syntax = \"proto3\";\n\nmessage emptyEvent {\n}\n"
			got, err := Schema[emptyEvent]()
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("Schema:\n%s\nwant:\n%s", got, want)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		type reserved struct {
			Value int `protobuf:"19000"`
		}
		type tooLarge struct {
			Value int `protobuf:"536870912"`
		}
		type duplicateNumber struct {
			A int `protobuf:"1"`
			B int `protobuf:"1"`
		}
		type duplicateName struct {
			A int `protobuf:"1,name=hello_world"`
			B int `protobuf:"2,name=HelloWorld"`
		}
		type unsupported struct {
			Data map[string]string `protobuf:"1"`
		}
		type repeatedSlices struct {
			Data [][]int `protobuf:"1"`
		}
		type badName struct {
			Value int `protobuf:"1,name=bad-name"`
		}
		type invalidZigZag struct {
			Text string `protobuf:"1,zigzag"`
		}
		type duplicateOption struct {
			Value int `protobuf:"1,name=first,name=second"`
		}
		type privateField struct {
			value int `protobuf:"1"` //nolint:unused // Schema checks the rejected private field through reflection.
		}
		type timeField struct {
			At time.Time `protobuf:"1"`
		}
		t.Run("distinct types with the same message name", func(t *testing.T) {
			t.Parallel()
			type detail struct {
				Code uint32 `protobuf:"1"`
			}
			type firstDetail = detail
			{
				type detail struct {
					Text string `protobuf:"1"`
				}
				type event struct {
					First  firstDetail `protobuf:"1"`
					Second detail      `protobuf:"2"`
				}
				_, err := Schema[event]()
				checkErrorContains(t, err, `share message name "detail"`)
			}
		})
		cases := []struct {
			name   string
			schema func() (string, error)
			err    string
		}{
			{name: "reserved field number", schema: Schema[reserved], err: `invalid protobuf field number "19000"`},
			{name: "out of range field number", schema: Schema[tooLarge], err: `invalid protobuf field number "536870912"`},
			{name: "duplicate field number", schema: Schema[duplicateNumber], err: "duplicate field number 1"},
			{name: "conflicting field names", schema: Schema[duplicateName], err: `conflicting JSON names for field "HelloWorld"`},
			{name: "unsupported type", schema: Schema[unsupported], err: "unsupported field type map[string]string"},
			{name: "nested non-byte slices", schema: Schema[repeatedSlices], err: "unsupported field type [][]int"},
			{name: "invalid field name", schema: Schema[badName], err: `invalid protobuf field name "bad-name"`},
			{name: "zigzag on non-integer", schema: Schema[invalidZigZag], err: "zigzag requires a signed integer"},
			{name: "duplicate option", schema: Schema[duplicateOption], err: `invalid or duplicate protobuf option "name=second"`},
			{name: "tagged private field", schema: Schema[privateField], err: "tagged field must be exported"},
			{name: "untagged nested struct", schema: Schema[timeField], err: "time.Time has Go fields but no protobuf fields"},
			{name: "unnamed struct", schema: Schema[struct{}], err: "is not a named struct with a valid protobuf identifier"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				_, err := c.schema()
				checkErrorContains(t, err, c.err)
			})
		}
	})
}

func TestRawMessage(t *testing.T) {
	t.Parallel()
	t.Run("Iter", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("wire order and borrowed values", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{
					0x30, 1,
					0x32, 3, 0, 0x96, 1,
					0x30, 2,
					0x15, 1, 2, 3, 4,
					0x19, 1, 2, 3, 4, 5, 6, 7, 8,
					0x23, 0x08, 1, 0x24,
					0xf8, 0xff, 0xff, 0xff, 0x0f, 0,
				}
				fields := m.Iter()
				for f, err := range fields {
					if err != nil || f.Number != 6 {
						t.Fatalf("first field = %+v, %v, want field 6", f, err)
					}
					break
				}
				var got []Field
				for f, err := range fields {
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, f)
				}
				want := []Field{
					{Number: 6, WireType: protowire.VarintType, Value: []byte{1}},
					{Number: 6, WireType: protowire.BytesType, Value: []byte{3, 0, 0x96, 1}},
					{Number: 6, WireType: protowire.VarintType, Value: []byte{2}},
					{Number: 2, WireType: protowire.Fixed32Type, Value: []byte{1, 2, 3, 4}},
					{Number: 3, WireType: protowire.Fixed64Type, Value: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
					{Number: 4, WireType: protowire.StartGroupType, Value: []byte{0x08, 1, 0x24}},
					{Number: 1<<29 - 1, WireType: protowire.VarintType, Value: []byte{0}},
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("Iter yielded %+v, want %+v", got, want)
				}
				if &got[0].Value[0] != &m[1] {
					t.Fatal("field value did not borrow the original bytes")
				}
			})
			t.Run("empty message", func(t *testing.T) {
				t.Parallel()
				for f, err := range RawMessage(nil).Iter() {
					t.Fatalf("empty message yielded %+v, %v", f, err)
				}
			})
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("truncated value after a valid field", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{0x08, 5, 0x12, 2, 'x'}
				n := 0
				for f, err := range m.Iter() {
					if n == 0 {
						if err != nil || f.Number != 1 || !bytes.Equal(f.Value, []byte{5}) {
							t.Fatalf("first field = %+v, %v, want field 1 with value 5", f, err)
						}
					} else if n != 1 || !reflect.DeepEqual(f, Field{}) || !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("result %d = %+v, %v, want a zero Field and io.ErrUnexpectedEOF", n, f, err)
					}
					n++
				}
				if n != 2 {
					t.Fatalf("Iter yielded %d results, want one field and one error", n)
				}
			})
			for _, tc := range []struct {
				name string
				raw  RawMessage
				err  string
			}{
				{"truncated tag", RawMessage{0x80}, "read field tag: unexpected EOF"},
				{"field number above the limit", RawMessage{0x80, 0x80, 0x80, 0x80, 0x10, 1}, "invalid field number 536870912"},
				{"reserved wire type", RawMessage{0x0e}, "reserved wire type"},
				{"mismatched group", RawMessage{0x0b, 0x14}, "mismatching end group marker"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					n := 0
					for f, err := range tc.raw.Iter() {
						if n != 0 || !reflect.DeepEqual(f, Field{}) {
							t.Fatalf("result %d = %+v, %v, want a zero Field on error", n, f, err)
						}
						checkErrorContains(t, err, tc.err)
						n++
					}
					if n != 1 {
						t.Fatalf("Iter yielded %d results, want one error", n)
					}
				})
			}
		})
	})
	t.Run("Decode", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := RawMessage{0x08, 5, 0x12, 2, 'x'}
			want := wireDetail{Code: 99, Text: "old"}
			got := want
			if err := m.Decode(&got); !errors.Is(err, io.ErrUnexpectedEOF) || got != want {
				t.Fatalf("Decode = %#v, %v, want the previous value and io.ErrUnexpectedEOF", got, err)
			}
		})
	})
	t.Run("DecodeField", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("last zigzag occurrence skips unrelated contents", func(t *testing.T) {
				t.Parallel()
				// The unselected string field contains invalid UTF-8.
				m := RawMessage{0x10, 0x09, 0x3a, 1, 0xff, 0x10, 0x21}
				got := wireEvent{Counter: 7, Text: "keep"}
				present, err := m.DecodeField(2, &got)
				if err != nil || !present || got.Delta != -17 || got.Counter != 7 || got.Text != "keep" {
					t.Fatalf("DecodeField = %#v, %v, %v", got, present, err)
				}
			})
			t.Run("absent and explicit zero", func(t *testing.T) {
				t.Parallel()
				got := wireEvent{Counter: 7}
				present, err := (RawMessage{0x18, 1}).DecodeField(1, &got)
				if err != nil || present || got.Counter != 7 || got.Active {
					t.Fatalf("absent field yielded %#v, %v, %v", got, present, err)
				}
				present, err = (RawMessage{0x08, 0}).DecodeField(1, &got)
				if err != nil || !present || got.Counter != 0 {
					t.Fatalf("explicit zero yielded %#v, %v, %v", got, present, err)
				}
			})
			t.Run("mixed packed and unpacked values", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{0x30, 1, 0x32, 3, 0, 0x96, 1, 0x30, 2}
				got := wireEvent{Counts: []uint64{99}}
				present, err := m.DecodeField(6, &got)
				if err != nil || !present || !reflect.DeepEqual(got.Counts, []uint64{1, 0, 150, 2}) {
					t.Fatalf("DecodeField = %v, %v, %v", got.Counts, present, err)
				}
			})
			t.Run("merge occurrences of a submessage", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{0x42, 2, 0x08, 5, 0x42, 3, 0x12, 1, 'x'}
				old := wireDetail{Code: 99, Text: "old"}
				got := wireEvent{Detail: &old}
				present, err := m.DecodeField(8, &got)
				if err != nil || !present || got.Detail == nil || *got.Detail != (wireDetail{Code: 5, Text: "x"}) {
					t.Fatalf("DecodeField = %#v, %v, %v", got.Detail, present, err)
				}
				if old != (wireDetail{Code: 99, Text: "old"}) {
					t.Fatalf("previous submessage changed to %#v", old)
				}
			})
			t.Run("decoded bytes own their storage", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{0x72, 3, 1, 2, 3}
				var got wireEvent
				present, err := m.DecodeField(14, &got)
				clear(m)
				if err != nil || !present || !bytes.Equal(got.Data, []byte{1, 2, 3}) {
					t.Fatalf("DecodeField = %x, %v, %v", got.Data, present, err)
				}
			})
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("invalid destination", func(t *testing.T) {
				t.Parallel()
				var m RawMessage
				_, err := m.DecodeField(1, (*wireEvent)(nil))
				checkErrorContains(t, err, "expected a non-nil pointer")
				var unsupported int
				_, err = m.DecodeField(1, &unsupported)
				checkErrorContains(t, err, "not a named struct")
			})
			t.Run("invalid or undeclared number", func(t *testing.T) {
				t.Parallel()
				var m RawMessage
				var got wireEvent
				_, err := m.DecodeField(0, &got)
				checkErrorContains(t, err, "invalid field number 0")
				_, err = m.DecodeField(1<<29, &got)
				checkErrorContains(t, err, "invalid field number 536870912")
				_, err = m.DecodeField(99, &got)
				checkErrorContains(t, err, "has no field 99")
			})
			t.Run("malformed skipped field rolls back selection", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{0x08, 1, 0x72, 2, 1}
				got := wireEvent{Counter: 7}
				present, err := m.DecodeField(1, &got)
				if !errors.Is(err, io.ErrUnexpectedEOF) || present || got.Counter != 7 {
					t.Fatalf("DecodeField = %d, %v, %v", got.Counter, present, err)
				}
			})
			t.Run("malformed selected field rolls back selection", func(t *testing.T) {
				t.Parallel()
				m := RawMessage{0x30, 1, 0x32, 2, 0x02, 0x80}
				got := wireEvent{Counts: []uint64{99}}
				present, err := m.DecodeField(6, &got)
				if !errors.Is(err, io.ErrUnexpectedEOF) || present || !reflect.DeepEqual(got.Counts, []uint64{99}) {
					t.Fatalf("DecodeField = %v, %v, %v", got.Counts, present, err)
				}
			})
		})
	})
}

func BenchmarkAppend(b *testing.B) {
	v := benchmarkWireEvent()
	buf, err := Append(nil, &v)
	if err != nil {
		b.Fatal(err)
	}
	n := int64(len(buf))
	b.Run("fresh", func(b *testing.B) {
		b.SetBytes(n)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Append(nil, &v); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("reuse", func(b *testing.B) {
		b.SetBytes(n)
		b.ReportAllocs()
		for b.Loop() {
			buf, err = Append(buf[:0], &v)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkUnmarshal(b *testing.B) {
	v := benchmarkWireEvent()
	data, err := Append(nil, &v)
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

func BenchmarkRawMessage(b *testing.B) {
	v := benchmarkWireEvent()
	data, err := Append(nil, &v)
	if err != nil {
		b.Fatal(err)
	}
	m := RawMessage(data)
	b.Run("Iter", func(b *testing.B) {
		b.SetBytes(int64(len(m)))
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			for f, err := range m.Iter() {
				if err != nil {
					b.Fatal(err)
				}
				n += len(f.Value)
			}
			if n == 0 {
				b.Fatal("Iter yielded no value bytes")
			}
		}
	})
	b.Run("DecodeField", func(b *testing.B) {
		b.SetBytes(int64(len(m)))
		b.ReportAllocs()
		var dst wireEvent
		for b.Loop() {
			present, err := m.DecodeField(1, &dst)
			if err != nil || !present {
				b.Fatalf("DecodeField = %v, %v", present, err)
			}
		}
	})
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
