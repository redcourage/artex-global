package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일의 테스트는 실제 PostgreSQL에 연결한다(DB가 없으면 건너뜀). SQL에는
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, 여러 행의 IN(...) 자리표시자 조합을 사용하며,
// 모두 컴파일은 되지만 실행 중 실패할 수 있으므로 실제로 실행해야 검증한 것이다.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel은 채널을 생성하고 테스트 종료 시 자동 삭제한다.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "测试渠道-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("채널 생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent는 finding을 거치지 않고 이벤트를 직접 기록해 분배와 발송을 테스트한다.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("이벤트 기록 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 세 자산 유형은 도메인, IP, URL별 표시 기준이 다르다.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 입력 순서는 의도적으로 섞었으며 존재하지 않는 id도 포함한다.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("자산 이름 해석 실패: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("자산 이름 개수 불일치, 기대값 %v 실제값 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("순서/값 불일치, 기대값 %v 실제값 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure는 저장점 메커니즘의 핵심 테스트다.
// 트랜잭션에서 항상 false인 임시 제약을 추가해 notification_events 쓰기가 반드시 실패하게 하고,
// ① 함수가 false를 반환하며 ② 트랜잭션이 aborted 상태가 되지 않아 후속 문장을 실행할 수 있음을 검증한다.
//
// 저장점이 없으면 PostgreSQL은 전체 트랜잭션을 무효화하고 이후 모든 문장이
// "current transaction is aborted"로 실패한다. 알림 테이블의 문제 하나가
// 취약점 DB 저장까지 막는 바로 그 실패 경로다.
//
// 여기서는 의도적으로 **COMMIT 대신 ROLLBACK으로 종료**한다. PG의 ALTER TABLE은 트랜잭션에 속하므로,
// 커밋하면 임시 제약이 schema에 영구적으로 남아 이후 모든 테스트를 실패시킨다.
// 롤백은 DDL을 자동으로 취소하므로 수동 정리가 필요 없다. 트랜잭션이 여전히 유효한지만 검증하며,
// 실제 커밋은 필요 없다.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 방어적 정리: 이전 실행에서 해당 제약이 남았다면 먼저 제거한다.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 임시 제약을 취소한다. 함수 주석 참조.

	// NOT VALID: 이후 기록할 행에만 제약을 적용하고 DB의 기존 이벤트는 검증하지 않는다.
	// 그렇지 않으면 기존 행의 위반으로 제약을 추가할 수 없을 수 있다.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("임시 제약 추가 실패: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("반드시 실패하는 제약 아래에서 쓰기 성공을 보고함")
	}
	// 핵심 검증: 트랜잭션을 계속 사용할 수 있다.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("트랜잭션이 오염됨(저장점 미작동): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("롤백 실패: %v", err)
	}
	// 롤백으로 DDL도 취소되어 후속 테스트에 영향을 주지 않는지 확인한다.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("롤백으로 임시 제약이 취소되지 않아 후속 테스트에 영향을 줌")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL注入"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"전체 수신 채널에 high 전달", highSQL, all.ID, true},
		{"전체 수신 채널에 low 전달", lowXSS, all.ID, true},
		{"critical 전용 채널은 high 건너뜀", highSQL, onlyCritical.ID, false},
		{"critical 전용 채널에 critical 전달", criticalXSS, onlyCritical.ID, true},
		{"SQL 전용 채널에 SQL 전달", highSQL, sqlOnly.ID, true},
		{"SQL 전용 채널은 XSS 건너뜀", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("발송 항목 존재 여부: 기대값 %v 실제값 %v", tc.want, exists)
			}
		})
	}

	// 다시 분배해도 발송 항목이 중복 생성되면 안 된다(fanned_out 멱등성).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("이미 분배한 이벤트를 다시 처리하면 안 됨, 실제 events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel은 이벤트와 일치하는 채널이 없는 경우를 검증한다.
