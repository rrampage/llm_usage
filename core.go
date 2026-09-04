package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const version = "0.1.2"

type Event struct {
	Harness            string
	Session            string
	Project            string
	Model              string
	Timestamp          time.Time
	Input              uint64
	Output             uint64
	CacheRead          uint64
	CacheWrite         uint64
	Reasoning          uint64 // informational reasoning/thought tokens shown separately
	BilledOutput       uint64 // optional output token count used for pricing; 0 means Output
	Total              uint64
	Cost               float64
	CostKnown          bool
	InputIncludesCache bool // Codex/OpenAI input_tokens includes cached/cache-write subsets.
}

// Harness is intentionally small. Cross-file semantics (fork replay,
// cumulative counters, archive precedence) belong inside the adapter.
type Harness interface {
	Name() string
	DefaultRoots() []string
	Load(ctx *LoadContext, roots []string, emit func(Event) error) error
}

type LoadContext struct {
	Strict  bool
	Verbose bool
	Report  string
	Since   time.Time
	Until   time.Time
	Stats   map[string]*ParseStats
}

type ParseStats struct {
	Files         int `json:"files"`
	Lines         int `json:"lines"`
	Malformed     int `json:"malformed_lines"`
	Skipped       int `json:"skipped_records"`
	Emitted       int `json:"emitted_events"`
	Deduplicated  int `json:"deduplicated_events"`
	ReplaySkipped int `json:"replay_skipped_events"`
}

func (c *LoadContext) stat(name string) *ParseStats {
	if c.Stats[name] == nil {
		c.Stats[name] = &ParseStats{}
	}
	return c.Stats[name]
}

// flexUint64 accepts normal JSON numbers and numeric strings. Agent logs are
// mostly regular JSON, but this makes the parser tolerant of common wrappers.
type flexUint64 uint64

func (u *flexUint64) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) || len(b) == 0 {
		*u = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if ferr != nil || f < 0 || f > math.MaxUint64 {
				return err
			}
			n = uint64(f)
		}
		*u = flexUint64(n)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	if v, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
		*u = flexUint64(v)
		return nil
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f < 0 || f > math.MaxUint64 {
		return fmt.Errorf("invalid uint64 %q", n.String())
	}
	*u = flexUint64(uint64(f))
	return nil
}

type flexFloat64 float64

func (f *flexFloat64) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) || len(b) == 0 {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return err
		}
		*f = flexFloat64(v)
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return err
	}
	*f = flexFloat64(v)
	return nil
}
