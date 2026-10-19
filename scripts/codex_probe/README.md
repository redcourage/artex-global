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

Chat은 사용자 턴마다 ARTEX의 저장된 transcript를 다시 읽습니다. 이 과정에서
도구 인자 JSON의 공백·키 순서·유니코드 표현이 바뀌어도 같은 이력으로 비교하여
기존 Codex thread를 이어갑니다. 실제 인자·도구 결과가 바뀐 이력은 계속 거부합니다.
단, Norma가 오래된 도구 결과를 `[Old tool result content cleared]`로 줄인 경우는
정상적인 문맥 정리로 인식합니다. 기존 thread와 원래 관측 결과를 유지하므로
저장된 transcript로 돌아와도 같은 세션을 이어갈 수 있습니다.

## 실행

macOS/Linux, Python 3.10+, Docker, Go, ChatGPT 계정으로 로그인한 Codex CLI가
필요합니다. 설치된 CLI의 experimental App Server 인터페이스에 의존합니다.
통합 실행은 **Codex 구독 사용량을 소비**합니다. 외부 API 키는 필요하지 않습니다.
`ARTEX_BRIDGE_TOKEN`은 로컬 연결 보호용 임의 문자열이며 OpenAI API 키가 아닙니다.

레포 루트에서:

```sh
codex login status
python3 -m unittest discover -s scripts/codex_probe -v
python3 scripts/codex_probe/verify_compaction.py
go build -o /tmp/artex-codex-integration-bin ./cmd/artex
python3 scripts/codex_probe/run_local.py --binary /tmp/artex-codex-integration-bin
```

`verify_compaction.py`는 실제 Norma 라이브러리의 압축·요약 요청·OpenAI HTTP
변환을 브리지에 연결해 검증합니다. 합성 도구 결과와 결정적인 Codex 테스트 응답을
사용하며 외부 모델·로그인·DB가 필요하지 않습니다. 임시 localhost 포트와 Go가
필요합니다. 반복 MicroCompact, 요약 전용 세션 분리, 전체 요약 후 복구,
도구 결과 중복 전달 여부를 검사합니다. 실제 모델의 장시간 동작을 대신 검증하는
테스트는 아닙니다.

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

- Norma 0.4.3의 MicroCompact는 알려진 도구 결과의 정확한 정리 표식만 허용합니다.
  사용자 메시지·도구 인자·호출 ID·임의의 결과 변경은 허용하지 않습니다.
  병렬 도구 호출은 응답당 최대 5개씩 전달하여 Norma의 최근 결과 보존 개수와
  맞춥니다. 나머지 호출은 다음 왕복에 전달하며 실행 순서와 호출 ID를 유지합니다.
  아직 Codex에 전달하지 않은 결과가 이미 지워졌다면 정상 관측으로 처리하지
  않고 원본 결과의 복원을 요구합니다.
- 전체 요약을 생성하는 요청은 원래 워커와 분리된 도구 없는 thread에서 처리합니다.
  요청별 내부 세션 키는 재시도 시 동일하므로 동일 요약을 중복 실행하지 않습니다.
  요약 요청도 세션 TTL과 용량 제한, 사용량 집계를 적용받습니다.
- 요약 이후에는 Norma의 continuation 표식, 보존된 최근 이력, 미완료 도구의 결과를
  확인하고 이전 turn을 interrupt/unsubscribe한 뒤 새 ephemeral thread에 복원합니다.
  같은 ARTEX 세션 ID를 유지하며, 요약 뒤에 가려진 완료 도구 결과도 살아 있는
  브리지의 복구 구간에서는 재사용합니다. 결과가 없는 도구는 추측해서 재실행하지
  않습니다. 교체 전 보고된 사용량은 보존하고 중단된 추론의 미보고분은 누락으로 표시합니다.
  새 thread/turn 생성이 실패해도 검증한 이력과 완료 결과를 보존하여 같은 요청을
  재시도할 수 있습니다. 브리지 재시작 당시 지워진 결과만 받았더라도, 이후
  transcript Resume가 원본 결과를 보내면 검증 후 새 thread에 복원합니다.
- `history_compacted`, `summary_request`, `history_rebased` 로그로 정상적인 문맥
  정리를 확인할 수 있습니다. 알 수 없는 변경은 `history_conflict`에 세션·thread·
  메시지 개수·첫 불일치 위치만 기록합니다. 프롬프트나 도구 결과 원문은 기록하지 않습니다.
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
- 같은 세션의 모델·도구 목록·시스템 지시 변경과 임의의 이력 재작성은 지원하지 않습니다.
  Planner의 새로운 사용자 문맥은 명시적으로 새 thread로 처리합니다.
- 세션 매핑·응답 캐시·미전달 사용량은 메모리에 있습니다. 브리지 자체 종료 후
  Codex 내부 상태나 아직 전달되지 않은 사용량까지 복원하는 영속 원장은 없습니다.
