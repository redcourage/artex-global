package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Real ARTEX Engine -> Worker -> Norma -> HTTP, plus a real PostgreSQL wait and
// on-disk transcript. Only the remote model is scripted; no subscription used.
func TestSubscriptionWorkerResumesPersistedToolResultAfterRestart(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	var calls, writes atomic.Int64
	var recovered atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/quota" {
			_ = json.NewEncoder(w).Encode(agent.SubscriptionStatus{Available: recovered.Load()})
			return
		}
		data, _ := io.ReadAll(r.Body)
		n := calls.Add(1)
		if n == 1 {
			io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"fixture-write","type":"function","function":{"name":"QuotaFixtureWrite","arguments":"{}"}}]}}]}`)
			return
		}
		if !strings.Contains(string(data), "fixture-write-completed") {
			t.Error("completed tool result lost")
		}
		if !recovered.Load() {
			w.WriteHeader(423)
			io.WriteString(w, `{"error":{"code":"codex_subscription_limit","message":"subscription waiting"}}`)
			return
		}
		if !strings.Contains(string(data), "ARTEX_AUTOMATIC_CONTINUATION") {
			t.Error("resume was not marked as continuation")
		}
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"finished from saved result"}}]}`)
	}))
	defer endpoint.Close()
	dir := t.TempDir()
	m, err := NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close() }()
	task, err := m.CreateTask("worker quota fixture", "finish fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := task.ID
	defer func() { m.DeleteTask(id, DeleteTaskOptions{}) }()
	iid, err := task.Store.AddIntent(map[string]any{"action": "call QuotaFixtureWrite once"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.Config{Format: llm.FormatOpenAI, BaseURL: endpoint.URL + "/v1", APIKey: "local-test", Model: "fixture"}
	prov, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	makeRuntime := func() (*Server, *agent.Worker) {
		s := &Server{m: m, engine: NewEngine(m), llmOn: true, llmProv: prov, llmCfg: cfg}
		runtime := &taskLLMRuntime{s: s, taskID: id, agentKey: "worker"}
		fixture := tool.Build(tool.Spec{Name: "QuotaFixtureWrite", Description: "Fixture write", Schema: map[string]any{"type": "object"},
			Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				writes.Add(1)
				return tool.Text("fixture-write-completed"), nil
			}})
		worker := agent.NewWorker(runtime, "fixture", dir, transcript.NewStore(filepath.Join(dir, "transcripts")), 200000, 10, fixture)
		worker.SetNonStreaming(func() bool { return true })
		return s, worker
	}
	s, worker := makeRuntime()
	if !s.engine.runWorkerStep(context.Background(), task, "fixture", worker) {
		t.Fatal("nothing claimed")
	}
	node, _ := task.Store.GetNode(iid)
	if node.State != "open" || writes.Load() != 1 || calls.Load() != 2 || !s.engine.subscriptionWaiting(id) {
		t.Fatalf("state=%s writes=%d calls=%d waiting=%v", node.State, writes.Load(), calls.Load(), s.engine.subscriptionWaiting(id))
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	m.LoadExisting() // registry restore used by Server.restoreTaskRuntimes
	var ok bool
	task, ok = m.Task(id)
	if !ok {
		t.Fatal("task lost on restart")
	}
	s, worker = makeRuntime()
	if s.engine.runWorkerStep(context.Background(), task, "fixture", worker) {
		t.Fatal("claimed before recovery")
	}
	recovered.Store(true)
	taskID, _ := parseTaskID(id)
	waits, err := m.pg.SubscriptionWaits(taskID)
	if err != nil || len(waits) != 1 {
		t.Fatalf("waits=%v err=%v", waits, err)
	}
	if err = m.pg.DeferSubscriptionCheck(waits[0], time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	s.checkSubscriptionWaits(context.Background())
	if !s.engine.runWorkerStep(context.Background(), task, "fixture", worker) {
		t.Fatal("did not resume")
	}
	node, _ = task.Store.GetNode(iid)
	if node.State != "done" || writes.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("state=%s writes=%d calls=%d", node.State, writes.Load(), calls.Load())
	}
	// Simulate a crash after dispatching another action but before its result was
	// persisted. Norma normally drops that trailing call; ARTEX must inspect the
	// raw transcript first and park the intent without any model/tool retry.
	store := transcript.NewStore(filepath.Join(dir, "transcripts"))
	sid := agent.WorkerSessionID(task.Store.ID(), iid)
	records, err := store.Load(store.MainPath(sid))
	if err != nil {
		t.Fatal(err)
	}
	writer := store.ResumeWriter(sid, transcript.LastUUID(records))
	writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "uncertain-write", Name: "QuotaFixtureWrite", Input: json.RawMessage(`{}`)}}}, llm.Usage{})
	if err = writer.Err(); err != nil {
		t.Fatal(err)
	}
	if err = task.Store.SetIntentState(iid, "open"); err != nil {
		t.Fatal(err)
	}
	if !s.engine.runWorkerStep(context.Background(), task, "fixture", worker) {
		t.Fatal("review case not claimed")
	}
	node, _ = task.Store.GetNode(iid)
	if node.State != "paused" || writes.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("uncertain action retried: state=%s writes=%d calls=%d", node.State, writes.Load(), calls.Load())
	}
}
