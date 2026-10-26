package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 채널 구현의 모든 오류 문구에 자격 증명을 포함하지 않는 불변식을 검증한다.
//
// 별도 파일로 둔 이유: 초기 테스트는 성공과 플랫폼 업무 오류만 다루고
// 연결 거부/DNS 실패/시간 초과 등 가장 위험한 전송 오류를 확인하지 않았다.
// http.Client.Do의 *url.Error가 전체 URL을 오류 문구에 담는데
// 채널 자격 증명이 URL에 있어 다음 네 경로로 유출되었다.
//
// notification_deliveries.last_error → DB 평문 저장.
// GET /api/notify/deliveries 응답 → 설정 마스킹을 우회해 브라우저에 표시.
// 서버 로그 → 외부 전송/보관될 수 있음.
// 테스트 발송 API 502 응답 → 프런트엔드에 직접 표시.
//
// 따라서 함수 하나만 확인하지 않고 각 채널에서 반드시 실패하는 실제 요청을 보내
// 오류 문구에 자격 증명이 없는지 검증한다.

// credentialCases는 URL에 자격 증명이 있는 모든 채널 형식을 다룬다.
// DingTalk/WeCom은 query, Feishu는 경로 끝, Telegram은 경로 중간.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "query의 DingTalk access_token",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "query의 WeCom key",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "경로 끝의 Feishu hook id",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "경로 중간의 Telegram bot token",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk 서명 키",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken은 실제 자격 증명일 수 없는 검색용 센티널 값이다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials는 핵심 불변식을 검증한다.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 127.0.0.1:1은 수신자가 없어 연결 거부 경로로 실패한다.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "유출 검사"}},
			})
			if err == nil {
				t.Fatal("접근 불가 주소는 오류여야 함")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath는 영구 실패 분기를 검증한다.
// URL 검증 실패와 플랫폼 오류도 외부에 표시되므로 자격 증명을 포함하면 안 된다.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 자격 증명 포함 잘못된 주소로 validateHTTPURL/url.Parse 분기를 실행한다.
		{"잘못된 DingTalk 주소", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"잘못된 WeCom 주소", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"잘못된 Feishu 주소", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"잘못된 Telegram API 주소", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"잘못된 일반 Webhook 주소", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("잘못된 설정은 오류여야 함")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("오류 문구가 자격 증명 %q를 유출함:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, 예상 %q", in, got, want)
		}
		// 가려진 결과에 원본 경로/query 조각이 남아 있으면 안 된다.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("가린 뒤 경로/query 조각 %q가 남음: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 해석 불가 입력도 원본을 표시하면 안 된다.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("해석 불가 입력 %q가 %q로 표시됨", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL은 http.Client.Do가 반환하는 *url.Error를
// 직접 확인한다. URL 유출이 처음 발생하는 곳이다.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("문제 확인용 host를 유지해야 함, 실제 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("문제 확인용 내부 원인을 유지해야 함, 실제 %q", got)
	}
	// 문제 확인에 유용한 Op(POST/GET)도 유지해야 한다.
	if !strings.Contains(got, "Post") {
		t.Errorf("작업 이름을 유지해야 함, 실제 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback은 *url.Error가 아닌 사용자 정의 오류의
// 주소(리디렉션 정책 오류 등)도 제거하는 보완 경로를 검증한다.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("호스트 간 리디렉션 거부(a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("주소를 가린 형태로 대체해야 함, 실제 %q", got)
	}
	// 주소가 없는 텍스트는 그대로 유지한다.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("주소 없는 텍스트를 바꾸면 안 됨")
	}
}

// TestCrossHostRedirectRefused는 URL 자격 증명이 호스트 간 이동으로 유출되는 경로를 검증한다.
// 두 httptest 서버는 127.0.0.1의 다른 포트이므로 Host가 달라
// 호스트 간 리디렉션을 구성한다.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("호스트 간 리디렉션은 거부해야 함")
	}
	if hit {
		t.Fatal("이동 대상에 접근함: 리디렉션으로 자격 증명이 유출됨")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed는 끝 슬래시 추가 같은 동일 호스트 이동을 허용하는 반대 사례다.
// 정상 작업 흐름까지 차단하면 안 된다.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 같은 호스트/포트 리디렉션.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("동일 호스트 리디렉션은 거부하면 안 됨: %v", err)
	}
}
