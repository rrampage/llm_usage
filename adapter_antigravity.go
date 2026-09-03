package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type antigravityHarness struct{}

func (antigravityHarness) Name() string { return "antigravity" }

func (antigravityHarness) DefaultRoots() []string {
	if env := os.Getenv("ANTIGRAVITY_DATA_DIR"); strings.TrimSpace(env) != "" {
		return splitPaths(env)
	}
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".gemini", "antigravity"),
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(home, ".gemini", "antigravity-ide"),
		filepath.Join(home, ".gemini", "antigravity-backup"),
		filepath.Join(home, ".config", "antigravity"),
	}
}

type antigravityUsage struct {
	ModelID, Input, TotalOutput, CacheWrite, CacheRead, Reasoning, VisibleOutput, Provider uint64
	MessageID, ResponseID, ProviderMessageID                                               string
}

type antigravityGenerator struct {
	Model     string
	ModelID   uint64
	Usage     *antigravityUsage
	Retries   []antigravityUsage
	Timestamp time.Time
}

type antigravityStep struct {
	Model     string
	ModelID   uint64
	Provider  uint64
	Usage     *antigravityUsage
	Retries   []antigravityUsage
	Timestamp time.Time
}

type antigravityEvent struct {
	Event
	Provider      uint64
	Identities    []string
	TimestampRank uint8
	MessageRank   uint8
	MessageID     string
}

func (antigravityHarness) Load(ctx *LoadContext, roots []string, emit func(Event) error) error {
	stats := ctx.stat("antigravity")
	var all []antigravityEvent
	seenPath := map[string]bool{}
	for _, root := range roots {
		conversationRoot := root
		if st, err := os.Stat(filepath.Join(root, "conversations")); err == nil && st.IsDir() {
			conversationRoot = filepath.Join(root, "conversations")
		}
		files, err := walkExtensions(conversationRoot, ctx.Since, ctx.Until, ".db")
		if err != nil {
			return err
		}
		for _, path := range files {
			canonical, err := filepath.EvalSymlinks(path)
			if err != nil {
				canonical = filepath.Clean(path)
			}
			if seenPath[canonical] {
				continue
			}
			seenPath[canonical] = true
			stats.Files++
			events, err := parseAntigravityDB(path)
			if err != nil {
				if ctx.Strict {
					return err
				}
				stats.Malformed++
				if ctx.Verbose {
					fmt.Fprintf(os.Stderr, "antigravity: skip %s: %v\n", path, err)
				}
				continue
			}
			all = append(all, events...)
		}
	}
	all = dedupeAntigravity(all, stats)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Timestamp.Before(all[j].Timestamp) })
	for _, e := range all {
		if err := emit(e.Event); err != nil {
			return err
		}
		stats.Emitted++
	}
	return nil
}

