package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter는 notification_channels.filter JSONB 열의 계약이며 채널 인스턴스 필터 조건이다.
// 모든 필드는 선택 사항이다. 생략은 필터 없음이며 잘못된 설정의 기본 처리도 동일하다(ParseFilter 참조).
type Filter struct {
	// MinSeverity는 최소 심각도(low/medium/high/critical). 빈 값이면 제한 없음.
	MinSeverity string `json:"min_severity"`
	// TaskIDs/AssetIDs 빈 배열은 제한 없음. 값이 있으면 이벤트와 교집합이 필요하다.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude가 비면 모두 수신. 값이 있으면 vulnclass가 키워드 하나 이상에 일치해야 한다.
	// VulnClassExclude 중 하나라도 일치하면 제외한다(제외가 포함보다 우선).
	// 대소문자 무시 부분 문자열로 일치시켜 잘못된 정규식으로 채널이 조용히 실패하는 일을 피한다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange는 취약점 상태 변경 수신 여부(realtime 모드 전용)다.
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter는 채널 필터 설정을 해석한다.
//
// error를 반환하지 않는 것은 의도된 설계다. 잘못된 필터는 영값 Filter로 대체한다.
// 즉 필터 없이 모두 일치한다. 취약점 알림 시스템에서는 높은 위험 알림을 조용히 놓치는 것보다
// 하나 더 보내는 것이 낫다. 해석 실패를 발송하지 않음으로 처리하면 설정은 된 것처럼 보이나
// 실제로 아무것도 보내지 않는 최악의 실패가 된다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 해석 실패 시 영값 f를 유지해 필터링하지 않는다.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity는 유효한 심각도 임계값인지 반환한다(빈 문자열은 제한 없음).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate는 설정 저장 시 값이 제한된 필드를 검증한다.
//
// 저장 단계에서 차단해야 하는 이유: Match는 알 수 없는 임계값을 `rank >= 0`으로 판단해 항상 참이다.
// 따라서 min_severity 오타("hgih")는 필터를 조용히 무효화해 전부 보내게 된다.
// 이는 누락보다 추가 발송을 우선하는 정책과 맞지만 사용자는 심각도별 발송이라고 생각하면서
// 실제로 모든 취약점을 그룹에 보내고 잘못된 설정이라는 단서도 받지 못한다.
// 이런 조용한 대체 동작은 입력 지점에서 차단해야 한다.
//
// Validate는 쓰기 경로 전용이다. 읽기는 ParseFilter의 관대한 규칙을 유지해
// 기존 잘못된 데이터로 채널 전체를 읽지 못하는 일이 없게 한다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("올바르지 않은 최소 심각도입니다: %q. low / medium / high / critical 중에서 선택하거나, 비워 두면 제한하지 않습니다", f.MinSeverity)
	}
	return nil
}

// Match는 이벤트를 해당 필터 채널로 발송할지 판단한다.
//
// ParseFilter와 같은 이유로 error를 반환하지 않고 내부 예외는 일치로 처리한다.
// 판정 순서: 이벤트 유형 → 심각도 임계값 → 작업/자산 범위 → 취약점 유형 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경은 명시적으로 켠 채널만 수신한다. 대부분 사용자는 알림을 새 취약점 발견으로
	// 기대하므로 모든 상태 전환을 추적하는 기능은 기본으로 끈다.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 제외 우선: 제외 키워드에 일치하면 포함 목록에도 일치하더라도 제외한다.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 양쪽 모두 사람이 선택한 수십 개 규모이므로 선형 탐색이면 충분하며
	// map 생성 비용이 이득보다 크다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold는 s에 keywords 중 하나가 포함되는지 대소문자 무시로 확인한다.
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
