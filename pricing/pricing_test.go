package pricing

import (
	"encoding/json"
	"math"
	"testing"
)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("%s = %.12f; want %.12f", name, got, want)
	}
}

func TestEstimateCacheAccounting(t *testing.T) {
	c := Estimate("gpt-6.1-sol", Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 40000, CacheWriteTokens: 10000})
	if c == nil {
		t.Fatal("missing estimate")
	}
	near(t, "input", c.InputUSD, .1)
	near(t, "cache read", c.CacheReadUSD, .004)
	near(t, "cache write", c.CacheWriteUSD, .025)
	near(t, "output", c.OutputUSD, .1)
	near(t, "total", c.TotalUSD, .229)
	near(t, "without cache", c.WithoutCacheUSD, .3)
	near(t, "read savings", c.CacheReadSavingsUSD, .076)
	near(t, "write premium", c.CacheWritePremiumUSD, .005)
	near(t, "net savings", c.NetSavingsUSD, .071)
	near(t, "component sum", c.TotalUSD, c.InputUSD+c.CacheReadUSD+c.CacheWriteUSD+c.OutputUSD)
	if c.Currency != "USD" || c.Basis != "standard_api_equivalent" || c.PriceVersion != "2026-10-16" || c.SourceURL != "https://developers.openai.com/api/docs/models/gpt-6.1-sol" {
		t.Fatalf("missing estimate provenance: %+v", c)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"currency", "basis", "price_version", "source_url", "input_usd", "cache_read_usd", "cache_write_usd", "output_usd", "total_usd", "without_cache_usd", "cache_read_savings_usd", "cache_write_premium_usd", "net_savings_usd"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("missing JSON field %s", field)
		}
	}
}

func TestEstimateModels(t *testing.T) {
	for _, tc := range []struct {
		model string
		total float64
	}{
		{"gpt-6.1-sol", .00286}, {"gpt-6-sol", .00287}, {"gpt-6-astra", .01435}, {"gpt-6-luna", .0001435},
	} {
		t.Run(tc.model, func(t *testing.T) {
			c := Estimate(tc.model, Usage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 100, CacheWriteTokens: 100})
			if c == nil {
				t.Fatal("missing estimate")
			}
			near(t, "total", c.TotalUSD, tc.total)
		})
	}
}

func TestEstimateLongContextBoundary(t *testing.T) {
	for _, tc := range []struct {
		input int
		total float64
		long  bool
	}{
		{272000, .4172, false}, {272001, .829404, true},
	} {
		c := Estimate("gpt-6.1-sol", Usage{InputTokens: tc.input, CacheReadTokens: 72000, OutputTokens: 1000})
		if c == nil {
			t.Fatal("missing estimate")
		}
		near(t, "total", c.TotalUSD, tc.total)
		if c.LongContext != tc.long {
			t.Errorf("long_context = %v; want %v", c.LongContext, tc.long)
		}
	}
	// The premium also applies to cached writes and all input, not just excess.
	c := Estimate("gpt-6.1-sol", Usage{InputTokens: 300000, CacheWriteTokens: 300000})
	near(t, "long writes", c.CacheWriteUSD, 1.5)
	near(t, "long baseline", c.WithoutCacheUSD, 1.2)
}

func TestEstimateWriteOnlySavingsStayNegative(t *testing.T) {
	c := Estimate("gpt-6.1-sol", Usage{InputTokens: 100000, CacheWriteTokens: 100000})
	near(t, "net savings", c.NetSavingsUSD, -.05)
	near(t, "read savings", c.CacheReadSavingsUSD, 0)
	near(t, "write premium", c.CacheWritePremiumUSD, .05)
}

func TestEstimateRejectsUnknownAndInvalidUsage(t *testing.T) {
	for _, model := range []string{"", "unknown", "gpt-6.1-sol-latest", "GPT-6.1-SOL", " gpt-6.1-sol"} {
		if c := Estimate(model, Usage{}); c != nil {
			t.Errorf("model %q unexpectedly priced", model)
		}
	}
	maxInt := int(^uint(0) >> 1)
	for _, usage := range []Usage{
		{InputTokens: -1}, {OutputTokens: -1}, {CacheReadTokens: -1}, {CacheWriteTokens: -1},
		{InputTokens: 10, CacheReadTokens: 11}, {InputTokens: 10, CacheWriteTokens: 11},
		{InputTokens: 10, CacheReadTokens: 6, CacheWriteTokens: 5},
		{InputTokens: maxInt, CacheReadTokens: maxInt, CacheWriteTokens: maxInt},
	} {
		if c := Estimate("gpt-6.1-sol", usage); c != nil {
			t.Errorf("invalid usage priced: %+v", usage)
		}
	}
	if c := Estimate("gpt-6.1-sol", Usage{}); c == nil || c.TotalUSD != 0 {
		t.Fatal("zero usage must have a zero estimate")
	}
}
