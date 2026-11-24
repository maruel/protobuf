# protobuf in Go

Define proto3 messages with Go struct tags. Use `page.Writer` and `page.Reader`
to write and read individual Zstandard pages. The `page.StreamWriter` and
`page.StreamReader` adapters let protobuf records span those pages.
Requires Go 1.27.

Use `Schema[Event]()` to export a proto3 definition for a tagged `Event` type.
See the [package documentation](https://pkg.go.dev/github.com/maruel/protobuf)
for tag syntax and the [daily archive example](page/example_test.go) for
writing compressed event streams and opening a specific day's file.

## Development

Run `make verify` to check Go source, formatting, module files, and the CI
workflow. Run `make fix` to tidy module files and apply lint and formatting fixes.
`make test` runs race tests with coverage and executes each benchmark once.
The first lint run builds the pinned linter and its plugins.
