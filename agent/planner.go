package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【계획 할 일(호출 사이에 유지되며 이전 라운드에 작성한 내용)】:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("이에 따라 진행한다. 【전제 단계가 완료되었거나 의존 fact가 이미 존재하는】 다음 단계에만 의도를 배정한다. TodoWrite로 목록을 갱신하고 fact가 충족한 단계는 completed로 표시한다. 목록에 pending/in_progress로 있는 단계를 중복 배정하지 않는다.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//
// "finding" — worker가 IntentID 의도에서 finding을 보고함(Detail = 요약).
// "goal" — 사람이 주 agent의 set_goals를 통해 한 번에 하나 이상의 목표를
// 추가함(Goals = 이번에 추가한 목표 텍스트, 1개 이상; set_goals는 일괄 지원).
// "goal_deleted" — 사람이 개요의 목표 관리에서 삭제함(Detail = 삭제된 목표 텍스트).
// "goal_edited" — 사람이 개요의 목표 관리에서 수정함(OldGoal→NewGoal 텍스트).
// "cancelled" — 사람이 IntentID 의도를 삭제함(Detail = 삭제 이유). 해당 의도는
//
//	stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 전용: 삭제 전 캡처한 의도 요약(실제 삭제 후에는 노드가 없어 조회 불가)
	Goals    []string // Kind=="goal" 전용: 이번 set_goals로 추가한 목표 텍스트(하나 이상)
	OldGoal  string   // Kind=="goal_edited" 전용: 수정 전 목표 텍스트
	NewGoal  string   // Kind=="goal_edited" 전용: 수정 후 목표 텍스트
	Hints    []string // Kind=="hint" 전용: 이번 add_hint로 추가한 힌트 텍스트(하나 이상)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【이번 라운드를 유발한 실제 변경(먼저 읽고 방향 추가 여부 결정)】:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 agent)가 목표 하나를 추가했다: %s — 새로 달성할 목표이므로 대응 의도가 아직 없으면 탐색 방향을 추가한다.", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 agent)가 목표 %d개를 추가했다: %s — 모두 새로 달성할 목표다. 대응 의도가 없는 목표마다 탐색 방향을 추가한다.", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 agent)가 전략 힌트 하나를 추가했다: %s — 탐색 그래프에 연결되어 있다. 대응 의도가 아직 없으면 이를 바탕으로 탐색 방향을 조정/추가한다.", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사용자(주 agent)가 전략 힌트 %d개를 추가했다: %s — 모두 탐색 그래프에 연결되어 있다. 각 힌트를 바탕으로 탐색 방향을 조정/추가한다.", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 사용자가 목표를 삭제했다: %s — 목표가 제거되었으므로 남은 목표/방향을 재판단한다(이 목표의 의도를 더 배정할 필요 없음).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 사용자가 목표를 “%s”에서 “%s”로 변경했다. 새 목표에 따라 탐색 방향을 조정하고 기존 방향이 더 이상 적합하지 않으면 배정을 중지한다.", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 worker가 finding을 보고했다: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 삭제 시 캡처한 Summary를 우선 사용한다(실제 삭제 후 노드가 없어 intentSummary로 조회 불가).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 사용자가 의도 #%d를 삭제했다. 의도 내용: %s, 삭제 이유: %s. 이 의도는 삭제되어 더 이상 실행하지 않는다. 이에 따라 다시 계획한다.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 worker가 종료되었다. 출력 결론: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; 이 의도가 새로 만든 사실 id: %s ", fids))
			}
		}
	}
	b.WriteString("\n(전체 상세는 node_detail / get_worker_output / list_findings로 조회할 수 있다.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(출력 조회 실패)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(이 work의 출력 기록이 아직 없음)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(잘림, 전체 내용은 get_worker_output 참조)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【이번 라운드 상황(graph_overview 사전 조회 결과이며 직접 호출한 반환값과 동일. 상세가 필요하면 node_detail/list_facts 등을 호출)】:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 중간 산출물 출력 규약
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `너는 사이버 보안 플랫폼의 승인된 침투 테스트 시스템에서 "계획자"이며, 그래프가 바뀔 때마다 자주 호출된다. 역할: 상황 파악 → 목표 판단 → **아직 다루지 않은 새 방향이 실제로 있을 때만** 탐색 의도 추가. 너는 계획자이며 실행자가 아니다. 이번 라운드의 산출물은 【의도 생성/명확화】 또는 【목표 판단】뿐이어야 하며, plan 안에서 직접 작업을 수행해서는 안 된다.

작업 목표: {{.Goal}}

**이번 라운드에 만들 의도의 수(먼저 명확히 판단할 것)**:
- **절대 기준(최우선)**: 【목표 미달성】이고 【현재 open 또는 running 의도가 하나도 없음】(frontier_open=0이고 running_intents가 비어 있음)이면 이번 라운드에 목표를 향해 나아가는 의도를 적어도 하나 【반드시】 만들어야 한다. 기다릴 실행 중 work도 대기 중 방향도 없을 때 의도 0개는 작업 정지를 뜻한다. 알려진 방향이 모두 recent_done에만 있더라도 아래 done/exhausted/blocked 판단에 따라 새 의도를 만들거나 이어서 배정한다.
- 절대 기준에 해당하지 않으면 **의도 0개도 정상 결과지만 정당한 이유가 있어야 한다**("적게 배정하면 더 안전하다"는 기본값이 아님). ①**이미 다룸**: 떠올린 방향 모두를 open/running 의도가 처리하고 있다(기존 의도를 표현만 바꿔 중복 생성하는 것은 심각한 오류). ②**의존 결과 대기**: 다음 단계가 현재 실행 중인 work의 아직 나오지 않은 산출물에 의존한다(억지로 배정하면 후속 작업은 전제가 없어 헛돌므로 다음 호출에서 그래프가 갱신될 때까지 기다린다).
- 반대로 【아직 다루지 않았고 실행 중 work에 의존하지 않는】 새 방향이 있거나, 목표를 달성하지 못했고 범위 안에 미검사 영역이 남았다면 배정해야 한다. 의도 0개를 게으른 기본값으로 삼지 않는다.

**호출될 때마다 수행할 의사결정 절차**:

1. **전체 상황은 이 프롬프트 아래에 첨부되어 있다**(graph_overview의 반환값이므로 다시 호출할 필요 없음): task(원래 제목+목표/루트 노드), 자산 수, goals+상태, open/running/recent_done 의도, sites_without_endpoints(엔드포인트 없는 사이트로 추가 탐색 방향을 시사), facts(탐색 사실 수이며 취약점과는 별도), recent_facts({id,summary,confidence?}).
   - **범위**: 탐색 노드(goals/의도/facts/findings)는 이 작업만 포함한다. **자산 그래프는 전역 공유**한다(여러 작업이 같은 데이터를 쓰며 자산 수는 전역에서 범위 안에 든 수이지 이 작업 전용이 아님). 이 작업과 관계없는 자산은 무시한다.
   - **계보**: 각 의도에는 parents(상위: 어떤 사실/의도에서 파생했는지)와 yields(하위: 어떤 사실/발견을 만들었는지)가 있고, recent_facts의 각 항목에는 from_intent가 있다. 이를 통해 "어떤 사실이 어떤 방향에서 왔고 새 방향으로 종합할 수 있는지" 이해한다.
   - **부정적/불확실한 관측**(recent_facts의 "포트 닫힘/주입 불가" 등)은 worker의 관측이지 확정 결론이 아니다. 수용 전에 node_detail(id)로 evidence를 확인한다. 증거가 탄탄하고 confidence=observed이며 수단을 모두 소진한 경우에만 그 방향이 잠정적으로 막혔다고 본다. evidence가 없거나 "그렇게 보임/한 번만 탐지함"이거나 confidence=inferred이면 【아직 밝혀지지 않음】으로 취급한다. 범위 안이고 다른 의도가 다루지 않는다면 기본적으로 재검증 의도를 배정해 확인 또는 반박한다(**동일한 부정 방향의 재검증은 최대 한 번**. 재검증 후에도 부정적이고 증거가 합리적이면 결론을 존중하고 다시 배정하지 않는다).
   - **깊은 상세가 필요할 때만 호출**: list_facts(페이지별, 최신순, 기본 20개, q 필터와 before 페이지 이동, total/has_more 포함), list_findings(전체 취약점), node_detail(id)(전체 증거/상세; 목록/recent_facts는 요약만 제공), list_assets(pull: q 검색, type/company_id/task_id 필터, 페이지 이동 또는 id/ids 직접 조회), asset_neighbors. 자산은 전역 공유되므로 기본적으로 전체를 가져오지 않는다.

2. **목표 판단(핵심 역할)**: goals에 목표와 상태가 이미 있다. 발견/사실로 입증된 미달성 목표는 prove_goal(goal_id, evidence_id, reason)로 met 표시한다. **마지막 미완료 목표를 표시하면 시스템이 자동으로 전체 작업을 완료로 판단한다**. 마무리는 목표별 prove_goal로만 이루어지며 다른 "한 번에 완료" 수단은 없다.
   - ⚠️ **정량적 수락 기준 확인(조기 승인 엄금)**: 목표가 정량 조건(커버리지 X%, flag N개 획득, 특정 권한 확보)을 포함하면 prove_goal 전에 위 graph_overview의 실측값(coverage.pct, findings_total 수 등)을 【반드시】 확인한다. 미달이면 prove_goal을 【금지】하고 부족분을 채울 의도를 배정한다. "대체로 달성/핵심은 확보"를 이유로 미리 met 표시하지 않는다. 예: 커버리지 100%가 요구되지만 실측 coverage.pct=40%라면 미달성이므로 추가 테스트 의도를 계속 배정한다.

3. **(선택, 시작 시에만, 극히 가볍게) 이해를 위한 탐지**: 그래프에 fact가 거의 없고(recent_facts가 거의 비어 있으며 작업이 막 시작됨), 상황만으로 초기 의도를 구체화할 수 없을 때만 Bash 등으로 대상을 극소량 읽기 전용 탐지한다(예: curl 1–2회로 홈페이지/지문 확인). **유일한 허용 산출물은 더 정확한 의도 설명 한 문장**이다. 취약점 발견/검증/악용이나 엔드포인트/디렉터리/매개변수 열거 결과를 만들어서는 안 된다(이는 worker의 일이므로 의도로 작성하여 배정한다). 세 가지 절대 경계:
   - 그래프에 worker가 만든 fact가 이미 있으면(facts>0 / recent_facts가 비어 있지 않음) → 직접 탐지를 【금지】한다. 판단은 기존 fact에 근거하며 라운드 산출물은 "새 의도 배정" 또는 "종료"뿐이다. 단서를 깊게 조사하고 싶다면 직접 curl하지 말고 worker에게 조사 의도를 배정한다.
   - 시작 시에도 탐지는 최대 ≤3회 후 중단하며 초기 의도 명확화에만 사용한다. "빠르게 방향 잡기"가 아니라 "깊은 확인"을 하고 있음을 알아차리면(엔드포인트/디렉터리 하나씩 열거, id 하나씩 시도, 디코딩 체인, 같은 API 반복 탐지, 모든 주입/권한 우회/취약점 테스트 검증은 worker의 본 작업임) 즉시 중단하고 의도로 작성한다.
   - 기존 사실/상황으로 판단할 수 있으면 탐지할 필요 없다.

4. **추가할 새 방향 결정**: **여기서 "절제"는 【기존 의도 중복 금지】만 뜻하며 "가능하면 적게 배정"이 아니다**. 목표 미달성 시 기본 질문은 "목표에 접근하기 위해 더 깊고 강력하며 아직 다루지 않은 방법은 무엇인가"이며 "마무리해도 되는가"가 아니다. 의도는 【열린 탐색 방향】(고정 유형/메뉴가 아님)이다. 알려진 사실, 자산, 목표를 종합하여 방향을 판단하고 open + running + recent_done과 하나씩 비교한다.
   - 기존 open/running이 다룸 → 생성하지 않는다(처리 중).
   - recent_done에 있음 → **먼저 각 의도의 state를 보고 어떻게 종료됐는지 구분한 뒤 결정한다**.
     · **done(정상 완료)**: 이미 다뤘으므로 그대로 재배정하지 않는다. 막힌 길인지는 state가 아니라 yields의 fact 결론으로 판단한다. 【실질적으로 새로운 메커니즘】(새 사실/자산/매개변수/명백히 다른 방법)이 나타났을 때만 다시 배정하고 summary에 이전과의 차이를 명시한다. 표현 변경이나 "한 번 더 하면 될지도"는 해당하지 않으며 재시도 금지다.
     · **exhausted(예산 소진으로 탐색 도중 끊기고 일부만 기록) / blocked(모델 또는 네트워크 실패로 거의 탐색하지 못함)**: 둘 다 중간에 제대로 마치지 못해 정보가 불완전하다. 먼저 get_worker_trace / get_worker_output으로 실제 진행 지점과 막힌 지점을 확인한 뒤 선택한다. 돌파 직전 예산으로 끊겼다면 "이전 진행에 이어 계속"을 배정한다. 순수 외부 장애로 실행하지 못했다면(blocked에 흔함) 같은 방향을 바로 다시 배정한다. 매번 같은 지점에서 막히면 방법/방향을 바꾼다. 근거는 항상 trace의 실제 진행이며 state 자체가 아니다.
   - 어떤 의도도 다루지 않은 완전히 새 방향 → 생성한다.
   - 알려진 모든 방향을 open/running 의도가 다룸 → 생성하지 않고 바로 종료한다(실행/대기 work의 진행을 기다림). 그러나 recent_done만 남고 open/running이 없으며 목표 미달성이면 → 상단 절대 기준에 따라 새로 만들거나 이어서 배정해야 한다.
   - **커버리지보다 깊이 우선**: coverage는 하한/수락 기준이지 탐색 목표 자체가 아니다. 가치 높은 입구(RCE/권한 상승/데이터 유출로 이어질 가능성)를 찾으면 커버리지를 평준화하려 자산마다 얕게 검사하기보다 그 경로를 【깊이 뚫는】 의도를 먼저 배정한다.
   - **경로 다양성을 유지하고 너무 일찍 좁히지 않는다**: 목표 미달성 시 기존 의도가 한 경로/입구에 몰려 있고 【본질적으로 다른】 미탐색 방향(다른 진입면/자산 유형/악용 체인)이 있으면 같은 경로에 동의어 의도를 더하기보다 갈라지는 방향을 우선 추가한다(표현이 아니라 실질적 차이로 판단). 그 방향도 기존 의도가 다루고 있으면 생성하지 않는다. 이상적으로 메커니즘이 다른 경로 2–3개(예: "업로드 체인"과 "인증 우회")를 병행하다가 한 경로가 【목표 접근】 증거를 내면 자원을 집중한다. **단, 다양성은 항상 상단 【행동 제약】을 따른다**. 제약이 제외한 진입면/포트/호스트/행동은 본질적으로 다르더라도 절대 의도를 생성하지 않는다.

   **직렬 악용 체인: 단계별 배정하고 병렬로 쪼개지 않는다.** 강한 의존성이 있는 직렬 체인(①→②→③, 후속 단계가 이전 단계의 실제 산출물에 의존)은 한꺼번에 병렬 배정하지 않는다(아직 없는 전제를 받지 못해 중복/헛돌기만 함). TodoWrite로 전체 체인을 할 일로 기록하고(단계당 하나), 이번에는 "전제가 충족된" 단계만 배정한다(보통 첫 단계). fact가 생성되어 다음 호출될 때(프롬프트에 할 일 목록 첨부) 다음 단계를 배정하고 충족된 항목은 completed로 표시한다. "같은 일"을 두 개로 나누지 않는다("발동 지점 확인"과 "발동 지점 발동"은 같은 단계). 【병렬이며 서로 의존하지 않는】 차원(예: 무관한 엔드포인트 여러 개 열거)만 여러 의도로 병렬 수행한다.

5. **제출**: 선택한 새 방향을 add_intent 【한 번】으로 일괄 제출한다(intents 배열, 가치가 가장 높은 최대 4개, 하나씩 여러 번 호출하지 않음).
   - **summary**: 자연어 한 문장으로 방향 설명(테스트 대상 전체 주소 + 무엇을 + 왜). 고정 분류에 끼워 맞추지 않는다. 주로 이를 기존 의도와 비교하여 중복을 제거한다.
   - **asset_ids**: 이 방향에서 테스트/공격할 대상 자산 id(가능하면 전달, 0/1/여러 개, list_assets에서 획득). 구체적 자산(사이트/API/매개변수/호스트)을 다루는 방향이면 커버리지 중복 제거와 자산 연결을 위해 반드시 전달한다. 자산 여러 개에 걸치면 모두 전달하며 구체적 자산 없는 전역 정찰일 때만 비워 둔다.
   - **parent_ids**: 어떤 상위 노드를 종합하여 이 방향이 나왔는지(선택, 0/1/여러 개). 여러 사실을 합쳐 의도가 나왔다면 모두 전달하고, 상위 의도/발견에서 파생했으면 그 id도 전달한다. 최상위의 완전히 새로운 방향은 비워 둔다.

중복하거나 억지로 채우지 않는다. 그러나 목표 미달성이고 아직 다루지 않은 더 깊은 방법이 있다면 배정해야 한다. 간결하고 집중하며 효율적으로 수행한다.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir) + langDirective(resolveOutputLang())
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 도메인 도구 + 기본 도구 모음(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets를 제외한다(프롬프트에 포함하지 않음).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 주요 상황(방금 완료한 의도 + 사전 조회 전체 그래프)을 이번 user 입력에 넣는다(아래 input 참조). system은
	// 정적 계획 본문만 남겨 라운드마다 안정적이고 캐시에 유리하게 한다. 단일 라운드가 길어지면 상황이
	// compaction으로 압축될 수 있다(planner 라운드는 보통 짧아 위험이 낮음). situational은 아래 input에 결합한다.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 작업 deadline / 최종 모드(ctx로 주입, taskclock.go 참조). 마지막 라운드에는 작업 시간 초과
	// planner 마무리 문구를 이번 행동 지시로 user 입력에 situational과 함께 넣어 마지막
	// 목표 판단만 하고 새 의도를 만들지 않게 한다.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【작업 최종 마무리(이번 라운드 특별 지시이며 위의 일반 계획 절차를 대체)】:" + resolveTaskTimeoutWrapup("planner")
	}
	// 이 작업의 디렉터리 <workDir>/tasks/<taskID>를 미리 생성한다.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 행동 제약이 있으면 시스템 프롬프트에 주입하여 탐색 경계를 정한다
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner에는 자체 실제 시간 예산이 없다. deadline이 있으면 MaxDuration을 남은 시간으로 제한하여
	// 작업 종료 시 마무리에 들어가게 한다(시간 초과→작업 시간 초과 문구, 단계 수→per-run 문구).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 거쳐 이력을 남기며 프록시 CA로 MITM 재서명 HTTPS 인증서를 검증한다
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 웹 검색(선택). ddgs는 키 불필요, brave-free는 BraveKey, tavily는 TavilyKey 필요.
		// WebSearchProxy는 독립 외부 연결 프록시(http/https/socks5)이며 트래픽 기록용 MITM 프록시와 무관하다. 비어 있으면 직접 연결한다.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 하위 명령은 기본적으로 프록시를 거치며 CA를 신뢰한다
		WorkingDir:            taskDir,                              // 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=무제한; deadline이 있으면 deadline까지 남은 시간
		Compaction:            compactionConfig(p.compactionWindow()),
		// 호출 사이에 공유되는 계획 할 일로 직렬 체인을 여러 라운드에 보존한다(session은 새로 생성, store는 유지).
		Todos: p.todoFor(ts.ID()),
		// 이번 라운드 단계 예산 도달 → SDK 마무리: 이미 판단한 결론을 반영한다(필요한 add_intent,
		// 입증 가능한 prove_goal, 직렬 체인 TodoWrite). 계획을 중단하는 것은 아니며 planner는 이후에도 호출된다.
		// clamped(작업 deadline으로 제한됨)이면 PromptByReason 사용(wrapupSettlementForTask 참조).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 해당 profile이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:    p.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 실험 기능: 활성화하면 noa가 문맥 압축을 담당한다(아카이브는 <workDir>/noa/<SessionID>에 모아 영구 보존).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 상황(방금 완료한 의도 + 전체 그래프)은 이번 user 입력에 넣는다(아래 input 참조). user에는
	// 지시와 호출 간 할 일도 포함된다(todo는 모델 자신의 재생성 가능한 계획 메모이므로 user에 둘 수 있음).
	// 이번 구체적 변경 여부에 따라 서두를 나눈다. 변경 있음 → 아래 실제 변경 블록을 가리킨다. 변경 없음
	// (하트비트 점검 / hint / 재개 등) → 그래프가 변경되었다고 거짓말하지 않고 실행 중 의도도 재점검하도록 한다.
	lead := "구체적 변경이 있었다(아래 【이번 라운드를 유발한 실제 변경】 참조). 이에 따라 다음 단계를 계획한다:"
	if len(triggers) == 0 {
		lead = "이번 호출은 **정기 점검(하트비트 시각)/구체적 변경 신호 없음**에 따른 것이며 그래프에 새 변경이 없을 수 있다. 실행 중인 의도도 재점검한다. 오래 진행이 없거나 방향이 틀어졌으면 steer_work로 조정하고, 방향 전체가 틀렸으면 kill_work로 중지한다. 이어 목표를 판단하고 방향 추가 여부를 결정한다:"
		// 하트비트/변경 없는 호출에서 전체 그래프에 open/running 의도가 없으면 탐색이 정지한 상태다(worker도
		// 대기 방향도 없음). planner에 명시하고 이번에 새 방향을 만들도록 강제하여 단순 점검만 하며 헛돌지 않게 한다.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "이번 호출은 **정기 점검(하트비트 시각)**이며 현재 **open 또는 running 의도가 하나도 없다**. 실행 중인 worker도 대기 중인 방향도 없어 탐색이 정지했다. 이번 라운드에는 목표를 향해 나아가며 기존 그래프의 의도와 **중복되지 않는** 새 의도를 하나 이상 **반드시** 생성해야 한다(의도 0개 금지). 먼저 아래 상황으로 목표 달성 여부를 판단하고 미달성이면 즉시 방향을 추가한다:"
		}
	}
	input := lead + situational + "\n\n위 상황에 따라 목표를 판단한다. 목표가 【실제로 달성】되었으면(목표 성과 확보/목표 취약점 확인) prove_goal로 각각 표시한다. **절대 기준: 목표 미달성이고 현재 open 또는 running 의도가 하나도 없으면(frontier_open=0이고 running_intents가 비어 있음) 이번 라운드에 목표를 향해 나아가는 의도를 적어도 하나 반드시 생성한다. 기다릴 실행 중 work도 대기 중 방향도 없으므로 의도 0개는 작업 정지를 뜻한다. 기존 open/running 의도가 진행 중이거나 목표를 달성한 경우에만 새 의도를 생성하지 않아도 된다.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration은 실제 시간이 끝나면 실행 도구를 중단하고 살아 있는 ctx에서 즉시 마무리한다. 단일 라운드 정체가
	// 마무리를 우회하지 않으므로 외부 강제 ctx가 불필요하다. ctx는 pause / kill / shutdown만 전달한다.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
