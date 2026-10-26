package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("마스킹 값에 전체 자격 증명 유출: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("마스킹 값이 주소 본체를 노출하면 안 됨: %q", got)
	}
	// 끝 6자리를 남겨 어떤 봇인지 식별할 수 있게 한다.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("식별용 끝 6자리를 유지해야 함: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("IsMasked가 마스킹 값을 인식해야 함: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 짧은 자격 증명의 끝 6자리를 노출하면 전체가 드러날 수 있다.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("길이 %d 자격 증명은 끝자리 표시하면 안 됨, 실제 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("마스킹 값에 원본이 포함됨: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s를 마스킹해야 함, 실제 %q", k, s)
		}
	}
	// 일반 필드는 UI 표시를 위해 그대로 유지해야 한다.
	if masked["port"] != float64(587) {
		t.Errorf("일반 필드 port를 바꾸면 안 됨: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// 알 수 없는 채널은 자격 증명 가능성이 있는 원본을 반환하는 대신 빈 설정을 표시한다.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("알 수 없는 채널은 빈 설정이어야 함, 실제 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// 마스킹은 표시 계층 동작이며 DB의 실제 값을 바꾸면 안 된다.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig가 입력을 바꾸면 실제 자격 증명이 마스킹으로 덮어써짐")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// 사용자는 method만 변경했고 브라우저는 마스킹 값과 새 method를 제출한다.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("마스킹 필드는 DB 원본을 유지해야 함, 실제 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("변경된 필드가 적용되어야 함, 실제 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("빈 문자열은 필드를 제거해야 함, 실제 %v", got)
	}
	// 언급하지 않은 필드는 부분 갱신 규칙에 따라 유지한다.
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("생략된 필드는 유지해야 함, 실제 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("생략된 필드는 유지해야 함, 실제 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("명시한 필드는 갱신해야 함, 실제 %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap은 중요한 보안 불변식이다.
// 목적지 주소 변경 시 이전 자격 증명을 가져가면 안 된다.
//
// 이 테스트는 주소만 바꾸고 자격 증명은 언급하지 않는 실제 공격 형태를 사용한다.
// 정상 입력만 확인하면 보호가 작동하지 않아도 테스트가 통과할 수 있다.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing은 오류에 명시되어야 할 자격 증명 key다.
		wantMissing string
	}{
		{
			name: "Webhook 주소 변경 후 Authorization 재사용 시도",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram base_url 변경으로 Bot Token을 자체 endpoint에 전달 시도",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "SMTP 호스트 변경으로 비밀번호 전달 시도",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "이메일 TLS 해제도 비밀번호 재명시 필요",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// 마스킹 값도 이전 자격 증명 재사용이므로 주소 변경 시 거부해야 한다.
			name:        "마스킹 자격 증명 반환과 새 주소",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk Webhook 변경 후 서명 키 재사용 시도",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("주소 변경 시 자격 증명 미명시는 거부해야 함, 설정 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("조치 가능한 안내용 전용 오류 타입이 필요함, 실제 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("누락 자격 증명 key %q를 명시해야 함, 실제 %v", tc.wantMissing, target.Missing)
			}
			// 오류는 사용자가 해결 방법을 알 수 있게 해야 한다.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("오류에 %q가 있어야 함: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits는 정상 편집을 잘못 차단하지 않는 반대 사례다.
// 불편한 오차단이 반복되면 보호를 우회하거나 제거하게 된다.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "이름만 변경하고 설정은 그대로 반환",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "주소와 자격 증명은 유지하고 요청 메서드만 변경",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "주소와 자격 증명을 함께 변경",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "주소 변경과 자격 증명 불필요를 명시",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "목적지가 아닌 Telegram chat_id 변경",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "목적지가 아닌 이메일 수신자 변경",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("정상 편집 오차단: %v", err)
			}
			if merged == nil {
				t.Fatal("병합 결과를 반환해야 함")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance는 오판하기 쉬운 세부 사항을 검증한다.
// JSON 포트는 float64지만 전달값은 int 등 다른 타입일 수 있다.
// == 비교가 같은 값을 변경으로 오판하면 이름만 바꾼 사용자에게
// 비밀번호 재입력을 요구해 보호 기능에 대한 신뢰를 낮춘다.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 같은 포트를 int로 제출한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("타입만 다른 동일 포트는 주소 변경이 아님: %v", err)
	}
	// 실제 포트 변경은 차단해야 한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("포트 변경은 차단해야 함")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination은 비워 둘 수 있는
// 목적지 필드를 검증한다. Telegram base_url 빈 값은 공식 API다.
//
// 과거에는 두 번째 저장부터 계속 실패했다.
//
// 생성 시 base_url:"" 저장(생성은 MergeConfig 없이 프런트 config 직접 저장).
// → 첫 저장에서 빈 문자열이 명시적 제거로 처리되어 key 삭제.
// → 두 번째 저장에서 incoming="", stored는 key 없음이라 주소 변경으로 오판.
// → bot_token이 마스킹 값이라 자격 증명 재입력 요구 400 오류.
//
// 사용자는 아무것도 안 바꿔도 Bot Token을 다시 붙여넣기 전에는 저장하지 못했다.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// 프런트 buildConfig()는 모든 필드에 값을 제출한다. 자격 증명은 마스킹, 빈 입력은
	// 빈 문자열이다. 변경 key만 보내지 않고 실제 출력을 재현한다.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 첫 저장: 채널 이름만 바꾸고 config는 그대로 반환한다.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("첫 저장 오차단: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("전제 변경: MergeConfig가 빈 문자열 key를 지워야 key 제거 후 경로를 검증할 수 있음")
	}

	// 두 번째 저장: 직전과 같은 내용이며 사용자는 변경하지 않았다.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("변경 없는 두 번째 저장 오차단: %v", err)
	}
	// 세 번째도 저장해 한 번만 되는 것이 아닌 안정된 동작인지 확인한다.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("세 번째 저장 오차단: %v", err)
	}
	// 자격 증명이 빈 문자열 처리로 함께 지워지지 않고 계속 유지되어야 한다.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token은 원본 유지, 실제 %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges는 앞 테스트와 짝을 이룬다.
