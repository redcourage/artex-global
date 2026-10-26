package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa는 norma v0.4.0의 모델 주도 문맥 압축이며 사용자가 시스템 설정에서 켜고 끄는
// 플랫폼 실험 기능이다. 내장 compaction과 배타적이며 noaadapter.Enable이 유일한 진입점으로
// Compactor, Compress 도구, 세 개의 상주 프롬프트를 한 번에 연결한다. Enable을 호출하지 않으면 꺼져 있고
// 내장 compaction이 그대로 작동한다. 각 agent에 주입한 noaEnabledFn을 run마다 한 번 읽으므로
// 전환은 이후 시작하는 run에만 영향을 주며 agent를 다시 만들 필요 없다.

// enableNoa는 활성화 해석 결과에 따라 opts에 noa를 연결한다. archiveRoot는 압축 원문 영구 저장의 기준 디렉터리다.
// 전역 workDir를 사용하며 모든 agent가 <workDir>/noa에 모이고 작업/의도 디렉터리로 분산하지 않는다. sessionID는
// 하위 아카이브 디렉터리 이름이며 전역 고유하므로 같은 기준 디렉터리에서 충돌하지 않는다.
//
// noa는 실험 기능이므로 연결 실패가 실제 작업을 중단해서는 안 된다. 오류는 onWarn으로 보고하고 내장 압축으로 대체한다.
// 활성화 성공 시 opts.Compaction을 비워 두 문맥 관리자가 동시에 설정되었다는 agentcore 경고를 피한다.
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 압축 활성화 실패, 내장 압축으로 대체:" + err.Error())
		}
		return
	}
	// Compactor가 Compaction을 덮어쓰지만 둘이 공존하면 매번 경고하므로 명시적으로 비운다.
	opts.Compaction = nil
}
