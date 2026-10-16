# API-equivalent token pricing

`pricing.Estimate(model, usage)` returns a USD estimate for **one completion
call**, or `nil` for an unknown model or invalid counters. These are standard
API-equivalent estimates, not actual invoices, subscription charges, or account
credit usage. Prices exclude taxes, tools, priority/batch discounts, and negotiated
rates. ARTEX stores the estimate and catalog provenance with new usage rows.
Historical rows without a cost snapshot are estimated using the active catalog,
not historical billing rates.

```go
cost := pricing.Estimate("gpt-6.1-sol", pricing.Usage{
    InputTokens: 100000,
    OutputTokens: 10000,
    CacheReadTokens: 40000,
    CacheWriteTokens: 10000,
})
// cost.TotalUSD = 0.229; cost.NetSavingsUSD = 0.071
```

Input includes both cache reads and cache writes. Normalize provider counters
before calling this package; input reported exclusive of cache must have cache
counts added once. Output already includes reasoning tokens where reported;
never add reasoning tokens again. All counters must be nonnegative and cache
reads plus writes cannot exceed input.

`input_usd` covers uncached input. `without_cache_usd` prices all input at the
uncached rate with the same output charge. Read savings and write premiums are
reported separately; `net_savings_usd = without_cache_usd - total_usd` can be
negative. Estimate each call first and then sum cost fields. Applying a rate to
aggregate token counts can incorrectly trigger long-context pricing.

The built-in catalog is dated **2026-10-16**, uses exact model IDs, and reports
`currency: "USD"` and `basis: "standard_api_equivalent"`. USD per million tokens:

| Model | Input | Cache read | Cache write | Output |
| --- | ---: | ---: | ---: | ---: |
| [gpt-6.1-sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol) | 2 | 0.1 | 2.5 | 10 |
| [gpt-6-sol](https://developers.openai.com/api/docs/models/gpt-6-sol) | 2 | 0.2 | 2.5 | 10 |
| [gpt-6-astra](https://developers.openai.com/api/docs/pricing) | 10 | 1 | 12.5 | 50 |
| [gpt-6-luna](https://developers.openai.com/api/docs/models/gpt-6-luna) | 0.1 | 0.01 | 0.125 | 0.5 |

For these models, input greater than 272,000 tokens doubles all input-side rates
and multiplies output rates by 1.5 for the entire call. Exactly 272,000 tokens
uses standard rates. Model suffixes and aliases are never inferred.

## Custom catalogs

Call `pricing.ConfigureFromEnv()` once at startup and surface any returned error.
When `ARTEX_MODEL_PRICING_FILE` is unset or empty, it activates the built-ins. A
configured file **replaces** the catalog; unlisted models remain unpriced.
Invalid configuration disables estimates and returns an error. It never silently
retains previously loaded or built-in rates.

Alternatively, `pricing.LoadRatesFile(path)` returns an immutable `Catalog` for
explicit use with `catalog.Estimate(model, usage)` without changing global rates.
Both estimation APIs are safe for concurrent reads; global configuration changes
are synchronized.

```json
{
  "price_version": "2026-10-16-custom",
  "source_url": "https://example.com/pricing",
  "models": {
    "my-exact-model-id": {
      "input_usd_per_million": 3,
      "cache_read_usd_per_million": 0.3,
      "cache_write_usd_per_million": 4,
      "output_usd_per_million": 9,
      "long_context": {
        "threshold": 272000,
        "input_multiplier": 2,
        "output_multiplier": 1.5
      }
    }
  }
}
```

All four rate fields are required and must be finite and nonnegative; explicit
zero rates are allowed. `price_version`, an absolute HTTP(S) `source_url`, and at
least one exact model ID are required. Unknown JSON fields are rejected. Omit
`long_context` if the model has no such policy; its threshold and multipliers
must otherwise be positive. Multipliers apply to the entire call above the
threshold. Catalog provenance is included with each estimate.

Run `go test ./pricing` to check cache accounting, model rates, long-context
boundaries, counter validation, JSON output, and custom configuration.
