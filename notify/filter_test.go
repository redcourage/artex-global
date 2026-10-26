package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 잘못된 JSON/빈 입력/틀린 필드 유형은 모두 영값 Filter, 즉 필터 없음으로
	// 대체해야 한다. 누락보다 추가 발송을 우선하는 불변식이다.
	// 오류나 부분 해석으로 바꾸면 오타 하나로 높은 위험 알림이 전부 조용히 누락될 수 있다.
	cases := []struct {
		name string
		raw  string
	}{
		{"빈 입력", ""},
		{"잘못된 JSON", `{not json`},
		{"잘린 JSON", `{"min_severity":`},
		{"유형 불일치", `{"min_severity": 123, "task_ids": "abc"}`},
		{"최상위 배열", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("잘못된 설정은 영값 Filter여야 함, 실제 %+v", f)
			}
			// 영값 Filter는 모든 이벤트에 일치해야 한다.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("영값 Filter는 모든 이벤트에 일치해야 함")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 알 수 없는 심각도는 순위 0이라 비어 있지 않은 임계값에 차단된다(불확실하면 발송 안 함).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 예상 %v, 실제 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL注入",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"빈 범위=무제한", Filter{}, true},
		{"작업 일치", Filter{TaskIDs: []int64{7}}, true},
		{"작업 불일치", Filter{TaskIDs: []int64{8}}, false},
		{"여러 작업 중 일치 포함", Filter{TaskIDs: []int64{8, 7}}, true},
		{"자산 교집합 있음", Filter{AssetIDs: []int64{20, 99}}, true},
		{"자산 교집합 없음", Filter{AssetIDs: []int64{99}}, false},
		{"작업과 자산 모두 일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"작업 일치/자산 불일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("예상 %v, 실제 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"빈 include=전체 수신", Filter{}, "任意类型", true},
		{"include 일치", Filter{VulnClassInclude: []string{"SQL"}}, "SQL注入", true},
		{"include 불일치", Filter{VulnClassInclude: []string{"命令执行"}}, "SQL注入", false},
		{"include 여러 단어 중 하나 일치", Filter{VulnClassInclude: []string{"命令执行", "SQL"}}, "SQL注入", true},
		{"대소문자 무시", Filter{VulnClassInclude: []string{"sql"}}, "SQL注入", true},
		{"exclude 일치 시 제외", Filter{VulnClassExclude: []string{"信息泄露"}}, "信息泄露", false},
		{"exclude 불일치 시 허용", Filter{VulnClassExclude: []string{"信息泄露"}}, "SQL注入", true},
		// 제외가 포함보다 우선하므로 동시에 일치하면 제외한다.
		{"제외가 포함보다 우선", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"注入"},
		}, "SQL注入", false},
		// 공백 키워드는 무시해야 공백을 포함한 모든 문자열에 일치하는 동작을 막는다.
		{"공백 키워드 무시", Filter{VulnClassInclude: []string{"", "  "}}, "SQL注入", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("예상 %v, 실제 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본 꺼짐. 대부분의 취약점 알림은 상태 추적보다 새 발견을 뜻한다.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("미활성 상태 변경 이벤트는 건너뛰어야 함")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change 활성화 후 상태 변경에 일치해야 함")
	}
	// 생성 이벤트는 on_status_change에 영향받지 않는다.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("생성 이벤트는 on_status_change에 의존하면 안 됨")
	}
}
