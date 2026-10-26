package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// 화면 원클릭 업데이트의 HTTP 계층입니다. 실제 다운로드/검증/교체는 selfupdate가 담당하고
// 여기서는 인증 경계, 동시 실행 배제, 진행 방송, main에 종료 요청 전달을 담당합니다.
//
// 새 버전 준비 후 selfupdate.ExitRestart로 종료하며 이 프로세스가 직접 재시작하지 않습니다.
// start.sh/start.bat 또는 Docker ENTRYPOINT의 감독 스크립트가 다시 실행합니다.

// 사용자에게 노출되는 업데이트 메시지(SSE 진행 메시지·검사 사유·에러 응답).
// 프런트엔드(system/settings 의 update-card)가 progress.message·reason·writeErr
// 본문을 그대로 렌더하므로 한국어로 둔다. 로그·중국어 주석은 Z2(로그 최하위)라 범위 밖.
const (
	updateMsgPreparing          = "준비 중…"
	updateMsgFailed             = "업데이트 실패"
	updateMsgStaged             = "새 버전이 준비되었습니다. 재시작 중…"
	updateErrNotReleaseFmt      = "현재 버전 %q 은(는) 정식 릴리스 버전이 아니므로 원클릭 업데이트를 사용할 수 없습니다"
	updateErrAlreadyLatestFmt   = "이미 최신 버전입니다 (%s)"
	updateErrInProgress         = "이미 업데이트가 진행 중입니다"
	updateErrRollbackInProgress = "업데이트가 진행 중이어서 롤백할 수 없습니다"
)

// restartCh는 업그레이드 준비/롤백 완료 후 닫히며 main이 받아 ExitRestart로 종료합니다.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested는 종료하고 감독 프로세스로 재실행해야 할 때 닫히는 채널을 반환합니다.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState는 이번 시작의 selfupdate.Bootstrap 결과(업그레이드 성공/방금 롤백/
// 임시 파일 폐기)이며 main이 주입하여 /api/update/check가 이전 업데이트 결과를 정확히 알리도록 합니다.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState는 시작 시 main이 한 번 호출합니다.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache는 GitHub 최신 버전 조회 결과를 캐시합니다.
//
// 상단 새 버전 알림은 전체 페이지를 읽을 때마다 조회하지만 비인증 GitHub API는
// IP당 시간당 60회 제한이므로 캐시가 없으면 여러 탭/새로 고침만으로 할당량이 소진되어
// 정작 업데이트 때 조회할 수 없습니다. 명시적 업데이트 확인은 force로 캐시를 우회합니다.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch는 테스트용 조회 주입점이며 nil이면 실제 GitHub를 조회합니다.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// GitHub에 연결되지 않을 때 페이지마다 시간 초과를 기다리지 않도록 실패도 잠깐 캐시하지만
	// 네트워크 복구 후 빠르게 회복하도록 TTL은 짧게 둡니다.
	releaseErrTTL = 2 * time.Minute
	// 버전 조회 제한 시간입니다. NewClient의 30분은 전체 다운로드용이라 조회에는 너무 깁니다.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get은 최신 Release를 반환하고 캐시가 있으면 네트워크에 접근하지 않습니다.
//
// 조회 중 잠금을 유지해 동시 요청이 같은 결과를 기다리도록 하며 각자 GitHub에 요청하지 않습니다.
// 여러 탭을 동시에 여는 순간이 속도 제한에 가장 취약합니다.
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// 탭을 닫는 등의 요청 취소는 GitHub 문제를 뜻하지 않으므로 캐시하지 않아
	// 다음 사용자가 이유 없는 취소 오류를 받지 않도록 합니다.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress는 프런트엔드에 전달할 진행 정보 하나입니다.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // 다운로드 단계에서만 의미가 있으며 그 외에는 -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub는 업그레이드 진행 상태를 보유하고 SSE 구독자에게 방송합니다.
//
// running은 상호 배제도 담당해 업그레이드 중 POST /api/update/apply에 409를 반환하고
// 두 goroutine이 같은 artex.new에 동시에 쓰지 못하게 합니다.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin은 업그레이드 권한을 획득하며 이미 실행 중이면 false입니다.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: updateMsgPreparing, Version: version}
	h.fanout(h.cur)
	return true
}

// finish는 업그레이드를 끝냅니다. err=nil이면 임시 저장에 성공하여 재시작을 기다립니다.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: updateMsgFailed, Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: updateMsgStaged, Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout은 h.mu를 잡고 호출해야 합니다. 구독자 채널은 버퍼가 있고 꽉 차면 버립니다.
// 진행 정보는 버려도 되는 순간 상태이므로 멈춘 SSE 연결이 업그레이드 자체를 막아서는 안 됩니다.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck는 GitHub 최신 정식 버전을 조회해 현재 버전과 비교합니다.
//
// 프런트엔드도 CORS=*인 api.github.com에 직접 연결하지만 이 API 결과를 기준으로 합니다.
// 실제 다운로드는 백엔드가 하므로 서버에서 GitHub에 접근할 수 있어야 업데이트할 수 있습니다.
// 내부망 서버나 브라우저에만 설정한 프록시 때문에 브라우저만 연결되는 경우가 흔하므로
// 업데이트 클릭 후 실패하기보다 확인 단계에서 바로 오류를 알립니다.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 상단 안내는 기본 캐시를 쓰며 사용자가 업데이트 확인을 누르면 force=1로 원본을 조회합니다.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// dev 또는 git describe 접미사가 있는 개발 빌드는 비교 가능한 버전이 없고
		// 업데이트하면 디버깅 중인 바이너리를 정식 버전으로 덮어쓰므로 허용하지 않습니다.
		out["reason"] = fmt.Sprintf(updateErrNotReleaseFmt, current)
	}
	writeJSON(w, 200, out)
}

// updateApply는 새 버전을 다운로드해 준비하고 프로세스를 종료해 감독 스크립트가 재시작하게 합니다.
//
// 즉시 202를 반환하고 백그라운드 goroutine에서 실행합니다. 전체 다운로드는 수분 걸릴 수 있어
// 요청에 묶으면 역방향 프록시 시간 초과로 끊깁니다. 진행은 /api/update/stream으로 제공합니다.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// 사용자가 화면에서 확인한 버전을 설치하도록 캐시를 사용합니다.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf(updateErrNotReleaseFmt, current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf(updateErrAlreadyLatestFmt, current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, updateErrInProgress)
		return
	}

	go func() {
		// HTTP 응답 후 요청 ctx는 종료되므로 s.ctx를 사용합니다.
		// 요청 ctx에 다운로드를 연결하면 즉시 취소됩니다.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 업데이트 실패: %v", err)
			return
		}
		log.Printf("[update] %s → %s 준비 완료, 교체를 완료하기 위해 곧 종료합니다", current, rel.TagName)
		// 마지막 진행 상태가 프런트엔드에 전달될 시간을 조금 준 뒤 종료합니다.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback은 교체 전에 백업한 artex.old로 수동 복귀합니다.
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, updateErrRollbackInProgress)
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 이전 버전으로 수동 롤백했습니다. 전환을 완료하기 위해 곧 종료합니다")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream은 업데이트 진행을 SSE로 전달합니다.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 새로 고침 후 진행 중인 업데이트가 바로 보이도록 현재 상태를 먼저 보냅니다.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
