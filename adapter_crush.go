package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type crushHarness struct{}

func (crushHarness) Name() string { return "crush" }

func (crushHarness) DefaultRoots() []string {
	roots := []string{crushGlobalDataDir()}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, filepath.Join(wd, ".crush"))
	}
	return uniqueStrings(roots)
}

func crushGlobalDataDir() string {
	if value := strings.TrimSpace(os.Getenv("CRUSH_GLOBAL_DATA")); value != "" {
		return filepath.Clean(expandHome(value))
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(filepath.Clean(xdg), "crush")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "crush")
}

type crushProject struct {
	Path    string `json:"path"`
	DataDir string `json:"data_dir"`
}

type crushProjectList struct {
	Projects []crushProject `json:"projects"`
}

type crushDBRef struct {
	Path    string
	Project string
}

func (crushHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("crush")
	var refs []crushDBRef
	for _, root := range roots {
		discovered, err := discoverCrushDBs(root)
		if err != nil {
			if ctx.Strict {
				return err
			}
			if ctx.Verbose {
				fmt.Fprintf(os.Stderr, "crush: skip discovery root %s: %v\n", root, err)
			}
			continue
		}
		refs = append(refs, discovered...)
	}

	seenDB := map[string]bool{}
	var events []Event
	for _, ref := range refs {
		canonical, err := filepath.EvalSymlinks(ref.Path)
		if err != nil {
			canonical = filepath.Clean(ref.Path)
		}
		if seenDB[canonical] {
			continue
		}
		seenDB[canonical] = true
		stats.Files++
		parsed, err := parseCrushDB(ref.Path, ref.Project)
		if err != nil {
			if ctx.Strict {
				return err
			}
			stats.Malformed++
			if ctx.Verbose {
				fmt.Fprintf(os.Stderr, "crush: skip %s: %v\n", ref.Path, err)
			}
			continue
		}
		events = append(events, parsed...)
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp.Before(events[j].Timestamp) })
	for _, e := range events {
		if err := emit(e); err != nil {
			return err
		}
		stats.Emitted++
	}
	return nil
}

func discoverCrushDBs(root string) ([]crushDBRef, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		switch filepath.Base(root) {
		case "crush.db":
			return []crushDBRef{{Path: root, Project: crushProjectForDB(root)}}, nil
		case "projects.json":
			return crushDBsFromProjectsFile(root)
		default:
			return nil, nil
		}
	}

	var out []crushDBRef
	if db := filepath.Join(root, "crush.db"); isRegularFile(db) {
		out = append(out, crushDBRef{Path: db, Project: crushProjectForDB(db)})
	}
	if db := filepath.Join(root, ".crush", "crush.db"); isRegularFile(db) {
		out = append(out, crushDBRef{Path: db, Project: filepath.Clean(root)})
	}
	if registry := filepath.Join(root, "projects.json"); isRegularFile(registry) {
		refs, err := crushDBsFromProjectsFile(registry)
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

func crushDBsFromProjectsFile(path string) ([]crushDBRef, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list crushProjectList
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("crush: parse %s: %w", path, err)
	}
	out := make([]crushDBRef, 0, len(list.Projects))
	for _, project := range list.Projects {
		dataDir := strings.TrimSpace(project.DataDir)
		if dataDir == "" {
			continue
		}
		dataDir = expandHome(dataDir)
		if !filepath.IsAbs(dataDir) && project.Path != "" {
			dataDir = filepath.Join(project.Path, dataDir)
		}
		db := filepath.Join(filepath.Clean(dataDir), "crush.db")
		if isRegularFile(db) {
			out = append(out, crushDBRef{Path: db, Project: filepath.Clean(project.Path)})
		}
	}
	return out, nil
}

func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func crushProjectForDB(dbPath string) string {
	dir := filepath.Dir(dbPath)
	if filepath.Base(dir) == ".crush" {
		return filepath.Dir(dir)
	}
	return dir
}

type crushSessionRow struct {
	ID         string
	Parent     string
	Prompt     uint64
	Completion uint64
	Cost       float64
	Created    time.Time
}

