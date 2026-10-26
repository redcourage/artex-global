package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache는 IP당 시간당 60회인 비인증 GitHub API 할당량을 보호합니다.
// 상단 새 버전 안내는 전체 페이지를 읽을 때마다 조회하므로 캐시가 없으면 여러 탭만으로
// 할당량이 소진되어 실제 업데이트 때 조회하지 못합니다.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("조회 5회에서 원본 요청은 1회여야 합니다, 실제 %d회", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 명시적 업데이트 확인은 캐시 만료를 기다리지 않고 방금 배포한 버전을 볼 수 있어야 합니다.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force는 캐시를 우회해야 합니다, 원본 요청 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 저장 시각을 만료 직전보다 이전으로 옮겨 TTL 만료를 재현합니다.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 만료 후 원본을 다시 조회해야 합니다, 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github에 연결할 수 없습니다")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류를 반환해야 합니다")
	}
	// GitHub 연결 실패 때 페이지마다 시간 초과를 기다리지 않도록 실패도 잠깐 캐시합니다.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류를 반환해야 합니다")
	}
	if calls != 1 {
		t.Errorf("오류를 잠시 캐시해야 합니다, 원본 요청 기대 1회, 실제 %d회", calls)
	}

	// 오류 TTL은 성공보다 훨씬 짧아 네트워크 복구 후 빨리 회복해야 합니다.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("오류 TTL(%v)은 성공 TTL(%v)보다 짧아야 합니다", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류를 반환해야 합니다")
	}
	if calls != 2 {
		t.Errorf("오류 TTL 만료 후 재시도해야 합니다, 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 탭을 닫아 요청이 취소된 것은 GitHub 문제가 아니므로 취소 오류를
	// 캐시해 이후 30분간 모든 방문자에게 잘못된 오류를 보여서는 안 됩니다.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시를 만료시켜 원본 조회를 강제합니다.

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("호출자가 취소했으면 오류를 그대로 전달해야 합니다")
	}

	// 취소된 요청은 캐시에 취소 오류를 남기지 않고
	// 이전 정상 결과도 유지해야 합니다.
	if c.err != nil {
		t.Fatalf("취소 오류를 캐시에 저장해서는 안 됩니다, 결과 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("캐시는 이전 정상 결과를 유지해야 합니다, 결과 %+v", c.rel)
	}

	// 취소 요청은 새 데이터를 얻지 못했으므로 다음 방문자는 원본을 다시 조회해 정상 결과를 받아야 하며
	// 이전 취소의 영향을 받지 않아야 합니다.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("취소 이후의 정상 요청이 실패해서는 안 됩니다: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("정상 결과를 받아야 합니다, 결과 %+v", rel)
	}
}
