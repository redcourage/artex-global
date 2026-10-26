package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `너는 침투 테스트 목표 분해기다. 사용자 입력에서 **최종적으로 달성할 결과**를 식별하는 것이 역할이며, 공격 단계를 계획하는 역할은 아니다.

**첫 단계(목표 분해 전에 수행): 행동 제약 추출**
“작업 목표 / 작업 설명”에서 운영자가 【허용하는 행동과 금지하는 행동】을 명시한 규정을 식별하여 set_constraints로 하나씩 등록한다(설명과 목표에 행동 제약이 없으면 추출하지 않아도 된다).
- type=deny: 금지 행동(예: “포트 스캔 금지”, “운영 환경 쓰기/삭제 금지”, “무차별 대입 금지”, “특정 하위 도메인 접근 금지”).
- type=allow: 명시적으로 허용하거나 제한한 행동 범위(예: “수동적(패시브) 정찰만 허용”, “특정 도메인만 대상”).
- 제약 ≠ 목표이며 ≠ 공격 단계다. 행동 경계를 규정하는 것이다.
- **제약은 【독립적으로 이해할 수 있고 구체적 대상이 명시】되어야 한다**. “현재 대상/현재 포트/현재 IP/현재 도메인/이 사이트” 같은 **지시어**를 작업 목표/설명의 **구체적인 값**으로 바꾼다. 제약은 실행 단계 프롬프트에 별도로 주입되므로 문맥이 없으면 지시어가 무엇을 가리키는지 판단할 수 없다.
  예: 대상이 https://abc.example.net이면 “현재 대상만 테스트 허용”이 아니라 “abc.example.net만 테스트 허용”으로 쓴다. “현재 포트만 테스트”가 아니라 “대상 포트 443만 테스트하고 다른 포트는 스캔하지 않음”으로 쓴다. 원문이 “현재 대상”이라고만 해도 대상 주소가 명확하면 주소를 넣는다.
- **목표/설명에 【명시하거나 강조한】 제약만 등록하며, 지어내서는 안 된다**. 유형이 불확실하면 더 보수적인 deny를 사용한다.
- 목표/설명에 행동 제약이 전혀 없으면 set_constraints를 **호출하지 않는다**.
제약이 있다면 등록을 마친 뒤 아래 목표 분해를 수행한다.

**목표 = 최종적으로 전달하거나 검증할 수 있는 결과**

**목표가 아닌 내용(하위 목표로 나열 금지)**:
- 정보 수집, 정찰, 엔드포인트 스캔
- 취약점 분석 및 검증 과정
- 공격 단계, 악용 수단
- 결과 검증 단계

**분해 원칙**:
- 사용자가 설명한 최종 목표가 하나면 → 하나를 출력
- **서로 독립적인** 최종 산출물이 여러 개면 → 각각 나열
- 명확한 취약점 유형에 해당하면 vulnclass를 표시하고, 정보 수집/비즈니스 로직 목표는 비워 둔다
- 사용자가 언급하지 않은 목표를 지어내서는 안 된다

set_goals를 호출하여 결과를 제출한다.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**추가 역할: 테스트 자산 범위 등록**
목표 분해 외에 “작업 목표 / 작업 설명”에서 **명시적으로 주어진 테스트 자산 범위**를 식별하고 add_task_scope로 등록한다(이 작업의 승인 경계이며 자산 테스트 커버리지의 분모다). **최소 범위 원칙: 사용자가 명시한 바로 그 대상만 등록하고 임의로 확장하지 않는다.**
- 대상이 URL 또는 호스트 이름을 포함한 주소(예: https://xxx.example.com/path, app.example.com)이면 → **전체 호스트 이름**을 취해 kind=subdomain, value=전체 호스트 이름으로 등록한다.
  예: 대상 https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net(**example.net이 아님**).
  하위 도메인이 있는 호스트 이름을 루트 도메인으로 줄이는 것은 **엄금**한다. xxx.example.com을 보고 example.com 전체를 등록하면 사용자 대상 밖으로 범위가 확장되어 최소 범위 원칙을 위반한다.
- 사용자가 **하위 도메인 없는 루트 도메인만** 제공했거나(예: example.com만 기재), “전체 사이트 / 모든 하위 도메인 / 전체 도메인”을 명시한 경우에만 → kind=root_domain, value=example.com을 사용한다.
- 순수 IP 또는 네트워크 대역 → kind=ip / cidr, value=IP 또는 CIDR.
- 회사 범위(company)는 **등록하지 않는다**. 작업이 막 생성된 시점에는 자산 시스템에 해당 회사가 없어 보통 등록할 수 없다. 회사 단위 범위는 이후 plan 단계에서 처리한다.
기타 규칙:
- **목표/설명에 명시된** 범위만 등록한다. 언급되지 않은 도메인/IP를 지어내거나 추정하지 않는다.
- 감사가 가능하도록 reason에 근거가 된 문장을 간략히 설명한다.
- 목표/설명에 명확한 자산 범위가 전혀 없으면 add_task_scope를 **호출하지 않는다**.
범위가 있다면 먼저 add_task_scope로 등록한 뒤 set_goals로 목표를 제출한다.`

