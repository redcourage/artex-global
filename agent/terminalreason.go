package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "모델이 이번 라운드를 정상 종료했지만 텍스트 요약을 남기지 않았다. 사실과 자산은 이번 도구 호출 기록을 기준으로 한다",
	harness.ReasonMaxTurns:          "단계 상한(MaxTurns)에 도달했다. SDK가 마무리를 수행하고 사실과 자산을 기록했다. 의도는 실패 대신 exhausted로 표시하여 계획자가 방향을 바꿔 계속할 수 있게 한다",
	harness.ReasonTimeout:           "단일 실행의 실제 시간 예산(MaxDuration)에 도달했다. 시간이 되면 실행 중 도구를 중단하고 즉시 마무리에 들어가 이미 식별한 사실과 자산을 기록한다. 의도는 exhausted로 표시된다",
	harness.ReasonModelError:        "모델 또는 API 호출 실패(네트워크, 인증, 속도 제한, 공급자 5xx 등). 재시도를 소진하면 의도는 blocked로 표시된다. 전송 계층 장애로 실제 탐색을 거의 하지 못한 것이므로 실행 과정(get_worker_trace)을 확인한 뒤 재배정 또는 방식 변경을 결정한다",
	harness.ReasonBlockingLimit:     "문맥 길이가 강제 상한에 도달하여 전송 전에 요청이 차단되었다. 의도를 더 좁게 나누거나 도구 반환을 압축해야 한다",
	harness.ReasonPromptTooLong:     "프롬프트가 너무 길고 문맥 압축 재시도를 소진하여 계속 실행할 수 없다",
	harness.ReasonImageError:        "현재 모델이 이번 멀티모달 내용을 지원하지 않는다. 시각 지원 모델로 바꾸거나 도구가 이미지를 반환하지 않도록 한다",
	harness.ReasonStopHookPrevented: "Stop 훅이 이번 종료를 막은 뒤 계속 진행하지 못했다. 작업 Guard 규칙이 지나치게 엄격한지 확인한다",
	harness.ReasonHookStopped:       "도구 또는 훅이 범위 밖 대상이나 금지 명령 등의 이유로 실행을 중지했다. 마지막 tool_result의 차단 설명을 확인한다",
	harness.ReasonAbortedStreaming:  "모델 출력 스트리밍 생성 단계에서 실행이 취소됨",
	harness.ReasonAbortedTools:      "도구 실행 단계에서 실행이 취소됨",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	if IsSubscriptionLimit(term.Err) {
		return SubscriptionWaitMessage, SubscriptionWaitMessage
	}
	if NeedsToolReview(term.Err) {
		return "도구 실행 결과 검토가 필요하여 작업을 일시정지했습니다.", term.Err.Error()
	}
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "취소 이유를 가져오지 못함"
		}
		stage := "실행 도중"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "모델 출력 단계"
		case harness.ReasonAbortedTools:
			stage = "도구 실행 단계"
		}
		sum = "(실행 중단: " + short + "; 중단 지점: " + stage + progressSuffix(term, tr) + ", 미완료)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(실행 예산 상한 도달(" + string(reason) + "), 마무리하여 사실 기록 완료" + progressSuffix(term, tr) + "; 이번 텍스트 요약 없음)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(텍스트 요약 없음, 종료 상태 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **종료 상태**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **중단 이유** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **중단 이유**: 확인 불가. 취소 주체가 context.WithCancelCause로 명명된 이유를 첨부하지 않았을 수 있음\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **하위 오류**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **취소 전에 생성한 부분 출력**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **실행 완료**: 모델 턴 %d회\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **이번 실행 소요 시간**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **누적 token**: 입력 %d / 출력 %d / 캐시 읽기 %d / 캐시 쓰기 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **도구 호출**: 이번 실행은 도구를 호출하기 전에 종료됨\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **중단 시 실행 중인 도구**: `%s`(%s 동안 실행, **결과 미반환**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **중단 전 마지막 도구**: `%s`(정상 반환됨)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "실행 context가 취소되었으나 하위 계층에서 Terminal 이벤트가 발생하지 않음"
	}
	return "알 수 없는 종료 상태. harness에 TerminalReason이 추가되었을 수 있으므로 reasonHint를 보완해야 함"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d회", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", 실행 시간 " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
