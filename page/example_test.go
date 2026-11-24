package page_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/maruel/protobuf"
	"github.com/maruel/protobuf/page"
)

func ExampleStreamReader() {
	type Event struct {
		Sequence uint64 `protobuf:"1,name=sequence"`
		Text     string `protobuf:"2,name=text"`
	}

	dir, err := os.MkdirTemp("", "daily-events-*")
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			panic(err)
		}
	}()

	for _, day := range []struct {
		date   string
		events []Event
	}{
		{"2026-11-02", []Event{{Sequence: 1, Text: "started"}}},
		{"2026-11-03", []Event{{Sequence: 2, Text: "started"}, {Sequence: 3, Text: "finished"}}},
	} {
		f, err := os.Create(filepath.Join(dir, day.date+".pbz")) //nolint:gosec // The day names are fixed and dir is temporary.
		if err != nil {
			panic(err)
		}
		pw, err := page.NewStreamWriter(f, 0)
		if err != nil {
			panic(errors.Join(err, f.Close()))
		}
		w := protobuf.NewWriter(pw)
		for i := range day.events {
			if err := w.Write(&day.events[i]); err != nil {
				panic(errors.Join(err, pw.Close(), f.Close()))
			}
		}
		if err := errors.Join(pw.Close(), f.Close()); err != nil {
			panic(err)
		}
	}

	// Open the selected day's stream directly.
	f, err := os.Open(filepath.Join(dir, "2026-11-03.pbz")) //nolint:gosec // The day name is fixed and dir is temporary.
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
	}()
	pr, err := page.NewStreamReader(f)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := pr.Close(); err != nil {
			panic(err)
		}
	}()
	r := protobuf.NewReader(pr)
	for e, err := range r.Iter[Event]() {
		if err != nil {
			panic(err)
		}
		fmt.Println(e.Sequence, e.Text)
	}

	// Output:
	// 2 started
	// 3 finished
}
