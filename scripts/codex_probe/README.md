# ARTEX → Codex 구독 통합 실험

기존 ARTEX 실행 엔진과 PostgreSQL을 사용하면서, LLM 추론을 로컬
`codex app-server`에 연결하는 실험입니다. 기본 제품에 추가된 정식 provider는
아닙니다. Python 표준 라이브러리만 사용합니다.

```text
ARTEX Goals / Planner / Workers
  → 로컬 Chat Completions 변환 서버
  → codex app-server (기존 ChatGPT 로그인)
  → 도구 호출을 ARTEX로 반환
  → ARTEX가 도구 실행·DB 기록
  → 결과를 같은 Codex thread/turn에 전달
```

에이전트마다 터미널을 만드는 방식이 아닙니다. App Server 프로세스 하나에
여러 thread를 만들고 `X-Artex-Codex-Session` 헤더로 구분합니다. Worker는
intent별로 분리됩니다. 도구 호출 왕복과 후속 사용자 메시지는 같은 thread를
사용합니다. ARTEX Planner가 새 문맥으로 다음 계획 라운드를 시작하면 새 thread를
만듭니다. Worker 사이의 결과 공유는 ARTEX의 사실·작업 DB와 도구를 통해 이뤄집니다.

## 실행

macOS/Linux, Python 3.10+, Docker, Go, ChatGPT 계정으로 로그인한 Codex CLI가
필요합니다. 설치된 CLI의 experimental App Server 인터페이스에 의존합니다.
통합 실행은 **Codex 구독 사용량을 소비**합니다. 외부 API 키는 필요하지 않습니다.
`ARTEX_BRIDGE_TOKEN`은 로컬 연결 보호용 임의 문자열이며 OpenAI API 키가 아닙니다.

레포 루트에서:

```sh
codex login status
python3 -m unittest discover -s scripts/codex_probe -v
go build -o /tmp/artex-codex-integration-bin ./cmd/artex
python3 scripts/codex_probe/run_local.py --binary /tmp/artex-codex-integration-bin
```

러너는 로그인 계정의 `model/list`를 조회하고, 모델 인자가 없으면 계정 기본 모델
(없으면 목록 첫 모델)을 두 역할에 사용합니다. 명시한 모델이 목록에 없으면 DB나
ARTEX를 띄우기 전에 실패합니다. 서로 다른 모델을 지정하려면 목록에 있는 ID를
전달합니다. 다음 ID는 테스트 계정의 예시이며 모든 계정에 제공된다는 뜻은 아닙니다:

```sh
python3 scripts/codex_probe/run_local.py \
  --binary /tmp/artex-codex-integration-bin \
  --planner-model gpt-6.1-sol \
  --worker-model gpt-6-luna \
  --timeout 360
```

러너는 일회용 PostgreSQL 컨테이너, 로컬 HTTP fixture, 변환 서버, ARTEX를
실행합니다. 별도 DB에 LLM 프로필과 역할별 모델을 설정하고 실제 작업을 제출합니다.
대상은 생성된 loopback fixture이며 기존 ARTEX 설정이나 DB를 사용하지 않습니다.
종료 시 자신이 띄운 프로세스와 컨테이너를 정리합니다. SIGKILL·전원 종료 시에는
정리할 수 없습니다. 강제 종료 후 남은 `artex-codex-probe-*` 컨테이너는 별도로 확인하세요.

## 검증하는 작업

1. Planner가 `/alpha`, `/beta` 조회를 별도 Worker intent로 만듭니다.
2. Worker들이 실제 응답의 숫자와 무작위 마커를 사실로 기록합니다.
3. 후속 Worker가 앞선 사실을 읽고 합계 및 두 마커를 기록합니다.
4. Planner가 목표를 증명하고 ARTEX 작업이 `done`으로 끝납니다.

출력된 `ARTIFACT_DIR`에 `checks.json`, `evidence.json`, `report.md`,
`fixture-requests.json`, 서비스 로그, `bridge/bridge.jsonl`이 남습니다.
`evidence.json`에는 실제 ARTEX DB의 intent·fact·goal과 도구 실행 기록이 있습니다.
자동 검사와 함께 이 기록을 확인하면 워커별 조회와 후속 사실 조회를 검토할 수 있습니다.
`report.md`는 ARTEX의 기본 취약점 보고서이므로 이 기능 실험의 상세 증거는
`evidence.json`에서 확인해야 합니다. 결과와 로그는 로컬에만 보관하세요.

