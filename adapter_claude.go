package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type claudeHarness struct{}

func (claudeHarness) Name() string { return "claude" }

func (claudeHarness) DefaultRoots() []string {
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		var roots []string
		for _, p := range splitPaths(env) {
			if filepath.Base(p) == "projects" {
				roots = append(roots, p)
			} else {
				roots = append(roots, filepath.Join(p, "projects"))
			}
		}
		return roots
	}
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".config", "claude", "projects"),
		filepath.Join(home, ".claude", "projects"),
	}
}

type claudeLine struct {
	Timestamp   string         `json:"timestamp"`
	SessionID   string         `json:"sessionId"`
	RequestID   string         `json:"requestId"`
	CWD         string         `json:"cwd"`
	IsSidechain bool           `json:"isSidechain"`
	CostUSD     *flexFloat64   `json:"costUSD"`
	Message     *claudeMessage `json:"message"`
}

type claudeMessage struct {
	ID    string       `json:"id"`
	Model string       `json:"model"`
	Usage *claudeUsage `json:"usage"`
}

type claudeUsage struct {
	Input       flexUint64        `json:"input_tokens"`
	Output      flexUint64        `json:"output_tokens"`
	CacheCreate flexUint64        `json:"cache_creation_input_tokens"`
	CacheRead   flexUint64        `json:"cache_read_input_tokens"`
	Iterations  []claudeIteration `json:"iterations"`
}

type claudeIteration struct {
	Type        string     `json:"type"`
	Model       string     `json:"model"`
	Input       flexUint64 `json:"input_tokens"`
	Output      flexUint64 `json:"output_tokens"`
	CacheCreate flexUint64 `json:"cache_creation_input_tokens"`
	CacheRead   flexUint64 `json:"cache_read_input_tokens"`
}

type claudeCandidate struct {
	event     Event
	messageID string
	requestID string
	sidechain bool
	timestamp time.Time
}

