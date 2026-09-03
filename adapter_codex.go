package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type codexHarness struct{}

func (codexHarness) Name() string { return "codex" }
func (codexHarness) DefaultRoots() []string {
	if env := os.Getenv("CODEX_HOME"); env != "" {
		return splitPaths(env)
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".codex")}
}

type rawUsage struct {
	Input              flexUint64 `json:"input_tokens"`
	Prompt             flexUint64 `json:"prompt_tokens"`
	InputAlt           flexUint64 `json:"input"`
	CachedInput        flexUint64 `json:"cached_input_tokens"`
	CacheReadInput     flexUint64 `json:"cache_read_input_tokens"`
	CachedTokens       flexUint64 `json:"cached_tokens"`
	CacheCreationInput flexUint64 `json:"cache_creation_input_tokens"`
	CacheWriteInput    flexUint64 `json:"cache_write_input_tokens"`
	Output             flexUint64 `json:"output_tokens"`
	Completion         flexUint64 `json:"completion_tokens"`
	OutputAlt          flexUint64 `json:"output"`
	ReasoningOutput    flexUint64 `json:"reasoning_output_tokens"`
	Reasoning          flexUint64 `json:"reasoning_tokens"`
	Total              flexUint64 `json:"total_tokens"`
}

type normalizedUsage struct {
	Input, CacheRead, CacheWrite, Output, Reasoning, Total uint64
}

func (r rawUsage) normalized() normalizedUsage {
	input := firstNonZero(uint64(r.Input), uint64(r.Prompt), uint64(r.InputAlt))
	output := firstNonZero(uint64(r.Output), uint64(r.Completion), uint64(r.OutputAlt))
	reasoning := firstNonZero(uint64(r.ReasoningOutput), uint64(r.Reasoning))
	cacheRead := firstNonZero(uint64(r.CachedInput), uint64(r.CacheReadInput), uint64(r.CachedTokens))
	if cacheRead > input {
		cacheRead = input
	}
	cacheWrite := firstNonZero(uint64(r.CacheWriteInput), uint64(r.CacheCreationInput))
	if cacheWrite > input-cacheRead {
		cacheWrite = input - cacheRead
	}
	total := uint64(r.Total)
	if total == 0 {
		total = saturatingSum(input, output)
	}
	return normalizedUsage{Input: input, CacheRead: cacheRead, CacheWrite: cacheWrite, Output: output, Reasoning: reasoning, Total: total}
}

func firstNonZero(vs ...uint64) uint64 {
	for _, v := range vs {
		if v != 0 {
			return v
		}
	}
	return 0
}

type codexLine struct {
	Timestamp      string        `json:"timestamp"`
	CreatedAt      string        `json:"created_at"`
	CreatedAtCamel string        `json:"createdAt"`
	Type           string        `json:"type"`
	Payload        *codexPayload `json:"payload"`
	Usage          *rawUsage     `json:"usage"`
	Model          string        `json:"model"`
	ModelName      string        `json:"model_name"`
	Data           *codexResult  `json:"data"`
	Result         *codexResult  `json:"result"`
	Response       *codexResult  `json:"response"`
}

type codexPayload struct {
	Type           string               `json:"type"`
	Info           *codexInfo           `json:"info"`
	Model          string               `json:"model"`
	ModelName      string               `json:"model_name"`
	ID             string               `json:"id"`
	SessionID      string               `json:"session_id"`
	CWD            string               `json:"cwd"`
	TriggerTurn    bool                 `json:"trigger_turn"`
	ThreadSettings *codexThreadSettings `json:"thread_settings"`
}

type codexThreadSettings struct {
	ServiceTier string `json:"service_tier"`
}

type codexInfo struct {
	LastTokenUsage  *rawUsage `json:"last_token_usage"`
	TotalTokenUsage *rawUsage `json:"total_token_usage"`
	Model           string    `json:"model"`
	ModelName       string    `json:"model_name"`
}

type codexResult struct {
	Timestamp      string    `json:"timestamp"`
	CreatedAt      string    `json:"created_at"`
	CreatedAtCamel string    `json:"createdAt"`
	Usage          *rawUsage `json:"usage"`
	Model          string    `json:"model"`
	ModelName      string    `json:"model_name"`
}

type codexFileRef struct {
	path string
	root string
}

const codexParseWorkers = 10

type codexPendingEvent struct {
	event           Event
	valid           bool
	skipOnReplay    bool
	replaySkipStats bool
}

type codexFileJob struct {
	index int
	ref   codexFileRef
}

type codexFileResult struct {
	index  int
	events []Event
	stats  ParseStats
	err    error
}

