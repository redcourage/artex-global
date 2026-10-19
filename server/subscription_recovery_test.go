package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
)

func TestSubscriptionRouterParksBothCompletionModesWithoutFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		provider := &scriptedLLMProvider{err: &agent.SubscriptionWaiting{}}
		waits := 0
		hooks := taskLLMStreamHooks{
			current: func() (taskLLMSelection, error) { return taskLLMSelection{provider: provider, profileID: 1}, nil },
			wait:    func(_ taskLLMSelection, err error) error { waits++; return err },
			exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
				t.Fatal("paid failover called")
				return db.TaskLLMTransition{}, nil
			},
		}
		var err error
		if stream {
			_, err = collectTaskLLMStream(streamTaskLLM(context.Background(), "1", llm.CompletionRequest{}, hooks))
		} else {
			_, _, _, err = completeTaskLLM(context.Background(), "1", llm.CompletionRequest{}, hooks)
		}
		if !agent.IsSubscriptionLimit(err) || provider.calls != 1 || waits != 1 {
			t.Fatalf("stream=%v calls=%d waits=%d err=%v", stream, provider.calls, waits, err)
		}
	}
}

func TestSubscriptionRecoveryDurableAdmissionAndUserControls(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	var available atomic.Bool
	var probes atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/quota" || r.Header.Get("Authorization") != "Bearer local-test" {
			t.Errorf("invalid quota probe")
			w.WriteHeader(401)
			return
		}
		probes.Add(1)
		_ = json.NewEncoder(w).Encode(agent.SubscriptionStatus{Available: available.Load(), RetryAt: 1})
	}))
	defer endpoint.Close()
	dir := t.TempDir()
	m, err := NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close() }()
	task, err := m.CreateTask("subscription lifecycle", "keep working", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := parseTaskID(task.ID)
	defer func() { m.DeleteTask(task.ID, DeleteTaskOptions{}) }()
	iid, err := task.Store.AddIntent(map[string]any{"action": "resume exactly here"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.Config{Format: llm.FormatOpenAI, BaseURL: endpoint.URL + "/v1", APIKey: "local-test", Model: "fixture"}
	provider := &scriptedLLMProvider{err: &agent.SubscriptionWaiting{}}
	makeServer := func() *Server {
		return &Server{m: m, engine: NewEngine(m), llmOn: true, llmProv: provider, llmCfg: cfg}
	}
	s := makeServer()
	runtime := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: "worker"}
	if _, _, _, err = runtime.Complete(context.Background(), llm.CompletionRequest{}); !agent.IsSubscriptionLimit(err) {
		t.Fatal(err)
	}
	if !s.engine.subscriptionWaiting(task.ID) {
		t.Fatal("missing durable gate")
	}
	if _, _, _, err = runtime.Complete(context.Background(), llm.CompletionRequest{}); !agent.IsSubscriptionLimit(err) || provider.calls != 1 {
		t.Fatalf("gate called provider: calls=%d err=%v", provider.calls, err)
	}
	if s.engine.runWorkerStep(context.Background(), task, "test", nil) {
		t.Fatal("worker claimed while waiting")
	}
	node, _ := task.Store.GetNode(iid)
	if node.State != "open" {
		t.Fatal(node.State)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	m.LoadExisting()
	s = makeServer()
	if !s.engine.subscriptionWaiting(task.ID) {
		t.Fatal("restart lost gate")
	}
	due := func() {
		t.Helper()
		waits, e := m.pg.SubscriptionWaits(taskID)
		if e != nil || len(waits) != 1 {
			t.Fatalf("waits=%v err=%v", waits, e)
		}
		if e = m.pg.DeferSubscriptionCheck(waits[0], time.Now().Add(-time.Second)); e != nil {
			t.Fatal(e)
		}
	}
	due()
	s.checkSubscriptionWaits(context.Background())
	if !s.engine.subscriptionWaiting(task.ID) || probes.Load() != 1 {
		t.Fatal("past reset time falsely resumed")
	}
	available.Store(true)
	if err = m.SetTaskPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	due()
	s.checkSubscriptionWaits(context.Background())
	if probes.Load() != 1 || !s.engine.subscriptionWaiting(task.ID) {
		t.Fatal("user pause overridden")
	}
	if err = m.SetTaskPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	s.llmCfg.APIKey = "different-account"
	s.checkSubscriptionWaits(context.Background())
	if probes.Load() != 1 || !s.engine.subscriptionWaiting(task.ID) {
		t.Fatal("changed config used for recovery")
	}
	s.llmCfg = cfg
	due()
	s.checkSubscriptionWaits(context.Background())
	if s.engine.subscriptionWaiting(task.ID) || probes.Load() != 2 {
		t.Fatal("confirmed recovery did not clear gate")
	}
	provider.err = nil
	runtime = &taskLLMRuntime{s: s, taskID: task.ID, agentKey: "worker"}
	if _, _, _, err = runtime.Complete(context.Background(), llm.CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 {
		t.Fatal(provider.calls)
	}
	// A global/bound pool may have selected a different member before reaching
	// a subscription limit. Poll that source, never the nominal head profile.
	profileID, err := m.pg.SaveProfile(&db.LLMProfile{Name: "quota-pool-fixture", Format: "openai", BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model})
	if err != nil {
		t.Fatal(err)
	}
	defer m.pg.DeleteProfile(profileID)
	sourceCfg, ok := s.loadProfileConfig(profileID)
	if !ok {
		t.Fatal("source config missing")
	}
	cause := &llmpool.MemberError{ProfileID: profileID, Fingerprint: subscriptionFingerprint(sourceCfg), Cause: &agent.SubscriptionWaiting{}}
	if err = runtime.waitForSubscription(taskLLMSelection{config: agent.Config{BaseURL: "http://wrong-head.invalid/v1"}}, cause); !agent.IsSubscriptionLimit(err) {
		t.Fatal(err)
	}
	due()
	s.checkSubscriptionWaits(context.Background())
	if s.engine.subscriptionWaiting(task.ID) || probes.Load() != 3 {
		t.Fatal("recovery did not probe actual pool member")
	}
}
