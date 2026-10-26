package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel은 URL/메서드/헤더/JSON 템플릿을 지정하는 일반 Webhook 어댑터다.
// Slack/Mattermost/Discord/자체 시스템마다 별도 구현을 만들지 않고
// 설정 가능한 템플릿 하나로 지원할 수 있다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 일반 Webhook에는 공식 제한이 없어 0을 반환하며 사용자가 상대 처리 능력에 맞춰 설정한다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url과 headers는 각각 token과 인증 정보를 포함할 수 있고 API 응답에 나타나므로
// 둘 다 마스킹한다.
// 헤더 하나를 변경하려면 전체를 다시 입력해야 한다(마스킹 값은 원본 유지 의미).
// 브라우저에 자격 증명을 노출하는 대신 재입력을 요구하는 의도적 선택이다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 목적지는 url이다. 변경 시 headers를 다시 명시해야 기존 Authorization이 새 주소로
// 그대로 발송되어 마스킹을 우회하는 일을 막는다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate은 미설정 시 기본 본문으로 단순 JSON 구조를 사용해
// JSON을 받아 저장하는 대부분의 자체 수신기를 지원한다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData는 사용자 템플릿에 제공하는 문맥이다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt은 수신 기록용 발송 시각(RFC3339).
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel은 대기 → 수정됨 같은 읽기 쉬운 상태 변경 설명. 변경 이벤트가 아니면 빈 값.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL이 없습니다")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("대상 URL이 올바르지 않습니다: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드입니다: %s (GET/POST/PUT/PATCH 중에서 선택하세요)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 문법이 올바르지 않습니다: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET에는 본문을 보내지 않는다. query에 넣는 것은 템플릿 범위와 GET 의미에 맞지 않아
	// GET은 호출 자체로 훅을 실행하는 수신기에만 적합하다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 템플릿의 JSON 문자열을 json.RawMessage로 변환해 그대로 보내고
		// 재이스케이프로 사용자 구조를 JSON 문자열 안에 넣지 않는다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿의 렌더링 결과가 올바른 JSON이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// headers 이후에 적용해 명시적 설정이 우선하도록 한다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 수신기는 사용자 서비스이며 크기는 body_template이 결정하므로 본문을 자르지 않는다.
	// 따라서 전체 묶음을 전달된 것으로 계산한다.
	return len(m.Items), nil
}

// renderWebhookBody는 사용자 또는 기본 템플릿으로 요청 본문을 만든다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 문법이 올바르지 않습니다: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿을 렌더링하지 못했습니다: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate은 템플릿을 해석한다.
//
// missingkey=zero는 없는 map key를 오류 대신 영값으로 표시한다. 여기 문맥은 구조체이며
// .Items가 비어도 range가 실패하지 않게 한다. 실제 주의할 것은 nil .Items다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs는 템플릿용 보조 함수다.
var webhookTemplateFuncs = template.FuncMap{
	// json은 임의 값을 JSON으로 직렬화한다.
	//
	// 이 함수가 없으면 사용자는 {{.Title}}로 직접 보간해야 하며 제목에 따옴표/줄바꿈이
	// 있기만 해도 전체가 유효한 JSON이 아니게 된다. 수신기가 거부하면서
	// JSON 해석 실패라고만 안내하면 원인이 제목의 따옴표임을 알아내기 어렵다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons는 JSON 조각을 다른 JSON 문자열 값에 넣기 위해 한 단계 문자열 이스케이프를 수행한다.
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥 따옴표를 제거하고 추가 여부는 호출자에게 맡긴다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
