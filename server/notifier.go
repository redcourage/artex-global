package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 전역 설정 키는 settings 키·값 테이블에 저장하므로 새 테이블이 필요 없습니다.
const (
	// settingNotifyEnabled는 기본 활성인 전체 알림 스위치입니다. 유지보수 때 한 번에 멈추는 용도이며
	// 실제 기능 활성 조건은 채널을 설정했는지 여부입니다.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL은 취약점 상세로 돌아가는 외부 접근 주소입니다.
	// 예: https://artex.example.com. 빈 값이면 메시지에 링크 버튼을 넣지 않습니다.
	// 재사용할 외부 주소 설정이 없어서 별도로 추가했습니다.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes는 요약 주기(분)입니다.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick은 전송 엔진의 폴링 간격이며 3초는 실시간성 상한이자
	// 취약점 저장부터 IM 도착까지 걸리는 주요 지연입니다.
	notifyTick = 3 * time.Second
	// notifyBackgroundDisabledEnv가 설정되면 서버가 백그라운드 전송 루프를
	// (newNotifier(s).Run)를 띄우지 않는다. 통합 테스트는 stepRealtime·stepDigest 를
	// 직접 호출해 분배 결과를 결정론적으로 검증하는데, 3 초 주기의 백그라운드 루프가
	// 같은 채널을 동시에 처리하면 집계가 타이밍에 따라 흔들린다. 이를 막기 위한
	// 테스트 전용 스위치이며, 변수가 없으면 평소대로 기동한다(프로덕션 동작 불변).
	notifyBackgroundDisabledEnv = "ARTEX_NOTIFY_BACKGROUND_DISABLED"
	// notifyLease는 전송 획득 임대 시간이며 단일 최악 전송 시간보다 충분히 길어야 합니다.
	// notify HTTP 클라이언트의 시간 제한은 15초이며 임대가 짧으면 두
	// dispatcher가 같은 행을 동시에 전송할 수 있습니다.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick은 매 주기 분배 이벤트 수를 제한하여 채널을 처음 켤 때
	// 이전 적체 전체가 한꺼번에 전송 작업으로 펼쳐지지 않게 합니다.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes는 기본 요약 주기입니다.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick은 속도 제한이 없는 채널의 주기당 전송 상한입니다.
	// 무제한 채널에서 한 번에 수천 취약점이 발견되어
	// 한 주기가 오래 막히지 않도록 합니다.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick은 채널 하나의 주기당 최대 전송 수입니다.
	//
	// 이 상한은 임대 시간에서 역산합니다. 획득 시 notifyLease=3분을 부여하므로
	// 한 주기의 순차 전송 최악 시간이 이를 넘으면 뒤쪽 전송이 끝나기 전에 임대가 만료됩니다.
	// Run은 단일 goroutine이고 tick이 재진입하지 않아 한 프로세스에서는 괜찮지만
	// 같은 DB를 쓰는 다른 프로세스가 만료 행을 다시 가져가 중복 전송하고
	// attempts도 두 번 늘려 원래 프로세스가 보내는 중에 실패 처리할 수 있습니다.
	//
	// 3분 임대/30초 단일 제한=6은 여유가 전혀 없어 사용할 수 없습니다.
	// 5개로 제한하면 최악 150초에 30초 여유가 남습니다. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease가 검증하므로 notifyLease,
	// notifySendTimeout, 이 상한 중 하나를 바꾸면 해당 단언을 다시 확인해야 합니다.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout은 단일 전송 시간 제한이며 앞의 상한도 결정합니다.
	// 두 값의 곱은 notifyLease보다 작아야 합니다. TestNotifyTickBudgetFitsWithinLease 참고.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff는 실패 재시도 백오프 배열이며 인덱스는 이미 시도한 횟수입니다.
// 최초 포함 3회는 db.MaxNotifyAttempts와 대응하며 함께 변경해야 합니다.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// 전달 실패·지연 사유. notification_deliveries.last_error 에 저장되어
// notify_api.go 의 전달 이력 응답을 거쳐 알림 페이지 「전달 기록」 표(delivery-list.tsx)에
// 그대로 표시된다. 따라서 사용자 노출 문구라 한국어로 둔다(진단 로그도 한국어).
const (
	errDeliveryChannelKindUnregistered = "등록되지 않은 채널 유형입니다: %q"
	errDeliverySnapshotUnrenderable    = "이벤트 스냅샷을 해석하지 못해 이 취약점을 메시지로 만들지 못했습니다"
	errDeliveryChannelLengthCapped     = "이 메시지가 채널 길이 상한에 도달해 앞의 %d건만 전송했고, 나머지는 다음 배치로 미룹니다"
	errDeliveryNoDeliveredCount        = "채널이 전송 건수를 보고하지 않았습니다(delivered=%d)"
	errDeliveryRetryExhausted          = "%d회 재시도 후에도 실패했습니다: %s"
	errDeliverySnapshotEmpty           = "전달 항목 #%d 의 이벤트 스냅샷이 비어 있습니다"
	errDeliverySnapshotParse           = "전달 항목 #%d 의 이벤트 스냅샷을 해석하지 못했습니다: %w"
	errDeliveryBatchAllUnparseable     = "요약 배치의 전달 %d건을 모두 해석하지 못했습니다"
)

// Notifier는 취약점 알림 전송 엔진입니다.
//
// Scheduler와 나란히 독립 goroutine으로 실행합니다(server.New 참고). 알림은 3초 실시간성이 필요해
// 업무 트리거와 주기가 다르므로 Scheduler의 tick을 재사용하지 않으며
// 알림 지연이 agent 트리거에 영향을 주지 않도록 실패도 분리합니다.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu는 buckets를 보호합니다. 채널 수와 경쟁이 적어 단일 뮤텍스로 충분하며
	// 더 세밀한 구조를 도입할 필요가 없습니다.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket은 채널별 토큰 버킷입니다.
//
// 분마다 카운터를 초기화하는 방식 대신 토큰 버킷을 사용해 경계에서
// 20개를 보낸 직후 다음 분에 또 20개를 보내 1초에 40개가 되는 상황을 피합니다.
// 토큰 버킷은 일정 속도로 보충하므로 이런 급증과 플랫폼 속도 제한을 방지합니다.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run은 ctx 종료까지 반복하며 server.New에서 한 번 시작합니다.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step은 한 주기를 실행해 새 이벤트를 분배한 뒤 기한이 된 작업을 전송합니다.
//
// 단계 실패는 로그만 남기고 루프를 중단하지 않습니다. 알림 오류가 프로세스 전체 문제가 되어서는 안 됩니다.
// tick마다 독립 실행하므로 다음 주기에 자연스럽게 다시 시도합니다.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 이벤트 분배 실패: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 채널 조회 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 버킷의 단위는 취약점 수가 아니라 메시지 수, 즉 HTTP 요청 수입니다.
		// 실시간 모드는 취약점 하나가 메시지 하나지만 요약 모드는 전체 배치가
		// 메시지 하나이므로 토큰 하나만 소비합니다.
		//
		// 두 모드 모두 먼저 토큰 버킷을 확인한 뒤 허용량만큼 획득해야
		// 속도 제한으로 전송하지 못한 항목이 재시도 횟수를 소비하지 않습니다.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan은 요약 채널의 이번 주기 토큰 소비량과 배치 크기 상한을 반환합니다.
//
// 두 값은 서로 다른 단위이므로 별도 함수로 분리했습니다.
//
// - tokens는 메시지 수이며 배치 하나가 메시지/HTTP 요청 하나가 되므로 항상 1입니다.
// 따라서 rate_per_min은 요약 메시지의 분당 상한으로 계속 적용됩니다.
// - claimLimit는 배치의 최대 취약점 수이며 요청 예산과 무관하게 메모리 상한만 따릅니다.
//
// 과거에는 digest에 rate_per_min을 적용하려고 주기별 요청 예산인
// notifyMaxSendsPerChannelPerTick(임대에서 역산)을 배치 크기로 전달했습니다.
// 그 결과 rate_per_min=20에서 3초마다 토큰 하나가 보충되어
// 요약 메시지에 취약점 하나만 들어가 사실상 요약 문구만 붙은 실시간 알림이 되었습니다.
// 사용자는 최근 30분에 취약점 하나가 추가되었다는 메시지를 반복해서 받고 db.MaxDigestBatchSize는 쓰이지 않았습니다.
//
// 기존 종단 테스트는 큰 limit를 stepDigest에 직접 전달하여
// step의 예산 계산을 건너뛰므로 이 문제를 찾기 어려웠습니다. 결정을 여기로 모아
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget에서 직접 검증합니다.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime은 채널의 실시간 작업을 획득하고 취약점마다 메시지 하나를 전송합니다.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전송 획득 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf(trMsg(errDeliveryChannelKindUnregistered), ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더링 실패는 로컬 데이터 문제라 재시도해도 나아지지 않습니다.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest는 배치 기한이 되면 채널의 대기 전송을 메시지 하나로 합쳐 보냅니다.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 요약 배치 판정 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 요약 배치 획득 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf(trMsg(errDeliveryChannelKindUnregistered), ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 스냅샷이 잘못되어 메시지에 포함되지 못한 항목은 명시적으로 실패 처리해야 합니다. 그렇지 않으면
	// included에도 실패 목록에도 없어 성공 후 일괄 상태 변경에서 누락되고
	// sending에 남아 임대가 만료될 때마다 반복 획득됩니다.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := errDeliverySnapshotUnrenderable
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 잘못된 스냅샷 전송 표시 실패 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅샷을 해석할 수 없는 전송 %d개 건너뜀 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 포함된 항목만 send에 넘깁니다. included[i]와 msg.Items[i]는 정확히 대응하며
	// send는 채널이 앞의 K개를 담았다는 응답을 이 대응 관계로 실제 전송 행에 반영합니다.
	n.send(ctx, channel, cfg, msg, included)
}

// send는 전송하고 결과에 따라 상태를 전환합니다.
//
// 같은 배치의 항목은 하나의 전송 결과를 공유해 전달 완료 또는 배치 전체 재시도로 처리합니다.
// 요약 메시지 일부만 재시도하면 배치 의미가 달라지므로 개별 재시도하지 않습니다.
//
// 예외는 채널 길이 제한에 따른 분할입니다. 앞의 K개만 담았다는 응답이면
// K+1번째부터는 성공 처리하지 않고 다음 배치로 넘겨야 합니다. 그렇지 않으면 잘린
// 취약점이 메시지에도 실패 목록에도 남지 않고 사라집니다.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 단일 전송 시간을 제한해 한 채널의 지연이 이번 주기의 나머지 채널까지 막지 않게 합니다.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 채널이 보고한 수가 전송 수를 넘으면 렌더링 계산 오류입니다.
			// 기록을 깨뜨리지 않도록 모두 전달된 것으로 처리하고 문제를 로그에 남깁니다.
			log.Printf("[notify] 채널 보고 전달 수 %d이(가) 전송 수 %d보다 많음 channel=%s, 모두 전달된 것으로 처리",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 전달 완료 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 채널 길이 상한에 도달했으므로 나머지는 즉시 큐로 돌려 다음 tick에 이어 보냅니다.
			// 실패가 아니므로 RescheduleDeliveries 대신 DeferDeliveries를 사용하여
			// 재시도 예산을 소모하지 않고 획득 때 증가한 시도 수를 되돌립니다.
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf(trMsg(errDeliveryChannelLengthCapped), delivered)); err != nil {
				log.Printf("[notify] 분할 후속 전송 대기열 등록 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 채널이 오류도 전달 수량도 반환하지 않으면 백오프하는 실패로 처리해
		// 상태를 끝내지 못한 채 반복 획득되는 일을 막습니다.
		err = fmt.Errorf(trMsg(errDeliveryNoDeliveredCount), delivered)
	}

	// 실패 처리는 배치 최대 시도 수가 아닌 항목별로 결정합니다.
	//
	// 이전에는 maxAttempts(deliveries) >= MaxNotifyAttempts이면 배치 전체를 실패 처리했지만
	// 항목별 시도 수는 다릅니다. 이미 두 번 시도한 이전 항목이
	// 한 번만 시도한 새 항목까지 failed로 보내 재시도 기회 없이 알림을 잃게 할 수 있습니다.
	// 이는 이전 행이 새 행에 영향을 주지 않도록 한 의도와 반대입니다.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf(trMsg(errDeliveryRetryExhausted), db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 백오프 세 단계별로 묶어 일정을 다시 설정합니다. 항목별 UPDATE를 하면
	// 500개 배치에서 왕복 500회가 생기므로 적은 수의 그룹으로 처리합니다.
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 전송 일정 재설정 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전송 실패 channel=%d kind=%s 영구 실패=%d 재시도 소진=%d 재시도 대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries는 포인터 동일성으로 all에서 keep에 없는 항목을 반환합니다.
// 메시지에 들어가지 못한 항목을 찾아 명시적으로 처리하고 미결 상태로 남기지 않습니다.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt는 채널 구현을 가져와 설정을 해석합니다.
// ok=false는 미등록 유형이며 무한 재시도 대신 즉시 실패 처리해야 합니다.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 해석 실패 시 빈 map을 전달하여 채널 Validate가 누락 필드를 알려주도록 합니다.
		// 이 오류가 JSON 해석 오류보다 사용자에게 수정 방법을 잘 알려줍니다.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle은 취약점 메시지 하나를 렌더링합니다.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch는 요약 메시지를 렌더링하며 스냅샷을 개별 해석해 잘못된 항목만 건너뜁니다.
// 잘못된 하나 때문에 배치 전체가 사라지지 않도록 합니다.
//
// included와 msg.Items는 엄격하게 일대일로 대응해야 합니다(i번째 전송 ↔ i번째 항목).
// 호출자는 채널의 앞 K개 포함 응답으로 앞 K개 전송을 전달 완료 처리하므로
// 잘못된 스냅샷을 메시지에서 제외할 때 included에서도 함께 제외해야 합니다.
// 그렇지 않으면 인덱스가 어긋나 잘못된 항목이 전달 완료되고 정상 항목이 미전달로 분류됩니다.
// 제외한 잘못된 항목은 stepDigest 호출자가 명시적으로 실패 처리합니다.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 잘못된 스냅샷은 메시지와 included 양쪽에서 제외하며 호출자가
			// 명시적으로 실패 처리합니다. 전달 완료 항목에 섞지 않습니다.
			log.Printf("[notify] 요약 배치에서 해석할 수 없는 스냅샷 건너뜀 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf(trMsg(errDeliveryBatchAllUnparseable), len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor는 이벤트 스냅샷을 알림 항목으로 렌더링하고 자산 이름과 상세 링크도 해석합니다.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산 이름 조회 실패는 알림을 막지 않습니다. 이름을 못 보는 것보다 알림 누락이 더 나쁘며
		// 메시지에서 자산 줄 하나만 빠집니다.
		log.Printf("[notify] 자산 이름 해석 실패 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 상세 경로는 web/src/app/(main)/function/findings/detail/page.tsx에 있으며
		// 쿼리 인자 id로 취약점 id를 읽습니다.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens는 채널 버킷에서 최대 want개 토큰을 가져와 실제 수량을 반환합니다.
//
// 토큰 하나는 메시지/HTTP 요청 하나입니다. 실시간 모드는 필요한 메시지 수를 전달하고
// 요약 모드는 배치 전체가 메시지 하나이므로 1을 전달합니다.
//
// 버킷 용량은 채널의 분당 상한이며 일정 속도로 보충합니다. ratePerMin<=0이면 무제한이지만
// 적체가 한 주기를 무한히 막지 않도록 유한하고 충분히 큰 값을 반환합니다.
//
// want 상한이 없으면 버킷 전체를 비우지만 호출자에게도 주기별 상한이 있어
// 초과 토큰을 쓰지 못한 채 잃게 됩니다. 그러면 쌓아 둔 순간 전송 용량도 활용하지 못하며
// 대기 전송이 없는 주기에도 토큰을 빼게 됩니다.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 실제 경과 시간에 따라 초당 ratePerMin/60으로 보충합니다.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 부동소수 누적 오차 때문에 정수 변환 전에 작은 epsilon을 더합니다. 두 번 보충한
	// 0.5 + 0.5가 0.9999999999가 되면 int()가 0으로 잘라
	// 수학적으로 찬 버킷에서도 토큰을 못 꺼냅니다. 1e-9는 토큰보다 훨씬 작아 실제 부족분은 허용하지 않습니다.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled는 전체 스위치를 읽습니다.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL은 상세 링크용 외부 주소의 끝 슬래시를 제거해 반환합니다.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval은 요약 주기를 반환하며 잘못되었거나 없으면 기본값을 사용합니다.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot은 전송 이벤트의 스냅샷을 해석합니다.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf(trMsg(errDeliverySnapshotEmpty), dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf(trMsg(errDeliverySnapshotParse), dl.ID, err)
	}
	if snap.Kind == "" {
		// 이벤트 유형은 이벤트 행을 기준으로 합니다. 스냅샷은 이전 버전에서 저장했을 수 있습니다.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
