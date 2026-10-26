# api-recon — 참고 설명서

Grep 예시, `config.json` 템플릿과 문제 해결. 모든 grep은 `js/` 디렉터리를 대상으로 한다. bundle이 한 줄이면 먼저 `js-beautify` 또는 `sed 's/}/}\n/g'`를 사용할 수 있지만 보통 주변 범위를 포함한 raw grep으로 충분하다.

## 스크립트 설명

`scripts/`의 모든 파일은 **참고 템플릿**이며 실행 전에 대상 사이트에 맞게 조정해야 한다. 일반 수정 지점:

| 스크립트 | 일반 조정 항목 |
|---|---|
| `harvest_static.py` | endpoint 정규식, webpack/Vite manifest 파싱, 마이크로 프런트엔드 publicPath, 재시도/동시 실행 |
| `runtime_harvest.js` | neutralize 필드명·성공값, stub 매칭 규칙·body 구조, routes 출처, WS 기록, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, L3 활성화 여부, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` 파괴적 링크, cookie, depth/max, 동일 도메인 필터 |
| `extract_route_map.py` | `routeMap` / `routeLink` 정규식, KEY 명명 패턴 |
| `build_perm_tree.py` | `userRouteAuth` 파싱, `ROOTS`/`PREFIX_PARENT` 계층 휴리스틱, stub 외부 필드명 |
| `config.json` | 위 사이트별 매개변수 전체의 통합 진입점 |

수정한 파일은 작업 디렉터리(예: `recon/`)에 두고 보고서에 참고 스크립트 대비 구체적인 변경을 명시하는 것이 좋다.

---

## A. 세 관문 역분석

### A1. 렌더링 관문 — 로그인 여부를 어떻게 판단하는가?

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

`isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` 체인을 찾아 **저장 키**, **컨테이너**(Cookie vs localStorage), **인코딩**을 확인한다.

| 인코딩 | config에서 구성하는 방식 |
|---|---|
| 평문 문자열 / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 서명 없음/`alg:none` JWT 또는 bundle 내부 키로 서명 |
| 암호화(SM2/AES/RSA) | 하드코딩된 키 찾기. 렌더링 관문이 디코딩 가능한 blob만 요구하면 forge 가능. 그렇지 않으면 정적 분석으로 대체 |

→ `cookies` / `localStorage`에 기록한다.

### A2. 인터셉터 관문 — 무엇이 /login 이동을 유발하는가?

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

**필드명**, **성공값**(보통 `0` 또는 `200`), **이동을 유발하는 실패값**을 확인한다. junk session으로 검증:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` + `neutralize.success`에 기록한다.

### A3. 콘텐츠 관문 — 메뉴와 권한은 어디서 오는가?

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**두 계층 데이터**(일반적인 기업 관리 화면):

| API | 일반 payload | 소비 지점 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | 라우트 가드, 버튼 단위 ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | 사이드바 메뉴 렌더링 |
| bundle 내부 `userRouteAuth` | `{ CODE: { url, name? } }` | code → 프런트엔드 path |
| bundle 내부 `routeMap` | `{ KEY: { name, link } }` | 별칭 해석(webpack `o.DASHBOARD`) |

소비하는 코드를 읽어 `getResultTree(tree, permissions)`의 필터 방식과 `v-if` / `hasAuth(code)`가 검사하는 필드를 확인한다.

**수동 forge**(소규모 사이트): 허용적인 payload 구성 → `stubs`.

**전체 권한 트리 복원**(대규모 사이트, 사이드바/하위 모듈이 여전히 비어 있음): **I절** 참고.

---

## B. config.json 템플릿

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

