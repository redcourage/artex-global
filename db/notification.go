package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 IM 알림 채널 설정과 이벤트 계층을 담당합니다. 전송 획득과 상태 전이는 notification_delivery.go를 참고하세요.
// 수정 시 두 불변 조건을 유지해야 합니다.
// 1. 취약점 저장 트랜잭션(RecordFindingTx)은 InsertNotificationEventTx의 INSERT 한 번만 호출합니다. 알림 테이블 조회나 필터 비교를 추가하면 잘못된 사용자 필터가 취약점 저장을 오염시키거나 중단할 수 있습니다.
// 2. 필터 비교는 오류를 반환하지 않으며 잘못된 설정은 일치로 처리합니다(notify.Match 참고). 알림 누락보다 중복 전송을 허용합니다.

// ErrNotificationChannelNotFound는 채널이 없음을 나타냅니다.
var ErrNotificationChannelNotFound = errors.New("알림 채널이 존재하지 않습니다")

// 전송 상태.
const (
	NotifyStatePending = "pending" // 전송 대기
	NotifyStateSending = "sending" // dispatcher가 획득했으며 임대가 아직 만료되지 않음
	NotifyStateSent    = "sent"    // 전달 완료
	NotifyStateFailed  = "failed"  // 재시도 소진 또는 영구 실패. 수동 재전송 가능
	NotifyStateSkipped = "skipped" // 채널이 비활성화되어 더 이상 전송하지 않음
)

