package db

import (
	"encoding/json"
	"time"
)

// LLM 재시도 정책: 5개 계층의 횟수와 간격에 대한 전역 설정입니다. 관련 설계 문서를 참고하세요.
// 전체 프로세스에서 공유하는 실행 매개변수이므로 별도 표 대신 settings의 JSON 값 하나에 저장합니다.
// 조회 시 내장 기본값을 사용하므로 키가 없는 새 DB/미설정 상태의 동작은 기존 상수 방식과 같습니다.

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
// Attempts: 0=내장 기본 횟수, -1=해당 계층 재시도 비활성화, >0=지정 횟수
// IntervalMS: 0=기존 간격 정책(대개 지수 백오프), >0=고정 밀리초 간격
//
// 0은 미설정을 뜻하므로 명시적 비활성화에는 -1을 사용합니다.
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy는 5개 계층의 재시도 설정을 가집니다. Connect/Empty/Stream은
// per-request layers (a profile may override them, see LLMProfile.Retry);
// Breaker and Intent are process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect: SDK 연결 재시도(스트림 시작 전 연결 초기화/시간 초과/429/5xx). 기본 3회, 지수 백오프.
	Connect RetryRule `json:"connect"`
	// Empty: SDK 빈 응답 재시도(완료했지만 content block 없음, openai 형식만). 기본 2회, 지수 백오프.
	Empty RetryRule `json:"empty"`
	// Stream: 같은 provider의 안전 구간 재시도(출력 전달 전 스트림 단절). 기본 2회, 0.5초부터 지수 증가하며 최대 4초.
	Stream RetryRule `json:"stream"`
	// Breaker: 장애 조치 회로 차단. Attempts=연속 일시 실패 차단 횟수(기본 3, -1이면 일시 실패로 차단하지 않음).
	// 잔액 부족/키 무효 같은 확정 실패는 즉시 차단합니다. IntervalMS=고정 대기 시간(0이면 기본 1/5/30분 단계).
	Breaker RetryRule `json:"breaker"`
	// Intent: worker가 model_error로 종료한 뒤 의도 전체를 재실행합니다. 기본 2회, 고정 3초.
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