func parseCrushDB(path, project string) ([]Event, error) {
	db, err := openSQLiteReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("crush: %s: %w", path, err)
	}
	defer db.Close()

	exists, err := sqliteTableExists(db, "sessions")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("missing sessions table")
	}
	cols, err := sqliteTableColumns(db, "sessions")
	if err != nil {
		return nil, err
	}
	if !sqliteHasColumns(cols, "id", "prompt_tokens", "completion_tokens", "cost", "created_at") {
		return nil, errors.New("sessions table missing required usage columns")
	}
	parentExpr := `''`
	if cols["parent_session_id"] {
		parentExpr = "parent_session_id"
	}
	rows, err := db.Query(`SELECT id, ` + parentExpr + `, prompt_tokens, completion_tokens, cost, created_at FROM sessions`)
	if err != nil {
		return nil, sqliteQueryError(path, "sessions", err)
	}
	sessions := map[string]crushSessionRow{}
	parents := map[string]string{}
	for rows.Next() {
		var id, parent sql.NullString
		var prompt, completion, created sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&id, &parent, &prompt, &completion, &cost, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if id.String == "" {
			continue
		}
		p := uint64(0)
		if prompt.Valid && prompt.Int64 > 0 {
			p = uint64(prompt.Int64)
		}
		c := uint64(0)
		if completion.Valid && completion.Int64 > 0 {
			c = uint64(completion.Int64)
		}
		createdAt := int64(0)
		if created.Valid {
			createdAt = created.Int64
		}
		sessions[id.String] = crushSessionRow{ID: id.String, Parent: parent.String, Prompt: p, Completion: c, Cost: cost.Float64, Created: crushUnixTime(createdAt)}
		parents[id.String] = parent.String
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Messages persist model/provider but not per-request usage. Associate all
	// assistant models (including child sessions) with their root session. If a
	// root used more than one model, keep the aggregate honest by reporting it as
	// crush:mixed instead of guessing a split that is not present on disk.
	modelsByRoot := map[string]map[string]bool{}
	if exists, err := sqliteTableExists(db, "messages"); err != nil {
		return nil, err
	} else if exists {
		mcols, err := sqliteTableColumns(db, "messages")
		if err != nil {
			return nil, err
		}
		if sqliteHasColumns(mcols, "session_id", "role", "model") {
			mrows, err := db.Query(`SELECT session_id, role, model FROM messages`)
			if err != nil {
				return nil, sqliteQueryError(path, "messages", err)
			}
			for mrows.Next() {
				var sid, role, model sql.NullString
				if err := mrows.Scan(&sid, &role, &model); err != nil {
					mrows.Close()
					return nil, err
				}
				name := strings.TrimSpace(model.String)
				if role.String != "assistant" || sid.String == "" || name == "" {
					continue
				}
				root := crushRootSession(sid.String, parents)
				if modelsByRoot[root] == nil {
					modelsByRoot[root] = map[string]bool{}
				}
				modelsByRoot[root][name] = true
			}
			if err := mrows.Err(); err != nil {
				mrows.Close()
				return nil, err
			}
			mrows.Close()
		}
	}

	var out []Event
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := sessions[id]
		if s.Parent != "" { // parent cost already includes child-session cost
			continue
		}
		model := crushModelLabel(modelsByRoot[id])
		total := saturatingAdd(s.Prompt, s.Completion)
		if total == 0 && s.Cost <= 0 {
			continue
		}
		created := s.Created
		if created.IsZero() {
			created = fileModifiedTime(path)
		}
		out = append(out, Event{
			Harness: "crush", Session: s.ID, Project: project, Model: model, Timestamp: created,
			Input: s.Prompt, Output: s.Completion, Total: total,
			// Crush knows the cumulative session cost, but its persisted prompt
			// counter folds cache reads into input and has no cache bucket. Do not
			// re-price those counters when the stored cost happens to be zero.
			Cost: s.Cost, CostKnown: true,
		})
	}
	return out, nil
}

func crushRootSession(id string, parents map[string]string) string {
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		seen[id] = true
		parent := parents[id]
		if parent == "" {
			return id
		}
		id = parent
	}
	return id
}

func crushModelLabel(models map[string]bool) string {
	if len(models) == 0 {
		return "crush:unknown"
	}
	if len(models) > 1 {
		return "crush:mixed"
	}
	for model := range models {
		return model
	}
	return "crush:unknown"
}

func crushUnixTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	// Current Crush stores Unix seconds. Be tolerant of old/future schemas that
	// use higher-resolution Unix timestamps.
	switch {
	case v >= 100_000_000_000_000_000:
		return time.Unix(0, v).UTC()
	case v >= 100_000_000_000_000:
		return time.UnixMicro(v).UTC()
	case v >= 100_000_000_000:
		return time.UnixMilli(v).UTC()
	default:
		return time.Unix(v, 0).UTC()
	}
}
