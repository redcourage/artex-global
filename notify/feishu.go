package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel은 Feishu(Lark 포함) 사용자 정의 봇을 대화형 카드로 구현한다.
//
// 플랫폼 특성:
//   - 서명 알고리즘이 DingTalk와 달라 실수하기 쉽다(feishuSign 참조).
//   - 업무 오류를 HTTP 200 body에 담는다(code != 0).
//   - 카드 header 색상 템플릿을 심각도에 연결해 목록에서 위험도를 바로 알 수 있다.
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// Feishu 사용자 정의 봇은 약 초당 5회, 분당 100회.
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook 주소의 마지막 구간은 봇 고유 식별자로 자격 증명이다.
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// Webhook 주소 변경 시 새 주소에 대한 서명 키를 다시 명시해야 한다.
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 서명 매개변수는 메시지와 같은 계층이며 secret 설정 시에만 포함한다.
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 일부 Feishu hook 버전은 이 필드 이름을 사용하므로 함께 지원한다.
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Feishu 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu에서 오류가 발생했습니다 (%d): %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu에서 오류가 발생했습니다 (%d): %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign은 공식 규칙에 따라 서명을 계산한다.
//
// 실수하기 쉬운 부분: 공식 예시는 아래와 같다.
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// 즉 key=timestamp + "\n" + secret, message는 비어 있다.
// 직관적인 key=secret/message=stringToSign은 DingTalk 알고리즘이다. 서로 반대라
// 다른 구현을 따라 하면 서명 검증이 실패한다(19021).
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate은 심각도를 카드 header 색상 템플릿에 연결한다.
// 알 수 없는 심각도는 low의 blue와 구별하기 위해 grey를 쓴다.
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes는 보수적인 카드 크기 상한이다. 초과 시 전체가 거부되므로
// JSON 포장 비용까지 고려해 공식 상한보다 충분히 낮게 잡는다.
const feishuMaxCardBytes = 24000

// feishuCard는 카드와 실제 포함 항목 수를 반환한다.
// kept는 markdownBody와 마찬가지로 실제 카드에 들어간 항목만 완료 처리하기 위해 쓴다.
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 먼저 항목 단위로 묶은 뒤 머리를 붙인다. 다음 메시지에 계속할 N건 안내의
		// N은 실제 포함 수에서 계산해야 한다.
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("플랫폼에서 전체 보기", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("상세 보기", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines는 개별 취약점의 lark_md 본문을 렌더링한다.
//
// lark_md도 링크와 강조를 해석하므로 외부 필드는 모두
// markdownText로 한 줄 변환과 이스케이프를 거친다. 그렇지 않으면 제목 하나가
// Feishu에서 클릭 가능한 외부 링크로 바뀔 수 있다.
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**상태 변경**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**유형**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**자산**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**개요**: %s", s)
		}
	}
	return out
}

// feishuBatchLine은 요약 카드 항목 하나를 렌더링한다.
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