// 알림 모드.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode는 알림 모드의 허용 목록을 검증합니다. findings.status처럼 확장을 쉽게 하기 위해 DB CHECK를 사용하지 않습니다.
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel은 채널 인스턴스 설정입니다. Config와 Filter는 원본 JSON을 유지하고 해석은 notify 패키지에 맡깁니다. DB 계층은 필드 의미를 알지 못합니다.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled는 필드 생략과 명시적 false를 구분하기 위해 포인터를 사용합니다. 프런트엔드 토글은 변경된 필드만 제출합니다.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled는 채널 활성화 여부를 반환합니다. Enabled가 nil(미조회)이면 활성으로 취급합니다.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent는 이벤트 사실 하나입니다.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels는 활성 채널 우선, 같은 상태에서는 id순으로 모든 채널을 반환합니다. SQL에서 정렬하여 UI와 dispatcher가 같은 안정적 순서를 보도록 합니다.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID는 채널 하나를 조회합니다.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel은 채널을 생성하거나 업데이트합니다. 업데이트 시 명시된 필드(nil이 아니거나 비어 있지 않은 값)만 덮어씁니다.
// 따라서 프런트엔드는 부분 수정 폼을 제출할 수 있고 표시하지 않은 config 필드를 다시 전송할 필요가 없습니다. 마스킹 값으로 실제 키를 덮어쓰는 사고를 막습니다.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// 0은 제한 없음을 뜻하는 유효한 설정이므로 변환하지 않습니다.
	// 이전에는 RatePerMin <= 0일 때 기본값을 넣었으나 명시적 0까지 기본 20(DingTalk/WeCom/Telegram) 또는 100(Feishu)으로 바꿨습니다. 문서, UI, takeTokens는 0을 무제한으로 해석하므로 사용자 예상과 달리 조용히 제한되는 문제가 있었습니다.
	// 필드 생략과 명시적 0은 호출자만 구분할 수 있으므로 기본값은 server.notifyCreateChannel에서 필드가 없을 때만 채웁니다.
	if c.RatePerMin < 0 {
		return 0, errors.New("속도 제한 값은 음수일 수 없습니다")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled는 활성 상태를 전환합니다. 비활성화하면 미전송 항목도 skipped로 표시하여 재활성화 시 오래된 취약점이 몰려와 새 취약점으로 오인되지 않도록 합니다.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "채널이 비활성화되었습니다", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel은 채널을 삭제합니다. 설정이 없으면 이력을 해석할 수 없으므로 외래 키를 따라 전송 이력도 삭제합니다.
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx는 호출자의 트랜잭션에서 알림 이벤트를 최선 노력 방식으로 저장합니다.
// 취약점 저장 경로의 유일한 알림 작업은 INSERT 한 번이며 다른 표 조회, 채널 확인, 필터 비교를 하지 않습니다. 커밋 시 취약점 저장과 알림 작업 생성이 원자적으로 일치합니다.
// PostgreSQL에서는 문장 하나의 오류도 전체 트랜잭션을 aborted로 만들어 COMMIT까지 실패하게 합니다. 따라서 오류를 무시하고 취약점을 저장하려면 SAVEPOINT로 이 INSERT의 오류를 격리해야 합니다.
// 알림은 편의 기능이므로 미이전 알림 테이블이나 순간 디스크 오류 때문에 중요한 취약점 저장을 취소해서는 안 됩니다. 오류를 격리하고 로그를 남긴 뒤 false를 반환하여 취약점 커밋을 허용합니다. 해당 알림 하나가 유실되는 것은 감수합니다.
// 반환형이 error가 아닌 bool인 이유도 호출자가 이를 취약점 저장 실패로 취급하지 않도록 하기 위해서입니다.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 알림 이벤트 직렬화 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 저장점 생성 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 알림 이벤트 저장 실패 finding=%d(취약점 기록에는 영향 없음): %v", findingID, err)
		// 저장점으로 롤백하여 트랜잭션을 aborted 상태에서 복구합니다.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 저장점으로 롤백 실패 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 긴 트랜잭션에 불필요한 저장점이 쌓이지 않도록 해제합니다.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent는 InsertNotificationEventTx를 독립 트랜잭션으로 실행하는 버전입니다. 실제 finding이 없는 채널의 테스트 메시지처럼 기존 트랜잭션 밖에서 사용합니다.
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("알림 이벤트 스냅샷 직렬화 실패: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents는 미분배 취약점 이벤트를 현재 활성 채널별 전송 작업으로 만들고 처리한 이벤트 수와 새 전송 수를 반환합니다.
// 한 트랜잭션에서 FOR UPDATE SKIP LOCKED로 이벤트를 획득하므로 여러 프로세스가 서로 다른 행을 처리합니다. db/task_archives.go의 completeNextArchiveJob도 같은 방식을 사용합니다.
// 선택적 JSONB 필드 여섯 조합을 SQL로 표현하면 유지보수가 어렵고 채널은 수동 설정한 소수이므로 Go에서 전체 채널을 읽어 메모리로 비교하는 편이 빠르고 검증하기 쉽습니다.
// 일치하는 채널이 없는 이벤트도 fanned_out으로 표시하여 매 tick마다 다시 검색하지 않도록 합니다.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋 성공 후에는 아무 작업도 하지 않습니다.

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// 직접 저장한 스냅샷은 해석 가능해야 하지만 해석 실패가 전체 전송을 중단하지는 않습니다. 필드가 비어 필터 채널에서 건너뛰더라도 잘못된 행 하나가 큐 전체를 막는 것보다 낫습니다.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind는 행의 값을 기준으로 합니다. 스냅샷의 사본은 렌더링용이며 이전 버전에서 저장했을 수 있습니다.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 이번 이벤트를 분배 완료로 표시합니다. 일치한 채널이 없는 이벤트도 포함합니다(함수 주석 참고).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx는 트랜잭션에서 활성 채널을 조회합니다. 수가 적으므로 페이지 처리나 캐시를 사용하지 않아 설정 변경의 적용 시점 문제를 피합니다.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames는 자산 ID를 알림 메시지용 짧은 표시 이름으로 바꿉니다.
// 없는 ID는 건너뛰므로 결과가 더 짧을 수 있지만 입력 순서는 유지합니다. 재시도 후 자산 순서가 달라져 자산이 바뀐 것으로 오해하지 않도록 합니다.
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName은 자산 유형별로 가장 식별하기 쉬운 값을 고릅니다. 없으면 빈 문자열을 반환하고 표현은 호출자가 결정합니다. '자산#42' 같은 임의 자리표시자가 실제 도메인으로 오인되지 않도록 합니다.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify는 취약점 처리 상태를 업데이트하고 같은 트랜잭션에 상태 변경 알림을 등록합니다.
// from은 이전 상태, found는 취약점 존재 여부, notified는 이벤트 등록 성공 여부입니다.
// 실제 상태가 변하지 않은 중복 제출이나 멱등 재실행은 알림을 만들지 않습니다. 취약점이 없으면 쓰지 않고 found=false를 반환하며 호출자가 404로 변환합니다.
// 이벤트 등록 실패는 저장점으로 격리하므로 notified=false여도 상태 업데이트는 성공한 것이며 호출자가 오류로 처리해서는 안 됩니다.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx는 호출자의 트랜잭션에서 취약점 상태를 변경하고 알림을 등록합니다.
// 모든 상태 변경 경로가 같은 의미를 공유하도록 트랜잭션 단위로 분리했습니다. 과거에는 patchFinding만 알림을 사용했고 finding_retests의 수정 완료 결론은 직접 UPDATE하여 on_status_change 채널에 알림이 가지 않았습니다.
// from=이전 상태, found=존재 여부, changed=실제 변경 여부, notified=이벤트 등록 성공 여부입니다. 등록 실패는 상태 업데이트에 영향을 주지 않습니다(RecordNotificationEventTx 참고).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 실제 상태가 같으면 이벤트를 등록하지 않습니다. 중복 제출이나 멱등 재실행은 알림을 만들지 않아야 합니다.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats는 알림 페이지 상단의 개요 집계입니다.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 가장 오래된 전송 대기 항목의 경과 시간(밀리초)
}

// NotificationStatsSnapshot은 알림 시스템의 상태를 집계합니다. BacklogAgeMS는 전송 지연을 직접 보여 줍니다. pending 개수가 3으로 같아도 3초 대기와 3시간 대기는 다르기 때문입니다.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
