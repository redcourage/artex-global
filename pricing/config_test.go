package pricing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const customJSON = `{"price_version":"2026-10-16-custom","source_url":"https://example.com/pricing","models":{"custom-model":{"input_usd_per_million":3,"cache_read_usd_per_million":0.3,"cache_write_usd_per_million":4,"output_usd_per_million":9}}}`

func writeCatalog(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rates.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRatesFilePricesExactCustomModel(t *testing.T) {
	catalog, err := LoadRatesFile(writeCatalog(t, customJSON))
	if err != nil {
		t.Fatal(err)
	}
	c := catalog.Estimate("custom-model", Usage{InputTokens: 1000000, CacheReadTokens: 200000, CacheWriteTokens: 100000, OutputTokens: 100000})
	if c == nil {
		t.Fatal("missing custom estimate")
	}
	near(t, "custom total", c.TotalUSD, 3.46)
	near(t, "custom savings", c.NetSavingsUSD, .44)
	if c.LongContext || c.PriceVersion != "2026-10-16-custom" || c.SourceURL != "https://example.com/pricing" {
		t.Fatalf("unexpected metadata: %+v", c)
	}
	if catalog.Estimate("gpt-6.1-sol", Usage{}) != nil {
		t.Fatal("custom catalogs must not fall back to built-ins")
	}
	if catalog.Estimate("custom-model-latest", Usage{}) != nil {
		t.Fatal("custom models must match exactly")
	}
}

func TestLoadRatesFileExplicitLongContextPolicy(t *testing.T) {
	data := strings.Replace(customJSON, `"output_usd_per_million":9`, `"output_usd_per_million":9,"long_context":{"threshold":100,"input_multiplier":2,"output_multiplier":1.5}`, 1)
	catalog, err := LoadRatesFile(writeCatalog(t, data))
	if err != nil {
		t.Fatal(err)
	}
	c := catalog.Estimate("custom-model", Usage{InputTokens: 101, OutputTokens: 100})
	if c == nil || !c.LongContext {
		t.Fatal("custom long context policy not applied")
	}
	near(t, "custom long total", c.TotalUSD, .001956)
}

func TestLoadRatesFileAllowsExplicitFreeRates(t *testing.T) {
	data := `{"price_version":"free","source_url":"https://example.com/free","models":{"free":{"input_usd_per_million":0,"cache_read_usd_per_million":0,"cache_write_usd_per_million":0,"output_usd_per_million":0}}}`
	catalog, err := LoadRatesFile(writeCatalog(t, data))
	if err != nil {
		t.Fatal(err)
	}
	c := catalog.Estimate("free", Usage{InputTokens: 100, OutputTokens: 100})
	if c == nil || c.TotalUSD != 0 {
		t.Fatal("explicit zero rates must remain valid")
	}
}

func TestLoadRatesFileRejectsInvalidCatalogs(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"empty", ""}, {"null", "null"}, {"empty object", "{}"},
		{"trailing object", customJSON + "{}"}, {"trailing garbage", customJSON + "garbage"},
		{"unknown field", strings.Replace(customJSON, `"models"`, `"typo"`, 1)},
		{"empty models", `{"price_version":"v1","source_url":"https://example.com/pricing","models":{}}`},
		{"missing rate", strings.Replace(customJSON, `"cache_write_usd_per_million":4,`, "", 1)},
		{"null rate", strings.Replace(customJSON, `"input_usd_per_million":3`, `"input_usd_per_million":null`, 1)},
		{"negative rate", strings.Replace(customJSON, `"input_usd_per_million":3`, `"input_usd_per_million":-1`, 1)},
		{"infinite rate", strings.Replace(customJSON, `"input_usd_per_million":3`, `"input_usd_per_million":1e999`, 1)},
		{"bad source", strings.Replace(customJSON, "https://example.com/pricing", "not-a-url", 1)},
		{"missing version", strings.Replace(customJSON, `"2026-10-16-custom"`, `""`, 1)},
		{"empty model", strings.Replace(customJSON, `"custom-model"`, `""`, 1)},
		{"padded model", strings.Replace(customJSON, `"custom-model"`, `" custom-model "`, 1)},
		{"bad long context", strings.Replace(customJSON, `"output_usd_per_million":9`, `"output_usd_per_million":9,"long_context":{"threshold":100,"input_multiplier":0,"output_multiplier":1.5}`, 1)},
		{"negative threshold", strings.Replace(customJSON, `"output_usd_per_million":9`, `"output_usd_per_million":9,"long_context":{"threshold":-1,"input_multiplier":2,"output_multiplier":1.5}`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := LoadRatesFile(writeCatalog(t, tc.data))
			if err == nil {
				t.Fatal("invalid catalog accepted")
			}
			if catalog.Estimate("gpt-6.1-sol", Usage{}) != nil {
				t.Fatal("invalid catalog fell back to built-ins")
			}
		})
	}
	if _, err := LoadRatesFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestConfigureFromEnvReplacesCatalogAndFailsClosed(t *testing.T) {
	t.Setenv("ARTEX_MODEL_PRICING_FILE", "")
	t.Cleanup(func() {
		os.Unsetenv("ARTEX_MODEL_PRICING_FILE")
		if err := ConfigureFromEnv(); err != nil {
			t.Error(err)
		}
	})
	if err := ConfigureFromEnv(); err != nil {
		t.Fatal(err)
	}
	if Estimate("gpt-6.1-sol", Usage{}) == nil {
		t.Fatal("missing built-in rates")
	}
	t.Setenv("ARTEX_MODEL_PRICING_FILE", writeCatalog(t, customJSON))
	if err := ConfigureFromEnv(); err != nil {
		t.Fatal(err)
	}
	if Estimate("custom-model", Usage{}) == nil {
		t.Fatal("custom rates not activated")
	}
	if Estimate("gpt-6.1-sol", Usage{}) != nil {
		t.Fatal("built-in rates leaked into custom catalog")
	}
	t.Setenv("ARTEX_MODEL_PRICING_FILE", writeCatalog(t, "{}"))
	if err := ConfigureFromEnv(); err == nil {
		t.Fatal("invalid configuration not reported")
	}
	if Estimate("custom-model", Usage{}) != nil || Estimate("gpt-6.1-sol", Usage{}) != nil {
		t.Fatal("invalid configuration silently retained rates")
	}
}
