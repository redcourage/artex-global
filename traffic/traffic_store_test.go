package traffic

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// flowOpt tweaks the synthetic flow built by newFlow.
type flowOpt func(*mproxy.Flow)

func withRespType(ct string) flowOpt {
	return func(f *mproxy.Flow) { f.Response.Header.Set("Content-Type", ct) }
}

// newFlow builds the minimal flow record() needs: a request with a URL, method
// and body, plus a response with a status and body.
func newFlow(host, method, path string, reqBody, respBody []byte, opts ...flowOpt) *mproxy.Flow {
	u, err := url.Parse("http://" + host + path)
	if err != nil {
		panic(err)
	}
	f := &mproxy.Flow{
		Request: &mproxy.Request{
			Method: method,
			URL:    u,
			Proto:  "HTTP/1.1",
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   reqBody,
		},
		Response: &mproxy.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       respBody,
		},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func openTraffic(t *testing.T) (*Traffic, string) {
	t.Helper()
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr, dir
}

func onlyExchangeID(t *testing.T, tr *Traffic) string {
	t.Helper()
	var id string
	if err := tr.DB().QueryRow(`SELECT id FROM exchanges`).Scan(&id); err != nil {
		t.Fatalf("exchange id 읽기: %v", err)
	}
	return id
}

// TestRecordKeepsBodiesInIndex is the core of the storage change: a recorded
// exchange produces no per-request directory at all, and its bodies are served
// back out of SQLite.
func TestRecordKeepsBodiesInIndex(t *testing.T) {
	tr, dir := openTraffic(t)

	tr.record(newFlow("api.example.com", "POST", "/v1/login",
		[]byte(`{"user":"admin","password":"P@ssw0rd"}`),
		[]byte(`{"token":"abc123","note":"内网测试账号"}`)))

	// The URL-mirroring tree is gone: no host directory, no nested path segments.
	if _, err := os.Stat(filepath.Join(dir, "api.example.com")); !os.IsNotExist(err) {
		t.Fatalf("record가 여전히 디스크에 host 디렉터리를 생성함(stat err=%v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "_") {
			t.Fatalf("data 디렉터리에 내부 디렉터리가 아닌 %q가 생김. 아직 파일 트리에 쓰고 있음", e.Name())
		}
	}

	id := onlyExchangeID(t, tr)
	req, resp, err := tr.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"POST /v1/login HTTP/1.1", "Host: api.example.com", `"password":"P@ssw0rd"`} {
		if !strings.Contains(req, want) {
			t.Fatalf("요청 원문에 %q가 없음, 실제:\n%s", want, req)
		}
	}
	for _, want := range []string{"HTTP 200", `"token":"abc123"`, "内网测试账号"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("응답 원문에 %q가 없음, 실제:\n%s", want, resp)
		}
	}
}

// TestFullTextSearchMatchesBodies covers what the trigram index buys over the
// previous URL-only search: arbitrary substrings and CJK, across request and
// response bodies.
func TestFullTextSearchMatchesBodies(t *testing.T) {
	tr, _ := openTraffic(t)
	if !tr.fts {
		t.Skip("드라이버에 FTS5가 활성화되지 않음")
	}
	const host = "api.example.com"
	tr.record(newFlow(host, "POST", "/v1/login",
		[]byte(`{"user":"admin","password":"P@ssw0rd"}`),
		[]byte(`{"token":"abc123","note":"内网测试账号"}`)))
	tr.record(newFlow(host, "GET", "/v1/health", nil, []byte(`{"status":"ok"}`)))

	hits := func(term string) int {
		t.Helper()
		rows, err := tr.query(host, "", term, 0, 10)
		if err != nil {
			t.Fatalf("본문으로 %q 검색 중 오류: %v", term, err)
		}
		return len(rows)
	}
	if n := hits("password"); n != 1 {
		t.Fatalf("password 검색 %d개 매칭, 기대값 1", n)
	}
	// Substring inside a token — the default unicode61 tokenizer cannot do this.
	if n := hits("ssw0r"); n != 1 {
		t.Fatalf("부분 문자열 ssw0r 검색 %d개 매칭, 기대값 1", n)
	}
	if n := hits("内网测试"); n != 1 {
		t.Fatalf("중국어 검색 %d개 매칭, 기대값 1", n)
	}
	if n := hits("nonexistent-marker"); n != 0 {
		t.Fatalf("무관한 키워드 %d개 매칭, 기대값 0", n)
	}

	// Too-short terms are reported, not silently treated as "no match".
	if _, err := tr.query(host, "", "ab", 0, 10); err == nil {
		t.Fatal("두 글자 본문 키워드는 명확한 오류를 반환해야 함")
	}
}

