package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 핵심 불변식: WeCom은 바이트 제한이고 중국어는 글자당 3바이트다.
	// 바이트를 그대로 자르면 글자가 나뉘어 잘못된 UTF-8로 플랫폼이 거부할 수 있다.
	// 서로소 길이의 여러 중국어/영어 혼합 입력으로 모든 절단점을 검사한다.
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4바이트 emoji라 잘못된 절단이 더 명확하다.
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("입력 %q max=%d: 유효하지 않은 UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("입력 %q max=%d: 결과 %d바이트가 상한 초과", in, max, len(got))
			}
			// 자르지 않았다면 내용을 바꾸면 안 된다.
			if len(in) <= max && got != in {
				t.Fatalf("입력 %q max=%d: 상한 미만인데 내용 변경 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0은 무제한이어야 함")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0은 무제한이어야 함")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max가 말줄임표보다 작을 때 추가 때문에 상한을 넘기면 안 된다.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1인데 결과 %q 길이 %d가 상한 초과", got, len(got))
	}
	// 일반적인 경우에는 말줄임표를 포함한다.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("말줄임표가 필요함, 실제 %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// Telegram은 문자 수 제한이므로 TruncateBytes와의 차이를 유지해야 한다.
	// 바이트 기준을 쓰면 중국어 메시지가 3분의 1 길이가 된다.
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("예상 5문자, 실제 %d (%q)", n, got)
	}
	// 같은 문자열도 바이트 기준이면 훨씬 짧아야 한다.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("바이트 기준이 문자 기준과 같은 문자 수를 내면 안 됨")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("第一行\n\n第二行\t带制表   多空格", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("모든 공백을 합쳐야 함, 실제 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("연속 공백을 유지하면 안 됨, 실제 %q", got)
	}
	// 잘린 뒤에도 읽을 수 있고 유효해야 한다.
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("예상 4문자, 실제 %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML을 직접 자르면 `<a href="htt` 같은 조각이 생겨 플랫폼이 전체를 거부할 수 있다.
	s := `<b>标题</b>正文正文正文<a href="https://example.com/very/long/path">查看详情</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 결과 %d문자가 상한 초과", max, n)
		}
		// 끝에 `<`가 있고 `>`가 없는 열린 태그가 남으면 안 된다.
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 끝 태그가 잘림 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("자산 없으면 빈 문자열이어야 함, 실제 %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("상한 미만이면 모두 표시해야 함, 실제 %q", got)
	}
	// 상한 초과 시 전체 수를 표시해야 독자가 미표시 자산 수를 알 수 있다.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "등 5개") {
		t.Fatalf("전체 수 5를 표시해야 함, 실제 %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("빈 심각도는 순위 0이라 모든 임계값에 차단되어야 함")
	}
	if !AtLeast("critical", "") {
		t.Fatal("빈 임계값은 허용해야 함")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("상태 매핑 오류, 실제 %q", got)
	}
	// 알 수 없는 상태는 라벨을 만들지 않고 그대로 표시한다.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("알 수 없는 상태는 원본 유지, 실제 %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity는 감사에서 발견한 누락을 검증한다. 자를 때
// 태그뿐 아니라 HTML 엔티티도 나누지 않아야 한다.
//
// `&amp;`를 `&amp`로 자르면 전체 메시지가 거부될 수 있다.
// 긴 요약 메시지는 흔하므로 비용이 크다.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 끝에 &는 있지만 대응하는 ;가 없는 엔티티 조각이 남으면 안 된다.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 끝에 엔티티 조각 %q가 남음", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 잘못된 엔티티 발생", max)
		}
	}
}
