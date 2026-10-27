.PHONY: ui build build-client test test-short run clean

BIN := bin/memspace
PKG := ./cmd/memspace
# FTS5 in mattn/go-sqlite3 is behind a build tag; search needs it.
TAGS := sqlite_fts5

# Rebuild the web UI into web/dist (committed, embedded by `go build`).
ui:
	cd web && npm ci --no-audit --no-fund && npm run build

build:
	CGO_ENABLED=1 go build -tags $(TAGS) -o $(BIN) $(PKG)

# Client-only binary (hooks, sync, agent setup): pure Go, trivially
# cross-compiled; `server start` reports that it is unavailable.
build-client:
	CGO_ENABLED=0 go build -o bin/memspace-client $(PKG)

test:
	CGO_ENABLED=1 go test -tags $(TAGS) -count=1 ./...

test-short:
	CGO_ENABLED=1 go test -tags $(TAGS) -short -count=1 ./...

run: build
	./$(BIN) server start --config config.toml

clean:
	rm -rf bin data
