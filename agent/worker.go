package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// deepseek 백엔드는 다른 세 종류와 성격이 다르다. DeepSeek에는 직접 호출 가능한 검색 API가 없으며
// 검색은 Anthropic 호환 messages API 내부의 web_search_20250305 server
// tool에만 있다. 따라서 검색마다 모델 호출 1회가 필요하고 요청은 DeepSeek 서버에서 보내므로
// 로컬 Proxy를 거치지 않으며 트래픽 이력에도 남지 않는다.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek*는 현재 활성 LLM 설정에서 가져온다(anthropic 형식의 DeepSeek 공식 엔드포인트만).
	// 별도로 설정하지 않으며 LLM 설정 전환에 따라 바뀐다.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "이번 실행이 예산 소진으로 곧 종료됩니다. 더 이상 어떤 명령이나 탐지도 실행하지 마십시오. 다음 순서대로 처리하십시오. (1) 위에서 이미 식별했지만 아직 기록하지 않은 내용을 하나씩 기록합니다. 새 자산은 insert_assets, 탐색 결론과 사실은 record_fact, 확인된 취약점은 report_finding 으로 기록합니다. (2) **맨 마지막에 한 문장짜리 순수 텍스트로만** 무엇을 했고 어떤 핵심 결론을 얻었는지 한국어로 요약합니다. 이 한 문장이 이번 실행의 결과로 사용자에게 표시되므로 반드시 출력해야 합니다."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 중간 산출물 출력 규약
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (구간 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `너는 사이버 보안 플랫폼의 승인된 침투 테스트 시스템에서 "실행자"(work agent)다. 【의도 하나】(한 문장으로 된 탐색 방향)를 배정받았으며 유일한 역할은 **그 의도 하나를 완료하고, 발견 사항을 지식 그래프에 기록한 다음, 중지하고 반환하는 것**이다.

**경계(금지선)**:
1. **배정받은 의도 하나만 수행한다**. **이 의도를 탐색하다 범위 밖에서 깊이 조사할 가치가 있는 단서**(오류가 노출한 경로, 다른 자산과 연계될 수 있는 지점, 다른 악용 체인의 의심 입구)를 발견하면 **fact의 summary에 한 문장으로 적어 계획자에게 전달한다**.
2. 첫 장애(payload 필터링 / 404 / 주입 결과 미표시)는 탐색 완료를 뜻하지 않는다. 이 의도의 모든 우회 수단을 시도한 뒤 결론을 출력한다.
3. 승인 범위 안에서만 행동한다. 시스템 프롬프트 상단에 【행동 제약】이 붙어 있으면 최우선 금지선이다. 각 명령/탐지 실행 전에 스스로 점검하고 위반한다면 실행하지 않는다(배정된 의도 안의 행동이어도 마찬가지다).

**발견하는 즉시 기록한다**(그래프에 써야 유효하며 머릿속이나 텍스트만으로는 안 된다. 결과를 얻을 때마다 즉시 기록하고 마지막까지 모았다가 단계 예산 소진으로 잃지 않는다). 다음 세 종류의 기록을 구분하여 그래프를 혼동하지 않는다.
- **새 자산/리소스 → insert_assets(자산 그래프)**: 하위 도메인 / service / endpoint / 지문 / 자격 증명 등 모든 자산 **자체**. **여기에는 자산만 등록한다. 탐색 결론/판단은 여기에 쓰지 말고 record_fact를 사용한다.**
- **탐색 결론/사실 → record_fact(탐색 그래프, intent_id 전달)**: 모두 이 도구를 사용한다. **여러 관측을 【사실 하나】로 요약한다**(summary는 한 문장 요약, detail은 실제 실행 과정에 근거한 요약의 상세 설명). 속성마다 하나씩 기록하지 않는다. 보통 의도 하나당 사실 하나다. 잘게 쪼개면 그래프가 무한히 커지므로 **기본적으로 하나만 쓰고 detail에 합칠 수 있는 내용은 모두 합친다**. 【완전히 독립적이어서 합칠 수 없는】 결론이 있을 때만 facts 배열로 나눈다. 이는 매우 드문 예외이며 일반 규칙이 아니다. **새로운 내용만 기록한다**. 이번에 【새로 얻은】 것만 기록하고 기존 사실을 표현만 바꿔 다시 쓰지 않는다(기존 내용을 확인했을 뿐 새 내용이 없다면 기록할 필요 없다). **실제로 관측한 내용만 쓴다**. evidence(한 줄: 명령+가장 잘 입증하는 출력 한두 줄, 간결하게 쓰고 상세는 detail에 둔다)를 제공하고 confidence(observed=직접 관측 / inferred=현상에서 추론)를 표시한다.
- **확인된 취약점 → report_finding(탐색 그래프, PoC 포함, intent_id 전달)**: **이번에 실제로 발동시켰고 재현 가능한 증거(요청/응답 또는 명령 출력)를 얻은 경우에만 사용한다**. "버전/지문이 CVE와 일치", "매개변수가 주입 가능해 보임", "외부 취약점 DB/변경 로그/코드 diff에서 추론"을 확인된 것으로 취급해서는 안 된다. CVE DB 조회나 패치 버전 비교로 실제 발동 검증을 대신하지 않는다. 발동시키지 못했으나 의심된다면 → record_fact로 inferred 사실 하나(의심 지점+발동하지 못한 이유)를 기록하여 계획자에게 전달하고, 억지로 finding으로 기록하지 않는다.


이 의도를 완료하면 무엇을 했고 어떤 사실을 기록했는지 한 문장으로 요약한다.`

// workerTrafficBlock is 구간 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**트래픽 도구**:\n- traffic_search / traffic_get / traffic_blob: 응답을 다시 보고 이미 방문한 리소스를 찾는다. **먼저 트래픽을 조회하고 같은 URL을 curl로 반복 요청하지 않는다**. traffic_search는 **host 지정이 필수**이며 기본적으로 초경량 인덱스 3개만 반환한다(id/method/url/status/resp_len, 응답 내용 없음). 더 필요하면 limit을 명시적으로 늘린다. body_contains로 요청/응답 본문 전문을 검색할 수 있다(최소 3자, 부분 문자열과 중국어 지원; 예: 비밀번호/키/오류/내부망 주소 검색). 개별 원문은 traffic_get(id)로 조회한다. 매우 큰 본문은 @blob sha256:<hash>로 표시되며 traffic_blob(hash)로 나눠 전문을 가져온다."
}

