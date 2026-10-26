// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType은 사고 활성화 필드(thinking.type)를 독립적으로 제어한다.
	//   "" = 전송 안 함(기본값, 미지원 모델 호환); "disabled" = 명시적 비활성화;
	//   "enabled" = 활성화. ReasoningEffort와 완전히 독립적이다. 일부 API는 thinking 필드 없이
	//   강도 매개변수만으로 사고를 활성화하므로 각각 별도로 설정할 수 있다.
	ThinkingType string
	// ReasoningEffort는 사고 강도 필드를 독립적으로 제어한다.
	//   "" = 전송 안 함(기본값); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도.
	//   OpenAI는 최상위 reasoning_effort, Anthropic은 output_config.effort에 매핑한다.
	ReasoningEffort string
	// Stream은 이 profile의 스트리밍(SSE) API 사용 여부를 제어한다. true(기본)=스트리밍; false=실제
	// 비스트리밍(stream:false를 보내 전체 JSON을 한 번에 받고 Provider.Complete 사용). 비스트리밍은
	// 일부 게이트웨이의 잘못된 SSE 구현(빈 프레임, 사고 필드 프레임 누락)을 우회하지만 실행 중 실시간 진행/실시간
	// token 집계를 잃는다. agentcore.Options.NonStreaming = !Stream에 매핑한다.
	Stream bool
	// MaxTokens는 응답 1회의 출력 상한(token)이다. 0이면 필드를 보내지 않고 서버 기본값에 맡긴다
	// (기존 동작). ContextWindowK는 모델 총 용량으로 로컬 압축 임계값 계산에만 쓰고 요청에 포함하지 않으나
	// 이 값은 매 요청에 보낸다. agentcore.Options.MaxTokens에 매핑한다.
	MaxTokens int
	// MaxTokensField는 MaxTokens를 보낼 요청 필드명을 선택하며 format=openai에서만 유효하다.
	//   "" = max_tokens(기본값); "max_completion_tokens" = 새 필드.
	// OpenAI 추론 모델(o 시리즈/GPT-5)은 후자만 인식하며 max_tokens를 받으면
	// unsupported_parameter 오류를 낸다. 호환 게이트웨이 대부분은 전자만 인식하므로 자동 추론 대신 사용자가 엔드포인트별로 선택한다.
	MaxTokensField string
	// SessionHeaderKey가 비어 있지 않으면 각 LLM 요청에 이 이름의 사용자 정의 HTTP 헤더를 넣으며
	// 값은 현재 세션의 session id다(chat=conv-<id>, worker=exp<x>-worker-i<intent>
	// 등, WorkerSessionID 참조). session-id 헤더로 프롬프트 캐시/고정 라우팅하는 게이트웨이에 사용한다.
	// 비어 있으면 보내지 않는다. transcript.WithSessionID로 요청 context에 넣고 RoundTripper가
	// 읽어 채우므로 공유 provider도 세션별로 다른 헤더 값을 보낼 수 있다.
	SessionHeaderKey string
	// Retry는 이 설정의 해석된 재시도 매개변수다(profile 덮어쓰기 → 전역 정책 → 내장 기본값 순으로
	// 서버에서 해석). 세 계층 의미는 RetryConfig 참조. 영 값이면 내장 기본값을 그대로 사용한다.
	Retry RetryConfig
}

