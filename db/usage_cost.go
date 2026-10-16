package db

import (
	"sort"

	"github.com/Autumn-27/artex/pricing"
)

func usageKnown(u LLMUsage) bool {
	if u.UsageKnown != nil {
		return *u.UsageKnown
	}
	// Legacy SDKs cannot distinguish absent usage from reported zero.
	return u.InputTokens > 0 || u.OutputTokens > 0
}

func estimateUsage(u LLMUsage) *pricing.Cost {
	if !usageKnown(u) {
		return nil
	}
	if u.Cost != nil {
		c := *u.Cost
		return &c
	}
	return pricing.Estimate(u.Model, pricing.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite})
}

func aggregateModelUsage(rows []LLMUsage) []ModelTokenStat {
	byModel := map[string]*ModelTokenStat{}
	for _, u := range rows {
		m := byModel[u.Model]
		if m == nil {
			m = &ModelTokenStat{Model: u.Model, UsageSources: []string{}}
			byModel[u.Model] = m
		}
		m.Calls++
		m.InputTokens += u.InputTokens
		m.OutputTokens += u.OutputTokens
		m.CacheReadTokens += u.CacheRead
		m.CacheWriteTokens += u.CacheWrite
		source := u.UsageSource
		if source == "" {
			source = "unknown"
		}
		present := false
		for _, s := range m.UsageSources {
			if s == source {
				present = true
			}
		}
		if !present {
			m.UsageSources = append(m.UsageSources, source)
		}
		if !usageKnown(u) {
			m.UsageMissingCalls++
		}
		c := estimateUsage(u)
		if c == nil {
			m.UnpricedCalls++
			continue
		}
		m.PricedCalls++
		if m.Cost == nil {
			m.Cost = c
			continue
		}
		addCost(m.Cost, c)
	}
	out := make([]ModelTokenStat, 0, len(byModel))
	for _, m := range byModel {
		sort.Strings(m.UsageSources)
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].InputTokens+out[i].OutputTokens, out[j].InputTokens+out[j].OutputTokens
		if a != b {
			return a > b
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func addCost(a, b *pricing.Cost) {
	a.InputUSD += b.InputUSD
	a.CacheReadUSD += b.CacheReadUSD
	a.CacheWriteUSD += b.CacheWriteUSD
	a.OutputUSD += b.OutputUSD
	a.TotalUSD += b.TotalUSD
	a.WithoutCacheUSD += b.WithoutCacheUSD
	a.CacheReadSavingsUSD += b.CacheReadSavingsUSD
	a.CacheWritePremiumUSD += b.CacheWritePremiumUSD
	a.NetSavingsUSD += b.NetSavingsUSD
	a.LongContext = a.LongContext || b.LongContext
	if a.PriceVersion != b.PriceVersion {
		a.PriceVersion = "mixed"
	}
	if a.SourceURL != b.SourceURL {
		a.SourceURL = ""
	}
}
