package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**취약점 트래픽 증거(선택)**: report_finding으로 취약점을 보고할 때 직접 읽고 취약점 결론을 뒷받침한다고 확인한 HTTP 요청/응답이 있으면 traffic_refs로 실제 ID를 재현 순서대로 연결할 수 있다. 도메인과 시간은 후보 선별에만 쓰며 연관성을 추정하지 않는다. TCP 등 비HTTP 취약점, 미수집 또는 정확한 일치가 없으면 생략하거나 []를 전달하고 evidence에 명령 출력, 로그 등 검증 가능한 다른 증거를 보존한다. 연결하지 못한 이유를 설명하는 것이 좋다. ID를 추측하거나 패킷 보충만을 위해 반복 탐지하지 않는다."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
