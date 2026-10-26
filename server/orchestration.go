package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// P2 작업 간 오케스트레이션 도구입니다. 임의 작업의 Store를 가진 Manager,
// 일시 정지용 Engine, 작업 생성 흐름에 접근해야 하는 host 도구이므로 server 계층에 둡니다.
// 조회 도구는 임시 ToolSet을 만들어 기존 작업별 도구의 Call을 대상 작업의 store에 전달함으로써
// 같은 로직을 재사용합니다. spawn/pause 제어 도구는 Manager/Engine을 직접 호출합니다.
// 트래픽 도구처럼 tools 테이블에 초기화하고 agent에 연결하며 연결된 오케스트레이션 agent에게만 보입니다.

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // Auto용 플랫폼 도구(skill/도구/MCP 생성 및 수정)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 로드 실패: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id는 필수입니다"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task가 존재하지 않습니다: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 전용 콜백 없는 쓰기의 공통 깨우기이며 읽기 도구에서는 아무 작업도 하지 않습니다.
	tsx.SetNotifyHint(t.NotifyHint) // add_hint는 사용자가 추가한 전략 힌트 수와 내용을 이벤트로 기록하고 planner를 깨웁니다.
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"모든 작업(id/설명/목표/상태/실행 시간/상위 작업/LLM 설정)을 나열합니다. 오케스트레이션 agent가 전체 현황, 오래 지연된 작업, 각 작업의 LLM을 파악할 때 사용합니다. 실행 시간(초): 실행 중이면 생성부터 현재까지, 최종 상태이면 생성부터 마지막 활동까지입니다. llm_profile은 작업의 planner/worker가 사용하는 설정 이름이며 (활성 설정)은 전역 활성 설정을 따릅니다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(활성 설정)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(삭제됨)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"사용 가능한 LLM 설정(profile)의 id, 이름, 모델, 형식, 현재 활성 설정 여부를 나열합니다. spawn_task의 llm_profile_id에 id를 전달해 하위 작업 전용 LLM을 지정하세요(예: 정찰에는 저렴한 모델, 악용에는 강력한 모델). API Key는 포함하지 않습니다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"하위 작업을 새로 만들고 탐색 엔진을 시작한 뒤 task_id를 반환합니다. 한 가지 일(예: 문제 하나/목표 하나)을 독립 작업으로 할당할 때 사용합니다. parent_ref는 선택 사항이며 현재 오케스트레이션과 연결된 상위 작업 id를 입력하면 상하위 관계를 설정합니다.",
		objSchema(map[string]any{
			"description":            strParam("작업 설명(짧은 제목)"),
			"goal":                   strParam("작업 목표(달성할 내용)"),
			"parent_ref":             strParam("선택: 상위 작업 id(상하위 관계 연결)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("선택: 읽기 전용으로 상속할 원본 작업 id 목록(최대 %d개). 하위 작업은 이 작업들에서 확인한 자산/결론을 읽기 전용으로 참조하여 시작할 수 있습니다. parent_ref의 단순 상하위 참조와 달리 내용을 상속합니다.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "선택: 이 하위 작업의 planner/worker가 사용할 LLM 설정 id(list_llm_profiles 참고). 비워 두면 상위 작업을 상속하고 없으면 전역 활성 설정을 사용합니다"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "선택: 작업 시간 제한(초). 시간이 되면 정상 마무리를 시작하고 timeout 최종 상태로 전환합니다. 생략하거나 0이면 제한 없음"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "선택: planner 하트비트 간격(초). 이전 계획 종료/작업 시작 후 이 시간이 지나도록 트리거가 없으면 계획을 한 번 실행합니다(교착 상태 방지 및 실행 중인 worker 감독). 생략하거나 0이면 기본값 600(10분)"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "선택: 간단한 작업에서 켤 수 있습니다. 생성 시 초기 의도 하나(설명+목표)를 전달하여 worker가 첫 planner를 기다리지 않고 테스트를 시작합니다. 기본값 false(계획 후 실행하는 표준 흐름)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "이름 없는 작업"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal은 필수입니다"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 읽기 전용 상속 원본 작업의 수 상한과 각 id의 유효성/중복/존재를 HTTP 작업 생성과 동일하게 검사합니다.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("연결 작업은 최대 %d개까지 선택할 수 있습니다", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("연결 작업 id가 올바르지 않거나 중복되었습니다"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("연결 작업 #%d이(가) 존재하지 않습니다", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 설정 #%d이(가) 존재하지 않거나 API Key가 설정되지 않았습니다", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// HTTP createTask와 launchTask의 생성 후 처리를 공유합니다.
			// 시드 생성 + 화면에 보이는 백그라운드 목표 분해(0차/LLM 단계/각 goal) + engine.Run.
			// seed_first_intent는 기본 false이며 계획 후 실행합니다. 간단한 작업은 켜서 work 하나를 바로 테스트할 수 있습니다.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "지정한 작업을 일시 정지합니다(planner/worker 루프 중지).",
		objSchema(map[string]any{"task_id": strParam("일시 정지할 작업 id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task가 존재하지 않습니다: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "지정한 작업의 탐색 그래프 개요를 읽습니다(graph_overview와 동일: 자산 수/frontier/발견/커버리지 등). task_id로 작업을 지정합니다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "지정한 작업의 확인된 취약점을 읽습니다(flag/PoC 포함, 각 항목에 id/task_id/intent_id/vulnclass/severity/요약/상태 포함). task_id로 작업을 지정합니다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "지정한 작업에 전략 힌트를 전달합니다(해당 작업의 planner가 다음 의도 생성 시 읽습니다).\n"+
		"★일괄 처리를 우선하세요. 여러 힌트는 hints 배열로 한 번에 제출합니다(hints와 길이·순서가 같은 ids 배열 반환, 실패 항목 id=0). 하나일 때는 hints를 생략하고 최상위 text에 입력합니다.",
		objSchema(map[string]any{
			"task_id":      strParam("작업 id"),
			"hints":        map[string]any{"type": "array", "description": "【우선 사용】힌트 배열. 각 요소의 필드는 최상위와 같습니다(text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[단일 항목] 힌트 내용"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "기준 자산 id(선택, 0/1/여러 개; 해당 작업의 자산 id)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"지정한 작업의 work(의도) 실행 과정을 확인합니다. get_task_worker_trace(task_id, intent_id)로 단계 요약을 읽고 step_ids=[...]를 추가하여 해당 단계의 전체 내용을 조회합니다(한 번에 최대 5개, 초과 시 처음 5개만 반환).",
		objSchema(map[string]any{
			"task_id":   strParam("작업 id"),
			"intent_id": map[string]any{"type": "integer", "description": "의도 id(해당 작업의 work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "선택: 전체 내용을 조회할 단계 id(한 번에 최대 5개, 초과 시 처음 5개만 반환하며 나머지는 omitted_step_ids에 표시)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "지정한 작업에서 실행한 work(의도)와 각각의 단계 수를 나열합니다. 자세히 볼 work를 고른 뒤 get_task_worker_trace로 조회하세요.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "지정한 작업에서 모든 work의 실행 과정을 키워드로 검색합니다(일치한 단계 요약 + intent_id 반환).",
		objSchema(map[string]any{"task_id": strParam("작업 id"), "q": strParam("검색 키워드")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"지정한 작업에서 탐색 그래프 노드 하나의 전체 내용을 읽습니다(발견/사실/의도/목표: 요약 + 상세/증거/PoC). id는 탐색 노드 id입니다(예: report_finding 반환값 또는 list_task_findings의 id). 취약점 보고서를 작성하기 전에 전체 증거를 조회하세요.",
		objSchema(map[string]any{
			"task_id": strParam("작업 id"),
			"id":      map[string]any{"type": "integer", "description": "탐색 그래프 노드 id(자산 id가 아님)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"등록된 취약점의 【상세 보고서】를 작성/업데이트합니다(Markdown 전체 내용으로 기존 내용을 덮어씀). finding_id에는 report_finding이 반환한 id(\"finding recorded: <id>\"의 숫자)를 전달하세요. 보고서에는 취약점 개요, 영향과 위험, 재현 단계, 증거/PoC, 수정 권고를 포함하는 것이 좋습니다.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "대상 취약점 id(report_finding이 반환한 id)"},
			"report":           strParam("상세 보고서 전체 내용, Markdown 형식"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic이 반환한 증거 version. 보고서가 새로운 증거 변경을 덮어쓰지 않도록 사용합니다"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 숫자 또는 숫자 문자열 해석을 재사용합니다.
			if nodeID <= 0 {
				return actool.Errorf("finding_id가 올바르지 않습니다"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d에 해당하는 취약점 기록이 없습니다(먼저 report_finding으로 등록하세요)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// 작업 운영/플랫폼 도구는 기본적으로 내장 Auto agent에 연결합니다. Auto는 플랫폼 운영용입니다.
	// SeedTool은 최초 삽입만 적용하므로 이전 행은 seedAutoDefaultBindings로 연결을 보충합니다.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // worker의 list_facts/list_companies/list_worker_traces 기본 연결을 한 번만 해제합니다.
	s.seedWorkerReadbackRebind()  // 이전 마이그레이션의 잘못된 제거를 보완해 search_all_worker_traces/get_worker_trace/node_detail을 worker에 한 번 다시 연결합니다.
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals의 작업 제약 추출 단계 추가에 맞춰 이전 DB에 새 기본 프롬프트 버전을 한 번 추가합니다.
	s.reseedMainAgentPrompt()         // mainagent에 목표 달성 후 add_intent로 직접 주입할 때 정식 목표 생성 여부를 묻는 안내를 한 번 추가합니다.
	s.reseedPlannerPrompt()           // planner의 의도 0개가 정당한 경우를 재작성하고 정량적 수용 기준 확인을 한 번 추가합니다.
	s.reseedWorkerPrompt()            // worker에 부정 결론의 증거 기준을 한 번 추가합니다.
	s.seedReporterAgent()             // 보고서 작성 agent, 도구 연결, finding 트리거를 한 번 초기화합니다.
	s.upgradeReporterTriggerMessage() // 이전 DB의 reporter가 evidence_version을 반환하도록 한 번 이전합니다.
	s.seedFindingTrafficTools()       // 사용자 설정을 보존하면서 선택적 증거 인자와 읽기 전용 증거 도구를 추가합니다.
	s.seedFindingWorkflowTools()
	// pentest 기본 연결은 별도 이전이 필요 없습니다. 새 초기화의 BuiltinToolSeeds가
	// list_assets/insert_assets/report_finding/list_findings/list_companies와
	// pentest를 함께 초기화합니다(해당 기능의 이전 DB가 없어 마이그레이션하지 않음).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool은 최초 삽입만 적용하므로 spawn_task의 llm_profile 같은 새 인자는
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 일부 내장 agent 도구도 코드 기본값으로 갱신합니다.
	// - goal_met: 이전 설명의 현재 계획 종료 문구가 planner에게 빈 턴 종료 수단으로 오해되어
	// 실행 직후 작업 전체를 완료로 잘못 판단하게 할 수 있었습니다.
	// - insert_assets: 작업 관련 여부와 커버리지 포함 여부를 나타내는 related 인자를 추가했으므로
	// 최초 삽입만 하는 SeedTool로는 이전 schema에 새 인자가 반영되지 않습니다.
	// - list_facts: 페이지 처리와 limit/before/q 인자를 추가했습니다. 이전 빈 schema를 유지하면
	// 도구 관리에 인자 없음으로 표시되고 모델도 인자 설명을 받을 수 없습니다.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Print(logT("[tools] orchestration/platform 도구 스키마를 코드 기본값으로 새로고침(1회성)"))
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met의 planner 연결 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt는 goals 프롬프트를 현재 코드 기본값으로 바꿉니다. 기본 내용에
// set_constraints로 작업 제약을 먼저 추출한 뒤 목표를 분해하는 단계가 추가되었지만 최초 삽입만 하는 SeedPromptIfEmpty로는
// 이전 version 1에 반영되지 않습니다. 새 버전을 추가하고 ResetPromptToDefault로 전환합니다.
// 이전 사용자 정의 버전은 이력에 남아 복구할 수 있습니다. settings 플래그로 한 번만 실행하며
// 기본값이 다시 바뀌면 플래그를 올립니다. 새 DB는 이미 최신값을 초기화하므로 처리하지 않습니다.
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 관계없이 한 번만 시도합니다.
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 새 DB에 agent 행이 없으면 seedPrompts가 최신 기본값을 넣으므로 이전이 필요 없습니다.
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 새 DB의 현재 버전이 이미 코드 기본값이면 중복 버전을 추가하지 않습니다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals 프롬프트를 새 기본값으로 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] goals 새 기본 프롬프트 버전 추가 완료(작업 제약 추출 단계 추가, 한 번만 적용)")
}

// reseedMainAgentPrompt는 mainagent를 현재 코드 기본값으로 바꿉니다. 목표를 모두 달성한 뒤
// add_intent로 직접 의도를 추가할 때 정식 목표 등록 여부를 묻는 안내가 추가되었지만 최초 삽입만으로는
// 기존 버전에 반영되지 않습니다. 새 버전을 추가해 ResetPromptToDefault로 전환하고 이전 버전은
// 이력에 보존하여 사용자 정의 내용을 복구할 수 있습니다. settings 플래그로 한 번만 실행하며 새 DB는
// 이미 최신값을 초기화하므로 제외합니다. reseedGoalsPrompt와 같은 구조입니다.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 관계없이 한 번만 시도합니다.
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 새 DB에 agent 행이 없으면 seedPrompts가 최신 기본값을 넣으므로 이전이 필요 없습니다.
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 새 DB의 현재 버전이 이미 코드 기본값이면 중복 버전을 추가하지 않습니다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent 프롬프트를 새 기본값으로 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] mainagent 새 기본 프롬프트 버전 추가 완료(목표 달성 후 새 목표 생성 질문 추가, 한 번만 적용)")
}

// reseedPlannerPrompt는 간소화한 planner 기본 프롬프트를 적용합니다. 절제 규칙을 중복 방지로 제한하고
// 커버리지보다 깊이 우선, 미달성 목표와 실행 의도 부재 시 의도 생성 의무, 부정 결론 재검토 상한을 추가합니다.
// 의미 있는 기본값 변경마다 아래 플래그(현재 v2)를 올려 이전 DB에 한 번 더 반영합니다. 최초 삽입만으로는
// 기존 버전이 갱신되지 않으므로 새 버전을 추가하고 전환합니다. 이전 사용자 정의 버전은 이력에서
// 복구할 수 있습니다. settings 플래그로 한 번만 실행하며 새 DB는 이미 최신값을 초기화하므로 제외합니다.
// reseedGoalsPrompt와 같은 구조입니다.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 관계없이 한 번만 시도합니다.
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 새 DB에 agent 행이 없으면 seedPrompts가 최신 기본값을 넣으므로 이전이 필요 없습니다.
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 새 DB의 현재 버전이 이미 코드 기본값이면 중복 버전을 추가하지 않습니다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner 프롬프트를 새 기본값으로 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] planner 새 기본 프롬프트 버전 추가 완료(간소화, 신중한 강등 및 중복 제거, 깊이 우선, 부정 재검토 상한, 한 번만 적용)")
}

// reseedWorkerPrompt는 worker 기본 프롬프트를 갱신합니다. record_fact에서 부정 결론의 관찰/잠정 해석 안내를
// 제거하고 confidence(observed/inferred)와 의도의 수단을 모두 시도했는지 여부를 분리해 planner 오해를 줄입니다.
// facts 배열 분할도 완전히 독립적이고 합칠 수 없는 소수 예외로 제한합니다. v3 플래그로 이전 DB에 한 번 반영합니다.
// 최초 삽입만으로는 기존 버전에 반영되지 않으므로 새 버전을 추가해 전환하며 이전 버전은 이력에 보존합니다.
// settings 플래그로 한 번만 적용하고 새 DB는 제외합니다. reseedGoalsPrompt와 같은 구조입니다.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 관계없이 한 번만 시도합니다.
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 새 DB에 agent 행이 없으면 seedPrompts가 최신 기본값을 넣으므로 이전이 필요 없습니다.
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 새 DB의 현재 버전이 이미 코드 기본값이면 중복 버전을 추가하지 않습니다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker 프롬프트를 새 기본값으로 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] worker 새 기본 프롬프트 버전 추가 완료(컨텍스트 조회를 list_assets/list_findings로 통합, list_facts/node_detail/asset_neighbors 제거, 한 번만 적용)")
}

// reporterToolCallMessage는 보고서 작성 전에 get_finding_traffic 조회를 항상 요구해야 합니다.
// 읽기 전용이며 캡처 스위치와 무관하게 수동 연결 증거도 읽습니다. 자동 연결 활성 시에만
// 조회하도록 하면 기본 비활성 상태에서 reporter가 evidence_version을 전달하지 않아
// SetFindingReportVersionByNodeID가 기존 의미에 따라 -1을 저장하고 취약점 상세/Markdown 내보내기가
// 계속 증거 변경 및 보고서 업데이트 필요로 표시되며 UI에서 해제할 방법이 없어집니다.
const reporterToolCallMessage = "방금 위에서 report_finding으로 취약점 하나를 등록했습니다. 반환 JSON의 finding_id(독립 취약점 기록 ID)와 finding_node_id(탐색 노드 ID)를 읽으세요. " +
	"먼저 get_finding_traffic(finding_id)로 현재 증거 목록과 version을 읽으세요(빈 목록도 정상이므로 보고서를 그대로 작성하세요). " +
	"실행 지침에서 자동 연결을 켰다면 조회 전에 이번 취약점의 트래픽을 확인하고 연결하세요. 노드 상세에는 finding_node_id를 사용하세요." +
	"마지막으로 update_finding_report(finding_id=finding_node_id, report, evidence_version=실제로 읽은 버전)을 호출하여 저장하세요. " +
	"evidence_version은 필수이며 생략하면 보고서가 계속 업데이트 필요로 표시됩니다. 두 종류의 번호를 혼용하지 마세요."

// 0.3.8 이하의 이전 트리거 원문입니다. 이 값과 정확히 같은 기록만 이전하며 사용자 수정본은 보존합니다.
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage는 이전 DB에 기본 원문으로 남은 reporter 트리거만 새 버전으로 바꿉니다.
// seedReporterAgent는 reporter_agent_seed_v1로 보호되고 새 agent 생성 때만 트리거를 쓰므로
// 업그레이드 DB에는 새 안내가 들어가지 않습니다. seedFindingTrafficTools가 schema에
// evidence_version을 추가해도 사용을 알려주지 못하므로 수정되지 않은 안내만 한 번 갱신합니다.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 한 번만 시도합니다.
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] 트리거 조회 실패: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // 사용자 수정본이나 finding 트리거가 아닌 항목은 유지합니다.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 메시지 업그레이드 실패: %v", err)
			return
		}
		log.Printf("[reporter] 트리거 메시지가 evidence_version 조회 및 반환 방식으로 업그레이드됨")
	}
}

// reporter 표시 이름과 설명은 한국어 UI 레이블이며 key는 reporter로 유지합니다.
// 모델 프롬프트와 새 트리거 안내도 한국어입니다. 기존 마이그레이션 비교용 V1 원문은 별도로 보존합니다.
const reporterAgentName = "보고서 작성"
const reporterAgentDescription = "취약점 상세 보고서 작성: 취약점을 발견하면 자동으로 트리거되어, 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 작성해 해당 취약점에 기록합니다."

// seedReporterAgent는 UI에서 수정/삭제 가능한 보고서 작성 사용자 정의 agent(builtin=false)를 초기화합니다.
// update_finding_report와 작업 조회 도구를 연결하고 report_finding 호출 트리거를 붙여
// 취약점마다 상세 보고서를 작성합니다. settings 플래그로 한 번만 실행해 사용자가 삭제한 뒤에는 재생성하지 않습니다.
// 오케스트레이션 도구는 앞에서 SeedTool로 저장되어 있어 연결할 수 있습니다.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 관계없이 한 번만 시도합니다.

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // 사용자가 이미 같은 key로 생성했으면 덮어쓰지 않습니다.
	}
	a, err := s.m.pg.CreateAgent("reporter", reporterAgentName, reporterAgentDescription)
	if err != nil {
		log.Printf("[reporter] agent 생성 실패: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt 실패: %v", err)
	}
	// parallel + none으로 취약점마다 보고서를 별도 작성하고 여러 finding을 병렬 처리합니다.
	// merge는 none이어야 합니다. 기본 all이면 여러 finding이 한 실행으로 병합되어 병렬 처리가 무의미합니다.
	// maxParallel=5로 보고서 대화를 최대 5개 동시 실행하여 LLM 호출 급증을 막습니다.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 트리거 실행 정책 설정 실패: %v", err)
	}
	// 보고서 작성과 증거/실행 과정/현황 조회에 필요한 도구를 연결합니다.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] 도구 연결 실패: %v", err)
	}
	// report_finding 호출 시 트리거합니다. 도구 반환 "finding recorded: <id>"의 finding_id와
	// 작업 id가 트리거 메시지에 포함됩니다.
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] 트리거 생성 실패: %v", err)
	}
	log.Printf(logT("[reporter] 「%s」 agent + finding 트리거 사전 구성"), reporterAgentName)
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // 기본 연결을 worker에서 planner로 전환합니다.
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope 기본 연결 실패: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope 연결 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s worker 연결 해제 실패: %v", k, err)
			return // 오류 시 플래그를 저장하지 않아 다음 시작 때 재시도합니다.
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 이력/상세 도구 추가 연결 실패: %v", err)
		return // 오류 시 플래그를 저장하지 않아 다음 시작 때 재시도합니다.
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 이전 자산 도구 이름을 교체하고 insert_assets/add_company_scope를 추가합니다.
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Auto는 플랫폼 운영 중 자산 조회/등록과 기업 범위 관리가 자주 필요합니다.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
