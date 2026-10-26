package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv는 스모크 테스트가 시작한 자식 프로세스가 Bootstrap을 바로 건너뛰게 한다.
//
// 없어도 문제는 없을 수 있다. 자식의 os.Executable()은 artex.new이므로 도출된
// 모든 경로에 .new 접두사가 붙어 실제 업그레이드 파일과 겹치지 않는다. 하지만 이런 우연에 의존하기보다
// 명시적으로 우회하면 의도가 분명하고 자식 프로세스의 불필요한 디스크 조회도 줄일 수 있다.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action은 Bootstrap이 main에 전달하는 지시다.
type Action int

const (
	// Continue: 평소대로 server를 시작한다.
	Continue Action = iota
	// Restart: 즉시 ExitRestart로 종료하여 감시 스크립트가 다시 실행하게 한다.
	Restart
)

// State는 이번 시작의 업그레이드 상태를 나타내며 /api/update/check가 프런트엔드에
// '지난 업그레이드가 성공했는지 롤백됐는지'를 정확히 알리는 데 사용한다.
type State struct {
	Pending     bool   // 교체 후 안정 동작을 아직 확인하지 않음
	RolledBack  bool   // 이번 시작에서 자동 롤백을 방금 수행함
	FailedStage bool   // 임시 파일의 검증/스모크 테스트 실패로 폐기함
	Detail      string // 사용자에게 보여 줄 한 문장 설명
}

// Bootstrap은 main의 맨 처음, 포트 수신이나 DB 열기 전에 호출해야 한다.
//
// 세 가지 상황:
//
// ① artex.new 있음 → 검증+스모크 테스트 후 성공하면 교체하고 재시작 요청, 실패하면 폐기하고 기존 버전 실행
// ② 표식 파일만 있음 → 교체 직후이므로 시도 횟수를 누적하며 연속 실패가 충분하면 롤백
// ③ 아무것도 없음 → 정상 시작
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 부트스트랩 건너뜀: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged는 '임시 파일이 있음'을 처리한다. 검증 성공 시 교체하고 실패 시 폐기한다.
//
// 전체 업그레이드 경로에서 실행 파일을 덮어쓰는 유일한 곳이자 마지막 관문이다. 스모크 테스트가
// 다운로드 손상, 아키텍처 선택 오류, 동적 링크 누락을 차단한다. 실행 불가능한 바이너리를 통과시키면
// 감시 스크립트가 계속 재실행하고 Go 코드가 실행될 기회도 없어 자동 롤백도 불가능하다.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 임시 저장한 새 버전이 검증에 실패해 폐기했습니다. 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전 검증에 실패해 폐기했습니다: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 교체 실패, 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "버전 교체에 실패했습니다: " + err.Error()}
	}

	// 교체 성공. 표식은 다음 시작(새 버전 실행)에서 안정성을 확인하도록 남긴다.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표식 기록 실패(자동 롤백 불가): %v", err)
	}
	log.Printf("[update] %s(으)로 교체했습니다. 재시작을 위해 종료합니다(exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback은 교체 후 시작을 처리한다. 시도 횟수를 누적하고 상한 초과 시 기존 버전을 복원한다.
//
// Go 코드가 실행된 뒤에만 횟수를 증가시키므로 '실행은 되지만 초기화 중 충돌하는'
// 오류(설정 비호환, 포트 점유, DB 마이그레이션 실패)를 다룬다. 'exec 자체가 불가능한' 경우는 교체 전
// 스모크 테스트가 차단하며 두 방식이 합쳐져야 완전하다.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// 롤백도 실패하면 무한 재시작을 피하기 위해 다시 시작하지 않는다. 표식을 지우고
			// 현재 상태로 프로세스를 시작하여, 실패하더라도 사용자가 로그에서 이유를 확인할 수 있게 한다.
			log.Printf("[update] 새 버전이 연속 %d회 시작에 실패했고 롤백도 실패했습니다: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전 시작에 실패하고 롤백도 실패했습니다: " + err.Error()}
		}
		log.Printf("[update] 새 버전이 연속 %d회 시작에 실패하여 %s(으)로 롤백했습니다. 재시작을 위해 종료합니다(exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전 시작에 실패해 이전 버전으로 롤백했습니다(%s)", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표식 갱신 실패: %v", err)
	}
	log.Printf("[update] 새 버전 시작 중(%d/%d번째 시도). 안정 동작 후 업그레이드를 확인합니다",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle은 새 버전의 안정 동작을 확인하고 업그레이드 표식을 지운다.
//
// main이 HTTP 수신을 시작한 뒤 지연 호출한다. 이 기간을 살아남아야 인정하며 그렇지 않으면 표식을 남겨
// 다음 시작에서 시도 횟수를 계속 누적하다 롤백을 유발한다.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 업그레이드 후 시작이 아니므로 할 일이 없다.
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 업그레이드 표식 삭제 실패: %v", err)
		return
	}
	log.Printf("[update] 새 버전이 안정적으로 실행되어 업그레이드를 완료했습니다(이전 버전은 %s에 보존)", p.Old)
}

// SettleDelay는 새 버전이 살아남았다고 판단하는 데 필요한 실행 시간이다.
const SettleDelay = 30 * time.Second

// verifyStaged는 SHA256을 비교한 뒤 실제 실행하여 임시 파일을 검증한다.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기 실패: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산 실패: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 이 일치하지 않습니다(다운로드가 손상됐거나 변조됨)")
	}
	return smokeTest(p.New)
}

// smokeTest는 새 바이너리를 -h로 실행하여 현재 시스템에서 실제 실행 가능한지 확인한다.
// 다운로드 잘림, 아키텍처 오류(exec format error), 의존성 누락 등 여러 문제를 차단한다.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여 실패: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리가 응답하지 않음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap은 현재 바이너리를 임시 저장한 새 버전으로 교체한다.
//
// Unix와 Windows 모두 실행 중인 실행 파일의 rename을 허용한다(Windows는 삭제와
// 덮어쓰기를 금지하지만 rename은 아님). 따라서 플랫폼 분기나 자기 프로세스 중지가 필요 없다.
func swap(p Paths) error {
	// Windows의 rename은 기존 대상을 덮어쓰지 않으므로 지난 업그레이드의 .old를 먼저 지워야 한다.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("이전 백업 %s 정리 실패: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업 실패: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 교체는 실패했지만 현재 버전을 이미 옮겼으므로 원래 위치에 돌려놔야 다음 시작에 실행 파일이 존재한다.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전 설치 실패(%v), 현재 버전 복구도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 설치 실패: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback은 swap이 백업한 기존 버전을 복원한다.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("롤백할 백업이 없습니다(%s): %w", p.Old, err)
	}
	// 실행되지 않는 새 버전은 바로 삭제하지 않고 .failed로 옮겨 조사에 남긴다.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 이동 실패: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("이전 버전 복구 실패: %w", err)
	}
	return nil
}

// Rollback은 /api/update/rollback의 구현이며 이전 버전으로 능동적으로 돌아간다.
// 교체만 수행하고 재시작은 감시 스크립트에 맡긴다(호출자가 이후 ExitRestart로 종료).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("롤백할 이전 버전이 없습니다(" + p.Old + " 없음)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 롤백을 거부합니다: %w", err)
	}
	// 현재 버전과 백업을 교환하여 롤백 후 다시 돌아갈 수도 있게 한다.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 이동 실패: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 설치 실패: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] 롤백 후 백업 정리 실패(실행에는 영향 없음): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup은 롤백 가능한 이전 버전이 있는지 반환하며 프런트엔드의 롤백 버튼 표시 여부에 사용한다.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
