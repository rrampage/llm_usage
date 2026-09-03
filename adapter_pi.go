package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type piHarness struct{}

func (piHarness) Name() string { return "pi" }
func (piHarness) DefaultRoots() []string {
	home, _ := os.UserHomeDir()
	if env := os.Getenv("PI_AGENT_DIR"); env != "" {
		var roots []string
		for _, p := range splitPaths(env) {
			// The upstream PI_AGENT_DIR commonly names ~/.pi/agent; ccusage-style
			// overrides may already name the sessions directory. Accept both.
			if filepath.Base(p) == "sessions" {
				roots = append(roots, p)
			} else if st, err := os.Stat(filepath.Join(p, "sessions")); err == nil && st.IsDir() {
				roots = append(roots, filepath.Join(p, "sessions"))
			} else {
				roots = append(roots, p)
			}
		}
		return roots
	}
	return []string{filepath.Join(home, ".pi", "agent", "sessions")}
}

type piLine struct {
	Type          string     `json:"type"`
	Timestamp     string     `json:"timestamp"`
	ParentSession string     `json:"parentSession"`
	ID            string     `json:"id"`
	ParentID      string     `json:"parentId"`
	CWD           string     `json:"cwd"`
	Message       *piMessage `json:"message"`
}

type piMessage struct {
	Role  string   `json:"role"`
	Model string   `json:"model"`
	Usage *piUsage `json:"usage"`
}

type piUsage struct {
	Input       flexUint64 `json:"input"`
	Output      flexUint64 `json:"output"`
	CacheRead   flexUint64 `json:"cacheRead"`
	CacheWrite  flexUint64 `json:"cacheWrite"`
	TotalTokens flexUint64 `json:"totalTokens"`
	Cost        *piCost    `json:"cost"`
}

type piCost struct {
	Total *flexFloat64 `json:"total"`
}

type piUsageRecord struct {
	ID        string
	Signature piSignature
	Event     Event
}

type piSignature struct {
	Timestamp  int64
	Model      string
	Input      uint64
	Output     uint64
	CacheRead  uint64
	CacheWrite uint64
	Total      uint64
	CostBits   uint64
	CostKnown  bool
}

type piSession struct {
	Path          string
	Root          string
	HeaderID      string
	ParentSession string
	ForkTime      time.Time
	Project       string
	Usage         []piUsageRecord
	Links         []piLink
}

type piLink struct {
	ID       string
	ParentID string
}

func (piHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("pi")
	for _, root := range roots {
		files, err := walkJSONL(root, ctx.Since, ctx.Until)
		if err != nil {
			return err
		}
		sessions := make([]*piSession, 0, len(files))
		for _, path := range files {
			stats.Files++
			s, err := readPiSession(ctx, root, path)
			if err != nil {
				return err
			}
			sessions = append(sessions, s)
		}
		skips := piReplaySkips(sessions)
		seen := map[string]struct{}{}
		for _, s := range sessions {
			skip := skips[s.Path]
			for i, u := range s.Usage {
				if i < skip {
					stats.ReplaySkipped++
					continue
				}
				key := fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d\x00%d\x00%d", u.Event.Session, u.Event.Timestamp.UnixNano(), u.Event.Model, u.Event.Input, u.Event.Output, u.Event.CacheRead, u.Event.CacheWrite)
				if _, dup := seen[key]; dup {
					stats.Deduplicated++
					continue
				}
				seen[key] = struct{}{}
				if err := emit(u.Event); err != nil {
					return err
				}
				stats.Emitted++
			}
		}
	}
	return nil
}

func readPiSession(ctx *LoadContext, root, path string) (*piSession, error) {
	stats := ctx.stat("pi")
	s := &piSession{Path: filepath.Clean(path), Root: root, Project: piProject(root, path)}
	sessionID := derivedSession(path)
	err := forEachLine(path, func(line []byte, lineNo int) error {
		stats.Lines++
		var row piLine
		if err := json.Unmarshal(line, &row); err != nil {
			return reportMalformed(ctx, "pi", path, lineNo, err)
		}
		if row.Type == "session" {
			if row.ID != "" {
				s.HeaderID = row.ID
				sessionID = row.ID
			}
			s.ParentSession = row.ParentSession
			if t, ok := parseTimestamp(row.Timestamp); ok {
				s.ForkTime = t
			}
			if row.CWD != "" {
				s.Project = row.CWD
			}
			return nil
		}
		// Every v3 tree entry participates in active-path reconstruction, not
		// only assistant messages.
		s.Links = append(s.Links, piLink{ID: row.ID, ParentID: row.ParentID})
		if row.Message == nil || row.Message.Role != "assistant" || row.Message.Usage == nil {
			return nil
		}
		t, ok := parseTimestamp(row.Timestamp)
		if !ok {
			stats.Skipped++
			return nil
		}
		u := row.Message.Usage
		e := Event{
			Harness: "pi", Session: sessionID, Project: s.Project, Model: row.Message.Model, Timestamp: t,
			Input: uint64(u.Input), Output: uint64(u.Output), CacheRead: uint64(u.CacheRead), CacheWrite: uint64(u.CacheWrite),
			InputIncludesCache: false,
		}
		e.Total = uint64(u.TotalTokens)
		if e.Total == 0 {
			e.Total = saturatingSum(e.Input, e.Output, e.CacheRead, e.CacheWrite)
		}
		if u.Cost != nil && u.Cost.Total != nil {
			e.Cost, e.CostKnown = float64(*u.Cost.Total), true
		}
		if e.Total == 0 {
			return nil
		}
		sig := piSignature{
			Timestamp: t.UnixNano(), Model: e.Model, Input: e.Input, Output: e.Output,
			CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Total: e.Total,
			CostKnown: e.CostKnown,
		}
		if e.CostKnown {
			sig.CostBits = math.Float64bits(e.Cost)
		}
		s.Usage = append(s.Usage, piUsageRecord{ID: row.ID, Signature: sig, Event: e})
		return nil
	})
	return s, err
}