## 사용량과 비용

Codex의 `thread/tokenUsage/updated` 누적값에서 증가분만 추출합니다. 각 추론에
고유 ID를 붙여 로컬 응답 헤더 `X-Artex-Usage`와 표준 `usage`로 전달합니다.
도구 호출 이벤트보다 사용량 이벤트가 늦게 올 수 있어, 도구 왕복 중에는 집계를
보류하고 turn이 끝날 때 모아 둔 추론별 사용량을 전달합니다. 추론 중 오류가 나도
그때까지 보고된 사용량은 오류 응답에 포함합니다. HTTP 타임아웃 뒤 늦게 발생한
실패의 사용량도 세션에 보존하여 복구 응답에 포함합니다. 보고되지 않은 시도는
고유 `missing_ids`로 표시하므로 정상 재시도가 앞선 누락을 0원으로 덮지 않습니다.
ARTEX는 모델·역할·세션·입력·출력·캐시 읽기/쓰기·추론 토큰을 사용량 원장에
기록하며, 동일한 추론 ID가 재전송되어도 원장에 한 번만 저장합니다.
추론 토큰은 출력 토큰의 일부이며 비용에 다시 더하지 않습니다.

작업 상세의 모델별 토큰 표 아래에 API 환산 예상 비용, 캐시 미사용 기준 비용,
캐시 읽기 절감액, 캐시 쓰기 추가 비용, 순절감액을 표시합니다. 순절감액은
캐시 쓰기 비용이 더 크면 음수일 수 있습니다. 미보고 사용량·미등록 단가는
0원으로 처리하지 않고 계산 불가 또는 부분 합계로 표시합니다.

계산은 실제 청구액이 아닌 **Standard API 단가 기준 추정**입니다. Codex 구독
요금이나 구독 잔여 한도를 토큰 비용으로 환산하지 않습니다. 기본 단가의 기준일,
지원 모델, 임의 모델 단가 파일 설정은 [pricing/README.md](../../pricing/README.md)를
참고하세요. 신규 기록은 계산한 비용과 단가 출처를 스냅샷으로 저장합니다.
과거 기록에 스냅샷이 없으면 보고된 토큰을 현재 설정 단가로 다시 추정합니다.
긴 문맥 할증은 전체 작업 합계가 아닌 개별 추론의 입력 길이에 적용합니다.

러너는 추가로 Codex의 최종 누적 사용량과 DB 합계의 일치, 저장된 비용 스냅샷과
모델별 집계의 일치, 캐시 토큰을 확인합니다. 단가가 없는 계정 모델도 명시적인
미계산 상태로 검증할 수 있습니다. 프로세스가 죽기 전에 Codex가 보고하지 못한
사용량이나, 브리지까지 종료되어 전달하지 못한
경우에는 ARTEX에 누락으로 표시될 수 있으므로 이를 완전한 청구 원장으로
사용하면 안 됩니다.

## 오류 복구와 수명 관리

- 입력 오류는 400, 이력·카탈로그 충돌과 결과가 불확실한 도구는 409입니다.
  일시적 연결·서버 오류는 503과 `Retry-After: 1`을 반환합니다. 인증·정책·문맥
  한도·구독 한도 오류를 일시 장애로 분류하지 않습니다.
- HTTP 응답 대기 시간이 지나도 진행 중인 추론과 도구 전달 상태는 유지합니다.
  동일 요청의 재시도는 그 실행을 기다리며, 성공 응답은 캐시에서 재전송합니다.
  다른 요청이 같은 세션의 진행 중 요청을 덮어쓸 수 없습니다.
- Codex 프로세스가 종료되면 health는 503이 됩니다. 다음 추론 요청에서 단일
  재시작을 수행하고, ARTEX가 보낸 이력을 새 임시 thread의 문맥으로 복원합니다.
  이는 기존 Codex 내부 추론 상태의 완전한 복원은 아닙니다.