필드 설명:
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` 접두사: `b64json:` → base64(JSON), `json:` → 원본 JSON, 접두사 없음 → 리터럴
- `forward: true`는 실제 요청을 전달하고 코드 필드를 수정. `false`는 완전 오프라인 stub
- `mockTier`: coverage 모드 preload의 활성 계층. 예: `L1+L2`, `L1+L2+L3`
- `routes`는 `routes.txt`에서 가져옴. 메뉴 forge 후 harness가 `<a href>` 자동 추가
- `captureResponses` / `recordWs`는 depth 모드에서만 유효
- `waitUntil`: 대형 SPA는 `domcontentloaded`를 사용해 `networkidle2` 대기 정체 방지
- `routeTimeout`: 라우트별 `page.goto` 제한 시간(밀리초)
- `proxy`: Puppeteer `--proxy-server`. `HTTP_PROXY` / `HTTPS_PROXY`도 설정 가능

### B1. 이중 stub 템플릿(role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

외부 필드명(`response_code` / `code` / `data`)은 A2 인터셉터 관문과 일치해야 하며 `permissions`는 tree의 모든 leaf code를 포함해야 한다.

---

## C. coverage 모드: preload 설정

`scripts/preload.js` 상단 `CONFIG` 객체를 편집하거나 CDP로 주입하기 전에 교체한다.

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* config.json stubs와 동일 */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

검증: `window.__API_RECON_PRELOAD__ === true`이고 pathname이 안정적이어야 한다.

기록 결과 내보내기:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook 기능

preload(coverage)와 runtime_harvest(depth)에 내장된 브라우저 Hook 기능과 범위:

| Hook 기능 | API 발견에 주는 가치 | 지원 범위 |
|---|---|---|
| Hook fetch / XHR.open | 요청 URL/메서드 기록 | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Authorization 등 헤더 발견 | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie 읽기 | 세션 키 이름 확인 | ⚠️ 선택 `observe.storageReads/cookieReads` |
| Vue 라우트 가져오기 | frontendRoutes 보완 | ✅ `__API_RECON_ROUTES__`(로드된 라우트) |
| Vue 라우트 가드 무력화 / 로그인 이동 차단 | 모듈을 열어 API 유발 | ✅ `neutralizeVueRouter` + 네이티브 이동 무력화 |
| React 라우트 가져오기 | 라우트 보완 | ⚠️ 정적 분석 + 클릭. 전용 Hook 없음 |
| 페이지 이동 차단(로그인 path) | 페이지에 머물며 분석 | ⚠️ 업무 탐색을 막지 않도록 로그인 path만 차단 |
| 암호화 라이브러리 Hook(CryptoJS/SM 등) | 암호화 매개변수 → 평문 API body | ❌ 암호화 함수 인수를 수동 Hook해야 함. 결론은 config에 기록 |
| 안티 디버깅 우회 | 우회하지 않으면 runtime에서 API 기록 불가 | ❌ 수동 처리 필요. 정적 분석은 사용 가능 |

---

## E. Endpoint 추출 정규식(정적 결과가 너무 적을 때)

`harvest_static.py`의 `extract_endpoints`를 넓히거나 수동 수행:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. 문제 해결

| 현상 | 원인 → 처리 |
|---|---|
| 정적 API가 매우 적음 | endpoint 방언 불일치 → 정규식 확대(D절) |
| chunk 수 ≪ manifest | CSS-only 또는 배포되지 않은 chunk. 404는 재시도됨 |
| runtime에서 여전히 로그인 화면 | 렌더링 관문 오류 → A1의 키 이름, 컨테이너, 인코딩, domain 재확인 |
| 셸 진입 후 모듈이 비어 있음 | 콘텐츠 관문 → 메뉴 forge(A3). `routes` path가 틀릴 수 있음 |
| 라우트마다 bootstrap/locale만 있음 | 권한 코드 부족 → I절 권한 트리 복원. `role_permissions` + `permissions/all` 이중 stub 확인 |
| 사이드바 항목은 있지만 하위 페이지가 비어 있음 | tree에 intermediate 노드가 없거나 code가 `userRouteAuth`와 불일치 |
| 모든 API가 로그인으로 이동 | 인터셉터 관문 → `neutralize` 확인. 중첩 필드는 walk 로직 확장 필요 |
| WS 프레임 0 | 사용자 상호작용 후에만 subscribe할 수 있음. `perRouteMs` 증가 |
| 응답 본문이 비어 있음 | `forward: true`일 때만 실제 응답 존재 |
| Chromium 없음 | chromium 설치 또는 `config.chromium` / `CHROMIUM` 설정 |
| Mock이 많아도 로그인으로 돌아감 | Hook이 너무 늦거나 `location.href` setter 누락 → document-start + preload |
| 목록이 모두 비어 있음 | L3 빈 배열은 정상. 탭/설정/상세 클릭 계속 |
| Redux action을 라우트로 오인 | get/set/change/clear/toggle/upload가 포함된 내부 path 필터링 |
| Vue가 여전히 로그인으로 이동 | preload가 document-start가 아님 → 주입 시점 변경. 또는 `neutralizeVueRouter: false`일 때 가드 수동 제거 |
| 응답에 URL이 있지만 log에 없음 | `extractUrlsFromResponse` 활성화 또는 `__API_RECON_DETAIL__`에서 수동 추출 |
| Authorization 헤더 이름을 모름 | `observe.xhrHeaders` 활성화 또는 DevTools에서 요청 헤더 확인 |
| runtime이 매우 느림 / 시간 초과 | `waitUntil: domcontentloaded` 설정, `routeTimeout` 감소, `networkidle2` 사용 금지 |
| 프록시 연결 실패 | `proxy` / 환경변수 확인. Puppeteer와 curl의 프록시 포트 일치 |

---

## G. 강화된 대상

서버가 단계별로 세션을 검증하는 경우(forge 불가능한 서명 cookie, 서버 렌더링으로 stub 불가능한 메뉴) runtime은 셸에서 멈춘다. 예상 동작:

- **정적 분석만으로 endpoint 열거 가능** — 모듈 path가 코드에 있음
- 승인이 허용하면 같은 harness를 **실제 세션**으로 실행: `forward: true`, neutralize 불필요, 실제 methods/params/responses 캡처

---

## H. 단일 작업 목록

1. 승인 범위 확인
2. `scripts/harvest_static.py` **읽기** → 대상에 맞게 조정 → 실행 → `api_static.txt`, `routes.txt` 검토
3. **Phase 1b**: path 기준점 주변 범위 확대 + 바인딩 계층 → `param_candidates.json`(J절)
4. A1/A2/A3 역분석 → 사이트 전용 `config.json` 작성
5. `runtime_harvest.js` / `preload.js`를 **읽고 조정한 뒤** 실행
6. `runtimeMode=depth`: `npm install` → 조정한 harvest 스크립트 실행
7. `runtimeMode=coverage/both`: document-start에 조정한 preload 주입 → browser MCP 동적 열거 + **매개변수 트리거 매트릭스**
8. 모듈이 렌더링되지 않으면 **I절 권한 트리 복원** → stubs 패치 → 재실행
9. 매개변수 다중 표본 diff + 오류 역추론 → `params_merged.json`
10. 병합 → `site_map.json` + `api_merged.txt`. 확인 범위, 누락, 스크립트 변경 지점을 정직하게 명시

---

## I. 권한 트리 복원(Phase 4 심화)

단순 `menus: [{ path, show: true }]` forge가 효과 없고 하위 모듈이 여전히 mount되지 않을 때 사용한다.

### I1. auth 모듈 찾기

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

**권한 API path**, **응답 필드명**, **소비하는 chunk 파일명**을 기록한다.

### I2. routeMap 추출

```bash
python3 scripts/extract_route_map.py recon/js recon/
# recon/route_map.json 생성
```

`[!] no routeMap pattern found`이면 `extract_route_map.py`의 정규식을 넓히거나 수동 grep:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 권한 트리와 stub 구성

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

스크립트 로직:
1. `userRouteAuth={MONITOR:{url:...},...}` 파싱(webpack 별칭 `He=o.DASHBOARD` 포함)
2. `route_map.json`으로 alias → 실제 path 해석
3. code 접두사로 parent 추론(`MONITOR_ALERT` → `MONITOR`)
4. `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json` 출력
5. `--config`이면 `config.json`의 `stubs`와 확장된 `routes` 자동 기록

**대상에 맞게 조정**(스크립트 상단):
- `DEFAULT_ROOTS`: 최상위 모듈 code 목록
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent 매핑
- `DEFAULT_EXTRA_PARENT`: 접두사 관계가 아닌 orphan 노드

### I4. stub 일관성 검증

```bash
# permissions 수 ≈ userRouteAuth 항목 수여야 함
wc -l recon/perm_codes_all.txt
# routes는 route_map의 모든 link를 포함해야 함
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. runtime 재실행과 비교

