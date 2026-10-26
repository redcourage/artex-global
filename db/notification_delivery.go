package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전송 작업 획득과 상태 전이를 담당합니다.
// 긴 트랜잭션 대신 임대를 사용합니다. 행을 sending으로 바꾸고 next_attempt_at을 미래의 임대 만료 시각으로 설정한 뒤 커밋하고 네트워크 전송을 수행합니다.
// 따라서 최대 15초 걸릴 수 있는 HTTP 요청 중 행 잠금으로 다른 쓰기를 막지 않습니다.
// 전송 중 프로세스가 중단되면 sending에 머물지만 임대 만료 후 state IN ('pending','sending') 조건으로 다시 획득하여 복구합니다.
// 획득 시 attempts를 늘리므로 중단이 반복되어도 무한 재시도하지 않으며 MaxNotifyAttempts에 도달하면 failed로 전환해 수동 처리를 기다립니다.
// MaxNotifyAttempts는 최초 시도를 포함한 최대 전송 횟수입니다. 엔진이 아닌 상태 머신의 정책이므로 여기에 정의합니다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize는 요약 배치에 합칠 최대 전송 수입니다.
// 전체 스캔 한 번으로도 수만 개의 취약점이 생길 수 있습니다. 상한이 없으면 모든 행을 메모리에 읽고 긴 메시지로 렌더링한 뒤 채널 길이 제한으로 잘려 메모리를 낭비하고 취약점 알림을 조용히 잃습니다.
// 초과 항목은 DB에 남겨 다음 배치와 주기에 전송합니다. 500개는 WeCom의 4096바이트 제한에서 읽을 만한 내용을 유지하는 규모이며 더 늘려도 잘리는 위치만 뒤로 이동합니다.
const MaxDigestBatchSize = 500

