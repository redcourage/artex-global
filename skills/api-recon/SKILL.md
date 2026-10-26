---
name: api-recon
description: 웹사이트 API 인터페이스를 수집할 때 이 스킬을 호출한다.
---

# API Recon(프런트엔드 인터페이스 정찰)

**승인된 범위에서** 다음을 가능한 한 완전하게 발견한다. **백엔드 API**(경로, 메서드, 매개변수, 응답 본문), **프런트엔드 라우트**, **UI 기능 트리거**(탭, 팝업, 테이블 작업 등).

---

## 경계와 금지(Agent 필독 · 위반 시 범위 초과)

이 스킬은 **API / 매개변수 표면 정찰만** 수행한다. 취약점 발굴이나 침투 악용 단계가 아니다.

### 작업 경계

| 범위 | 허용 | 금지 |
|---|---|---|
| **목표** | path, method, 매개변수, 라우트, UI 트리거 열거 | SQLi/XSS/권한 우회/무차별 대입/fuzz 취약점 테스트, 패킷 변조 공격, 파괴적 작업 |
| **인증** | Hook + stub/mock으로 **클라이언트** 로그인 관문 우회 | 사용자에게 계정·암호 요청 또는 추측, 실제 로그인 폼 제출 시도 |
| **런타임** | 자격증명 없이 API를 hook하고 mock 응답으로 SPA의 로그인 후 셸 진입 | 실제 백엔드 세션이 있어야 진행할 수 있는 흐름 |

### 자격증명 없는 동적 분석(Phase 3 기본)

1. `preload.js` / `runtime_harvest.js`로 로그인·권한·메뉴 등의 bootstrap API를 **가로채고 stub 처리**한다.
2. 업무 조회 API에는 **구조가 올바르고 업무 코드가 성공이며 데이터는 비어 있어도 되는** mock body를 반환한다.
3. 백엔드가 없거나 401 환경이어도 프런트엔드가 로그인 후 페이지를 렌더링하게 하여 더 많은 XHR/fetch/WebSocket을 유발한다.
4. **빈 데이터, 빈 테이블, 자리표시 UI는 모두 예상된 결과**다. 이를 이유로 실제 로그인이나 취약점 테스트로 전환하지 않는다.

**핵심**: mock으로 프런트엔드 라우트와 컴포넌트 마운트를 열고 **outbound 요청만 기록**한다. 백엔드 응답보다 프런트엔드가 **어떤 API를 더 호출하는지**가 중요하다.

### 절차상 절대 금지

| 금지 | 대안 |
|---|---|
| Phase 1 완료 전 주 entry `index-*.js`에 grep/curl/Read로 API path 추출 | `OUTDIR/harvest_static.py` 실행 |
| harvest를 대체하는 `extract_apis.py` 등을 직접 작성 | `OUTDIR/harvest_static.py` 수정 후 재실행 |
| 같은 grep/명령이 ≥2회 실패했는데 반복 | tool_logs 읽기, harvest 수정, reference 확인 등 전략 전환 |
| 관문 A/B를 생략하고 `scripts/` 원본 직접 실행 | OUTDIR로 복사해 대상에 맞게 수정 |
| 실제 사용자명/암호, OTP, OAuth 등의 인증 | 위의 stub/mock 사용 |
| '실제 데이터 확보'를 이유로 stub을 생략하고 권한 우회/주입 테스트 | recon 경계 안에서 outbound만 기록 |
| 삭제, 민감 데이터 내보내기, 대량 쓰기 등 비가역 작업 | coverage 클릭에도 같은 금지 적용 |
| runtime + 동적 열거 없이 모든 페이지와 API를 확보했다고 주장 | '완료 정의' 참고 또는 한계 명시 |
| 매개변수 트리거 매트릭스 + diff 없이 모든 매개변수를 파악했다고 주장 | Phase 3b 매트릭스 + Phase 5 diff |
| 단일 runtime 표본으로 필수/선택 판단 | 여러 표본 diff 또는 검증 규칙/오류에서 역추론 |

---

## 두 계층 모델과 실행 모드

| 계층 | 산출물 | 한계 |
|---|---|---|
| **정적**(JS bundle) | 전체 endpoint 경로, 라우트 초안, 요청 구성 지점의 후보 필드 | HTTP 메서드 없음, 매개변수는 Phase 1b 필요, 런타임 조합 URL 누락 |
| **런타임**(활성 세션) | 메서드 + body + 응답 + 동적 URL + WS/SSE, 다중 표본 diff로 매개변수 보완 | 페이지가 실제 렌더링돼야 요청 발생, 단일 표본으로 필수/선택 확정 불가 |

