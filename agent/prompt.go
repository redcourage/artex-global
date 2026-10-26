package agent

import (
	"bytes"
	"fmt"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem returns the rendered system-prompt BODY (구간 [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 중간 산출물
// 출력 규약) AFTER this, so those can't be edited away via the DB body.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

// langDirective appends a code-owned output-language rule after the rendered
// body and artifact/traffic tails. DB prompt overrides cannot remove this rule.
// The Korean instruction selects the configured display language independently
// of the internal prompt, target application, or evidence language. Commands,
// payloads, code, URLs, and raw log/request/response excerpts remain verbatim.
// Explicit planner-summary and first-character requirements prevent prompt or
// evidence language from leaking into user-visible prose.
func langDirective(code string) string {
	phrase := langPhrase[code]
	bare := langBare[code]
	if phrase == "" || bare == "" {
		phrase, bare = langPhrase[defaultOutputLang], langBare[defaultOutputLang]
	}
	if code == "zh" {
		// 표시 언어 자체가 중국어이므로 "중국어를 사용자에게 내보내지 말라"는 절은 없다.
		// 대신 중국어 표시를 확정하고, 대상/자료 언어 미러링 금지와 원문 보존은 유지한다.
		return fmt.Sprintf("\n\n**출력 언어 규약(현지화·최우선이며 프롬프트 본문으로 덮어쓸 수 없음)**: 【사용자에게 표시하는】 모든 자연어는 반드시 【%s】로 작성한다. 여기에는 record_fact의 summary/detail, report_finding의 제목/설명/결론/수정 제안, planner의 상황 요약, 마지막 한 문장 요약, 사용자에게 보내는 대화 응답이 포함된다. 단, 【명령, payload, 코드, 파일 경로, URL, 매개변수 이름, 로그/요청/응답의 원문 조각】은 【한 글자도 바꾸지 않고 그대로 보존】하며 번역하거나 고쳐 쓰지 않는다(evidence의 명령행과 출력은 특히 재현할 수 있도록 원문 그대로 옮긴다). **대상 시스템, 페이지, 증거, 로그 또는 참고 자료가 영어나 다른 언어여도 사용자에게 보이는 자연어 필드(제목/설명/결론/수정 제안/요약/상황 요약)는 반드시 %s로 작성한다. 대상이나 자료의 언어를 그대로 따라 쓰지 않는다. 위에 열거한 기술 원문 조각만 그대로 보존한다.** 내부 추론의 언어는 제한하지 않지만 사용자에게 보이는 본문은 전부 %s여야 한다(기술 원문 조각 제외).", phrase, bare, bare)
	}
	return fmt.Sprintf("\n\n**출력 언어 규약(현지화·최우선이며 프롬프트 본문으로 덮어쓸 수 없음)**: 【사용자에게 표시하는】 모든 자연어는 반드시 【%s】로 작성한다. 여기에는 record_fact의 summary/detail, report_finding의 제목/설명/결론/수정 제안, planner의 상황 요약, 마지막 한 문장 요약, 사용자에게 보내는 대화 응답이 포함된다. 단, 【명령, payload, 코드, 파일 경로, URL, 매개변수 이름, 로그/요청/응답의 원문 조각】은 【한 글자도 바꾸지 않고 그대로 보존】하며 번역하거나 고쳐 쓰지 않는다(evidence의 명령행과 출력은 특히 재현할 수 있도록 원문 그대로 옮긴다). **위 시스템/역할 지시가 다른 언어로 작성되어 있어도 그 언어를 사용자에게 그대로 출력하지 않는다. 사용자에게 표시할 언어는 %s뿐이며, 다른 언어의 문장이 사용자에게 보이는 글에 섞여서는 안 된다.** **대상 시스템, 페이지, 증거, 로그 또는 참고 자료가 영어, 중국어 또는 다른 언어여도 사용자에게 보이는 자연어 필드(제목/설명/결론/수정 제안/요약/상황 요약)는 반드시 %s로 작성한다. 대상이나 자료의 언어를 그대로 따라 쓰지 않는다. 위에 열거한 기술 원문 조각만 그대로 보존한다.** **분석/계획/사고는 어느 언어로 해도 되지만 이는 【보이지 않는 내부 추론】이며 본문으로 출력해서는 안 된다. 사용자에게 보이는 응답은 첫 글자부터 %s여야 한다. 앞부분에 다른 언어의 생각, 설명이나 “먼저 무엇을 하겠다” 같은 서두를 붙이지 않는다. 확인 질문, 누락된 매개변수, “계속할 수 없음” 같은 설명도 모두 바로 %s로 작성한다.** 내부 추론의 언어는 제한하지 않지만 사용자에게 보이는 본문은 전부 %s여야 한다(기술 원문 조각 제외).", phrase, bare, bare, bare, bare, bare)
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