```bash
node recon/runtime_harvest.js recon/config.json
# forge 전후 runtime_api.json 항목 수 비교. /attack, /asset 등에 모듈 API가 나타나는지 확인
```

| forge 전 | forge 후(성공) |
|---|---|
| 라우트마다 같은 bootstrap 3–5개 | 서로 다른 라우트가 다른 module API 유발 |
| `/api/locale/language`만 있음 | `/api/web/...` 모듈 endpoint 등장 |
| `routes.txt`의 한 자릿수 라우트 | route_map에서 나온 `routes` 80–110+개 |

### I6. 여전히 실패하는 경우

- **coverage 모드**: 사이드바 + 탭 클릭. 권한 gating은 상호작용 후에 요청할 수 있음
- **stub 필드**: 실제 API(curl + 실제 session)와 stub의 중첩 구조 비교
- **추가 가드**: `hasPermission|checkRole|func.` 등 버튼 단위 검사를 grep하고 `role_permissions.permissions` 확장
- **정적 분석으로 대체**: 모듈 API path는 여전히 `api_static.txt`에 있음. runtime은 METHOD/body만 보완하며 매개변수는 `param_candidates.json` + 기록된 표본 유지

---

## J. 매개변수 역분석(Phase 1b / 5b / 5c)

**범용 스크립트가 아닌 방법론이다.** path는 정규식으로 찾고 매개변수는 기준점 주변 범위 확대 + UI 바인딩 체인 + 다중 표본 diff + 오류 역추론으로 찾는다.

