---

## name: scopesentry-mcp
description: ScopeSentry MCP로 보안 스캔 플랫폼(프로젝트, 작업, 템플릿, 자산, 노드)을 관리한다. 사용자가 ScopeSentry, MCP, API Key, 스캔 작업, 자산 조회를 언급하면 사용한다.

# ScopeSentry MCP 사용 안내

**이미 배포된 ScopeSentry 인스턴스**의 사용자를 대상으로 한다. Cursor 또는 다른 MCP 클라이언트로 플랫폼에 연결하며 로컬 소스 코드는 필요하지 않다.

## 1. 준비

### 1.1 서비스 접근 확인

- 기본 웹 화면: `http://<호스트>`
- MCP 엔드포인트: `http://<호스트>/mcp`(리버스 프록시나 프런트엔드 프록시가 있으면 실제 `/mcp` 주소 사용)

### 1.2 API Key 생성

1. 브라우저에서 ScopeSentry 웹 화면에 로그인한다.
2. **API Key** 관리 페이지에서 키를 생성한다(또는 관리자가 제공한 API 사용).
3. 반환된 `ssk_...` 문자열을 저장한다(**한 번만 표시됨**).

### 1.3 Cursor MCP 설정

Cursor → Settings → MCP → 서버 추가:

```json
{
  "mcpServers": {
    "scopesentry": {
      "url": "http://<호스트>:8082/mcp",
      "headers": {
        "X-API-Key": "ssk_사용자키"
      }
    }
  }
}
```

`Authorization: Bearer ssk_사용자키`도 사용할 수 있다.

설정 후 MCP를 재시작하거나 Cursor를 다시 로드하고 도구 목록에 `list_projects`, `list_assets` 등이 나타나는지 확인한다.

---

## 2. 도구 목록

| 도구 | 용도 |
| --- | --- |
| `list_projects` | 태그별 프로젝트 트리(프로젝트 ID 포함) |
| `list_projects_data` | 페이지별 프로젝트 목록, 이름 검색 가능 |
| `get_project` | 프로젝트 상세 |
| `create_project` | 프로젝트 생성 |
| `list_tasks` | 스캔 작업 목록 |
| `get_task` | 작업 상세 |
| `list_scan_templates` | 스캔 템플릿 목록 |
| `get_scan_template` | 템플릿 상세 |
| `list_plugin_modules` | 스캔 파이프라인 모듈명 |
| `list_plugins` | 사용 가능한 플러그인(hash, 기본 매개변수 포함) |
| `create_scan_template` | 스캔 템플릿 생성 |
| `create_scan_task` | 스캔 작업 생성 |
| `list_assets` | 유형별 자산 조회(페이지별 목록) |
| `count_assets` | 자산 수 집계(`/api/assets/common/total`) |
| `get_asset_detail` | 자산 또는 취약점 상세 |
| `add_asset_tag` | 자산에 태그 추가 |
| `list_nodes` | 스캔 노드 목록 |

도구별 인수는 MCP 도구 설명(schema)을 기준으로 한다. `list_assets` / `count_assets`는 search, filter 문법이 같다. 자산 조회 전에 `list_assets` description을 읽어도 된다.

'전체 몇 개인지' 확인하려면 `count_assets`(웹 페이지 전체 개수 API)를 사용한다. 총수를 세려고 `list_assets`를 반복 페이지 조회할 필요는 없다.

---

## 3. 일반 작업 흐름

### 3.1 프로젝트별 자산 조회

사용자나 맥락에 **프로젝트 조건이 있으면** `filter.project`로 범위를 먼저 좁혀 프로젝트 간 데이터 과다로 응답이 느려지지 않게 한다. 명확한 프로젝트가 없으면 프로젝트 필터를 강제하지 않아도 된다.

1. `list_projects` 또는 `list_projects_data`로 대상 프로젝트의 **ObjectID**(`id` / `children[].value`)를 얻는다.
2. `list_assets`에 `filter.project`를 전달한다(**ID여야 하며 프로젝트 표시 이름을 쓰면 안 됨**).

```json
{
  "asset_type": "asset",
  "pageIndex": 1,
  "pageSize": 20,
  "search": "domain=^example.com",
  "filter": {
    "project": ["<프로젝트ObjectID>"]
  }
}
```

### 3.2 스캔 작업 생성