// RetryConfig는 LLM 설정에 딸린 재시도 매개변수다. 각 계층의 횟수 의미는 동일하다.
// 0=내장 기본 횟수; 음수=해당 계층 재시도 비활성화; >0=지정 횟수. 각 계층의 간격은
// 0=기존 지수 백오프; >0=지정한 고정 간격을 사용한다.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK 연결 재시도(스트림 시작 전 연결 초기화/시간 초과/429/5xx).
	// llm.Config.MaxRetries / RetryInterval에 직접 매핑한다. 기본 3회, 0.5초부터 지수 증가(최대 8초).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK 빈 응답 재시도(완료했으나 content block 없음, openai
	// 형식만 해당). llm.Config.EmptyResponseRetries / EmptyResponseInterval에 매핑한다.
	// 기본 2회, 동일한 지수 증가를 사용한다.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: 동일 provider 안전 구간 재시도. 프로젝트가 SDK 위에 추가한
	// 계층으로 호출자에게 출력을 전혀 전달하지 않은 경우에만 단절/과부하/스트림 내 429를 재실행한다. SDK에서는 보이지 않고
	// server/task_llm.go가 사용한다. 기본 2회, 0.5초부터 지수 증가(최대 4초).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 기본 스트리밍. ARTEX_LLM_STREAM=false/0/off로 명시적으로 비활성화하면 비스트리밍.
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 기본 스트리밍이며 호출자가 profile에 따라 덮어쓴다
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 사고 활성화와 강도 필드를 각각 그대로 전달한다(비면 해당 필드 미전송). 서로 독립이므로
	// 둘 다 보내거나 둘 다 생략하거나 thinking.type 또는 effort만 보낼 수 있다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 상한 필드명 선택(비면 max_tokens). 상한 값은 여기 있지 않고 라운드마다
	// agentcore.Options.MaxTokens로 전달하며 provider는 어느 키에 넣을지만 결정한다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 매개변수는 SDK와 같은 의미로 그대로 전달한다(횟수 0=기본/음수=끔, 간격 0=지수/>0=고정).
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Usage headers are lightweight and captured even when body recording is off.
	metering := llmrec.MeteringFrom(req.Context())
	metering.CaptureHeaders(resp.Header)
	resp.Body = metering.ObserveResponse(resp)
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 비면 직접 연결하며 HTTP_PROXY/HTTPS_PROXY 환경 변수로 대체하지 않는다
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "테스트 클릭" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청을 보내지 못함(설정 해석 또는 연결 생성 시 실패)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\n응답 본문: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어 있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(잘림, 전체 %d바이트)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 원시 wire 메시지 캡처. 연결 테스트에서 중요한 게이트웨이 반환값(상태 코드+응답 본문)은
	// norma가 StreamEvent로 디코딩하면 사라진다. quotaAwareTransport가
	// context의 Capture를 찾아 HTTP 시도마다 상태 코드와 body를 채운다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 단발 경로로 agentcore 세션 루프를 거치지 않아 context에
	// session id를 넣는 곳이 없다. SessionHeaderKey가 설정된 엔드포인트(opencode zen은
	// x-opencode-session 필수, 누락 시 바로 400 MissingSessionID)에서는 대화 정상/테스트 400의 차이를 낳는다. 일회성 무작위 session id를 넣어
	// 테스트와 실제 대화가 같은 헤더 로직을 사용하게 한다. SessionHeaderKey 미설정 엔드포인트는 읽지 않아 부작용 없다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens를 충분히 준다. 추론 모델(예: deepseek-v4-pro)은 답변 전에 긴 사고를
	// 생성한다(실측 "ping"에도 약 2900 token). 32만 주면 사고 단계에서
	// 출력 상한(finish=length)에 도달하여 잘린다. 연결 자체는 성공(err=nil)이어도
	// 중단됨/length/resume로 표시된다. 충분한 예산으로 OK를 정상 완료하게 한다(finish=stop).
	// EscalateMaxTokens는 false로 유지한다. 잘림 때문에 상한을 늘려 재시도하지 않아 resume 반복 낭비를 피한다.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"너는 연결 테스트다. 두 글자 OK만 바로 출력한다. 사고하거나 설명하거나 다른 내용을 출력하지 않는다."},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 해당 profile의 실제 송수신 모드로 연결 테스트
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil만으로 충분하지 않다. 연결되었으나 모델이 아무 글자도 반환하지 않는 경우가 있다(사고로 예산 소진,
	// 정책이 본문 제거, 호환 계층이 content 누락). 대화에서는 응답이 없는데
	// 테스트에서는 성공하는 차이를 없앤다. 표시할 본문이 없으면 항상 실패로 판단한다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델 응답 내용 없음(요청은 연결되었으나 텍스트를 반환하지 않음)")
	}
	return lat, reply, nil
}
