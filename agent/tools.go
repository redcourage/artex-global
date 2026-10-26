package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 상위: 이 의도가 파생된 노드(여러 사실이 의도 하나를 함께 생성할 수 있음)
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 하위: 이 의도가 생성한 사실/발견
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "사용자가 목표 N개 추가: …" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "사용자가 전략 힌트 N개 추가: …"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "사실1 자산25 취약점0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("사실%d 자산%d 취약점%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own 범위 정의 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/사용자 정의 agent/별도 질문). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + "에는 작업 문맥(탐색 그래프)이 필요하다. 현재 agent가 작업 안에서 실행 중이 아니므로 탐색 그래프를 얻을 수 없어 이 도구를 사용할 수 없다. 작업 안에서 사용하거나 task_id가 있는 작업 간 읽기 도구(get_task_node_detail / list_task_findings / get_task_graph 등)를 사용한다."), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(탐색 연결 그래프) 탐색 상황 요약: 자산 수, API 없는 사이트, frontier, 발견, hints(사람/주 agent의 전략 힌트, 의도 생성 시 반영 필수). 계획 시 먼저 호출한다.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 사람/주 agent가 add_hint로 그래프에 연결한 전략 힌트. 함께 포함하여
	// planner가 의도 생성 시 매번 읽게 한다(기록만 하고 읽지 않는 상황 방지).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC — 우선순위가 가장 높은 N개. 실제 전체 수는 frontier_open
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 최신순(all은 id 내림차순), 접힌 항목 제외. 출력 시 최신 N개로 자름
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total: 종료된 의도(done/blocked/exhausted) 전체 수. recent_done_intents와
	// 나란한 이름이며 후자는 최신 구간만 잘라 보여준다. 두 키로 표시 수/전체 수를 알 수 있어
	// planner가 중복 제거 시 표시되지 않은 것을 배정하지 않은 것으로 오인하지 않으며 별도 프롬프트 설명이 불필요하다.
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open: 열린 의도의 실제 전체 수(open_intents는 우선순위 높은 N개만 잘라 보여줌).
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 확인된 취약점 전체 수(목표 판단 기준). 최신 구간 상세는 finding_list
	out["facts"] = len(factNodes)                      // 탐색 사실/결론 수(부정 결론 포함)
	// findings는 작업의 가장 가치 높은 산출물이므로 개요에 최신 구간을 포함한다(≤10개, vulnNodes는 id 내림차순).
	// planner가 매번 목표 판단 시 최근 확인된 취약점을 바로 볼 수 있다. 전체/이전 항목은 list_findings로 조회한다.
	// 각 항목은 {id, summary, from_intent?}만 유지한다. from_intent는 해당 취약점을 만든 의도다.
	// evidence/assets/vulnclass/severity/state 등은 list_findings / node_detail(id)로 조회할 수 있다.
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 이 취약점을 생성한 의도
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts: 접히지 않은 사실의 최신 구간(≤N, factNodes는 id 내림차순). 이미 digest에 접혀
	// 여전히 cold인 hidden 항목은 cold_digests로 보내며 중복하지 않는다. 항목은 {id, summary, from_intent?,
	// confidence?}. evidence 등 상세는 node_detail(id), 이전 항목은 list_facts로 조회한다.
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // 이미 digest에 접혀 여전히 cold임. cold_digests 참조
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 이 사실을 생성한 의도
		}
		// confidence를 개요에 포함하여 계획자가 추론뿐인 결론을 바로 식별하게 한다(특히 부정 결론을
		// 확정 사실로 보지 않도록). 긴 evidence는 node_detail(id)에 둔다.
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents: 접히지 않은 종료 의도의 최신 구간(≤N, recentDone은 id 내림차순).
	// 이전 항목은 done_intents_total 수 + node_detail(id)로 확인한다.
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 접힌 cold 영역의 digest body를 최신 구성원 시각 내림차순으로 N개 조회한다. 잘린 오래된 digest는
	// id만 제공하며 expand_digest로 펼칠 수 있어 cold 영역의 유일한 출구가 무한히 길어지는 것을 막는다.
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] — body를 직접 읽음(§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 잘린 오래된 digest의 id. expand_digest(id)로 펼침
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage: 대략적인 자산 테스트 커버리지 참고값. task_scope 안 자산 중 fact가 다룬
	// 비율 + by_type(유형별 전체/검사 수). 구체적 미검사 자산은 필요 시 list_untested_assets로 조회하고 직접 판단한다. 작업 문맥 전용.
	// 커버리지 기능이 꺼져 있으면(coverageDisabled) host_count(대상 호스트 수 인식 정보)만 유지하고
	// denominator/tested/pct/by_type/note 등 측정값은 버려 문맥 오염과 숨겨진 도구
	// add_task_scope/list_untested_assets의 사용 유도를 피한다.
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage 자산 테스트 커버리지(API 등 관련 자산 포함)는 참고용 대략 추정치다. 현재 작업과 직접 연결된 작업의 scope 및 사실 기준점을 포함하며 연결 scope는 읽기 전용이다. 컨테이너형 자산/대량 열거로 낮아질 수 있으므로 테스트 완료로 판단하지 않는다. add_task_scope로 이 작업 범위를 보완하고 list_untested_assets로 미검사 자산을 확인할 수 있다【보통 list_untested_assets는 호출하지 않고 작업 진행에 따른다】;"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "범위 기준점 없음"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// 호스트 전체 수만 제공하고 graph_overview에 목록을 나열하지 않는다. 넓은 범위 작업에서는 매 라운드
				// 반복되는 문자열이 많고 계획 판단 가치는 제한적이다. 구체적 호스트는 필요 시 list_assets로 조회한다.
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 잘린 오래된 digest의 id. expand_digest(id)로 펼침
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "이 작업과 직접 연결된 작업의 【확인된 취약점】 목록(간결: id+task_id+intent_id+vulnclass+severity+요약+상태). 연결 작업 항목에는 source_task_id/inherited=true가 붙으며 읽기 전용이다. 취약점만 포함하며 일반 탐색 사실은 list_facts, 상세는 node_detail(id)를 사용한다.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> 생성한 intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "이 작업과 직접 연결된 작업의 【탐색 사실/결론】을 최신순으로 페이지별 조회한다(간결: id+요약+상태. 긴 요약은 잘리며 전문은 node_detail(id)). 모든 매개변수 선택: limit(기본 20, 최대 100), before(커서, 이전 페이지 next_before를 전달하여 더 오래된 페이지 조회; 생략/0=최신), q(요약 키워드 필터). 반환 {facts, total, has_more, next_before}: total은 필터 후 전체 수, has_more=true이면 next_before로 계속 조회. 연결 작업 항목은 source_task_id/inherited=true이며 읽기 전용. 취약점은 list_findings.",
		obj(map[string]any{
			"limit":  intp("반환 수, 기본 20, 최대 100"),
			"before": intp("페이지 커서: 이 값보다 id가 작은 오래된 사실만 반환. 생략 또는 0=최신 페이지"),
			"q":      str("사실 요약 키워드 필터(대소문자 구분 없음). 생략=필터 없음"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // 이 값을 다시 전달하여 더 오래된 다음 페이지 조회
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "id로 이 작업 또는 직접 연결된 작업의 【탐색 그래프 노드】 전체 내용을 조회한다. 상속 노드는 source_task_id/inherited=true이며 읽기 전용. list_facts/list_findings/graph_overview가 반환한 탐색 노드 id만 사용한다. 자산은 list_assets/asset_neighbors 사용.",
		obj(map[string]any{"id": idp("탐색 그래프 노드 id(자산 id 아님)")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id 필수"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("탐색 노드 %d를 찾지 못했다. 자산을 조회하려면 list_assets / asset_neighbors를 사용한다(자산과 탐색 노드는 서로 다른 id 공간이므로 자산 id를 node_detail에 전달할 수 없음).", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem은 add_intent 일괄/단일 입력의 탐색 방향 하나다.
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent는 의도 노드를 만들고 상위 계보에 연결하여 id를 반환한다.
// 제약: 의도는 확인된 지식에만 연결한다. 각 parent_id는 기존 fact/finding
// 노드여야 한다(다른 의도/목표/힌트는 금지). 최상위 새 방향은 parent_ids를 비워 origin fact에 연결한다.
// 생성 경로에서 모든 의도의 사실 연결과 발견 주도 계획을 강제한다.
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary는 비어 있을 수 없음")
	}
	// 노드 생성 전에 기준점을 검증하여 잘못된 기준점이 고아 의도를 남기지 않게 한다.
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d는 이 작업이나 직접 연결된 작업에 없음: parent_ids는 기존 【사실(fact)/발견(finding)】 노드 id여야 함. 최상위 새 방향은 parent_ids를 비워 둔다", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d는 %q 노드이므로 의도 기준점으로 쓸 수 없음. 의도는 확인된 【사실(fact)/발견(finding)】에만 연결할 수 있고 의도/목표/힌트에는 연결할 수 없다. 최상위 새 방향은 parent_ids를 비워 둔다", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 자산 차단: 연결 자산이 시스템 자산 차단 규칙에 일치하면 의도 배정을 금지한다.
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("자산 차단 검증 실패: %w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "의도 “%s”에 연결된 자산이 테스트 범위 검증을 통과하지 못했다. 관련 자산 테스트를 중지한다: ", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【탐색 방향】을 생성해 frontier에 쓰고 탐색 연결에 포함한다. 의도는 고정 유형이 아닌 열린 탐색 방향이다. summary 한 문장으로 무엇을 탐색/검증/악용할지 자유롭게 설명한다.\n"+
		"★일괄 우선: 라운드에서 고른 여러 새 방향은 intents 배열로 한 번에 제출한다(개별 호출보다 왕복 감소). 반환 ids는 intents와 길이/순서가 같다(실패 항목 id=0, 상세 errors). 단일 항목은 intents를 생략하고 최상위 summary를 제공한다.",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【우선 사용】추가할 탐색 방향 배열, 순서대로 처리. 요소 필드는 아래 최상위 필드(summary/asset_ids/parent_ids/priority)와 같다. 반환 ids는 이 배열과 길이/순서가 같다.", "items": map[string]any{"type": "object"}},
			"summary":    str("[단일] 탐색 방향 한 문장: 무엇을+왜. 방향이 명확하면 충분하며 자산 id에 의존하지 않는다."),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "이 방향에서 테스트/공격할 【대상 자산 id】(**가능하면 전달**, 0/1/여러 개. list_assets가 반환한 자산 id이며 탐색 노드 id 아님). 대상 자산(사이트/API/매개변수/호스트 등)을 나타낸다. 구체적 자산을 다루는 방향이면 반드시 전달한다. 대상의 구조화 표시로 커버리지 중복 제거와 자산 연결에 사용된다. 구체적 대상이 전혀 없는 순수 전역 정찰일 때만 비워 둔다."},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "상위 기준점 id(선택, 0/1/여러 개): 이 방향을 도출한 【확인된 사실(fact)/발견(finding)】. **기존 fact/finding 노드 id만 가능하며 의도/목표/힌트는 금지**. 의도는 확인된 지식에 연결되어야 하며 임의 계획이 아닌 발견 주도로 생성한다. 여러 사실이 새 의도를 만들었으면 모두 전달한다. 최상위의 완전히 새 정찰 방향은 비워 둔다(작업 시작 origin fact에 자동 연결)."},
			"priority":   intp("우선순위 0-10, 기본 5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 단일 모드: 최상위 summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 사람이 주 agent로 의도를 직접 넣으면 이미 done인 작업(open 목표 없는 goalless 분기)을
			// running으로 돌려 worker가 배정받아 실행하게 한다. resumeTask는 주 agent의 Chat에서만 연결한다
			// (SetResumeTask). planner ToolSet은 nil이므로 planner가 add_intent를 호출할 때는
			// no-op이며 정상 의도 생성에 영향 없다. 위에서 의도를 open으로 생성했으므로 재활성화 시 비었다고 오판하지 않는다.
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 단일: 기존 반환 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "이 작업의 목표 노드와 상태(open/met)를 나열하여 달성 여부 판단에 사용한다.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "발견/사실이 목표 달성을 입증한다고 판단할 때 호출한다. 증거 노드를 목표 노드에 연결하고 목표를 met로 표시한다.",
		obj(map[string]any{
			"goal_id":     idp("목표 노드 id"),
			"evidence_id": idp("입증하는 발견/사실 노드 id"),
			"reason":      str("이 증거가 목표를 충족하는 이유"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id와 evidence_id 필수"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id는 이 작업의 목표 노드여야 함(연결 작업 목표는 읽기 전용)"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id는 이 작업 또는 직접 연결된 작업의 사실/취약점 노드여야 함"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// 목표를 met로 표시할 때마다 이 작업의 모든 목표가 met인지 확인하고 그렇다면 자동으로
			// 작업 완료(GoalMet 설정)를 판단하여 모델의 명시적 goal_met 호출에 의존하지 않는다.
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("목표 %d개 모두 met(마지막으로 goal %d가 유발)", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met; 이 작업의 모든 목표가 달성되어 작업을 자동 완료로 판단함", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【전체 작업 즉시 종료】. 작업의 【모든 목표가 실제로 달성되어 전체 마무리】됨을 확인한 경우에만 호출한다. 목표 하나/flag 하나/취약점 하나만 달성한 것은 전체 완료가 아니며 prove_goal로 그 목표만 표시한다. ⚠️이번 계획 라운드를 끝내는 도구가 아니다. 새 의도가 없거나 worker 산출물을 기다리면 【이번 라운드를 그냥 종료하고 이 도구는 호출하지 않는다】(의도 0개도 정상). 보통 prove_goal로 각 목표를 입증하는 것이 우선이며 goal_met는 개별 입증을 우회하여 전역에서 바로 마무리하는 수단일 뿐이다.",
		obj(map[string]any{"reason": str("달성 근거(실제 목표 달성 증거여야 하며 “이번 라운드에 새 방향 없음” 같은 라운드 종료 이유는 안 됨)")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "확인된 취약점을 기록하며 evidence에 명령 출력, 로그 등 검증 가능한 증거를 제공한다. 작업 문맥에서는 현재 intent_id를 전달한다. 반환 finding_id는 독립 취약점 기록 ID이며 finding_node_id는 탐색 노드 ID다(첫 줄에는 해당 노드 번호 유지).", obj(map[string]any{
		"vulnclass": str("취약점 유형"), "name": str("취약점 이름"), "severity": str("critical|high|medium|low"), "summary": str("발견 요약"),
		"intent_id": idp("현재 작업의 의도 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "영향받는 자산 id"},
		"evidence":         str("증거/PoC 텍스트"),
		"evidence_hint_id": idp("선택: 이 작업에서 해당 취약점에 대응하는 힌트 노드 ID. 구조화 traffic_refs를 자동 포함한다. 상속 힌트나 다른 취약점 힌트는 참조 불가"),
		"traffic_refs": map[string]any{"type": "array", "description": "선택. HTTP/HTTPS 취약점은 먼저 검색한 뒤 각 요청/응답이 결론을 실제로 뒷받침하는지 확인하고 실제 ID를 재현 순서대로 기입한다. TCP 등 비HTTP 취약점, 미수집, 정확한 기록 없음이면 생략하거나 []를 전달해도 보고를 막지 않는다. evidence에 이유와 다른 검증 가능한 증거를 제공할 수 있다. ID 추측, 도메인/시간에 따른 연관 추정, 패킷 보충만을 위한 반복 탐지는 금지한다. 용도: baseline 정상 대조 / proof 취약점 입증 / verification 추가 검증 / supporting 보조 증거.",
			"items": obj(map[string]any{"traffic_id": str("traffic_search가 반환한 실제 트래픽 ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("이 트래픽이 취약점 결론을 뒷받침하는 방식")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding에는 작업 문맥이 필요하다. 플랫폼 대화에서는 add_task_hint로 해당 작업에 취약점과 기존 traffic_refs를 인계하고 작업 Agent가 등록하게 한다. 등록된 취약점에는 bind_finding_traffic으로 추가 연결할 수 있다."), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "꺼져 있음, 페이지에서 수동 연결 가능" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id는 유효한 힌트 노드 ID여야 하며 인계 힌트가 없으면 생략"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("트래픽 증거 저장소 사용 불가; 취약점 미등록"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "취약점은 저장되었으나 트래픽은 연결되지 않았다. TCP/패킷 없음이면 정상 진행 가능하다. 확인된 HTTP 트래픽이 있으면 사용 가능한 bind_finding_traffic 또는 취약점 페이지에서 추가 연결한 뒤 증거 인계를 완료한다. 취약점을 중복 생성하지 않는다."
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "취약점이 저장되었다. Agent 자동 트래픽 연결은 꺼져 있으며 페이지에서 수동 연결할 수 있다."
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem은 record_fact 일괄/단일 입력의 사실 하나다.
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 결론을 뒷받침하고 사후 확인할 수 있는 주요 증거 한 줄(명령+핵심 출력)
	Confidence string            `json:"confidence"` // observed(직접 관측) | inferred(현상에서 추론)
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact는 fact 노드를 쓰고 의도에 연결한다(intent→yields→fact). defaultIntent는
// 일괄 입력에서 해당 항목의 intent_id가 없을 때 사용하는 기본 의도다.
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary는 비어 있을 수 없음")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id는 이 작업의 의도여야 함(연결 작업 의도는 읽기 전용)")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "탐색 【사실/결론】을 탐색 그래프에 기록하고 생성한 의도(intent_id)에 연결한다. 지문/열거 같은 【긍정 결론】과 “포트 닫힘”/“매개변수 주입 불가”/“로그인 입구 미발견” 같은 【부정 결론】 모두 기록한다.\n"+
		"⚠️한 탐색의 여러 관측은 【사실 하나로 종합】한다. 합칠 수 있으면 가능한 하나로 표현한다. summary=이번 결론의 한 문장 요약, detail=관련 상세(여러 구체 항목 포함 가능). 예: 지문 의도→사실 하나 {summary:\"X 사이트의 기술 스택과 응답 특징 식별\", detail:\"nginx 1.25 / Vue3 / 200 / title=.. / body_len=..\"}. 상태 코드/지문/제목을 각각 기록하지 않는다. 의도 하나는 보통 사실 하나를 생성하며 너무 쪼개면 그래프가 무한히 커진다.\n"+
		"★facts 배열은 【서로 다른】 여러 결론을 한 번에 기록하는 용도다(항목의 intent_id 생략 시 최상위 intent_id 사용). 반환 ids 배열은 facts와 길이/순서가 같다.\n"+
		"⚠️도구 출력에서 【실제로 관측한】 결론만 쓰고 지어내지 않는다. evidence와 confidence는 부정확한 결론이 그래프를 오염시키지 않게 한다.\n"+
		"  · evidence=결론을 뒷받침하는 【한 줄】 주요 증거(명령+가장 잘 입증하는 출력 한두 줄). **반드시 간결하게** 작성하며 상세는 detail에 있으므로 긴 출력을 다시 붙이지 않는다.\n"+
		"  · confidence=observed(출력에서 직접 관측) | inferred(현상에서 추론).\n"+
		"  · **부정 결론**(주입 불가/포트 닫힘/입구 미발견 등)은 “관측 + 잠정 해석”만 작성한다. 실제 관측을 서술하고 방향 포기 여부는 계획자가 전체 상황으로 판단한다. evidence는 필수이며 수단 미소진 또는 약한 증거(한 번 탐지/그렇게 보임 포함)는 inferred, 실제로 소진했고 직접 관측한 경우만 observed로 표시한다.",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【서로 다른 결론이 여러 개일 때 사용】사실 배열. 요소는 아래 최상위 필드(summary/detail/evidence/confidence/intent_id/asset_ids)와 같다. intent_id 생략 시 최상위 intent_id 사용. 반환 ids는 배열과 길이/순서가 같다.", "items": map[string]any{"type": "object"}},
			"summary":    str("이번 탐색 결론의 【한 문장 요약】(detail의 개요)"),
			"intent_id":  idp("사실을 생성한 의도 id(배정받은 의도; 일괄에서는 각 항목의 기본값)"),
			"detail":     str("사실의 관련 상세. 이번 탐색의 여러 관측 사실을 모두 여기에 작성"),
			"evidence":   str("【한 줄】 주요 증거: 명령 + 결론을 가장 잘 입증하는 출력 한두 줄. 반드시 간결하게 작성하고 긴 출력은 붙이지 않는다(상세는 detail)."),
			"confidence": str("observed(출력에서 직접 관측) | inferred(현상에서 추론). 부정 결론은 반드시 사실대로 표시."),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "관련 자산 id(선택, 0/1/여러 개): 이 사실이 다루는 자산"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 단일 모드 + 일괄 기본 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 최상위 intent_id = 일괄 기본값

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 단일: 기존 반환 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint는 hint 노드(active/human)를 탐색 그래프에 붙이고 자산에 연결할 수 있으며 id를 반환한다.
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent 자동 트래픽 연결이 꺼져 있어 traffic_refs가 있는 힌트를 저장하지 않았다. 시스템 설정에서 켜거나 텍스트만 인계할 수 있다.")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text는 비어 있을 수 없음")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// planner는 여기서 항목마다 호출하지 않고 addHint가 전체 일괄 기록 후 힌트 텍스트와 함께 한 번 호출한다.
	// 한 add_hint의 여러 힌트가 planner 트리거를 반복해서 채우지 않게 한다.
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal은 goal 노드(open)를 탐색 그래프의 작업 루트(origin fact, rel spawns)에 연결한다.
// origin은 t.worker(기본 system)이며 goals 분해기는 "goals", 주 agent 실행 중에는
// "human"으로 기록한다. planner 호출은 전체 기록 후 setGoals가 한 번 수행하며 여기서는 저장만 한다.
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text는 비어 있을 수 없음")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals는 이 작업에 탐색 목표(goal 노드)를 추가한다. 목표 분해기의 제출 도구이자
// 주 agent의 실행 중 목표 보완 도구다. 같은 관리 도구로 웹에서 설명/schema 수정 및 agent별 연결 가능하다.
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【이 작업】에 탐색 목표(goal)를 추가한다. 목표=최종적으로 전달/검증할 수 있는 결과이며 공격 단계나 정찰 행동이 아니다.\n"+
			"★일괄 우선: 여러 목표는 goals 배열로 한 번에 제출한다. 반환 ids는 길이/순서가 같다(실패 id=0, 상세 errors). 단일 항목은 goals를 생략하고 최상위 text 제공.\n"+
			"vulnclass 선택: 대응 취약점 유형(예: SQLi/IDOR). 비즈니스 로직 목표는 비워 둔다. 달성 여부는 시스템이 met로 판단/표시하며 이 도구는 추가만 담당한다.",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【우선 사용】추가할 목표 배열, 순서대로 처리. 요소: text(필수, 독립적으로 검증 가능한 최종 목표 하나)+vulnclass(선택). 반환 ids는 배열과 길이/순서가 같다.", "items": map[string]any{"type": "object"}},
			"text":      str("[단일] 독립적으로 검증 가능한 최종 목표 하나"),
			"vulnclass": str("[단일] 명확한 경우 대응 취약점 유형(예: SQLi/IDOR); 비즈니스 로직 목표는 비워도 됨"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals 비활성: ExplorationStore 미초기화"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 단일 모드: 최상위 text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner는 일괄 전체에 한 번 호출한다. notifyGoal을 우선하여 set_goals 한 번에 “사용자가
				// 목표 N개 추가: …” 트리거 하나를 기록한다. 분해기/worker에 콜백이 없으면 notify로 대체한다.
				// 분해기 round-0에는 notify도 없어 무동작이다. 아직 planner가 시작되지 않았기 때문이다.
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// 주 agent가 실행 중 목표 추가 → 완료/일시 중지 작업을 running으로 되돌려 계속한다(종료 상태 게이트가
				// 일반 notify를 삼키므로 명시적 재활성화 필수). mainagent에만 콜백을 연결하며 분해기/worker는 nil.
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 단일: 기존 반환 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint는 행동 제약을 task_constraints에 기록한다. origin은 t.worker(기본 system)이며
// 분해기는 "goals", 주 agent는 "human"으로 기록한다.
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text는 비어 있을 수 없음")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 유형 미지정 시 더 보수적인 금지로 기본 처리
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type은 allow 또는 deny여야 함")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints는 이 작업에 행동 제약(allow=허용 / deny=금지)을 추가한다. 목표
// 분해기 round-0의 제약 제출 도구이자 주 agent의 실행 중 제약 보완 도구다. 동일한 관리 도구로 웹에서
// 설명/schema 수정 및 agent별 연결 가능하다. planner/worker 시스템 프롬프트에 주입되어 탐색 경계를 제한한다.
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【이 작업】에 탐색 경계를 정할 행동 제약을 추가한다. type=allow(허용 행동) 또는 deny(금지 행동).\n"+
			"제약은 “할 수 있는/없는 행동” 규정(예: “현재 포트만 테스트, 다른 포트 스캔 금지”, “운영 DB 쓰기 금지”, “수동적(패시브) 정찰만 허용”)이며 목표나 공격 단계가 아니다.\n"+
			"★일괄 우선: 여러 항목은 constraints 배열로 한 번에 제출한다. 반환 ids는 길이/순서가 같다(실패 id=0, 상세 errors). 단일 항목은 constraints를 생략하고 최상위 text/type 제공.\n"+
			"작업 목표/설명에 【명시된】 제약만 등록하며 지어내지 않는다. 유형이 불확실하면 더 보수적인 deny 사용.",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【우선 사용】추가할 제약 배열, 순서대로 처리. 요소: text(필수, 제약 하나)+type(allow|deny). 반환 ids는 배열과 길이/순서가 같다.", "items": map[string]any{"type": "object"}},
			"text":        str("[단일] 행동 제약 하나의 내용"),
			"type":        str("[단일] allow(허용) 또는 deny(금지); 기본 deny 처리"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints 비활성: ExplorationStore 미초기화"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 단일 모드: 최상위 text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 단일: 단순 반환 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "사람/주 agent의 전략 힌트를 탐색 그래프에 연결하여 계획자가 다음 의도 생성 때 읽게 한다.\n"+
		"★일괄 우선: 여러 힌트는 hints 배열로 한 번에 제출한다(개별 호출보다 왕복 감소). 반환 ids는 hints와 길이/순서가 같다(실패 id=0, 상세 errors). 단일 항목은 hints를 생략하고 최상위 text 제공.",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【우선 사용】추가할 힌트 배열, 순서대로 처리. 요소는 아래 최상위 필드(text/asset_ids/traffic_refs)와 같다. 반환 ids는 배열과 길이/순서가 같다.", "items": obj(map[string]any{"text": str("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[단일] 힌트 내용, 예: “인증 후 API를 집중 탐색”"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "연결할 자산 id(선택, 0/1/여러 개)"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 단일 모드: 최상위 text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner는 일괄 전체에 한 번 호출한다. notifyHint를 우선하여 add_hint 한 번에 “사용자가
				// 전략 힌트 N개 추가: …” 트리거를 남겨 새 hint로 유발된 라운드임과 힌트 내용을 명확히 한다.
				// 콜백 미연결 시 일반 notify로 대체한다(bare wake, hint는 그래프에 접혀 있어 직접 읽을 수 있음).
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 단일: 기존 반환 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "실행 중인 의도(work) 하나를 종료한다. 방향이 틀어지거나 무의미한 탐색을 멈추며 종료된 의도는 stopped로 표시되어 자동 재배정하지 않는다. 먼저 get_worker_output으로 실행 내용을 보고 결정한다.",
		obj(map[string]any{"intent_id": idp("종료할 의도 id(=work 핸들)")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work는 현재 사용 불가"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 필수"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id는 이 작업의 의도여야 함(연결 작업 의도는 읽기 전용)"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("의도 %d의 work에 종료 신호를 보냄", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("X를 중지하고 Y에 집중"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "실행 중 의도(work)에 방향 조정 지시를 실시간 주입한다. 중단하거나 기존 진행을 잃지 않으며 worker가 다음 행동 전에 받아 조정한다. “X를 그만하고 Y에 집중” 같은 【의도 안의】 조정용이다. 방향 전체가 틀렸으면 kill_work 후 새 의도를 배정한다. 먼저 get_worker_output으로 실행 내용을 확인하는 것이 좋다.",
		obj(map[string]any{
			"intent_id": idp("조정할 의도 id(=work 핸들)"),
			"message":   str("worker에 줄 조정 지시. 무엇을 중지하고 어디로 전환할지 명확히 지정"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work는 현재 사용 불가"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 필수"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id는 이 작업의 의도여야 함(연결 작업 의도는 읽기 전용)"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message 필수"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("의도 %d의 work에 방향 조정 지시를 주입함(다음 단계부터 적용)", id)), nil
		})
}

// getWorkerOutput returns a work의 최종 또는 중단 시점까지의 결론 텍스트를 intent id로 반환한다.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "이 작업 또는 직접 연결된 작업의 의도(work) 최종 출력 결론을 조회한다. 연결 작업 결과는 source_task_id/inherited=true이며 읽기 전용. 정상 종료 시 요약, stopped/비정상 work는 중단 시점까지의 마지막 출력을 반환한다.",
		obj(map[string]any{"intent_id": idp("의도 id(=work 핸들)")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 필수"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id가 이 작업이나 직접 연결된 작업에 속하지 않음"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "(이 work는 아직 출력이 없음)",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("(이 work는 아직 출력이 없음)"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"의도(work)의 【실행 과정】을 조회한다(최종 결론만 주는 get_worker_output과 다름). 세 가지 사용법:\n"+
			"① intent_id만 전달 → 각 단계 요약 스트림 반환(summary≤100자, step_id 포함; 행동 개요만 있으며 전체 출력 없음).\n"+
			"② intent_id + q → 키워드에 일치하는 단계 요약만 반환(요약/전체 출력 모두 검색하나 summary만 반환; 내용 조회는 ③).\n"+
			"③ intent_id + step_ids → 해당 단계의 전체 내용(detail) 반환. 한 번에 최대 5개이며 초과 시 앞 5개만 반환하고 notice/omitted_step_ids에 미조회 항목 표시.\n"+
			"일반 절차: ①/②로 의심 단계 step_id를 찾고 ③으로 전체 출력 조회. thinking 단계 제외. 직접 연결된 작업의 과거 trace도 지원하며 결과는 source_task_id/inherited=true이고 읽기 전용.",
		obj(map[string]any{
			"intent_id": idp("의도 id(=work 핸들)"),
			"q":         str("키워드: 요약/전체 출력에 일치하는 단계만 반환(선택, step_ids와 함께 사용 불가)"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "전체 내용을 조회할 step_id(①/② 결과에서 획득. 한 번에 최대 5개, 초과하면 앞 5개만 반환하고 나머지는 omitted_step_ids에 표시)"},
			"limit":     intp("요약 스트림/검색 반환 상한(선택)"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 필수"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id가 이 작업이나 직접 연결된 작업에 속하지 않음"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"한 번에 최대 %d개 단계의 전체 내용을 조회할 수 있다. 이번에는 앞 %d개(%v)를 반환했으며 미조회 %d개는 %v다."+
							"이 내용만으로 충분히 파악했다면 나머지는 조회할 필요 없다. 계속 필요하면 해당 step_id로 다시 호출한다.",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【시스템이 대부분의 정보를 이미 제공하므로 보통 권장하지 않음】키워드(q)로 【이 작업의 다른 work 실행 과정】을 검색하여 worker가 봤지만 fact에 기록하지 않은 것(경로/token/오류 등)을 되찾는다."+
			"자신의 의도 단계는 자동 제외된다(이미 자신의 문맥에 있음)."+
			"일치 단계의 요약(summary≤100자)만 반환하며 각 항목에 intent_id 포함. 이를 바탕으로 get_worker_trace(intent_id, step_ids=[...])로 전체 내용 조회.",
		obj(map[string]any{
			"q":     str("키워드(모든 work 단계 요약+전체 출력 검색)"),
			"limit": intp("반환 상한, 기본 100(선택)"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q 필수"), nil
			}
			// 호출자 자신의 의도 단계는 제외한다(worker의 자체 trace는 이미 문맥에 있음).
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【시스템이 대부분의 정보를 이미 제공하므로 보통 권장하지 않음】이 작업에서 【실행한 work(의도)】의 인덱스 나열: intent_id + 방향 한 문장(summary) + 상태."+
			"worker는 탐색 그래프를 볼 수 없으므로 이를 통해 살펴볼 가치가 있는 work를 찾고 get_worker_trace(intent_id)로 단계, get_worker_trace(intent_id, step_ids=[...])로 상세를 조회한다."+
			"이미 실행한 running/done/exhausted/blocked/stopped만 나열하고 미실행 open은 제외한다. 작업 경계는 여전히 배정받은 의도 하나이며 다른 work 조회는 관측 재사용/중복 노동 방지용일 뿐이다.",
		obj(map[string]any{
			"q":     str("summary 키워드 필터(선택)"),
			"limit": intp("반환 상한, 기본 50(선택)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding: 계획 상황 판단 시 스스로 확인한 취약점은 직접 등록할 수 있다(worker와 같은 도구).
		t.addFinding(),
		// list_companies: 회사 목록 + scope + 자산 수 조회(company_id 확보 / 귀속 범위 이해).
		t.listCompanies(),
		// list_assets: 계획 시 DSL로 전체 자산 DB 검색(list_untested_assets의 범위 안 미검사 관점에
		// 도메인/지문/포트/상태 코드 등 조건으로 전체 DB 조회 능력을 보완).
		t.listAssets(),
		// add_company_scope: 계획 시 도메인/IP/CIDR/ICP/키워드를 회사 자산 범위에 추가(일치 자산 자동 귀속).
		t.addCompanyScope(),
		// add_task_scope: 전체 루트 도메인/회사/특정 하위 도메인/IP를 이 작업 테스트 범위(커버리지 분모)에 능동 추가.
		t.addTaskScope(),
		// list_untested_assets: 필요 시 이 작업 범위의 미검사 자산을 유형/페이지로 조회하고 추가 테스트를 직접 결정한다.
		t.listUntestedAssets(),
	}
}
