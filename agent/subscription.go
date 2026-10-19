package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

const SubscriptionWaitMessage = "Codex 구독 한도 회복을 기다리고 있습니다. 사용 가능 여부를 확인하면 작업을 자동 재개합니다."
const AutomaticContinuation = "[ARTEX_AUTOMATIC_CONTINUATION] Continue unfinished work using the recorded tool results. Do not repeat completed actions."

// Norma returns HTTP errors as text; require the bridge's structured error code.
func IsSubscriptionLimit(err error) bool {
	return bridgeErrorCode(err) == "codex_subscription_limit"
}

func bridgeErrorCode(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	i := strings.Index(raw, "{\"error\"")
	if i < 0 {
		return ""
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		}
	}
	if json.Unmarshal([]byte(raw[i:]), &body) != nil {
		return ""
	}
	return body.Error.Code
}

type ToolReviewRequired struct{ Reason string }

func (e *ToolReviewRequired) Error() string {
	return "도구 실행 결과 검토가 필요합니다: " + e.Reason
}
func NeedsToolReview(err error) bool {
	var review *ToolReviewRequired
	return errors.As(err, &review) || bridgeErrorCode(err) == "tool_resume_review_required"
}

type SubscriptionWaiting struct{}

func (*SubscriptionWaiting) Error() string {
	return `{"error":{"code":"codex_subscription_limit","message":"` + SubscriptionWaitMessage + `"}}`
}

type SubscriptionStatus struct {
	Available bool  `json:"available"`
	RetryAt   int64 `json:"retry_at"`
}

// CheckSubscription only reads account limits. It never runs inference or
// redeems credits. Redirects cannot send the local bearer token elsewhere.
func (c Config) CheckSubscription(ctx context.Context) (SubscriptionStatus, error) {
	var result SubscriptionStatus
	client, err := quotaAwareHTTPClient(c.Proxy, "")
	if err != nil {
		return result, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/quota", nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("quota endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("quota endpoint status %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result)
	return result, err
}

// Inspect the raw transcript BEFORE Norma drops unanswered calls on Resume.
// Error results can describe an interrupted write, so automatic recovery is
// conservative; a human can inspect the trace and explicitly continue it.
func SafeToolResume(messages []llm.Message) error {
	pending := map[string]bool{}
	for _, m := range messages {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				pending[b.ID] = true
			case llm.BlockToolResult:
				if b.IsError {
					return &ToolReviewRequired{Reason: fmt.Sprintf("tool %s has an unsuccessful result", b.ToolUseID)}
				}
				delete(pending, b.ToolUseID)
			}
		}
	}
	if len(pending) > 0 {
		return &ToolReviewRequired{Reason: fmt.Sprintf("%d tool result(s) were not persisted", len(pending))}
	}
	return nil
}
