package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets는 루프백/링크 로컬 주소로 발송을 허용할지 결정한다.
//
// 기본은 거부다. 이런 주소는 IM 봇이나 공용 메일 서버에 보통 쓰이지 않으며
// 같은 컴퓨터 서비스의 관리 포트나 클라우드 메타데이터 엔드포인트처럼 민감한 대상에
// 접근할 수 있다(169.254.169.254는 인스턴스 자격 증명을 반환할 수 있음). 주소는 관리자가 설정하지만
// XSS/CSRF로 이용된 관리자 세션이나 같은 JWT를 공유하는 다른 사람이 설정을 바꿔 응답을 읽을 수 있다.
// doJSON이 4xx/5xx 응답의 앞 200바이트를 last_error에 기록하고 발송 이력 API가
// 이를 표시하므로 부분적으로 응답을 읽는 경로가 생긴다.
//
// 다만 127.0.0.1:25의 postfix 같은 로컬 SMTP 중계는 자체 메일의 일반적 설정이므로
// 일괄 거부 대신 명시적인 허용 설정을 제공한다.
// ARTEX_NOTIFY_ALLOW_LOCAL=1로 설정하면 허용된다.
//
// AllowLocalTargetsEnv를 내보내 테스트에서 명시적으로 켤 수 있게 한다. 이 패키지와 server 테스트가
// 127.0.0.1의 httptest 수신기를 많이 사용하므로 켜지 않으면 가드에 모두 차단된다.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP는 대상 IP가 기본 발송 금지 대역인지 반환한다.
//
// 루프백, 링크 로컬(클라우드 메타데이터 169.254.169.254 포함), 미지정, 멀티캐스트만 거부한다.
// RFC1918 사설망은 거부하지 않는다. 내부 Mattermost/SMTP 중계는 정상적인 일반 사용이며
// 함께 차단하면 실제 환경에서 기능을 쓰지 못하므로 의도적으로 허용한다.
// 민감한 대상을 차단하면서 정상 배포는 유지해야 한다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6(::ffff:127.0.0.1)는 IPv4로 복원해 검사 우회를 막는다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial은 http.Transport 다이얼러의 Control 훅으로 실제 연결 시
// 대상 주소를 검사한다.
//
// 설정 저장 검사만 하지 않고 연결 단계에서 검사하는 이유는 최종 적용 지점이기 때문이다.
// 검사 시 공인 IP였지만 연결 때 내부 IP가 되는 DNS 리바인딩과
// 리디렉션을 모두 다룬다. 호스트 간 이동은 거부하지만 같은 호스트의 이동도
// 다른 경로를 가리킬 수 있다.
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소를 해석할 수 없습니다: %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬 또는 링크 로컬 주소 %s 로는 전송하지 않습니다 (로컬 서비스로 보내야 하면 %s=1 로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport는 기본 Transport에 연결 가드만 추가한다.
// Clone으로 연결 풀/HTTP2/시간 제한/proxy 등 기본 튜닝을 유지해
// 검사 하나 때문에 다른 동작을 바꾸지 않는다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient는 모든 채널 발송에서 공유한다.
//
// 침투 대상 트래픽용 전역 외부 프록시(server의 GlobalProxy)는 재사용하지 않는다.
// 불안정한 터널일 수 있어 알림 가용성이 대상 네트워크 상태에 좌우되면 안 된다.
// IM 알림은 직접 연결하며 제한은 15초다. 이보다 느린 상대는 사실상 장애 상태다.
//
// 호스트 간 리디렉션은 거부한다. 발송은 고정 endpoint라 정상적으로 다른 호스트에
// 이동할 일이 없고 DingTalk access_token, WeCom key, Telegram bot token 등의
// 자격 증명이 URL에 있어 이동을 따르면 대상에 전달하게 된다.
// 끝 슬래시 추가 같은 동일 호스트 이동은 허용한다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리다이렉트가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("다른 호스트로의 리다이렉트는 허용하지 않습니다 (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit은 응답 본문 읽기 크기를 제한한다. 상대 오류로 큰 응답이 와도
// 발송 이력에는 오류 코드와 짧은 설명만 필요하다.
const respBodyLimit = 8 << 10

// doJSON은 요청을 보내고 길이 제한된 응답 본문을 반환한다.
//
// payload=nil이면 빈 body를 보낸다(GET 또는 본문이 필요 없는 플랫폼용).
// headers의 key/value는 일반 Webhook 사용자 헤더로 그대로 추가한다.
//
// 핵심 책임은 오류 분류다. 네트워크 실패와 5xx/408/429는 재시도 가능,
// 나머지 4xx는 영구 실패다. 403 재시도는 같은 오류 로그를 세 번 남길 뿐이다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 잘못된 설정 필드 유형 등 로컬 버그이므로 재시도로 해결되지 않는다.
			return nil, Permanent(fmt.Errorf("요청 본문을 만들지 못했습니다: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// 잘못된 URL은 사용자 입력 오류일 가능성이 높아 영구 실패다.
		// url.Parse 오류 문구에는 전체 주소가 있어 err를 그대로 전달하면 안 된다.
		return nil, Permanent(fmt.Errorf("요청 주소가 올바르지 않습니다: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거부/DNS 실패/시간 초과는 주로 순간 장애이므로 백오프 재시도에 맡긴다.
		//
		// 오류 문구는 민감정보를 제거하고 전달해야 한다. http.Client.Do의 *url.Error는
		// `Op "전체URL": 내부 오류` 형식인데 채널 자격 증명이 URL에 있다.
		// DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/ 등이 해당한다.
		// 제거하지 않으면 자격 증명이 오류 문자열을 따라 notification_deliveries의
		// last_error(DB 평문), 발송 이력 API 응답(설정 마스킹 우회),
		// 서버 로그, 테스트 발송 API의 프런트엔드 502 응답에 유출된다.
		return nil, fmt.Errorf("요청에 실패했습니다: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답을 읽지 못했습니다: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429(속도 제한)와 408(시간 초과)는 재시도하고 나머지 4xx는 설정/권한 문제라 재시도하지 않는다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대 서버가 요청을 제한하거나 응답이 지연되었습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 서버에 오류가 발생했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대 서버가 요청을 거부했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet은 응답을 한 줄의 짧은 오류 문구로 만든다. 줄바꿈과 과한 공백을
// last_error에 그대로 넣으면 발송 이력 페이지 배치가 망가진다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget은 오류 표시용 발송 주소를 scheme://host/… 형식으로 줄인다.
//
// 패키지의 유일한 주소 가림 규칙으로 scheme과 host만 남기고
// 모두 제거한다. URL 중 자격 증명 부분을 일반적이고 안전하게 판단할 방법이 없기 때문이다.
//
// DingTalk: query의 자격 증명 /robot/send?access_token=xxx
// WeCom: query의 자격 증명 /cgi-bin/webhook/send?key=xxx
// Feishu: 경로 마지막 구간의 자격 증명 /open-apis/bot/v2/hook/<hook_id>
// Telegram: 경로 중간 구간의 자격 증명 /bot<token>/sendMessage
//
// 유용한 부분만 남기려면 채널별 패치가 필요하고 하나라도 누락하면 자격 증명이 유출된다.
// host만으로도 DNS/연결/인증서 문제를 확인할 수 있고 구체적인 봇은
// 채널 설정의 마스킹 끝자리로 식별한다.
//
// 해석 실패 시 고정 플레이스홀더를 반환하며 원본 문자열을 표시하지 않는다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소를 해석할 수 없음)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError는 전송 오류에서 주소를 제거하고 내부 원인만 남긴다.
//
// *url.Error 구조는 {Op, URL, Err}이며 Error()는 URL도 출력한다.
// 여기서는 Err를 직접 읽어 Error()를 우회한다. URL 인코딩/이스케이프 변형을
// 빠짐없이 처리해야 하는 사후 문자열 치환보다 신뢰할 수 있다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error가 아닌 리디렉션 정책 오류 등도 주소를 포함할 수 있어 동일하게 가린다.
	return redactURLsInText(err.Error())
}

// redactURLsInText는 텍스트의 http(s) 주소를 가린 형태로 치환한다.
//
// 구조화된 필드가 없는 오류(리디렉션 정책/외부 라이브러리 사용자 오류)를 보완한다.
// http/https 접두사만 인식하고 URL에 포함되지 않는 공백/따옴표로 구분한다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
