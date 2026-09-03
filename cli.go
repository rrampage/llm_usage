package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Reporting commands are deliberately offline. Network access is reachable
	// only through this explicit subcommand.
	if len(args) > 0 && strings.EqualFold(args[0], "pricing") {
		return runPricingCommand(args[1:])
	}

	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			printUsage()
			return nil
		}
	}
	harnessName, report, rest, err := parseCommand(args)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("llm_usage", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	allTime := fs.Bool("all", false, "include all historical usage (disable default 3-month cutoff for daily)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	sinceText := fs.String("since", "", "inclusive local date, YYYY-MM-DD (defaults to last 3 months for daily reports)")
	untilText := fs.String("until", "", "inclusive local date, YYYY-MM-DD")
	tzText := fs.String("timezone", "Local", "Local, UTC, or IANA timezone")
	pricingPath := fs.String("pricing", "", "override local pricing JSON (default: XDG data directory)")
	strict := fs.Bool("strict", false, "fail on malformed relevant JSONL records")
	verbose := fs.Bool("verbose", false, "print parser diagnostics")
	noCost := fs.Bool("no-cost", false, "do not calculate or print cost")
	showVersion := fs.Bool("version", false, "print version")
	claudePath := fs.String("claude-path", "", "comma-separated Claude projects roots")
	codexPath := fs.String("codex-path", "", "comma-separated Codex homes or JSONL roots")
	piPath := fs.String("pi-path", "", "comma-separated pi sessions roots")
	geminiPath := fs.String("gemini-path", "", "comma-separated Gemini CLI data roots")
	antigravityPath := fs.String("antigravity-path", "", "comma-separated Antigravity data roots")
	opencodePath := fs.String("opencode-path", "", "comma-separated OpenCode data roots")
	crushPath := fs.String("crush-path", "", "comma-separated Crush data roots, project roots, DBs, or projects.json")
	var genericPaths pathFlag
	fs.Var(&genericPaths, "path", "repeatable harness=path override; works for future adapters too")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	loc, err := loadLocation(*tzText)
	if err != nil {
		return err
	}
	since, until, err := determineDateBounds(*sinceText, *untilText, *allTime, report, loc, time.Now().In(loc))
	if err != nil {
		return err
	}

	var prices PriceBook
	effectivePricingPath := strings.TrimSpace(*pricingPath)
	pricingWasExplicit := effectivePricingPath != ""
	if effectivePricingPath == "" {
		effectivePricingPath, err = defaultPricingPath()
		if err != nil {
			return err
		}
	}
	if effectivePricingPath != "" {
		prices, err = loadPriceBook(effectivePricingPath)
		if err != nil {
			if pricingWasExplicit || !errors.Is(err, os.ErrNotExist) {
				return err
			}
			prices = nil // First run: source-recorded costs still work offline.
		}
	}
	if *verbose && effectivePricingPath != "" {
		if len(prices) == 0 {
			fmt.Fprintf(os.Stderr, "pricing: no local price book at %s (run `llm_usage pricing update` to create it)\n", effectivePricingPath)
		} else {
			fmt.Fprintf(os.Stderr, "pricing: loaded %d local model prices from %s\n", len(prices), effectivePricingPath)
		}
	}

	adapters := allHarnesses()
	selected := make([]Harness, 0, len(adapters))
	for _, h := range adapters {
		if harnessName == "all" || harnessName == h.Name() {
			selected = append(selected, h)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("unknown harness %q", harnessName)
	}

	pathOverrides := map[string][]string{
		"claude":      splitPaths(*claudePath),
		"codex":       splitPaths(*codexPath),
		"pi":          splitPaths(*piPath),
		"gemini":      splitPaths(*geminiPath),
		"antigravity": splitPaths(*antigravityPath),
		"opencode":    splitPaths(*opencodePath),
		"crush":       splitPaths(*crushPath),
	}
	for name, paths := range genericPaths {
		pathOverrides[name] = uniqueStrings(append(pathOverrides[name], paths...))
	}
	ctx := &LoadContext{
		Strict:  *strict,
		Verbose: *verbose,
		Report:  report,
		Since:   since,
		Until:   until,
		Stats:   map[string]*ParseStats{},
	}
	// Every report is model-oriented. The report dimension (day, week, month,
	// session, or all-time model scope) is the first grouping key and model is
	// the second. Harness identity is deliberately not part of reporting: it is
	// only an adapter concern used while parsing and deduplicating source logs.
	groupByModel := true
	groupByHarness := false
	agg := newAggregator(report, groupByModel, groupByHarness, harnessName, loc, prices, *noCost)

	for _, h := range selected {
		roots := pathOverrides[h.Name()]
		if len(roots) == 0 {
			roots = h.DefaultRoots()
		}
		roots = existingRoots(roots)
		if len(roots) == 0 {
			continue
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "%s roots: %s\n", h.Name(), strings.Join(roots, ", "))
		}
		if err := h.Load(ctx, roots, func(e Event) error {
			if !since.IsZero() && e.Timestamp.In(loc).Before(since) {
				return nil
			}
			if !until.IsZero() && e.Timestamp.In(loc).After(until) {
				return nil
			}
			agg.Add(e)
			return nil
		}); err != nil {
			return fmt.Errorf("%s: %w", h.Name(), err)
		}
	}

	if *jsonOut {
		return agg.PrintJSON(ctx.Stats)
	}
	agg.PrintTable(*noCost)
	if *verbose {
		printStats(ctx.Stats)
	}
	return nil
}

func printUsage() {
	fmt.Fprint(os.Stdout, `llm_usage - offline local usage reporter

Usage:
  llm_usage [all|claude|codex|pi|gemini|antigravity|opencode|crush] [daily|weekly|monthly|session|model] [options]
  llm_usage pricing path
  llm_usage pricing status
  llm_usage pricing update

Examples:
  llm_usage                   # default: daily report for last 3 months
  llm_usage --all             # daily report across all history
  llm_usage codex daily --json
  llm_usage claude monthly --since 2026-09-01
  llm_usage pi session --pi-path ~/.pi/agent/sessions
  llm_usage gemini daily
  llm_usage antigravity monthly
  llm_usage opencode daily
  llm_usage crush monthly
  llm_usage daily --path codex=/tmp/codex-logs --path pi=/tmp/pi-sessions
  llm_usage daily             # date x model rows, then TOTAL (last 3 months by default)
  llm_usage weekly            # week x model rows, then TOTAL
  llm_usage monthly           # month x model rows, then TOTAL
  llm_usage session           # session x model rows, then TOTAL
  llm_usage pricing update       # the only command that uses the network

Important options:
  --all                  include all historical usage (disable default 3-month cutoff for daily)
  --json                 JSON output
  --since YYYY-MM-DD     inclusive date filter (defaults to last 3 months for daily)
  --until YYYY-MM-DD     inclusive date filter
  --timezone ZONE        Local, UTC, or IANA name
  --pricing FILE         override the local price book; reporting remains offline
  --no-cost              token accounting only
  --path agent=DIR       repeatable generic path override
  --strict               fail on malformed relevant JSONL
  --verbose              parser/dedup diagnostics

Offline/pricing:
  Reporting never contacts the network. The default price book is read from
  $LLM_USAGE_PRICING_FILE when set, otherwise
  $XDG_DATA_HOME/llm-usage/pricing.json (fallback: ~/.local/share/llm-usage/pricing.json).
  pricing update explicitly downloads models.dev pricing and atomically replaces
  that local file. pricing path and pricing status are offline.

Adding a harness:
  implement the Harness interface and register it in allHarnesses() in adapters.go.
  The generic --path agent=DIR override requires no new CLI flag.
`)
}

type pathFlag map[string][]string

func (p *pathFlag) String() string {
	if p == nil || len(*p) == 0 {
		return ""
	}
	var xs []string
	for name, paths := range *p {
		for _, path := range paths {
			xs = append(xs, name+"="+path)
		}
	}
	sort.Strings(xs)
	return strings.Join(xs, ",")
}

func (p *pathFlag) Set(s string) error {
	name, raw, ok := strings.Cut(s, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.TrimSpace(raw) == "" {
		return fmt.Errorf("--path must be harness=directory")
	}
	if *p == nil {
		*p = pathFlag{}
	}
	(*p)[name] = append((*p)[name], splitPaths(raw)...)
	return nil
}

func parseCommand(args []string) (harness, report string, rest []string, err error) {
	harness, report = "all", "daily"
	knownHarness := map[string]bool{"all": true, "claude": true, "codex": true, "pi": true, "gemini": true, "antigravity": true, "opencode": true, "crush": true}
	knownReport := map[string]bool{"daily": true, "weekly": true, "monthly": true, "session": true, "model": true}
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		x := strings.ToLower(args[0])
		switch {
		case knownHarness[x] && harness == "all":
			harness = x
		case knownReport[x] && report == "daily":
			report = x
		default:
			return "", "", nil, fmt.Errorf("unknown command %q", args[0])
		}
		args = args[1:]
	}
	return harness, report, args, nil
}

func loadLocation(s string) (*time.Location, error) {
	switch strings.ToLower(s) {
	case "", "local":
		return time.Local, nil
	case "utc":
		return time.UTC, nil
	default:
		loc, err := time.LoadLocation(s)
		if err != nil {
			return nil, fmt.Errorf("invalid timezone %q: %w", s, err)
		}
		return loc, nil
	}
}

func parseDateBound(s string, loc *time.Location) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation("2006-01-02", s, loc)
}

