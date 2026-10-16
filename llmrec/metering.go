package llmrec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

const meteringHeaderName = "X-Artex-Usage"
const maxMeteringHeaderBytes = 64 * 1024
const maxMissingInferenceIDs = 1024

type meteringContextKey struct{}

// Metering captures only usage metadata, independently of optional body capture.
// A fresh instance belongs to each recorder call; its mutex also covers transports
// that perform concurrent requests or retries within that call.
type Metering struct {
	mu    sync.Mutex
	state meteringSnapshot
	api   *apiUsageObserver
}

type meteringSample struct {
	ID               string `json:"id"`
	Model            string `json:"model"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens"`
}

type meteringSnapshot struct {
	Present    bool
	Source     string
	Known      bool
	Deferred   bool
	Samples    []meteringSample
	MissingIDs []string
	API        apiUsageSnapshot
}

func NewMetering(ctx context.Context) (context.Context, *Metering) {
	m := &Metering{}
	return context.WithValue(ctx, meteringContextKey{}, m), m
}

func MeteringFrom(ctx context.Context) *Metering {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(meteringContextKey{}).(*Metering)
	return m
}

// CaptureHeaders accepts the bridge's bounded base64 JSON envelope. Invalid or
// ambiguous metadata stays explicitly unknown; it must never fall back to a
// normalized SDK zero that could be mistaken for measured usage.
func (m *Metering) CaptureHeaders(headers http.Header) {
	if m == nil {
		return
	}
	values, present := headers[http.CanonicalHeaderKey(meteringHeaderName)]
	if !present {
		return
	}
	s := meteringSnapshot{Present: true, Source: "unknown"}
	if len(values) == 1 && len(values[0]) <= maxMeteringHeaderBytes {
		if b, err := base64.StdEncoding.Strict().DecodeString(values[0]); err == nil {
			var envelope struct {
				Source     string          `json:"source"`
				Known      *bool           `json:"known"`
				Deferred   bool            `json:"deferred"`
				Samples    json.RawMessage `json:"samples"`
				MissingIDs json.RawMessage `json:"missing_ids"`
			}
			if json.Unmarshal(b, &envelope) == nil && envelope.Source == "codex_subscription" {
				s.Source = envelope.Source
				samples, valid := decodeSamples(envelope.Samples)
				missing, missingValid := decodeMissingIDs(envelope.MissingIDs, samples)
				if envelope.Known != nil && valid && missingValid && validSamples(samples, *envelope.Known) && (*envelope.Known || len(missing) == 0) && (!envelope.Deferred || (!*envelope.Known && len(samples) == 0 && len(envelope.MissingIDs) == 0)) {
					s.Known = *envelope.Known
					s.Deferred = envelope.Deferred
					s.Samples = samples
					s.MissingIDs = missing
				}
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Present {
		m.state = s
		return
	}
	// A deferred bridge response carries no inference yet. Retries can replace
	// it with terminal accounting; an invalid later response must stay unknown.
	if m.state.Deferred {
		m.state = s
		return
	}
	if s.Deferred {
		return
	}
	// Preserve usage from separate backend inferences across HTTP attempts, while
	// retries of the same inference ID count once. Conflicting copies are unknown.
	// A legacy unknown attempt adds an unknown row, without erasing measurements
	// already obtained from another attempt. Missing IDs give that row a stable
	// identity in newer bridge responses.
	m.state.Known = m.state.Known && s.Known && m.state.Source == s.Source
	if s.Source == "codex_subscription" {
		m.state.Source = s.Source
	}
	merged := append([]meteringSample(nil), m.state.Samples...)
	seen := make(map[string]meteringSample, len(merged))
	for _, sample := range merged {
		seen[sample.ID] = sample
	}
	for _, sample := range s.Samples {
		if prior, ok := seen[sample.ID]; ok {
			if prior != sample {
				m.state.Known = false
				m.state.Samples = nil
				m.state.MissingIDs = nil
				return
			}
			continue
		}
		seen[sample.ID] = sample
		merged = append(merged, sample)
	}
	missing := append([]string(nil), m.state.MissingIDs...)
	missingSeen := make(map[string]bool, len(missing))
	for _, id := range missing {
		missingSeen[id] = true
	}
	for _, id := range s.MissingIDs {
		if !missingSeen[id] {
			missing = append(missing, id)
			missingSeen[id] = true
		}
	}
	if !validSamples(merged, true) || !validMissingIDs(missing, merged) {
		m.state.Known = false
		m.state.Samples = nil
		m.state.MissingIDs = nil
		return
	}
	m.state.Samples = merged
	m.state.MissingIDs = missing
}

// Missing IDs are optional, but when present must be a bounded array of
// distinct nonblank strings and cannot identify an already measured inference.
func decodeMissingIDs(raw json.RawMessage, samples []meteringSample) ([]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil || !validMissingIDs(ids, samples) {
		return nil, false
	}
	return ids, true
}

func validMissingIDs(ids []string, samples []meteringSample) bool {
	if len(ids) > maxMissingInferenceIDs {
		return false
	}
	seen := make(map[string]bool, len(ids)+len(samples))
	for _, sample := range samples {
		seen[sample.ID] = true
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// Require explicit counters: JSON's omitted/null integer defaults are not
// evidence of measured zero. Unknown and no-inference envelopes still carry [].
func decodeSamples(raw json.RawMessage) ([]meteringSample, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil, false
	}
	samples := make([]meteringSample, 0, len(entries))
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		var sample meteringSample
		if json.Unmarshal(entry, &fields) != nil || json.Unmarshal(entry, &sample) != nil {
			return nil, false
		}
		for _, name := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
			value, ok := fields[name]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, false
			}
		}
		samples = append(samples, sample)
	}
	return samples, true
}

func validSamples(samples []meteringSample, known bool) bool {
	if !known {
		return len(samples) == 0
	}
	// Ledger counters use PostgreSQL INTEGER. Also bound aggregate addition so a
	// crafted header cannot overflow Go integers or downstream aggregate usage.
	const maxTokens = 1<<31 - 1
	var totalInput, totalOutput int64
	seen := make(map[string]bool, len(samples))
	for _, s := range samples {
		if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.Model) == "" || seen[s.ID] {
			return false
		}
		seen[s.ID] = true
		if s.InputTokens < 0 || s.OutputTokens < 0 || s.CacheReadTokens < 0 || s.CacheWriteTokens < 0 || s.ReasoningTokens < 0 {
			return false
		}
		if s.InputTokens > maxTokens || s.OutputTokens > maxTokens || s.CacheReadTokens > s.InputTokens || s.CacheWriteTokens > s.InputTokens-s.CacheReadTokens || s.ReasoningTokens > s.OutputTokens {
			return false
		}
		totalInput += int64(s.InputTokens)
		totalOutput += int64(s.OutputTokens)
		if totalInput > maxTokens || totalOutput > maxTokens {
			return false
		}
	}
	return true
}

func (m *Metering) snapshot() meteringSnapshot {
	if m == nil {
		return meteringSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.API = m.api.snapshot()
	s.Samples = append([]meteringSample(nil), s.Samples...)
	s.MissingIDs = append([]string(nil), s.MissingIDs...)
	return s
}

func (s meteringSnapshot) usage() llm.Usage {
	var usage llm.Usage
	for _, sample := range s.Samples {
		usage.Add(llm.Usage{InputTokens: sample.InputTokens, OutputTokens: sample.OutputTokens, CacheReadTokens: sample.CacheReadTokens, CacheWriteTokens: sample.CacheWriteTokens})
	}
	return usage
}