// TestLargeBodySpillsButStaysSearchable is the case that motivated indexing from
// memory: the body lives in the blob store, only a preview is inline, and the
// part past the preview is still findable.
func TestLargeBodySpillsButStaysSearchable(t *testing.T) {
	tr, dir := openTraffic(t)
	if !tr.fts {
		t.Skip("드라이버에 FTS5가 활성화되지 않음")
	}
	const host = "dump.example.com"
	const marker = "DB_PASSWORD=hunter2"
	// Marker sits far past blobPreview, so only the full-text index can find it.
	big := []byte(strings.Repeat("-- MySQL dump\n", maxInlineBody/14+2000) + marker)
	if len(big) <= maxInlineBody+blobPreview {
		t.Fatalf("테스트 데이터가 충분히 크지 않음: %d바이트", len(big))
	}
	tr.record(newFlow(host, "GET", "/backup.sql", nil, big, withRespType("application/sql")))

	// Stored under a single bucket level, named by hash.
	var hash string
	if err := tr.DB().QueryRow(`SELECT resp_blob FROM exchange_bodies`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Fatalf("resp_blob=%q, 64자리 sha256이어야 함", hash)
	}
	blob := filepath.Join(dir, "_blobs", "sha256", hash[:2], hash+".bin")
	st, err := os.Stat(blob)
	if err != nil {
		t.Fatalf("blob이 단일 계층 버킷 %s에 저장되지 않음: %v", blob, err)
	}
	if st.Size() != int64(len(big)) {
		t.Fatalf("blob 크기 %d, 기대값 %d", st.Size(), len(big))
	}

	// The reference is registered, which is what GC consults.
	var refs int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM blob_refs WHERE hash=?`, hash).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 1 {
		t.Fatalf("blob_refs 행 수 %d, 기대값 1", refs)
	}

	// Inline: a readable preview plus the pointer, not the whole body.
	_, resp, err := tr.Get(onlyExchangeID(t, tr))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "-- MySQL dump") {
		t.Fatalf("응답에 헤더 미리보기가 없음:\n%s", clip(resp, 300))
	}
	if !strings.Contains(resp, "@blob sha256:"+hash) {
		t.Fatalf("응답에 blob 포인터가 없음:\n%s", clip(resp, 300))
	}
	if strings.Contains(resp, marker) {
		t.Fatal("미리보기에 blobPreview를 넘는 내용을 포함하면 안 됨")
	}
	if len(resp) > blobPreview*2 {
		t.Fatalf("인라인 내용 %d바이트, 미리보기 상한을 크게 초과함", len(resp))
	}

	// Searchable despite living on disk — the index was fed from memory.
	rows, err := tr.query(host, "", marker, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("대형 본문의 키워드 %d개 매칭, 기대값 1", len(rows))
	}

	// And retrievable in pages.
	data, total, err := tr.BlobRange(hash, int64(len(big)-len(marker)), 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(big)) {
		t.Fatalf("BlobRange total=%d, 기대값 %d", total, len(big))
	}
	if string(data) != marker {
		t.Fatalf("BlobRange에서 읽은 값 %q, 기대값 %q", data, marker)
	}
	if _, _, err := tr.BlobRange("../../etc/passwd", 0, 10); err == nil {
		t.Fatal("잘못된 hash는 거부해야 함")
	}
}

// TestBinaryBodyStaysOutOfIndex keeps the index spend on things worth searching:
// binary payloads contribute nothing but a type tag.
func TestBinaryBodyStaysOutOfIndex(t *testing.T) {
	tr, _ := openTraffic(t)
	if !tr.fts {
		t.Skip("드라이버에 FTS5가 활성화되지 않음")
	}
	const host = "cdn.example.com"
	const marker = "SECRETINIMAGE"
	png := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00},
		[]byte(strings.Repeat("x", maxInlineBody)+marker)...)
	tr.record(newFlow(host, "GET", "/logo.png", nil, png, withRespType("image/png")))

	rows, err := tr.query(host, "", marker, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("바이너리 본문이 전문 인덱스에 들어가면 안 되는데 %d개 매칭됨", len(rows))
	}
	_, resp, err := tr.Get(onlyExchangeID(t, tr))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "[binary image/png") || !strings.Contains(resp, "magic=89504e47") {
		t.Fatalf("바이너리 본문은 유형과 매직 바이트를 표시해야 함, 실제:\n%s", clip(resp, 300))
	}
}

// TestGetFallsBackToLegacyTree keeps pre-migration captures readable: their rows
// carry a path and their bodies are still .http files on disk.
func TestGetFallsBackToLegacyTree(t *testing.T) {
	tr, dir := openTraffic(t)
	const host = "old.example.com"
	const id = "1-0001"
	rel := filepath.Join(host, "GET", id)
	exDir := filepath.Join(dir, rel)
	if err := os.MkdirAll(exDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exDir, "request.http"), []byte("GET / HTTP/1.1\nHost: old.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exDir, "response.http"), []byte("HTTP 200\n\nlegacy body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, 1, host, "GET", "/", "http://"+host+"/", 200, "text/html", 0, 11, rel); err != nil {
		t.Fatal(err)
	}

	req, resp, err := tr.Get(id)
	if err != nil {
		t.Fatalf("기존 기록도 읽을 수 있어야 함: %v", err)
	}
	if !strings.Contains(req, "Host: old.example.com") {
		t.Fatalf("기존 요청 원문 오류: %q", req)
	}
	if !strings.Contains(resp, "legacy body") {
		t.Fatalf("기존 응답 원문 오류: %q", resp)
	}
}

// TestGCCollectsBlobsAndEmptyBuckets covers both halves of the collector: the
// reference lookup now comes from blob_refs, and emptied buckets are removed
// instead of accumulating forever.
func TestGCCollectsBlobsAndEmptyBuckets(t *testing.T) {
	tr, dir := openTraffic(t)
	const host = "dump.example.com"
	big := []byte(strings.Repeat("A", maxInlineBody+1024))
	tr.record(newFlow(host, "GET", "/big.bin", nil, big, withRespType("application/sql")))

	var hash string
	if err := tr.DB().QueryRow(`SELECT resp_blob FROM exchange_bodies`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(dir, "_blobs", "sha256", hash[:2])
	if _, err := os.Stat(filepath.Join(bucket, hash+".bin")); err != nil {
		t.Fatal(err)
	}

	if n, err := tr.DeleteHostsExact([]string{host}); err != nil || n != 1 {
		t.Fatalf("DeleteHostsExact=(%d,%v), 기대값 (1,nil)", n, err)
	}
	if _, err := os.Stat(filepath.Join(bucket, hash+".bin")); !os.IsNotExist(err) {
		t.Fatalf("참조가 사라진 blob이 회수되지 않음: %v", err)
	}
	if _, err := os.Stat(bucket); !os.IsNotExist(err) {
		t.Fatalf("빈 버킷 디렉터리가 정리되지 않음: %v", err)
	}
	// Bodies and full-text rows go with the exchange.
	for _, q := range []string{
		`SELECT COUNT(*) FROM exchange_bodies`,
		`SELECT COUNT(*) FROM blob_refs`,
	} {
		var c int
		if err := tr.DB().QueryRow(q).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != 0 {
			t.Fatalf("%s = %d, 기대값 0", q, c)
		}
	}
	if tr.fts {
		var c int
		if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM ex_fts WHERE ex_fts MATCH ?`, ftsQuote("AAAA")).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != 0 {
			t.Fatalf("전문 인덱스에 %d개 남음", c)
		}
	}
}

