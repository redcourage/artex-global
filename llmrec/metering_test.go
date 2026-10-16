package llmrec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func meteringHeader(v any) http.Header {
	b, _ := json.Marshal(v)
	return http.Header{"X-Artex-Usage": []string{base64.StdEncoding.EncodeToString(b)}}
}

func sampleMetadata() map[string]any {
	return map[string]any{"source": "codex_subscription", "known": true, "samples": []any{
		map[string]any{"id": "thread:1", "model": "gpt-5.4", "input_tokens": 100, "output_tokens": 30, "cache_read_tokens": 40, "cache_write_tokens": 10, "reasoning_tokens": 20},
	}}
}

func TestMeteringHeaderValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"source", func(v map[string]any) { v["source"] = "untrusted" }},
		{"negative", func(v map[string]any) { v["samples"].([]any)[0].(map[string]any)["input_tokens"] = -1 }},
		{"cache overlap", func(v map[string]any) { v["samples"].([]any)[0].(map[string]any)["cache_write_tokens"] = 70 }},
		{"reasoning exceeds output", func(v map[string]any) { v["samples"].([]any)[0].(map[string]any)["reasoning_tokens"] = 31 }},
		{"missing id", func(v map[string]any) { v["samples"].([]any)[0].(map[string]any)["id"] = "" }},
		{"missing model", func(v map[string]any) { v["samples"].([]any)[0].(map[string]any)["model"] = "" }},
		{"unknown samples", func(v map[string]any) { v["known"] = false }},
		{"missing known", func(v map[string]any) { delete(v, "known") }},
		{"missing samples", func(v map[string]any) { delete(v, "samples") }},
		{"null samples", func(v map[string]any) { v["samples"] = nil }},
		{"missing counter", func(v map[string]any) { delete(v["samples"].([]any)[0].(map[string]any), "cache_write_tokens") }},
		{"null counter", func(v map[string]any) { v["samples"].([]any)[0].(map[string]any)["cache_write_tokens"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m := NewMetering(context.Background())
			v := sampleMetadata()
			tc.change(v)
			m.CaptureHeaders(meteringHeader(v))
			s := m.snapshot()
			if !s.Present || s.Known || len(s.Samples) > 0 {
				t.Fatalf("invalid header accepted: %+v", s)
			}
		})
	}
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("null")), strings.Repeat("A", 65537)} {
		_, m := NewMetering(context.Background())
		m.CaptureHeaders(http.Header{"X-Artex-Usage": []string{value}})
		if s := m.snapshot(); !s.Present || s.Known {
			t.Fatalf("invalid header accepted: %+v", s)
		}
	}
}

func TestMeteringDeduplicatesRetriesAndIsParallelSafe(t *testing.T) {
	ctx, m := NewMetering(context.Background())
	if MeteringFrom(ctx) != m || MeteringFrom(nil) != nil {
		t.Fatal("context metering mismatch")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.CaptureHeaders(meteringHeader(sampleMetadata())); _ = m.snapshot() }()
	}
	wg.Wait()
	s := m.snapshot()
	if !s.Known || len(s.Samples) != 1 {
		t.Fatalf("retry duplicated: %+v", s)
	}
	u := s.usage()
	if u.InputTokens != 100 || u.OutputTokens != 30 || u.CacheReadTokens != 40 || u.CacheWriteTokens != 10 {
		t.Fatalf("usage=%+v", u)
	}
}

type headerProvider struct {
	completeProvider
	header http.Header
	t      *testing.T
}