func (codexHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("codex")
	seen := map[string]struct{}{}
	var refs []codexFileRef
	for _, root := range roots {
		files, err := discoverCodex(root, ctx.Since, ctx.Until)
		if err != nil {
			return err
		}
		refs = append(refs, files...)
	}
	stats.Files += len(refs)
	if len(refs) == 0 {
		return nil
	}

	workerCount := min(codexParseWorkers, len(refs))
	jobs := make(chan codexFileJob)
	results := make(chan codexFileResult, len(refs))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer workers.Done()
			for job := range jobs {
				fileCtx := &LoadContext{
					Strict:  ctx.Strict,
					Verbose: ctx.Verbose,
					Report:  ctx.Report,
					Since:   ctx.Since,
					Until:   ctx.Until,
					Stats:   map[string]*ParseStats{},
				}
				events := make([]Event, 0)
				err := loadCodexFile(fileCtx, job.ref.path, job.ref.root, func(e Event) error {
					events = append(events, e)
					return nil
				})
				results <- codexFileResult{
					index:  job.index,
					events: events,
					stats:  *fileCtx.stat("codex"),
					err:    err,
				}
			}
		}()
	}
	for i, ref := range refs {
		jobs <- codexFileJob{index: i, ref: ref}
	}
	close(jobs)
	workers.Wait()
	close(results)

	parsed := make([]codexFileResult, len(refs))
	for result := range results {
		parsed[result.index] = result
	}
	for _, result := range parsed {
		mergeCodexStats(stats, result.stats)
		if result.err != nil {
			return result.err
		}
		for _, e := range result.events {
			key := fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d", e.Session, e.Timestamp.UnixNano(), e.Model, e.Input, e.Output, e.CacheRead, e.CacheWrite, e.Total)
			if _, dup := seen[key]; dup {
				stats.Deduplicated++
				continue
			}
			seen[key] = struct{}{}
			stats.Emitted++
			if err := emit(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeCodexStats(dst *ParseStats, src ParseStats) {
	dst.Lines += src.Lines
	dst.Malformed += src.Malformed
	dst.Skipped += src.Skipped
	dst.ReplaySkipped += src.ReplaySkipped
}

func discoverCodex(root string, since, until time.Time) ([]codexFileRef, error) {
	sessions := filepath.Join(root, "sessions")
	archived := filepath.Join(root, "archived_sessions")
	ss, sErr := os.Stat(sessions)
	as, aErr := os.Stat(archived)
	if (sErr != nil || !ss.IsDir()) && (aErr != nil || !as.IsDir()) {
		files, err := walkJSONL(root, since, until)
		refs := make([]codexFileRef, len(files))
		for i, p := range files {
			refs[i] = codexFileRef{path: p, root: root}
		}
		return refs, err
	}
	// Active sessions win when the same relative JSONL path also appears in
	// archived_sessions.
	chosen := map[string]string{}
	if sErr == nil && ss.IsDir() {
		files, err := walkJSONL(sessions, since, until)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			rel, _ := filepath.Rel(sessions, p)
			chosen[filepath.ToSlash(rel)] = p
		}
	}
	if aErr == nil && as.IsDir() {
		files, err := walkJSONL(archived, since, until)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			rel, _ := filepath.Rel(archived, p)
			key := filepath.ToSlash(rel)
			if _, exists := chosen[key]; !exists {
				chosen[key] = p
			}
		}
	}
	keys := make([]string, 0, len(chosen))
	for k := range chosen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	refs := make([]codexFileRef, 0, len(keys))
	for _, k := range keys {
		refs = append(refs, codexFileRef{path: chosen[k], root: root})
	}
	return refs, nil
}

func loadCodexFile(ctx *LoadContext, path, root string, emit func(Event) error) error {
	stats := ctx.stat("codex")
	session := derivedSession(path)
	project := ""
	currentModel := ""
	var previous *normalizedUsage
	hasTask, hasTrigger := false, false
	pending := make([]codexPendingEvent, 0)

	if err := forEachLine(path, func(line []byte, lineNo int) error {
		stats.Lines++
		// Avoid decoding large transcript/tool records that cannot contain
		// usage or session metadata.
		if !bytes.Contains(line, []byte("token_count")) &&
			!bytes.Contains(line, []byte("session_meta")) &&
			!bytes.Contains(line, []byte("turn_context")) &&
			!bytes.Contains(line, []byte("task_started")) &&
			!bytes.Contains(line, []byte("inter_agent_communication")) &&
			!bytes.Contains(line, []byte(`"usage"`)) {
			return nil
		}
		var row codexLine
		if err := json.Unmarshal(line, &row); err != nil {
			return reportMalformed(ctx, "codex", path, lineNo, err)
		}

		taskStarted := false
		if row.Payload != nil {
			p := row.Payload
			taskStarted = row.Type == "event_msg" && p.Type == "task_started"
			if taskStarted {
				hasTask = true
			}
			if (row.Type == "inter_agent_communication_metadata" || row.Type == "inter_agent_communication") && p.TriggerTurn {
				hasTrigger = true
			}
			if row.Type == "session_meta" {
				if p.ID != "" {
					session = p.ID
				} else if p.SessionID != "" {
					session = p.SessionID
				}
				if p.CWD != "" {
					project = p.CWD
				}
			}
			if row.Type == "turn_context" || p.Type == "turn_context" {
				currentModel = firstString(p.Model, p.ModelName, currentModel)
			}
			if row.Type == "event_msg" && p.Type == "token_count" && p.Info != nil {
				info := p.Info
				model := firstString(info.Model, info.ModelName, p.Model, p.ModelName, currentModel)
				if model != "" {
					currentModel = model
				}
				var delta normalizedUsage
				var have bool
				if info.TotalTokenUsage != nil {
					cur := info.TotalTokenUsage.normalized()
					if previous == nil {
						if info.LastTokenUsage != nil {
							delta = info.LastTokenUsage.normalized()
						} else {
							delta = cur
						}
					} else {
						delta = usageDelta(cur, *previous)
						if delta.Total == 0 && !usageEqual(cur, *previous) && info.LastTokenUsage != nil {
							// Counter reset / incompatible schema change: use the explicit
							// per-turn value rather than dropping a real request.
							delta = info.LastTokenUsage.normalized()
						}
					}
					previous = &cur
					have = delta.Total > 0 || delta.Input > 0 || delta.Output > 0
				} else if info.LastTokenUsage != nil {
					delta = info.LastTokenUsage.normalized()
					have = delta.Total > 0 || delta.Input > 0 || delta.Output > 0
				}
				if !have {
					return nil
				}
				t, ok := parseTimestamp(row.Timestamp)
				pending = append(pending, codexPendingEvent{event: Event{
					Harness: "codex", Session: session, Project: project, Model: model, Timestamp: t,
					Input: delta.Input, Output: delta.Output, CacheRead: delta.CacheRead,
					CacheWrite: delta.CacheWrite, Reasoning: delta.Reasoning, Total: delta.Total,
					InputIncludesCache: true,
				}, valid: ok, skipOnReplay: !hasTask, replaySkipStats: !hasTask})
				return nil
			}
		}

		// codex exec --json and SDK wrappers can expose a direct per-request
		// usage object rather than rollout event_msg/token_count records.
		usage, model, ts := directCodexUsage(row)
		if usage == nil {
			return nil
		}
		t, ok := parseTimestamp(ts)
		u := usage.normalized()
		if u.Total == 0 {
			return nil
		}
		if model == "" {
			model = currentModel
		}
		pending = append(pending, codexPendingEvent{event: Event{
			Harness: "codex", Session: session, Project: project, Model: model, Timestamp: t,
			Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			Reasoning: u.Reasoning, Total: u.Total, InputIncludesCache: true,
		}, valid: ok, skipOnReplay: taskStarted, replaySkipStats: false})
		return nil
	}); err != nil {
		return err
	}

	replayBoundary := hasTask && hasTrigger
	for _, pendingEvent := range pending {
		if pendingEvent.skipOnReplay && replayBoundary {
			if pendingEvent.replaySkipStats {
				stats.ReplaySkipped++
			}
			continue
		}
		if !pendingEvent.valid {
			stats.Skipped++
			continue
		}
		if err := emit(pendingEvent.event); err != nil {
			return err
		}
	}
	return nil
}

func usageEqual(a, b normalizedUsage) bool {
	return a == b
}

func usageDelta(cur, prev normalizedUsage) normalizedUsage {
	// A cumulative counter must be monotonic. If any billing dimension goes
	// backwards, signal a reset by returning zero; the caller can use last usage.
	if cur.Input < prev.Input || cur.Output < prev.Output || cur.CacheRead < prev.CacheRead || cur.CacheWrite < prev.CacheWrite || cur.Total < prev.Total {
		return normalizedUsage{}
	}
	return normalizedUsage{
		Input:      cur.Input - prev.Input,
		CacheRead:  cur.CacheRead - prev.CacheRead,
		CacheWrite: cur.CacheWrite - prev.CacheWrite,
		Output:     cur.Output - prev.Output,
		Reasoning:  saturatingSub(cur.Reasoning, prev.Reasoning),
		Total:      cur.Total - prev.Total,
	}
}

func directCodexUsage(row codexLine) (*rawUsage, string, string) {
	if row.Usage != nil {
		return row.Usage, firstString(row.Model, row.ModelName), firstString(row.Timestamp, row.CreatedAt, row.CreatedAtCamel)
	}
	for _, r := range []*codexResult{row.Data, row.Result, row.Response} {
		if r != nil && r.Usage != nil {
			return r.Usage, firstString(r.Model, r.ModelName, row.Model, row.ModelName), firstString(r.Timestamp, r.CreatedAt, r.CreatedAtCamel, row.Timestamp, row.CreatedAt, row.CreatedAtCamel)
		}
	}
	return nil, "", ""
}

func firstString(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
