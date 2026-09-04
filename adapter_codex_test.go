package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codexTestTokenLine(timestamp string, input, output, total uint64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}}`, timestamp, input, output, total)
}

func writeCodexTestFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	writeCodexFile(t, path, lines...)
	return path
}

func writeCodexFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadCodexTestFile(t *testing.T, path string) ([]Event, *ParseStats) {
	t.Helper()
	ctx := &LoadContext{Stats: map[string]*ParseStats{}}
	var events []Event
	if err := loadCodexFile(ctx, path, t.TempDir(), func(e Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events, ctx.Stats["codex"]
}

func TestLoadCodexFileFiltersReplayAfterSinglePass(t *testing.T) {
	path := writeCodexTestFile(t,
		codexTestTokenLine("2026-08-01T00:00:00Z", 10, 2, 12),
		`{"timestamp":"2026-08-01T00:00:01Z","type":"event_msg","payload":{"type":"task_started"}}`,
		codexTestTokenLine("2026-08-01T00:00:02Z", 30, 6, 36),
		`{"type":"inter_agent_communication_metadata","payload":{"trigger_turn":true}}`,
	)

	events, stats := loadCodexTestFile(t, path)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(events), events)
	}
	if e := events[0]; e.Input != 20 || e.Output != 4 || e.Total != 24 {
		t.Fatalf("event = %#v, want post-replay delta", e)
	}
	if stats.Lines != 4 || stats.ReplaySkipped != 1 || stats.Skipped != 0 {
		t.Fatalf("stats = %#v, want four lines and one replay skip", stats)
	}
}

func TestCodexTokenizerParsesRepresentativeRecords(t *testing.T) {
	path := writeCodexTestFile(t,
		`{"type":"session_meta","payload":{"cwd":"/tmp/project","id":"session-1"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		codexTestTokenLine("2026-08-01T00:00:00Z", 10, 2, 12),
		`{"type":"response","createdAt":"2026-08-01T00:00:01Z","model_name":"gpt-5.5","result":{"usage":{"input_tokens":"20","output_tokens":4,"total_tokens":24}}}`,
		`{"type":"response_item","payload":{"type":"message","content":[{"text":"contains token_count and usage words"}]}}`,
	)

	events, stats := loadCodexTestFile(t, path)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}
	if e := events[0]; e.Session != "session-1" || e.Project != "/tmp/project" || e.Model != "gpt-5.5" || e.Input != 10 || e.Output != 2 || e.Total != 12 {
		t.Fatalf("token event = %#v, want session metadata and usage fields", e)
	}
	if e := events[1]; e.Model != "gpt-5.5" || e.Input != 20 || e.Output != 4 || e.Total != 24 {
		t.Fatalf("direct usage event = %#v, want nested usage fields", e)
	}
	if stats.Lines != 5 || stats.Malformed != 0 || stats.Skipped != 0 || stats.ReplaySkipped != 0 {
		t.Fatalf("stats = %#v, want five valid lines and no skips", stats)
	}
}

func TestLoadCodexFileKeepsPreTaskUsageWithoutReplayTrigger(t *testing.T) {
	path := writeCodexTestFile(t,
		codexTestTokenLine("2026-08-01T00:00:00Z", 10, 2, 12),
		`{"timestamp":"2026-08-01T00:00:01Z","type":"event_msg","payload":{"type":"task_started"}}`,
		codexTestTokenLine("2026-08-01T00:00:02Z", 30, 6, 36),
	)

	events, stats := loadCodexTestFile(t, path)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}
	if events[0].Input != 10 || events[1].Input != 20 || stats.ReplaySkipped != 0 {
		t.Fatalf("events/stats = %#v / %#v, want both token records and no replay skip", events, stats)
	}
}

func TestCodexHarnessLoadsFilesThroughWorkerPool(t *testing.T) {
	root := t.TempDir()
	line1 := codexTestTokenLine("2026-08-01T00:00:00Z", 10, 2, 12)
	line2 := codexTestTokenLine("2026-08-02T00:00:00Z", 20, 4, 24)
	writeCodexFile(t, filepath.Join(root, "sessions", "2026", "08", "01", "one.jsonl"), line1)
	writeCodexFile(t, filepath.Join(root, "sessions", "2026", "08", "02", "two.jsonl"), line2)

	ctx := &LoadContext{Stats: map[string]*ParseStats{}}
	var events []Event
	if err := (codexHarness{}).Load(ctx, []string{root}, func(e Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}
	stats := ctx.Stats["codex"]
	if stats.Files != 2 || stats.Lines != 2 || stats.Emitted != 2 || stats.Deduplicated != 0 {
		t.Fatalf("stats = %#v, want two parsed and emitted files", stats)
	}
}
