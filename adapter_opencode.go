package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type openCodeHarness struct{}

func (openCodeHarness) Name() string { return "opencode" }

func (openCodeHarness) DefaultRoots() []string {
	if env, ok := os.LookupEnv("OPENCODE_DATA_DIR"); ok {
		return splitPaths(env)
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" && filepath.IsAbs(xdg) {
		return []string{filepath.Join(filepath.Clean(xdg), "opencode")}
	}
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".local", "share", "opencode")}
}

type openCodeEvent struct {
	Event
	ID string
}

func (openCodeHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("opencode")
	seen := map[string]bool{}
	var events []openCodeEvent

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		base := root
		if !info.IsDir() {
			base = filepath.Dir(root)
		}

		if dbPath := openCodeDBPath(root); dbPath != "" {
			stats.Files++
			dbEvents, err := parseOpenCodeDB(dbPath, ctx)
			if err != nil {
				if ctx.Strict {
					return err
				}
				stats.Malformed++
				if ctx.Verbose {
					fmt.Fprintf(os.Stderr, "opencode: skip %s: %v\n", dbPath, err)
				}
			} else {
				for _, e := range dbEvents {
					if e.ID != "" && seen[e.ID] {
						stats.Deduplicated++
						continue
					}
					if e.ID != "" {
						seen[e.ID] = true
					}
					events = append(events, e)
				}
			}
		}

		// Legacy files are storage/message/<session>/<message>.json. Database
		// IDs win, matching current ccusage precedence.
		legacyRoot := filepath.Join(base, "storage", "message")
		if st, err := os.Stat(legacyRoot); err == nil && st.IsDir() {
			files, err := walkExtensions(legacyRoot, ctx.Since, ctx.Until, ".json")
			if err != nil {
				return err
			}
			for _, path := range files {
				idHint := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				if idHint != "" && seen[idHint] {
					continue
				}
				stats.Files++
				stats.Lines++
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				e, ok, err := parseOpenCodeMessage(b, idHint, "", 0, fileModifiedTime(path))
				if err != nil {
					if e2 := reportMalformed(ctx, "opencode", path, 1, err); e2 != nil {
						return e2
					}
					continue
				}
				if !ok {
					stats.Skipped++
					continue
				}
				if e.ID != "" && seen[e.ID] {
					stats.Deduplicated++
					continue
				}
				if e.ID != "" {
					seen[e.ID] = true
				}
				events = append(events, e)
			}
		}
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp.Before(events[j].Timestamp) })
	for _, e := range events {
		if err := emit(e.Event); err != nil {
			return err
		}
		stats.Emitted++
	}
	return nil
}