// TestPageSearchesBodies checks the UI-facing search box picks up the full-text
// index too, not just metadata columns.
func TestPageSearchesBodies(t *testing.T) {
	tr, _ := openTraffic(t)
	if !tr.fts {
		t.Skip("드라이버에 FTS5가 활성화되지 않음")
	}
	tr.record(newFlow("api.example.com", "POST", "/v1/login", nil, []byte(`{"error":"invalid credentials"}`)))
	tr.record(newFlow("api.example.com", "GET", "/v1/health", nil, []byte(`{"status":"ok"}`)))

	rows, total, err := tr.Page(PageQuery{Query: "invalid credentials", RespMin: -1, RespMax: -1}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("본문 키워드 매칭 total=%d rows=%d, 기대값 1/1", total, len(rows))
	}
	// Metadata matching still works alongside it.
	if _, total, err := tr.Page(PageQuery{Query: "health", RespMin: -1, RespMax: -1}, 0, 100); err != nil || total != 1 {
		t.Fatalf("URL 키워드 total=%d err=%v, 기대값 1", total, err)
	}
}

// TestPageFiltersAndSort covers the issue #177 additions: status-class/exact
// filtering, response-size bounds, path (url_template) filtering, and
// server-side sorting by resp_len.
func TestPageFiltersAndSort(t *testing.T) {
	tr, _ := openTraffic(t)
	status := func(code int) flowOpt { return func(f *mproxy.Flow) { f.Response.StatusCode = code } }
	// Three exchanges with distinct status codes and response sizes.
	tr.record(newFlow("api.example.com", "GET", "/api/users", nil, make([]byte, 10), status(200)))
	tr.record(newFlow("api.example.com", "GET", "/api/admin", nil, make([]byte, 100), status(404)))
	tr.record(newFlow("api.example.com", "GET", "/api/users/1", nil, make([]byte, 50), status(500)))

	// Status class band.
	if rows, _, err := tr.Page(PageQuery{Status: "4xx", RespMin: -1, RespMax: -1}, 0, 100); err != nil || len(rows) != 1 || rows[0].Status != 404 {
		t.Fatalf("status=4xx는 404 하나에 매칭되어야 함, 실제 %d개 err=%v", len(rows), err)
	}
	// Exact status.
	if rows, _, err := tr.Page(PageQuery{Status: "500", RespMin: -1, RespMax: -1}, 0, 100); err != nil || len(rows) != 1 || rows[0].Status != 500 {
		t.Fatalf("status=500은 하나에 매칭되어야 함, 실제 %d개 err=%v", len(rows), err)
	}
	// Response-size lower bound (>=60 keeps only the 100-byte row).
	if rows, _, err := tr.Page(PageQuery{RespMin: 60, RespMax: -1}, 0, 100); err != nil || len(rows) != 1 || rows[0].RespLen != 100 {
		t.Fatalf("resp_min=60은 100B 하나에 매칭되어야 함, 실제 %d개 err=%v", len(rows), err)
	}
	// Path (url_template) filter narrows to the /api/admin exchange.
	if rows, _, err := tr.Page(PageQuery{Path: "/api/admin", RespMin: -1, RespMax: -1}, 0, 100); err != nil || len(rows) != 1 || rows[0].Status != 404 {
		t.Fatalf("path=/api/admin은 하나에 매칭되어야 함, 실제 %d개 err=%v", len(rows), err)
	}
	// Sort by response length, ascending then descending.
	asc, _, err := tr.Page(PageQuery{RespMin: -1, RespMax: -1, Sort: "resp_len", Order: "asc"}, 0, 100)
	if err != nil || len(asc) != 3 {
		t.Fatalf("resp_len asc는 3개를 반환해야 함, 실제 %d개 err=%v", len(asc), err)
	}
	if asc[0].RespLen != 10 || asc[1].RespLen != 50 || asc[2].RespLen != 100 {
		t.Fatalf("resp_len asc 순서 오류: %d,%d,%d", asc[0].RespLen, asc[1].RespLen, asc[2].RespLen)
	}
	desc, _, err := tr.Page(PageQuery{RespMin: -1, RespMax: -1, Sort: "resp_len", Order: "desc"}, 0, 100)
	if err != nil || len(desc) != 3 || desc[0].RespLen != 100 || desc[2].RespLen != 10 {
		t.Fatalf("resp_len desc 순서 오류 err=%v", err)
	}
}

