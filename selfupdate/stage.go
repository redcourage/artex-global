package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset은 release.yml이 만드는 체크섬 목록이며 Release의 모든 zip을 포함한다.
const sumsAsset = "SHA256SUMS"

// maxBinarySize는 비정상 zip이 디스크를 채우지 않도록 압축 해제된 바이너리 크기를 제한한다.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase는 업그레이드 단계이며 SSE 이벤트의 phase 필드에 그대로 사용한다.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress는 호출자가 제공하며 진행률을 프런트엔드로 전달한다. pct는 다운로드 중에만 유효하며(0-100),
// 다른 단계에서는 -1을 전달한다.
type Progress func(ph Phase, pct int, msg string)

// Stage는 지정 Release에서 현재 플랫폼 패키지를 내려받고 검증 후 새 바이너리를 artex.new로 임시 저장한다.
//
// 개별 바이너리 대신 전체 zip을 사용하는 이유는 두 가지다. 기존 Release의 SHA256SUMS가
// zip만 포함하므로 CI 변경 없이 이미 배포된 버전과 호환되며, zip에는
// skills/도 있어 향후 내장 스킬 동기화에 활용할 수 있다. 추가 비용은 스킬 수백 KB 다운로드뿐이다.
//
// 함수가 반환되면 임시 저장이 완료된 것이며, 호출자는 정상 종료 절차 후 ExitRestart로 종료한다.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("이 버전은 %s/%s 용 릴리스 패키지를 제공하지 않습니다(%s 누락)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "체크섬 목록을 가져오는 중…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s 에 %s 항목이 없어 검증되지 않은 바이너리 설치를 거부합니다", sumsAsset, name)
	}

	// 임시 파일은 모두 대상 디렉터리에 두어 최종 rename이 같은 파일 시스템 안에서 원자적으로 이루어지게 한다.
	// (장치 간 rename은 실패하며 /tmp는 별도 마운트 지점인 경우가 많다.)
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s 내려받는 중(%s)…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 검증 중…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 이 일치하지 않습니다: 기대값 %s, 실제값 %s(다운로드가 손상됐거나 변조됨)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "압축을 풀고 스모크 테스트 중…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("새 버전이 현재 시스템에서 실행되지 않습니다: %w", err)
	}

	// 임시 저장 파일의 sha256을 별도로 보관해 다음 시작 시 교체 전에 다시 검증한다.
	// 임시 저장과 재시작 사이에 파일이 변조되거나 잘못 기록되는 경우를 방지한다.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("새 바이너리 체크섬 계산 실패: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("체크섬 기록 실패: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("새 버전 준비 실패: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// 표식은 자동 롤백에만 영향을 준다. 임시 파일은 준비됐으므로 업그레이드를 중단하지 않는다.
		prog(PhaseStaged, -1, "경고: 업데이트 표시 파일 기록에 실패해 이번 업데이트는 자동 롤백 보호를 받지 못합니다")
	}

	prog(PhaseStaged, 100, "새 버전이 준비되었습니다. 재시작 중…")
	return nil
}

// fetchSums는 SHA256SUMS를 내려받아 파싱하고 파일명 → 16진수 해시를 반환한다.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("이 릴리스에 %s 항목이 없어 무결성을 검증할 수 없으므로 업데이트를 거부합니다", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s 내려받기 실패: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s 읽기 실패: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 내용이 비어 있거나 형식을 인식할 수 없습니다", sumsAsset)
	}
	return out, nil
}

// parseSums는 sha256sum 형식의 목록을 파싱하고 파일명 → 16진수 해시를 반환한다.
//
// 첫 필드가 64자리 16진수인 경우만 포함한다. '정확히 두 필드'만 확인해서는 부족하다.
// 두 단어로 된 설명도 유효한 항목으로 오인하여 잘못된 값을 해시 표에 넣고,
// 실제 자산에 잘못된 해시가 대응될 수 있기 때문이다.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 형식은 "<sha256>  <filename>"이다(sha256sum은 공백 두 개, shasum의 바이너리
		// 모드는 파일명에 * 접두사를 붙인다).
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download는 자산을 dst에 쓰면서 SHA256을 계산하고 Content-Length 기준 진행률을 보고한다.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s 내려받기 실패: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("임시 파일 생성 실패: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("다운로드가 중단되었습니다: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("디스크 기록 실패: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("다운로드가 완전하지 않습니다: 기대 %d바이트, 실제 %d바이트", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get은 허용 목록의 제약을 받는 GET을 실행하고 응답 본문을 반환한다.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary는 배포 패키지에서 artex 실행 파일을 꺼낸다.
//
// 패키지 구조는 artex-<버전>-<os>-<arch>/artex이지만 전체 경로 대신 **파일명**으로 찾는다.
// 패키지명에 버전이 있어 한 글자만 틀려도 업그레이드가 실패하므로 파일명 매칭이 변경에 더 강하다.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("릴리스 패키지 열기 실패: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s 읽기 실패: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("새 바이너리 쓰기 실패: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s 압축 해제 실패: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("릴리스 패키지 안의 실행 파일이 %s 보다 커서 압축 해제를 거부합니다", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("릴리스 패키지 안의 %s 파일이 비어 있습니다", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("릴리스 패키지에서 %s 파일을 찾지 못했습니다", want)
}

// checkWritable은 디렉터리에 쓸 수 있는지 미리 확인한다. 이 검사가 없으면 비 root 실행이나 시스템
// 디렉터리에 둔 바이너리는 수십 MB를 내려받은 뒤 실제 교체 순간에야 실패한다.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("프로그램 디렉터리 %s 에 쓸 수 없어 자동 업데이트를 할 수 없습니다(권한을 확인하거나 수동 업데이트를 사용하세요): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter는 기록한 바이트를 집계하고 보고 빈도를 제한해 32KiB 청크마다 SSE를 보내지 않는다.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("내려받는 중 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
