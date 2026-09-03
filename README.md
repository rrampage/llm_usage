# llm_usage

`llm_usage` is an offline-first local usage reporter for AI coding harnesses. It
normalizes token/cost records from Claude Code, Codex, pi-agent, Gemini CLI,
Google Antigravity, OpenCode, and Crush, then reports daily/weekly/monthly,
session, or model totals.

Normal reporting does not make network requests. The only network-capable
production command is the explicit `llm_usage pricing update` operation.

## Build

The module targets **Go 1.26** and uses `modernc.org/sqlite`, the pure-Go,
CGO-free SQLite driver, for harnesses whose authoritative history is stored in
SQLite.

```sh
go build -o llm_usage .
```

`go.mod` pins both `modernc.org/sqlite` and the exact `modernc.org/libc` version
used by that SQLite release. The latter is intentional: modernc documents its
libc dependency as version-sensitive for downstream users.

## Usage

```sh
llm_usage                 # equivalent to: llm_usage daily (last 3 months by default)
llm_usage --all           # daily report across all history
llm_usage weekly
llm_usage monthly
llm_usage session
llm_usage model
llm_usage opencode daily
llm_usage crush monthly
```

Period reports group by **period x model** across harnesses and end with a
`TOTAL` row.

Useful filters/overrides:

```sh
llm_usage --since 2026-09-01 --until 2026-09-03
llm_usage --all           # disable the default 3-month cutoff for daily reports
llm_usage --timezone UTC
llm_usage --json
llm_usage --path opencode=/tmp/opencode
llm_usage --crush-path /src/project/.crush
```

## Layout

```text
core.go                    normalized Event/Harness API
cli.go                     command parsing and offline orchestration
adapters.go                adapter registry
adapter_claude.go          Claude Code
adapter_codex.go           OpenAI Codex
adapter_pi.go              pi-agent
adapter_gemini.go          Gemini CLI
adapter_antigravity.go     Google Antigravity
adapter_opencode.go        OpenCode
adapter_crush.go           Crush
sqlite.go                  production read-only modernc SQLite boundary
pricing.go                 offline price book + explicit updater
report.go                  aggregation/output
util_json.go               shared JSON helpers
internal/minisqlite/       experimental SQLite file-format parser + tests
cmd/minisqlite-corpus/     explicit downloader for optional public DB corpus
```

The adapters remain small implementations of one interface:

```go
type Harness interface {
    Name() string
    DefaultRoots() []string
    Load(ctx *LoadContext, roots []string, emit func(Event) error) error
}
```

## SQLite design

Production code intentionally does **not** parse SQLite files itself. OpenCode,
Crush, and Antigravity open databases read-only through `database/sql` and
`modernc.org/sqlite`. The connection uses SQLite URI `mode=ro` together with
query-only/defensive settings, and is restricted to one connection. This gives
us real SQLite behavior for WALs, rollback journals, locking, encodings, schema
changes, virtual-table metadata, and edge cases without CGO.

### Experimental mini parser

`internal/minisqlite` preserves the earlier hand-written reader as a learning
exercise. It understands ordinary rowid table B-trees, serial types, overflow
pages, UTF-8/UTF-16 database encodings, and committed WAL-page overlays. It is
**not used by llm_usage production adapters** and intentionally does not attempt
to implement SQL, indexes, virtual tables, or `WITHOUT ROWID` tables.

Fast generated tests cover small/large page sizes, integer serial widths,
negative rowids, blobs/nulls, deep B-trees, overflow pages, UTF-16LE/BE, quoted
identifiers, and a committed but uncheckpointed WAL.

For a broader stress test, download the optional public corpus and compare every
supported table/row/value with real SQLite as the oracle:

```sh
go run ./cmd/minisqlite-corpus -dir /tmp/llm-usage-sqlite-corpus
MINISQLITE_CORPUS_DIR=/tmp/llm-usage-sqlite-corpus \
  go test ./internal/minisqlite -run Corpus -v
```

The corpus binaries are not part of this source tree or release archive. See
`testdata/minisqlite-corpus/README.md` for provenance and licensing notes.

## Tests

```sh
go test ./...
go vet ./...
```

`adapter_sqlite_test.go` creates toy OpenCode, Crush, and Antigravity databases
through the real SQLite driver and verifies the production adapters. The
external miniSQLite corpus test is opt-in; all other tests are self-contained.
