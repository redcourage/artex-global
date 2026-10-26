package agent

import (
	"context"
	"encoding/json"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// 내장 도구를 순수 코드에서 열거 가능하고 DB로 덮어쓸 수 있는 카탈로그로 만든다.
// - BuiltinToolSeeds(): 세 실행 agent의 내장 도구 모음을 seed 기록(key +
//   설명 + 매개변수 schema + 기본 연결 agent)으로 펼쳐 서버 시작 시 tools 테이블에 멱등하게 초기 등록한다.
// - ToolResolve 훅: 런타임에 DB tools 행에 따라 조립된 도구를 agent별로 필터하고
//   설명/schema를 덮어쓰며 매개변수 기본값을 주입한다. key/handler는 코드에 남고 DB는 설명과 기본값만 바꾼다.
// handler(Call 동작)는 항상 코드에서 오며 DB로 바꿀 수 없다. 모델이 보는 설명과 기본 입력만 변경 가능하다.

// ToolSeed는 내장 도구의 초기 등록용 스냅샷이다. key는 CoreTool.Name()이며 handler와 고정 연결되어
// UI에서 읽기 전용이다. Desc/Schema는 코드 도구 정의에서, Agents는 기본 연결 agent 목록에서 가져온다.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), 변경 불가 기본 키
	Desc   string         // 최상위 설명(UI에서 덮어쓰기 가능)
	Schema map[string]any // 매개변수 JSON-Schema(구조는 읽기 전용, description/default는 UI 편집 가능)
	Agents []string       // 기본 연결 agent key(worker/planner/mainagent)
}

// builtinToolsByAgent는 읽기 전용 빈 ToolSet(nil stores)으로 각 실행 agent의
// 도메인 도구 모음을 만든다. 생성자는 Spec에 클로저만 넣고 store를 역참조하지 않아 nil에 안전하다.
// 여기서는 Name()/Description()/InputSchema()만 읽으며 절대 Call하지 않는다.
//
// SDK 공통 도구 actool.DefaultTools()(Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash)는 의도적으로 제외한다. 모든 agent가 고정 보유하므로 연결 선택이 없고 설명 대부분이 Prompt()에
// 있어 Description()만 덮어쓰는 이 테이블로는 부분 적용되어 오해를 낳는다. seed 없음 → DB 행 없음 → ToolResolve가
// 그대로 통과시키고 덮어쓰지 않아 이전과 동작이 같다. artex 자체 도메인 도구만 테이블에서 관리한다.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals(목표 분해기)는 set_goals + set_constraints를 기본 연결하여 분해한 목표와
		// 추출한 행동 제약을 저장한다. mainagent와 같은 관리 도구를 공유하며 웹에서 설명/schema 수정과 agent별 선택 가능하다.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto는 취약점 보고 + 자산 관리 도구를 기본 연결한다. 다른 도메인 도구는 UI에서 필요에 따라 선택한다.
		// 새 DB는 이 seed로 기록하며 기존 DB는 seedAutoDefaultBindings로 마이그레이션한다.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest(독립 침투 agent)는 자산 조회/삽입, 취약점 보고/조회, 회사 조회를 기본 연결한다.
		// 새 DB는 이 seed로 기록하며 기존 DB는 seedPentestDefaultBindings로 마이그레이션한다.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: 이 system 도구는 카탈로그에 정상 등록하여 웹에서 보고 agent별 수동 선택할 수 있으나
// 기본적으로 어떤 agent에도 연결하지 않는다. ToolResolve는 빈 연결 도구를 모든 agent에서 제거하므로 명시적 선택이 필요하다.
// 일부 agent의 base 도구 모음에 남기는 이유(예: PlannerTools의 goal_met)는 seed가 구성하여
// desc/schema를 얻고, 사용자가 다시 연결하면 런타임 base에 존재하여 ToolResolve가 유지할 수 있게 하기 위해서다.
//
// goal_met는 개별 prove_goal을 우회하여 전체 작업 완료를 선언한다. 권한이 크고 오판 위험이 있으며
// 마지막 목표 prove_goal → 자동 완료와 중복이므로 기본적으로 연결하지 않고 필요 시 수동 연결한다.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds는 각 agent의 내장 도구 모음을 중복 제거하여 seed 목록으로 합친다. list_assets처럼
// 여러 agent에 같은 이름 도구가 있으면 하나로 합치고 Agents는 합집합, defaultUnbound 도구는 강제로 빈 연결로 둔다.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 카탈로그에 등록하여 수동 연결 가능하지만 기본 연결은 없음(다른 도구처럼 null 대신 [] 저장)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 기본 입력 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — 기본값만 병합한다.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}

// aliasTool is a CoreTool decorator that overrides only Name(): it lets a
// CamelCase built-in (e.g. Bash) also answer to the lowercase/snake_case spelling
// a model drifts into (bash, web_fetch). Handler, description, schema, prompt and
// permission all delegate to the wrapped tool, so an alias is behaviourally
// identical — it only repairs exact-match dispatch lookup (norma's execOne does
// reg.Get(use.Name), no case folding). See issue #19.
type aliasTool struct {
	actool.CoreTool
	name string
}

func (a *aliasTool) Name() string { return a.name }

// toolAliasNames returns the lowercase / snake_case / UPPER spellings a CamelCase
// tool name is commonly confused with, dropping the original and duplicates. A
// name already in lower/snake form yields nothing (no drift to defend against).
func toolAliasNames(name string) []string {
	if name == strings.ToLower(name) {
		return nil
	}
	candidates := []string{strings.ToLower(name), camelToSnake(name), strings.ToUpper(name)}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{name: true}
	for _, v := range candidates {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// camelToSnake converts CamelCase to snake_case (WebFetch → web_fetch,
// TodoWrite → todo_write) while leaving an all-caps run intact (LS → ls).
func camelToSnake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := runes[i-1]
				var next rune
				if i+1 < len(runes) {
					next = runes[i+1]
				}
				if (prev >= 'a' && prev <= 'z') || (next >= 'a' && next <= 'z') {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// addToolAliases registers drift-tolerant aliases for every CamelCase tool. Each
// alias is added to the returned tool list (so exact-match dispatch finds it) and
// its name is appended to deferred (so the schema is withheld and the model keeps
// seeing only the official CamelCase names — no duplicate exposure, no token
// waste). Names already taken by a real tool are skipped, so nothing is shadowed.
func addToolAliases(tools []actool.CoreTool, deferred []string) ([]actool.CoreTool, []string) {
	taken := map[string]bool{}
	for _, t := range tools {
		taken[t.Name()] = true
	}
	for _, n := range deferred {
		taken[n] = true
	}
	out := tools
	extra := deferred
	for _, t := range tools {
		for _, alias := range toolAliasNames(t.Name()) {
			if taken[alias] {
				continue
			}
			taken[alias] = true
			out = append(out, &aliasTool{CoreTool: t, name: alias})
			extra = append(extra, alias)
		}
	}
	return out, extra
}
