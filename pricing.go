package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	modelsDevPricingURL       = "https://models.dev/api.json"
	pricingSchemaVersion      = 1
	maxPricingDownloadBytes   = 64 << 20
	defaultPricingHTTPTimeout = 30 * time.Second
)

type PriceTier struct {
	Context    uint64  `json:"context"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type Price struct {
	Input              float64     `json:"input"`
	Output             float64     `json:"output"`
	CacheRead          float64     `json:"cache_read"`
	CacheWrite         float64     `json:"cache_write"`
	InputIncludesCache *bool       `json:"input_includes_cache,omitempty"`
	Tiers              []PriceTier `json:"tiers,omitempty"`
}

type PriceBook map[string]Price

type priceFile struct {
	Schema    int              `json:"schema,omitempty"`
	Source    string           `json:"source,omitempty"`
	UpdatedAt string           `json:"updated_at,omitempty"`
	Models    map[string]Price `json:"models"`
}

func defaultPricingPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("LLM_USAGE_PRICING_FILE")); p != "" {
		return filepath.Clean(expandHome(p)), nil
	}
	if base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); base != "" && filepath.IsAbs(base) {
		return filepath.Join(filepath.Clean(base), "llm-usage", "pricing.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pricing: determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "llm-usage", "pricing.json"), nil
}

func runPricingCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printPricingUsage()
		return nil
	}

	switch strings.ToLower(args[0]) {
	case "path":
		if len(args) != 1 {
			return fmt.Errorf("pricing path takes no arguments")
		}
		path, err := defaultPricingPath()
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("pricing status takes no arguments")
		}
		return printPricingStatus()
	case "update":
		fs := flag.NewFlagSet("llm_usage pricing update", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		output := fs.String("output", "", "write price book here instead of the XDG default")
		timeout := fs.Duration("timeout", defaultPricingHTTPTimeout, "HTTP timeout for the explicit update")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if len(fs.Args()) != 0 {
			return fmt.Errorf("unexpected pricing update arguments: %s", strings.Join(fs.Args(), " "))
		}
		if *timeout <= 0 {
			return fmt.Errorf("--timeout must be positive")
		}
		path := strings.TrimSpace(*output)
		if path == "" {
			var err error
			path, err = defaultPricingPath()
			if err != nil {
				return err
			}
		} else {
			path = filepath.Clean(expandHome(path))
		}
		return updatePricing(path, *timeout)
	default:
		return fmt.Errorf("unknown pricing command %q (expected path, status, or update)", args[0])
	}
}

func printPricingUsage() {
	fmt.Fprint(os.Stdout, `llm_usage pricing - local model-price database

Usage:
  llm_usage pricing path
  llm_usage pricing status
  llm_usage pricing update [--output FILE] [--timeout 30s]

`+"`pricing path` and `pricing status` are offline. `pricing update` is the only\n"+
		"command in llm_usage that performs HTTP; it fetches "+modelsDevPricingURL+"\n"+
		"and atomically stores a compact local price book. Reporting never updates it.\n")
}

func printPricingStatus() error {
	path, err := defaultPricingPath()
	if err != nil {
		return err
	}
	doc, err := loadPriceDocument(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("path: %s\nstatus: not installed\n", path)
			return nil
		}
		return err
	}
	fmt.Printf("path: %s\nstatus: installed\nmodels: %d\n", path, len(doc.Models))
	if doc.Source != "" {
		fmt.Printf("source: %s\n", doc.Source)
	}
	if doc.UpdatedAt != "" {
		fmt.Printf("updated_at: %s\n", doc.UpdatedAt)
	}
	return nil
}

func loadPriceDocument(path string) (priceFile, error) {
	path = expandHome(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return priceFile{}, fmt.Errorf("pricing: %w", err)
	}
	var wrapped priceFile
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Models != nil {
		return wrapped, nil
	}
	// Preserve compatibility with hand-written files whose top-level object is
	// directly model -> price.
	var direct map[string]Price
	if err := json.Unmarshal(b, &direct); err != nil {
		return priceFile{}, fmt.Errorf("pricing: invalid JSON: %w", err)
	}
	return priceFile{Models: direct}, nil
}

func loadPriceBook(path string) (PriceBook, error) {
	doc, err := loadPriceDocument(path)
	if err != nil {
		return nil, err
	}
	return PriceBook(doc.Models), nil
}

func (p PriceBook) price(model string) (Price, bool) {
	if len(p) == 0 || model == "" {
		return Price{}, false
	}
	if v, ok := p[model]; ok {
		return v, true
	}
	// Antigravity can persist internal model-enum placeholders. Resolve them
	// through the same user-facing aliases used by its adapter so a freshly
	// updated price book can still price events from older parser output.
	if resolved := normalizeAntigravityModel(model); resolved != model {
		if v, ok := p[resolved]; ok {
			return v, true
		}
	}
	// Anthropic commonly records dated aliases such as ...-20250514. Restrict
	// this fallback to an eight-digit suffix so numeric model generations are
	// never collapsed accidentally.
	if base := stripDateModelSuffix(model); base != model {
		if v, ok := p[base]; ok {
			return v, true
		}
	}
	bestLen := -1
	var best Price
	for pattern, v := range p {
		if !strings.HasSuffix(pattern, "*") {
			continue
		}
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			best, bestLen = v, len(prefix)
		}
	}
	return best, bestLen >= 0
}

func stripDateModelSuffix(model string) string {
	i := strings.LastIndexByte(model, '-')
	if i < 0 || len(model)-i-1 != 8 {
		return model
	}
	for _, r := range model[i+1:] {
		if r < '0' || r > '9' {
			return model
		}
	}
	return model[:i]
}

func priceForContext(p Price, contextTokens uint64) Price {
	bestContext := uint64(0)
	best := p
	for _, tier := range p.Tiers {
		if tier.Context == 0 || contextTokens < tier.Context || tier.Context < bestContext {
			continue
		}
		bestContext = tier.Context
		best.Input = tier.Input
		best.Output = tier.Output
		best.CacheRead = tier.CacheRead
		best.CacheWrite = tier.CacheWrite
	}
	best.Tiers = nil
	return best
}

func billedOutputTokens(e Event) uint64 {
	if e.BilledOutput > e.Output {
		return e.BilledOutput
	}
	return e.Output
}

func calculateCost(e Event, book PriceBook) (float64, bool) {
	if e.CostKnown {
		return e.Cost, true
	}
	p, ok := book.price(e.Model)
	if !ok {
		return 0, false
	}

	includesCache := e.InputIncludesCache
	if p.InputIncludesCache != nil {
		includesCache = *p.InputIncludesCache
	}
	contextTokens := e.Input
	if !includesCache {
		contextTokens = saturatingSum(contextTokens, e.CacheRead, e.CacheWrite)
	}
	p = priceForContext(p, contextTokens)

	billableInput := e.Input
	if includesCache {
		billableInput = saturatingSub(billableInput, e.CacheRead)
		billableInput = saturatingSub(billableInput, e.CacheWrite)
	}
	const million = 1_000_000.0
	cost := float64(billableInput)/million*p.Input +
		float64(billedOutputTokens(e))/million*p.Output +
		float64(e.CacheRead)/million*p.CacheRead +
		float64(e.CacheWrite)/million*p.CacheWrite
	return cost, true
}

// models.dev's public API is a provider map. We only keep fields needed by this
// program and compact it into our stable local schema during explicit updates.
type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID   string         `json:"id"`
	Cost *modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input           *float64        `json:"input"`
	Output          *float64        `json:"output"`
	CacheRead       *float64        `json:"cache_read"`
	CacheWrite      *float64        `json:"cache_write"`
	ContextOver200K *modelsDevCost  `json:"context_over_200k"`
	Tiers           []modelsDevTier `json:"tiers"`
}

type modelsDevTier struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	Tier       struct {
		Type string `json:"type"`
		Size uint64 `json:"size"`
	} `json:"tier"`
}

type priceCandidate struct {
	Provider string
	Price    Price
}

func importModelsDev(b []byte) (PriceBook, error) {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(b, &providers); err != nil {
		return nil, fmt.Errorf("pricing: invalid models.dev JSON: %w", err)
	}
	if len(providers) == 0 {
		return nil, errors.New("pricing: models.dev response contains no providers")
	}

	book := PriceBook{}
	bare := map[string][]priceCandidate{}
	providerIDs := make([]string, 0, len(providers))
	for id := range providers {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)

	for _, providerKey := range providerIDs {
		provider := providers[providerKey]
		providerID := strings.TrimSpace(provider.ID)
		if providerID == "" {
			providerID = providerKey
		}
		modelKeys := make([]string, 0, len(provider.Models))
		for key := range provider.Models {
			modelKeys = append(modelKeys, key)
		}
		sort.Strings(modelKeys)
		for _, modelKey := range modelKeys {
			m := provider.Models[modelKey]
			price, ok := priceFromModelsDev(m.Cost)
			if !ok {
				continue
			}
			ids := uniqueStrings([]string{strings.TrimSpace(m.ID), strings.TrimSpace(modelKey)})
			for _, id := range ids {
				if id == "" {
					continue
				}
				book[providerID+"/"+id] = price
				bare[id] = append(bare[id], priceCandidate{Provider: providerID, Price: price})
			}
		}
	}

	// Bare model IDs are what Claude Code and Codex normally persist. Prefer the
	// model vendor's own catalog when a reseller publishes the same ID. If there
	// is no obvious vendor, only emit a bare key when it is unambiguous.
	for id, candidates := range bare {
		if selected, ok := selectBarePrice(id, candidates); ok {
			book[id] = selected
		}
	}
	if len(book) == 0 {
		return nil, errors.New("pricing: models.dev response has no usable model prices")
	}
	return book, nil
}

func priceFromModelsDev(c *modelsDevCost) (Price, bool) {
	if c == nil || c.Input == nil || c.Output == nil || *c.Input < 0 || *c.Output < 0 {
		return Price{}, false
	}
	p := Price{Input: *c.Input, Output: *c.Output}
	// A missing cache field means the catalog has no special rate. Falling back
	// to the ordinary input rate avoids silently treating cache tokens as free.
	p.CacheRead = p.Input
	p.CacheWrite = p.Input
	if c.CacheRead != nil && *c.CacheRead >= 0 {
		p.CacheRead = *c.CacheRead
	}
	if c.CacheWrite != nil && *c.CacheWrite >= 0 {
		p.CacheWrite = *c.CacheWrite
	}

	if c.ContextOver200K != nil {
		if tier, ok := modelsDevTierPrice(200_000, c.ContextOver200K.Input, c.ContextOver200K.Output, c.ContextOver200K.CacheRead, c.ContextOver200K.CacheWrite, p); ok {
			p.Tiers = append(p.Tiers, tier)
		}
	}
	for _, raw := range c.Tiers {
		if raw.Tier.Type != "" && raw.Tier.Type != "context" {
			continue
		}
		if tier, ok := modelsDevTierPrice(raw.Tier.Size, raw.Input, raw.Output, raw.CacheRead, raw.CacheWrite, p); ok {
			p.Tiers = append(p.Tiers, tier)
		}
	}
	if len(p.Tiers) > 1 {
		sort.SliceStable(p.Tiers, func(i, j int) bool { return p.Tiers[i].Context < p.Tiers[j].Context })
		// Prefer the newer explicit tier when it duplicates the legacy 200K field.
		out := p.Tiers[:0]
		for _, tier := range p.Tiers {
			if len(out) > 0 && out[len(out)-1].Context == tier.Context {
				out[len(out)-1] = tier
				continue
			}
			out = append(out, tier)
		}
		p.Tiers = out
	}
	return p, true
}

func modelsDevTierPrice(context uint64, input, output, cacheRead, cacheWrite *float64, base Price) (PriceTier, bool) {
	if context == 0 {
		return PriceTier{}, false
	}
	t := PriceTier{Context: context, Input: base.Input, Output: base.Output, CacheRead: base.CacheRead, CacheWrite: base.CacheWrite}
	if input != nil {
		if *input < 0 {
			return PriceTier{}, false
		}
		t.Input = *input
	}
	if output != nil {
		if *output < 0 {
			return PriceTier{}, false
		}
		t.Output = *output
	}
	if cacheRead != nil {
		if *cacheRead < 0 {
			return PriceTier{}, false
		}
		t.CacheRead = *cacheRead
	}
	if cacheWrite != nil {
		if *cacheWrite < 0 {
			return PriceTier{}, false
		}
		t.CacheWrite = *cacheWrite
	}
	return t, true
}

func selectBarePrice(model string, candidates []priceCandidate) (Price, bool) {
	if len(candidates) == 0 {
		return Price{}, false
	}
	preferred := preferredProvider(model)
	if preferred != "" {
		var found *Price
		for i := range candidates {
			if candidates[i].Provider != preferred {
				continue
			}
			if found != nil && !pricesEqual(*found, candidates[i].Price) {
				return Price{}, false
			}
			v := candidates[i].Price
			found = &v
		}
		if found != nil {
			return *found, true
		}
	}
	first := candidates[0].Price
	for _, c := range candidates[1:] {
		if !pricesEqual(first, c.Price) {
			return Price{}, false
		}
	}
	return first, true
}

func preferredProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "openai"
	case strings.HasPrefix(m, "gemini-"):
		return "google"
	default:
		return ""
	}
}

func pricesEqual(a, b Price) bool {
	if a.Input != b.Input || a.Output != b.Output || a.CacheRead != b.CacheRead || a.CacheWrite != b.CacheWrite || len(a.Tiers) != len(b.Tiers) {
		return false
	}
	for i := range a.Tiers {
		if a.Tiers[i] != b.Tiers[i] {
			return false
		}
	}
	return true
}

func updatePricing(path string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("pricing: too many redirects")
			}
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Hostname(), "models.dev") {
				return fmt.Errorf("pricing: refusing redirect to %s", req.URL.String())
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, modelsDevPricingURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "llm_usage/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pricing: fetch %s: %w", modelsDevPricingURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pricing: fetch %s: HTTP %s", modelsDevPricingURL, resp.Status)
	}
	limited := io.LimitReader(resp.Body, maxPricingDownloadBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("pricing: read response: %w", err)
	}
	if len(body) > maxPricingDownloadBytes {
		return fmt.Errorf("pricing: response exceeds %d MiB", maxPricingDownloadBytes>>20)
	}
	book, err := importModelsDev(body)
	if err != nil {
		return err
	}
	doc := priceFile{
		Schema:    pricingSchemaVersion,
		Source:    modelsDevPricingURL,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Models:    map[string]Price(book),
	}
	if err := writePriceDocumentAtomic(path, doc); err != nil {
		return err
	}
	fmt.Printf("updated %d model price entries\n%s\n", len(book), path)
	return nil
}

func writePriceDocumentAtomic(path string, doc priceFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pricing: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".pricing-*.tmp")
	if err != nil {
		return fmt.Errorf("pricing: create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("pricing: chmod temporary file: %w", err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("pricing: encode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("pricing: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pricing: close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("pricing: replace %s: %w", path, err)
	}
	keep = true
	return nil
}
