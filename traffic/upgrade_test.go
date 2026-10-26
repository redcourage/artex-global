package traffic

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openLegacyIndex builds the index exactly as the pre-reclamation Open did: a
// plain-path DSN, pragmas via the pool, and auto_vacuum left at its default 0.
func openLegacyIndex(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "_index"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := old.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestUpgradeFromOldInstall guards the upgrade path. Open now names the database
// through a file: URI so per-connection pragmas can ride in the DSN, and a
// driver that did not treat that as a URI would quietly open a file literally
// named "file:/…" — an empty index, with every recorded exchange apparently
// gone. The assertions below are what prove that does not happen.
func TestUpgradeFromOldInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_index", "index.sqlite")
	old := openLegacyIndex(t, dir)
	if _, err := old.Exec(ftsSchema); err != nil {
		t.Fatal(err)
	}
	// 기존 트래픽 세 개. legacy path<>'' 행 하나를 포함한다.
	for i, row := range [][]any{
		{"1700000000-0001", "old.example.com", ""},
		{"1700000000-0002", "old.example.com", ""},
		{"1700000000-0003", "legacy.example.com", "legacy.example.com/GET/x"},
	} {
		if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,'GET','/x','http://x/x',200,'text/html',0,9,?)`, row[0], 1700000000+i, row[1], row[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,'GET /x','','HTTP 200','老数据正文')`, row[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, i+1, "老数据正文 secret-token"); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 새 버전에서 인계받음
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("새 버전이 기존 DB를 열 수 없음: %v", err)
	}
	defer tr.Close()

	// 1. 동일 파일이어야 하며 조용히 새 빈 DB를 열면 안 된다.
	if st2, err := os.Stat(path); err != nil || st2.Size() == 0 {
		t.Fatalf("원본 인덱스 파일 이상: size=%v err=%v", st2, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "_index")); len(entries) > 3 {
		for _, e := range entries {
			t.Logf("_index 아래: %s", e.Name())
		}
		t.Fatal("_index 아래 예상하지 않은 파일이 생김. DSN이 다른 DB를 가리킬 수 있음")
	}
	t.Logf("기존 DB %d바이트, 새 버전이 인계한 뒤에도 같은 파일", stat.Size())

	// 2. 기존 데이터가 모두 보여야 한다.
	n, err := tr.Count()
	if err != nil || n != 3 {
		t.Fatalf("Count=(%d,%v), 기대값 (3,nil) — 기존 트래픽 소실", n, err)
	}
	// 3. 기존 전문 인덱스도 검색 가능해야 한다.
	if tr.fts {
		rows, err := tr.query("old.example.com", "", "secret-token", 0, 10)
		if err != nil {
			t.Fatalf("기존 전문 검색 실패: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("기존 전문 검색 %d개 매칭, 기대값 2", len(rows))
		}
	}
	// 4. 기존 본문도 읽을 수 있어야 한다.
	if _, resp, err := tr.Get("1700000000-0001"); err != nil {
		t.Fatalf("기존 본문 읽기 실패: %v", err)
	} else if resp == "" {
		t.Fatal("기존 응답이 비어 있음")
	}
	// 5. 기존 DB를 증분 회수 활성화 상태로 오인하면 안 된다.
	if tr.incrementalVacuum {
		t.Fatal("기존 DB를 증분 회수 활성화 상태로 오인함")
	}
	// 6. 삭제가 정상 동작하고 기존 DB의 회수 과정도 수렴해야 한다.
	deleted, err := tr.DeleteHostsExact([]string{"old.example.com"})
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteHostsExact=(%d,%v), 기대값 (2,nil)", deleted, err)
	}
	tr.reaping.Wait()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("삭제 후 Count=(%d,%v), 기대값 (1,nil)", n, err)
	}
	// 7. legacy path<>'' 행이 영향을 받지 않아야 한다.
	var legacyPath string
	if err := tr.DB().QueryRow(`SELECT path FROM exchanges`).Scan(&legacyPath); err != nil {
		t.Fatal(err)
	}
	if legacyPath == "" {
		t.Fatal("legacy 행의 path가 비워짐")
	}
}

// TestDowngradeToOldBinary covers a rollback: a database created with
// auto_vacuum=incremental must stay readable and writable by a build that knows
// nothing about it. auto_vacuum only changes where SQLite tracks free pages, so
// the old binary simply goes back to never returning them.
func TestDowngradeToOldBinary(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.incrementalVacuum {
		t.Fatal("새 DB는 증분 회수가 활성화되어야 함")
	}
	bulkRecord(tr, "keep.example.com", 5, 100*1024)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	old := openLegacyIndex(t, dir) // 기존 버전 바이너리로 인계받음
	defer old.Close()
	var n int
	if err := old.QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("기존 버전이 읽은 값 (%d,%v), 기대값 (5,nil)", n, err)
	}
	if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('x',1,'new.example.com','GET','/x','http://x/x',200,'',0,0,'')`); err != nil {
		t.Fatalf("기존 버전 쓰기 실패: %v", err)
	}
	if _, err := old.Exec(`DELETE FROM exchanges WHERE host='keep.example.com'`); err != nil {
		t.Fatalf("기존 버전 삭제 실패: %v", err)
	}
}
