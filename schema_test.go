package protobuf

import (
	"testing"
	"time"
)

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