// 빈 문자열과 key 부재를 동일하게 보더라도 실제 주소 변경은 허용하면 안 된다.
// 두 방향 모두 Token이 URL 경로에 담기는 Telegram의 자격 증명 외부 발송 경로다.
// base_url을 바꾸면 새 주소에 Token을 전달하는 셈이다.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 방향 1: 빈 값(공식 주소)에서 자체 주소로 변경.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("공식 주소에서 자체 주소로 변경 시 Token 재입력을 요구해야 함")
	}

	// 방향 2: 자체 주소를 비워 공식 API로 돌아가는 것도 주소 변경이다.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("자체 주소를 비워 공식 API로 돌아가도 Token 재입력을 요구해야 함")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// 채널이 목적지 key를 선언하지 않으면 PrepareConfigUpdate가 보호할 수 없다.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("채널 %s에 목적지 key가 없어 주소 변경 자격 증명 유출 방어가 적용되지 않음", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s에 자격 증명 key 선언 없음", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// 컴파일러가 모든 채널의 SecretKeys 구현을 강제하지만 빈 슬라이스를 반환하는
	// 채널은 자격 증명이 브라우저에 평문 표시되므로 내용도 확인한다.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("채널 %s의 마스킹 예상값이 테스트에 없음", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s에 자격 증명 필드가 없어 설정이 평문 표시됨", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer는 감사에서 발견한 빈틈을 검증한다.
// webhook.headers 같은 문자열 아닌 구조에 마스킹 센티널을 넣으면
// MergeConfig는 이를 마스킹으로 보지 않아 "__masked__"가 실제 헤더 값으로 저장되고
// 이후 인증이 오류 안내 없이 실패한다.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// 객체 내부에 마스킹 센티널 포함.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("구조 내부 마스킹 센티널은 실제 값 저장을 막기 위해 거부해야 함")
	}
	// 실제 새 값인 객체 전체 제출은 정상 허용한다.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("정상 새 요청 헤더 제출은 차단하면 안 됨: %v", err)
	}
}
