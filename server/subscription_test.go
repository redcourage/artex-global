package server

import (
	"errors"
	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/harness"
	"testing"
)

func TestSubscriptionWaitNeverRetriesOrRotatesPaidProfiles(t *testing.T) {
	err := &agent.SubscriptionWaiting{}
	if isRetryableStreamError(err) {
		t.Fatal("subscription wait entered same-provider retries")
	}
	if retryableWorkerModelError(harness.ReasonModelError, err) {
		t.Fatal("subscription wait restarts worker")
	}
	if isQuotaExhaustedError(err) {
		t.Fatal("subscription wait advances paid chain")
	}
	if agent.IsSubscriptionLimit(errors.New("network timeout")) {
		t.Fatal("network classified as subscription")
	}
	review := errors.New(`openai: status 409: {"error":{"code":"tool_resume_review_required"}}`)
	if isRetryableStreamError(review) || retryableWorkerModelError(harness.ReasonModelError, review) {
		t.Fatal("ambiguous external action must not be retried")
	}
}
