# KB Core build and dev tasks.
#
# SQLite uses the cgo driver, so CGO_ENABLED=1 is required (a plain `go build` on
# a machine without a C toolchain produces linker errors). FTS5 search (Phase 3)
# will require GO_TAGS=sqlite_fts5 — set it then so a plain build does not ship a
# binary that panics when a `CREATE VIRTUAL TABLE ... USING fts5` migration runs.

export CGO_ENABLED := 1
GO_TAGS ?=
TAGFLAG := $(if $(GO_TAGS),-tags '$(GO_TAGS)',)

.PHONY: build test race vet fmt fmtcheck lint tidy run all

all: fmtcheck vet build race

build:
	go build $(TAGFLAG) ./...

test:
	go test $(TAGFLAG) ./...

race:
	go test -race $(TAGFLAG) ./...

vet:
	go vet $(TAGFLAG) ./...

fmt:
	gofmt -l -w .

fmtcheck:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

lint:
	golangci-lint run

tidy:
	go mod tidy

run:
	go run $(TAGFLAG) ./cmd/kbd
