package agent

import (
	"errors"
	"github.com/Autumn-27/norma/llm"
	"testing"
)

func TestSubscriptionLimitClassification(t *testing.T) {
	for _, raw := range []string{`openai: status 429: rate limit`, `openai: status 402: insufficient_quota`, `openai: status 409: conflict`} {
		if IsSubscriptionLimit(errors.New(raw)) {
			t.Fatal(raw)
		}
	}
	if !IsSubscriptionLimit(errors.New(`openai: status 423: {"error":{"code":"codex_subscription_limit"}}`)) {
		t.Fatal("lost subscription error")
	}
}

func TestResumeRequiresCompletedToolResults(t *testing.T) {
	messages := []llm.Message{{Role: "assistant", Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "write1", Name: "Write"}}}}
	if SafeToolResume(messages) == nil {
		t.Fatal("uncertain write accepted")
	}
	messages = append(messages, llm.Message{Role: "user", Content: []llm.ContentBlock{llm.ToolResultText("write1", "saved", false)}})
	if err := SafeToolResume(messages); err != nil {
		t.Fatal(err)
	}
	messages[1].Content[0].IsError = true
	if SafeToolResume(messages) == nil {
		t.Fatal("failed/possibly interrupted write accepted")
	}
	if !NeedsToolReview(SafeToolResume(messages)) {
		t.Fatal("unsafe resume must require review, not a model retry")
	}
}