func determineDateBounds(sinceText, untilText string, allTime bool, report string, loc *time.Location, now time.Time) (time.Time, time.Time, error) {
	until, err := parseDateBound(untilText, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--until: %w", err)
	}
	if !until.IsZero() {
		until = until.Add(24*time.Hour - time.Nanosecond)
	}

	var since time.Time
	if sinceText != "" {
		since, err = parseDateBound(sinceText, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--since: %w", err)
		}
	} else if !allTime && report == "daily" {
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		defaultSince := today.AddDate(0, -3, 0)
		if until.IsZero() || !defaultSince.After(until) {
			since = defaultSince
		}
	}

	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return time.Time{}, time.Time{}, errors.New("--since is after --until")
	}
	return since, until, nil
}

func splitPaths(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = expandHome(strings.TrimSpace(p))
		if p != "" {
			out = append(out, filepath.Clean(p))
		}
	}
	return uniqueStrings(out)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return h
			}
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func existingRoots(paths []string) []string {
	out := paths[:0]
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return uniqueStrings(out)
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func parseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05Z07:00",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func derivedSession(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if strings.HasPrefix(base, "rollout-") {
		parts := strings.Split(base, "-")
		if len(parts) >= 7 {
			// Codex rollout names end in a UUID. Keep the full basename if the
			// format is unfamiliar rather than guessing a truncated identifier.
			if i := strings.Index(base, "-019"); i >= 0 {
				return base[i+1:]
			}
		}
	}
	return base
}

func walkJSONL(root string, since, until time.Time) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if (!since.IsZero() || !until.IsZero()) && shouldSkipDateDir(root, path, since, until) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".jsonl") {
			if !since.IsZero() {
				info, err := d.Info()
				if err == nil && info.ModTime().Before(since) {
					return nil
				}
			}
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func forEachLine(path string, fn func(line []byte, lineNo int) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 128*1024)
	for n := 1; ; n++ {
		line, readErr := r.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			if err := fn(line, n); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func reportMalformed(ctx *LoadContext, harness, path string, lineNo int, err error) error {
	s := ctx.stat(harness)
	s.Malformed++
	if ctx.Strict {
		return fmt.Errorf("%s:%d: malformed JSON: %w", path, lineNo, err)
	}
	if ctx.Verbose && s.Malformed <= 8 {
		fmt.Fprintf(os.Stderr, "%s: skip malformed %s:%d: %v\n", harness, path, lineNo, err)
	}
	return nil
}
