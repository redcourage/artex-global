package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 알림 기능의 종단 간 동작을 검증한다: 취약점 저장 → 이벤트 → 분배 → 실제 HTTP 발송.
//
// 안전상 유의점: 이 테스트는 **전역 Notifier.step()을 호출하지 않고** 직접 생성한
// 채널에만 stepRealtime/stepDigest를 호출한다. step()은 DB의 모든 활성 채널을 순회하므로,
// 실제 DingTalk/WeCom 봇을 설정한 개발 DB에서 테스트를 실행하면 전역 step이 테스트 중
// 생성한 취약점을 실제 그룹에 보낸다. 채널별 호출로 영향을 테스트가 만든 모의 수신기에만 엄격히 제한한다.
//
// 정리: 테스트 종료 시 해당 테스트가 생성한 이벤트(발송 항목 연쇄 삭제)와 채널을 삭제해 실제 채널에 대기 항목을 남기지 않는다.
//
// 검증 기준: stepRealtime/stepDigest는 반환값 없이 내부 로그를 기록하므로 여기서는
// 함수 반환값 대신 **관측 가능한 외부 동작**(모의 수신기가 받은 내용, 발송 행의 최종 상태)을 검증한다.
// 반환값을 스텁 처리하는 것보다 실제 호출 경로에 가깝다.

// notifyFixture는 이 파일 테스트의 공통 장치다.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 직접 만든 task/exploration에 취약점을 기록해 다른 테스트 데이터와 격리한다.
	taskID int64
	expID  int64
	// cleanupMark 이후 생성된 이벤트는 정리 시 함께 삭제한다.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 이 파일의 모든 모의 수신기는 127.0.0.1에서 실행되지만 발송은 기본적으로 루프백 주소를 거부한다.
	// 동일 호스트 서비스와 클라우드 메타데이터에 대한 SSRF를 방지하기 위함이다. 테스트에서는 이 스위치를 명시적으로 켜며,
	// 기본 거부 동작은 notify 패키지의 ssrf_test.go에서 검증한다.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	// 백그라운드 발송 루프(3초 tick)를 끈다. 이 파일의 케이스들은 stepRealtime·
	// stepDigest 를 직접 호출해 한 번의 분배 결과(앞 K건 송달·나머지 보류)를 검증하는데,
	// 백그라운드 루프가 같은 채널을 동시에 처리하면 집계가 타이밍에 따라 흔들려(느린 CI
	// 에서 간헐 실패) 결정성이 깨진다. 이 변수는 trafficEvidenceServer 가 서버를 세우기
	// 전에 설정돼야 효과가 있다.
	t.Setenv(notifyBackgroundDisabledEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// 공유 장치 trafficEvidenceServer가 만든 task에서는 exploration id를 얻을 수 없으므로 task를 직접 만든다.
	// 취약점을 기록하려면 이 id가 필요하다.
	task, err := s.m.CreateTask("通知推送测试", "推送行为验证", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// fixture 생성 전에 존재한 이벤트를 일괄 분배 완료 처리해 장치가 자체적으로 완결되게 한다.
	//
	// 필요한 이유: FanOutPendingEvents는 **전역**으로 DB의 모든 미분배 이벤트를
	// 일치하는 모든 채널에 분배한다. 공유 장치 trafficEvidenceServer 자체도 취약점 하나를 기록하며
	// (반환하는 초기 finding), 다른 테스트의 잔여 데이터도 있을 수 있다. 격리하지 않으면,
	// 이런 외부 이벤트가 현재 테스트 채널로 분배되어 N건의 발송 항목을 기대하는 검증이
	// 간헐적으로 실패한다. 실패 여부가 테스트 실행 순서에 의존해 항상 실패하는 경우보다 원인 파악이 어렵다.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("알림 이벤트 정리 실패: %v", err)
		}
	})
	// 다른 테스트가 껐을 수 있으므로 전역 스위치가 켜져 있어야 한다.
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record는 실제 증거 기록 경로로 취약점 하나를 저장하고 finding id를 반환한다.
// 이 경로는 **동일 트랜잭션**에서 알림 이벤트를 등록하며, 이것이 이 기능의 연결 지점이다.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 的摘要",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("취약점 기록 실패: %v", err)
	}
	return out.FindingID
}

