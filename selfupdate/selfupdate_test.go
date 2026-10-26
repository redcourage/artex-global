package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths는 격리된 업그레이드 디렉터리를 만든다. ResolvePaths()를 직접 쓰면 테스트
// 바이너리 자체를 가리켜 go test 실행 파일의 이름을 바꾸게 되므로 사용할 수 없다.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin은 artex를 흉내 내는 실행 가능한 셸 스크립트를 쓴다. smokeTest는 -h로 실행해 종료 코드만 확인하므로
// 스크립트로 충분하며 실제 바이너리를 컴파일하는 것보다 훨씬 빠르다.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("가짜 바이너리 %s 쓰기: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage는 bin을 '임시 저장 후 교체 대기' 상태로 만든다. artex.new와 체크섬을 기록한다.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("체크섬 계산: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("체크섬 쓰기: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s 읽기: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("가짜 바이너리는 sh 스크립트이므로 Windows에서 실행할 수 없음")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh는 v를 제거하고 태그에는 v가 있으므로 둘 다 인식해야 한다.
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 사전순이 아닌 숫자로 비교한다.
		{"1.0.0", "0.99.99", 1, true},
		// 개발 빌드는 비교 불가로 판정하여 정식 버전이 미커밋 변경을 덮어쓰지 않게 한다.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, 기대값 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, 기대값 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 핵심 불변식: 모든 업그레이드 파일은 실행 파일과 같은 디렉터리에 있어야 한다. CWD에 두면 서비스 실행 시
	// 작업 디렉터리가 /일 수 있으므로 교체가 완전히 실패한다.
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s가 실행 파일 디렉터리에 없음: %s (기대값 %s)", name, path, p.Dir)
		}
	}
	// Windows의 .new/.old에는 .exe가 있어야 스모크 테스트와 교체 후 실행이 성공한다.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows의 .new/.old는 .exe로 끝나야 함: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// 체크섬 기록 후 파일을 수정해 다운로드 손상 / 파일 바꿔치기를 모사한다.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 불일치로 거부되어야 하는데 통과함")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 실행은 가능하지만 종료 코드는 0이 아니다.

	if err := verifyStaged(p); err == nil {
		t.Fatal("스모크 테스트 실패로 거부되어야 하는데 통과함")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("표식 쓰기: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart 기대, 실제 %v", action)
	}
	if !st.Pending {
		t.Error("교체 후 상태는 Pending이어야 함")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex가 새 버전으로 교체되어 있어야 함")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("기존 버전이 artex.old에 백업되어 있어야 함")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("교체 후 artex.new가 없어야 함")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("교체 후 체크섬 파일이 정리되어 있어야 함")
	}
	// 다음 시작(새 버전 실행)에서 횟수를 세고 필요하면 롤백하도록 표식은 남겨야 한다.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("교체 후 업그레이드 표식을 보존해야 함")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // 체크섬을 깨뜨린다.

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("검증 실패 시 Continue 기대, 실제 %v", action)
	}
	if !st.FailedStage {
		t.Error("상태는 FailedStage여야 함")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("검증 실패 시 현재 버전을 절대로 변경하면 안 됨")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("검증 실패한 임시 파일을 정리해야 다음 시작 때 재시도하지 않음")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 이전 업그레이드에서 남긴 백업
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3로 교체되어 있어야 함")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("백업은 방금 교체된 v2로 갱신되어야 함")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 처음 maxAttempts번 시작은 횟수만 누적해 새 버전이 안정화될 기회를 준다.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d번째 시도에서 Continue 기대, 실제 %v", i, action)
		}
		if !st.Pending {
			t.Errorf("%d번째 시도에서 상태는 Pending이어야 함", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d번째 시도 후 attempts=%d(ok=%v), 기대값 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// 한 번 더 충돌하면 상한을 넘어 기존 버전을 자동 복원한다.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("시도 상한 초과 시 Restart 기대, 실제 %v", action)
	}
	if !st.RolledBack {
		t.Error("상태는 RolledBack이어야 함")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("기존 버전으로 롤백되어 있어야 함")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("무한 롤백을 피하려면 롤백 후 표식을 지워야 함")
	}
	// 시작하지 못한 버전은 바로 삭제하지 않고 조사용으로 남긴다.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("실패 버전은 조사용으로 .failed에 보존해야 함")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback()은 ResolvePaths()를 사용하므로 여기서는 하위 교환 의미를 직접 검사한다.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("롤백 후 현재 버전은 v1이어야 함")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("다시 되돌릴 수 있도록 롤백 후 백업은 v2여야 함")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum 출력은 공백 두 개로 구분한다. shasum -a 256은 바이너리 모드에서 파일명에 *를 붙인다.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // 정확히 두 필드지만 첫 필드가 해시가 아니다.
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // 해시 길이가 잘못됐다.

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux 항목 파싱 오류: %v", out)
	}
	// 대소문자로 인한 불일치를 피하려면 해시를 소문자로 통일한다.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows 항목 오류(* 접두사 제거 및 해시 소문자 변환 필요): %q", got)
	}
	if len(out) != 2 {
		t.Errorf("빈 줄, 해시가 아닌 줄, 길이가 틀린 줄을 무시해야 함, 실제 %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 패키지 파일명은 artex.exe이며 이 사례는 Unix 명명 규칙으로 구성됨")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 실제 배포 패키지 구조: artex-<버전>-<os>-<arch>/artex와 일부 방해 파일.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("압축 해제된 파일이 artex 실행 파일이 아님: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("압축 해제된 바이너리에는 실행 비트가 있어야 함")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("패키지에 실행 파일이 없으면 오류를 반환해야 함")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS 아님
		"https://evil.com/artex.zip",    // 도메인이 허용 목록에 없음
		"https://github.com.evil.com/x", // 접미사 위장
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q)는 거부해야 함", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // 도메인은 대소문자를 구분하지 않음
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q)는 허용해야 하는데 오류 발생: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh의 package_binary는 artex-<버전>-<os>-<arch>.zip을 사용하며 버전의
	// v 접두사를 제거한다. 한 글자라도 틀리면 모든 플랫폼의 원클릭 업데이트가 자산을 찾지 못한다.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q 파싱: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("안정 동작 확인 후 업그레이드 표식을 지워야 함")
	}
	// 표식이 없으면 이후 정상 재시작에서 횟수를 누적하거나 롤백을 잘못 유발하지 않는다.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("표식 읽기가 실패해야 함")
	}
	// 사용자가 수동 롤백할 수 있도록 백업은 남긴다.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("안정 동작 확인 후에도 이전 버전 백업을 보존해야 함")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 일반 시작 경로이므로 panic이나 파일 변경이 없어야 한다.
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("표식이 없을 때 settle은 파일에 영향을 주면 안 됨")
	}
}
