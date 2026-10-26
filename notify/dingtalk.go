package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel은 DingTalk 사용자 정의 봇을 구현한다.
//
// 구현 선택에 영향을 주는 플랫폼 특성:
//   - 봇당 분당 20건 초과는 HTTP 200에서도 조용히 버려질 수 있어
//     클라이언트 속도 제한이 필요하다(DefaultRatePerMin 참조).
//   - 보안 설정은 서명/사용자 키워드/IP 허용 목록 중 선택한다. 내용에 의존하지 않는
//     서명만 지원하며 보안 설정 없는 webhook도 지원한다.
//   - 성공/실패 모두 HTTP 200이므로 body의 errcode로 구분해야 한다.
//     검사하지 않으면 발송 실패를 성공으로 기록한다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// Webhook URL의 access_token 자체가 자격 증명이므로 주소 전체를 마스킹한다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 대상은 Webhook 주소이며 변경 시 새 주소의 서명 키도 명시해야 한다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

// Send는 단건에 상세 링크가 있으면 버튼 있는 ActionCard, 나머지는 markdown으로 발송한다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// markdown 본문의 명확한 바이트 제한은 없지만 증거 필드의 과도한 증가를 막기 위해 상한을 둔다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk는 업무 오류를 HTTP 200 응답에 담는다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000은 서명 검증 실패, 310000은 키워드 불일치로 모두 설정 오류이며
		// 재시도로 해결되지 않는다.
		return 0, Permanent(fmt.Errorf("DingTalk에서 오류가 발생했습니다 (%d): %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL은 공식 서명 규칙에 따라 webhook에 timestamp/sign을 추가한다.
//
// 서명 문자열=timestamp + "\n" + secret, HMAC-SHA256의 키도 secret이다.
// 결과를 base64 후 URL 인코딩한다. timestamp는 밀리초. secret이 비면 원본을 반환해
// 서명을 켜지 않은 봇을 지원한다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// url.Parse 오류에 access_token을 포함한 전체 주소가 있어 err를 그대로 전달하지 않는다.
		return "", fmt.Errorf("Webhook 주소를 해석하지 못했습니다: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL은 주소와 프로토콜을 검증하고 리터럴 IP의 내부 주소 여부를 판단한다.
//
// 주의할 두 가지:
//
//  1. 오류 정보는 가려야 한다. url.Parse의 *url.Error.Error()에는
//     전체 원본 주소가 있고 여기에 DingTalk access_token, WeCom key,
//     Telegram bot token, Feishu hook id 등이 포함된다. 과거 `return err`는
//     주소 형식 오류를 통해 테스트 API 400 응답, 발송 last_error,
//     서버 로그와 발송 이력 API에 자격 증명을 전달했다.
//
//  2. 리터럴 IP는 즉시 내부 주소를 판단하고 도메인은 최종 연결 단계
//     blockInternalDial에서 확인해 DNS 리바인딩도 막는다. 여기서 검사하는 것은
//     첫 발송 실패까지 기다리지 않고 설정 저장 때 알려주기 위해서다.
//
// 프로토콜 제한은 방어 목적이다. file:///gopher:// 등은 http.Client에 예상 밖의
// 동작을 일으킬 수 있으며 scheme 검사 외에도 허용할 이유가 없다.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다 (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http 또는 https 만 지원합니다. 받은 값: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트 이름이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬 또는 링크 로컬 주소 %s 로는 전송하지 않습니다 (로컬 서비스로 보내야 하면 %s=1 로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