// NotificationDelivery는 렌더링에 필요한 채널 설정과 이벤트 스냅샷을 포함하는 전송 작업입니다.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 함께 조회한 렌더링 컨텍스트. JSON에는 포함하지 않으며 서버 계층에서 DTO로 구성합니다.
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind는 이벤트에서 가져와 이력 목록에서 취약점 상세로 바로 이동할 때 사용합니다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind는 목록 표시용 중복 필드이며 프런트엔드의 추가 조회를 줄입니다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery는 전송 + 이벤트 스냅샷 + 채널 설정을 함께 조회합니다. 메시지 렌더링에는 세 정보가 모두 필요하며 따로 조회하면 왕복이 세 번 필요합니다.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery는 sel로 후보를 선택·잠금한 뒤 sending으로 바꾸고 임대를 연장합니다. sel의 lease 위치는 호출자가 $n 자리표시자와 인자로 지정합니다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries는 채널 하나에서 기한이 된 실시간 전송을 최대 limit개 획득합니다.
// 속도 제한은 엔진에서 채널별로 관리하므로 먼저 전송 가능 수를 계산하고 그만큼만 획득해야 대기로 재시도 횟수를 소모하지 않습니다. 먼저 획득한 뒤 버리면 attempts가 증가하여 3회 예산이 대기만으로 소진될 수 있습니다.
// 만료된 sending도 포함해 중단 후 복구합니다. lease는 HTTP 클라이언트의 최악 전송 시간 15초보다 충분히 길어야 dispatcher 두 개가 같은 행을 동시에 전송하지 않습니다.
// 비활성화 시 기존 전송을 skipped로 바꾸지만 비활성화와 획득의 경합을 막기 위해 여기서도 비활성 채널을 제외합니다.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue는 대기 전송이 있고 가장 오래된 항목이 요약 주기에 도달했는지 확인합니다.
// 벽시계가 아닌 가장 오래된 항목의 나이를 사용하므로 새 채널이 정각에 항목 하나짜리 요약을 보내거나 오래 쌓인 배치가 한 주기를 더 기다리지 않습니다.
// 이 함수는 전송 시점만 판단합니다. ClaimDigestBatch는 아직 주기가 지나지 않은 항목도 함께 가져와 한 주기의 요약이 여러 메시지로 나뉘지 않도록 합니다.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch는 현재 전송할 채널의 대기 항목을 최대 MaxDigestBatchSize개씩 요약 배치로 획득합니다.
// 배치의 최소 id를 안정적이고 읽기 쉬운 batch_id로 사용하므로 별도 시퀀스가 필요 없습니다. 재시도 때는 COALESCE로 원래 번호를 유지하여 함께 보낸 항목의 관계를 보존합니다.
// id 오름차순으로 오래된 항목부터 전송하여 새 취약점 때문에 이전 항목이 계속 밀리지 않도록 합니다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit는 메모리 상한입니다. 호출자가 MaxDigestBatchSize보다 큰 값을 전달하지 못하도록 다시 제한합니다.
	// 속도 제한의 단위는 메시지 수이며 배치 하나가 server.takeTokens에서 토큰 하나를 소비합니다. 이는 배치에 담을 취약점 수와 다릅니다.
	// 과거에 요청 예산을 배치 크기로 전달하여 rate=20/min 채널의 배치에 취약점 하나만 담겼고 요약이 사실상 실시간 알림이 되었습니다. 속도 제한은 이 값 대신 takeTokens의 want를 수정해야 합니다.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries는 선택, sending 전환과 임대 연장, 전체 행 조회를 하나의 트랜잭션에서 수행합니다. postClaim은 선택적 추가 단계이며 요약 배치에서 batch_id를 저장할 때 사용합니다.
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋에 성공한 뒤에는 아무 작업도 하지 않습니다.

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending으로 바꾸고 next_attempt_at을 임대 만료 시각으로 설정합니다. 임대 미만료와 재시도 시각 미도달을 같은 조건으로 표현하므로 열을 추가할 필요가 없습니다.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent는 전송 배치를 전달 완료로 표시합니다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries는 전송 배치를 pending으로 돌리고 재시도 시각을 늦춥니다. 중간 상태를 추가하지 않아 남은 기회를 MaxNotifyAttempts 한 곳에서 표현하며 재시도 정책 때문에 상태 머신 분기가 늘지 않습니다.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries는 전송 배치를 즉시 다시 획득 가능한 pending으로 돌리고 획득 시 증가한 시도 횟수를 되돌립니다.
// 채널 길이 제한으로 요약을 나눠 보낼 때 이번 메시지에 들어가지 못한 항목을 다음 배치로 넘기는 용도입니다. 실패가 아니므로 재시도 예산을 소모하지 않습니다.
// 되돌리지 않으면 500개 적체를 20개씩 25개 메시지로 보낼 때 실제 오류가 없는 뒤쪽 항목도 세 번째 시도에서 failed가 됩니다. 수동 재전송으로 attempts가 0이 되었을 때도 음수가 되지 않도록 GREATEST(...,0)을 사용합니다.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries는 전송 배치를 최종 실패로 표시하고 이력에서 수동 재전송하기를 기다립니다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 자리표시자는 $3부터 시작합니다. $1은 state, $2는 last_error입니다.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery는 전송 하나를 pending으로 돌리고 시도 횟수를 0으로 초기화해 즉시 재전송합니다. 사용자의 재전송 요청은 이전 실패 원인을 처리했다는 의미이므로 이전 시도 횟수로 제한하지 않습니다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전송 %d이(가) 없거나 현재 상태에서는 재전송할 수 없습니다", id)
	}
	return nil
}

// NotificationDeliveryFilter는 전송 이력 조회 조건입니다.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries는 최신순 전송 이력을 페이지 단위로 반환합니다.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError는 오류 메시지를 열이 허용하는 길이로 자릅니다. 자체 서비스에 보내는 범용 Webhook 등의 긴 응답 본문이 이력 목록을 불필요하게 키우지 않도록 합니다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// UTF-8 문자가 중간에서 잘려 화면에 깨진 글자가 나오지 않도록 문자 경계까지 되돌립니다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders는 IN (...)에 사용할 start부터의 $n 자리표시자와 인자를 만듭니다. 예: start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
