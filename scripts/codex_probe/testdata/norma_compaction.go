// Deterministic integration client: real Norma compaction and OpenAI encoding,
// with synthetic tool observations and no external model or DB.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type headers struct{}

func (headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Artex-Codex-Session", transcript.SessionIDFrom(r.Context()))
	return http.DefaultTransport.RoundTrip(r)
}

func main() {
	p, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: os.Args[1],
		APIKey: "local-test", Model: "test-model", MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headers{}, Timeout: 15 * time.Second}})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, scenario := range []struct {
		name         string
		size, rounds int
	}{
		{"micro", 25000, 30}, {"full", 120000, 10},
	} {
		c := compaction.New(compaction.Config{ContextWindow: 200000},
			compaction.ProviderSummarizer(p, 0, true))
		msgs := []llm.Message{llm.UserText("Read synthetic observations and retain their sequence.")}
		requestCtx := transcript.WithSessionID(ctx, scenario.name)
		compacted := false
		for n := 0; n < scenario.rounds; n++ {
			msgs = c.Pre(requestCtx, msgs, 0)
			compacted = compacted || llm.LastBoundaryIndex(msgs) >= 0
			message, _, _, err := p.Complete(requestCtx, llm.CompletionRequest{
				System: []string{"Fixture worker"}, Messages: llm.MessagesForAPI(msgs),
				Tools: []llm.ToolSchema{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
			})
			if err != nil {
				panic(err)
			}
			msgs = append(msgs, message)
			if len(message.ToolUses()) == 0 {
				if scenario.name != "full" || !compacted || message.Text() != "restored" {
					panic("unexpected terminal response")
				}
				break
			}
			for _, call := range message.ToolUses() {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
					Type: llm.BlockToolResult, ToolUseID: call.ID,
					Content: []llm.ContentBlock{llm.TextBlock(strings.Repeat("x", scenario.size))},
				}}})
			}
		}
		if scenario.name == "full" && !compacted {
			panic("full compaction was not exercised")
		}
		fmt.Printf("%s: real Norma compaction/HTTP passed\n", scenario.name)
	}
}