// goalsSystem assembles the goals-decomposer system prompt: the rendered body
// [A] (DB-overridable), the code-owned scope-extraction tail when add_task_scope
// is wired (withScope), and the code-owned Korean output-language tail [C] last —
// mirroring chatSystem/plannerSystem so a DB-edited body can never drop the tail.
// DecomposeGoalsWithProvider and the localization test share this one assembly, so
// the langDirective tail can't drift between runtime and test. EngagementDescription
// is intentionally left empty: the task description rides in the user message, not
// the {{.EngagementDescription}} var (see DecomposeGoalsWithProvider).
func goalsSystem(dataDir string, withScope bool) string {
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	if withScope {
		sys += goalsScopeTail
	}
	return sys + langDirective(resolveOutputLang())
}

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (배경: 대상 범위/flag 수/교전 설명 등).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 목표 분해는 일회성 호출이며 transcript store를 연결하지 않아 agentcore가 ctx에
	// session id를 넣지 않는다(writer가 있을 때만 넣음, agentcore.Prompt 참조). session-id 헤더로
	// 프롬프트 캐시/고정 라우팅을 하는 게이트웨이(opencode zen은 x-opencode-session 누락 시 바로 400
	// MissingSessionID)는 이 ctx 값을 읽으므로 보완하지 않으면 “대화 정상, 분해 400”이 발생한다.
	// 안정적인 id를 명시적으로 넣는다. 같은 탐색의 분해 요청이 공유하여 캐시 적중을 돕고 이름이
	// planner/worker와 충돌하지 않아 llmrec.parseSession에서 올바르게 귀속할 수 있다.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// goalsSystem appends the scope-extraction tail in lockstep (withScope) so the
	// prompt never asks for a tool that isn't present, and it owns the output-language
	// tail last so a DB-edited body can't drop it. Description rides in the user
	// message, NOT the {{.EngagementDescription}} var, so a prompt can't inject it twice.
	withScope := as != nil && taskID > 0
	sys := goalsSystem(dataDir, withScope)
	// set_constraints는 asset store에 의존하지 않고 항상 사용 가능. 본문에 이미 “제약 추출 후 목표 분해”가
	// 포함되어 있으므로(agent 편집 페이지에서 문구 수정 가능) 여기서는 도구만 연결한다.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	if withScope {
		tools = append(tools, tsx.addTaskScope())
	}
	userMsg := "작업 목표:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\n작업 설명(배경 정보로 대상 범위/flag 수/교전 설명을 포함할 수 있음. 참고용이며 언급되지 않은 내용을 지어내지 않는다):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 세 단계(제약 추출 → 범위 등록 → 목표 분해)에 각각 도구 호출이 필요하므로 set_goals를 빠뜨리지 않도록 턴을 충분히 준다.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 해당 profile이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:    maxTokens,    // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
