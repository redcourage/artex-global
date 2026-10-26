package server

import "testing"

// reporter의 표시 이름/설명이 한국어인지 검증합니다. 모델 프롬프트도 한국어지만 이 테스트는 표시 레이블을 검사합니다.
// agent 선택과 트리거 연결은 표시 이름이 아닌 reporter key와 report_finding 호출을 사용합니다.
func TestReporterSeedLabelsLocalized(t *testing.T) {
	assertKoreanError(t, "reporter_name", reporterAgentName)
	assertKoreanError(t, "reporter_description", reporterAgentDescription)
}
