package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
)

type catalogFile struct {
	PriceVersion string              `json:"price_version"`
	SourceURL    string              `json:"source_url"`
	Models       map[string]rateFile `json:"models"`
}

type rateFile struct {
	Input       *float64 `json:"input_usd_per_million"`
	CacheRead   *float64 `json:"cache_read_usd_per_million"`
	CacheWrite  *float64 `json:"cache_write_usd_per_million"`
	Output      *float64 `json:"output_usd_per_million"`
	LongContext *struct {
		Threshold        int     `json:"threshold"`
		InputMultiplier  float64 `json:"input_multiplier"`
		OutputMultiplier float64 `json:"output_multiplier"`
	} `json:"long_context,omitempty"`
}

// LoadRatesFile reads a complete replacement catalog. All four rates must be
// explicitly provided, finite and nonnegative; zero is allowed. Invalid files
// return an error and an empty catalog, never a partial or built-in fallback.
// See README.md for the JSON schema and optional long-context policy.
func LoadRatesFile(path string) (Catalog, error) {
	f, err := os.Open(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("open model pricing: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var file catalogFile
	if err := decoder.Decode(&file); err != nil {
		return Catalog{}, fmt.Errorf("decode model pricing: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Catalog{}, fmt.Errorf("model pricing must contain exactly one JSON object")
	}
	if strings.TrimSpace(file.PriceVersion) == "" {
		return Catalog{}, fmt.Errorf("model pricing requires price_version")
	}
	source, err := url.Parse(file.SourceURL)
	if err != nil || source.Host == "" || (source.Scheme != "https" && source.Scheme != "http") {
		return Catalog{}, fmt.Errorf("model pricing requires an absolute HTTP(S) source_url")
	}
	if len(file.Models) == 0 {
		return Catalog{}, fmt.Errorf("model pricing requires at least one model")
	}
	catalog := Catalog{models: make(map[string]rates, len(file.Models))}
	for model, entry := range file.Models {
		if model == "" || strings.TrimSpace(model) != model {
			return Catalog{}, fmt.Errorf("model pricing contains an empty or whitespace-padded model ID")
		}
		for name, value := range map[string]*float64{"input": entry.Input, "cache_read": entry.CacheRead, "cache_write": entry.CacheWrite, "output": entry.Output} {
			if value == nil || *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
				return Catalog{}, fmt.Errorf("model pricing %q requires a finite nonnegative %s rate", model, name)
			}
		}
		r := rates{input: *entry.Input, read: *entry.CacheRead, write: *entry.CacheWrite, output: *entry.Output, version: file.PriceVersion, source: file.SourceURL}
		if policy := entry.LongContext; policy != nil {
			if policy.Threshold <= 0 || !positiveFinite(policy.InputMultiplier) || !positiveFinite(policy.OutputMultiplier) {
				return Catalog{}, fmt.Errorf("model pricing %q requires a positive long_context threshold and finite positive multipliers", model)
			}
			r.threshold, r.inputMultiplier, r.outputMultiplier = policy.Threshold, policy.InputMultiplier, policy.OutputMultiplier
		}
		catalog.models[model] = r
	}
	return catalog, nil
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

// ConfigureFromEnv activates ARTEX_MODEL_PRICING_FILE as a complete replacement
// catalog. An unset or empty value restores the built-ins. Call once at startup
// and surface its error: invalid configuration disables all estimates rather
// than silently retaining another catalog. Calls are safe alongside Estimate.
func ConfigureFromEnv() error {
	var catalog Catalog
	var err error
	if path := os.Getenv("ARTEX_MODEL_PRICING_FILE"); path != "" {
		catalog, err = LoadRatesFile(path)
	} else {
		catalog = builtinCatalog()
	}
	active.Lock()
	active.catalog = catalog
	active.Unlock()
	return err
}
