package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 서버에서 5계층 재시도 정책을 해석합니다. LLM 재시도 설계 참고:
// - 연결/빈 응답/같은 provider 안전 구간은 엔드포인트별이며 각 LLM 설정이 전역 기본값을
// 재정의할 수 있습니다. 설정에 없으면 전역, 전역에도 없으면 내장 기본값을 사용합니다.
// - 회로 차단과 의도 재실행은 프로세스 전체의 전역 설정만 사용합니다.
//
// 전역 정책은 DB settings 행 하나를 읽습니다. provider 구성/work 마무리/설정 저장처럼 호출 빈도가 낮아
// 추가 캐시는 필요 없습니다. 다만 회로 차단 매개변수는 실패 때마다 읽으므로
// applyRetryPolicy로 Registry에 전달해 저장합니다.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 횟수의 0=기본값, 음수=끄기 의미를 유지합니다. SDK MaxRetries와
		// EmptyResponseRetries도 같은 의미이므로 SDK가 해석하도록 둡니다.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 회로 차단 대기 기본값은 llmpool 내장값과 같고 사용자가 값을 지정했을 때만 덮어씁니다.
// 의도 재실행 기본값은 engine.go의 modelErrorRetries / modelErrorRetryBackoff를 참고하세요.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// 주입합니다(steerHooks.Stop 참고). 의도적으로 ②계층의 빈 응답 재시도 횟수를 공유합니다.
// 같은 목적의 두 방식입니다. SDK는 내용 블록이 전혀 없을 때 같은 요청을 그대로 보내고
// 여기서는 생각만 있고 본문/도구가 없을 때 지시를 추가하여
// 기존 생각에서 계속 행동하게 합니다. 컨텍스트 형식 때문에 생긴 무응답에는 동일 재전송이 효과가 없습니다.
// SDK는 이벤트 yield 여부로 판단하며 생각 증분도 이벤트이므로 빈 응답 판정은 다르지만 사용자는
// 실질적인 결과가 없으면 다시 시도한다는 의미로 횟수를 설정하므로
// 두 계층이 같은 횟수를 공유해야 합니다.
//
// 설정별 재정의 대신 전역 정책을 읽습니다. run 중 장애 조치로 profile이 바뀔 수 있지만
// 이 상한은 의도 전체의 총량이므로 엔드포인트 변경에 따라 변하면 안 됩니다. SDK emptyRetries()와 같은 의미입니다.
// 0=defaultEmptyTurnNudges, 음수(-1)=계속 실행 끄기, >0=지정 횟수.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
