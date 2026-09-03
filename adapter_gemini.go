package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type geminiHarness struct{}

func (geminiHarness) Name() string { return "gemini" }

func (geminiHarness) DefaultRoots() []string {
	if env := os.Getenv("GEMINI_DATA_DIR"); strings.TrimSpace(env) != "" {
		return splitPaths(env)
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".gemini", "tmp")}
}

type geminiTokens struct {
	Input, Output, Cached, Thoughts, Tool uint64
	Total                                 uint64
	HasTotal                              bool
}

func (geminiHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("gemini")
	for _, root := range roots {
		files, err := walkExtensions(root, ctx.Since, ctx.Until, ".json", ".jsonl")
		if err != nil {
			return err
		}
		for _, path := range files {
			stats.Files++
			var events []Event
			if strings.EqualFold(filepath.Ext(path), ".jsonl") {
				events, err = parseGeminiJSONL(ctx, path)
			} else {
				events, err = parseGeminiJSON(ctx, path)
			}
			if err != nil {
				return err
			}
			for _, e := range events {
				if err := emit(e); err != nil {
					return err
				}
				stats.Emitted++
			}
		}
	}
	return nil
}

func parseGeminiJSON(ctx *LoadContext, path string) ([]Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record map[string]any
	if err := decodeJSONUseNumber(b, &record); err != nil {
		if e := reportMalformed(ctx, "gemini", path, 1, err); e != nil {
			return nil, e
		}
		return nil, nil
	}
	stats := ctx.stat("gemini")
	stats.Lines++
	fallback := fileModifiedTime(path)
	session := firstMapString(record, "sessionId", "session_id")
	if session == "" {
		session = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	sessionTS := firstTimestamp(record, "startTime", "lastUpdated")
	if sessionTS.IsZero() {
		sessionTS = fallback
	}

	if messages, ok := record["messages"].([]any); ok {
		out := make([]Event, 0, len(messages))
		for _, raw := range messages {
			m, ok := raw.(map[string]any)
			if !ok || stringValue(m["type"]) != "gemini" {
				continue
			}
			if e, ok := geminiDirectEvent(m, "", session, sessionTS); ok {
				out = append(out, e)
			}
		}
		return out, nil
	}
	if stringValue(record["type"]) == "gemini" {
		if e, ok := geminiDirectEvent(record, "", session, fallback); ok {
			return []Event{e}, nil
		}
		return nil, nil
	}
	return geminiStatsEvents(geminiStatsObject(record), firstMapString(record, "model"), session, firstTimestampOr(record, fallback, "timestamp")), nil
}

func parseGeminiJSONL(ctx *LoadContext, path string) ([]Event, error) {
	fallback := fileModifiedTime(path)
	session := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	currentModel := ""
	var events []Event
	directByID := map[string]int{}
	stats := ctx.stat("gemini")
	err := forEachLine(path, func(line []byte, lineNo int) error {
		stats.Lines++
		var record map[string]any
		if err := decodeJSONUseNumber(line, &record); err != nil {
			return reportMalformed(ctx, "gemini", path, lineNo, err)
		}
		if s := firstMapString(record, "sessionId", "session_id"); s != "" {
			session = s
		}
		if m := firstMapString(record, "model"); m != "" {
			currentModel = m
		}
		if stringValue(record["type"]) == "gemini" {
			e, ok := geminiDirectEvent(record, currentModel, session, fallback)
			if !ok {
				stats.Skipped++
				return nil
			}
			id := firstMapString(record, "id")
			if id != "" {
				if i, exists := directByID[id]; exists {
					events[i] = e
					stats.Deduplicated++
					return nil
				}
				directByID[id] = len(events)
			}
			events = append(events, e)
			return nil
		}
		if obj := geminiStatsObject(record); obj != nil {
			ts := firstTimestampOr(record, fallback, "timestamp")
			events = append(events, geminiStatsEvents(obj, currentModel, session, ts)...)
		}
		return nil
	})
	return events, err
}

func geminiDirectEvent(record map[string]any, modelHint, session string, fallback time.Time) (Event, bool) {
	tokens, ok := parseGeminiTokens(record["tokens"])
	if !ok {
		return Event{}, false
	}
	model := firstMapString(record, "model")
	if model == "" {
		model = modelHint
	}
	if strings.TrimSpace(model) == "" {
		return Event{}, false
	}
	ts := firstTimestampOr(record, fallback, "timestamp", "created_at")
	input, cached := normalizeGeminiSessionInput(tokens)
	return buildGeminiEvent(model, session, ts, tokens, input, cached)
}

func geminiStatsObject(record map[string]any) map[string]any {
	if v, ok := record["stats"].(map[string]any); ok {
		return v
	}
	if result, ok := record["result"].(map[string]any); ok {
		if v, ok := result["stats"].(map[string]any); ok {
			return v
		}
	}
	return nil
}

func geminiStatsEvents(stats map[string]any, modelHint, session string, ts time.Time) []Event {
	if stats == nil {
		return nil
	}
	if models, ok := stats["models"].(map[string]any); ok {
		var out []Event
		keys := make([]string, 0, len(models))
		for model := range models {
			keys = append(keys, model)
		}
		sort.Strings(keys)
		for _, model := range keys {
			data, ok := models[model].(map[string]any)
			if !ok {
				continue
			}
			tokens, ok := parseGeminiTokens(data["tokens"])
			if !ok {
				continue
			}
			input := saturatingSub(tokens.Input, minU64(tokens.Input, tokens.Cached))
			if e, ok := buildGeminiEvent(model, session, ts, tokens, input, tokens.Cached); ok {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	tokens, ok := parseGeminiTokens(stats)
	if !ok {
		return nil
	}
	model := modelHint
	if model == "" {
		model = "unknown"
	}
	input := saturatingSub(tokens.Input, minU64(tokens.Input, tokens.Cached))
	if e, ok := buildGeminiEvent(model, session, ts, tokens, input, tokens.Cached); ok {
		return []Event{e}
	}
	return nil
}

func buildGeminiEvent(model, session string, ts time.Time, tokens geminiTokens, inputWithoutCache, cacheRead uint64) (Event, bool) {
	input := saturatingAdd(inputWithoutCache, tokens.Tool)
	output := tokens.Output
	reasoning := tokens.Thoughts
	total := tokens.Total
	if !tokens.HasTotal || total == 0 {
		total = saturatingSum(input, output, cacheRead, reasoning)
	} else {
		// Gemini logs are not completely uniform across CLI releases. Match
		// ccusage's total-token fallback: if a recorded total contains tokens
		// that are absent from the individual buckets, treat them as output when
		// output is otherwise unknown, or as extra/reasoning tokens when output
		// is already known.
		known := saturatingSum(input, output, cacheRead, reasoning)
		missing := saturatingSub(total, known)
		if missing > 0 {
			if output == 0 {
				output = missing
			} else {
				reasoning = saturatingAdd(reasoning, missing)
			}
		}
	}
	if total == 0 && input == 0 && output == 0 && cacheRead == 0 && reasoning == 0 {
		return Event{}, false
	}
	return Event{
		Harness: "gemini", Session: session, Project: "gemini", Model: model, Timestamp: ts,
		Input: input, Output: output, CacheRead: cacheRead, Reasoning: reasoning,
		BilledOutput: saturatingAdd(output, reasoning), Total: total,
	}, true
}

func normalizeGeminiSessionInput(tokens geminiTokens) (uint64, uint64) {
	inclusive := saturatingSum(tokens.Input, tokens.Output, tokens.Thoughts, tokens.Tool)
	exclusive := saturatingAdd(inclusive, tokens.Cached)
	if tokens.Cached > 0 && tokens.HasTotal && tokens.Total == inclusive && tokens.Total != exclusive {
		return saturatingSub(tokens.Input, minU64(tokens.Input, tokens.Cached)), tokens.Cached
	}
	return tokens.Input, tokens.Cached
}

func parseGeminiTokens(raw any) (geminiTokens, bool) {
	record, ok := raw.(map[string]any)
	if !ok {
		return geminiTokens{}, false
	}
	var t geminiTokens
	t.Input = firstJSONU64(record, "input", "prompt", "input_tokens", "prompt_tokens")
	t.Output = firstJSONU64(record, "output", "candidates", "output_tokens", "candidates_tokens")
	t.Cached = firstJSONU64(record, "cached", "cached_tokens")
	t.Thoughts = firstJSONU64(record, "thoughts", "reasoning", "thoughts_tokens", "reasoning_tokens")
	t.Tool = firstJSONU64(record, "tool", "tool_tokens")
	if v, ok := jsonU64(record["total"]); ok {
		t.Total, t.HasTotal = v, true
	} else if v, ok := jsonU64(record["total_tokens"]); ok {
		t.Total, t.HasTotal = v, true
	}
	return t, true
}