### J1. 기준점 주변 범위 확대 — path에서 요청 구성 객체 찾기

```bash
# Phase 1에서 알려진 path를 기준점으로 사용
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. 래퍼 계층과 전송 형태

```bash
# axios / 공통 request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# 경로 매개변수
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 검증 관문 — 필수 / 형식 / 열거값

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. 바인딩 계층 — 폼 → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime 보완: DevTools → Network → 요청 → **시작 프로그램**(call stack)에서 `fetch`/`send`를 기준으로 상위 요청 구성 함수를 추적한다.

### J5. 암호화 매개변수

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**암호문에서 필드를 추측하지 않는다.** 암호화 함수의 **인수**를 Hook해 암호화 전 plaintext payload를 기록한다. 결론은 `config.json` / `param_candidates.json`에 쓴다.

### J6. 매개변수 트리거 매트릭스(Phase 3 필수)

모듈별로 작업마다 한 번씩 기록하고 요청 body/query를 diff한다.

| 작업 | 주목할 항목 |
|---|---|
| 목록 첫 화면 | 페이지 처리 기본값 |
| 검색 | keyword, filters |
| 고급 필터 | optional 필드 |
| 생성/편집 | 전체 entity |
| 일괄/내보내기 | `ids[]`, `exportType` |
| 정렬/페이지 이동 | `sortField`, `order` |

`param_samples.json` 생성: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 신뢰도 규칙

| 신뢰도 | 조건 |
|---|---|
| **높음** | 정적 callsite + runtime ≥2개 표본 일치 |
| **중간** | 정적만 또는 runtime 1회만 |
| **낮음** | 응답/오류 역추론, 재검증 안 함 |
| **트리거 대기** | 정적으로 알려진 필드이나 UI/권한 경로 미실행 |

### J8. 상황별 빠른 구성

| 상황 | 순서 |
|---|---|
| REST 목록 페이지 | J1 요청 구성 객체 → J6 네 번 diff → J3 rules |
| 생성/편집 폼 | J3 Form name → J4 submit 체인 → runtime 제출 + 일부러 비워 400 확인 |
| GraphQL | J2 variables 선언 → runtime 각 operation의 variables 기록 |
| 암호화 body | J5 인수 Hook → 암호화 전 필드가 실제 params |

### J9. api-recon 단계 대응

| api-recon | 매개변수 recon |
|---|---|
| Phase 1 정적 | J1 기준점 주변 범위 확대 |
| Phase 2 A2 인터셉터 | 전역 주입 필드(tenantId, sign) |
| Phase 3 runtime | J6 트리거 매트릭스 + `param_samples.json` |
| Phase 4 권한 트리 | 모듈마다 폼이 다름 → 권한이 충분해야 전체 필드 유발 |
| Phase 5 병합 | `params_merged.json` + 신뢰도. 단일 표본으로 필수 여부 확정 금지 |

### J10. 문제 해결

| 현상 | 처리 |
|---|---|
| 정적 필드명이 runtime에 전혀 나오지 않음 | '트리거 대기' 표시. 권한 트리 보완 / 고급 필터 클릭 / 연동 select의 각 option 실행 |
| 같은 path의 body 형태가 다름 | 정상. `action`별로 기록하며 schema를 강제로 병합하지 않음 |
| stub 응답은 가짜지만 params를 보고 싶음 | **outbound 요청** body/headers를 확인하며 stub 응답에서 역추론하지 않음 |
| 400이 nested field를 보고함 | 외부 래퍼 `data`/`bizData`/`variables` 주의 |
| GraphQL에서 operation 이름만 보임 | `variables` JSON 펼치기. 정적 분석에서 `$var: Type` 찾기 |

---
