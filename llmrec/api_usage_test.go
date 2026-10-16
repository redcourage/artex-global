package llmrec

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func observeAPI(t *testing.T, body, contentType string) meteringSnapshot {
	t.Helper()
	_, m := NewMetering(context.Background())
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
	resp.Body = m.ObserveResponse(resp)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return m.snapshot()
}

func TestAPIObserverUsageOnlyJSONAndSSE(t *testing.T) {
	for _, tc := range []struct{ name, body, contentType string }{
		{"json", `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":100,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":20}}}`, "application/json"},
		{"sse", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":30,\"cache_write_tokens\":10,\"completion_tokens_details\":{\"reasoning_tokens\":20}}}\n\ndata: [DONE]\n\n", "text/event-stream"},
		{"responses", `{"response":{"usage":{"input_tokens":100,"output_tokens":30,"input_tokens_details":{"cache_write_tokens":10},"output_tokens_details":{"reasoning_tokens":20}}}}`, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observeAPI(t, tc.body, tc.contentType)
			u := s.applyAPI(llm.Usage{InputTokens: 100, OutputTokens: 30, CacheReadTokens: 40})
			if u.CacheWriteTokens != 10 || u.InputTokens != 100 || u.OutputTokens != 30 || u.CacheReadTokens != 40 {
				t.Fatalf("usage=%+v", u)
			}
			r := Wrap(completeProvider{}, nil, "model", "profile", "", "", nil)
			rows := r.usageRows("", 0, "chat", "", u, s, 1, "ok")
			if len(rows) != 1 || rows[0].ReasoningTokens != 20 || rows[0].UsageKnown == nil || !*rows[0].UsageKnown {
				t.Fatalf("rows=%+v", rows)
			}
		})
	}
}

func TestAPIObserverKnownZeroMissingAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		known                   bool
	}{
		{"zero", `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, "application/json", true},
		{"missing", `{"choices":[]}`, "application/json", false},
		{"null usage", `{"usage":null}`, "application/json", false},
		{"invalid counter", `{"usage":{"prompt_tokens":1,"cache_write_tokens":-1}}`, "application/json", false},
		{"large json", `{"text":"` + strings.Repeat("x", maxAPIUsageBytes) + `","usage":{"prompt_tokens":1}}`, "application/json", false},
		{"large frame", "data: {\"text\":\"" + strings.Repeat("x", maxAPIUsageBytes) + "\"}\n\ndata: {\"usage\":{\"prompt_tokens\":1}}\n\n", "text/event-stream", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observeAPI(t, tc.body, tc.contentType)
			r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
			rows := r.usageRows("", 0, "", "", llm.Usage{}, s, 1, "ok")
			if len(rows) != 1 || rows[0].UsageKnown == nil || *rows[0].UsageKnown != tc.known {
				t.Fatalf("rows=%+v metadata=%+v", rows, s)
			}
		})
	}
}

func TestAPIObserverChunkBoundariesAndRetryReset(t *testing.T) {
	const wire = "data: {\"usage\":{\"prompt_tokens\":100,\"cache_write_tokens\":10}}\r\n\r\ndata: [DONE]\r\n\r\n"
	for size := 1; size <= len(wire); size++ {
		o := &apiUsageObserver{streaming: true}
		for offset := 0; offset < len(wire); offset += size {
			end := min(offset+size, len(wire))
			o.feed([]byte(wire[offset:end]), end == len(wire))
		}
		s := o.snapshot()
		if s.Incomplete || s.CacheWrite == nil || *s.CacheWrite != 10 {
			t.Fatalf("size=%d snapshot=%+v", size, s)
		}
	}
	_, m := NewMetering(context.Background())
	for _, body := range []string{`{"usage":{"prompt_tokens":100,"cache_write_tokens":10}}`, `{"usage":{"prompt_tokens":7}}`} {
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		observed := m.ObserveResponse(resp)
		_, _ = io.Copy(io.Discard, observed)
		_ = observed.Close()
	}
	if s := m.snapshot(); s.API.CacheWrite != nil || s.API.Input == nil {
		t.Fatalf("retry retained previous usage: %+v", s)
	}
}

func TestAPIObserverDoesNotAddExistingSDKCacheWrites(t *testing.T) {
	s := observeAPI(t, `{"usage":{"input_tokens":50,"output_tokens":3,"cache_creation_input_tokens":10,"cache_read_input_tokens":40}}`, "application/json")
	u := s.applyAPI(llm.Usage{InputTokens: 100, OutputTokens: 3, CacheReadTokens: 40, CacheWriteTokens: 10})
	if u.CacheWriteTokens != 10 || u.InputTokens != 100 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestAPIObserverOversizedCRLFFrameRecovers(t *testing.T) {
	s := observeAPI(t, "data: {\"text\":\""+strings.Repeat("x", maxAPIUsageBytes)+"\"}\r\n\r\ndata: {\"usage\":{\"prompt_tokens\":1,\"cache_write_tokens\":0}}\r\n\r\n", "text/event-stream")
	if !s.API.Incomplete || s.API.Input == nil {
		t.Fatalf("bounded parser failed to resume after frame: %+v", s.API)
	}
}

func TestAPIObserverPartialCountersAreNotKnownZero(t *testing.T) {
	for _, body := range []string{
		`{"usage":{"reasoning_tokens":0}}`,
		`{"usage":{"cache_write_tokens":0}}`,
		`{"usage":{"prompt_tokens":0}}`,
		`{"usage":{"completion_tokens":0}}`,
	} {
		s := observeAPI(t, body, "application/json")
		r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
		rows := r.usageRows("", 0, "", "", llm.Usage{}, s, 1, "ok")
		if *rows[0].UsageKnown {
			t.Fatalf("partial usage priced as zero: %s", body)
		}
	}
}

func TestAPIObserverReadAheadIsNotKnownZero(t *testing.T) {
	_, m := NewMetering(context.Background())
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":30}}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	observed := m.ObserveResponse(resp)
	// One underlying read sees both frames while the SDK consumer stops at the
	// first content event, before it can deliver any normalized usage event.
	buffer := make([]byte, len(body))
	if n, err := observed.Read(buffer); n != len(body) || err != nil {
		t.Fatalf("read=%d,%v", n, err)
	}
	_ = observed.Close()
	r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
	rows := r.usageRows("", 0, "", "", llm.Usage{}, m.snapshot(), 1, "ok")
	if *rows[0].UsageKnown && rows[0].InputTokens == 0 && rows[0].OutputTokens == 0 {
		t.Fatalf("read-ahead became priced zero: %+v", rows[0])
	}
}

func TestAPIObserverAnthropicSplitReportsMainCounters(t *testing.T) {
	s := observeAPI(t, "data: {\"message\":{\"usage\":{\"input_tokens\":50,\"cache_read_input_tokens\":40,\"cache_creation_input_tokens\":10}}}\n\ndata: {\"usage\":{\"output_tokens\":3}}\n\n", "text/event-stream")
	r := Wrap(completeProvider{}, nil, "model", "", "", "", nil)
	rows := r.usageRows("", 0, "", "", llm.Usage{InputTokens: 100, OutputTokens: 3, CacheReadTokens: 40, CacheWriteTokens: 10}, s, 1, "ok")
	if !*rows[0].UsageKnown {
		t.Fatalf("split usage not known: %+v", rows[0])
	}
}
