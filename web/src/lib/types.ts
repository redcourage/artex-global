// ARTEX domain model — types used across the UI.
// 기능 명세에서 도출한 타입(7절: 핵심 데이터 구조).

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle" | "quota_wait";

export interface Task {
  id: string;
  name?: string; // 선택적 작업 이름. 빈 값/생략은 이름 없음이며 표시할 때 설명으로 대체한다.
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 등록된 취약점 수(심각도별).
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 자산 커버리지 기능 설정(생성 시 지정, 기본 켜짐). false면 계산/표시하지 않는다.
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // 사전 설정 분류. null/생략은 없음.
  intercept_rules?: AssetInterceptRuleInput[]; // 사전 설정된 작업별 차단/허용 규칙.
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- Asset graph (global, shared across tasks) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey
  value?: string;
  company_id?: string; // 소속 회사 자산 id. 빈 값은 소속 없음.
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// Task asset view — server-side enriched, paginated.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- New unified asset model (new backend) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 힘 기반 자산 커버리지 그래프의 노드. 고유 key: 자산="a:<id>", 회사="c:<id>",
// 자산 행이 없는 루트 도메인="r:<domain>". in_scope=false는 연결선에만 쓰는 회색 문맥 노드다.
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// 이 작업 탐색 그래프에서 해당 자산에 연결된 의도/사실/발견(커버리지 노드 서랍용).
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// 작업 테스트 범위의 항목(커버리지 분모 + 승인 경계).
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // 백엔드에서 companies JOIN으로 조회하며 kind=company일 때만 값이 있다.
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 회사 추가 시 제출하는 구조화된 자산 범위 규칙.
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 자산 범위 저장 결과. errors는 이번 제출에서 유효하지 않은 행이고 warnings는 이번 제출과 무관하지만
// 소속 결과를 예상과 다르게 만드는 기존 데이터 문제다(예: ip 필드에 호스트명이 저장된 자산).
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 회사 자산 범위 규칙의 항목(소속 판단의 유일한 기준).
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain일 때 값이 있다.
  net?: string; // kind=ip|cidr일 때 값이 있다.
  value?: string; // kind=icp|keyword일 때 백엔드에서 직접 반환할 수 있다.
  raw: string; // 표시와 재입력에 쓰는 사용자 원본 입력.
  reason?: string;
}

// 회사: type=company 자산 노드 + 아이콘 + 자산 수 + 자산 범위 규칙.
export interface Company {
  id: number;
  name: string;
  logo?: string; // 원격 아이콘 URL. 비어 있으면 프런트엔드에서 이름 첫 글자를 사용한다.
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 의도 소프트 삭제(state='deleted') 시 삭제 사유.
}

// 방송판 한 페이지: 생성 순서로 페이지를 나눈 노드 + 관련 간선 + 간선 반대편 노드(refs, id 기준 인덱스).
// 전체 그래프를 받지 않고도 각 방송의 출처와 산출물을 설명할 수 있다.
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // 노드 id → 연결된 자산(방송판 확장 시 표시하며 현재 페이지 노드와 이웃을 포함).
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 목표 관리 카드의 목표(백엔드에서 payload를 text/vulnclass로 분해함).
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 제약 관리 카드의 작업 제약(allow=허용 / deny=금지).
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 취약점 처리 상태: 대기 / 처리 중 / 확인됨 / 처리됨 / 수정됨 / 오탐 / 무시 / 중복 / 위험 수용.
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset은 취약점에 연결된 자산(백엔드에서 label을 미리 렌더링함).
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 별도 findings 테이블의 행 id이자 상태 갱신 핸들(작업 내 오래된 노드에는 없을 수 있음).
  vulnclass: string;
  name?: string; // 취약점 이름. 비어 있으면 vulnclass로 표시한다.
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 상세 보고서(Markdown). 상세 API만 반환하며 목록에서는 비어 있다.
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage는 발견 목록의 서버 페이지 응답이다.
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 선택적 작업 이름. 빈 값/생략은 이름 없음.
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats는 발견 전체 집계(통계 카드 + 취약점 유형 드롭다운)이며 서버에서 계산해 페이지 구분의 영향을 받지 않는다.
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption은 발견 페이지의 작업별 필터 항목: 취약점이 있는 작업과 취약점 수.
// 설명이 비어 있으면 삭제된 작업이므로 프런트엔드에서 id로 대체한다.
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 선택적 작업 이름. 빈 값/생략은 이름 없음.
  description: string;
  count: number;
}

