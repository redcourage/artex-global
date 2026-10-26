package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout/emailSessionTimeout은 각각 연결과 전체 SMTP 세션을 제한한다.
// net/smtp 자체에는 시간 제한이 없어 멈춘 상대가 발송 goroutine을
// 영구 대기시킬 수 있다. dispatcher는 단일 goroutine의 직렬 처리이므로
// 전체 알림 시스템이 멈추는 셈이다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel은 SMTP 이메일 발송을 구현한다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 플랫폼 제한은 없지만 과도한 발송을 피하도록 여유 있는 기본 한도를 둔다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 마스킹한다. 호스트/계정/수신자는 비밀이 아니며 마스킹하면 편집이 어려워진다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port는 비밀번호를 전달할 서버, tls는 전송 암호화를 결정한다. 하나라도 바뀌면
// 비밀번호를 다시 명시해야 하므로 TLS 끄기도 자격 증명을 명시한 변경이 된다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소가 없습니다")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트가 올바르지 않습니다 (1-65535 범위여야 합니다)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("발신자 주소가 없습니다")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 최소 하나 필요합니다")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// 상대가 STARTTLS를 지원하면 업그레이드한다. 평문 세션에서는 자격 증명을 보내면 안 된다(아래 auth 참조).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth는 localhost 외 비암호화 연결의 자격 증명 전송을 거부한다.
			// 올바른 보안 동작이므로 우회하지 않되 이유를 명확히 설명해야 한다.
			// 그렇지 않으면 사용자는 unencrypted connection만 보고 대응 방법을 알 수 없다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 전송을 거부했습니다: 연결이 암호화되지 않았습니다. TLS 를 켜거나, 465 포트(암시적 TLS)를 사용하거나, 「TLS 사용」을 체크하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("발신자 %s 주소가 거부되었습니다", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 주소가 거부되었습니다", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일 제출 실패: %w", err)
	}
	// Quit 실패는 서버의 메일 수신 사실을 바꾸지 않으므로 무시한다.
	_ = client.Quit()
	// 메일은 HTML 전체를 보내며 길이 자르기가 없어 전체 묶음이 전달된다.
	return len(m.Items), nil
}

// emailDial은 SMTP 연결을 생성한다.
//
// implicitTLS=true는 465 같은 즉시 TLS 방식, false는 25/587 평문 연결 후
// STARTTLS다. 465에 평문 greeting을 보내면 바로 끊기므로 혼용하면 안 된다.
//
// net/smtp Client가 연결을 비공개 필드에 숨기므로 연결 시점에 세션 deadline을
// 설정해야 한다. 넘긴 뒤에는 외부에서 접근할 수 없어 미리 설정한 제한에
// 의존하며 이는 핸드셰이크 중 멈춤도 포함한다.
// Control에 blockInternalDial을 연결해 HTTP 채널과 가드를 공유한다. 없으면 SMTP가
// SSRF 보호의 빈틈이 되어 169.254.169.254나 127.0.0.1에 직접 연결할 수 있다.
// smtp.NewClient 핸드셰이크 오류가 상대 응답 행을 담아 last_error와
// 발송 이력에 노출할 수 있고 연결 거부/시간 초과의 시간 차이로 포트 탐지도 가능하다.
// 연결 단계는 최종 적용 지점으로 DNS 리바인딩도 처리한다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError는 SMTP 응답 코드로 단계 실패를 재시도 가능/영구 실패로 분류한다.
//
// SMTP 4xx와 5xx는 의미가 다르므로 구분해야 한다.
//   - 4xx(450 그레이리스트/451 로컬 오류/452 저장 공간 부족)는 임시 거부이며
//     나중에 재시도하는 것이 정상이다. 그레이리스트는 첫 발송 때 특히 흔하다.
//   - 5xx(550 사용자 없음/553 잘못된 주소)는 영구 거부라 재시도할 의미가 없다.
//
// 모두 영구 실패로 보면 그레이리스트 서버의 모든 알림이 첫 시도 후 failed가 된다.
// 이런 실패야말로 자동 재시도가 필요한 상황이다.
// 오류 문구의 앞 세 자리 숫자를 응답 코드로 사용하며 없으면 재시도 가능으로 처리해
// 해석 불가를 이유로 순간 장애를 영구 실패로 확정하지 않는다.
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode는 오류 문구 앞의 세 자리 코드를 읽고 없으면 0을 반환한다.
// net/smtp는 코드 필드를 공개하지 않아 "450 4.7.1 ..." 형식의 텍스트에서 읽는다.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage는 전체 RFC 5322 메일을 구성한다.
//
// 본문 base64 이유: SMTP 행 제한 1000바이트를 HTML, 특히 요약 메일이 쉽게 넘기며
// base64에는 "."로 시작하는 행이 생기지 않아
// SMTP 점 이스케이프도 피할 수 있다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 중국어 제목은 RFC 2047로 인코딩해야 클라이언트에서 깨지지 않는다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 메일에는 길이 상한이 없어 본문을 자르지 않는다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64는 RFC 2045에 따라 76문자마다 줄바꿈한다.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
