package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 생각만 있고 본문/도구가 없는 턴의 감지와 계속 실행을 검증합니다(steerHooks.Stop 참고).

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "먼저 포트 스캔"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"생각만 있음", []llm.Message{llm.UserText("시작"), assistantThinking("생각")}, true},
		{"생각+도구", []llm.Message{llm.UserText("시작"), toolUse}, false},
		{"생각+본문", []llm.Message{assistantThinking("생각"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("결론")},
		}}, false},
		{"본문에 공백만 있음", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"완전히 빈 assistant 턴", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// 도구 결과는 user 역할이므로 마지막 메시지만 보지 않고 그 앞의 assistant까지 역추적해야 합니다.
		{"마지막 항목이 도구 결과", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant 메시지 없음", []llm.Message{llm.UserText("시작")}, false},
		{"빈 기록", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks는 설정 가능한 inner HookRunner이며 steerHooks가 inner 결정을 존중하는지 검증합니다.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("먼저 서브도메인을 열거해야 합니다")}

	t.Run("무응답 턴에 계속 실행 지시 주입", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("무응답 턴을 강제 중지해서는 안 됩니다")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("본문이나 도구가 있으면 개입하지 않음", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("스캔을 완료했으며 열린 포트가 없습니다")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("정상 종료를 무응답으로 잘못 판단했습니다: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("개입하지 않으면 집계해서는 안 됩니다, 결과 %d", n)
		}
	})

	t.Run("상한 도달 후 종료 허용", func(t *testing.T) {
		const limit = 5 // 사용자가 빈 응답 재시도 횟수를 5로 설정한 경우
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d번째는 아직 예산 안이어야 합니다, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("상한을 넘었는데 계속 주입합니다: %v", blocking)
		}
	})

	// 빈 응답 재시도를 -1로 설정하면 이 계층을 끄고 emptyTurnNudgeLimit는 0이 됩니다.
	t.Run("설정이 꺼져 있으면 개입하지 않음", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("비활성 상태인데 계속 주입합니다: %v", blocking)
		}
	})

	t.Run("inner의 강제 중지 결정에 추가하지 않음", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard가 종료를 거부함"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard가 종료를 거부함" || blocking != nil {
			t.Fatalf("inner의 강제 중지 결정이 변경되었습니다: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner에 맡길 때 예산을 소모해서는 안 됩니다, 결과 %d", n)
		}
	})

	t.Run("inner가 계속 실행을 요구하면 추가하지 않음", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard의 계속 실행 사유"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard의 계속 실행 사유" {
			t.Fatalf("inner의 계속 실행 메시지가 변경되었습니다: %v", blocking)
		}
	})

	t.Run("카운터가 없으면 동작 유지", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 다른 호출부가 nudges 전달을 잊은 경우 등
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("카운터가 없으면 주입해서는 안 됩니다: %v", blocking)
		}
	})
}