// FindingQuery는 발견 목록 페이지/필터/정렬 매개변수다.
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // 작업 id. "all"/빈 값이면 작업으로 필터링하지 않는다.
  query?: string;
  sort?: "severity" | "time";
  // 자산 트리 노드 key. 노드 선택은 전체 하위 트리 선택이며 빈 값이면 자산으로 필터링하지 않는다.
  assetScope?: string;
}

// ---- Findings by asset (자산 보기) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode는 자산 트리 노드다. key 형식: a:<id>(자산), c:<id>(회사),
// r:<domain>(DB에 자산 행이 없는 루트 도메인), __none__(연결된 자산 없음).
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // 해당 자산에 직접 연결된 발견 수.
  total: number; // 하위 노드를 포함하고 발견 기준으로 중복 제거한 수.
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET은 백엔드 db.FindingUnassignedAsset에 대응한다.
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- Activity / sessions ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment는 업로드 파일이다. path는 해당 대화/작업 디렉터리(agent CWD) 기준 상대 경로다.
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 절대 경로(scope=staging 임시 업로드 시 반환하며 작업 생성 전에 설명에 추가).
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 스케줄링, 사용자 정의 agent 전용) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 주기: N초마다(0=주기 없음).
  on_finding: boolean; // 어떤 작업에서든 finding 발견 시 실행.
  on_goal_met: boolean; // 어떤 작업에서든 목표 달성 시 실행.
  on_task_timeout: boolean; // 어떤 작업에서든 시간 초과 시 실행.
  on_tool_call: boolean; // 선택한 도구 호출이 완료되면 실행.
  on_task_create: boolean; // 어떤 작업이든 생성되면 실행.
  interval_message: string; // 각 실행 조건에 대한 개별 사용자 메시지.
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call에서 선택한 도구 key(하나 이상).
  last_fire?: string;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- Backend logs (/logs page) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id; present for DB-persisted lines
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 일괄 분류 변경의 작업별 결과. 실패는 작업이 이미 삭제된 경우뿐이며 분류 저장 자체는 원자적이다.
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelCostEstimate {
  currency: "USD";
  basis: "standard_api_equivalent";
  price_version: string;
  source_url: string;
  input_usd: number;
  cache_read_usd: number;
  cache_write_usd: number;
  output_usd: number;
  total_usd: number;
  without_cache_usd: number;
  cache_read_savings_usd: number;
  cache_write_premium_usd: number;
  net_savings_usd: number;
}

export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  // Cost covers priced calls only; absent fields support older servers and mocks.
  cost?: ModelCostEstimate | null;
  priced_calls?: number;
  unpriced_calls?: number;
  usage_missing_calls?: number;
  usage_sources?: string[];
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // main-agent session: which conversation segment (0 = original)
}

