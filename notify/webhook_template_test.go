package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 일반 Webhook 템플릿의 기능 경계를 고정하는 테스트.
//
// 패키지에서 사용자 문자열을 코드처럼 평가하는 유일한 곳이므로 허용 범위를 명확히
// 테스트로 고정해야 한다. 나중에 문맥 메서드나 FuncMap의 readFile 하나를 추가하면
// 겉으로는 작은 함수 변경처럼 보여도 기능 범위가
// 조용히 확대될 수 있다.

// TestTemplateContextHasNoMethods가 가장 중요한 검증이다.
//
// text/template은 공개 메서드를 호출한다({{.Foo}}는 필드 읽기와 메서드 호출 모두 가능).
// 따라서 공개 메서드가 있는 어떤 타입에든 접근할 수 있으면 그 메서드도 템플릿 작성자에게 노출된다.
// 여기 문맥은 의도적으로 공개 필드만 있고 메서드 없는 순수 데이터다.
//
// 실패하면 webhookTemplateData/webhookItem에 메서드가 추가되었다는 뜻이다.
// 허용 전 그 메서드로 공개하면 안 되는 데이터를 읽을 수 있는지 확인해야 한다.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s에 %d개 메서드(%s)가 노출됨: text/template에서 호출 가능하므로 "+
				"템플릿 작성자에게 해당 메서드 기능을 공개하게 됨", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal은 공개 함수 집합을 고정한다.
//
// FuncMap 함수 추가는 기능 추가다. 현재 json/jsons는 JSON 직렬화만 하며
// 파일 읽기/요청 전송/명령 실행은 불가능하다.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("템플릿 함수 집합 변경: 실제 %v, 예상 %v. 추가 함수가 권한을 확장하지 않는지 확인해야 함"+
			"(파일 읽기/쓰기, 네트워크 요청, 명령 실행 불가)", got, want)
	}
}

// TestTemplateCannotReachUnknownData는 없는 데이터 접근을 검증한다.
// 존재하지 않는 항목 접근은 실패하고 오류에 내부 데이터를 포함하면 안 된다.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("없는 필드 접근은 오류여야 함")
	}
	// 오류에 실제 문맥의 취약점 제목/요약이 나타나면 안 된다.
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("템플릿 오류가 메시지 내용 %q를 유출함: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: 잘못된 템플릿은 재시도로 회복되지 않는 설정 오류다.
// 재시도 가능으로 보면 매 발송마다 불필요한 백오프 세 번이 발생한다.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("템플릿 구문 오류는 저장 시 차단해야 함")
	}
	// 검증을 우회해 직접 발송하더라도 영구 실패로 처리해야 한다.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("잘못된 템플릿은 영구 실패여야 함, 실제 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON은 렌더링 결과가 유효한 JSON이어야 함을 검증한다.
// 템플릿으로 일반 텍스트를 만들어 다른 프로토콜을 호출하는 사용도 막는다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 유효한 템플릿은 통과한다.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("유효한 템플릿은 검증을 통과해야 함: %v", err)
	}
	// JSON 아닌 결과는 그대로 보내지 않고 거부한다.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON 아닌 결과는 영구 실패여야 함, 실제 %v", err)
	}
	if !strings.Contains(err.Error(), "올바른 JSON") {
		t.Errorf("오류에 JSON 문제임을 밝혀야 함, 실제 %v", err)
	}
}
