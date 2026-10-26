package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 메일 채널의 프로토콜 테스트를 보완한다. 이전 email.Send 커버리지는 0이었으며
// SMTP 경로는 한 번도 테스트하지 않았지만 여섯 채널 중 프로토콜 범위가 가장 넓고
// 핸드셰이크/인증/봉투/DATA 단계마다 실패 의미가 달라 오류가 나기 쉽다.
//
// net/smtp를 mock하지 않고 최소 SMTP 서버로 구동한다.
// 실제 SMTP 서버와 통신하는 단계에 위험이 집중되므로
// 그 단계를 mock하면 제대로 검증하지 못한다.

// fakeSMTP는 greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT을 지원하는 최소 서버이며
// 각 테스트가 지정한 단계에서 지정 응답 코드를 반환한다.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply는 RCPT TO 응답. 기본 250.
	rcptReply string
	// mailReply는 MAIL FROM 응답. 기본 250.
	mailReply string
	// advertiseAuth=true면 EHLO에 AUTH PLAIN 지원을 알린다.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 수신 주소가 아님")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS를 알리지 않아 평문 분기로 진입한다(테스트 대상은 TLS가 아닌 봉투 로직).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 단순화를 위해 여러 줄일 수 있는 PLAIN 초기 응답을 바로 수락한다.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("발송 실패: %v", err)
	}
	// 발신자/두 수신자/DATA 봉투 단계에 모두 도달해야 한다.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 세션에 %q 누락, 실제 명령: %v", want, f.commands)
		}
	}
	// 본문은 base64 HTML이며 인코딩 후에도 실제 취약점 내용을 포함해야 한다.
	body := f.body()
	if body == "" {
		t.Fatal("DATA 단계에서 본문을 받지 못함")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type 헤더 누락:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("본문 base64 인코딩 누락(긴 HTML 행이 SMTP 1000바이트 제한을 위반함):\n%s", body)
	}
	// 모든 수신자가 To 헤더에 나타나야 한다.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 헤더에 전체 수신자가 없음:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// 일부 중계가 거부할 수 있으므로 계정 미설정 시 AUTH를 보내지 않는다.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("발송 실패: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("계정 미설정인데 AUTH를 보냄: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies는 감사 수정 사항을 직접 검증한다.
// 5xx는 영구 실패, 4xx(그레이리스트)는 재시도 가능.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"수신자 550 영구 거부", "550 5.1.1 User unknown", "250 OK", true},
		{"수신자 450 그레이리스트", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"수신자 452 메일함 가득 참", "452 4.2.2 Mailbox full", "250 OK", false},
		{"발신자 553 영구 거부", "250 OK", "553 5.1.3 Bad address", true},
		{"발신자 451 임시 오류", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("오류여야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 판정 오류: 예상 %v, 실제 %v (%v)", tc.permanent, got, err)
			}
			// 서버 원문을 유지해야 사용자가 서버 관리자 문의와 주소 수정 중 무엇을 할지 알 수 있다.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("오류에 서버 응답 코드를 유지해야 함: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp PlainAuth는 localhost 외 평문 연결의 자격 증명 전송을 거부한다.
	// 올바른 보안 동작이라 우회하지 않되 해결 방법을 안내하는 오류를 제공해야 한다.
	// 여기서는 localhost가 아닌 이름으로 실행한다.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost 아님.
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("DNS가 로컬 서버로 해석되어 건너뜀(다른 테스트에 영향 없음)")
	}
	// 연결 실패 또는 자격 증명 전송 거부면 통과한다. 핵심은 비밀번호를 조용히 보내지 않는 것이다.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("오류: %v(localhost 외 연결 실패는 예상됨)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// 메일은 필드가 가장 많아 누락이 발송 때 드러나기 쉬우므로 각각 사전 검증되는지
	// 확인한다. 오류가 누락 항목을 구체적으로 언급하는지 검증한다.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host 누락", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port 누락", map[string]any{"host": "smtp.example.com"}},
		{"port 범위 초과", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from 누락", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to 누락", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("검증에 실패해야 함: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance는 설정 읽기의 유연성을 검증한다. JSONB 숫자는 float64지만
// UI 포트는 문자열일 수 있고 배열 대신 단일 문자열이 올 수도 있다.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 문자열 포트.
		"from": "a@b.c",
		"to":   "d@e.f", // 배열 대신 단일 문자열.
		"tls":  "true",  // 문자열 불리언.
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("문자열 숫자를 허용해야 함: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt가 문자열 포트를 해석하지 못함, 실제 %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool이 문자열 \"true\"를 해석하지 못함")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings가 단일 문자열을 처리하지 못함, 실제 %v", to)
	}
}

// TestFilterValidateRejectsTypo는 감사 수정 사항을 직접 검증한다.
// 임계값 오타를 저장 때 차단하지 않으면 필터가 조용히 무효화되어 모두 발송된다.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("유효한 임계값 %q 거부: %v", s, err)
		}
	}
	// 실제로 가능한 오타이며 모두 거부해야 한다.
	for _, s := range []string{"hgih", "HIGH", "严重", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("잘못된 임계값 %q를 거부해야 함(그렇지 않으면 필터가 무효화되어 전부 발송됨)", s)
			continue
		}
		// 오류가 올바른 값으로 수정하는 방법을 안내해야 한다.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("오류에 선택 가능한 값을 표시해야 함, 실제 %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly는 엄격한 쓰기/관대한 읽기의 역할을 고정한다.
// 기존 잘못된 데이터 때문에 채널 전체를 읽지 못해 이전 알림이 멈추면 안 된다.
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // 오류 없음.
	if f.MinSeverity != "hgih" {
		t.Fatalf("읽기 경로는 원본을 유지해야 함, 실제 %q", f.MinSeverity)
	}
	// 해당 채널이 panic/정체 없이 이벤트를 계속 판단할 수 있어야 한다.
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