// 이벤트가 분배 대기 집합에 영구적으로 남아 매 tick마다 재검색되지 않도록 이 경우에도 분배 완료로 표시해야 한다.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["绝不匹配的类型"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("발송 항목이 생성되면 안 됨, 실제 %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("일치하는 채널이 없는 이벤트도 분배 완료로 표시해야 무한 재검색을 방지함")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 실시간 선점은 realtime 채널의 항목만 가져오고 digest 채널은 건드리면 안 된다.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("선점 실패: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1건을 선점해야 함, 실제 %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("선점 후 sending 및 attempts=1이어야 함, 실제 state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 함께 불러오는 렌더링 컨텍스트가 완전해야 한다(채널 설정 + 이벤트 스냅샷 + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("선점 결과에 채널 설정이 없어 렌더링이 실패함")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("이벤트에서 finding id를 가져오지 못함, 실제 %d", got[0].FindingID)
	}

	// 임대가 만료되지 않았으므로 두 번째 선점 결과는 비어 있어야 한다. 동일 행을 두 dispatcher가
	// 동시에 발송하지 않는다는 보장이다.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("임대 기간 내에 중복 선점하면 안 됨, 실제 %d건", len(again))
	}

	// 실시간 선점은 digest 채널의 발송 항목에 영향을 주면 안 된다.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("실시간 선점으로 digest 채널의 발송 항목을 가져오면 안 됨, 실제 %d건", len(left))
	}
}

