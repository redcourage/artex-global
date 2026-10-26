package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// doJSON의 HTTP 오류 분류를 검증한다.
//
// 채널 어댑터는 자체 업무 코드(DingTalk errcode/Feishu code/Telegram ok)를 판단하고
// HTTP 계층은 doJSON이 별도로 분류하므로 독립된 두 방어 계층이다.
// 이 검증이 없으면 503 중계 게이트웨이를 영구 실패로 오판해 재시도를 포기하거나
// 403을 재시도 가능으로 보아 불필요한 백오프 세 번이 발생할 수 있다.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 성공은 오류 아님", 200, false},
		{"429 속도 제한 재시도 가능", 429, false},
		{"408 요청 시간 초과 재시도 가능", 408, false},
		{"500 서버 오류 재시도 가능", 500, false},
		{"502 게이트웨이 오류 재시도 가능", 502, false},
		{"503 서비스 사용 불가 재시도 가능", 503, false},
		{"400 매개변수 오류 영구 실패", 400, true},
		{"401 인증 오류 영구 실패", 401, true},
		{"403 접근 금지 영구 실패", 403, true},
		{"404 주소 없음 영구 실패", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx는 오류가 아니어야 함: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx가 아니면 오류여야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d의 permanent 판정 오류: 예상 %v, 실제 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 상태 코드가 있어야 사용자 설정 문제와 상대 장애를 구별할 수 있다.
			// Go 영문 StatusText 대신 숫자를 검증한다. 문구는 현지화되므로
			// 숫자가 언어와 무관한 안정적인 검증 대상이다.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("오류에 HTTP 상태 코드 %d가 있어야 함, 실제 %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet은 상대 오류 설명을 가져오는지 검증한다.
// 그렇지 않으면 사용자는 실패 사실만 알고 거부 이유를 모른다.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 함")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("오류에 상대 설명이 있어야 함, 실제 %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded는 snippet 형식을 제한한다.
// 상대 응답이 last_error와 표에 들어가므로 여러 줄/과도한 길이는 배치와 데이터를 망가뜨린다.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈/탭/5000문자 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 함")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("오류는 한 줄이어야 함, 실제 %q", msg)
	}
	// snippet은 최대 200문자 + 고정 접두사라 전체가 원본보다 충분히 작아야 한다.
	if len(msg) > 400 {
		t.Errorf("오류가 너무 김(%d바이트), snippet으로 잘라야 함: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse는 읽기 상한을 검증한다. 비정상 대형 응답을
// 전부 메모리에 읽으면 안 된다(각 발송 이력에 last_error 저장).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 함")
	}
	if len(err.Error()) > 400 {
		t.Errorf("큰 응답은 읽기와 길이를 제한해야 함, 오류 길이 %d", len(err.Error()))
	}
}
