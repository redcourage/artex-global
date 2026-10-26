package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**취약점 번호 규약**: finding_id는 독립 취약점 기록 ID이며 finding_node_id는 탐색 노드 ID다. list_findings / list_task_findings / node_detail / get_task_node_detail의 id는 탐색 노드 ID로 유지된다. 독립 번호는 같은 반환값의 finding_id에서 읽는다. get_finding_traffic / bind_finding_traffic은 독립 finding_id를 사용한다. 기존 update_finding_report의 finding_id 매개변수에는 여전히 finding_node_id를 전달한다. report_finding 첫 줄의 숫자를 증거 도구에 사용하거나 번호 오류 후 다른 숫자를 추측하지 않는다."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n기본적으로 보고서 Agent가 보고서 작성 전에 트래픽을 확인하고 연결한다. 보고자는 evidence에 검증 명령, 주요 출력, 기존 실제 트래픽 ID와 용도를 보존하여 보고서 Agent가 실행 기록과 대조하도록 한다. 연결을 위해 추가로 패킷을 조회할 필요 없다. 명시적 즉시 연결도 호환된다. traffic_refs 또는 evidence_hint_id로 확인된 참조를 제출할 수 있으며 후자는 이 작업의 지정 hint에서 구조화 참조를 읽는다. 하나라도 유효하지 않으면 이번 보고 전체가 실패한다. TCP/패킷 없음에는 이 선택 매개변수가 필요 없다. 반환값 finding_id와 finding_node_id는 각각 독립 기록과 탐색 노드를 뜻한다."
		case "add_hint", "add_task_hint":
			note = "\n확인된 취약점을 인계할 때 해당 힌트의 traffic_refs에 검증된 트래픽의 ID, 용도, 설명, 순서를 보존한다(단일 항목은 최상위, 일괄은 대응 hints 요소에 기록). text에는 어떤 구체적 취약점을 입증하는지 설명한다. 호출자는 텍스트만 인계하며 기존 트래픽 참조를 버려서는 안 된다. 확인하지 않은 후보는 증거로 전달할 수 없다."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**트래픽 증거 인계(선택)**: 자동 연결은 기본적으로 취약점 저장 후 보고서 작성 전에 보고서 Agent가 수행한다. 보고자는 evidence에 검증 명령, 주요 출력, 기존 실제 트래픽 ID와 용도를 보존하고 작업에서는 intent_id를 포함하여 추적할 수 있게 한다. 연결을 위해 추가로 패킷을 조회할 필요 없다. Auto / Planner가 대신 보고할 때 실행자의 기존 참조를 버리지 않는다. add_hint / add_task_hint는 traffic_refs로 인계할 수 있다. report_finding의 traffic_refs / evidence_hint_id를 통한 명시적 즉시 연결도 계속 지원한다. TCP 또는 패킷 없음이면 정상 등록하며 ID를 추측하거나 패킷 보충만을 위해 반복 탐지하지 않는다."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\n플랫폼 대화에 작업 문맥이 없으면 report_finding을 직접 호출하지 않는다. add_task_hint로 기존의 해당 작업에 인계하여 작업 Agent가 등록하게 하고 list_task_findings로 결과를 확인한다."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n목표 완료 판단 전에 이번에 확보한 증거의 보고/인계를 먼저 완료한다. 증거 인계가 끝나지 않았는데 텍스트 취약점이 등록되었다는 이유만으로 작업을 종료하거나 Worker를 취소하지 않는다. 패킷이 없으면 기다리거나 억지로 캡처할 필요 없다."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**보고서 작성 전 자동 트래픽 연결(활성화됨)**: 이번에 유발된 취약점의 트래픽을 확인하고 연결한 뒤 보고서를 작성한다. 먼저 report_finding 반환 JSON 또는 get_task_node_detail / list_task_findings에서 명확한 finding_id와 finding_node_id를 얻는다. 취약점 상세, 해당 의도 실행 기록, 기존 증거 목록을 읽고 보고자가 인계한 실제 ID를 우선 사용한다. 이번 검증이 HTTP이고 트래픽 도구를 사용할 수 있으면 traffic_search로 후보를 선별한 뒤 traffic_get으로 각 요청/응답이 취약점을 실제로 뒷받침하는지 확인한다. 도메인과 시간은 선별용일 뿐 귀속을 입증하지 않는다. 확인한 증거를 재현 순서대로 bind_finding_traffic(finding_id, traffic_refs)로 연결하고 baseline / proof / verification / supporting을 선택하여 용도를 설명한다. 이번 취약점만 다루며 중복 생성하거나 대상을 다시 탐지하지 않는다. 연결에 성공하면 get_finding_traffic을 다시 호출해 최신 version을 얻고 필요한 본문을 읽는다. 실제로 읽은 version을 evidence_version으로 update_finding_report에 전달한다(그 finding_id 매개변수는 여전히 finding_node_id 사용). 기존 연결을 중복 추가할 필요 없다. TCP, 미수집, 도구 사용 불가 또는 정확한 일치가 없으면 자동 연결을 건너뛰고 텍스트/명령 증거로 정상 작성하며 이유를 설명한다. 트래픽을 채우려고 추측해서는 안 된다. 연결 실패를 성공으로 주장하지 않는다. 기존 증거를 보존하고 보고서에 미연결 이유를 설명한다."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "선택: 확인되었고 이 힌트의 구체적 취약점에 해당하는 트래픽 참조. 순서를 보존한다. 인계 후 report_finding에 evidence_hint_id를 전달해 이 참조를 포함할 수 있다.", "items": obj(map[string]any{"traffic_id": str("실제 트래픽 ID"), "role": str("baseline / proof / verification / supporting"), "note": str("이 트래픽이 뒷받침하는 결론")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d는 이 작업의 힌트 노드여야 한다(상속 힌트는 직접 연결에 사용할 수 없음)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