func TestQueryHostPortAndURLForms(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("api.example.com:8082", "GET", "/admin", nil, []byte("8082")))
	tr.record(newFlow("api.example.com:8088", "GET", "/admin", nil, []byte("8088")))
	tr.record(newFlow("[2001:db8::1]:8443", "GET", "/admin", nil, []byte("8443")))

	for _, tc := range []struct {
		name string
		host string
		want int
	}{
		{name: "bare host", host: "api.example.com", want: 2},
		{name: "host and port", host: "api.example.com:8082", want: 1},
		{name: "full URL", host: "http://api.example.com:8088/admin", want: 1},
		{name: "IPv6 host and port", host: "[2001:db8::1]:8443", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := tr.query(tc.host, "", "", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.want {
				t.Fatalf("query(%q) returned %d rows, want %d", tc.host, len(rows), tc.want)
			}
		})
	}
}

func TestNormalizeSearchHost(t *testing.T) {
	for _, tc := range []struct {
		raw, host, port string
	}{
		{raw: "API.Example.com", host: "api.example.com"},
		{raw: "api.example.com:8088", host: "api.example.com", port: "8088"},
		{raw: "https://[2001:db8::1]:8443/path", host: "2001:db8::1", port: "8443"},
		{raw: "[2001:db8::1]", host: "2001:db8::1"},
	} {
		host, port, err := normalizeSearchHost(tc.raw)
		if err != nil {
			t.Fatalf("normalizeSearchHost(%q): %v", tc.raw, err)
		}
		if host != tc.host || port != tc.port {
			t.Fatalf("normalizeSearchHost(%q)=(%q,%q), want (%q,%q)", tc.raw, host, port, tc.host, tc.port)
		}
	}
}

