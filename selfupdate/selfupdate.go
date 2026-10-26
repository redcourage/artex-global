// Package selfupdate는 ARTEX 페이지의 원클릭 업데이트를 구현한다. GitHub Release에서 새
// 바이너리를 내려받아 검증·임시 저장하고 다음 시작 시 원자적으로 교체한다.
//
// 역할 분담(start.sh / start.bat 참고):
//
// 시작 스크립트 = 단순 감시 루프. 프로세스 종료 코드에 따라 다시 실행할지만 결정한다.
// 이 패키지 = 오류가 나기 쉬운 모든 로직(다운로드 / SHA256 검증 / 스모크 테스트 / 교체 / 실패 시 롤백).
//
// 교체를 스크립트 대신 Go에서 수행하는 이유는 SHA256 검증과 스모크 테스트를 sh와 bat에서
// 각각 구현해야 하기 때문이다(sha256sum / shasum / certutil). 실행할 수 없는 바이너리로 교체하는
// 오류는 특히 치명적이다. 감시 프로세스가 계속 재실행하면 사용자가 직접 서버에서 복구해야 한다.
//
// 전체 업그레이드는 프로세스 시작 세 번을 거친다.
//
// ① 기존 server가 /api/update/apply 수신 → 다운로드·검증 → artex.new 임시 저장 → exit 75
// ② 스크립트가 기존 버전 재실행 → Bootstrap이 artex.new 발견 → 검증+스모크 테스트 → 교체 → exit 75
// ③ 스크립트가 새 버전 실행 → Bootstrap이 시도 1회 기록 → 시작 성공 후 표식 삭제
//
// 어느 단계든 실패하면 기존 버전으로 돌아간다. ②에서 검증 실패 시 임시 파일을 삭제하고 기존 버전을 계속 실행한다.
// ③에서 표식을 지우기 전에 연속 3회 충돌하면 artex.old를 자동으로 복원한다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart는 '감시 프로세스가 다시 실행해 달라'는 종료 코드(EX_TEMPFAIL)다. 시작 스크립트는
// 충돌 백오프 없이 즉시 재실행한다. 0은 사용자 정상 종료(루프 종료), 나머지는 충돌로 처리한다.
const ExitRestart = 75

// maxAttempts는 교체 후 허용되는 시작 시도 횟수다. 새 버전은 시작할 때마다 1을 더하고
// settleDelay 동안 살아 있으면 표식을 지운다. maxAttempts번 연속 충돌하면 자동 롤백한다.
const maxAttempts = 3

// Paths는 업그레이드 관련 파일을 모두 **실행 파일이 있는 디렉터리** 아래에 둔다.
// 의도적으로 CWD를 쓰지 않는다. 서비스 실행의 작업 디렉터리는 / 또는 임의 경로일 수 있어
// CWD를 쓰면 임시 파일이 다른 곳에 생기고 교체 로직이 작동하지 않는다.
type Paths struct {
	Dir     string // 실행 파일이 있는 디렉터리
	Current string // 현재 실행 중인 바이너리 artex / artex.exe
	New     string // 임시 저장한 새 버전 artex.new / artex.new.exe
	Sum     string // 새 버전 sha256(hex) artex.new.sha256 / artex.new.exe.sha256
	Old     string // 교체 전 백업한 기존 버전 artex.old / artex.old.exe
	Marker  string // 업그레이드 상태 표식 artex.upgrade.json
}

// ResolvePaths는 현재 실행 파일에서 모든 업그레이드 경로를 도출한다.
//
// Windows의 .new/.old에도 .exe 확장자가 있어야 스모크 테스트와 교체 후 실행이 성공한다.
// 따라서 확장자를 먼저 제거하고 조합하여 두 플랫폼의 명명 방식을 맞춘다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 확인 실패: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows에서는 ".exe", Unix에서는 보통 빈 문자열이다.
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker는 교체 진행 상황을 기록하고 새 버전이 시작되지 않을 때 자동 롤백을 유발한다.
type marker struct {
	From     string `json:"from"`     // 업그레이드 전 버전
	To       string `json:"to"`       // 대상 버전
	Attempts int    `json:"attempts"` // 교체 후 시작 시도 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged는 임시 파일을 지운다. 교체 성공, 검증 실패, 사용자 취소 시 모두 호출하여
// 남은 artex.new가 다음 시작 때 다시 시도되지 않도록 한다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions는 두 버전을 비교해 -1/0/1(a<b / a==b / a>b)을 반환한다.
// ok=false는 한쪽 이상이 비교할 수 없는 버전임을 뜻한다(예: 로컬 개발 빌드 "dev" 또는
// git describe의 "0.3.7-2-gabc1234-dirty"). 호출자는 이때 원클릭 업데이트를 비활성화해야 한다.
// 그렇지 않으면 개발 빌드를 정식 버전으로 '업그레이드'하여 미커밋 변경을 덮어쓸 수 있다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion은 "v0.3.7" / "0.3.7" 형식의 버전을 [3]int로 파싱한다.
//
// 순수한 세 부분 형식만 허용한다. build.sh는 태그가 없는 빌드에서 git describe로
// "0.3.7-2-gabc1234" 같은 접미사가 있는 버전을 만든다. 이를 0.3.7로 취급하지 않고
// 비교 불가로 판정해야 개발 빌드를 '최신'으로 오인하거나 정식 버전으로 덮어쓰지 않는다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker는 프로세스가 컨테이너에서 실행 중인지 반환한다. Docker에서 교체는 컨테이너 쓰기 계층을 바꾸며,
// `docker compose up -d`로 컨테이너를 재생성하면 이미지에 포함된 버전으로 돌아간다. 이는 예상된 동작이지만
// (사용자가 새 이미지를 가져오는 경우이므로), 프런트엔드에서 이 점을 명확히 안내해야 한다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
