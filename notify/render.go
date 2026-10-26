package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes는 s를 max바이트 이하로 자르며 문자를 나누지 않아 유효한 UTF-8을 보장한다.
//
// 문자 경계에서 잘라야 하는 이유: WeCom markdown은 문자 수가 아닌 4096바이트 제한이며
// 중국어 한 글자는 3바이트다. 바이트 슬라이스로 바로 자르면 문자가 나뉘어 잘못된
// UTF-8이 되고 플랫폼이 전체를 거부하거나 깨진 문자로 표시할 수 있다. 예산 위치에서
// 가장 가까운 rune 시작 바이트까지 되돌린다(utf8.RuneStart로 연속 바이트 0b10xxxxxx 판단).
//
// max<=0은 무제한. 자른 뒤 말줄임표를 붙이되 max에 들어가지 않을 만큼 작으면 생략한다.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max가 말줄임표보다 작으면 말줄임표 없이 잘라 상한을 넘지 않게 한다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine은 모든 공백을 합쳐 여러 줄을 한 줄로 만들고 문자 수로 자른다.
// IM 제목 행에 사용하며 요약의 줄바꿈이 표/제목 배치를 망가뜨리지 않게 한다.
// max<=0은 길이 무제한.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes는 바이트가 아닌 max문자 이하로 자르고 초과 시 말줄임표를 추가한다.
// max<=0은 무제한.
//
// TruncateBytes와 차이는 플랫폼 기준이다. WeCom은 바이트, Telegram은 문자 수 제한이다.
// 잘못된 기준은 오류 없이 메시지를 예상보다 짧게 만든다. 중국어는 글자당 3바이트라
// 4096바이트는 약 1365글자다. 두 함수를 유지하고 채널에 맞게 선택해야 한다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML은 HTML을 문자 수로 자르며 불완전한 태그를 만들지 않는다.
//
// HTML을 바로 자르면 `<a href="htt` 같은 조각이 생겨 파서가 전체를 거부하거나
// 뒤 본문을 속성값으로 삼킬 수 있다. 먼저 문자 수로 자른 뒤
// 끝에 닫히지 않은 `<`가 있으면 그 앞까지 되돌린다.
//
// Telegram 파서가 열린 태그를 닫으므로 </b> 같은 닫는 태그를 직접 보충하지 않는다.
// 직접 구현하려면 속성 따옴표/주석/자기 닫힘 태그를 처리해야 해 복잡도가 이득보다 크다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 끝에 `<` 이후 `>`가 없는 태그 조각이 있으면 `<` 앞까지 되돌린다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// `&amp;`가 `&amp`로 잘리는 것처럼 끝의 HTML 엔티티가 불완전해도 되돌린다.
	// 엔티티 조각 때문에 전체 메시지가 거부될 수 있다. 길이 제한을 넘는 요약 메시지는
	// 흔하므로 이 문제로 알림 전체를 잃지 않게 한다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount는 예산 안에 완전히 들어가는 항목 수를 계산해 요약을 항목 단위로 묶는다.
//
// 전체 렌더링 후 자르면 뒷부분 항목이 사라져도 발송 기록은 전달됨이 된다.
// 메시지와 이력 어디에서도 누락을 알 수 없어 취약점이 사라지는 셈이다.
// 항목 단위로 묶으면 들어가지 않는 항목은 DB에서 다음 묶음을 기다리고
// kept는 이번 메시지에 실제 전달된 항목 수를 뜻한다.
//
// 매개변수: maxSize<=0은 무제한, reserve는 머리/꼬리에 예약한 크기다.
// size는 길이 측정 함수다(WeCom/DingTalk는 바이트, Telegram은 문자 수).
// 잘못된 기준은 오류 없이 중국어 메시지를 상한보다 훨씬 작게 만든다.
// render는 idx 항목의 실제 텍스트를 만든다. 길이는 내용에 따라 달라 추정할 수 없다.
//
// 항목이 있으면 최소 1을 반환한다. 단일 항목이 매우 길어도 보내고 호출자의 최종 자르기에
// 맡겨야 긴 취약점 하나가 전체 묶음을 영구 정체시키지 않는다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize/runeSize는 packItemCount의 두 길이 기준이다. 이름을 붙여 호출 위치에서
// 익명 func(s string) int 대신 어떤 기준인지 바로 알 수 있게 한다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine은 자산 목록을 한 줄로 표시하고 limit 초과분을 생략하며 전체 수를 표시한다.
// 취약점 하나가 수십 자산에 연결될 수 있어 모두 표시하면 메시지를 가득 채운다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " 등 " + itoa(len(assets)) + "개"
}

// itoa는 표시 텍스트 조합용 strconv.Itoa 별칭으로 여러 파일의 import를 줄인다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
