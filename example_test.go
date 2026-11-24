package protobuf_test

import (
	"bytes"
	"fmt"

	"github.com/maruel/protobuf"
)

func ExampleRawMessage_Iter() {
	type Event struct {
		Sequence uint64 `protobuf:"1"`
		Text     string `protobuf:"2"`
	}

	var buf bytes.Buffer
	w := protobuf.NewWriter(&buf)
	if err := w.Write(&Event{Sequence: 42, Text: "started"}); err != nil {
		panic(err)
	}

	r := protobuf.NewReader(&buf)
	raw, err := r.ReadRaw()
	if err != nil {
		panic(err)
	}
	for f, err := range raw.Iter() {
		if err != nil {
			panic(err)
		}
		fmt.Printf("field %d, wire type %d, value %x\n", f.Number, f.WireType, f.Value)
	}

	// Output:
	// field 1, wire type 0, value 2a
	// field 2, wire type 2, value 0773746172746564
}
