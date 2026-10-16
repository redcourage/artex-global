package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
)

func TestRecorderMetersBridgeHeaderThroughNormaWithoutRecording(t *testing.T) {
	const metadata = `{"source":"codex_subscription","known":true,"samples":[{"id":"thread:17","model":"gpt-5.4","input_tokens":100,"output_tokens":30,"cache_read_tokens":40,"cache_write_tokens":10,"reasoning_tokens":20}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Artex-Usage", base64.StdEncoding.EncodeToString([]byte(metadata)))
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","model":"bridge-alias","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer srv.Close()
	cfg := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "test-key", Model: "bridge-alias"}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	recorder := llmrec.Wrap(provider, nil, "bridge-alias", "profile", "", "", func() bool { return false })
	msg, _, usage, err := recorder.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Text() != "hello" {
		t.Fatalf("message=%s", msg.Text())
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 30 || usage.CacheReadTokens != 40 || usage.CacheWriteTokens != 10 {
		t.Fatalf("usage=%+v; metadata lost in transport", usage)
	}
}

func TestRecorderPreservesOpenAIUsageDetailsWithoutRecording(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":100,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":20}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],"+usage+"}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"test","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+usage+`}`)
	}))
	defer srv.Close()
	cfg := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "test", Model: "gpt-5.4"}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	recorder := llmrec.Wrap(provider, nil, "gpt-5.4", "profile", "", "", func() bool { return false })
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}}}
	_, _, completeUsage, err := recorder.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var streamUsage llm.Usage
	for event, err := range recorder.Stream(context.Background(), req) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.SEMessageStart || event.Type == llm.SEMessageDelta {
			streamUsage.Add(event.Usage)
		}
		if event.Type == llm.SEMessageStop {
			break
		}
	}
	for _, u := range []llm.Usage{completeUsage, streamUsage} {
		if u.InputTokens != 100 || u.OutputTokens != 30 || u.CacheReadTokens != 40 || u.CacheWriteTokens != 10 {
			t.Fatalf("usage=%+v", u)
		}
	}
}

func TestRecorderPreservesBridgeUsageOnHTTPError(t *testing.T) {
	const metadata = `{"source":"codex_subscription","known":true,"samples":[{"id":"thread:error","model":"gpt-5.4","input_tokens":100,"output_tokens":30,"cache_read_tokens":40,"cache_write_tokens":10,"reasoning_tokens":20}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Artex-Usage", base64.StdEncoding.EncodeToString([]byte(metadata)))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"backend inference failed after usage was reported","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	cfg := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "test-key", Model: "bridge-alias"}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	recorder := llmrec.Wrap(provider, nil, "bridge-alias", "profile", "", "", func() bool { return false })
	_, _, usage, err := recorder.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}}})
	if err == nil {
		t.Fatal("HTTP400 unexpectedly succeeded")
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 30 || usage.CacheReadTokens != 40 || usage.CacheWriteTokens != 10 {
		t.Fatalf("usage lost on HTTP400: %+v", usage)
	}
}
