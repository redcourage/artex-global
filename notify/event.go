package notify

// Snapshot은 notification_events.snapshot JSONB 열의 계약이다. db 계층의 취약점 저장
// 트랜잭션이 쓰고 server 계층의 발송 엔진과 필터가 읽는다. 알림 도메인의 데이터이므로
// 여기에 정의하며 db는 필드 의미를 해석하지 않고 직렬화만 한다.
//
// 렌더링 때 조회하지 않고 취약점 필드를 중복 저장하는 이유: 나중에 이름/심각도/상태가 바뀌어도
// 알림은 발생 당시 결론을 반영해야 한다. 재조회하면 사후에 low로 바뀐 결과가 전달될 수 있다.
// 또한 fan-out과 렌더링에서 findings/tasks/assets 세 테이블 JOIN이 필요 없어졌다.
type Snapshot struct {
	// 이벤트 유형: finding_created / finding_status_changed.
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed일 때만 비어 있지 않다.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item은 채널이 렌더링할 발송 대상 취약점 한 건이다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets는 해석된 자산 표시 이름(도메인/IP 등)이다. DB에 접근하지 않는 이 패키지 대신
	// server 계층에서 채운다.
	Assets []string
	// DetailURL은 취약점 상세 링크. public_base_url 미설정으로 비어 있으면 렌더링 시 생략한다.
	DetailURL string
	// 상태 변경 이벤트 전용. 두 값이 모두 있으면 대기 → 수정됨처럼 표시한다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange는 해당 항목이 상태 변경 이벤트인지 반환한다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title은 표시 제목을 반환한다. 수동 name, vulnclass 순으로 사용하며
// 둘 다 비어 있으면 플레이스홀더로 대체해 빈 제목을 출력하지 않는다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message는 한 번의 채널 발송에 필요한 전체 내용이다.
type Message struct {
	// 단건 발송은 길이 1, 요약(digest)은 전체 묶음이다.
	// 빈 슬라이스는 유효하지 않으며 호출자가 하나 이상 보장해야 한다.
	Items []Item
	// Batch=true면 제목/시간 범위/개수를 포함하는 요약 메시지로 렌더링한다.
	Batch bool
	// WindowMinutes는 요약 주기(분)이며 Batch=true일 때 최근 N분 문구에 사용한다.
	// 렌더링 시 time.Since로 계산하지 않고 설정에서 명시적으로 전달해 결정성과 테스트 용이성을 유지한다.
	WindowMinutes int
	// HomeURL은 플랫폼 주소(전역 public_base_url). 빈 값이면 대시보드 링크 생략.
	HomeURL string
}
