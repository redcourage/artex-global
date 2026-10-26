package notify

import (
	"net/url"
	"testing"
	"time"
)

// 서명 기준값은 자체 구현 대신 OpenSSL로 독립 계산했다.
// 자체 구현으로 생성하면 코드가 안 바뀐 것만 입증하고 알고리즘 정확성을 확인하지 못한다.
//
//	TS=1700000000000, SECRET=SECtest123
//
// DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//
//	-> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//
// Feishu: printf ” | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//
//	-> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("생성된 주소 해석 실패: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("서명 불일치\n예상 %s\n실제 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("timestamp는 밀리초로 그대로 전달해야 함, 실제 %q", q.Get("timestamp"))
	}
	// 기존 query(access_token)가 서명으로 덮어써지면 안 된다.
	if q.Get("access_token") != "tok" {
		t.Errorf("기존 query 매개변수 누락, 실제 %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("서명 불일치\n예상 %s\n실제 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer는 두 알고리즘의 차이를 고정한다. DingTalk는 key=secret,
// Feishu는 key=서명 문자열로 매개변수 순서가 반대라 다른 구현을 따르면 검증이 실패한다.
// 향후 리팩터링에서 두 알고리즘을 같은 함수로 합치지 않도록 확인한다.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk와 Feishu 서명이 같아 한쪽 알고리즘이 잘못됨")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 서명 미사용 봇에는 timestamp/sign을 임의로 추가하면 안 된다.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret 미설정 시 주소를 바꾸면 안 됨, 실제 %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q는 허용해야 함: %v", s, err)
		}
	}
	// file:// 등은 http.Client의 예상 밖 동작을 유발할 수 있어 허용하지 않는다.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q는 거부해야 함", s)
		}
	}
}