func parseAntigravityDB(path string) ([]antigravityEvent, error) {
	db, err := openSQLiteReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("antigravity: %s: %w", path, err)
	}
	defer db.Close()
	fallback := fileModifiedTime(path)
	session := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	trajectoryTS := time.Time{}

	if exists, err := sqliteTableExists(db, "trajectory_metadata_blob"); err != nil {
		return nil, err
	} else if exists {
		cols, err := sqliteTableColumns(db, "trajectory_metadata_blob")
		if err != nil {
			return nil, err
		}
		if cols["data"] {
			rows, err := db.Query(`SELECT data FROM trajectory_metadata_blob ORDER BY rowid ASC`)
			if err != nil {
				return nil, sqliteQueryError(path, "trajectory_metadata_blob", err)
			}
			for rows.Next() {
				var blob []byte
				if err := rows.Scan(&blob); err != nil {
					rows.Close()
					return nil, err
				}
				if ts, ok := parseAntigravityTrajectoryTimestamp(blob); ok && trajectoryTS.IsZero() {
					trajectoryTS = ts
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}

	exists, err := sqliteTableExists(db, "gen_metadata")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("missing gen_metadata table")
	}
	genCols, err := sqliteTableColumns(db, "gen_metadata")
	if err != nil {
		return nil, err
	}
	if !genCols["data"] {
		return nil, errors.New("gen_metadata table missing data column")
	}
	genRows, err := db.Query(`SELECT rowid, data FROM gen_metadata ORDER BY rowid ASC`)
	if err != nil {
		return nil, sqliteQueryError(path, "gen_metadata", err)
	}
	var generators []antigravityGenerator
	for genRows.Next() {
		var rowid int64
		var blob []byte
		if err := genRows.Scan(&rowid, &blob); err != nil {
			genRows.Close()
			return nil, err
		}
		g, err := parseAntigravityGenerator(blob)
		if err != nil {
			genRows.Close()
			return nil, fmt.Errorf("gen_metadata row %d: %w", rowid, err)
		}
		generators = append(generators, g)
	}
	if err := genRows.Err(); err != nil {
		genRows.Close()
		return nil, err
	}
	genRows.Close()

	var steps []antigravityStep
	if exists, err := sqliteTableExists(db, "steps"); err != nil {
		return nil, err
	} else if exists {
		cols, err := sqliteTableColumns(db, "steps")
		if err != nil {
			return nil, err
		}
		if cols["metadata"] {
			rows, err := db.Query(`SELECT rowid, metadata FROM steps WHERE metadata IS NOT NULL ORDER BY rowid ASC`)
			if err != nil {
				return nil, sqliteQueryError(path, "steps", err)
			}
			for rows.Next() {
				var rowid int64
				var blob []byte
				if err := rows.Scan(&rowid, &blob); err != nil {
					rows.Close()
					return nil, err
				}
				s, err := parseAntigravityStep(blob)
				if err != nil {
					rows.Close()
					return nil, fmt.Errorf("steps row %d: %w", rowid, err)
				}
				steps = append(steps, s)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}

	generationModel := ""
	for i := len(generators) - 1; i >= 0; i-- {
		if m := antigravityModel(generators[i].Model, generators[i].ModelID); m != "" {
			generationModel = m
			break
		}
	}
	var events []antigravityEvent
	identityTS := map[string]rankedTimestamp{}
	for _, step := range steps {
		model := antigravityModel(step.Model, step.ModelID)
		if model == "" {
			model = generationModel
		}
		ctx := antigravityEventContext{Model: model, Provider: step.Provider, Timestamp: step.Timestamp, Trajectory: trajectoryTS, Fallback: fallback, Session: session}
		if step.Usage != nil {
			appendAntigravityEvent(&events, identityTS, *step.Usage, ctx)
		}
		for _, u := range step.Retries {
			appendAntigravityEvent(&events, identityTS, u, ctx)
		}
	}
	currentModel := ""
	for _, g := range generators {
		rowModel := antigravityModel(g.Model, g.ModelID)
		if rowModel == "" && g.Usage != nil {
			rowModel = antigravityModel("", g.Usage.ModelID)
		}
		if rowModel != "" {
			currentModel = rowModel
		}
		ctx := antigravityEventContext{Model: currentModel, Timestamp: g.Timestamp, Trajectory: trajectoryTS, Fallback: fallback, Session: session}
		if g.Usage != nil {
			appendAntigravityEvent(&events, identityTS, *g.Usage, ctx)
		}
		for _, u := range g.Retries {
			appendAntigravityEvent(&events, identityTS, u, ctx)
		}
	}
	return events, nil
}

type rankedTimestamp struct {
	Time time.Time
	Rank uint8
}

type antigravityEventContext struct {
	Model      string
	Provider   uint64
	Timestamp  time.Time
	Trajectory time.Time
	Fallback   time.Time
	Session    string
}

func appendAntigravityEvent(events *[]antigravityEvent, identityTS map[string]rankedTimestamp, u antigravityUsage, c antigravityEventContext) {
	if u.Input == 0 && u.TotalOutput == 0 && u.CacheWrite == 0 && u.CacheRead == 0 && u.Reasoning == 0 && u.VisibleOutput == 0 {
		return
	}
	identities := antigravityIdentities(u)
	ts, rank := c.Timestamp, uint8(3)
	if ts.IsZero() {
		rank = 0
		for _, id := range identities {
			if old, ok := identityTS[id]; ok {
				ts, rank = old.Time, old.Rank
				break
			}
		}
		if ts.IsZero() && !c.Trajectory.IsZero() {
			ts, rank = c.Trajectory, 1
		}
		if ts.IsZero() {
			ts = c.Fallback
		}
	}
	for _, id := range identities {
		old, ok := identityTS[id]
		if !ok || rank > old.Rank || (rank == old.Rank && ts.Before(old.Time)) {
			identityTS[id] = rankedTimestamp{Time: ts, Rank: rank}
		}
	}
	totalOutput := maxU64(u.TotalOutput, saturatingAdd(u.VisibleOutput, u.Reasoning))
	output := maxU64(u.VisibleOutput, saturatingSub(totalOutput, u.Reasoning))
	reasoning := maxU64(u.Reasoning, saturatingSub(totalOutput, output))
	model := antigravityModel(c.Model, u.ModelID)
	if model == "" {
		model = c.Model
	}
	if model == "" {
		model = "gemini-internal-model"
	}
	provider := u.Provider
	if provider == 0 {
		provider = c.Provider
	}
	message, messageRank := antigravityPreferredMessage(u)
	total := saturatingSum(u.Input, u.CacheWrite, u.CacheRead, totalOutput)
	*events = append(*events, antigravityEvent{
		Event: Event{Harness: "antigravity", Session: c.Session, Project: "antigravity", Model: model, Timestamp: ts,
			Input: u.Input, Output: output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Reasoning: reasoning,
			BilledOutput: totalOutput, Total: total},
		Provider: provider, Identities: identities, TimestampRank: rank, MessageRank: messageRank, MessageID: message,
	})
}

func dedupeAntigravity(in []antigravityEvent, stats *ParseStats) []antigravityEvent {
	var slots []*antigravityEvent
	indexes := map[string]int{}
	for _, event := range in {
		matches := map[int]bool{}
		for _, id := range event.Identities {
			if i, ok := indexes[id]; ok {
				matches[i] = true
			}
		}
		if len(matches) == 0 {
			e := event
			i := len(slots)
			slots = append(slots, &e)
			for _, id := range e.Identities {
				indexes[id] = i
			}
			continue
		}
		ids := make([]int, 0, len(matches))
		for i := range matches {
			ids = append(ids, i)
		}
		sort.Ints(ids)
		target := ids[0]
		for _, i := range ids[1:] {
			if slots[i] != nil {
				mergeAntigravity(slots[target], *slots[i])
				slots[i] = nil
				stats.Deduplicated++
			}
		}
		mergeAntigravity(slots[target], event)
		stats.Deduplicated++
		for _, id := range slots[target].Identities {
			indexes[id] = target
		}
	}
	out := make([]antigravityEvent, 0, len(slots))
	for _, e := range slots {
		if e != nil {
			out = append(out, *e)
		}
	}
	return out
}

func mergeAntigravity(a *antigravityEvent, b antigravityEvent) {
	a.Input = maxU64(a.Input, b.Input)
	a.Output = maxU64(a.Output, b.Output)
	a.CacheWrite = maxU64(a.CacheWrite, b.CacheWrite)
	a.CacheRead = maxU64(a.CacheRead, b.CacheRead)
	a.Reasoning = maxU64(a.Reasoning, b.Reasoning)
	a.BilledOutput = maxU64(maxU64(a.BilledOutput, b.BilledOutput), saturatingAdd(a.Output, a.Reasoning))
	a.Total = saturatingSum(a.Input, a.CacheWrite, a.CacheRead, a.BilledOutput)
	if a.Model == "gemini-internal-model" && b.Model != "gemini-internal-model" {
		a.Model = b.Model
	}
	if a.Provider == 0 {
		a.Provider = b.Provider
	}
	if b.TimestampRank > a.TimestampRank || (b.TimestampRank == a.TimestampRank && b.Timestamp.Before(a.Timestamp)) {
		a.Timestamp, a.TimestampRank = b.Timestamp, b.TimestampRank
	}
	if b.MessageRank > a.MessageRank {
		a.MessageID, a.MessageRank = b.MessageID, b.MessageRank
	}
	for _, id := range b.Identities {
		if !containsString(a.Identities, id) {
			a.Identities = append(a.Identities, id)
		}
	}
}

func antigravityIdentities(u antigravityUsage) []string {
	var out []string
	if u.ResponseID != "" {
		out = append(out, "response:"+u.ResponseID)
	}
	if u.ProviderMessageID != "" {
		out = append(out, "provider:"+u.ProviderMessageID)
	}
	if u.MessageID != "" {
		out = append(out, "message:"+u.MessageID)
	}
	return out
}

func antigravityPreferredMessage(u antigravityUsage) (string, uint8) {
	if u.ResponseID != "" {
		return u.ResponseID, 3
	}
	if u.ProviderMessageID != "" {
		return u.ProviderMessageID, 2
	}
	if u.MessageID != "" {
		return u.MessageID, 1
	}
	return "", 0
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func lastBlob(values []any) []byte {
	for i := len(values) - 1; i >= 0; i-- {
		if b, ok := values[i].([]byte); ok {
			return b
		}
	}
	return nil
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// ---- Antigravity protobuf decoding ----

type protoValue struct {
	Wire   uint64
	Varint uint64
	Bytes  []byte
}

type protoField struct {
	Number uint32
	Value  protoValue
}

func decodeProto(blob []byte) ([]protoField, error) {
	var out []protoField
	for len(blob) > 0 {
		tag, n, err := readProtoVarint(blob)
		if err != nil {
			return nil, err
		}
		blob = blob[n:]
		number := uint32(tag >> 3)
		if number == 0 {
			return nil, errors.New("protobuf field number is zero")
		}
		wire := tag & 7
		f := protoField{Number: number, Value: protoValue{Wire: wire}}
		switch wire {
		case 0:
			v, n, err := readProtoVarint(blob)
			if err != nil {
				return nil, err
			}
			blob = blob[n:]
			f.Value.Varint = v
		case 1:
			if len(blob) < 8 {
				return nil, errors.New("truncated protobuf fixed64")
			}
			blob = blob[8:]
		case 2:
			ln, n, err := readProtoVarint(blob)
			if err != nil {
				return nil, err
			}
			blob = blob[n:]
			if ln > uint64(len(blob)) {
				return nil, errors.New("truncated protobuf bytes")
			}
			f.Value.Bytes = blob[:int(ln)]
			blob = blob[int(ln):]
		case 5:
			if len(blob) < 4 {
				return nil, errors.New("truncated protobuf fixed32")
			}
			blob = blob[4:]
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
		out = append(out, f)
	}
	return out, nil
}

func readProtoVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		if i >= len(b) {
			return 0, 0, errors.New("truncated protobuf varint")
		}
		c := b[i]
		if i == 9 && c > 1 {
			return 0, 0, errors.New("protobuf varint overflow")
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errors.New("protobuf varint overflow")
}

func protoVarint(fields []protoField, number uint32) uint64 {
	for i := len(fields) - 1; i >= 0; i-- {
		if fields[i].Number == number && fields[i].Value.Wire == 0 {
			return fields[i].Value.Varint
		}
	}
	return 0
}

func protoBytes(fields []protoField, number uint32) []byte {
	for _, f := range fields {
		if f.Number == number && f.Value.Wire == 2 {
			return f.Value.Bytes
		}
	}
	return nil
}

func protoBytesAll(fields []protoField, number uint32) [][]byte {
	var out [][]byte
	for _, f := range fields {
		if f.Number == number && f.Value.Wire == 2 {
			out = append(out, f.Value.Bytes)
		}
	}
	return out
}

func protoText(fields []protoField, number uint32) string {
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		if f.Number == number && f.Value.Wire == 2 && strings.TrimSpace(string(f.Value.Bytes)) != "" {
			return string(f.Value.Bytes)
		}
	}
	return ""
}

func parseAntigravityGenerator(blob []byte) (antigravityGenerator, error) {
	root, err := decodeProto(blob)
	if err != nil {
		return antigravityGenerator{}, err
	}
	chatBlob := protoBytes(root, 1)
	if chatBlob == nil {
		return antigravityGenerator{}, errors.New("missing chat model field 1")
	}
	chat, err := decodeProto(chatBlob)
	if err != nil {
		return antigravityGenerator{}, err
	}
	var g antigravityGenerator
	g.ModelID = protoVarint(chat, 3)
	g.Model = protoText(chat, 19)
	if g.Model == "" {
		g.Model = protoText(chat, 21)
	}
	if b := protoBytes(chat, 4); b != nil {
		u, err := parseAntigravityUsage(b)
		if err != nil {
			return g, err
		}
		g.Usage = &u
	}
	for _, b := range protoBytesAll(chat, 17) {
		u, ok, err := parseAntigravityRetry(b)
		if err != nil {
			return g, err
		}
		if ok {
			g.Retries = append(g.Retries, u)
		}
	}
	if b := protoBytes(chat, 9); b != nil {
		fields, err := decodeProto(b)
		if err != nil {
			return g, err
		}
		if tb := protoBytes(fields, 4); tb != nil {
			g.Timestamp, _ = parseAntigravityTimestamp(tb)
		}
	}
	return g, nil
}

func parseAntigravityStep(blob []byte) (antigravityStep, error) {
	fields, err := decodeProto(blob)
	if err != nil {
		return antigravityStep{}, err
	}
	var s antigravityStep
	if b := protoBytes(fields, 9); b != nil {
		u, err := parseAntigravityUsage(b)
		if err != nil {
			return s, err
		}
		s.Usage = &u
	}
	for _, b := range protoBytesAll(fields, 28) {
		u, ok, err := parseAntigravityRetry(b)
		if err != nil {
			return s, err
		}
		if ok {
			s.Retries = append(s.Retries, u)
		}
	}
	if b := protoBytes(fields, 24); b != nil {
		m, err := decodeProto(b)
		if err != nil {
			return s, err
		}
		s.ModelID = protoVarint(m, 1)
		s.Provider = protoVarint(m, 7)
		s.Model = protoText(m, 12)
		if s.Model == "" {
			s.Model = protoText(m, 8)
		}
	}
	if b := protoBytes(fields, 8); b != nil {
		s.Timestamp, _ = parseAntigravityTimestamp(b)
	} else if b := protoBytes(fields, 1); b != nil {
		s.Timestamp, _ = parseAntigravityTimestamp(b)
	}
	return s, nil
}

func parseAntigravityRetry(blob []byte) (antigravityUsage, bool, error) {
	fields, err := decodeProto(blob)
	if err != nil {
		return antigravityUsage{}, false, err
	}
	b := protoBytes(fields, 2)
	if b == nil {
		return antigravityUsage{}, false, nil
	}
	u, err := parseAntigravityUsage(b)
	return u, err == nil, err
}

func parseAntigravityUsage(blob []byte) (antigravityUsage, error) {
	f, err := decodeProto(blob)
	if err != nil {
		return antigravityUsage{}, err
	}
	return antigravityUsage{
		ModelID: protoVarint(f, 1), Input: protoVarint(f, 2), TotalOutput: protoVarint(f, 3),
		CacheWrite: protoVarint(f, 4), CacheRead: protoVarint(f, 5), Provider: protoVarint(f, 6),
		MessageID: protoText(f, 7), Reasoning: protoVarint(f, 9), VisibleOutput: protoVarint(f, 10),
		ResponseID: protoText(f, 11), ProviderMessageID: protoText(f, 12),
	}, nil
}

func parseAntigravityTimestamp(blob []byte) (time.Time, bool) {
	f, err := decodeProto(blob)
	if err != nil {
		return time.Time{}, false
	}
	sec := protoVarint(f, 1)
	if sec == 0 || sec > math.MaxInt64 {
		return time.Time{}, false
	}
	nanos := protoVarint(f, 2)
	if nanos > 999999999 {
		nanos = 999999999
	}
	return time.Unix(int64(sec), int64(nanos)).UTC(), true
}

func parseAntigravityTrajectoryTimestamp(blob []byte) (time.Time, bool) {
	f, err := decodeProto(blob)
	if err != nil {
		return time.Time{}, false
	}
	b := protoBytes(f, 2)
	if b == nil {
		return time.Time{}, false
	}
	return parseAntigravityTimestamp(b)
}

func antigravityModel(raw string, id uint64) string {
	recorded := normalizeAntigravityModel(raw)
	if id == 0 {
		return recorded
	}

	resolved := normalizeAntigravityModel(antigravityModelNameFromID(id))
	if !isAntigravityFallbackModel(resolved) {
		return resolved
	}
	if recorded != "" && !isAntigravityFallbackModel(recorded) {
		return recorded
	}
	return resolved
}

func isAntigravityFallbackModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(lower, "model_placeholder_") || strings.HasPrefix(lower, "antigravity-model-")
}

func antigravityModelNameFromID(id uint64) string {
	switch id {
	case 246:
		return "gemini-2.5-pro"
	case 312:
		return "gemini-2.5-flash"
	case 313, 329:
		return "gemini-2.5-flash-thinking"
	case 330:
		return "gemini-2.5-flash-lite"
	case 281, 282:
		return "claude-4-sonnet"
	case 290, 291:
		return "claude-4-opus"
	case 333, 334:
		return "claude-4.5-sonnet"
	case 340, 341:
		return "claude-4.5-haiku"
	case 342:
		return "model_openai_gpt_oss_120b_medium"
	default:
		if id >= 1000 {
			return fmt.Sprintf("model_placeholder_m%d", id-1000)
		}
		return fmt.Sprintf("antigravity-model-%d", id)
	}
}

func normalizeAntigravityModel(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	base := lower
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	aliases := map[string]string{
		"gemini 3.7 flash": "gemini-3.7-flash", "gemini 3.7 flash thinking": "gemini-3.7-flash",
		"gemini 3.7 pro": "gemini-3.7-pro", "gemini 3.7 pro thinking": "gemini-3.7-pro",
		"gemini 3.6 flash": "gemini-3.6-flash", "gemini 3 flash": "gemini-3.6-flash",
		"gemini 3.6 pro": "gemini-3.6-pro", "gemini 3 pro": "gemini-3-pro", "gemini 3 pro thinking": "gemini-3-pro",
		"gemini 2.5 flash": "gemini-2.5-flash", "gemini 2.5 pro": "gemini-2.5-pro",
		"gemini 2.0 flash": "gemini-2.0-flash", "gemini 2 flash": "gemini-2.0-flash", "gemini 2.0 pro": "gemini-2.0-pro",
		"gemini 1.5 flash": "gemini-1.5-flash", "gemini 1.5 pro": "gemini-1.5-pro",
		"model_placeholder_m26": "claude-opus-4-6", "model_placeholder_m35": "claude-sonnet-4-6",
		"model_placeholder_m36": "gemini-3.1-pro", "model_placeholder_m37": "gemini-3.1-pro", "model_placeholder_m16": "gemini-3.1-pro",
		"model_placeholder_m18": "gemini-3-flash-preview", "model_placeholder_m84": "gemini-3-flash-preview", "model_placeholder_m47": "gemini-3-flash-preview",
		"model_placeholder_m132": "gemini-3.5-flash-high", "model_placeholder_m133": "gemini-3.5-flash-high",
		"model_placeholder_m187": "gemini-3.5-flash-extra-low", "model_placeholder_m20": "gemini-3.5-flash-medium",
		"model_placeholder_m318":           "gemini-3.8-flash",
		"model_placeholder_m319":           "gemini-3.8-flash",
		"model_openai_gpt_oss_120b_medium": "gpt-oss-120b-medium", "gemini-pro-default": "gemini-3.1-pro", "gemini-pro-agent": "gemini-3.1-pro",
		"gemini-3-flash-agent": "gemini-3.5-flash-high", "gemini-3-flash-agent-a": "gemini-3.5-flash-high", "gemini-3-flash-agent-b": "gemini-3.5-flash-high",
		"gemini-3-flash-a": "gemini-3.5-flash-high", "gemini-3-flash-b": "gemini-3.5-flash-high",
		"gemini-3-flash-c": "gemini-3-flash-preview", "gemini-3-flash": "gemini-3-flash-preview",
		"gemini-3.5-flash-low": "gemini-3.5-flash-medium", "gemini-3.1-pro-high": "gemini-3.1-pro", "gemini-3.1-pro-low": "gemini-3.1-pro",
		"gemini-3-pro-high": "gemini-3-pro", "gemini-3-pro-low": "gemini-3-pro",
		"claude 3.7 sonnet": "claude-3-7-sonnet", "claude 3.7 sonnet thinking": "claude-3-7-sonnet",
		"claude 3.5 sonnet": "claude-3-5-sonnet", "claude 3.5 haiku": "claude-3-5-haiku", "claude 3 opus": "claude-3-opus",
	}
	if v := aliases[base]; v != "" {
		return v
	}
	converted := strings.ReplaceAll(base, " ", "-")
	if strings.HasPrefix(converted, "gemini-") || strings.HasPrefix(converted, "claude-") || strings.HasPrefix(converted, "gpt-") {
		return converted
	}
	return trimmed
}
