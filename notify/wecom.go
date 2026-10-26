package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit은 WeCom 그룹 봇 markdown content의 바이트 상한이다(문자 수 아님).
// 여섯 채널 중 가장 엄격한 제한이며 TruncateBytes의 주요 존재 이유다.
const weComMarkdownLimit = 4096

// weComChannel은 WeCom 그룹 봇을 구현한다.
//
// 플랫폼 특성:
//   - URL key로만 인증하고 서명은 지원하지 않아 webhook 주소가 전체 자격 증명이다.
//   - markdown content 상한은 4096바이트. 초과하면 잘리는 대신 전체가 거부된다.
//     중국어는 글자당 3바이트라 천여 글자만 가능하므로 클라이언트에서 잘라야 한다.
//   - 분당 20건 제한도 클라이언트가 제어한다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom은 URL key가 포함된 Webhook 하나만 자격 증명이며 서명을 지원하지 않는다.
// 주소 전체가 자격 증명이라 다른 마스킹 필드는 없다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// Webhook이 목적지와 자격 증명을 겸하므로 주소 변경 후 남는 별도 자격 증명은 없다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 요약 묶음은 50개 행과 접두사로 4096바이트를 쉽게 초과할 수 있다.
	// 플랫폼 거부는 전체 누락이므로 여기서 잘라 최소 앞부분이라도 전달한다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009는 호출 제한 초과다. 제한 구간이 이동하므로 백오프 후 재시도가 효과적이다.
		// 따라서 재시도 가능으로 분류한다. 이 경우 클라이언트 rate_per_min이 과도한 것이므로
		// 재시도는 보완 수단이고 실제 해결은 채널 속도 제한을 낮추는 것이다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom 요청이 제한되었습니다 (%d): %s", res.ErrCode, res.ErrMsg)
		}
		// 93000은 webhook key 무효. 영구 실패이므로 재시도로 회복되지 않는다.
		return 0, Permanent(fmt.Errorf("WeCom에서 오류가 발생했습니다 (%d): %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
