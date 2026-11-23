# protobuf in Go

Define proto3 messages with Go struct tags and store event streams in
independently compressed Zstandard pages. Requires Go 1.27.

Use `Schema[Event]()` to export a proto3 definition for a tagged `Event` type.
See the [package documentation](https://pkg.go.dev/github.com/maruel/protobuf)
for tag syntax and the [writer example](example_test.go) for storing and reading
events.