| 실행 모드 | 엔진 | 용도 |
|---|---|---|
| **depth** | `runtime_harvest.js`(Puppeteer) | API 목록, METHOD/params/응답 본문, WS/SSE, 재현 가능한 일괄 실행 |
| **coverage** | browser + `preload.js` | 탭/팝업/테이블 클릭으로 기능 범위 확대 |
| **both** | depth 다음 coverage | 가장 완전하며 가장 오래 걸림 |

**매개변수 방법론**(범용 스크립트 없음): path는 harvest/정규식, 매개변수는 **기준점 주변 범위 확대 + UI 바인딩 체인 + 다중 표본 diff + 오류 역추론**을 사용한다(grep 예시는 [reference.md](reference.md) J절).

---

## 완료 정의

다음을 모두 충족해야 recon 완료를 주장할 수 있다.

- [ ] **정적**: Phase 1 harvest로 `api_static.txt`, `routes.txt`, `js/` 생성
- [ ] **런타임**: depth 또는 coverage 중 하나 이상. coverage/both는 **Hook 작동 + 동적 열거 루프** 필요
- [ ] **셸 진입**: 업무 path 방문 시 `/login`이 아님(hash 라우트 주의)
- [ ] **매개변수**: coverage/both의 트리거 매트릭스 + `param_samples.json` 완료, Phase 5에서 `params_merged.json` 병합
- [ ] **깊이**(모듈 페이지가 비어 있으면): Phase 4 권한 트리 복원 후 **module 단위 API**가 나올 때까지 재실행(locale/bootstrap만으로 부족)
- [ ] **전달**: Phase 5 산출물 전체 확보(Phase 5 표 참고), `insert_assets`로 서비스·엔드포인트 자산 저장

---

## 스크립트와 관문

`scripts/`는 참고 템플릿이다. **원본을 바로 실행하고 최종 결과로 삼는 것은 금지**한다.

**규칙**: 먼저 읽기 → 대상에 맞게 수정 → `OUTDIR`(예: `recon/`)에 저장 → `CHANGES.md` 기록. 맞지 않으면 구조만 참고하고 방법론에 따라 다시 작성한다.

| 관문 | 시점 | 참고 스크립트 → OUTDIR 사본 | 일반 수정 항목 |
|---|---|---|---|
| **A(정적)** | Phase 0 이후, harvest/spider **첫 실행** 전 | `harvest_static.py` / `spider_mpa.py` | **대부분 사이트는 기본 regex로 실행 가능**. manifest/방언이 맞지 않을 때만 endpoint 정규식, webpack/Vite `publicPath`, MPA exclude/cookie 수정 |
| **B(런타임)** | Phase 2 이후, depth/coverage 실행 전 | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage 키, neutralize 성공값, stubs, login 정규식, api 접두사, hash/history |

**SPA 필수 순서**(교환 불가. Phase 번호는 '먼저 탐색 후 스크립트'보다 우선):

| 단계 | 필수 | 금지 |
|---|---|---|
| Phase 0 완료 후 | 다음 Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | 주 entry `index-*.js`에 curl/grep/Read(보통 >500KB) |
| 관문 A | 스크립트 복사 → 필요시 소폭 수정 → **즉시 실행** | API를 먼저 수동 추출한 뒤 harvest 여부 결정 |
| Phase 1 완료 전 | `wc -l`로 산출물 검증, 404이면 harvest 수정 후 재시도 | extract 스크립트 직접 작성, 내려받지 않은 URL에 grep 반복 |
| Phase 1b부터 | grep 대상은 `OUTDIR/js/*.js`만 | 주 bundle로 harvest 대체 |

- ✅ `harvest_static.py` 복사 → (선택) regex 수정 → **즉시 실행**
- ❌ 주 bundle curl → grep 반복 → 임시 extract 작성 → 마지막에 harvest
- **MPA**: Phase 0 후 다음 Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## 도구와 출력 제약

| 제약 | 설명 |
|---|---|
| 대형 파일 | >100KB `index-*.js`를 Read/grep으로 컨텍스트에 넣는 것은 **금지**. OUTDIR 스크립트로 일괄 처리 |
| grep 출력 | 반드시 `\| head -20` 또는 `-m 5` 사용. 대화에는 path 요약만 남기고 bundle 발췌를 붙이지 않음 |
| 검증 | `wc -l`, `ls \| wc -l` 사용. 전체 디렉터리를 Read하지 않음 |
| regex 초기 탐색 | 선택 사항, ≤1회, ≤50KB 작은 chunk 또는 HTML만. 정식 정적 분석은 harvest 기준 |
| reference | 예시/템플릿/문제 해결은 [reference.md](reference.md) 참고. 본문 전체를 반복 삽입하지 않음 |

