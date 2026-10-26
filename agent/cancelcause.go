package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "사용자가 작업을 일시 중지함",
		"사용자가 작업 제어 API(POST /api/tasks/{id}/control, action=pause)로 작업을 일시 중지했다. 이번 Planner/Worker 실행이 명시적으로 취소되며 실행 중 의도는 frontier(open)로 돌아간다. 작업 재개 후 다시 배정받아 처음부터 실행한다")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "조정 Agent가 작업을 일시 중지함",
		"조정 Agent가 pause_task 도구로 이 작업을 일시 중지했다. 이번 Planner/Worker 실행이 명시적으로 취소되며 실행 중 의도는 frontier(open)로 돌아가 재개 후 다시 실행한다")
	AbortTaskDeleted = cause("task_deleted", "작업이 삭제됨",
		"작업 삭제 중이다(DELETE /api/tasks/{id}). 삭제 차단 장치가 이 작업의 실행 중인 Planner, Worker, 주 Agent를 취소했으며 이번 실행 결과는 더 이상 사용하지 않는다")
	AbortPausedOnReload = cause("paused_on_reload", "백엔드가 작업의 일시 중지 상태를 복원함",
		"백엔드 시작 시 DB에 저장된 상태에 따라 작업의 일시 중지를 복원했다. 이번 실행은 취소되며 정상적인 복원 단계에는 실행 중인 Agent가 없다")
	AbortGoalMet = cause("goal_met", "계획자가 작업 목표 달성을 판단함",
		"계획자가 작업 목표 달성을 판단하고 작업을 done으로 바꾼 후 실행 중인 Worker를 취소했다. 이 의도들은 실패가 아니라 stopped로 표시된다")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "작업 시간 초과 마무리 대기 시간이 소진됨",
		"작업 timeout 이후 실행 중인 Worker의 정상 마무리를 기다렸지만 90초 drain 유예로도 부족하여 강제 취소했다. 의도는 exhausted로 표시되며 마무리 단계에 이미 기록한 사실과 자산은 보존된다")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "계획자가 이 의도를 종료함",
		"계획자가 kill_work로 이 의도를 명시적으로 종료했다. 보통 방향이 틀어졌거나 계속할 가치가 없음을 뜻한다. 의도는 stopped로 표시되며 자동 재배정하지 않는다")
	AbortWorkPausedByUser = cause("work_paused_by_user", "사용자가 이 Worker 의도를 일시 중지함",
		"사용자가 실행 중 Worker를 일시 중지했다. 이번 호출은 취소되고 의도는 paused로 바뀐다. 등록된 의도, 사실, 취약점, 활동 기록은 모두 보존되며 재개 후 처음부터 다시 실행한다")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "사용자가 이 Worker 의도를 삭제함",
		"사용자가 실행 중 Worker를 삭제했다. 이번 호출은 취소된다. Worker가 쓰기 구역에서 나간 뒤 서버가 사용자가 선택한 삭제 방식으로 처리한다. 소프트 삭제는 삭제 표시만 하고 모든 산출물을 보존하며 실제 삭제는 의도와 오직 그 의도만이 뒷받침하는 하위 노드를 연쇄 제거한다")
	AbortWorkFinished = cause("work_finished", "Worker가 정상 종료되어 context를 해제함",
		"Worker가 정상 종료되었고 엔진이 detachWork에서 context 자원을 해제한다. 실행 중단이 아니다. 중단 메시지에 표시된다면 취소와 종료 이벤트 간 경합이 발생한 것이다")
	AbortPausedRaceGuard = cause("paused_race_guard", "작업 일시 중지 중 새 실행 시작을 거부함",
		"작업이 일시 중지된 동안 엔진이 새 실행 context 발급을 거부하여 claim과 일시 중지 사이 경합으로 Worker가 시작되는 것을 방지한다. 이미 배정된 의도는 frontier로 돌아간다")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "사용자가 이번 대화를 중지함",
		"사용자가 중지 버튼을 눌러 이번 주 Agent 또는 대화 Agent 실행을 명시적으로 중단했다. 기존 활동 기록은 보존되며 다음 메시지를 계속 보낼 수 있다")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "작업 일시 중지로 주 Agent 대화가 중단됨",
		"사용자가 작업을 일시 중지할 때 실행 중인 주 Agent 대화도 함께 취소되었다. 활동 기록은 보존되며 작업 재개 후 이번 메시지를 자동 재실행하지 않는다")
	AbortChatTurnFinished = cause("chat_turn_finished", "이번 대화가 정상 종료되어 context를 해제함",
		"이번 대화가 정상 종료되어 서버가 해당 context 자원을 해제 중이다. 실행 중단이 아니다. 중단 메시지에 표시된다면 취소와 종료 이벤트 간 경합이 발생한 것이다")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "백엔드 프로세스 종료 중",
		"백엔드가 SIGINT 또는 SIGTERM을 수신하여 재시작, 업데이트 또는 종료 중이다. 실행 중인 모든 Agent가 취소된다. 재시작 후 남은 running 의도는 open으로 초기화하여 다시 실행한다")
	AbortRunHardTimeout = cause("run_hard_timeout", "단일 실행의 강제 시간 초과 보호가 발동함",
		"단일 실행이 소프트 실제 시간 예산과 추가 유예를 초과했다. 모델 요청이나 도구가 오래 반환하지 않아 정상적인 턴 경계 마무리를 수행할 수 없다는 뜻이다. 중단 전 마지막으로 반환하지 않은 도구 호출을 집중 확인한다")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "상위 context가 deadline에 도달함",
			"상위 context가 deadline에 도달했으나 설정 주체가 WithTimeoutCause로 명명된 이유를 첨부하지 않음: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "취소 주체가 명명된 이유를 첨부하지 않음",
			"상위 context가 취소되었으나 취소 주체가 context.WithCancelCause로 명명된 이유를 첨부하지 않았다. agent/cancelcause.go에 이유를 등록하고 해당 취소 지점에 연결한다", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
