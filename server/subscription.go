package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
)

// Bind recovery to the exact bridge credentials/endpoint/model of the failed call.
// The bridge's underlying ChatGPT login is not identified by this fingerprint.
// Never persist bearer tokens. Changing configuration requires explicit review.
func subscriptionFingerprint(cfg agent.Config) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", cfg.Format, cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Proxy))))
}

func (r *taskLLMRuntime) waitForSubscription(sel taskLLMSelection, cause error) error {
	id, _ := parseTaskID(r.taskID)
	fingerprint, profileID := subscriptionFingerprint(sel.config), int64(0)
	var memberErr *llmpool.MemberError
	if errors.As(cause, &memberErr) {
		fingerprint, profileID = memberErr.Fingerprint, memberErr.ProfileID
	}
	if err := r.s.m.pg.WaitForSubscription(id, r.agentKey, fingerprint, time.Now().Add(time.Minute), profileID); err != nil {
		// Persistence failure is NOT a resumable wait; never silently lose the task.
		return &taskLLMError{taskID: r.taskID, cause: fmt.Errorf("persist subscription wait: %w", err)}
	}
	if sel.task != nil {
		r.s.engine.emitActivity(sel.task, db.Activity{Worker: "system", Kind: "subscription_wait", Summary: agent.SubscriptionWaitMessage})
	}
	return cause
}

func (e *Engine) subscriptionWaiting(id string) bool {
	if e.m == nil || e.m.pg == nil {
		return false
	}
	n, err := parseTaskID(id)
	if err != nil {
		return true
	}
	waiting, err := e.m.pg.HasSubscriptionWait(n)
	return err != nil || waiting // Fail closed when durable state cannot be read.
}

func (s *Server) runSubscriptionRecovery(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkSubscriptionWaits(ctx)
		}
	}
}

func (s *Server) checkSubscriptionWaits(ctx context.Context) {
	waits, err := s.m.pg.SubscriptionWaits(0)
	if err != nil {
		return
	}
	for _, w := range waits {
		if ctx.Err() != nil {
			return
		}
		if time.Now().Before(w.NextCheck) {
			continue
		}
		pt, err := s.m.pg.GetTask(w.TaskID)
		if err != nil || pt == nil || db.IsTerminal(pt.Status) || pt.Paused {
			continue
		}
		id := strconv.FormatInt(w.TaskID, 10)
		runtime := taskLLMRuntime{s: s, taskID: id, agentKey: w.AgentKey}
		cfg, ok := runtime.activeCfg()
		if w.ProfileID > 0 {
			cfg, ok = s.loadProfileConfig(w.ProfileID)
		}
		// A missing/changed profile, auth error, stale/unknown status or unreachable
		// bridge leaves the wait intact; none is evidence of quota recovery.
		next := time.Now().Add(time.Minute)
		if ok && subscriptionFingerprint(cfg) == w.Fingerprint {
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			status, probeErr := cfg.CheckSubscription(probeCtx)
			cancel()
			if probeErr == nil && status.Available {
				cleared, clearErr := s.m.pg.ClearSubscriptionWait(w)
				if clearErr == nil && cleared {
					if task, ok := s.m.Task(id); ok {
						s.engine.emitActivity(task, db.Activity{Worker: "system", Kind: "subscription_recovered", Summary: "Codex 구독 한도 회복을 확인했습니다. 미완료 작업을 재개합니다."})
						task.Notify() // Does not unpause or revive a terminal task.
					}
				}
				continue
			}
			if probeErr == nil && status.RetryAt > next.Unix() {
				next = time.Unix(status.RetryAt, 0)
			}
		}
		if err := s.m.pg.DeferSubscriptionCheck(w, next); err != nil {
			log.Printf("[quota] task %s: failed to schedule recovery check", id)
		}
	}
}