// channel은 채널 설정을 읽는다(채널별 stepX 호출에 사용).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("채널 읽기 실패: %v", err)
	}
	return ch
}

// deliver는 이벤트를 분배하고 지정한 채널에만 발송 한 회차를 실행한다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel은 HTTP API로 채널을 생성하여 API 자체의 검증 경로도 함께 확인한다.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("채널 생성 실패 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("채널 생성 반환값 이상: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook은 수신한 요청 본문을 기록하는 모의 수신기다.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("모의 수신기는 요청 %d건만 받아 %d번째를 가져올 수 없음", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("모의 수신기가 요청을 받지 못함")
	}
	return f.body(t, f.count()-1)
}

// markdownText는 요청에서 본문을 추출하며 서비스별 필드 이름 차이를 지원한다.
// DingTalk markdown과 ActionCard는 `text`, WeCom markdown은 `content`를 사용한다.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("요청에서 인식 가능한 본문을 찾지 못함: %v", body)
	return ""
}

// agePendingBatch는 채널의 발송 대기 항목을 오래된 것으로 바꿔 요약 배치 만료를 테스트한다.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "实时推送",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL注入", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("메시지 1건을 발송해야 함, 실제 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	// "SQL注入"은 입력한 취약점 제목이므로 카드에 그대로 표시된다(사용자 데이터,
	// 번역 대상 아님). "높음"·"개요" 는 렌더 라벨이다 — 심각도는 notify.SeverityLabel("high")
	// 가 내는 "🟠 높음", 요약 머리글은 writeItem 이 붙이는 "**개요**:" 에 각각 들어 있다.
	for _, want := range []string{"SQL注入", "높음", "개요"} {
		if !strings.Contains(text, want) {
			t.Fatalf("메시지 본문에 %q 없음:\n%s", want, text)
		}
	}
	// 발송 상태는 sent로 전이되어야 한다.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("발송 후에도 sent로 표시되지 않은 항목이 %d건 있음", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "掩码用例",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("채널 목록 조회 실패 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("API 응답에 자격 증명이 노출됨: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("새로 생성한 채널이 목록에 없음")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("자격 증명 필드는 마스킹된 값이어야 함: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API는 자격 증명인 필드를 프런트엔드에 알려야 함")
	}

	// PATCH로 이름만 바꾸며 마스킹된 자격 증명을 다시 보낼 때 실제 자격 증명은 그대로 유지해야 한다.
	body, _ := json.Marshal(map[string]any{
		"name":   "改名后",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("갱신 실패 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("다시 보낸 마스킹 값이 실제 자격 증명을 덮어씀: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("다시 보낸 마스킹 값이 secret을 덮어씀: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "改名后" {
		t.Fatal("이름이 갱신되지 않음")
	}

	// secret의 명시적 비우기는 반영되어야 한다(마스킹 값 재전송은 기존 값 유지라는 의미와 구별).
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret 비우기 실패 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("빈 문자열은 secret을 비워야 함")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		// wantSub 는 server/notify_api.go 가 내보내는 한국어 검증 오류와 일치시킨다
		// (notifyErrKindInvalidFmt·notifyErrNameMissing·notifyErrModeInvalid 의 안정 문구).
		// webhook 두 건은 notify/dingtalk.go 의 Validate 가 내는 오류인데, 그 webhook 주소
		// 검증 문구는 이미 한국어로 현지화됐으므로(notify/dingtalk_feishu_wecom_localized_test.go
		// 가 "Webhook 주소가 없습니다"·"Webhook 주소가 올바르지 않습니다" 로 검증한다) 기대
		// 문자열도 한국어 안정 문구로 맞춘다.
		{"잘못된 유형", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "채널 유형이 올바르지 않습니다"},
		{"이름 누락", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름을 입력하세요"},
		{"webhook 누락", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook 주소가 없습니다"},
		{"잘못된 webhook 프로토콜", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook 주소가 올바르지 않습니다"},
		{"잘못된 모드", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "발송 모드가 올바르지 않습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400을 반환해야 함, 실제 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("오류 메시지에 %q가 포함되어야 함, 실제 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("존재하지 않는 채널 삭제는 404여야 함, 실제 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "仅严重",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "低危问题", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("임계값보다 낮은 취약점에 발송 항목이 생성되면 안 됨, 실제 %d건", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("필터링된 취약점은 메시지를 발송하면 안 됨")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "汇总推送",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("汇总漏洞%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 만료 전에는 발송하지 않는다.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("요약 배치가 만료되기 전에 발송됨")
	}

	// 배치를 오래된 것으로 바꾼 후에는 3건을 메시지 하나로 합친다.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("3건을 메시지 하나로 요약해야 함, 실제 발송 %d건", got)
	}
	text := markdownText(t, hook.last(t))
	// 시간창이 있는 다이제스트 머리말은 markdown.go 가 "**최근 N분간 신규 취약점 N건**"
	// 으로 렌더한다(digestInterval 기본 30분이라 WindowMinutes>0).
	if !strings.Contains(text, "최근") || !strings.Contains(text, "신규 취약점 3건") {
		t.Fatalf("요약 메시지에 건수/시간 범위 문구 없음:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("汇总漏洞%d", i)) {
			t.Fatalf("요약 메시지에 %d번째 항목 없음:\n%s", i, text)
		}
	}
	// 동일 배치는 batch_id를 공유해야 한다.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("발송 3건은 하나의 batch_id를 공유해야 함, 실제 distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "停用渠道",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "停用期间的漏洞", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("비활성 채널에 발송 항목이 생성되면 안 됨, 실제 %d건", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状态变更订阅",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "状态变更用例", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 2건이어야 한다. fixed는 상태 변경이며 finding_created도 같은 회차에 발송될 수 있다.
	// 상태 변경 이벤트는 실제로 더 늦게 생성되지만 순서에 의존하지 않고 전체를 검색한다.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		// 상태 변경 카드는 markdown.go 가 "**상태 변경**: %s → %s" 로, 상태값은
		// notify.StatusLabel 이 렌더한다(fixed → "수정됨").
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("상태 변경 → 수정됨을 포함하는 메시지를 받지 못함(총 %d건)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "不订阅状态变更",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "不订阅变更", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("상태 변경을 구독하지 않은 채널에 상태 변경 발송 항목이 전달되면 안 됨, 실제 %d건", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "测试发送",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("테스트 발송 실패 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("모의 수신기가 테스트 메시지 1건을 받아야 함, 실제 %d", hook.count())
	}
	// 테스트 메시지는 한눈에 테스트임을 알 수 있어야 하며 실제 취약점으로 오인하면 안 된다.
	// 테스트 메시지 제목은 notify_api.go 의 notifyTestName("테스트 메시지 · 채널 설정 정상").
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("테스트 메시지에 테스트임을 명시해야 함: %s", text)
	}
	// 설정이 잘못되면 채널의 원래 오류를 사용자에게 그대로 반환해야 한다.
	badID := f.createChannel(t, map[string]any{
		"name":   "坏地址",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("발송 실패는 502를 반환해야 함, 실제 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소를 지정해 failed 발송 항목을 만든다.
	chID := f.createChannel(t, map[string]any{
		"name":   "失败重试",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "会失败的推送", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 예산이 소진될 때까지 연속 발송한다.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("재시도 소진 후 failed여야 함, 실제 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("이력 조회 실패 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("실패 발송 항목 1건을 조회해야 함, 실제 total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("사용자가 원인을 조사할 수 있도록 이력에 실패 사유를 포함해야 함")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("시도 횟수를 기록해야 함, 실제 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "会失败的推送" {
		t.Fatalf("이력에 취약점 제목을 포함해야 함, 실제 %q", hist.Deliveries[0].Title)
	}

	// 수동 재발송은 pending으로 돌아가고 횟수를 0으로 초기화해야 한다.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("재발송 실패 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("재발송 후 pending 및 attempts=0이어야 함, 실제 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 실패: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta에 전체 %d개 채널을 나열해야 함, 실제 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("채널 %s가 자격 증명 필드를 보고하지 않음", k.Kind)
		}
	}

	// 전역 설정 3개의 저장/조회 왕복. 상세 링크에 "//function/..."이 생기지 않도록 끝 슬래시를 정규화해야 한다.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("설정 저장 실패 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("상세 링크 주소가 정규화되지 않음: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("요약 주기가 반영되지 않음: %v", payload["notify_digest_interval_min"])
	}

	// 유효하지 않은 값은 거부해야 한다.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s는 400을 반환해야 함, 실제 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL은 링크 조합을 검증한다. public_base_url을 설정하면
// 개별 메시지는 버튼을 포함한 ActionCard를 사용하고 취약점 상세 페이지로 연결해야 한다.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "回链",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "带回链的漏洞", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("상세 링크가 있으면 ActionCard를 사용해야 함, 실제 msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("상세 링크가 잘못됨\n기대값 %s\n실제값 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL은 반대 경우를 검증한다. 외부 주소를 설정하지 않으면 잘못된 링크를
// 생성하지 않고(localhost나 상대 경로 등), 일반 markdown으로 대체해야 한다.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "无回链",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "无回链的漏洞", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("외부 주소 미설정 시 markdown을 발송해야 함, 실제 %v", body["msgtype"])
	}
	// 상세 링크 머리글은 markdown.go 가 "[상세 보기](URL)" 로 렌더한다 — 외부 주소가
	// 없으면 이 링크가 아예 나오지 않아야 한다(과거 중국어 상세 보기 라벨을 검사하면 라벨이
	// 한국어로 바뀐 지금은 항상 통과해 회귀를 못 잡는다).
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("외부 주소 미설정 시 상세 링크가 나타나면 안 됨:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder는 알림 없이 유실되던 문제 수정의 종단 간 증거다.
//
// 요약 메시지는 채널 길이 상한(WeCom 4096바이트)을 따르며 한 배치가 들어가지 않으면 **항목 단위로 온전히** 나눠야 한다.
// 이번 메시지에 포함된 항목만 전달 완료로 표시하고 나머지는 큐로 돌려 다음 메시지를 기다린다. 과거 구현은 전체 배치를
// 성공으로 표시했다. 잘린 항목은 메시지에도 실패 목록에도 없고 발송 이력에는 성공으로 표시되어,
// 취약점 알림이 그대로 사라졌다.
//
// 4가지를 검증한다: ① 실제 포함된 건수만 완료 표시 ② 나머지는 발송 대기 유지 ③ 연기한 항목은
// **재시도 횟수를 소진하지 않음** ④ 다시 실행하면 나머지를 발송할 수 있음(영구 정체 없음).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom을 사용한다. markdown 상한이 4096바이트로 6개 채널 중 가장 작다.
	chID := f.createChannel(t, map[string]any{
		"name":   "分段汇总",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 60건이 4096바이트를 크게 넘고 반드시 분할되도록 제목을 길게 만든다.
	longName := strings.Repeat("超长漏洞名称", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("메시지 하나만 발송해야 함, 실제 %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("전달 완료로 표시된 항목이 있어야 함")
	}
	if pending == 0 {
		t.Fatalf("배치 %d건이 4096바이트에 전부 들어갈 수 없으므로 발송 대기 항목이 남아야 함; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("항목 수 불일치: sent=%d pending=%d total=%d(전달도 대기도 아닌 항목은 유실)", sent, pending, total)
	}
	// 메시지 본문은 이번 메시지에 포함되지 않은 항목 수를 정확히 알려야 한다.
	// 분절 안내는 markdown.go 가 "(이 메시지에는 앞 N건만 … 나머지 N건은 다음 메시지에서 …)" 로 렌더한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("이번 메시지에 포함되지 않은 항목이 남았음을 안내해야 함:\n%.400s", text)
	}

	// 연기한 항목은 재시도 예산을 소진하면 안 된다. 선점 시 attempts를 미리 +1 했으므로 연기할 때 다시 빼야 한다.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("연기한 항목이 재시도 횟수를 소진하면 안 됨(몇 차례 후 실패로 판단될 수 있음), 실제 attempts=%d", maxAttempts)
	}

	// 수렴할 때까지 반복한다. **최종적으로 모두 전달되며** 실제로 여러 회차로 분할되었는지 검증한다.
	// 두 번째 회차에 끝난다는 검증보다 강하다. 분할이 정체되거나 나머지 항목을 유실하지 않음을 입증한다.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("분할 발송이 수렴하지 않음: %d회 실행 후에도 %d건 미완료", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d번째 회차에 진전이 없어 나머지 %d건이 영구 정체됨", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096바이트 메시지 하나에 긴 제목의 취약점 %d건이 들어갈 수 없어 여러 회차 발송이 필요함, 실제 %d회", total, rounds)
	}
	// 첫 회차 이후의 모든 회차는 **이어서 발송만** 해야 하며 채널이 거부한 항목은 없어야 한다.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("모의 수신기가 항상 성공을 반환하므로 실패 항목이 없어야 함, 실제 %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget는 값이 서로 어긋나지 않도록 검증한다.
//
// 재시도 예산(db.MaxNotifyAttempts)과 백오프 시퀀스(notifyBackoff)는 서로 다른 패키지에 있다.
// 전자는 상태 머신 정책, 후자는 엔진 실행 간격이다. 하나만 바꾸면, 예를 들어 예산을 5회로 올리고
// 백오프 단계를 추가하지 않으면 오류 없이 4, 5번째 재시도가 마지막 단계의 간격을 반복 사용한다.
// 재시도 속도가 이유 없이 느려진 것처럼 보여 원인 조사 시 이 부분을 떠올리기 어렵다.
// 둘의 길이가 같음을 검증해 CI에서 불일치를 발견한다.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("백오프 단계 수(%d)와 최대 시도 횟수(%d)가 다름. 하나를 바꾸면 다른 하나도 함께 바꿔야 함",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 백오프 간격은 단조 비감소여야 한다. 그렇지 않으면 재시도가 점점 빨라져 속도 제한을 악화한다.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("백오프 간격은 단조 비감소여야 함: %d단계 %v < %d단계 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget는 토큰을 먼저 가져온 뒤 선점하는 순서를 고정한다.
// 순서가 뒤바뀌어 먼저 선점한 뒤 포기하면 속도 제한으로 막힌 발송도 attempts가 한 번 증가하여,
// 단순 대기만으로 예산이 소진되고 결국 failed가 된다.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷 자체만 테스트하므로 Server가 필요 없으며 이를 위해 만들지도 않아야 한다.
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1건: 버킷이 가득 차도 최대 1건이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("분당 1건인 가득 찬 버킷에서 토큰 1개를 가져와야 함, 실제 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("토큰 소진 후 즉시 0을 반환해야 함, 실제 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("반 주기에는 토큰 하나가 완전히 충전되면 안 됨, 실제 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("한 주기가 지나면 토큰 1개가 충전되어야 함, 실제 %d", got)
	}
	// 제한 없는 채널에도 유한한 상한을 적용해 무한한 대기 항목으로 한 회차가 지연되지 않게 한다.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("제한 없음은 회차당 상한 %d를 반환해야 함, 실제 %d", notifyUnlimitedBurstPerTick, got)
	}
	// 채널별 토큰 버킷은 서로 독립적이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("채널 1의 버킷은 여전히 비어 있어야 함, 실제 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens는 want개만 가져온다는 의미를 고정한다.
//
// 과거 구현은 버킷을 전부 비운 뒤 호출자가 잘랐으므로 rate=100/min 채널에 토큰이 가득 차도
// 한 회차에 5개만 사용하면 나머지 95개를 버렸다. 이번 회차에 발송 대기가 없어도 동일하게 차감했다.
// 그 결과 대기 항목이 있으면 rate_per_min건을 한 번에 처리할 수 있다는 주석의 설명이 어떤 경우에도 성립하지 않았다.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 버킷은 처음에 가득 차 있고(100), 이번 회차에는 5개만 필요하다.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5이면 정확히 토큰 5개를 가져와야 함, 실제 %d", got)
	}
	// 핵심 검증: 나머지 95개는 비워서 버리지 말고 버킷에 그대로 남아 있어야 한다.
	// 시간을 진행하지 않아 새 충전이 아니라 기존 토큰만 가져올 수 있도록 한다.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("나머지 토큰을 계속 사용할 수 있어야 함(기대 95), 실제 %d. 버킷을 한 회차에 전부 비움", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("버킷을 모두 사용했으므로 0을 반환해야 함, 실제 %d", got)
	}
	// want<=0이면 토큰을 차감하면 안 된다(빈 회차에는 비용 없음).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0이면 0을 반환해야 함, 실제 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 호출은 토큰을 소진하면 안 되므로 20개를 모두 가져올 수 있어야 함, 실제 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget는 요약 모드의 서로 다른 두 단위를 고정한다.
//
// 요약 배치 크기가 회차당 요청 예산에 연결되면 rate_per_min=20 채널은 매
// 3초 tick마다 토큰 1개만 충전하므로 요약 메시지당 취약점 1개만 포함한다. 기능상 요약이 사라진 셈인데,
// 메시지 머리말에는 여전히 최근 30분에 취약점 1개가 추가되었다고 적힌다. 이 퇴행은 오류를 발생시키지 않으며,
// 기존 종단 간 테스트도 감지하지 못한다(stepDigest에 충분히 큰 limit을 직접 전달하여,
// step 내부 할당량 계산을 우회하기 때문). 따라서 여기서는 결정 자체를 직접 검증한다.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 배치 하나 = 메시지 하나 = 요청 하나 = 토큰 하나. 토큰의 단위는 취약점이 아니라 메시지다.
	if tokens != 1 {
		t.Fatalf("요약 배치 하나는 메시지 하나만 발송하므로 토큰을 정확히 1개 소진해야 함, 실제 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("요약 배치 크기는 메모리 상한 db.MaxDigestBatchSize=%d여야 함, 실제 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 핵심 관계: 배치 크기는 회차당 요청 예산보다 훨씬 커야 한다. 둘의 크기가 비슷해지면,
	// 메시지 발송 개수와 한 배치에 담는 취약점 개수를 다시 하나의 수치로 혼동한 것이다.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("요약 배치 크기 %d가 회차당 요청 예산 %d로 제한되면 안 됨. "+
			"요청 예산은 임대에서 역산한 요청 횟수이며 한 배치에 담는 취약점 개수와는 단위가 다름",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease는 값이 서로 어긋나지 않도록 하는 또 다른 검증이다.
//
// 채널 하나의 회차당 최대 발송 건수(notifyMaxSendsPerChannelPerTick)는 임대 시간에서 역산한다.
// 한 회차의 순차 발송 최악 소요 시간은 임대보다 짧아야 한다. 그렇지 않으면 마지막 항목을 발송하기 전에 임대가 만료되어,
// 다중 인스턴스 배포 시 다른 인스턴스가 다시 선점해 중복 발송한다. 이 상수 3개는 서로 다른 위치에 있으므로,
// 하나만 바꿔도 오류 없이 관계가 깨질 수 있어 여기서 고정한다.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("채널 하나의 한 회차 최악 소요 시간 %v가 임대 %v 이상이면 안 됨"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"이 상수 3개 중 하나를 바꾸면 나머지 2개도 함께 확인해야 함",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
