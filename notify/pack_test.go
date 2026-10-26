package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 요약이 채널 길이를 넘으면 항목 단위로 묶어야 한다는 수정을 검증한다.
// 포함하지 못한 수를 정확히 반환해 호출자가 실제 전달된 것만 완료 처리하게 한다.
//
// 전체 렌더링 후 자르고 모두 완료 처리하던 방식은 뒤 내용이 사라져도
// 이력은 모두 성공이라 취약점 누락을 어디에서도 발견하지 못했다.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 중국어 요약 200건은 WeCom 4096바이트를 확실히 넘긴다.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 상한 %d 초과", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("본문이 유효한 UTF-8이 아님")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 포함해야 함(0 < kept < %d), 실제 %d", len(m.Items), kept)
	}
	// 머리에 이번 포함 수와 남은 수를 정확히 표시해야 독자가 그 숫자를
	// 전체 수로 오해하지 않는다.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에서") {
		t.Fatalf("머리에 미포함 개수를 안내해야 함:\n%s", body[:minInt(400, len(body))])
	}
	// 앞 kept개만 포함해야 한다.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("%d번째 항목이 이번 메시지에 있어야 함:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("%d번째 항목은 다음 묶음이므로 포함하면 안 됨", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0=무제한.
	if kept != len(m.Items) {
		t.Fatalf("무제한이면 전체 유지, 실제 kept=%d", kept)
	}
	if strings.Contains(body, "其余") {
		t.Fatalf("자르지 않으면 잘림 안내가 없어야 함:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 한 건도 못 넣을 작은 예산이어도 한 건은 보내고 최종 자르기에 맡긴다.
	// 그렇지 않으면 긴 취약점 하나가 매번 들어가지 않아 전체 묶음이 영구 대기한다.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("최소 1건 유지, 실제 %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("단건 메시지 전달 수는 1, 실제 %d", kept)
	}
	// 빈 메시지에는 전달할 항목이 없다.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("빈 메시지는 0건, 실제 %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram은 문자 수 제한. 바이트 기준은 중국어 메시지를 3분의 1로 줄인다.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("본문 %d문자가 상한 %d 초과", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 포함해야 함, 실제 %d", kept)
	}
	if !strings.Contains(text, "다음 메시지") {
		t.Fatalf("미포함 나머지가 있음을 안내해야 함:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("카드에 일부만 포함해야 함, 실제 %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 이 두 채널은 본문을 자르지 않아 전체가 전달된다.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("전제 조건 불충족")
	}
	// markdownBody(0)가 전체를 유지하는 렌더링 반환값으로 간접 확인한다.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("무제한이면 모두 포함, 실제 %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent는 신뢰할 수 없는 내용이 메시지 구조를 바꾸지 않는 회귀 테스트다.
// 제목/요약은 대상 응답을 읽은 모델 출력, 자산 이름은 대상 URL에서 온다.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 결과에 반드시 포함할 이스케이프된 형태.
		wrong []string // 결과에 포함하면 안 되는 이스케이프 전 형태.
	}{
		{
			name: "제목의 줄바꿈과 외부 링크",
			item: Item{
				Severity: "high",
				Name:     "登录口 SQL 注入\n[紧急：点此验证账号](http://attacker.tld)",
			},
			// 줄바꿈은 접어야 새 목록/인용 블록을 위조하지 못한다.
			// 대괄호/소괄호는 이스케이프해야 외부 링크로 동작하지 않는다.
			must:  []string{`\[紧急：点此验证账号\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[紧急", "\n\n[紧急"},
		},
		{
			name: "제목의 이미지 비컨",
			item: Item{
				Severity: "high",
				Name:     "漏洞 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "자산 이름의 강조와 인용",
			item: Item{
				Severity: "high",
				Name:     "普通标题",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "요약의 백틱과 세로줄",
			item: Item{
				Severity: "high",
				Name:     "标题",
				Summary:  "`code` | 表格",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단건 writeItem은 세 markdown 채널이 공유하는 렌더링 경로다.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("이스케이프 형태 %q 누락:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("이스케이프되지 않은 %q로 구조/외부 링크 주입 가능:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst는 역슬래시를 먼저 처리하는 순서를 고정한다.
// 그렇지 않으면 나중에 추가한 역슬래시를 다시 감싸 두 개가 출력된다.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("이스케이프 순서 오류, 실제 %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes는 구체적인 회귀를 고정한다.
// Markdown 이스케이프가 Telegram HTML로 새어 나오면 안 된다. 과거 공통 제목 함수에서
// 처리했을 때 `\(1\)` 같은 역슬래시가 메시지에 표시되었다.
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 본문에 markdown 역슬래시가 표시됨:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