func openCodeDBPath(root string) string {
	st, err := os.Stat(root)
	if err != nil {
		return ""
	}
	if !st.IsDir() {
		if strings.EqualFold(filepath.Ext(root), ".db") {
			return root
		}
		return ""
	}
	preferred := filepath.Join(root, "opencode.db")
	if st, err := os.Stat(preferred); err == nil && !st.IsDir() {
		return preferred
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "opencode-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		channel := strings.TrimSuffix(strings.TrimPrefix(name, "opencode-"), ".db")
		if channel == "" {
			continue
		}
		valid := true
		for _, r := range channel {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				valid = false
				break
			}
		}
		if valid {
			candidates = append(candidates, filepath.Join(root, name))
		}
	}
	sort.Strings(candidates)
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func parseOpenCodeDB(path string, ctx *LoadContext) ([]openCodeEvent, error) {
	db, err := openSQLiteReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("opencode: %s: %w", path, err)
	}
	defer db.Close()
	stats := ctx.stat("opencode")
	var out []openCodeEvent
	seen := map[string]bool{}

	if exists, err := sqliteTableExists(db, "message"); err != nil {
		return nil, err
	} else if exists {
		cols, err := sqliteTableColumns(db, "message")
		if err != nil {
			return nil, err
		}
		if sqliteHasColumns(cols, "id", "session_id", "data") {
			rows, err := db.Query(`SELECT id, session_id, data FROM message`)
			if err != nil {
				return nil, sqliteQueryError(path, "message", err)
			}
			for rows.Next() {
				stats.Lines++
				var id, session, data sql.NullString
				if err := rows.Scan(&id, &session, &data); err != nil {
					rows.Close()
					return nil, err
				}
				if !data.Valid {
					stats.Skipped++
					continue
				}
				e, ok, err := parseOpenCodeMessage([]byte(data.String), id.String, session.String, 0, time.Unix(0, 0).UTC())
				if err != nil {
					if ctx.Strict {
						rows.Close()
						return nil, fmt.Errorf("opencode: %s message %q: %w", path, id.String, err)
					}
					stats.Malformed++
					continue
				}
				if !ok {
					stats.Skipped++
					continue
				}
				if e.ID != "" && seen[e.ID] {
					stats.Deduplicated++
					continue
				}
				if e.ID != "" {
					seen[e.ID] = true
				}
				out = append(out, e)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}

	// OpenCode v2 moved assistant messages to session_message with the type
	// discriminator outside the JSON data blob.
	if exists, err := sqliteTableExists(db, "session_message"); err != nil {
		return nil, err
	} else if exists {
		cols, err := sqliteTableColumns(db, "session_message")
		if err != nil {
			return nil, err
		}
		if sqliteHasColumns(cols, "id", "session_id", "type", "data") {
			createdExpr := "0"
			if cols["time_created"] {
				createdExpr = "time_created"
			}
			rows, err := db.Query(`SELECT id, session_id, type, data, ` + createdExpr + ` FROM session_message`)
			if err != nil {
				return nil, sqliteQueryError(path, "session_message", err)
			}
			for rows.Next() {
				stats.Lines++
				var id, session, kind, data sql.NullString
				var created sql.NullInt64
				if err := rows.Scan(&id, &session, &kind, &data, &created); err != nil {
					rows.Close()
					return nil, err
				}
				if kind.String != "assistant" {
					continue
				}
				if id.String != "" && seen[id.String] {
					stats.Deduplicated++
					continue
				}
				if !data.Valid {
					stats.Skipped++
					continue
				}
				e, ok, err := parseOpenCodeMessage([]byte(data.String), id.String, session.String, created.Int64, time.Unix(0, 0).UTC())
				if err != nil {
					if ctx.Strict {
						rows.Close()
						return nil, fmt.Errorf("opencode: %s session_message %q: %w", path, id.String, err)
					}
					stats.Malformed++
					continue
				}
				if !ok {
					stats.Skipped++
					continue
				}
				if e.ID != "" {
					seen[e.ID] = true
				}
				out = append(out, e)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}
	return out, nil
}

func parseOpenCodeMessage(data []byte, idHint, sessionHint string, createdHint int64, fallback time.Time) (openCodeEvent, bool, error) {
	var record map[string]any
	if err := decodeJSONUseNumber(data, &record); err != nil {
		return openCodeEvent{}, false, err
	}
	tokens, ok := record["tokens"].(map[string]any)
	if !ok {
		return openCodeEvent{}, false, nil
	}
	input := firstJSONU64(tokens, "input")
	output := firstJSONU64(tokens, "output")
	reasoning := firstJSONU64(tokens, "reasoning")
	var cacheRead, cacheWrite uint64
	if cache, ok := tokens["cache"].(map[string]any); ok {
		cacheRead = firstJSONU64(cache, "read")
		cacheWrite = firstJSONU64(cache, "write")
	}
	total := firstJSONU64(tokens, "total")
	known := saturatingSum(input, output, reasoning, cacheRead, cacheWrite)
	if total > known {
		missing := total - known
		if output == 0 {
			output = missing
		} else {
			reasoning = saturatingAdd(reasoning, missing)
		}
	}
	known = saturatingSum(input, output, reasoning, cacheRead, cacheWrite)
	if total < known {
		total = known
	}
	cost, costOK := jsonFloat64(record["cost"])
	if known == 0 && !(costOK && cost > 0) {
		return openCodeEvent{}, false, nil
	}

	model := firstMapString(record, "modelID")
	provider := firstMapString(record, "providerID")
	if nested, ok := record["model"].(map[string]any); ok {
		if model == "" {
			model = firstMapString(nested, "id", "modelID")
		}
		if provider == "" {
			provider = firstMapString(nested, "providerID")
		}
	}
	if model == "" {
		return openCodeEvent{}, false, nil
	}
	model = normalizeOpenCodeModel(model)
	_ = provider // provider is useful for source interpretation; reporting groups by model only.

	id := idHint
	if id == "" {
		id = firstMapString(record, "id")
	}
	session := sessionHint
	if session == "" {
		session = firstMapString(record, "sessionID")
	}
	if session == "" {
		session = "unknown"
	}

	created := uint64(0)
	if tm, ok := record["time"].(map[string]any); ok {
		created = firstJSONU64(tm, "created")
	}
	if created == 0 && createdHint > 0 {
		created = uint64(createdHint)
	}
	ts := fallback
	if created > 0 && created <= math.MaxInt64 {
		ts = time.UnixMilli(int64(created)).UTC()
	}
	if ts.IsZero() {
		ts = time.Unix(0, 0).UTC()
	}

	e := Event{
		Harness: "opencode", Session: session, Project: "opencode", Model: model, Timestamp: ts,
		Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
		Reasoning: reasoning, BilledOutput: saturatingAdd(output, reasoning), Total: total,
	}
	if costOK && cost > 0 {
		e.Cost, e.CostKnown = cost, true
	}
	return openCodeEvent{Event: e, ID: id}, true, nil
}

func normalizeOpenCodeModel(model string) string {
	switch strings.TrimSpace(model) {
	case "gemini-3-pro-high":
		return "gemini-3-pro-preview"
	case "k2p6":
		return "kimi-k2.6"
	default:
		return strings.TrimSpace(model)
	}
}

func jsonFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case int64:
		return float64(x), true
	}
	return 0, false
}
