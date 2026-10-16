// Package pricing estimates standard API-equivalent token costs. Estimates are
// not invoices or measurements of subscription charges.
package pricing

import "sync"

// Usage describes one completion call. InputTokens includes cache reads and
// cache writes; OutputTokens already includes reasoning tokens when reported.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Cost is a USD estimate with rate provenance and cache savings. InputUSD is
// the cost of uncached input only. NetSavingsUSD can be negative.
type Cost struct {
	Currency             string  `json:"currency"`
	Basis                string  `json:"basis"`
	PriceVersion         string  `json:"price_version"`
	SourceURL            string  `json:"source_url"`
	InputUSD             float64 `json:"input_usd"`
	CacheReadUSD         float64 `json:"cache_read_usd"`
	CacheWriteUSD        float64 `json:"cache_write_usd"`
	OutputUSD            float64 `json:"output_usd"`
	TotalUSD             float64 `json:"total_usd"`
	WithoutCacheUSD      float64 `json:"without_cache_usd"`
	CacheReadSavingsUSD  float64 `json:"cache_read_savings_usd"`
	CacheWritePremiumUSD float64 `json:"cache_write_premium_usd"`
	NetSavingsUSD        float64 `json:"net_savings_usd"`
	LongContext          bool    `json:"long_context,omitempty"`
}

type rates struct {
	input, read, write, output float64
	version, source            string
	threshold                  int
	inputMultiplier            float64
	outputMultiplier           float64
}

// Catalog is an immutable collection of exact model IDs and their rates. A
// zero-value Catalog returns no estimates. Obtain a custom one with LoadRatesFile.
type Catalog struct{ models map[string]rates }

func builtinCatalog() Catalog {
	models := map[string]rates{
		"gpt-6.1-sol": {input: 2, read: .1, write: 2.5, output: 10, source: "https://developers.openai.com/api/docs/models/gpt-6.1-sol"},
		"gpt-6-sol":   {input: 2, read: .2, write: 2.5, output: 10, source: "https://developers.openai.com/api/docs/models/gpt-6-sol"},
		"gpt-6-astra": {input: 10, read: 1, write: 12.5, output: 50, source: "https://developers.openai.com/api/docs/pricing"},
		"gpt-6-luna":  {input: .1, read: .01, write: .125, output: .5, source: "https://developers.openai.com/api/docs/models/gpt-6-luna"},
	}
	for model, r := range models {
		r.version = "2026-10-16"
		r.threshold, r.inputMultiplier, r.outputMultiplier = 272000, 2, 1.5
		models[model] = r
	}
	return Catalog{models: models}
}

var active = struct {
	sync.RWMutex
	catalog Catalog
}{catalog: builtinCatalog()}

// Estimate prices a single call using the active catalog. Unknown models and
// invalid counters return nil. Never pass aggregate usage: long-context pricing
// is applied per call, and its premium covers the entire request.
func Estimate(model string, u Usage) *Cost {
	active.RLock()
	catalog := active.catalog
	active.RUnlock()
	return catalog.Estimate(model, u)
}

// Estimate prices one call using this catalog without changing global rates.
func (catalog Catalog) Estimate(model string, u Usage) *Cost {
	r, ok := catalog.models[model]
	if !ok || u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 ||
		u.CacheReadTokens > u.InputTokens || u.CacheWriteTokens > u.InputTokens-u.CacheReadTokens {
		return nil
	}
	c := &Cost{Currency: "USD", Basis: "standard_api_equivalent", PriceVersion: r.version, SourceURL: r.source}
	if r.threshold > 0 && u.InputTokens > r.threshold {
		c.LongContext = true
		r.input *= r.inputMultiplier
		r.read *= r.inputMultiplier
		r.write *= r.inputMultiplier
		r.output *= r.outputMultiplier
	}
	const million = 1e6
	c.InputUSD = float64(u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens) / million * r.input
	c.CacheReadUSD = float64(u.CacheReadTokens) / million * r.read
	c.CacheWriteUSD = float64(u.CacheWriteTokens) / million * r.write
	c.OutputUSD = float64(u.OutputTokens) / million * r.output
	c.TotalUSD = c.InputUSD + c.CacheReadUSD + c.CacheWriteUSD + c.OutputUSD
	c.WithoutCacheUSD = float64(u.InputTokens)/million*r.input + c.OutputUSD
	c.CacheReadSavingsUSD = float64(u.CacheReadTokens) / million * (r.input - r.read)
	c.CacheWritePremiumUSD = float64(u.CacheWriteTokens) / million * (r.write - r.input)
	c.NetSavingsUSD = c.WithoutCacheUSD - c.TotalUSD
	return c
}