func (p headerProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if CaptureFrom(ctx) != nil {
		p.t.Error("body recording unexpectedly enabled")
	}
	if MeteringFrom(ctx) == nil {
		p.t.Fatal("metering unavailable without body recording")
	}
	MeteringFrom(ctx).CaptureHeaders(p.header)
	return p.completeProvider.Complete(ctx, req)
}
func TestCompleteUsesHeaderWithoutBodyRecording(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		header                     http.Header
		input, output, read, write int
	}{
		{"codex", meteringHeader(sampleMetadata()), 100, 30, 40, 10},
		{"invalid", http.Header{"X-Artex-Usage": []string{"invalid"}}, 0, 0, 0, 0},
		{"known empty", meteringHeader(map[string]any{"source": "codex_subscription", "known": true, "samples": []any{}}), 0, 0, 0, 0},
		{"api", nil, 7, 3, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Wrap(headerProvider{header: tc.header, t: t}, nil, "alias", "profile", "", "", func() bool { return false })
			msg, stop, u, err := r.Complete(context.Background(), llm.CompletionRequest{})
			if err != nil || msg.Text() != "answer" || stop != "stop" {
				t.Fatalf("response changed: %v %s %s", err, msg.Text(), stop)
			}
			if u.InputTokens != tc.input || u.OutputTokens != tc.output || u.CacheReadTokens != tc.read || u.CacheWriteTokens != tc.write {
				t.Fatalf("usage=%+v", u)
			}
		})
	}
}

func TestUsageRowsAttributeBackendSamples(t *testing.T) {
	_, m := NewMetering(context.Background())
	data := sampleMetadata()
	data["samples"] = append(data["samples"].([]any), map[string]any{"id": "thread:2", "model": "gpt-5.5", "input_tokens": 7, "output_tokens": 4, "cache_read_tokens": 0, "cache_write_tokens": 0, "reasoning_tokens": 2})
	m.CaptureHeaders(meteringHeader(data))
	r := Wrap(completeProvider{}, nil, "configured-alias", "profile", "", "", nil)
	rows := r.usageRows("42", 12, "exp12-worker-i3", "worker", llm.Usage{}, m.snapshot(), 5, "ok")
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	first := rows[0]
	if first.TaskID != "42" || first.ExplorationID != 12 || first.SessionID != "exp12-worker-i3" || first.Worker != "worker" || first.ProfileName != "profile" || first.Model != "gpt-5.4" || first.MeteringID != "thread:1" || first.UsageSource != "codex_subscription" || first.UsageKnown == nil || !*first.UsageKnown || first.ReasoningTokens != 20 || first.CacheRead != 40 || first.CacheWrite != 10 || first.InputTokens != 100 || first.OutputTokens != 30 {
		t.Fatalf("row=%+v", first)
	}
	if rows[1].Model != "gpt-5.5" || rows[1].MeteringID != "thread:2" || rows[1].InputTokens != 7 {
		t.Fatalf("second row=%+v", rows[1])
	}
}

func TestUsageRowsKnownEmptyUnknownAndAPI(t *testing.T) {
	r := Wrap(completeProvider{}, nil, "model", "profile", "", "", nil)
	for _, tc := range []struct {
		name     string
		metadata meteringSnapshot
		usage    llm.Usage
		count    int
		known    bool
		source   string
	}{
		{"known no inference", meteringSnapshot{Present: true, Known: true, Source: "codex_subscription"}, llm.Usage{}, 0, true, "codex_subscription"},
		{"missing", meteringSnapshot{Present: true, Source: "codex_subscription"}, llm.Usage{InputTokens: 9}, 1, false, "codex_subscription"},
		{"API usage", meteringSnapshot{}, llm.Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 4}, 1, true, "api"},
		{"API zero ambiguous", meteringSnapshot{}, llm.Usage{}, 1, false, "api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := r.usageRows("", 0, "chat", "judge", tc.usage, tc.metadata, 4, "ok")
			if len(rows) != tc.count {
				t.Fatalf("rows=%+v", rows)
			}
			if len(rows) == 0 {
				return
			}
			if rows[0].UsageKnown == nil || *rows[0].UsageKnown != tc.known || rows[0].UsageSource != tc.source {
				t.Fatalf("row=%+v", rows[0])
			}
			if !tc.known && rows[0].InputTokens != 0 {
				t.Fatalf("unknown retained misleading usage: %+v", rows[0])
			}
			if tc.source == "api" && tc.known && (rows[0].InputTokens != 10 || rows[0].CacheRead != 4) {
				t.Fatalf("API usage changed: %+v", rows[0])
			}
		})
	}
}