- Norma 0.4.3의 기본 MicroCompact/AutoCompact를 지원합니다. 실험적 Noa 압축은
  이 호환성 테스트 범위에 포함되지 않습니다. Norma의 요약 표식·프로토콜이 바뀌면
  재검증해야 합니다. 브리지가 재시작되면 요약에 사라진 도구의 원본 결과까지 복원할 수는 없습니다.
- 실제 구독 한도를 소진하는 실험과 수시간 실행은 검증 범위 밖입니다.
  한도 소진/회복은 결정적인 가짜 Codex 응답으로 검증합니다. 결과를 잃은 외부
  도구의 자동 재실행과 구독 한도 때문에 유료 API로 자동 전환하는 동작은 허용하지 않습니다.
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

## 구독 한도 대기와 자동 재개

Codex의 `usageLimitExceeded`는 전용 `423 / codex_subscription_limit`로 전달합니다.
일반 429 재시도나 API 잔액 부족에 따른 프로필 전환과 구분합니다. ARTEX는 작업의
대기 상태를 PostgreSQL `task_subscription_waits`에 저장하고 Planner/Worker의 새
실행을 막습니다. 실행 중인 외부 도구를 강제로 취소하지 않으며, 이미 진행 중인
Worker는 다음 모델 호출에서 대기합니다. 중단된 Worker 의도는 `open`으로 남고
재개 시 기존 transcript와 도구 결과를 읽습니다.

- 작업 상태 배지에 **구독 한도 회복 대기**가 표시됩니다. 활동 기록에는 대기/회복이
  남고, `/api/stats?task=<id>`의 `subscription_waits`에는 다음 확인 시각이 나옵니다.
- 약 1분 뒤 인증된 `GET /v1/quota`로 `account/rateLimits/read`를 조회합니다.
  소진된 창의 초기화 시각이 더 늦으면 그때까지 기다립니다. 시각은 조회 일정일 뿐이며,
  **`ordinaryUsageAllowed: true`만 실제 회복으로 인정합니다.** 구형 CLI가 이 필드를
  제공하지 않거나, 네트워크/인증 오류가 발생하면 대기를 유지합니다. 조회에 추론이나
  크레딧 사용/충전 요청은 포함되지 않습니다.
- 서버/브리지 재시작 후에도 대기가 유지됩니다. 브리지 산출물 디렉터리의
  `quota-state.json`에는 소진 여부만 저장합니다. 프롬프트나 인증 정보는 넣지 않습니다.
- 사용자 일시정지·종료와 작업의 기존 마감 시간이 우선합니다. 마감 시간은 대기 중에도
  흐르므로, 회복 전에 timeout이 된 작업을 자동으로 되살리지 않습니다.
- 기다리는 중 해당 역할의 endpoint/모델/키/proxy를 바꾸면 원래 설정과 일치할 때까지
  자동 복구하지 않습니다. 이 상태에서 설정을 바꾸려면 원래 구성을 복원하거나,
  기존 작업을 정리하고 변경한 구성으로 새 작업을 시작하세요.
  폴백 풀에서는 실제 한도가 소진된 멤버의 설정을 추적합니다. 이 비교는 브리지
  설정 기준이며, 브리지 뒤 Codex 로그인 계정의 변경을 감지하는 장치는 아닙니다.
- 도구 결과가 없거나 오류인 transcript는 자동 Worker 재개 전에 검토를 요구합니다.
  도구가 성공한 직후 기록 저장 전에 프로세스가 죽는 상황은 외부 작업과 transcript가
  하나의 트랜잭션이 아니므로 성공 여부를 보장할 수 없습니다. 실행 추적을 확인한 뒤
  명시적인 작업 메시지로 재개하세요. 이미 기록된 결과는 새 Codex thread 복구 때
  다시 요청되더라도 가능한 범위에서 내부 재생하고 외부 실행으로 넘기지 않습니다.
  새 작업을 시작한 뒤 이미 완료한 쓰기/Bash/알 수 없는 도구를 같은 이름과
  정규화된 인자로 다시 요청하면
  `tool_resume_review_required`로 해당 Worker 의도를 일시정지합니다. 기본 읽기 도구의
  최신 결과 조회는 허용합니다.
- 자동 재개 대상은 작업 엔진의 Planner/Worker입니다. 독립 채팅이나 Main Agent의
  중단된 응답 자체를 자동 재전송하지는 않습니다. 회복 후 메시지를 다시 보내세요.

검증은 `python3 -m unittest discover -s scripts/codex_probe -q` 및 별도의
`ARTEX_PG_DSN`을 지정한 `go test ./db ./server ./agent ./llmpool -run Subscription`
으로 실행합니다. 테스트 DSN에는 운영 DB를 지정하지 마세요.
