// Package notify는 취약점 발견의 IM/이메일 알림 채널 어댑터 계층을 구현한다.
//
// 구조: 이 패키지는 표준 라이브러리만 사용하는 말단 패키지이며 DB와 server를 모른다. 채널 설정은
// map[string]any로 전달(notification_channels.config JSONB 열에 대응)하고
// 발송 내용은 Message로 전달한다. 서명 계산, UTF-8 자르기, 필터 일치처럼 오류가 나기 쉬운 부분을
// PostgreSQL 없이 단위 테스트할 수 있으며 호스트는 server 측에서 실행만 조정한다.
//
// 동시성 규칙: Channel 구현은 상태가 없어야 한다. 같은 인스턴스를 여러 채널 설정과
// 같은 채널의 여러 봇 인스턴스가 동시에 재사용하므로 모든 자격 증명은 cfg로 전달한다.
// webhook URL 등을 구현체 필드에 캐시하면 안 된다.
package notify

// 채널 유형 식별자. notification_channels.kind의 허용값이며 server 측에서
// 허용 목록으로 검증한다(findings.status처럼 DB CHECK 없이 채널 추가를 용이하게 함).
const (
	KindDingTalk = "dingtalk" // DingTalk 사용자 정의 봇.
	KindFeishu   = "feishu"   // Feishu(Lark 포함) 사용자 정의 봇.
	KindWeCom    = "wecom"    // WeCom 그룹 봇.
	KindWebhook  = "webhook"  // 일반 Webhook: 사용자 정의 메서드/헤더/JSON 템플릿.
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP 이메일.
)

// 이벤트 유형. notification_events.kind에 대응.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind는 config의 kind가 비어 있을 때 사용하는 기본값이다.
const InitKind = KindDingTalk

// severityRank는 취약점 심각도를 비교 가능한 순위로 변환한다. 알 수 없는 값은 0이므로
// 어떤 min_severity 설정에서도 제외해 불확실한 알림이 대량 발송되지 않게 한다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank는 심각도 순위를 반환한다. 알 수 없는 값은 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// Lang, when wired by the server, returns the active user-facing output language
// ("en"/"ko"/"zh"/"es"/"ja") for the strings this leaf package renders into push /
// email messages (severity and status labels). Kept as an injected func so the
// package stays standard-library-only: it does not import config/db/server. nil
// (unit tests / standalone) → labelLang falls back to "ko", preserving the Korean
// localization golden tests.
var Lang func() string

func labelLang() string {
	if Lang != nil {
		switch v := Lang(); v {
		case "en", "ko", "zh", "es", "ja":
			return v
		}
	}
	return "ko"
}

// severityLabels / statusLabels hold the per-language display strings. "ko" keeps
// the exact Korean values the localization golden tests pin.
var severityLabels = map[string]map[string]string{
	"ko": {"critical": "🔴 심각", "high": "🟠 높음", "medium": "🟡 중간", "low": "🔵 낮음"},
	"en": {"critical": "🔴 Critical", "high": "🟠 High", "medium": "🟡 Medium", "low": "🔵 Low"},
	"zh": {"critical": "🔴 严重", "high": "🟠 高危", "medium": "🟡 中危", "low": "🔵 低危"},
	"es": {"critical": "🔴 Crítico", "high": "🟠 Alto", "medium": "🟡 Medio", "low": "🔵 Bajo"},
	"ja": {"critical": "🔴 重大", "high": "🟠 高", "medium": "🟡 中", "low": "🔵 低"},
}

var statusLabels = map[string]map[string]string{
	"ko": {"pending": "처리 대기", "in_progress": "처리 중", "confirmed": "확인됨", "resolved": "처리됨", "fixed": "수정됨", "false_positive": "오탐", "ignored": "무시", "duplicate": "중복", "risk_accepted": "위험 수용"},
	"en": {"pending": "Pending", "in_progress": "In progress", "confirmed": "Confirmed", "resolved": "Resolved", "fixed": "Fixed", "false_positive": "False positive", "ignored": "Ignored", "duplicate": "Duplicate", "risk_accepted": "Risk accepted"},
	"zh": {"pending": "待处理", "in_progress": "处理中", "confirmed": "已确认", "resolved": "已处理", "fixed": "已修复", "false_positive": "误报", "ignored": "忽略", "duplicate": "重复", "risk_accepted": "接受风险"},
	"es": {"pending": "Pendiente", "in_progress": "En curso", "confirmed": "Confirmado", "resolved": "Resuelto", "fixed": "Corregido", "false_positive": "Falso positivo", "ignored": "Ignorado", "duplicate": "Duplicado", "risk_accepted": "Riesgo aceptado"},
	"ja": {"pending": "対応待ち", "in_progress": "対応中", "confirmed": "確認済み", "resolved": "対応済み", "fixed": "修正済み", "false_positive": "誤検知", "ignored": "無視", "duplicate": "重複", "risk_accepted": "リスク受容"},
}

// SeverityLabel은 현재 출력 언어의 emoji 포함 심각도 이름을 반환해 제목과 카드 색상에 사용한다.
// 알 수 없는 심각도는 추정하지 않고 그대로 표시한다.
func SeverityLabel(severity string) string {
	if v, ok := severityLabels[labelLang()][severity]; ok {
		return v
	}
	return severity
}

// StatusLabel은 처리 상태를 현재 출력 언어로 번역해 상태 변경 메시지에 사용한다. 알 수 없는 값은 그대로 표시한다.
func StatusLabel(status string) string {
	if v, ok := statusLabels[labelLang()][status]; ok {
		return v
	}
	return status
}

// AtLeast는 severity가 min 이상인지 판단한다. min이 비어 있으면 모두 통과한다.
// 알 수 없는 severity는 순위 0이라 비어 있지 않은 min에 의해 거부된다(severityRank 참조).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