- 복원 중 현재 사용자 턴에서 이미 완료한 동일 도구·인자 요청에는 기록된 결과를
  과거 결과라고 표시해 전달하며 ARTEX에 다시 실행시키지 않습니다. 새로운 도구
  작업을 노출하면 이 재사용 구간을 끝내므로, 쓰기 이후 검증용 읽기 등에 과거 값을
  재사용하지 않습니다. 이후 사용자 턴이 요청하는 새 실행도 허용합니다.
  동일 도구·인자만으로 의도적 반복과 재시도를 완벽히 구분할 수는 없습니다.
  호출 ID에 대응하는 결과가 없는 이력은 추측해서 재실행하지 않고
  409로 거절합니다. 외부 시스템 전체의 exactly-once 실행을 보장하지 않습니다.
- 비JSON 줄과 잘못된 알림은 내용 없이 오류 종류만 기록하고 다음 줄을 읽습니다.
  EOF·치명적 I/O·RPC 타임아웃은 연결을 비정상으로 표시하여 복구하도록 합니다.
- thread는 기본적으로 `ephemeral: true`입니다. 완료 후 유휴 세션과 클라이언트가
  돌아오지 않은 요청은 기본 1,800초 후 interrupt/unsubscribe하고 메모리에서
  제거합니다. 기본 최대 세션은 256개이며, 활성 세션만 남으면 503으로 용량을
  제한합니다. lock은 마지막 요청·대기자가 빠질 때 제거합니다.
- 브리지 직접 실행 옵션 `--idle-ttl`, `--max-sessions`, `--response-timeout`으로
  위 수치를 조정할 수 있습니다. 정리 주기는 최대 30초입니다. 세션 퇴거 또는
  브리지 재시작 뒤에는 클라이언트가 완전한 이력과 도구 결과를 다시 보내야 합니다.
- 도구 전에 나온 텍스트는 해당 도구 응답에, 이후 텍스트는 후속 응답에 전달합니다.
  병렬 도구의 결과는 호출 ID로 대응하고, 미등록·중복 호출은 거절합니다.

## 범위와 한계

- 텍스트, 함수 도구, `streaming=false`만 지원합니다. 도구는 ARTEX가 실행합니다.
- 같은 세션의 모델·도구 목록 변경과 기존 이력 재작성은 지원하지 않습니다.
  Planner의 새로운 사용자 문맥은 명시적으로 새 thread로 처리합니다.
- 세션 매핑·응답 캐시·미전달 사용량은 메모리에 있습니다. 브리지 자체 종료 후
  Codex 내부 상태나 아직 전달되지 않은 사용량까지 복원하는 영속 원장은 없습니다.
- ARTEX 자체의 이력 압축, 결과를 잃어버린 외부 도구의 재실행, 자동 API 전환, 구독 제한 도달,
  수시간 실행은 검증 범위 밖입니다.
- 이번 실험은 기본 모델 설정과 비스트리밍 프로필을 사용합니다. 임의의 Chat
  Completions 옵션을 모두 지원하는 API 호환 서버로 사용하면 안 됩니다.
- 테스트 기준 CLI는 `codex-cli 0.163.0`입니다. CLI·모델 가용성이 바뀌면 재검증이 필요합니다.
- 임시 thread 설정은 ARTEX DB·실험 산출물·기존 Codex 세션 파일을 삭제하지 않습니다.
  과거 `ephemeral: false` 실험의 rollout은 그대로 남아 있습니다.
- [공식 App Server 문서](https://learn.chatgpt.com/docs/app-server)는 제품 통합을
  안내하며, [인증 문서](https://learn.chatgpt.com/docs/auth)는 구독 인증과 API 키를
  구분합니다. 이 실험은 특정 운영 방식의 약관 적합성이나 계정 한도를 보증하지
  않습니다. CLI의 실험적 인터페이스 변경 시 재검증해야 합니다. 근거 없는 차단
  전례나 인증 우회 주장으로 표현하지 않습니다.
- 원래 리뷰와 수정 검증은 [리뷰 검증 기록](../../docs/superpowers/reviews/2026-10-16-codex-bridge.md)에 있습니다.
