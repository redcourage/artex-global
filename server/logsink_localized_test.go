package server

import "testing"

func TestLogLevelPreservesLocalizedMessages(t *testing.T) {
	// 기존 로그와 번역된 로그가 같은 수준으로 분류되어야 합니다.
	cases := []struct {
		name, original, localized, want string
	}{
		{"failure", "[planner] 规划失败", "[planner] 계획 실패", "error"},
		{"discard", "[activity] 丢弃活动记录", "[activity] 활동 기록 폐기", "error"},
		{"denial", "[guard] 请求被拒绝", "[guard] 요청 거부", "error"},
		{"unreachable", "[network] 不可达", "[network] 연결할 수 없습니다", "error"},
		{"disabled", "[notify] 渠道禁用", "[notify] 채널 비활성", "warn"},
		{"retry", "[worker] 重试", "[worker] 재시도", "warn"},
		{"enrichment_queue_full", "[enrich] 队列已满，丢弃任务 kind=%d id=%d", "[enrich] 대기열이 가득 차 작업을 폐기합니다 kind=%d id=%d", "error"},
		{"seed_gateway", "[seed] task %s: 目标 %q 是 LLM 网关，拒绝作为渗透目标", "[seed] task %s: 대상 %q은(는) LLM 게이트웨이이므로 침투 대상으로 사용하는 것을 거부합니다", "error"},
		{"incremental_vacuum_not_enabled", "[traffic] 索引库未启用增量回收（auto_vacuum=%d）：删除流量不会缩小 index.sqlite，需要执行一次存储压缩来转换", "[traffic] 인덱스 DB의 증분 회수가 켜져 있지 않습니다(auto_vacuum=%d). 트래픽을 삭제해도 index.sqlite가 줄지 않으며 저장소 압축을 한 번 실행해 변환해야 합니다", "info"},
		{"ordinary", "[planner] 规划完成", "[planner] 계획 완료", "info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, message := range []string{tc.original, tc.localized} {
				if got := levelOf(message); got != tc.want {
					t.Errorf("levelOf(%q) = %q, want %q", message, got, tc.want)
				}
			}
		})
	}
}