// TestClaimExpiredLeaseRecovers는 프로세스 비정상 종료 후 자동 복구를 검증한다. 발송 중 프로세스가 종료되면 sending
// 행이 남는데, 발송이 영구 정체되지 않도록 임대 만료 후 다시 선점할 수 있어야 한다.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("첫 선점 실패: %v (%d건)", err, len(first))
	}
	// 임대 시각을 수동으로 과거로 옮겨 임대 만료를 모의한다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("임대가 만료된 sending 행을 다시 선점할 수 있어야 함, 실제 %d건", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("다시 선점하면 시도 횟수가 누적되어야 함, 실제 %d", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 비활성화하면 기존 발송 대기 항목도 skipped로 표시한다.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("비활성 채널의 기존 발송 대기 항목은 skipped여야 함, 실제 %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("비활성 채널을 선점할 수 있으면 안 됨, 실제 %d건", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 방금 생성한 배치의 경과 시간은 0이므로 30분 주기에서는 아직 만료되지 않아야 한다.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("배치 만료 판단 실패: %v", err)
	}
	if due {
		t.Fatal("방금 생성한 배치가 즉시 만료되면 안 됨")
	}

	// 발송 3건의 생성 시각을 함께 과거로 옮겨 한 주기만큼 누적된 배치를 모의한다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("주기를 초과한 배치는 만료로 판단해야 함")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("요약 배치 선점 실패: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("요약은 3건 모두 한 번에 선점해야 함, 실제 %d건", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("요약 배치에 batch_id를 기록해야 이력에서 함께 발송된 항목임을 알 수 있음")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("동일 배치는 batch_id를 공유해야 함, 실제 %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// 이 배치 **전체**를 실패 처리해 재예약한 후 다시 선점해도 batch_id는 원래 값을 유지해야 한다(COALESCE의 역할).
	// 그렇지 않으면 한 번의 재시도로 이 배치가 함께 발송되었다는 사실이 사라진다.
	//
	// 한 건만이 아니라 전체 배치를 재예약해야 한다. 발송 엔진이 요약 메시지를 처리하는 방식과 같다.
	// 메시지 하나가 전체 배치를 나타내므로 성공/실패도 함께 적용된다. 한 건만 재예약하면 나머지는 임대 기간 중이어서
	// 다시 선점할 때 해당 한 건만 가져오게 된다.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "模拟失败"); err != nil {
		t.Fatal(err)
	}
	// 임대를 과거로 옮겨 백오프 시간이 지났음을 모의한다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("다시 선점하면 3건 모두 가져와야 함, 실제 %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("재시도 후에도 batch_id는 원래 값 %d여야 함, 실제 %v", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("선점 실패: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "网络抖动"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "网络抖动" {
		t.Fatalf("재예약 후 pending이며 사유를 기록해야 함, 실제 state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "重试耗尽"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed여야 함, 실제 %s", state)
	}

	// 수동 재발송은 재시도 횟수를 0으로 초기화하고 즉시 만료되어야 한다. 그렇지 않으면 기존 실패 예산을 물려받는다.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("재발송 실패: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("재발송 후 pending 및 attempts=0이어야 함, 실제 state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("재발송은 즉시 선점할 수 있어야 함")
	}

	// 이미 전달된 항목은 재발송할 수 없어야 한다.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("이미 전달된 항목의 재발송을 허용하면 안 됨")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "分页测试"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if total != 5 {
		t.Fatalf("총 5건이어야 함, 실제 %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("페이지당 2건이어야 함, 실제 %d", len(page1))
	}
	// 최신 항목 우선: 첫 페이지의 첫 id가 두 번째 페이지의 첫 id보다 커야 한다.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("페이지 순서는 최신 항목 우선이어야 함, 실제 page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// 목록에서 발송 내용을 표시할 수 있도록 렌더링 컨텍스트를 이력과 함께 반환해야 한다.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("이력 항목에 표시 필드가 없음: %+v", page1[0])
	}

	// 상태별 필터링: pending 항목이 없다.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending 발송 항목이 없어야 함, 실제 %d건 (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知状态变更测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "状态变更用例",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 저장 시 finding_created 이벤트 하나를 등록했으므로 먼저 개수를 세어 기준값으로 삼는다.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("취약점 저장과 동일 트랜잭션에서 알림 이벤트 하나를 등록해야 함")
	}

	// 동일 상태로 변경할 때는 이벤트를 생성하면 안 된다(중복 제출로 인한 알림 반복 방지).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("상태가 변하지 않았으면 알림 이벤트를 등록하면 안 됨")
	}
	if from != "pending" {
		t.Fatalf("변경 전 상태 pending을 반환해야 함, 실제 %q", from)
	}

	// 실제 변경: 이벤트를 등록하고 from/to를 기록해야 한다.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("상태가 실제로 변하면 알림 이벤트를 등록해야 함")
	}
	if from != "pending" {
		t.Fatalf("from은 pending이어야 함, 실제 %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("상태 변경 이벤트를 찾지 못함: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("스냅샷의 상태 전이가 잘못됨: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 상태 변경 메시지가 빈 껍데기가 되지 않도록 스냅샷에 렌더링 필드를 포함해야 한다.
	if snap.VulnClass != "SQL注入" || snap.Severity != "high" || snap.Name != "状态变更用例" {
		t.Fatalf("스냅샷에 렌더링 필드가 없음: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("상태가 fixed로 갱신되어 있어야 함, 실제 %s", status)
	}

	// 존재하지 않는 취약점: 오류 없이 found=false.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("존재하지 않는 취약점은 오류 없이 found=false를 반환해야 함, 실제 found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("통계 실패: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("채널 수가 잘못됨: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("발송 대기 항목을 집계해야 함: %+v", stats)
	}
	// 방금 생성한 발송 대기의 경과 시간은 음수나 매우 큰 값이 아니라 0에 가까워야 한다.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("발송 대기 경과 시간이 유효하지 않음: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 往返",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 往返" {
		t.Fatalf("저장 후 읽은 필드가 일치하지 않음: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("기본값은 활성화여야 함")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("설정이 올바르게 저장되지 않음: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("필터 조건이 올바르게 저장되지 않음: %+v", filter)
	}

	// 갱신 후 다시 읽는다.
	got.Name = "改名了"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "改名了" || after.IsEnabled() {
		t.Fatalf("갱신이 반영되지 않음: %+v", after)
	}

	// 삭제 후에는 조용히 성공하지 말고 존재하지 않는다는 오류를 반환해야 한다.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("기대값 ErrNotificationChannelNotFound, 실제 %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("중복 삭제는 존재하지 않는다는 오류를 반환해야 함, 실제 %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate는 과거에 잘못 구현한 부분을 고정한다.
// **0은 제한 없음을 뜻하는 유효한 설정이다. db 계층에서 미지정으로 간주해 기본값으로 덮어쓰면 안 된다.**
//
// 과거 버그: SaveNotificationChannel에 `if RatePerMin <= 0 { 기본값 사용 }`을 작성하여,
// 문서, UI 안내, takeTokens 모두 0을 제한 없음으로 해석하는데 DB 쓰기 계층만 조용히
// 20(DingTalk/WeCom/Telegram) 또는 100(Feishu)으로 바꿨다. 사용자는 제한을 해제했다고 생각하지만 실제로는 제한되었고,
// 안내도 전혀 없었다. 미지정과 명시적 0의 차이는 요청 본문에서만 표현할 수 있으므로,
// 기본값은 server 계층에서 채우고(notifyCreateChannel 참조), db 계층은 저장만 담당한다.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 명시적 0(제한 없음)은 그대로 저장해야 한다.
	unlimited := &NotificationChannel{
		Name: "不限流", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("명시적 0은 제한 없음을 뜻하므로 그대로 저장해야 함, 실제 %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("기본 모드는 realtime이어야 함, 실제 %s", got.Mode)
	}

	// 음수는 유효하지 않은 입력이므로 다른 값으로 조용히 바꾸지 말고 거부해야 한다.
	bad := &NotificationChannel{
		Name: "负限流", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("음수 속도 제한을 거부해야 함")
	}
}

// TestDeleteChannelCascadesDeliveries는 외래 키 동작을 고정한다. 채널 삭제 시 발송 이력도 사라져야 하지만
// (설정이 없어 이력을 해석할 수 없음), 다른 채널에서 참조할 수 있는 이벤트 자체는 남겨야 한다.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("사전 조건 불충족: 발송 항목이 생성되지 않음")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("채널 삭제 시 발송 항목도 연쇄 삭제되어야 함, 아직 %d건 남음", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("채널 삭제 시 이벤트 자체까지 삭제하면 안 됨")
	}
}

// TestClaimDigestBatchHonorsCallerLimit는 감사에서 지적한 허점을 검증한다.
// 이전에는 요약 채널이 토큰 버킷을 완전히 우회했다. takeTokens가 allow를 차감해도 사용하지 않아,
// digest 모드에서는 rate_per_min이 아무 효과가 없었다. 이제 limit도 제한에 참여한다.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3이면 3건만 선점하고 나머지는 DB에 남겨야 한다.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("선점 실패: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("호출자 제한 할당량에 따라 3건만 선점해야 함, 실제 %d", len(got))
	}
	// limit=0은 이번 회차 할당량 소진을 뜻한다. 오류 없이 한 건도 선점하지 않아야 한다.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("할당량이 0이면 오류 없이 0건을 선점해야 함, 실제 %d건 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange는 감사에서 지적한 완전성 누락을 검증한다.