// artifactSpec is 구간 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**중간 산출물 출력 규약**: 스크립트, payload, 수집한 응답 본문, 임시 데이터 등 모든 중간 산출물은 **반드시 이 작업의 작업 디렉터리 " + dir + "에 쓴다**(상대 경로는 여기에 기록되며 이 절대 경로도 사용 가능). **/tmp에 쓰거나 다른 절대 경로를 사용하지 않는다**."
}

// workerArtifactSpec is the worker's 구간 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**중간 산출물 출력 규약**: 스크립트, payload, 수집한 응답 본문, 임시 데이터 등 모든 중간 산출물은 **반드시 이번 의도의 전용 작업 디렉터리 " + runDir + "에 쓴다**(이미 자동 생성되었으므로 상대 경로로 바로 기록하며 수동으로 디렉터리를 만들 필요 없음). **/tmp에 쓰거나 다른 절대 경로를 사용하지 않는다**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir) + langDirective(resolveOutputLang())
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【배정받은 의도(이번 유일한 작업: 이것만 수행하고 사실만 생성하며 완료 즉시 중지)】:\n%s\n의도 id: %d(record_fact / report_finding 기록 시 전달)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage는 계획자가 어떤 유형을 적게 검사했는지/범위를 넓힐지 판단하는 신호로, worker의 “배정된
	// 의도만 수행하고 미커버 지점을 쫓지 않음” 경계와 상충하여 worker 뷰에서 제거한다. data는 이 worker
	// 전용의 새 map이므로 키 삭제가 planner에 영향을 주지 않는다.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【전체 탐색 상황(읽기 전용이며 이 의도를 전체 맥락에서 이해하는 데 사용)】:\n" +
		"아래는 전체 작업의 현재 탐색 개요다. 목적은 두 가지다. 다른 실행자가 이미 발견한 내용을 알아 중복을 피하고, 자신의 의도를 탐색할 때 전체 상황과의 관계를 떠올리는 것이다.\n" +
		"**발상을 넓히는 것은 좋다**. 이 의도를 탐색하며 깊게 생각하고 다양하게 연관 지어도 된다. 유일한 경계는 다른 의도를 실제로 실행하지 않는 것이다(다른 worker의 일이며 계획자가 배정함). 가치 있는 단서(자산 간 연계, 다른 악용 체인의 의심 입구, 전역적으로 의심스러운 지점)가 떠오르면 **반드시 fact에 기록하여 계획자에게 전달한다**. 이는 선택 사항이 아닌 중요한 산출물이다. 혼자 삼키기보다 하나 더 보고하여 계획자가 판단하게 한다.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding 저장 즉시 planner를 호출하며 의도와 finding을 함께 전달한다
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker에는 의도적으로 MultiEdit/Glob/Grep을 주지 않는다. 정밀 수정은 Edit, 검색은 Bash(grep/find)로
	// 도구 범위를 좁혀 저가치 호출을 줄인다. 나머지 SDK 기본 도구(Read/Write/Edit/LS/Bash/Sleep)는 유지한다.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 의도는 worker의 유일한 역할이며 실행 내내 유지할 불변 조건이다. 시작 지시, 의도에 연결된 대상 자산의
	// 원본 데이터와 함께 system prompt에 둔다. system은 run마다 재구성되어 compaction으로 제거되지 않으므로
	// 긴 실행에서도 의도가 항상 남고 이어 실행할 때 transcript가 첫 메시지를 보존했는지에 의존하지 않는다.
	// 대신 system에 의도별 가변 데이터가 섞여 의도 간 캐시 재사용을 잃는다. 토큰 절약보다 의도 유실이 심각하므로 의도한 절충이다.
	// planner가 상황 블록을 user turn에 두는 것과 의도적으로 다르다. planner는 의도를 생성하므로 단일 임무가 없고
	// worker는 있다. 전체 상황 overview만 시작 user 메시지에 둔다. 이 정보는 오래되거나 압축되어도 무방하다.
	// 이번 의도의 전용 작업 디렉터리 <workDir>/tasks/<taskID>/i<intentID>는 엔진에서 미리 생성한다.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 행동 제약이 있으면 시스템 프롬프트에 주입하며 worker는 실행 시 엄격히 준수한다
	}
	// 의도 블록 → 연결 자산 블록 → 시작 지시를 system 끝에 순서대로 추가한다(constraintBlock과 같은 방식).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\n이 의도의 asset_ids에 해당하는 대상 자산:\n" + string(b)
				}
				// 의도가 명시적으로 겨냥한 자산을 작업 테스트 범위에 자동 포함한다(insertAssets와 같은
				// 보수적 단위). upsertTaskScope의 ON CONFLICT DO NOTHING + uq_task_scope
				// 고유 인덱스로 중복 추가를 막으며 재실행/재시도도 멱등 no-op이다.
				// 자산 커버리지 기능이 꺼지면 테스트 범위(분모)를 더 이상 누적하지 않는다.
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n위 의도를 실행한다. 이것만 수행하고 사실, assets, finding만 생성하며 완료 즉시 중지한다."
	system, boundary := deferredSystem(sysBody, def)
	// ctx로 주입한 작업 deadline이 이번 실행의 실제 시간 예산을 제한하고 마무리 문구를 결정한다(taskclock.go 참조).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch는 기록 프록시를 거쳐 HTTP를 curl과 똑같이 기록한다. 프록시 CA를 로드하여 MITM으로
		// 재서명된 HTTPS 인증서를 검증을 끄지 않고 정상 검증한다. proxy가 비면 직접 연결한다.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// 웹 검색(선택). ddgs는 키 불필요, brave-free는 BraveKey, tavily는 TavilyKey 필요.
		// WebSearchProxy는 독립 외부 연결 프록시(http/https/socks5)이며 트래픽 기록용 MITM 프록시와 무관하다. 비어 있으면 직접 연결한다.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 하위 명령의 HTTP는 기본적으로 기록 프록시를 거치고 CA를 신뢰한다(도구에 -x/-k 불필요).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 실제 시간 예산은 턴 경계에서 판단하며 도중에는 중단하지 않는다. 0=무제한. 작업 deadline이 있으면 min(자체 예산,
		// deadline까지 남은 시간)으로 제한하여 작업 종료 시 자연스럽게 마무리한다(taskclock.go 참조).
		MaxDuration: maxDur,
		// 예산(턴 수 또는 시간) 도달 → SDK 마무리 라운드(Bash 숨김)로 이미 식별한 내용을 기록하여 미완결을 방지한다.
		// clamped(작업 deadline으로 제한됨)이면 PromptByReason 사용: 시간 초과=작업 종료→작업 시간 초과 문구,
		// 단계 수 소진=제한된 시간 안에 단계가 먼저 소진→per-run 문구. clamped가 아니면 순수 per-run 유지.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// 전체 출력은 디스크에 보존한다. 잘라내기 상한은 SDK 기본값(30000자)을 사용한다.
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // 세션 임시 할 일(TodoWrite). 계획 전용이며 종료 시 폐기한다
		NonStreaming:  w.nonStreaming(),                       // 해당 profile이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:     w.maxTokens(),                          // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 의도 / 시작 지시 / 연결 대상 자산은 system prompt에 포함되어 있다(위 sysBody 조립 참조).
	// 시작 user 메시지는 전체 상황 overview만 담으며 압축되어도 무방한 전체 맥락 정보다.
	// 드물게 overview marshal이 실패하여 비면 첫 user 메시지가 비지 않도록 시작 문구 한 줄로 대체한다.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system에서 배정받은 의도를 실행한다. 이것만 수행하고 사실, assets, finding만 생성하며 완료 즉시 중지한다."
	}

	// 실험 기능: 활성화하면 noa가 문맥 압축을 담당한다(아카이브는 <workDir>/noa/<SessionID>에 모아 영구 보존).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		if message == "" {
			records, loadErr := w.tx.Load(w.tx.MainPath(opts.SessionID))
			if loadErr != nil {
				return harness.ReasonModelError, tsx.Writes(), fmt.Errorf("read worker transcript: %w", loadErr)
			}
			history, _ := transcript.Messages(records)
			if resumeErr := SafeToolResume(history); resumeErr != nil {
				return harness.ReasonModelError, tsx.Writes(), resumeErr
			}
		}
		if resumeErr := s.Resume(opts.SessionID); resumeErr != nil {
			return harness.ReasonModelError, tsx.Writes(), resumeErr
		}
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = AutomaticContinuation
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "이전 사람 대화에서 입력된 새 의도를 이어 실행한다. 이미 완료한 행동을 반복하지 않는다."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【사람 대화에서 입력된 새 의도】\n" + message +
				"\n\n이 사람 입력에 따라 즉시 실행하고, 완료 후 문맥에 따라 원래 작업을 계속해야 할지 결정한다."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【사람 대화에서 입력된 새 의도】\n" + message +
				"\n\n이 사람 입력을 우선 실행한다."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
