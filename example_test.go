package protobuf_test

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/maruel/protobuf"
)

func ExampleWriter() {
	type Event struct {
		Sequence uint64 `protobuf:"1,name=sequence"`
		Text     string `protobuf:"2,name=text"`
	}

	f, err := os.CreateTemp("", "events-*.pbz")
	if err != nil {
		panic(err)
	}
	defer os.Remove(f.Name())
	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
	}()

	w, err := protobuf.NewWriter[Event](f, 0)
	if err != nil {
		panic(err)
	}
	if err := errors.Join(w.Write(&Event{Sequence: 1, Text: "started"}), w.Close()); err != nil {
		panic(err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		panic(err)
	}
	r, err := protobuf.NewReader(f)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			panic(err)
		}
	}()
	for event, err := range r.Events[Event]() {
		if err != nil {
			panic(err)
		}
		fmt.Println(event.Sequence, event.Text)
	}
	// Output: 1 started
}
