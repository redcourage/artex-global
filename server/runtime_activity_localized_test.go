package server

import (
	"fmt"
	"strings"
	"testing"
)

// 런타임 활동 요약이 한국어인지 검증합니다. 작업 단위 활동이며 agent 컨텍스트 조회는 의도 범위에서 이루어집니다.
// 모델용 초기 의도 요약과 기본 설명도 한국어이지만 이 테스트는 표시 요약 상수를 검사합니다.
func TestRuntimeActivitySummariesLocalized(t *testing.T) {
	// 포맷 인자 없는 고정 요약: 그대로 한국어여야 한다.
	plain := map[string]string{
		"goalless_task_done": goallessTaskDoneSummary,
		"goal_breakdown_r0":  goalBreakdownRound0Summary,
		"queued_no_llm":      queuedNoLLMSummary,
		"queued_fifo":        queuedFIFOSummary,
	}
	for label, msg := range plain {
		assertKoreanError(t, label, msg)
	}

	// %d 포맷 문자열: 포맷 인자가 살아 있고, 포맷한 결과도 한국어이며 숫자가 실제로
	// 반영되는지 확인한다.
	fmtCases := map[string]string{
		"planner_round":       plannerRoundSummaryFmt,
		"timeout_final_round": timeoutFinalRoundSummaryFmt,
		"queued_conc_limit":   queuedConcurrencyLimitSummaryFmt,
	}
	for label, f := range fmtCases {
		if !strings.Contains(f, "%d") {
			t.Fatalf("%s: 포맷 인자 %%d 가 사라졌습니다: %q", label, f)
		}
		got := fmt.Sprintf(f, 7)
		assertKoreanError(t, label, got)
		if !strings.Contains(got, "7") {
			t.Fatalf("%s: 포맷 인자가 결과에 반영되지 않았습니다: %q", label, got)
		}
	}
}