func piProject(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(filepath.Dir(path))
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 1 {
		return parts[0]
	}
	return filepath.Base(root)
}

func piReplaySkips(sessions []*piSession) map[string]int {
	byPath := map[string]*piSession{}
	byBase := map[string][]*piSession{}
	byID := map[string][]*piSession{}
	for _, s := range sessions {
		abs, _ := filepath.Abs(s.Path)
		byPath[filepath.Clean(abs)] = s
		byBase[filepath.Base(s.Path)] = append(byBase[filepath.Base(s.Path)], s)
		if s.HeaderID != "" {
			byID[s.HeaderID] = append(byID[s.HeaderID], s)
		}
	}
	resolve := func(child *piSession) *piSession {
		p := strings.TrimSpace(child.ParentSession)
		if p == "" {
			return nil
		}
		candidates := []string{}
		if filepath.IsAbs(p) {
			candidates = append(candidates, p)
		} else {
			candidates = append(candidates, filepath.Join(filepath.Dir(child.Path), p), filepath.Join(child.Root, p))
		}
		for _, c := range candidates {
			abs, _ := filepath.Abs(c)
			if s := byPath[filepath.Clean(abs)]; s != nil && s != child {
				return s
			}
		}
		if xs := byID[p]; len(xs) == 1 && xs[0] != child {
			return xs[0]
		}
		if xs := byBase[filepath.Base(p)]; len(xs) == 1 && xs[0] != child {
			return xs[0]
		}
		return nil
	}

	out := map[string]int{}
	for _, child := range sessions {
		parent := resolve(child)
		if parent == nil || child.ForkTime.IsZero() {
			continue
		}
		candidate, ok := piActiveUsage(parent)
		if !ok {
			continue
		}
		limit := len(child.Usage)
		if len(candidate) < limit {
			limit = len(candidate)
		}
		n := 0
		for n < limit {
			p := candidate[n]
			if time.Unix(0, p.Signature.Timestamp).After(child.ForkTime) {
				break
			}
			if child.Usage[n].Signature != p.Signature {
				break
			}
			n++
		}
		out[child.Path] = n
	}
	return out
}

func piActiveUsage(s *piSession) ([]piUsageRecord, bool) {
	allUnlinked := true
	for _, l := range s.Links {
		if l.ID != "" || l.ParentID != "" {
			allUnlinked = false
			break
		}
	}
	if allUnlinked {
		return s.Usage, true // v1 physical-order sessions
	}
	parents := map[string]string{}
	lastID := ""
	for _, l := range s.Links {
		if l.ID == "" {
			return nil, false
		}
		if _, dup := parents[l.ID]; dup {
			return nil, false
		}
		parents[l.ID] = l.ParentID
		lastID = l.ID
	}
	for _, p := range parents {
		if p != "" {
			if _, ok := parents[p]; !ok {
				return nil, false
			}
		}
	}
	pathIDs := make([]string, 0, len(parents))
	seen := map[string]struct{}{}
	for id := lastID; id != ""; id = parents[id] {
		if _, cycle := seen[id]; cycle {
			return nil, false
		}
		seen[id] = struct{}{}
		pathIDs = append(pathIDs, id)
	}
	for i, j := 0, len(pathIDs)-1; i < j; i, j = i+1, j-1 {
		pathIDs[i], pathIDs[j] = pathIDs[j], pathIDs[i]
	}
	usageByID := map[string]piUsageRecord{}
	for _, u := range s.Usage {
		if u.ID != "" {
			usageByID[u.ID] = u
		}
	}
	out := make([]piUsageRecord, 0, len(s.Usage))
	for _, id := range pathIDs {
		if u, ok := usageByID[id]; ok {
			out = append(out, u)
		}
	}
	return out, true
}