1. `list_nodes`로 온라인 노드 이름을 얻는다.
2. `list_scan_templates` 또는 `create_scan_template`로 템플릿 **ObjectID**를 얻는다.
3. `create_scan_task`: `name`, `node`는 필수이며 `template`에는 템플릿 ID를 넣는다(이름 아님).

**대상 출처 `targetSource`(웹과 동일):**

| targetSource | 설명 | 필수 인수 |
| --- | --- | --- |
| `general` | 대상 직접 입력 | `target` |
| `project` | 프로젝트에서 대상 읽기 | `project`(프로젝트 ObjectID 배열) |
| `asset` | 웹 자산 DB에서 검색 | `search`; 선택: `project`, `filter`, `targetNumber` |
| `RootDomain` | 루트 도메인 DB에서 검색 | `search`; 선택: `project`, `filter`, `targetNumber` |
| `subdomain` | 하위 도메인 DB에서 검색 | `search`; 선택: `project`, `filter`, `targetNumber` |
| `UrlScan` | URL 스캔 결과에서 검색 | `search`; 선택: `project`, `filter`, `targetNumber` |
| `*Source`(예: `subdomainSource`) | 자산 페이지의 '선택/검색'에서 생성 | `targetTp=search`이면 `search`, `targetTp=select`이면 `targetIds` |

**예시 — 루트 도메인 직접 스캔:**

```json
{
  "name": "example-하위도메인수집",
  "node": ["node-1"],
  "template": "<템플릿ObjectID>",
  "targetSource": "general",
  "target": "example.com\nfoo.com",
  "project": ["<프로젝트ObjectID>"]
}
```

**예시 — 하위 도메인 DB에서 이어서 스캔(이전 작업명으로 필터):**

```json
{
  "name": "example-포트와취약점",
  "node": ["node-1"],
  "template": "<후속모듈템플릿ObjectID>",
  "targetSource": "subdomain",
  "search": "task==\"example-하위도메인수집\"",
  "project": ["<프로젝트ObjectID>"]
}
```

### 3.3 루트 도메인 전체 정보 수집(두 단계 권장)

입력이 **루트 도메인**이고 **전체 정보 수집**을 하려면 전체 파이프라인을 한 번에 실행하기보다 두 번 나누어 스캔하는 것이 좋다.

**이유:** 분산 작업은 **대상 하나** 단위로 배분된다. 루트 도메인을 받은 노드는 그 노드에서 찾은 하위 도메인의 후속 모듈도 계속 실행하므로 부하 불균형, 속도 저하, 오류가 발생하기 쉽다.

**권장 방식:**

1. **1단계 — 하위 도메인 수집만 수행**
   - `targetSource`: `general`
   - `target`: 모든 루트 도메인(여러 줄)
   - 템플릿: `SubdomainScan`, `SubdomainSecurity`만 활성화(하위 도메인 스캔 + 하위 도메인 탈취)
   - `get_task`로 작업 완료를 기다린다.

2. **2단계 — 후속 모듈**
   - `targetSource`: `subdomain`
   - `search`: `task=="<1단계작업명>"`(작업명 정확히 매칭)
   - 필요하면 `project`로 범위를 좁힌다.
   - 템플릿: 포트 스캔, 자산 매핑, 취약점 스캔 등(SubdomainScan 제외 가능)
   - 하위 도메인을 독립 대상으로 각 노드에 분배하여 병렬 효율을 높인다.

웹의 '하위 도메인' 자산 페이지에서 작업명으로 필터링한 뒤 '하위 도메인에서 작업 생성'을 사용해도 같다.

```mermaid
flowchart LR
  A[루트 도메인 목록] --> B[1단계: general + SubdomainScan]
  B --> C[하위 도메인 저장]
  C --> D[2단계: subdomain + task==1단계작업명]
  D --> E[포트/자산/취약점 등 모듈]
```

### 3.4 스캔 템플릿 생성

1. `list_plugin_modules` → 모듈명 목록
2. `list_plugins`(`module` 필터 가능) → 각 플러그인의 `hash`와 기본 `parameter`
3. `create_scan_template`: `modules`로 '모듈 → 플러그인 hash 배열' 지정

---

## 4. 자산 조회(`list_assets` / `count_assets`)

`count_assets`와 `list_assets`는 같은 `asset_type`, `search`, `filter`를 사용한다. `{ "total": N }`을 반환하며 웹의 `/api/assets/common/total`에 대응한다.