// ---- Security ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent가 트래픽 증거를 자동 연결. 기본 꺼짐이며 수동 연결에 영향 없음.
  llm_record: boolean; // LLM 녹화 설정(기본 꺼짐). 꺼져 있으면 LLM 호출을 기록하지 않는다.
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 검색 엔드포인트 접근용 별도 외부 프록시(http/https/socks5). 트래픽 기록용 MITM 프록시와 무관하며 빈 값은 직접 연결.
  web_search_proxy?: string;
  // 전역 외부 프록시(http/https/socks5, user:pass 허용). 모든 대상 트래픽이 경유하며 캡처를 켜면
  // MITM 상위 프록시로 사용한다. 캡처를 끄면 agent의 bash/WebFetch에 직접 주입한다. 빈 값은 직접 연결.
  global_proxy?: string;
  python_interpreter?: string; // 사용자 정의 스크립트 도구의 python 인터프리터 경로(빈 값은 런타임 감지).
  workers?: number; // 동시 worker agent 수(기본 3). 이후 시작하는 작업에 적용된다.
  // 작업 동시 실행 한도. 끄면 무제한이며 켜면 한도를 넘는 새 작업은 대기열에서 빈자리가 날 때 자동 시작한다.
  task_concurrency_enabled?: boolean; // 기본 false.
  task_concurrency_limit?: number; // 활성화 시 기본 5.
  // LLM 순환(장애 조치). 기본 꺼짐. 켜면 모델을 지정하지 않은 agent가 현재 설정을 사용할 수 없을 때
  // (잔액 부족/key 무효/속도 제한/서비스 오류) 다음 설정으로 자동 전환한다.
  llm_pool_enabled?: boolean; // 기본 false.
  // 특정 설정을 지정한 agent/작업 실패 시 순환 체인으로 대체할지 여부. 기본 false는 지정 설정만 사용한다.
  llm_pool_bind_fallback?: boolean;
  // 작업 제약 주입 범위(기본 모두 켜짐). 작업의 allow/deny 제약을 해당 agent 시스템 프롬프트에 추가한다.
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 실험 기능: noa 모델 기반 문맥 압축(기본 꺼짐). 활성화 시 플랫폼의 네 종류 agent(planner/
  // worker/주 agent/대화)의 문맥 압축을 noa가 맡아 내장 compaction을 대체한다. run마다 한 번 읽으며
  // 이후 시작하는 run에 적용된다.
  noa_compaction?: boolean;
  // ---- 취약점 IM 알림(채널은 독립 리소스, /api/notify/* 참조. 여기에는 전역 설정 세 항목만 있음) ----
  notify_enabled?: boolean; // 전체 알림 설정. 기본 켜짐이며 유지보수 중 한 번에 중단할 때 사용한다.
  notify_public_base_url?: string; // 취약점 상세 링크의 외부 접근 주소. 빈 값이면 메시지에 링크를 넣지 않는다.
  notify_digest_interval_min?: number; // 요약 모드 주기(분), 기본 30.
  // 사용자 대면 출력 언어(en/ko/zh/es/ja). 에이전트가 생성하는 리포트·요약·대화 응답이
  // 이 언어를 따른다. 설정 화면의 "표시 언어" 선택기가 UI 언어와 함께 이 값도 저장한다.
  output_language?: string;
}

// ---- 취약점 IM 알림 ----

// NotificationFilter는 채널 필터 조건이다. 모든 필드는 선택 사항이며 생략 시 필터링하지 않는다.
// 백엔드는 모든 필드를 검증하지 않는다. 잘못된 설정은 일치로 처리한다(누락보다 추가 발송을 우선).
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 빈 값이면 제한 없음. 값이 있으면 취약점 소속 작업과 교집합이 필요하다.
  asset_ids?: number[]; // 빈 값이면 제한 없음. 값이 있으면 취약점 연결 자산과 교집합이 필요하다.
  vulnclass_include?: string[]; // 빈 값이면 모두 수신. 값이 있으면 취약점 유형에 키워드 하나 이상이 포함되어야 함(대소문자 무시 부분 문자열).
  vulnclass_exclude?: string[]; // 키워드 하나라도 일치하면 제외(제외가 포함보다 우선).
  on_status_change?: boolean; // 취약점 처리 상태 변경 이벤트도 수신할지 여부.
}

// NotificationChannel은 채널 인스턴스다. config 필드는 kind에 따라 달라지며
// 자격 증명 필드는 읽을 때 "__masked__"로 시작하는 마스킹 값으로 대체한다. 그대로 반환하면 변경하지 않는다.
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys는 백엔드에서 채널 유형별로 제공하며 프런트엔드는 이를 바탕으로 비밀번호 입력과 빈 값이면 유지 안내를 표시한다.
  // 채널별 지식은 하드코딩하지 않는다.
  secret_keys: string[];
}

