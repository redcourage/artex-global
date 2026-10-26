package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel은 알림 채널 어댑터다. 같은 인스턴스를 여러 설정에서 동시에 재사용하므로
// 구현은 무상태여야 하며 자격 증명은 cfg로 전달한다.
type Channel interface {
	// Kind는 등록표 key와 일치하는 채널 유형 식별자를 반환한다.
	Kind() string
	// Validate는 설정 저장 시 필수 필드와 형식을 검증한다. 오류를 설정자에게 직접 표시하므로
	// 단순한 설정 오류 대신 누락된 필드를 구체적으로 설명해야 한다.
	Validate(cfg map[string]any) error
	// Send는 메시지를 발송하고 실제 전달한 항목 수와 오류를 반환한다.
	//
	// 개수를 반환하는 이유: 플랫폼별 길이 제한으로 요약 메시지에 전체 묶음이 들어가지 않을 수 있다.
	// 전체를 무조건 전달됨으로 표시하면 잘린 항목은 메시지에 없고 발송 기록은 성공이라
	// 전달되지 않은 취약점을 찾을 수 없다. kept를 반환하면 호출자가
	// 앞 kept개만 완료 처리하고 나머지는 다음 묶음에 남긴다.
	//
	// 오류는 발송 실패를 의미하며 *PermanentError는 재시도하지 않아야 한다.
	// 실패 시 kept는 의미 없으므로 무시해야 한다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin은 채널의 공식 권장 분당 발송 한도를 반환해 새 인스턴스의
	// 기본 속도 제한으로 사용한다. 0이면 알려진 제한 없음.
	DefaultRatePerMin() int
	// SecretKeys는 자격 증명인 설정 key를 반환한다. API 응답에서는 값을 마스킹하며
	// 갱신 시 마스킹 값을 받으면 DB 원본을 유지한다. 어떤 필드가 자격 증명인지는 구현만 안다.
	// 예를 들어 WeCom은 Webhook 전체, DingTalk는 그중 secret이 해당한다.
	// 따라서 상위 계층이 추측하지 않고 채널이 이 정보를 제공해야 한다.
	SecretKeys() []string
	// DestinationKeys는 메시지 목적지를 결정하는 설정 key를 반환한다.
	//
	// SecretKeys와 마찬가지로 보안 관련 정보다. 주소와 자격 증명은 별도 필드이므로
	// 주소만 바꾸고 자격 증명을 유지하도록 허용하면 설정 편집자가 DB의 실제 자격 증명을
	// 자신의 서버로 보내게 할 수 있어 마스킹이 무의미해진다.
	// PrepareConfigUpdate 참조.
	DestinationKeys() []string
}

// registry는 명시적 리터럴 채널 등록표다. init() 자동 등록 대신 한곳에서 모든 채널을 확인하고
// 추가 시 누락을 런타임 부작용이 아닌 컴파일 단계에서 드러내기 위해 사용한다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get은 유형별 채널 구현을 반환한다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind는 지원하는 채널 유형인지 반환한다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds는 모든 지원 채널을 사전순으로 반환한다(UI 드롭다운 안정 표시용).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError는 자격 증명 오류/대상 거부/잘못된 본문처럼 재시도하지 않을 실패를 표시한다.
// 재시도는 순간 장애(네트워크 흔들림/속도 제한/상대 5xx)에만 의미가 있다. 영구 실패를 반복하면
// 성공하지 못하면서 실제 오류가 재시도 로그에 묻힌다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent는 err를 영구 실패로 표시한다. nil이면 nil을 반환해
// `return Permanent(someCheck())`로 작성할 수 있다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent는 err 체인에 영구 실패 표시가 있는지 반환한다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기 보조 함수 ----
//
// 채널 설정은 DB JSONB를 encoding/json으로 역직렬화한 map[string]any다.
// 숫자는 float64, 배열은 []any이므로 아래 보조 함수가 변환을 통일하고 UI의 빈 입력 등으로
// 생기는 유형 차이(예: 문자열 포트)도 허용한다.

// cfgString은 문자열 설정의 앞뒤 공백을 제거한다. 웹 양식 붙여넣기에 공백이 섞이기 쉽다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt는 정수 설정을 읽으며 JSON 기본 float64와 문자열을 지원한다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool은 불리언 설정을 읽으며 문자열 "true"/"1"도 지원한다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings는 문자열 배열을 읽고 공백과 빈 문자열을 제거한다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 값 하나인 양식 제출을 위해 단일 문자열도 허용한다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap은 문자열 맵(사용자 정의 HTTP 헤더 등)을 읽고 key/value 공백과 빈 key를 제거한다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
