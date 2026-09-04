`llm_usage` is an offline-first local usage reporter for AI coding harnesses inspired by [ccusage](https://github.com/ccusage/ccusage). It normalizes token/cost records from Claude Code, Codex, pi-agent, Gemini CLI, Google Antigravity, OpenCode, and Crush, then reports daily/weekly/monthly, session, or model totals.

Normal reporting does not make network requests. The only network-capable production command is the explicit `llm_usage pricing update` operation.

## Build

The module targets **Go 1.26** and uses `modernc.org/sqlite`, a pure-Go, CGO-free SQLite driver, for harnesses whose authoritative history is stored in
SQLite.

```sh
go build -o llm_usage .
```

To install the command into your Go bin directory:

```sh
go install github.com/rrampage/llm_usage@latest
```

To install a specific release:

```sh
go install github.com/rrampage/llm_usage@v0.1.0
```


## Usage

```sh
llm_usage                 # equivalent to: llm_usage daily (last 3 months by default)
llm_usage --all           # daily report across all history
llm_usage weekly
llm_usage monthly
llm_usage session
llm_usage model
llm_usage daily --harness codex
llm_usage monthly --harness crush --path crush=/src/project/.crush
llm_usage daily --harness claude --model claude-sonnet-4-20250514
```

Period reports group by **period x model** across harnesses and end with a
`TOTAL` row.

Useful filters/overrides:

```sh
llm_usage --since 2026-09-01 --until 2026-09-03
llm_usage --all           # disable the default 3-month cutoff for daily reports
llm_usage --json
llm_usage daily --path opencode=/tmp/opencode --path crush=/src/project/.crush
```
