package notify

import (
	"fmt"
	"strings"
)

// DingTalk/WeCom 등 Markdown 채널의 공통 메시지 렌더링.
// Feishu 카드 JSON, Telegram HTML, 이메일 HTML은 각각 어댑터에서 렌더링한다.

// maxAssetsShown은 메시지에 표시할 최대 자산 수다. 취약점에 수십 자산이 연결되어도
// 모두 표시하면 공간만 차지하고 IM에서 네 번째 이후 도메인을 읽을 가능성은 낮다.
const maxAssetsShown = 3

// maxSummaryRunes는 요약 압축 문자 수다. IM은 상세 확인을 안내하는 용도이며
// 전체 보고서는 플랫폼에 있다.
const maxSummaryRunes = 120

// markdownReservedBytes는 머리(요약 행/심각도 분포/잘림 안내)와
// 꼬리(플랫폼 링크)의 예약 크기다. 항목 묶음 예산에서 빼 머리/꼬리를 보존한다.
// 잘리면 어느 묶음인지, 미표시 항목이 몇 개인지 알 수 없다.
const markdownReservedBytes = 320

// markdownEscape는 markdown 메타 문자를 이스케이프한다.
//
// 취약점 제목/요약/유형/자산 이름은 신뢰할 수 없는 출처에서 온다.
// 제목/요약은 대상 응답을 읽은 모델 출력이며 자산 URL은 대상이 제어하는
// query까지 포함하는 스캔 결과다. 이스케이프하지 않으면 아래 제목의 취약점이
//
// 로그인 SQL 삽입\n[긴급: 계정 확인](http://attacker.tld)
//
// 보안 엔지니어의 DingTalk/Feishu에 클릭 가능한 외부 링크로 표시된다.
// `![](http://attacker.tld/beacon)`은 렌더링 시 클라이언트가 가져와
// 취약점을 읽었음을 알리고 읽은 사람의 IP를 유출한다. 악의가 없는 굵은 글씨나
// 인용 블록도 뒤의 중요한 취약점을 접힘 아래로 밀어낼 수 있다.
//
// 제목/링크/강조/목록/인용/취소선 등 구조나 클릭 요소를 만드는 문자를 처리한다.
// `\`를 먼저 처리해야 뒤에 추가한 역슬래시를 다시 이스케이프하지 않는다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText는 신뢰할 수 없는 텍스트를 한 줄로 만들고 이스케이프한다.
// 줄바꿈 자체가 목록/인용 블록을 위조할 수 있고 문자 이스케이프만으로는
// 막을 수 없어 한 줄 변환이 함께 필요하다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle은 IM 제목/카드 제목용으로 이스케이프하지 않은 원본 제목을 반환한다.
//
// Markdown 본문, Telegram HTML, Feishu plain_text, 일반 Webhook JSON과 이메일 제목이
// 이 함수를 공유하므로 여기서는 이스케이프하지 않는다. 각 출력의 규칙이 달라
// Markdown 처리를 HTML에 넣으면 역슬래시가 보이고 JSON에 넣으면 데이터가 오염된다.
// 따라서 writeItem/feishuItemLines/telegramEscape 등 출력 측이 처리해야 한다.
// 과거 공통 함수에 Markdown 이스케이프를 추가하자 Telegram 메시지에
// `\(1\)`처럼 역슬래시가 그대로 표시되었다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 요약 · 총 %d건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody는 본문과 실제 포함한 항목 수를 반환한다.
//
// kept는 이번 실제 전달 수이며 호출자는 앞 kept개만 전달됨으로 표시한다.
// 채널 길이 제한으로 제외된 항목은 다음 묶음에 남겨야 하며 함께 성공으로
// 표시하면 메시지는 잘렸는데 발송 기록은 전체 성공인 조용한 누락이 발생한다.
// 그 경우 뒷부분이 발송되지 않았음을 어디에서도 알 수 없다.
//
// maxBytes<=0은 무제한.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단일 메시지는 길어도 최종 자르기에 맡겨 발송한다. 취약점 일부 정보라도
		// 전혀 보내지 않는 것보다 낫다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro는 요약 머리에 시간 범위, 개수, 심각도 분포를 표시한다.
// 수신자가 플랫폼에 들어가지 않고 즉시 처리 필요 여부를 판단할 수 있다.
//
// items는 실제 포함한 항목, total은 이번 묶음의 전체 수다. 다르면 다음 메시지로
// 이어질 개수를 명시해야 독자가 제목의 수를 전체로 오해하지 않는다.
// 그렇지 않으면 아직 발송하지 않은 항목은 화면상 존재하지 않는 셈이다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분간 신규 취약점 %d건**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**신규 취약점 %d건**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(이 메시지에는 앞 %d건만 표시하며, 나머지 %d건은 다음 메시지에서 이어서 보냅니다)", len(items), extra)
	}
	// 심각도 분포로 치명적 항목 여부를 바로 알린다. 이번 메시지에 실제 포함된
	// 항목만 집계해 표시 개수와 아래 항목 수를 일치시킨다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem은 개별 취약점을 렌더링한다.
//
// prefix는 요약 목록 번호다. single=true이면 요약/링크를 포함한 전체 형식,
// 요약 목록에서는 한 줄만 표시해 50건이 긴 문서로 늘어나지 않게 한다.
//
// 외부 내용(제목/유형/자산/요약)은 모두 markdownText로 한 줄 변환과 이스케이프를 수행한다.
// 상세 링크는 관리자의 public_base_url로 구성하며 클릭 가능해야 하므로
// 원본 그대로 출력한다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 요약 모드: 한 줄로 표시하고 압축한 자산과 요약을 뒤에 추가한다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**개요**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