func TestCompleteMeteringIsCallScoped(t *testing.T) {
	r := Wrap(headerProvider{header: meteringHeader(sampleMetadata()), t: t}, nil, "alias", "profile", "", "", func() bool { return false })
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, u, err := r.Complete(context.Background(), llm.CompletionRequest{})
			if err != nil || u.InputTokens != 100 {
				t.Errorf("parallel usage=%+v err=%v", u, err)
			}
		}()
	}
	wg.Wait()
}

func TestDeferredMeteringSkipsOnlyValidatedPendingEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		envelope map[string]any
		wantRows int
	}{
		{"pending", map[string]any{"source": "codex_subscription", "known": false, "deferred": true, "samples": []any{}}, 0},
		{"unknown", map[string]any{"source": "codex_subscription", "known": false, "samples": []any{}}, 1},
		{"known deferred invalid", map[string]any{"source": "codex_subscription", "known": true, "deferred": true, "samples": []any{}}, 1},
		{"deferred missing samples invalid", map[string]any{"source": "codex_subscription", "known": false, "deferred": true}, 1},
		{"deferred wrong source invalid", map[string]any{"source": "other", "known": false, "deferred": true, "samples": []any{}}, 1},
		{"deferred samples invalid", func() map[string]any { v := sampleMetadata(); v["deferred"] = true; v["known"] = false; return v }(), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m := NewMetering(context.Background())
			m.CaptureHeaders(meteringHeader(tc.envelope))
			r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
			rows := r.usageRows("", 0, "session", "", llm.Usage{}, m.snapshot(), 1, "ok")
			if len(rows) != tc.wantRows {
				t.Fatalf("rows=%+v state=%+v", rows, m.snapshot())
			}
			if len(rows) > 0 && *rows[0].UsageKnown {
				t.Fatalf("invalid deferred metadata trusted: %+v", rows[0])
			}
		})
	}
}

func TestDeferredMeteringRetryCanBecomeTerminalOrUnknown(t *testing.T) {
	for _, terminal := range []http.Header{meteringHeader(sampleMetadata()), http.Header{"X-Artex-Usage": []string{"invalid"}}} {
		_, m := NewMetering(context.Background())
		pending := meteringHeader(map[string]any{"source": "codex_subscription", "known": false, "deferred": true, "samples": []any{}})
		m.CaptureHeaders(pending)
		m.CaptureHeaders(pending)
		m.CaptureHeaders(terminal)
		s := m.snapshot()
		if s.Deferred {
			t.Fatalf("terminal metadata still deferred: %+v", s)
		}
		r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
		if rows := r.usageRows("", 0, "", "", s.usage(), s, 1, "ok"); len(rows) != 1 {
			t.Fatalf("terminal row missing: %+v", rows)
		}
	}
}

func TestMissingInferenceIDsPreservePartialUsageAndDeduplicateRetries(t *testing.T) {
	_, m := NewMetering(context.Background())
	first := sampleMetadata()
	first["missing_ids"] = []string{"attempt-a", "attempt-b"}
	second := sampleMetadata()
	second["missing_ids"] = []string{"attempt-b", "attempt-c"}
	m.CaptureHeaders(meteringHeader(first))
	m.CaptureHeaders(meteringHeader(first))
	m.CaptureHeaders(meteringHeader(second))
	s := m.snapshot()
	r := Wrap(completeProvider{}, nil, "configured-model", "profile", "", "", nil)
	rows := r.usageRows("42", 12, "exp12-worker-i3", "worker", s.usage(), s, 7, "error")
	if len(rows) != 4 {
		t.Fatalf("expected sample and three missing attempts, got %+v", rows)
	}
	if rows[0].Model != "gpt-5.4" || !*rows[0].UsageKnown || rows[0].InputTokens != 100 {
		t.Fatalf("measured sample lost: %+v", rows[0])
	}
	for i, id := range []string{"attempt-a", "attempt-b", "attempt-c"} {
		row := rows[i+1]
		if row.MeteringID != id || *row.UsageKnown || row.Model != "configured-model" || row.UsageSource != "codex_subscription" || row.SessionID != "exp12-worker-i3" || row.InputTokens != 0 || row.OutputTokens != 0 || row.CacheRead != 0 || row.CacheWrite != 0 || row.ReasoningTokens != 0 {
			t.Fatalf("missing attempt row: %+v", row)
		}
	}
	if u := s.usage(); u.InputTokens != 100 || u.OutputTokens != 30 {
		t.Fatalf("partial totals lost: %+v", u)
	}
}

