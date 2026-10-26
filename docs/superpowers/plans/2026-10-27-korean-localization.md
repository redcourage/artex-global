# 내부 코드·프롬프트 한글화 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 중국어로 작성된 내부 설명, 주석, 기본 문구와 모델 프롬프트를 한국어로 옮기고 `dev` 대상 PR에서 전체 변경을 검토할 수 있게 한다.

**Architecture:** `dev`의 `0d4a97bd27edf023a7002eaa4214cd9f4f807752`에서 만든 `localization/korean` 격리 브랜치에서 디렉터리별로 병렬 번역한다. 공개 API·저장 형식·도구 이름·정책의 의미는 유지한다. 기존 설치의 사용자 저장 데이터는 덮어쓰지 않는다.

**Tech Stack:** Go, TypeScript/React, Python, Markdown.

**Spec:** 사용자의 2026-10-27 승인: 내부 프롬프트를 포함한 전체 한글화, 별도 브랜치에서 작업 후 `dev`로 PR 제출, 사용자가 모든 변경 검토 후 병합 결정.

## Global Constraints

- `agent/**`, `server/**`·`db/**`, `web/**`를 서로 겹치지 않게 나누고 나머지는 통합 담당자가 처리한다.
- zh/ja 지원 번역 리소스, 원문 증거, 탐지 시그니처, 호환성 식별자는 보존하고 잔여 목록에 이유를 기록한다.
- 프롬프트의 권한, 중단 조건, 도구 계약, 치환 변수와 출력 파서는 보존한다. 한국어 출력 계약을 도입하면 기존 형식도 호환한다.
- 기존 보안 허점이나 별도 결함은 중앙 ISSUE_LIST.md에 기록하고 임의 수정하지 않는다.
- 실행 중 앱·사용자 DB와 미커밋 UI 작업은 건드리지 않는다. 자동 병합하지 않는다.

## Review Focus

- 모델이 사용하는 JSON 키·도구 이름·템플릿 변수가 번역되지 않았는지 대조한다.
- 허용·차단·승인 요청 조건의 의미가 달라지지 않았는지 정책 원문과 비교한다.
- 과거 저장 데이터와 새 한국어 표시가 공존하는지 검사한다.
- 중국어·일본어 지원과 다국어 회귀 테스트가 유지되는지 검사한다.
- 소스 테스트 통과는 실제 모델의 성능 동등성을 증명하지 않음을 명시한다.

## Tasks

- [x] 격리 브랜치 생성 및 기준 Go 전체·웹 80개 테스트 통과 확인.
- [x] agent/** 프롬프트·도구 설명·주석 번역, 관련 테스트 확인.
- [x] server/**·db/** 문구·기본값·주석 번역, 관련 테스트 확인.
- [x] web/** 문구·주석 번역, locale·protocol 예외 보존 및 테스트 확인.
- [x] intercept, guard, notify, skills 및 나머지 패키지 문구·주석 번역.
- [x] 전체 잔여 Han 목록 분류와 교차 검토. 구조·플레이스홀더·정책 의미 검사.
- [x] Go 전체 테스트, 웹 테스트·타입 검사, diff 검사 실행.
- [ ] 번역 변경만 커밋하고 원격 dev 존재를 재확인 후 PR 생성. 병합은 사용자 검토 이후.