// NotificationKind는 /api/notify/meta가 반환하는 채널 유형 메타데이터다.
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery는 발송 이력과 실패 재발송에 사용하는 발송 기록이다.
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 사고 설정(thinking.type): ""=보내지 않음(기본) | "disabled"=끄기 | "enabled"=켜기
  thinking_type?: string;
  // 사고 강도: ""=보내지 않음(기본) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // 순환 우선순위. 클수록 먼저 선택한다. 활성 설정은 이 값과 무관하게 항상 체인 첫 번째다.
  priority?: number;
  // true면 장애 조치 대상으로 사용하지 않음(agent/작업에서 명시적 지정은 가능).
  pool_exclude?: boolean;
  // true(기본)=스트리밍(SSE) | false=실제 비스트리밍(stream:false, 한 번에 반환).
  streaming?: boolean;
  // 응답 한 번의 출력 상한(token). 0이면 필드를 보내지 않고 서버 기본값을 사용한다.
  // context_window_k와 구별한다. 후자는 모델 전체 용량이며 로컬 압축 임계값에만 사용한다.
  max_tokens?: number;
  // 상한에 사용할 요청 필드 이름. format="openai"에서만 의미가 있다.
  // ""=max_tokens(기본) | "max_completion_tokens"(OpenAI 추론 모델이 인식하는 필드)
  max_tokens_field?: string;
  // 사용자 정의 세션 헤더 이름. 비어 있지 않으면 매 요청에 헤더를 추가하며 값은 현재 대화/의도의 session id다.
  // ""=보내지 않음. session-id 헤더 기반 프롬프트 캐시/고정 라우팅 게이트웨이에 사용한다.
  session_header_key?: string;
  // 이 설정의 재시도 재정의(연결/빈 응답/동일 provider 안전 구간). 비어 있거나 모두 0이면 전역 정책을 따른다.
  retry?: LLMRetryOverride;
}

// ---- LLM 재시도 정책 ----
// 재시도 계층의 두 설정. 둘 다 0은 미설정이다.
//   attempts    0=기본 횟수 | -1=해당 계층 재시도 끄기 | >0=재시도 횟수
//   interval_ms 0=기본 지수 백오프 | >0=지정된 고정 밀리초 간격
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 개별 LLM 설정에서 재정의할 수 있는 세 계층(모두 엔드포인트에 종속된 재시도).
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 연결 재시도: 연결 초기화/시간 초과/429/5xx, 스트림 시작 전.
  empty: LLMRetryRule; // 빈 응답 재시도: 완료했으나 내용이 없음(openai 형식 전용).
  stream: LLMRetryRule; // 동일 provider 안전 구간 재시도: 출력 전달 전 끊어진 스트림 재실행.
}

// 전역 정책 = 위 세 계층의 기본값 + 전역 전용 두 계층:
//   breaker 순환 차단기(attempts=차단까지의 연속 순간 실패 횟수, interval_ms=고정 대기 시간)
//   intent  의도 재실행(worker가 model_error로 끝난 후 전체 의도 재실행)
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM 순환(장애 조치) ----
// 순환 체인에서 설정의 위치와 상태. state:
//   ok       정상
//   degraded 연속 실패가 있으나 차단 임계값 미만
//   tripped  차단되어 대기 시간 동안 건너뜀(cooldown_secs는 남은 초)
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 현재 활성 설정인지 여부(항상 체인 첫 번째).
  excluded: boolean; // pool_exclude: 순환에서 제외.
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 내장 key는 goals/planner/mainagent/worker. 사용자 정의는 사용자가 지정한다.
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 지정한 LLM 설정. null/생략이면 작업/대화/전역 설정을 따른다.
  max_turns?: number; // 0=무제한.
  run_seconds?: number; // worker 한 번 실행의 실제 시간 상한(초). 0=무제한.
  web_search?: boolean; // 웹 검색 활성화 여부(시스템 전역 설정에 따름).
  interactive_shell?: boolean; // 대화형 shell 활성화 여부(영구 PTY 세션 도구 모음).
  // P3 트리거 후 처리 정책(사용자 정의 agent 전용).
  trigger_run_mode?: "serial" | "parallel"; // 직렬 대기열 / 실행마다 별도 동시 세션.
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial 전용: 동일 작업 병합 / 전체 병합 / 병합 안 함.
  trigger_max_parallel?: number; // parallel 전용: agent별 동시 실행 한도. 0=무제한.
  // 연결 수(목록 API 전용): 보이는 MCP / 보이는 Skill / 연결된 도구.
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 지정 가능한 LLM 설정 후보(기본 모델 드롭다운용). 현재 지정은 agent.llm_profile_id 참조.
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 저장된 마무리 프롬프트(빈 값이면 내장 기본값).
  wrapup_default?: string; // 내장 기본 마무리 프롬프트(플레이스홀더/기본값 복원용).
  wrapup_max_turns?: number; // 저장된 마무리 턴 수(0이면 내장 기본값).
  wrapup_max_turns_default?: number; // 내장 기본 마무리 턴 수("0=기본 N" 안내용).
  // 작업 시간 초과 마무리 프롬프트(worker/planner 전용, task_timeout_wrapup_supported=true일 때만 표시).
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 호출 통계(skill_usage 원장). 호출된 적 없는 skill은 calls=0, last_used 생략.
  calls: number;
  tasks: number; // 해당 skill을 로드한 작업 수(chat 세션 제외).
  usage_agents: string[]; // 해당 skill을 로드한 agent key.
  last_used?: string;
}