func TestMissingInferenceIDsRejectMalformedMetadata(t *testing.T) {
	oversized := make([]string, 1025)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("attempt-%d", i)
	}
	for _, tc := range []struct {
		name     string
		ids      any
		deferred bool
		known    bool
	}{
		{"blank", []string{" "}, false, true},
		{"duplicate", []string{"a", "a"}, false, true},
		{"sample collision", []string{"thread:1"}, false, true},
		{"nonstring", []any{12}, false, true},
		{"null", nil, false, true},
		{"oversized", oversized, false, true},
		{"legacy unknown", []string{"a"}, false, false},
		{"deferred", []string{"a"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := sampleMetadata()
			data["missing_ids"] = tc.ids
			data["known"] = tc.known
			if !tc.known {
				data["samples"] = []any{}
			}
			if tc.deferred {
				data["deferred"] = true
				data["samples"] = []any{}
			}
			_, m := NewMetering(context.Background())
			m.CaptureHeaders(meteringHeader(data))
			s := m.snapshot()
			if s.Known || s.Deferred || len(s.Samples) != 0 {
				t.Fatalf("invalid partial metadata accepted: %+v", s)
			}
		})
	}
}

func TestMissingInferenceWithoutSamplesProducesUnknownRow(t *testing.T) {
	_, m := NewMetering(context.Background())
	m.CaptureHeaders(meteringHeader(map[string]any{"source": "codex_subscription", "known": true, "samples": []any{}, "missing_ids": []string{"failed-attempt"}}))
	r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
	s := m.snapshot()
	rows := r.usageRows("", 0, "session", "", s.usage(), s, 1, "error")
	if len(rows) != 1 || *rows[0].UsageKnown || rows[0].MeteringID != "failed-attempt" {
		t.Fatalf("missing attempt suppressed: %+v", rows)
	}
}

func TestUnknownRetryDoesNotDiscardMeasuredSamples(t *testing.T) {
	_, m := NewMetering(context.Background())
	m.CaptureHeaders(meteringHeader(sampleMetadata()))
	m.CaptureHeaders(meteringHeader(map[string]any{"source": "codex_subscription", "known": false, "samples": []any{}}))
	r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
	s := m.snapshot()
	rows := r.usageRows("", 0, "session", "", s.usage(), s, 1, "error")
	if len(rows) != 2 || !*rows[0].UsageKnown || rows[0].InputTokens != 100 || *rows[1].UsageKnown {
		t.Fatalf("known usage discarded by unknown retry: %+v", rows)
	}
}

func TestMissingInferenceIDsRejectCrossAttemptSampleCollision(t *testing.T) {
	_, m := NewMetering(context.Background())
	m.CaptureHeaders(meteringHeader(map[string]any{"source": "codex_subscription", "known": true, "samples": []any{}, "missing_ids": []string{"thread:1"}}))
	m.CaptureHeaders(meteringHeader(sampleMetadata()))
	if s := m.snapshot(); s.Known || len(s.Samples) != 0 || len(s.MissingIDs) != 0 {
		t.Fatalf("cross-attempt ID collision accepted: %+v", s)
	}
}

func TestMissingInferenceSnapshotCannotMutateMetering(t *testing.T) {
	_, m := NewMetering(context.Background())
	m.CaptureHeaders(meteringHeader(map[string]any{"source": "codex_subscription", "known": true, "samples": []any{}, "missing_ids": []string{"attempt-a"}}))
	snapshot := m.snapshot()
	snapshot.MissingIDs[0] = "changed"
	if got := m.snapshot().MissingIDs[0]; got != "attempt-a" {
		t.Fatalf("snapshot mutated live state: %s", got)
	}
}
