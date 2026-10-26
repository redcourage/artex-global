package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 두 관련 보호 동작을 검증한다.
//   ① 발송 주소로 서버를 내부망/클라우드 메타데이터 접근에 이용하지 못하게 함(SSRF).
//   ② 주소 검증 오류에 URL의 자격 증명을 포함하지 않음.
//
// 많은 테스트가 127.0.0.1 httptest 수신기를 사용해 기본 가드에 차단된다.
// 따라서 TestMain에서 AllowLocalTargetsEnv를 켜고 각 SSRF 테스트가
// 명시적으로 해제해 기본 거부 동작을 확인한다.

func TestMain(m *testing.M) {
	// 일반 테스트는 로컬 수신기에 연결 가능. SSRF 테스트가 일시 해제한다.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault는 핵심 SSRF 검증이다.
// 기본 설정에서 루프백 발송은 연결 계층이 거부해야 한다.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 허용 설정 해제=기본 동작.
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("기본 설정은 루프백 발송을 허용하면 안 됨")
	}
	if hit {
		t.Fatal("로컬 서비스에 요청이 도달함: 가드가 작동하지 않음")
	}
	// 로컬 SMTP 중계는 유효한 설정이므로 오류에 명시적 허용 방법을 안내해야 한다.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 안내에 명시적 허용 방법이 있어야 함: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn은 반대 사례다. 명시적으로 켜면 사용 가능해야
// 로컬 postfix/내부 중계 같은 정상 배포를 막지 않는다.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("명시적 허용 후 발송 가능해야 함: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 endpoint. 이 함수의 주요 존재 이유.
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped는 복원 후 판단해야 검사 우회를 막는다.
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s를 거부해야 함", s)
		}
	}
	// RFC1918은 의도적으로 허용한다. 자체 Mattermost/SMTP 중계는 일반적인 정상 사용이다.
	// 이 검증은 해당 선택을 고정해 나중에 사설망 차단을 추가할 경우 실패하게 하며
	// 조용히 기존 배포를 막는 대신 의식적인 결정을 요구한다.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s는 허용해야 함(사설망은 일반적인 정상 발송 대상)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets는 설정 단계의 사전 안내를 검증한다.
// 리터럴 IP는 첫 발송 실패를 기다리지 않고 저장할 때 거부해야 한다.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s를 설정 단계에서 거부해야 함", raw)
		}
	}
	// 공인/사설 주소는 정상 통과한다(사설 주소는 연결 단계에서도 차단하지 않음).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s는 검증을 통과해야 함: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials는 감사에서 발견한 누락 분기를 검증한다.
//
// url.Parse 실패는 전체 원본 주소가 담긴 *url.Error를 반환한다. 이전에는
// http.Client.Do 오류만 가렸고 영구 실패 테스트의 file/gopher/ftp 주소는
// url.Parse가 성공해 scheme 분기로 갔으므로
// 통과해도 이 경로의 안전성을 입증하지 못했다.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 잘못된 퍼센트 이스케이프.
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 숫자가 아닌 포트.
		"http://[::1?access_token=" + leakProbeToken,                  // 짝이 맞지 않는 괄호.
	}
	for _, raw := range cases {
		// 입력이 실제 url.Parse 실패를 일으키는지 먼저 확인한다. 그렇지 않으면
		// 눈치채지 못한 채 다른 분기를 검증할 수 있다.
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q 해석이 실패해야 목표 분기를 검증할 수 있음", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q 검증에 실패해야 함", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 채널 계층의 포장도 주소를 유출하지 않는지 확인한다.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("잘못된 주소는 검증에 실패해야 함")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault는 SMTP 연결 가드를 검증한다.
//
// 메일은 과거 단순 net.Dialer를 써 SSRF 방어의 빈틈이었다. host에
// 169.254.169.254/127.0.0.1을 넣으면 연결되고 smtp.NewClient 오류에
// 상대 응답이 담겨 last_error와 발송 이력으로 표시되었다. 다른 채널에서 막은
// 부분 응답 읽기 경로이며 연결 거부/시간 초과 차이로 포트 탐지도 가능했다.
//
// TestMain은 로컬 수신기 때문에 AllowLocalTargetsEnv를 전역으로 켜므로
// 이 테스트가 직접 해제해야 한다. 그렇지 않으면 가드 유무와 무관하게 통과하며
// 이것이 처음에 테스트로 누락을 찾지 못한 이유다.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 허용 설정 해제=기본 동작.
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("기본 설정은 루프백 이메일 발송을 허용하면 안 됨")
	}
	// Control 훅에서 차단하므로 연결 자체가 없어 EHLO를 보내면 안 된다.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 세션이 연결됨: 가드가 작동하지 않음")
	}
	// 로컬 postfix는 유효한 설정이므로 명시적 허용 방법을 안내해야 한다.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 안내에 명시적 허용 방법이 있어야 함: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn은 명시적으로 허용하면 정상 발송하는 반대 사례다.
// 자체 SMTP/로컬 중계는 흔한 배포이므로 가드가 일괄 차단하면 안 된다.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("명시적 허용 후 로컬 SMTP 발송이 가능해야 함: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO가 없어 실제 세션이 연결되지 않음")
	}
}
