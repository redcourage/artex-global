package db

import (
	"regexp"
	"testing"
)

// 내장 삭제 API 경로 규칙은 전체 tool_input JSON을 비교하므로 테스트도 JSON으로 제공하여
// Interceptor가 실제로 받는 subject와 맞춥니다.
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET으로 삭제 API 호출
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST로 삭제 API 호출
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 경로 규칙이 지원하지 않던 접미사를 여기서 보완합니다.
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("일치해야 하지만 허용되었습니다: %s", s)
		}
	}

	// 동사 뒤 구분자를 요구하여 /delivery나 /details 같은 읽기 경로가 잘못 차단되지 않도록 합니다.
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 삭제 동사가 경로가 아닌 도메인에 있음
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("잘못된 차단: %s", s)
		}
	}
}