```json
{
  "asset_type": "subdomain",
  "search": "task==\"작업명\"",
  "filter": {"project": ["<프로젝트ObjectID>"]}
}
```

**성능 권장 사항(`list_assets` / `count_assets` 공통):** 프로젝트 조건이 있으면 `filter.project`로 먼저 범위를 좁힌다. `search`의 인덱스가 있는 필드는 가급적 `==` 완전 일치나 `^` 접두사 매칭([4.3](#43-search-검색-표현식) 참고)을 사용하여 광범위한 `=` 퍼지 검색으로 응답이 느려지지 않게 한다. 프로젝트 맥락이 없으면 프로젝트 필터를 강제하지 않는다.

`filter.project` 지원 유형은 [4.4](#44-filter-정확한-필터) 표를 참고한다.

### 4.1 자산 유형 `asset_type`

`asset`, `RootDomain`, `subdomain`, `app`, `mp`, `UrlScan`, `SensitiveResult`, `DirScanResult`, `crawler`, `vulnerability`, `PageMonitoring`, `IPAsset`, `SubdomainTakerResult`

별칭 예시: `web`→asset, `vuln`→vulnerability, `ip`→IPAsset, `url`→UrlScan

### 4.2 인수 설명

| 인수 | 설명 |
| --- | --- |
| `pageIndex` / `pageSize` | 페이지 처리, 기본 1 / 20 |
| `search` | 검색 표현식(다음 절 참고) |
| `filter` | 정확한 필터 JSON(다음 절 참고) |
| `sort` | UrlScan, DirScanResult만 `length` 정렬 지원 |
| `sid` | SensitiveResult 전용: 민감 정보 규칙명 |

`search`와 `filter`는 **함께 사용할 수 있다**.

### 4.3 search 검색 표현식

사용자 정의 DSL(**SQL 아님**):

| 연산자 | 의미 | 인덱스 | 예시 |
| --- | --- | --- | --- |
| `=` | 퍼지 매칭(regex) | 사용 안 함 | `domain=example` |
| `==` | 정확한 매칭(완전 일치) | **사용** | `port==443` |
| `!=` | 제외 | — | `port!="80"` |
| `&&` | AND | — | `domain==example.com && port==443` |
| `||` | OR | — | `title=admin || body=login` |

**인덱스와 연산자:** `domain`, `ip`, `port`, `title` 등은 인덱스가 있지만 **`==` 완전 일치** 또는 **값이 `^`로 시작하는 접두사 매칭**(예: `domain=^example.com`)만 인덱스를 사용할 수 있다. **`=`는 regex 퍼지 매칭으로 바뀌어 인덱스를 사용하지 못하므로** 데이터가 많으면 느려지기 쉽다.

**모든 유형에 공통인 search 필드:** `tag`, `task`(작업명), `rootDomain`

**project는 search에 쓰면 안 된다**(무효이거나 `&&` 조합 시 오류). 프로젝트 필터는 `filter.project`를 사용한다.

**유형별 일반 search 필드:**

| asset_type | 필드 |
| --- | --- |
| asset | domain, ip, port, service, app, title, statuscode, icon, banner, type, body, header |
| RootDomain | domain, icp, company |
| subdomain | domain, ip, type, value |
| app | name, icp, company, category, description, url, apk |
| mp | name, icp, company, category, description, url |
| UrlScan | url, input, source, resultId, type |
| SensitiveResult | url, sname, body, info, md5 |
| DirScanResult | url, statuscode, redirect, length |
| vulnerability | url, vulname, matched, request, response, level |
| crawler | url, method, body, resultId |
| PageMonitoring | url, hash, diff, response |
| IPAsset | ip, domain, port, service, webServer, app |
| SubdomainTakerResult | domain, value, type, response |

**search 예시:**

- `domain==www.example.com && port==443`(완전 일치, 인덱스 사용)
- `domain=^example.com`(접두사 매칭, 인덱스 사용)
- `ip==192.168.1.1`
- `task=="작업명"`
- `level==high`(vulnerability)
- `statuscode==200`(DirScanResult)

퍼지 포함 검색이 필요할 때만 `=`를 쓴다. 예: `title=admin`(인덱스를 사용하지 않으므로 프로젝트 등 조건으로 범위를 좁히는 것이 좋음).

### 4.4 filter 정확한 필터

JSON 객체: 같은 key의 여러 값은 **OR**, 서로 다른 key는 **AND**다.

**프로젝트 조건이 있으면 `project` 우선:** 사용자나 맥락에 프로젝트가 명확하고 asset_type이 `project`를 지원하면 포함해 범위를 좁힌다. 프로젝트 정보가 없으면 강제하지 않는다.

| filter key | 의미 | 값 설명 |
| --- | --- | --- |
| `project` | 소속 프로젝트 | **ObjectID**, `list_projects` / `list_projects_data`로 조회 |
| `task` | 출처 작업 | **작업명**, `list_tasks`의 `name` 사용 |
| `port` | 포트 | 예: `"443"` |
| `service` | 서비스/프로토콜 | 예: `"https"` |
| `app` | 애플리케이션 지문 | 예: `"Nginx"` |
| `icon` | 아이콘 hash | |
| `statuscode` | HTTP 상태 코드 | 주로 asset에서 사용 |
| `status` | 상태 | UrlScan/DirScan HTTP 코드, 취약점/민감 정보 처리 상태 |
| `level` | 취약점 등급 | critical / high / medium / low / info |
| `type` | 유형 | 예: 하위 도메인 레코드 유형 A, CNAME |
| `color` | 민감 정보 규칙 색상 | SensitiveResult |
| `sname` | 민감 정보 규칙명 | SensitiveResult |
| `tags` | 태그 | |

**유형별 사용 가능한 filter key:**

| asset_type | filter key |
| --- | --- |
| asset | project, port, service, app, icon, statuscode, type, task, tags |
| RootDomain | project, tags |
| subdomain | project, type, task, tags |
| app / mp | project, tags |
| UrlScan | status, tags |
| DirScanResult | status, tags |
| SensitiveResult | status, color, sname, tags |
| crawler | project, task, tags |
| vulnerability | project, level, status, task, tags |
| PageMonitoring / SubdomainTakerResult | tags |
| IPAsset | project, port, service, app |

**filter 예시:**

```json
{"project": ["<프로젝트ObjectID>"], "port": ["443"]}
```

**조합 조회 예시:**

```json
{
  "asset_type": "asset",
  "search": "domain=^baidu && port==443",
  "filter": {"project": ["<프로젝트ObjectID>"]},
  "pageIndex": 1,
  "pageSize": 10
}
```

**주의:**

- 프로젝트 조건이 있으면 `filter.project`를 우선 포함한다(지원하는 경우). 프로젝트 맥락이 없으면 강제하지 않아도 된다.
- `filter.project`에 프로젝트 표시 이름을 넣지 않는다.
- 알려진 값은 `==`, 접두사는 `^`를 사용하며 큰 테이블에서 `=` 퍼지 매칭을 남용하지 않는다.
- UrlScan의 HTTP 상태는 `filter.status`를 사용한다. DirScanResult는 search에서 `statuscode==200`을 사용할 수 있다.
- SensitiveResult의 규칙명 검색: `search`에 `sname=규칙명` 또는 `filter.sname` 사용.

### 4.5 sort 정렬

**UrlScan**, **DirScanResult**만 지원한다.

```json
{"length": "ascending"}
```

다른 유형은 `sort`를 무시하고 기본 시간순으로 정렬한다.

---

## 5. 스캔 템플릿 모듈명

`TargetHandler`, `SubdomainScan`, `SubdomainSecurity`, `PortScanPreparation`, `PortScan`, `PortFingerprint`, `AssetMapping`, `AssetHandle`, `URLScan`, `WebCrawler`, `URLSecurity`, `DirScan`, `VulnerabilityScan`, `PassiveScan`

---

## 6. 문제 해결

| 현상 | 처리 |
| --- | --- |
| MCP에 도구 없음 | URL, API Key, ScopeSentry 실행 여부 확인 |
| 401 / 403 | API Key 재생성 또는 교체 |
| 자산을 찾을 수 없음 | `filter.project`가 ObjectID인지 확인. search에 project를 쓰지 않음 |
| 템플릿/작업 생성 실패 | `template`에는 템플릿 ObjectID, `node`에는 온라인 노드명 사용 |
| 조회가 느리거나 멈춤 | 프로젝트가 있으면 `filter.project` 추가. 인덱스 필드의 search는 `==` 또는 `^` 접두사를 쓰고 `=`를 줄임. `pageSize` 축소 |

---
