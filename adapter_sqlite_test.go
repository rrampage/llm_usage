package main

import (
	"database/sql"
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"
)

func newSQLiteFixture(t *testing.T, name string) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return path, db
}

func TestOpenCodeSQLiteV1AndV2(t *testing.T) {
	path, db := newSQLiteFixture(t, "opencode.db")
	_, err := db.Exec(`
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT);
		CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, time_created INTEGER, data TEXT);
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO message VALUES(?,?,?,?)`, "m1", "s1", int64(1767312000000),
		`{"id":"m1","sessionID":"s1","providerID":"anthropic","modelID":"claude-sonnet-4-20250514","time":{"created":1767312000000},"tokens":{"input":100,"output":50,"reasoning":7,"cache":{"read":10,"write":20},"total":187},"cost":0.02}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session_message VALUES(?,?,?,?,?)`, "m2", "s2", "assistant", int64(1767398400000),
		`{"model":{"id":"gpt-5.4","providerID":"openai"},"tokens":{"input":200,"output":80,"cache":{"read":25,"write":5},"total":310},"cost":0.03}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session_message VALUES(?,?,?,?,?)`, "m3", "s2", "user", int64(1767398400000), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := &LoadContext{Stats: map[string]*ParseStats{}}
	events, err := parseOpenCodeDB(path, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if e := events[0].Event; e.Session != "s1" || e.Model != "claude-sonnet-4-20250514" || e.Input != 100 || e.Output != 50 || e.Reasoning != 7 || e.CacheRead != 10 || e.CacheWrite != 20 || e.Total != 187 || !e.CostKnown || e.Cost != 0.02 {
		t.Fatalf("v1 event = %#v", e)
	}
	if e := events[1].Event; e.Session != "s2" || e.Model != "gpt-5.4" || e.Input != 200 || e.Output != 80 || e.CacheRead != 25 || e.CacheWrite != 5 || e.Total != 310 || !e.CostKnown || e.Cost != 0.03 {
		t.Fatalf("v2 event = %#v", e)
	}
}

func TestCrushSQLiteRootChildAndMixedModels(t *testing.T) {
	path, db := newSQLiteFixture(t, "crush.db")
	_, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY, parent_session_id TEXT, prompt_tokens INTEGER,
			completion_tokens INTEGER, cost REAL, created_at INTEGER
		);
		CREATE TABLE messages (
			id TEXT PRIMARY KEY, session_id TEXT, role TEXT, model TEXT, provider TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, parent         string
		prompt, completion int64
		cost               float64
	}{
		{"root", "", 1000, 200, 1.25},
		{"child", "root", 400, 80, 0.25},
		{"root2", "", 300, 40, 0.50},
	} {
		if _, err := db.Exec(`INSERT INTO sessions VALUES(?,?,?,?,?,?)`, row.id, row.parent, row.prompt, row.completion, row.cost, int64(1767312000)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ id, sid, model string }{
		{"a", "root", "claude-opus-4-6"},
		{"b", "child", "gpt-5.4"},
		{"c", "root2", "gemini-2.5-pro"},
	} {
		if _, err := db.Exec(`INSERT INTO messages VALUES(?,?,?,?,?)`, row.id, row.sid, "assistant", row.model, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := parseCrushDB(path, "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events: %#v", len(events), events)
	}
	if events[0].Session != "root" || events[0].Model != "crush:mixed" || events[0].Input != 1000 || events[0].Output != 200 || events[0].Cost != 1.25 || !events[0].CostKnown {
		t.Fatalf("root = %#v", events[0])
	}
	if events[1].Session != "root2" || events[1].Model != "gemini-2.5-pro" || events[1].Total != 340 {
		t.Fatalf("root2 = %#v", events[1])
	}
}

func TestAntigravitySQLiteProtobufBlob(t *testing.T) {
	path, db := newSQLiteFixture(t, "conversation.db")
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	usage := protoMessage(
		protoV(1, 246), // gemini-2.5-pro
		protoV(2, 100),
		protoV(3, 30),
		protoV(4, 5),
		protoV(5, 10),
		protoV(9, 7),
		protoV(10, 23),
		protoB(11, []byte("resp-1")),
	)
	ts := protoMessage(protoV(1, 1767312000), protoV(2, 123000000))
	generationInfo := protoMessage(protoB(4, ts))
	chat := protoMessage(protoV(3, 246), protoB(4, usage), protoB(9, generationInfo), protoB(19, []byte("gemini 2.5 pro")))
	metadata := protoMessage(protoB(1, chat))
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx,data) VALUES(1,?)`, metadata); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := parseAntigravityDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events: %#v", len(events), events)
	}
	e := events[0].Event
	if e.Model != "gemini-2.5-pro" || e.Input != 100 || e.Output != 23 || e.Reasoning != 7 || e.CacheWrite != 5 || e.CacheRead != 10 || e.Total != 145 || e.BilledOutput != 30 {
		t.Fatalf("event = %#v", e)
	}
	want := time.Unix(1767312000, 123000000).UTC()
	if !e.Timestamp.Equal(want) {
		t.Fatalf("timestamp=%s want %s", e.Timestamp, want)
	}
}

func TestAntigravityPlaceholderModelAliases(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		id   uint64
		want string
	}{
		{name: "gemini 3.8 flash", id: 1318, want: "gemini-3.8-flash"},
		{name: "new placeholder alias", id: 1319, want: "gemini-3.8-flash"},
		{name: "known legacy placeholder", id: 1026, want: "claude-opus-4-6"},
		{name: "recorded name for unknown ID", raw: "gemini-3.8-flash", id: 1999, want: "gemini-3.8-flash"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityModel(tc.raw, tc.id); got != tc.want {
				t.Fatalf("antigravityModel(%q, %d) = %q, want %q", tc.raw, tc.id, got, tc.want)
			}
		})
	}
}

func TestPriceBookResolvesAntigravityPlaceholder(t *testing.T) {
	want := Price{Input: 0.75, Output: 3.75, CacheRead: 0.075, CacheWrite: 0.75}
	book := PriceBook{"gemini-3.8-flash": want}

	for _, model := range []string{"model_placeholder_m318", "model_placeholder_m319"} {
		got, ok := book.price(model)
		if !ok {
			t.Fatalf("price lookup did not resolve Antigravity placeholder %q", model)
		}
		if !pricesEqual(got, want) {
			t.Fatalf("resolved price for %q = %#v, want %#v", model, got, want)
		}
	}
}

type protoPiece []byte

func protoMessage(parts ...protoPiece) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func protoV(field uint64, value uint64) protoPiece {
	var out []byte
	out = appendProtoVarint(out, field<<3)
	out = appendProtoVarint(out, value)
	return out
}

func protoB(field uint64, value []byte) protoPiece {
	var out []byte
	out = appendProtoVarint(out, field<<3|2)
	out = appendProtoVarint(out, uint64(len(value)))
	out = append(out, value...)
	return out
}

func appendProtoVarint(out []byte, value uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], value)
	return append(out, buf[:n]...)
}
