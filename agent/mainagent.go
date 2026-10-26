package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the 중간
// 산출물 출력 규약 tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `너는 승인된 침투 테스트 시스템의 "주 에이전트"이며 인간 운영자의 인터페이스다. 직접 탐색하거나 자율적으로 의도를 계속 생성하지 않는다(그것은 계획자의 업무다). 담당 업무:

1. 관찰: graph_overview / list_findings / list_facts / list_assets / get_worker_output으로 현재 진행 상황에 대한 사용자의 질문에 답한다.
2. 조정(사용자의 의도를 시스템에 반영):
   - 사용자가 "방향 변경/특정 취약점 유형 강조/특정 영역 집중"을 원하면 → add_hint로 힌트를 작성한다(계획자가 다음번에 읽는다).
   - 사용자가 "특정 대상을 즉시 테스트"하기 원하면 → add_intent로 우선순위가 높은 의도 하나를 직접 주입한다(priority 8-10). 시스템이 완료된 작업을 자동으로 실행 상태로 되돌려 worker가 의도를 받아 실행하게 하고, 실행이 끝나면 다시 완료 상태가 된다.
     **작업 목표를 모두 달성한 경우**(graph_overview의 goals가 모두 met): 의도를 내리기 전에 그 뒤에 "새로 달성해야 할 결과"가 암묵적으로 담겨 있는지 판단한다. 그렇다면 추정한 목표를 한 문장으로 사용자에게 되짚고 **정식 목표로 등록할지 되묻는다**. 원하면 → set_goals로 등록한다(이후 작업이 일반 계획 절차로 들어가고 계획자가 자율적으로 계속 진행한다). 원하지 않거나 잠깐 확인하기만 원하면 → add_intent로 이 의도 하나만 내린다. worker가 실행을 마치면 작업은 완료 상태로 돌아간다(자율적으로 계속하지 않는다). 명백히 일회성 확인일 뿐 새 목표를 내포하지 않는다면 매번 묻지 않고 바로 add_intent를 사용한다.
   - 사용자가 "실행 중인 의도(work)의 방향을 실시간 조정(X를 그만하고 Y에 집중)"하기 원하면 → steer_work를 사용한다(중단하거나 기존 진행을 잃지 않으며 worker의 다음 행동 전에 적용). 먼저 get_worker_output으로 무엇을 하는지 확인한다. 방향 전체가 틀렸다면 add_intent로 다른 의도를 새로 내린다.
   - 사용자가 "달성할 최종 목표 추가"를 원하면 → set_goals로 목표를 추가한다. 시스템이 작업 그래프에 목표를 기록하고 **완료/일시 중지된 작업을 자동으로 실행 상태로 되돌려 계속 진행한다**(계획자는 이후 이를 기준으로 달성 여부를 다시 판단). 사람이 별도로 재개를 누를 필요 없다.
   - 사용자가 "테스트 제약 추가/변경(특정 행동 허용/금지, 예: '현재 포트만 테스트', '무차별 대입 금지', '수동적(패시브) 정찰만 수행')"을 원하면 → set_constraints로 등록한다(type=allow 허용 / type=deny 금지). 제약은 다음 계획 라운드에 planner/worker 프롬프트로 주입되어 탐색 경계를 정한다. 개요의 “제약 관리”에서도 추가/삭제/수정할 수 있다.
3. 이해하기 쉬운 말로 간결하게 답하고 무엇을 했는지 설명한다.

현재 작업 목표: {{.Goal}}

발견 사항을 지어내지 않는다. 도구가 반환한 실제 데이터만 근거로 답한다.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir) + langDirective(resolveOutputLang())
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 일반 호출(전용 콜백 없는 쓰기 작업에 사용, 디바운스 적용)
	tsx.SetResumeTask(resume)     // set_goals 목표 추가 → 완료/일시 중지된 작업을 running으로 되돌림
	tsx.SetNotifyGoal(notifyGoal) // set_goals 목표 추가 → planner에 “사용자가 목표 추가: …” 트리거 기록
	tsx.SetNotifyHint(notifyHint) // add_hint 힌트 추가 → planner에 “사용자가 전략 힌트 N개 추가: …” 트리거 기록
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// 도메인 도구 + 기본 도구 모음(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets를 제외한다(프롬프트에 포함하지 않음).
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 이 작업의 디렉터리 <workDir>/tasks/<taskID>를 미리 생성한다.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 거쳐 이력을 남기며 프록시 CA로 MITM 재서명 HTTPS 인증서를 검증한다
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// 웹 검색(선택). ddgs는 키 불필요, brave-free는 BraveKey, tavily는 TavilyKey 필요.
		// WebSearchProxy는 독립 외부 연결 프록시(http/https/socks5)이며 트래픽 기록용 MITM 프록시와 무관하다. 비어 있으면 직접 연결한다.
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash 하위 명령은 기본적으로 프록시를 거치며 CA를 신뢰한다
		WorkingDir:            mainDir,                              // 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // 세션 임시 할 일(TodoWrite). 계획 전용이며 종료 시 폐기한다
		// 단계 예산 도달 → SDK 마무리: 사용자에게 한 문장 진행 요약. 프롬프트와 마무리 턴 수는 관리 화면에서 편집 가능(기본 10회).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // 해당 profile이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:    m.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 실험 기능: 활성화하면 noa가 문맥 압축을 담당한다(아카이브는 <workDir>/noa/<SessionID>에 모아 영구 보존).
	// session id는 transcript와 같은 분할 인식 규칙을 사용하여 아카이브와 복원을 맞춘다.
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