// 재검증 결과가 수정됨이면 상태는 실제로 바뀌지만 해당 UPDATE가 DB에 직접 기록하며
// 알림을 포함한 경로를 우회했다. 그 결과 on_status_change 채널은 이런 상태 전이에 대해
// 알림을 전혀 받지 못하고 UI 상태만 조용히 바뀌어 운영자가 플랫폼을 열어야 알 수 있었다.
//
// 이 테스트는 모든 상태 변경 경로가 상태 변경 이벤트를 등록해야 한다는 조건을 고정한다.
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("复测推送测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "复测目标",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 재검증 기록을 생성하고 직접 완료 상태로 전환한다.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "复核")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("재검증에는 세션 하나가 연결되어야 함")
	}
	// 실제 절차와 같이 재검증이 먼저 running이 되어야 결론을 기록할 수 있다.
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("재검증 시작 실패: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "已修复", "证据"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("재검증 종료 실패: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("재검증에서 수정됨으로 판단하면 상태는 fixed여야 함, 실제 %s", status)
	}

	// 핵심 검증: from/to가 올바른 상태 변경 이벤트가 하나 있어야 한다.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("재검증에서 수정됨으로 판단하면 상태 변경 알림 이벤트를 등록해야 함(on_status_change 채널 수신에 필요): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("스냅샷의 상태 전이가 잘못됨: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 알림이 빈 껍데기가 되지 않도록 스냅샷에 렌더링 필드를 포함해야 한다.
	if snap.Name != "复测目标" || snap.Severity != "high" {
		t.Fatalf("스냅샷에 렌더링 필드가 없음: %+v", snap)
	}
}