---

## 실행 순서

```
Phase 0 분류 + OUTDIR
  → 관문 A → Phase 1 harvest(★ 즉시 실행 ★)
  → Phase 1b 매개변수 역분석
  → Phase 2 인증의 세 관문 → config.json
  → 관문 B → Phase 3 런타임 + 매개변수 매트릭스
  → Phase 4 권한 트리(필요시) → Phase 3 재실행
  → Phase 5 보고서 병합 + insert_assets로 발견한 모든 서비스·엔드포인트 API 자산 일괄 삽입. 삽입 시 발견한 자산을 어떤 경우에도 누락하지 않는다.
```

순서대로 확인한다. **이전 항목을 완료하기 전에는 다음 Phase로 넘어가면 안 된다**.

1. [ ] **Phase 0**: SPA/MPA 초기 탐색, `OUTDIR` 생성 → [Phase 0](#phase-0--분류)
2. [ ] **관문 A + Phase 1**: 스크립트 복사 → **즉시** harvest → `wc -l` 검증 → [Phase 1](#phase-1--정적)
3. [ ] **Phase 1b**: 기준점 주변 범위 확대 + 바인딩 계층 → `param_candidates.json` → [Phase 1b](#phase-1b--매개변수-역분석)
4. [ ] **Phase 2**: 인증의 세 관문 → `config.json` → [Phase 2](#phase-2--인증의-세-관문)
5. [ ] **관문 B**: runtime 스크립트 조정 → [Phase 3](#phase-3--런타임)
6. [ ] **Phase 3**: depth / coverage / both, 셸 진입 확인, 매개변수 트리거 매트릭스 → `param_samples.json`
7. [ ] **Phase 4**(필요시): 권한 트리 → stubs 패치 → Phase 3 재실행 → [Phase 4](#phase-4--권한-트리-복원)
8. [ ] **Phase 5**: 산출물 병합 + 보고서 + `insert_assets` → [Phase 5](#phase-5--병합과-보고)

---

## Phase 0 — 분류

진입 HTML을 가져오고 **`OUTDIR`를 생성**한다(스킬 안의 `scripts/` 수정 금지).

- **SPA**: 빈 셸 + `<div id=app>` + chunk → Phase 1–5
- **MPA**: SSR + `<form>`, endpoint bundle 없음 → 관문 A 이후:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

`forms.txt`, `links.txt`, `api_inline.txt` 생성. SPA에서 forms ≈ 0이면 Phase 1로 전환한다.

---

## Phase 1 — 정적

[스크립트와 관문](#스크립트와-관문), [도구와 출력 제약](#도구와-출력-제약)을 따른다.

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: HTML script 파싱 → webpack/Vite manifest → 모든 lazy chunk 다운로드 → `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` 생성.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk 수와 manifest 비교: 404이면 harvest를 수정해 재시도. chunk별 수동 curl 금지
- `api_static.txt`가 너무 적으면 OUTDIR의 endpoint 정규식을 넓히고 재실행(reference 참고)

### Phase 1b — 매개변수 역분석

path는 Phase 1에서 얻으며 매개변수 필드는 별도로 recon해야 한다. grep 규칙은 [도구와 출력 제약](#도구와-출력-제약) 참고.

**완료 기준**: 주요 API의 필드명, 전송 위치, 추론한 유형, 필수 여부, 표본 값, 신뢰도를 답할 수 있어야 한다.

#### 1b.0 — 전송 형태

| 형태 | 매개변수 위치 | 정적 분석 시 우선 확인 |
|---|---|---|
| REST JSON | body + query | path 기준점 옆 `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql 템플릿, `$page: Int` |
| 전통적인 form | urlencoded | `<form>`, `FormData` |
| 파일 업로드 | multipart | `FormData.append` |
| 경로 매개변수 | `/user/:id` | 라우트 표 + `useParams` / `$route.params` |
| 암호화/서명 | `sign`/`data`에 포함 | 암호화 함수 인수를 Hook(reference D절) |

산출물: API마다 `transport: query|json|form|graphql|encrypted` 표시.

#### 1b.1 — 기준점 주변 범위 확대

알려진 path를 기준점으로 주변 범위를 넓혀 요청 구성 객체를 찾는다.

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| 래퍼 계층 | 매개변수 단서 |
|---|---|
| axios 인스턴스 | `data` / `params` |
| 공통 request | 인터셉터가 전역 필드 주입 |
| OpenAPI 클라이언트 | 생성된 method 시그니처 |
| React Query / SWR | hook의 두 번째 인수 |
| Vue composable | composable 인수 |

남아 있는 유형 정보: `yup`/`zod`/rules, `Form.Item name=`, 내장 Swagger.

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — 바인딩 계층

```
Form field → onFinish/handleSubmit → transform → API payload
```

| 바인딩 출처 | 방법 |
|---|---|
| 폼 submit | submit → transform → API 추적 |
| 테이블 검색 | `getFieldsValue()` → `params` |
| 라우트 | `:id` / `?tab=` |
| 인터셉터 | 전역 `tenantId`, 페이지 처리, sign |
| 열거 select | `options` → API 열거값 |

DevTools call stack에서 `fetch`/`XHR.send`를 시작점으로 상위 요청 구성 함수를 추적한다.

#### 1b.3 — 요청 구성의 세 질문(Phase 2 인증의 세 관문과 다름)

| 질문 | 확인할 내용 |
|---|---|
| **구성** | payload의 build 위치, transform 흔적 |
| **검증** | required, pattern, enum |
| **전송** | path / query / body / multipart / 헤더 |

인터셉터 관문(Phase 2)에서 전역 주입 필드(Authorization, `X-Tenant-Id`, sign)도 함께 읽는다.

#### 1b.4 — Phase 3 연결

후보 필드는 정적/바인딩 계층에서 얻는다. **필수/선택/조건 의존성** 판단에는 Phase 3 매개변수 매트릭스 + diff + Phase 5 오류 역추론이 필요하다.

---

## Phase 2 — 인증의 세 관문

`OUTDIR/js/`에서 grep(`head` 포함)한 뒤 `config.json`에 기록한다(예시는 reference 참고).

| 관문 | 질문 | 키워드 |
|---|---|---|
| **렌더링 관문** | 로그인 여부를 어떻게 판단하는가? | `isLogin`, `getToken`, Cookie/localStorage |
| **인터셉터 관문** | 무엇이 `/login` 이동을 유발하는가? | `response_code`, `errno`, axios interceptor |
| **콘텐츠 관문** | 메뉴/권한은 어디서 오는가? | `menu`, `permission`, `role`, `acl`, `routes` |

localStorage 키 이름을 자격증명으로 취급하지 않는다. chunk/요청 체인에서 확인해야 한다.

**출구 = 관문 B**: 결론을 `config.json`에 반영하고 `OUTDIR/runtime_harvest.js` / `preload.js`를 수정한다.

### Phase 2b — API 관찰(선택)

OUTDIR의 `preload.js`로 세션 키 이름, Authorization, 중첩 API URL을 확인한다.

| 설정 | 산출물 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers 관찰 |
| `extractUrlsFromResponse: true` | 응답 내부 하위 API |
| `observe.storageReads/cookieReads: true` | config에 반영 |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage 매 회차 내보내기: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — 런타임

관문 B를 통과해야 한다. [경계와 금지](#경계와-금지agent-필독--위반-시-범위-초과) 및 자격증명 없는 mock 전략을 따른다.

`config.json`에 `"runtimeMode": "depth" | "coverage" | "both"` 설정(템플릿은 reference 참고).

### Hook과 stub(depth + coverage 공통)

| 계층 | 범위 | 목적 |
|---|---|---|
| L1 정확 매칭 | auth/권한/bootstrap stub | 첫 화면 인증 통과 |
| L2 부정 응답 수정 | 모든 JSON 응답 | 미로그인 코드 → 성공 |
| L3 대체 처리 | L1에 매칭되지 않은 `/api` 등 | 빈 성공 본문으로 UI 열기 |

- **depth**: fake auth + `forward`로 업무 코드 수정 + `stubs`. `routes` 순회(hash/history), `runtime_api.json` 생성
- **coverage**: **document-start**에 `preload.js` 주입(CDP `addScriptToEvaluateOnNewDocument` 또는 Userscript)

검증: `window.__API_RECON_PRELOAD__`가 존재하며 업무 path가 `/login`으로 돌아가지 않는다.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage 동적 열거(필수)

1. 주 탐색/사이드바 — 각 항목 클릭 후 네트워크 1–3s 대기
2. Tab — `role=tab`、`.ant-tabs-tab`
3. 테이블 — 첫 행 보기/편집/상세
4. 도구 모음 — 내보내기, 필터, 생성(**비가역 삭제 피하기**)
5. 모듈 진입마다 — API/라우트 병합
6. SPA — `routes.txt`가 다루지 못한 path에 통제된 `pushState`(MPA에서는 금지)

**매개변수 트리거 매트릭스**(필수): 모듈별로 작업 유형마다 한 번씩 기록하고 **여러 표본을 diff**한다.

| 작업 | 보통 추가되는 매개변수 |
|---|---|
| 목록 첫 화면 | 페이지 처리 + 기본 필터 |
| 검색 클릭 | keyword, filter |
| 고급 필터 | 더 많은 optional |
| 생성/편집 | 전체 entity |
| 일괄/내보내기/정렬 | `ids[]`, `exportType`, `sortField` |

**stub 상태에서도 outbound body/headers는 실제 요청 값**이므로 요청을 기준으로 한다. 기록 → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**：`neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + 사이드바 클릭 + `pushState`
- **both**: 먼저 3a depth, 다음 3b coverage

---

## Phase 4 — 권한 트리 복원

**트리거**: 모듈 페이지가 비어 있거나 라우트마다 bootstrap(예: locale)만 있으면 콘텐츠 관문을 통과하지 못한 것이다.

| 현상 | 의미 |
|---|---|
| 셸 진입 성공 | 렌더링 관문 + 인터셉터 관문 통과 |
| 사이드바 항목 누락/클릭 시 빈 화면 | stub shape 또는 권한 코드 부족 |
| 라우트마다 API가 같고 매우 적음 | `v-if permission` 미통과 |
| `routes.txt`가 bundle보다 훨씬 적음 | auth 모듈에서 보완 필요 |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

일반 체인: `role_permissions`(flat codes) + `permissions/all`(tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

중간 산출물: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

stub 검사: 외부 `response_code`가 인터셉터 관문과 일치하고 flat codes와 tree가 맞아야 한다. `routes`가 `route_map`의 모든 link를 포함해야 한다.

`config.json` 갱신 후 **Phase 3를 재실행**한다. 대형 SPA는 `waitUntil`, `routeTimeout`, `perRouteMs` 조정 가능(reference A3/I절 참고).

---

## Phase 5 — 병합과 보고

### 산출물 표

| 파일 | 단계 | 내용 |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | 정적 bundle과 path |
| `param_candidates.json` | 1b | 정적 매개변수 후보 필드 |
| `config.json` | 2 | 세 관문 + runtime 설정 |
| `runtime_api.json` | 3a | depth 상세 기록(WS/SSE 포함) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | 다중 표본, 클릭 로그, detail |
| `route_map.json` 등 | 4 | 권한 트리 중간 파일(수행한 경우) |
| `params_merged.json` | 5 | 병합한 매개변수 필드 + 신뢰도 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | 라우트, API, params, 기능 지점, 한계 |
| **insert_assets** | 5 | 모든 서비스·엔드포인트 자산을 자산 DB에 저장 |

### 5b — 매개변수 병합

`param_samples.json`에서 diff한다. **범용 병합 스크립트는 없다**. 신뢰도 규칙은 reference J7(높음/중간/낮음/트리거 대기) 참고.

### 5c — 오류 역추론

승인 범위에서 불완전한 요청을 보내 400을 읽을 수 있다(**매개변수 recon이며 취약점 테스트가 아님**). 예: `field 'x' is required`, 열거값 오류. `data` 래퍼, `variables`, 암호화 전 `bizData`에 주의한다.

보고서에 runtimeMode, 정적/런타임 API 수, 매개변수 신뢰도, 미확인 모듈, 참고 스크립트 대비 `CHANGES.md` 요약을 명시해야 한다.

권장 `site_map.json` 구조:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

추가 필드와 grep 예시는 [reference.md](reference.md) 참고.

---

## 일반 설명

- **프레임워크 무관**: webpack/Vite/Angular lazy load는 같은 방법 사용
- **전송**: REST/JSON, GraphQL, WebSocket, SSE. gRPC-web은 범위 밖
- **SSR**: 클라이언트 fetch 기록 가능, RSC/Server Actions는 완전 열거 불가
- **사각지대**: JSVMP, WASM, HMAC/mTLS 강한 검증 → 정적 분석 + 한계 명시
- **매개변수 사각지대**: 조건 연동, hidden params, WASM 요청 구성 → '트리거 대기'/'도달 불가'
- **정적 분석은 안전망**: runtime이 막혀도 정적으로 endpoint 열거 가능

---

## 추가 자료

- Grep 예시, `config.json` 템플릿, 문제 해결, Hook, 매개변수 역분석 J절, site_map 템플릿: **[reference.md](reference.md)**
- 참고 스크립트 경로는 [스크립트와 관문](#스크립트와-관문) 표 참고
