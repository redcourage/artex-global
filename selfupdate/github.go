package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo는 배포 소스다. 설정 항목 대신 하드코딩한다. 업데이트 소스를 설정 가능하게 하면 설정을 바꿀 수 있는 누구에게나
// 원격 코드 실행 경로를 주게 되므로 침투 테스트 플랫폼에서는 허용할 수 없다.
const Repo = "Autumn-27/artex"

// latestURL은 GitHub의 '최신 정식 버전' API이며 prerelease와 draft를 자동으로 제외한다.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts는 업그레이드 과정에서 접근할 수 있는 도메인을 제한한다. 아래 checkRedirect와 함께
// 어느 단계든 목록 밖 호스트로 리디렉션되면 즉시 실패한다. DNS 오염 / 중간자에 의한
// 바이너리 교체를 막는 첫 관문이며 두 번째는 SHA256SUMS 비교다.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 자산이 실제 저장되는 객체 저장소
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release는 GitHub Release에서 사용하는 필드다.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset은 Release에 첨부된 파일 하나다.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient는 GitHub 도메인만 허용하는 HTTP 클라이언트를 만든다. proxy가 비어 있으면 직접 연결한다.
//
// 기본 Transport를 의도적으로 재사용하지 않는다. 업그레이드 경로는 TLS와 인증서 검증을 강제해야 하며
// 다른 곳에서 설정한 InsecureSkipVerify 등에 영향을 받으면 안 된다.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 전체 패키지를 다운로드하므로 요청 단위 제한 시간으로 막지 않는다.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리다이렉트 횟수가 너무 많습니다")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL은 https와 도메인 허용 목록을 강제한다.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 가 아닌 주소를 거부합니다: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 도메인이 아닌 주소를 거부합니다: %s", u.Hostname())
	}
	return nil
}

// FetchLatest는 최신 정식 버전을 조회한다.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 에 접속하지 못했습니다(시스템 설정에서 전역 프록시를 설정할 수 있습니다): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 비인증 GitHub API는 IP당 시간당 60회이므로 출구 IP를 공유하면 쉽게 제한에 걸린다.
		return nil, fmt.Errorf("GitHub API 요청 제한에 걸렸습니다(시간당 60회). 잠시 후 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s 에 아직 정식 릴리스가 없습니다", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 응답 코드 %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 해석 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release 에 tag 가 없습니다")
	}
	return &rel, nil
}

// AssetName은 현재 플랫폼의 배포 패키지명을 반환하며 build.sh의 package_binary와 일치시킨다.
// artex-<버전>-<os>-<arch>.zip(버전에는 v 접두사가 없음).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset은 Release에서 이름으로 자산을 찾는다.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
