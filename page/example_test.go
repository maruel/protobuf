package page_test

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/maruel/protobuf"
	"github.com/maruel/protobuf/page"
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
	defer func() {
		if err := os.Remove(f.Name()); err != nil {
			panic(err)
		}
	}()
	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
	}()

	pw, err := page.NewWriter(f, 0)
	if err != nil {
		panic(err)
	}
	w, err := protobuf.NewWriter[Event](pw)
	if err != nil {
		panic(errors.Join(err, pw.Close()))
	}
	if err := errors.Join(w.Write(&Event{Sequence: 1, Text: "started"}), pw.Close()); err != nil {
		panic(err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		panic(err)
	}
	pr, err := page.NewReader(f)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := pr.Close(); err != nil {
			panic(err)
		}
	}()
	r, err := protobuf.NewReader(pr)
	if err != nil {
		panic(err)
	}
	for event, err := range r.Events[Event]() {
		if err != nil {
			panic(err)
		}
		fmt.Println(event.Sequence, event.Text)
	}
	// Output: 1 started
}