// SkillCall은 한 번의 Skill() 호출이다(개별 skill의 최근 호출 목록).
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0=작업 외 상황(대화 세션).
  session_id: string;
  args_len: number;
}

// MissingSkill은 이름으로 요청했으나 존재하지 않는 skill, 즉 사용하려 했지만 없는 항목이다.
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (내장 도구 목록) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // 사용자 정의 도구 유형.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // 사용자 정의 도구 실행 명세(kind!=builtin).
  deferred?: boolean; // 지연 schema(SearchExtraTools/ExecuteExtraTool).
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules (자산 차단: 전역 차단 목록) ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action은 작업별 규칙 전용: block=차단(테스트 금지), allow=허용(허용 목록).
export type AssetInterceptAction = "block" | "allow";

// 작업별 자산 차단/허용 규칙 입력 항목(작업 생성과 상세 편집용).
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // 전역 규칙에는 없는 필드(항상 차단). 작업별 규칙에서 block/allow를 구분한다.
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // 규칙 message 또는 모델 판단 사유(모델 판단에는 모델 접두사 포함).
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: 어떤 차단 규칙에도 일치하지 않을 때 모델이 판단하는 대체 승인의 전역 설정.
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0=활성/기본 설정을 따른다.
  prompt: string; // 판단 프롬프트. GET에서 미설정이면 백엔드가 내장 템플릿 전체를 채운다.
  timeout_seconds: number; // 모델 호출 시간 제한.
  fail_action: "allow" | "ask" | "deny"; // 모델 오류/시간 초과/해석 불가 시 대체 동작.
  ask_timeout_seconds: number; // 모델이 ask로 판단해 수동 승인으로 넘긴 뒤 승인 대기 제한 시간.
  ask_timeout_action: "allow" | "deny"; // 승인 시간 초과 후 기본 동작.
}

// JudgeUsage: 모델 판정(대체 승인, judge 레인)의 누적 token 사용량과 최근 N일 일별 시계열.
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 자산 동기화 (ScopeSentry 데이터 소스) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// 개별 도구 호출 통계(/commands/stats). errors는 실패 횟수다.
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider가 실제로 주고받은 HTTP 원문. 요청은 buildBody()가 보낸 전체 body(도구
  // schema 포함), 응답은 원본 SSE 프레임이다. 위 request_body/response_body는 정규화된 보기이며
  // 도구 schema와 tool_use 블록은 제외한다. 이전 기록은 비어 있다.
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check — 현재 버전과 GitHub 최신 정식 버전 비교 결과. */
export interface UpdateCheck {
  /** 현재 실행 버전. 개발 빌드는 "dev" 또는 접미사가 있는 git describe 형식. */
  current: string;
  /** 실행 형태. docker에서 교체는 컨테이너 쓰기 계층에만 적용되며 재생성하면 이미지 버전으로 돌아간다. */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** 롤백 가능한 이전 버전(artex.old)의 존재 여부. */
  has_backup: boolean;
  /** 이번 시작 시 자체 업데이트 부트스트랩 결과(교체 실패/롤백 등). 특이 사항이 없으면 빈 값. */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub 조회 실패 사유. 실패 시 아래 필드는 제공되지 않는다. */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 현재 플랫폼의 배포 패키지 이름과 해당 Release에 실제 포함되었는지 여부. */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 양쪽 버전을 비교할 수 있는지 여부. 개발 빌드는 false이며 원클릭 업데이트를 비활성화한다. */
  comparable?: boolean;
  /** comparable이 false인 이유. */
  reason?: string;
}

/** /api/update/stream이 보내는 업데이트 진행 상황 한 건. */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** 다운로드 단계에서만 의미 있음(0-100). 나머지 단계는 -1. */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