// TestTruncateUTF8 guards the preview cut: never split a multi-byte rune.
func TestTruncateUTF8(t *testing.T) {
	s := "内网测试账号"
	for n := 0; n <= len(s); n++ {
		got := truncateUTF8([]byte(s), n)
		if !strings.HasPrefix(s, got) {
			t.Fatalf("n=%d 잘라낸 결과 %q가 원문 접두사가 아님", n, got)
		}
		if len(got) > n {
			t.Fatalf("n=%d 잘라낸 후 %d바이트로 상한 초과", n, len(got))
		}
	}
	if got := truncateUTF8([]byte("abc"), 10); got != "abc" {
		t.Fatalf("상한보다 짧으면 원문 그대로 반환해야 함, 실제 %q", got)
	}
}

// TestIsBinaryBody documents the classification: declared binary types, and the
// NUL backstop for anything mislabeled.
func TestIsBinaryBody(t *testing.T) {
	cases := []struct {
		ct   string
		body string
		want bool
	}{
		{"application/json", `{"a":1}`, false},
		{"text/html; charset=utf-8", "<html>", false},
		{"application/sql", "-- dump", false},
		{"", "plain text", false},
		{"image/png", "whatever", true},
		{"application/zip", "PK", true},
		{"APPLICATION/PDF", "%PDF", true},
		{"text/plain", "has\x00nul", true},
	}
	for _, c := range cases {
		if got := isBinaryBody(c.ct, []byte(c.body)); got != c.want {
			t.Errorf("isBinaryBody(%q, %q)=%v, 기대값 %v", c.ct, c.body, got, c.want)
		}
	}
}

// TestRecordConcurrent exercises the write path under contention: ids stay
// unique and every exchange lands in all three tables.
func TestRecordConcurrent(t *testing.T) {
	tr, _ := openTraffic(t)
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			tr.record(newFlow("api.example.com", "GET", fmt.Sprintf("/item/%d", i),
				nil, fmt.Appendf(nil, `{"id":%d}`, i)))
		})
	}
	wg.Wait()
	var exchanges, bodies int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&exchanges); err != nil {
		t.Fatal(err)
	}
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchange_bodies`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if exchanges != n || bodies != n {
		t.Fatalf("동시 쓰기 후 exchanges=%d bodies=%d, 각각 %d여야 함", exchanges, bodies, n)
	}
}
