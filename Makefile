.DEFAULT_GOAL := help
.PHONY: help fix verify test lint-tool

LINTER := .lint/custom-gcl

help:
	@printf '%s\n' 'make fix     Tidy the module and apply lint and formatting fixes' \
	               'make verify  Check Go source, module files, and CI workflows' \
	               'make test    Run race tests, coverage, and benchmark smoke checks'

# Rebuild when the pinned linter, plugins, or Go toolchain changes.
lint-tool:
	@set -eu; \
	version=$$(awk '/^version: / {print $$2}' .custom-gcl.yml); \
	go_platform=$$(go env GOVERSION GOOS GOARCH); \
	key=$$({ cat .custom-gcl.yml; printf '%s\n' "$$go_platform"; } | git hash-object --stdin); \
	if [ -x "$(LINTER)" ] && [ "$$key" = "$$(cat .lint/build-key 2>/dev/null || :)" ]; then exit 0; fi; \
	mkdir -p .lint; \
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"$$version" custom --destination .lint; \
	test -x "$(LINTER)"; \
	printf '%s\n' "$$key" > .lint/build-key

fix: lint-tool
	@go mod tidy
	@$(LINTER) fmt
	@$(LINTER) run --fix ./...

verify: lint-tool
	@go mod tidy -diff
	@go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
	@$(LINTER) run ./...

test:
	@go test -race -cover ./...
	@go test -run '^$$' -bench . -benchtime=1x ./...
