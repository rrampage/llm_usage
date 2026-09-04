package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type Aggregate struct {
	Harness           string    `json:"harness"`
	Group             string    `json:"group"`
	Project           string    `json:"project,omitempty"`
	Model             string    `json:"model,omitempty"`
	Input             uint64    `json:"input_tokens"`
	Output            uint64    `json:"output_tokens"`
	CacheRead         uint64    `json:"cache_read_tokens"`
	CacheWrite        uint64    `json:"cache_write_tokens"`
	Reasoning         uint64    `json:"reasoning_output_tokens"`
	Total             uint64    `json:"total_tokens"`
	CostUSD           float64   `json:"cost_usd"`
	CostComplete      bool      `json:"cost_complete"`
	UnknownCostEvents int       `json:"unknown_cost_events"`
	Events            int       `json:"events"`
	FirstActivity     time.Time `json:"first_activity"`
	LastActivity      time.Time `json:"last_activity"`
}

type Aggregator struct {
	report       string
	byModel      bool
	harnessScope string
	loc          *time.Location
	prices       PriceBook
	noCost       bool
	groups       map[string]*Aggregate
	total        Aggregate
}

func newAggregator(report string, byModel bool, harnessScope string, loc *time.Location, prices PriceBook, noCost bool) *Aggregator {
	return &Aggregator{
		report: report, byModel: byModel, harnessScope: harnessScope,
		loc: loc, prices: prices, noCost: noCost, groups: map[string]*Aggregate{},
		total: Aggregate{Harness: "all", Group: "TOTAL", CostComplete: true},
	}
}

func (a *Aggregator) Add(e Event) {
	group := a.groupName(e)
	model := ""
	if a.byModel || a.report == "model" {
		model = e.Model
		if model == "" {
			model = "(unknown)"
		}
	}
	harness := a.harnessScope
	key := harness + "\x00" + group + "\x00" + model
	g := a.groups[key]
	if g == nil {
		g = &Aggregate{Harness: harness, Group: group, Model: model, CostComplete: true}
		a.groups[key] = g
	}
	if g.Project == "" {
		g.Project = e.Project
	} else if g.Project != e.Project && e.Project != "" {
		g.Project = "(multiple)"
	}
	a.addTo(g, e)
	a.addTo(&a.total, e)
}

func (a *Aggregator) groupName(e Event) string {
	t := e.Timestamp.In(a.loc)
	switch a.report {
	case "daily":
		return t.Format("2006-01-02")
	case "weekly":
		// Monday-based ISO-style week label by its start date.
		wd := (int(t.Weekday()) + 6) % 7
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, a.loc).AddDate(0, 0, -wd)
		return start.Format("2006-01-02")
	case "monthly":
		return t.Format("2006-01")
	case "session":
		if e.Session != "" {
			return e.Session
		}
		return "(unknown)"
	case "model":
		return "all"
	default:
		return t.Format("2006-01-02")
	}
}

func (a *Aggregator) addTo(g *Aggregate, e Event) {
	g.Input = saturatingAdd(g.Input, e.Input)
	g.Output = saturatingAdd(g.Output, e.Output)
	g.CacheRead = saturatingAdd(g.CacheRead, e.CacheRead)
	g.CacheWrite = saturatingAdd(g.CacheWrite, e.CacheWrite)
	g.Reasoning = saturatingAdd(g.Reasoning, e.Reasoning)
	g.Total = saturatingAdd(g.Total, e.Total)
	g.Events++
	if g.FirstActivity.IsZero() || e.Timestamp.Before(g.FirstActivity) {
		g.FirstActivity = e.Timestamp
	}
	if g.LastActivity.IsZero() || e.Timestamp.After(g.LastActivity) {
		g.LastActivity = e.Timestamp
	}
	if a.noCost {
		return
	}
	if c, ok := calculateCost(e, a.prices); ok {
		g.CostUSD += c
	} else {
		g.CostComplete = false
		g.UnknownCostEvents++
	}
}

func (a *Aggregator) sortedGroups() []*Aggregate {
	out := make([]*Aggregate, 0, len(a.groups))
	for _, g := range a.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if a.report == "session" {
			if !x.LastActivity.Equal(y.LastActivity) {
				return x.LastActivity.After(y.LastActivity)
			}
		} else if x.Group != y.Group {
			return x.Group < y.Group
		}
		if x.Harness != y.Harness {
			return x.Harness < y.Harness
		}
		return x.Model < y.Model
	})
	return out
}

func (a *Aggregator) PrintTable(noCost bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	label := strings.ToUpper(a.report)
	if a.report == "weekly" {
		label = "WEEK_START"
	}
	if a.report == "model" {
		label = "SCOPE"
	}
	header := []string{label, "MODEL", "INPUT", "OUTPUT", "CACHE_RD", "CACHE_WR", "REASON", "TOTAL"}
	if !noCost {
		header = append(header, "COST USD")
	}
	header = append(header, "EVENTS")
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, g := range a.sortedGroups() {
		model := g.Model
		if model == "" {
			model = "-"
		}
		writeAggregateRow(w, shortGroup(g.Group), model, *g, noCost)
	}
	if len(a.groups) > 0 {
		writeAggregateRow(w, "TOTAL", "-", a.total, noCost)
	}
	_ = w.Flush()
}

func writeAggregateRow(w *tabwriter.Writer, group, model string, g Aggregate, noCost bool) {
	row := []string{
		group,
		model,
		fmtInt(g.Input),
		fmtInt(g.Output),
		fmtInt(g.CacheRead),
		fmtInt(g.CacheWrite),
		fmtInt(g.Reasoning),
		fmtInt(g.Total),
	}
	if !noCost {
		row = append(row, formatCost(g))
	}
	row = append(row, strconv.Itoa(g.Events))
	fmt.Fprintln(w, strings.Join(row, "\t"))
}

func shortGroup(s string) string {
	if len(s) <= 42 {
		return s
	}
	return s[:18] + "…" + s[len(s)-18:]
}

func formatCost(g Aggregate) string {
	if g.CostComplete {
		return fmt.Sprintf("$%.4f", g.CostUSD)
	}
	if g.CostUSD == 0 {
		return "?"
	}
	return fmt.Sprintf("$%.4f+?", g.CostUSD)
}

func fmtInt(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

type jsonReport struct {
	Version   string                 `json:"version"`
	Report    string                 `json:"report"`
	Generated string                 `json:"generated_at"`
	Groups    []*Aggregate           `json:"groups"`
	Totals    Aggregate              `json:"totals"`
	Parser    map[string]*ParseStats `json:"parser"`
}

func (a *Aggregator) PrintJSON(stats map[string]*ParseStats) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonReport{
		Version: version, Report: a.report, Generated: time.Now().UTC().Format(time.RFC3339),
		Groups: a.sortedGroups(), Totals: a.total, Parser: stats,
	})
}

func printStats(stats map[string]*ParseStats) {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := stats[k]
		fmt.Fprintf(os.Stderr, "%s: files=%d lines=%d events=%d dedup=%d replay-skip=%d malformed=%d skipped=%d\n",
			k, s.Files, s.Lines, s.Emitted, s.Deduplicated, s.ReplaySkipped, s.Malformed, s.Skipped)
	}
}

func saturatingAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func saturatingSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

func saturatingSum(vs ...uint64) uint64 {
	var out uint64
	for _, v := range vs {
		out = saturatingAdd(out, v)
	}
	return out
}