func (claudeHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("claude")
	// Claude Code writes multiple assistant transcript records for one API
	// response (stream/content-block snapshots). Keep one logical request and
	// replace it when a later candidate has a larger usage total. This mirrors
	// the important behavior of current ccusage rather than naively summing
	// every usage-bearing JSONL line.
	var deduped []claudeCandidate
	exactIndexes := map[string][]int{}
	messageIndexes := map[string][]int{}

	push := func(c claudeCandidate) {
		if c.messageID == "" {
			deduped = append(deduped, c)
			return
		}
		exact := claudeLogicalKey(c.event.Session, c.messageID, c.requestID)
		var existing = -1
		for _, i := range exactIndexes[exact] {
			e := deduped[i]
			if e.messageID == c.messageID && e.requestID == c.requestID && e.event.Session == c.event.Session {
				existing = i
				break
			}
		}
		if existing < 0 {
			// /btw sidechain logs can replay a parent message using a new
			// requestId. Current ccusage only applies this weaker match when
			// timestamps agree and at least one copy is marked sidechain.
			msgKey := c.event.Session + "\x00" + c.messageID
			for _, i := range messageIndexes[msgKey] {
				e := deduped[i]
				if e.timestamp.Equal(c.timestamp) && (c.sidechain || e.sidechain) {
					existing = i
					break
				}
			}
		}
		if existing >= 0 {
			stats.Deduplicated++
			if c.sidechain || deduped[existing].sidechain {
				stats.ReplaySkipped++
			}
			if shouldReplaceClaude(c, deduped[existing]) {
				deduped[existing] = c
				exactIndexes[exact] = appendUniqueInt(exactIndexes[exact], existing)
				msgKey := c.event.Session + "\x00" + c.messageID
				messageIndexes[msgKey] = appendUniqueInt(messageIndexes[msgKey], existing)
			}
			return
		}
		idx := len(deduped)
		deduped = append(deduped, c)
		exactIndexes[exact] = append(exactIndexes[exact], idx)
		msgKey := c.event.Session + "\x00" + c.messageID
		messageIndexes[msgKey] = append(messageIndexes[msgKey], idx)
	}

	for _, root := range roots {
		files, err := walkJSONL(root, ctx.Since, ctx.Until)
		if err != nil {
			return err
		}
		for _, path := range files {
			stats.Files++
			projectFromPath := claudeProject(root, path)
			err := forEachLine(path, func(line []byte, lineNo int) error {
				stats.Lines++
				if !bytes.Contains(line, []byte(`"usage"`)) || !bytes.Contains(line, []byte(`"message"`)) {
					return nil
				}
				var row claudeLine
				if err := json.Unmarshal(line, &row); err != nil {
					return reportMalformed(ctx, "claude", path, lineNo, err)
				}
				if row.Message == nil || row.Message.Usage == nil {
					stats.Skipped++
					return nil
				}
				t, ok := parseTimestamp(row.Timestamp)
				if !ok {
					stats.Skipped++
					return nil
				}
				session := row.SessionID
				if session == "" {
					session = claudeSession(root, path)
				}
				project := projectFromPath
				if row.CWD != "" {
					project = row.CWD
				}
				u := row.Message.Usage
				e := Event{
					Harness: "claude", Session: session, Project: project,
					Model: row.Message.Model, Timestamp: t,
					Input: uint64(u.Input), Output: uint64(u.Output),
					CacheRead: uint64(u.CacheRead), CacheWrite: uint64(u.CacheCreate),
					InputIncludesCache: false,
				}
				e.Total = saturatingSum(e.Input, e.Output, e.CacheRead, e.CacheWrite)
				if row.CostUSD != nil {
					e.Cost, e.CostKnown = float64(*row.CostUSD), true
				}
				if e.Total > 0 {
					push(claudeCandidate{event: e, messageID: row.Message.ID, requestID: row.RequestID, sidechain: row.IsSidechain, timestamp: t})
				}

				// Advisor iterations are separate billable model calls. Give each
				// iteration a synthetic message ID so streaming copies dedupe too.
				for i, it := range u.Iterations {
					if it.Type != "advisor_message" || it.Model == "" {
						continue
					}
					a := Event{
						Harness: "claude", Session: session, Project: project, Model: it.Model,
						Timestamp: t, Input: uint64(it.Input), Output: uint64(it.Output),
						CacheRead: uint64(it.CacheRead), CacheWrite: uint64(it.CacheCreate),
						InputIncludesCache: false,
					}
					a.Total = saturatingSum(a.Input, a.Output, a.CacheRead, a.CacheWrite)
					if a.Total == 0 {
						continue
					}
					msgID := row.Message.ID
					if msgID != "" {
						msgID = fmt.Sprintf("%s:advisor:%d", msgID, i)
					}
					push(claudeCandidate{event: a, messageID: msgID, requestID: row.RequestID, sidechain: row.IsSidechain, timestamp: t})
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	for _, c := range deduped {
		if err := emit(c.event); err != nil {
			return err
		}
		stats.Emitted++
	}
	return nil
}

func claudeLogicalKey(session, messageID, requestID string) string {
	return session + "\x00" + messageID + "\x00" + requestID
}

func shouldReplaceClaude(candidate, existing claudeCandidate) bool {
	if candidate.sidechain != existing.sidechain {
		return existing.sidechain // prefer the non-sidechain parent copy
	}
	if candidate.event.Total != existing.event.Total {
		return candidate.event.Total > existing.event.Total
	}
	return false
}

func appendUniqueInt(xs []int, v int) []int {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func claudeProject(root, path string) string {
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

func claudeSession(root, path string) string {
	rel, _ := filepath.Rel(root, path)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 { // project/session/file.jsonl
		return parts[1]
	}
	return derivedSession(path)
}

func claudeExactKey(session, msgID, reqID, ts string, e Event) string {
	if msgID == "" {
		msgID = "<no-message-id>"
	}
	if reqID == "" {
		return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d", session, msgID, ts, e.Input, e.Output, e.CacheRead, e.CacheWrite)
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d", session, msgID, reqID, e.Input, e.Output, e.CacheRead, e.CacheWrite)
}

func claudeReplayKey(session, msgID string, e Event) string {
	if msgID == "" {
		// Without an ID there is no safe cross-file sidechain replay identity.
		return fmt.Sprintf("noid\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d", session, e.Timestamp.UnixNano(), e.Input, e.Output, e.CacheRead, e.CacheWrite)
	}
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d\x00%d", session, msgID, e.Input, e.Output, e.CacheRead, e.CacheWrite)
}
