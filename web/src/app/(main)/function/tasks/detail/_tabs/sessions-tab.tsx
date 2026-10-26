"use client";

import * as React from "react";

import {
  ArrowUpIcon,
  BrainIcon,
  ChevronDownIcon,
  CircleCheckIcon,
  CircleSlashIcon,
  CircleXIcon,
  ClockIcon,
  HistoryIcon,
  Loader2Icon,
  NetworkIcon,
  PanelLeftCloseIcon,
  PanelLeftOpenIcon,
  PanelsTopLeftIcon,
  PauseIcon,
  PlusIcon,
  RadioIcon,
  RotateCwIcon,
  ShieldAlertIcon,
  SquareIcon,
  Trash2Icon,
  UserIcon,
  WifiOffIcon,
  ZapOffIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { ApprovalExecutionFocus, useApprovalFocus, useApprovalHistory } from "@/components/approval-execution-focus";
import type { DockGripProps } from "@/components/dock-workspace";
import { MentionTextarea } from "@/components/mention-textarea";
import { SessionDockWorkspace } from "@/components/session-dock-workspace";
import { SideQuestionButton, SideQuestionWorkspace } from "@/components/side-question-workspace";
import { TodoPopover } from "@/components/todo-popover";
import { Transcript } from "@/components/transcript";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { InputGroup, InputGroupAddon, InputGroupButton } from "@/components/ui/input-group";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { api, sseUrl } from "@/lib/api";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import {
  AUTO_DONE_HOLD_MS,
  advanceAutoLiveSlots,
  createAutoLiveSlots,
  canUseLiveLayout,
  createLiveSlots,
  fillLiveSlots,
  LIVE_LAYOUTS,
  type LiveLayout,
  replaceLiveSlot,
  SESSION_DRAG_TYPE,
} from "@/lib/live-session-layout";
import { MOCK } from "@/lib/mock/enabled";
import {
  type DockNode,
  type DockZone,
  dockGeometry,
  dockGrid,
  dockInsert,
  dockMove,
  dockRemove,
} from "@/lib/session-dock";
import { isBtwCommand } from "@/lib/side-questions";
import type {
  Activity,
  IntentAsset,
  InterceptApprovalRow,
  Session,
  SessionStatus,
  SessionTokenUsage,
  TaskLLMResolution,
  TaskLLMResolutions,
  TaskNode,
  TokenTotal,
} from "@/lib/types";
import { cn } from "@/lib/utils";

import { LIVE_GRAPH_ID, LiveGraphPane } from "./live-graph-pane";
import { type LiveScrollPosition, LiveSessionPane } from "./live-session-pane";
import { EMPTY_MAIN_DRAFT, MainChatComposer, type MainChatDraft } from "./main-chat-composer";

// resolutionLabel names the LLM a session runs on. The badge shows this alone and
// keeps the model id in its tooltip. Env-backed configs can arrive without a name,
// so fall back to the model id rather than rendering an empty badge.
function resolutionLabel(r: TaskLLMResolution, unnamed: string): string {
  return r.name || r.model || unnamed;
}

// ── Reliability model (see docs/task-session-history-sse-remediation.md) ──────────
// The task's activity is NO LONGER one unbounded `allActivity` array replayed from
// SSE since=0. Instead:
//   • Each UI session (main | plan | intent:<id>) has its own lazily-loaded, reverse-
//     paginated cache (SessionState below). Opening a session loads only its latest
//     page; scrolling up pages older history in.
//   • A single task-level SSE (opened at since=snapshot_cursor from the first history
//     page) tails ALL agents' new activity; frames are dispatched by session_key.
//   • History + SSE meet gap-free at snapshot_cursor and are merged by seq (dedup),
//     so refresh / tab-switch / sleep / reconnect never drop the newest records.

const PAGE = 200; // history page size
const SYSTEM_SCAN_PAGE = 500; // generic activity pages scanned to recover sparse system audit events
const MAX_KEEP = 4000; // per-session in-memory cap; older pages re-fetched on scroll-up
const STREAM_WINDOW_MS = 5000; // "live" = activity seen within this window
const MAX_WORKER_MESSAGE_CHARS = 4000;

function newWorkerMessageRequestID(): string {
  return (
    globalThis.crypto?.randomUUID?.() ??
    `worker-message-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
  );
}

function workerMessageCharCount(value: string): number {
  return Array.from(value).length;
}

// One session's lazily-loaded, reverse-paginated cache. `lastTs`/`unread` are kept
// live even for sessions that were never opened, so the list shows liveness + unread
// without holding their full history.
type SessionState = {
  items: Activity[];
  loaded: boolean;
  loading: boolean;
  loadingMore: boolean;
  hasMore: boolean; // older history remains above the loaded window
  earliestSeq: number; // earliest loaded id — reverse-pagination anchor
  unread: number;
  lastTs: string; // most-recent activity time (drives live badge; updated even when unloaded)
  error?: string;
};
type SessionStore = Record<string, SessionState>;

function emptyState(): SessionState {
  return {
    items: [],
    loaded: false,
    loading: false,
    loadingMore: false,
    hasMore: false,
    earliestSeq: 0,
    unread: 0,
    lastTs: "",
  };
}

// sessionKeyOf routes an activity to its stable session key. worker="planner" covers
// BOTH the Goal Agent's round-0 decomposition and the Planner (single Plan session).
function sessionKeyOf(a: Activity): string {
  if (a.worker === "system" || a.kind === "llm_switch" || a.kind === "llm_failover") return "system";
  if (a.worker === "mainagent") return `main:${a.main_seg ?? 0}`; // one key per conversation segment
  if (a.worker === "planner") return "plan";
  if (a.intent_id) return `intent:${a.intent_id}`;
  return "unknown";
}

// mergeBySeq unions two activity lists by seq (dedup) in ascending seq order. Every
// data source — latest page, older page, SSE compensation, live tail — goes through
// this, so history responses can never clobber live records received meanwhile.
function mergeBySeq(current: Activity[], incoming: Activity[]): Activity[] {
  if (!incoming.length) return current;
  const bySeq = new Map<number, Activity>();
  for (const a of current) bySeq.set(a.seq, a);
  for (const a of incoming) bySeq.set(a.seq, a);
  return [...bySeq.values()].sort((p, q) => p.seq - q.seq);
}

// statusIcon maps a session status to its icon. Worker terminal states are
// 구별되는 상태 색상: 완료(초록 체크 원) / 취소·중지(호박색 사선 원) / 오류(빨간 X 원) /
// 턴 소진(보라). running=파란 회전 아이콘, pending(담당 대기)=회색 시계.
function statusIcon(status: SessionStatus) {
  switch (status) {
    case "running": // 실행 중.
      return <Loader2Icon className="size-3.5 animate-spin text-blue-500" />;
    case "paused":
      return <PauseIcon className="size-3.5 text-amber-500" />;
    case "pending": // 담당 대기(open intent).
      return <ClockIcon className="size-3.5 text-muted-foreground" />;
    case "done": // 완료.
      return <CircleCheckIcon className="size-3.5 text-emerald-500" />;
    case "stopped": // 취소/중지(planner가 종료).
      return <CircleSlashIcon className="size-3.5 text-amber-500" />;
    case "blocked": // 오류.
      return <CircleXIcon className="size-3.5 text-red-500" />;
    case "exhausted": // 턴 소진(max_turns 도달).
      return <ZapOffIcon className="size-3.5 text-violet-500" />;
    case "deleted": // 사용자 소프트 삭제.
      return <CircleSlashIcon className="size-3.5 text-muted-foreground" />;
  }
}

// fmtTokens renders a compact token count (1234 → 1.2k, 2_000_000 → 2M).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

const TokenMetrics = React.forwardRef<
  HTMLSpanElement,
  React.ComponentPropsWithoutRef<"span"> & {
    input: number;
    cache: number;
    output: number;
    labels?: "short" | "long";
  }
>(({ input, cache, output, labels = "short", className, ...props }, ref) => {
  const t = useTranslations("sessions");
  const names =
    labels === "short"
      ? [t("token.inShort"), t("token.cacheShort"), t("token.outShort")]
      : [t("token.inLong"), t("token.cacheLong"), t("token.outLong")];
  const values = [input, cache, output];
  return (
    <span
      ref={ref}
      className={cn("inline-flex min-w-0 flex-wrap items-center gap-1 text-muted-foreground", className)}
      {...props}
    >
      {values.map((value, index) => (
        <React.Fragment key={names[index]}>
          <span>{names[index]}</span>
          <Badge variant="secondary" className="h-5 px-1.5 font-mono tabular-nums">
            {fmtTokens(value)}
          </Badge>
        </React.Fragment>
      ))}
    </span>
  );
});
TokenMetrics.displayName = "TokenMetrics";

// fmtDuration renders an elapsed milliseconds span compactly (90s → 1m30s).
function fmtDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h${String(m % 60).padStart(2, "0")}m`;
}

// Role labels moved to the `sessions.role.*` message catalog (resolved at render in
// SessionsTab); only the icon stays here since it is not a translatable string.
const roleMeta = {
  mainagent: { icon: UserIcon },
  planner: { icon: BrainIcon },
  worker: { icon: RadioIcon },
  system: { icon: HistoryIcon },
} as const;

// The main-agent session is the interactive entry point of this tab and has no
// dedicated backend "sessions" endpoint — it is a fixed UI affordance whose
// transcript is the main-agent activity stream (worker="mainagent") for the task.
// A main-agent session is one resettable conversation segment. Segment 0 is the
// original session; "새 대화" creates further segments (seq 1,2,…) so the agent
// starts on a clean transcript while the task's graph/assets/goal stay shared. Each
// segment is a switchable UI session; only the current (highest) one is writable.
const mainSessionId = (seg: number) => `s-main-${seg}`;
const mainSessionKey = (seg: number) => `main:${seg}`;
// Session titles live in the `sessions.title.*` catalog and are resolved at render
// inside SessionsTab (module scope has no translator). These fixed sessions carry an
// empty title placeholder; every consumer overrides it with t("title.*").
const MAIN_ID = mainSessionId(0);
const MAIN_SESSION: Session = {
  id: MAIN_ID,
  role: "mainagent",
  title: "",
  status: "running",
  live: true,
  last_activity: "",
  seg: 0,
};

// The planner session is, like the main-agent session, a fixed UI affordance with
// no dedicated backend "sessions" endpoint — its transcript is every activity step
// the planner emits (worker === "planner", which also carries the Goal Agent's
// round-0 decomposition; the planner carries no intent_id since it generates intents).
const PLANNER_ID = "s-planner";
const PLANNER_SESSION: Session = {
  id: PLANNER_ID,
  role: "planner",
  title: "",
  status: "running",
  live: true,
  last_activity: "",
};

// LLM provider switches are task-level audit events rather than agent output. The
// history endpoint has no "system" filter, so this fixed session is populated by
// scanning the generic incremental activity endpoint and then tailed by task SSE.
const SYSTEM_ID = "s-system";
const SYSTEM_SESSION: Session = {
  id: SYSTEM_ID,
  role: "system",
  title: "",
  status: "done",
  live: false,
  last_activity: "",
};

// keyForSession maps a UI Session → its stable store key (main:<seg> | plan | intent:<id>).
function keyForSession(s: Session): string {
  if (s.role === "mainagent") return `main:${s.seg ?? 0}`;
  if (s.role === "planner") return "plan";
  if (s.role === "system") return "system";
  return `intent:${s.intent_id}`;
}

// Map an exploration intent (TaskNode) state → a session status the UI renders.
function intentStatus(state: string): SessionStatus {
  switch (state) {
    case "done":
      return "done";
    case "blocked":
      return "blocked";
    case "exhausted":
      return "exhausted";
    case "stopped":
      return "stopped";
    case "paused":
      return "paused";
    case "open": // 담당 대기, 실행 중과 구별.
      return "pending";
    case "deleted": // 사용자 소프트 삭제.
      return "deleted";
    default: // running
      return "running";
  }
}

// Derive worker sessions from running/open intents — there is no backend
// sessions endpoint, so intents (≈ worker units) are the closest real source.
function intentToSession(n: TaskNode): Session {
  const label = (n.payload ?? "").trim();
  const state = intentStatus(n.state);
  return {
    id: n.id,
    role: "worker",
    title: label || `Intent ${n.id}`,
    status: state,
    live: !n.inherited && state === "running",
    last_activity: n.ts,
    intent_id: n.id,
    source_task_id: n.source_task_id,
    inherited: n.inherited,
  };
}

function SessionItem({
  s,
  active,
  displayTitle,
  hasPending,
  unread,
  onClick,
  onCancel,
  controlling,
  deleted,
  onAdd,
}: {
  s: Session;
  active: boolean;
  displayTitle: string;
  hasPending?: boolean;
  unread?: number;
  onClick: () => void;
  onCancel?: () => void;
  controlling?: boolean;
  deleted?: boolean;
  onAdd?: () => void;
}) {
  const t = useTranslations("sessions");
  const icon = deleted ? (
    <Trash2Icon className="size-3.5 text-destructive" />
  ) : s.role === "worker" ? (
    statusIcon(s.status)
  ) : s.live ? (
    <Loader2Icon className="size-3.5 animate-spin text-blue-500" />
  ) : null;

  const cancellable =
    s.role === "worker" &&
    !s.inherited &&
    !deleted &&
    // pending=담당 대기(open) 의도. 실행 중/일시 중지와 함께 삭제 가능하다.
    (s.status === "running" || s.status === "paused" || s.status === "pending");
  return (
    <div
      className={cn(
        "group/session flex w-full items-center rounded-md transition-colors",
        active ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
      )}
    >
      <button
        type="button"
        onClick={onClick}
        draggable={!!onAdd}
        onDragStart={(e) => {
          if (!onAdd) return;
          e.dataTransfer.setData(SESSION_DRAG_TYPE, s.id);
          e.dataTransfer.effectAllowed = "copy";
        }}
        title={onAdd ? t("live.dragToAdd") : undefined}
        className="flex min-w-0 flex-1 items-center gap-1 px-2 py-1.5 text-left text-sm"
      >
        {icon ?? <span className="size-3.5 shrink-0" />}
        {s.intent_id && (
          <span className="shrink-0 rounded bg-muted px-1 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
            #{s.intent_id}
          </span>
        )}
        {s.inherited && s.source_task_id && (
          <Badge variant="outline" className="shrink-0">
            {t("item.sourceTask", { id: s.source_task_id })}
          </Badge>
        )}
        <span
          className={cn("min-w-0 flex-1 truncate text-sm font-medium", deleted && "text-muted-foreground line-through")}
        >
          {displayTitle}
        </span>
        {deleted && (
          <Badge variant="outline" className="shrink-0 border-destructive/40 text-destructive">
            {t("item.deleted")}
          </Badge>
        )}
        {hasPending && <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-500" />}
        {!active && unread ? (
          <span className="inline-flex min-w-4 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
            {unread > 99 ? "99+" : unread}
          </span>
        ) : null}
        {s.live && (
          <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
            <span className="size-1 animate-pulse rounded-full bg-blue-500" />
            {t("item.live")}
          </span>
        )}
      </button>
      {onAdd && (
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          className="shrink-0 opacity-0 group-hover/session:opacity-100 group-focus-within/session:opacity-100"
          title={t("live.addPane")}
          aria-label={t("live.addSession", { title: displayTitle })}
          onClick={onAdd}
        >
          <PlusIcon />
        </Button>
      )}
      {cancellable && (
        <div className="flex shrink-0 items-center gap-0.5 pr-1 opacity-100 sm:opacity-0 sm:transition-opacity sm:group-hover/session:opacity-100 sm:group-focus-within/session:opacity-100">
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            onClick={onCancel}
            disabled={controlling}
            title={t("item.deleteTitle")}
            aria-label={t("item.deleteTitle")}
            className="text-destructive hover:text-destructive"
          >
            <Trash2Icon />
          </Button>
        </div>
      )}
    </div>
  );
}

function truncateWorkerAssetLabel(value: string, maxChars = 30): string {
  const chars = Array.from(value.trim());
  if (chars.length <= maxChars) return chars.join("");
  return `${chars.slice(0, Math.max(0, maxChars - 1)).join("")}…`;
}

function WorkerAssetBadge({ assets }: { assets: IntentAsset[] }) {
  const t = useTranslations("sessions");
  const displayAssets = assets.filter(
    (asset) => asset.type === "root_domain" || asset.type === "subdomain" || asset.type === "ip",
  );
  if (displayAssets.length === 0) return null;

  const first = displayAssets[0];
  const firstRawLabel = first.label.trim() || `#${first.asset_id}`;
  const firstLabel = truncateWorkerAssetLabel(firstRawLabel);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="max-w-60 shrink-0 font-normal" title={firstRawLabel}>
          <span className="truncate">{t("asset.current", { label: firstLabel })}</span>
          {displayAssets.length > 1 && <span className="shrink-0 tabular-nums">+{displayAssets.length - 1}</span>}
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="right" align="start" className="max-w-sm">
        <div className="flex flex-col gap-2">
          {displayAssets.map((asset) => (
            <div key={`${asset.intent_id}-${asset.asset_id}`} className="min-w-0">
              <div className="break-all font-mono text-xs">{asset.label.trim() || `#${asset.asset_id}`}</div>
              <div className="mt-0.5 text-xs text-muted-foreground">
                {t(`asset.type.${asset.type}`)} ·{" "}
                {t.has(`asset.source.${asset.source}`) ? t(`asset.source.${asset.source}`) : asset.source}
                {asset.inherited ? ` · ${t("asset.sourceTask", { id: asset.source_task_id })}` : ""}
              </div>
              <div className="mt-0.5 [overflow-wrap:anywhere] text-xs">{asset.source_summary}</div>
            </div>
          ))}
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

export function SessionsTab({
  taskId,
  liveMode = false,
  onLiveModeChange,
  initialAutoFollow = true,
}: {
  taskId: string;
  liveMode?: boolean;
  onLiveModeChange?: (live: boolean) => void;
  initialAutoFollow?: boolean;
}) {
  const t = useTranslations("sessions");
  const approvalFocus = useApprovalFocus({ taskId });
  const [graphOpen, setGraphOpen] = React.useState(false);
  const [liveCount, setLiveCount] = React.useState(3);
  const nextPane = React.useRef(6);
  const [liveLayout, setLiveLayout] = React.useState<LiveLayout>("3");
  const [customDock, setCustomDock] = React.useState<DockNode | null>(null);
  const [autoFollow, setAutoFollow] = React.useState(initialAutoFollow);
  const autoTask = React.useRef(taskId);
  const doneSince = React.useRef(new Map<string, number>());
  const [completionEpoch, setCompletionEpoch] = React.useState(0);
  const [liveSlots, setLiveSlots] = React.useState(() =>
    initialAutoFollow ? createAutoLiveSlots(PLANNER_ID) : createLiveSlots(),
  );
  const [activePane, setActivePane] = React.useState<number | null>(null);
  const [liveListHidden, setLiveListHidden] = React.useState(false);
  const [liveListWidth, setLiveListWidth] = React.useState(320);
  const [liveScrollMemory] = React.useState(() => new Map<string, LiveScrollPosition>());
  const visibleKeysRef = React.useRef<string[]>([]);
  const liveReadingRef = React.useRef(new Map<string, { key: string; following: boolean }>());
  const liveModeRef = React.useRef(liveMode);
  React.useEffect(() => {
    liveModeRef.current = liveMode;
  }, [liveMode]);
  React.useEffect(() => {
    if (!liveMode) return;
    const clearSelection = (event: PointerEvent) => {
      if (!(event.target instanceof Element)) return;
      if (event.target.closest("[data-live-pane], [data-live-pane-controls], [data-session-list]")) return;
      setActivePane(null);
    };
    document.addEventListener("pointerdown", clearSelection, true);
    return () => document.removeEventListener("pointerdown", clearSelection, true);
  }, [liveMode]);
  const layout = LIVE_LAYOUTS.find((item) => item.id === liveLayout) ?? LIVE_LAYOUTS[2];
  const visibleSlots = autoFollow
    ? liveSlots.slice(0, 3).filter((slot) => slot.sessionId)
    : liveSlots.slice(0, liveCount);
  const sessionTree = visibleSlots.length
    ? dockGrid(
        visibleSlots.map((slot) => slot.id),
        autoFollow ? visibleSlots.length : layout.columns,
      )
    : null;
  const liveTree =
    (autoFollow ? null : customDock) ??
    (graphOpen
      ? sessionTree
        ? {
            kind: "split" as const,
            id: "graph-row",
            axis: "y" as const,
            ratio: 0.5,
            first: { kind: "pane" as const, id: LIVE_GRAPH_ID },
            second: sessionTree,
          }
        : { kind: "pane" as const, id: LIVE_GRAPH_ID }
      : sessionTree);

  const [selectedSessionId, setActiveId] = React.useState(MAIN_ID);
  const focusSession = React.useMemo<Session | undefined>(() => {
    const source = approvalFocus.state?.source;
    if (!source) return undefined;
    if (source.session.startsWith("main:")) {
      const seg = Number(source.session.slice(5));
      return { ...MAIN_SESSION, id: mainSessionId(seg), seg, title: t("title.main", { n: seg + 1 }), live: false };
    }
    if (source.session === "plan") return { ...PLANNER_SESSION, title: t("title.planner"), live: false };
    if (source.session.startsWith("intent:")) {
      const id = source.session.slice(7);
      return {
        id,
        role: "worker",
        intent_id: id,
        title: `Worker #${id}`,
        status: "done",
        live: false,
        last_activity: source.items[0]?.ts ?? "",
      };
    }
    return undefined;
  }, [approvalFocus.state?.source, t]);
  // Keep a located archived/older session selectable after leaving focus mode.
  const [locatedSession, setLocatedSession] = React.useState<{ taskId: string; session: Session }>();
  React.useEffect(() => {
    if (focusSession) setLocatedSession({ taskId, session: focusSession });
  }, [taskId, focusSession]);
  const retainedSession = locatedSession?.taskId === taskId ? locatedSession.session : undefined;
  const activeId = focusSession?.id ?? selectedSessionId;
  // Main-agent conversation segments (newest-first); currentSeg is the writable one.
  const [mainSegs, setMainSegs] = React.useState<{ seq: number; created_at: string }[]>([{ seq: 0, created_at: "" }]);
  const [currentSeg, setCurrentSeg] = React.useState(0);
  const [creatingMain, setCreatingMain] = React.useState(false);
  const [confirmNewMain, setConfirmNewMain] = React.useState(false);
  // 모바일(<lg)은 대화 목록을 기본으로 접는다. 화면이 짧아 목록이 10~15rem을 고정 차지하면
  // 아래 기록이 제목과 입력만 남을 수 있다. 접으면 기록이 거의 전체 높이를 사용하고
  // 제목 클릭으로 펼쳐 대화를 선택하면 자동으로 접는다. 데스크톱(lg 이상)은 항상 펼친다.
  const [listOpen, setListOpen] = React.useState(false);
  // Per-session lazily-loaded caches, keyed by session_key (main | plan | intent:<id>).
  const [store, setStore] = React.useState<SessionStore>({});
  // Worker sessions derived from exploration intents (paged past the old 300 cap).
  const [intents, setIntents] = React.useState<TaskNode[]>([]);
  const [intentAssets, setIntentAssets] = React.useState<IntentAsset[]>([]);
  const [olderIntents, setOlderIntents] = React.useState<TaskNode[]>([]);
  const [firstIntentsHasMore, setFirstIntentsHasMore] = React.useState(false);
  const [olderIntentsHasMore, setOlderIntentsHasMore] = React.useState(false);
  const [hasLoadedOlderIntentsPage, setHasLoadedOlderIntentsPage] = React.useState(false);
  const [loadingOlderIntents, setLoadingOlderIntents] = React.useState(false);
  const [mainDrafts, setMainDrafts] = React.useState<Record<number, MainChatDraft>>({});
  const mainSendingRef = React.useRef(false);
  function updateMainDraft(seg: number, update: (draft: MainChatDraft) => MainChatDraft) {
    setMainDrafts((drafts) => ({ ...drafts, [seg]: update(drafts[seg] ?? EMPTY_MAIN_DRAFT) }));
  }
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  const [mainChatRunning, setMainChatRunning] = React.useState<boolean | null>(null);
  const [controllingIntent, setControllingIntent] = React.useState<string | null>(null);
  const [cancelIntent, setCancelIntent] = React.useState<Session | null>(null);
  const [cancelReason, setCancelReason] = React.useState("");
  // 삭제 모드: soft=기본 소프트 삭제(deleted + 사유 기록, 데이터 유지) | hard=실제 삭제(독점 하위 노드 연쇄 제거).
  const [deleteMode, setDeleteMode] = React.useState<"soft" | "hard">("soft");
  const [workerMessage, setWorkerMessage] = React.useState("");
  const [workerMessageRequestId, setWorkerMessageRequestId] = React.useState("");
  const [workerMessageSending, setWorkerMessageSending] = React.useState(false);
  async function pickFiles(seg: number, files: FileList | null) {
    if (!files?.length) return;
    updateMainDraft(seg, (draft) => ({ ...draft, uploading: true }));
    try {
      const r = await api.chatUpload("task", taskId, Array.from(files));
      updateMainDraft(seg, (draft) => ({ ...draft, attachments: [...draft.attachments, ...r.attachments] }));
    } catch (e) {
      toast.error(t("toast.uploadFailed", { msg: (e as Error).message }));
    } finally {
      updateMainDraft(seg, (draft) => ({ ...draft, uploading: false }));
    }
  }

  // Start a fresh main-agent session: only the segment counter advances — the task's
  // graph/assets/goal are untouched, so the agent continues over the same task with a
  // clean context. The old segment stays as read-only history you can switch back to.
  async function createMainSession() {
    if (creatingMain) return;
    setCreatingMain(true);
    try {
      const r = await api.newMainSession(taskId);
      setMainSegs((prev) => [{ seq: r.seq, created_at: r.created_at }, ...prev.filter((m) => m.seq !== r.seq)]);
      setCurrentSeg(r.current ?? r.seq);
      // Seed an empty, loaded state so the new (empty) session renders immediately.
      setStore((prev) => ({ ...prev, [mainSessionKey(r.seq)]: { ...emptyState(), loaded: true } }));
      setActiveId(mainSessionId(r.seq));
      setListOpen(false);
    } catch (e) {
      toast.error(t("toast.createSessionFailed", { msg: (e as Error).message }));
    } finally {
      setCreatingMain(false);
      setConfirmNewMain(false);
    }
  }

  const patchIntentState = React.useCallback((intentId: string, state?: string) => {
    const patch = (rows: TaskNode[]) =>
      state
        ? rows.map((row) => (row.id === intentId ? { ...row, state } : row))
        : rows.filter((row) => row.id !== intentId);
    setIntents(patch);
    setOlderIntents(patch);
  }, []);

  const controlWorker = React.useCallback(
    async (session: Session, action: "pause" | "resume" | "cancel", reason?: string, mode?: "soft" | "hard") => {
      if (!session.intent_id || session.inherited || controllingIntent) return;
      if (action === "cancel" && !reason?.trim()) {
        toast.error(t("toast.reasonRequired"));
        return;
      }
      setControllingIntent(session.intent_id);
      try {
        const res = await api.controlIntent(taskId, session.intent_id, action, reason, mode);
        if (action === "pause") {
          patchIntentState(session.intent_id, "paused");
          toast.success(t("toast.workerPaused", { id: session.intent_id }));
        } else if (action === "resume") {
          patchIntentState(session.intent_id, "open");
          toast.success(t("toast.workerResumed", { id: session.intent_id }));
        } else if (mode === "hard") {
          // 실제 삭제: 의도와 독점 하위 항목이 물리적으로 제거되었으므로 목록에서 행을 제외한다.
          patchIntentState(session.intent_id);
          const d = res.deleted;
          const extra = d
            ? t("toast.hardDeleteExtra", { intents: d.intents, facts: d.facts, findings: d.findings })
            : "";
          toast.success(t("toast.workerHardDeleted", { id: session.intent_id, extra }));
          setCancelReason("");
        } else {
          // 소프트 삭제: 의도를 deleted로 변경하고 사유를 기록하며 노드와 산출물을 유지한다.
          patchIntentState(session.intent_id, "deleted");
          toast.success(t("toast.workerSoftDeleted", { id: session.intent_id }));
          setCancelReason("");
        }
      } catch (error) {
        toast.error(t("toast.workerActionFailed", { msg: (error as Error).message }));
      } finally {
        setControllingIntent(null);
        setCancelIntent(null);
      }
    },
    [controllingIntent, patchIntentState, taskId, t],
  );

  // SSE connection state — surfaced so a dropped realtime link is visible, never
  // silently shown as "no messages".
  const [sseLive, setSseLive] = React.useState(false);
  // Whole-task token total (all agents), polled from the backend aggregate.
  const [taskTokens, setTaskTokens] = React.useState<TokenTotal | null>(null);
  const [sessionTokens, setSessionTokens] = React.useState<Record<string, SessionTokenUsage>>({});
  const [llmResolutions, setLLMResolutions] = React.useState<TaskLLMResolutions | null>(null);
  // Pending intercept requests for this task — used to show warning icons on sessions.
  const [pendingIntercepts, setPendingIntercepts] = React.useState<InterceptApprovalRow[]>([]);

  // Refs backing SSE/loading without re-render churn.
  const snapshotRef = React.useRef(0); // task-level snapshot cursor → SSE since=
  const esRef = React.useRef<EventSource | null>(null);
  const activeKeyRef = React.useRef(mainSessionKey(0)); // current session key (for SSE dispatch/unread)
  const atBottomRef = React.useRef(true); // transcript pinned to bottom?
  const llmToastSeqRef = React.useRef<Set<number>>(new Set());
  const chatStatusRequestRef = React.useRef(0);
  const firstIntentsRef = React.useRef<TaskNode[]>([]);
  // Per-key request token: a stale response for a key is ignored (guards fast
  // latest/older interleaving). Writes are ALWAYS keyed, so a late response can only
  // touch its own session cache — never the currently-viewed one (see §7.5).
  const reqTokenRef = React.useRef<Record<string, number>>({});
  // Keys with a latest-page load in flight — dedups the double trigger where the
  // first-load effect and the active-session effect both want "main" on mount (the
  // former also opens the SSE, so it must not be pre-empted).
  const loadingKeysRef = React.useRef<Set<string>>(new Set());

  // ── store helpers ──────────────────────────────────────────────────────────────
  const patchStore = React.useCallback((key: string, fn: (s: SessionState) => SessionState) => {
    setStore((prev) => ({ ...prev, [key]: fn(prev[key] ?? emptyState()) }));
  }, []);

  // Load a session's LATEST page (before=0) on first open. Keyed + request-token
  // guarded so a switch away can't corrupt the view.
  const loadSession = React.useCallback(
    (key: string) => {
      if (MOCK) return; // MOCK preloads everything up front
      if (loadingKeysRef.current.has(key)) return; // already in flight (e.g. main on mount)
      loadingKeysRef.current.add(key);
      const token = (reqTokenRef.current[key] ?? 0) + 1;
      reqTokenRef.current[key] = token;
      patchStore(key, (s) => ({ ...s, loading: true, error: undefined }));

      if (key === "system") {
        void (async () => {
          let since = 0;
          let systemItems: Activity[] = [];
          for (;;) {
            const page = await api.activity(taskId, { since, limit: SYSTEM_SCAN_PAGE });
            if (reqTokenRef.current[key] !== token) return;
            systemItems = mergeBySeq(
              systemItems,
              page.items.filter(
                (item) => item.worker === "system" || item.kind === "llm_switch" || item.kind === "llm_failover",
              ),
            );
            if (page.items.length < SYSTEM_SCAN_PAGE || page.cursor <= since) break;
            since = page.cursor;
          }
          patchStore(key, (s) => {
            const items = mergeBySeq(systemItems, s.items);
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: false,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              lastTs: items.length ? items[items.length - 1].ts : s.lastTs,
              error: undefined,
            };
          });
        })()
          .catch((error) => {
            if (reqTokenRef.current[key] !== token) return;
            patchStore(key, (s) => ({
              ...s,
              loading: false,
              error: (error as Error).message || t("toast.loadFailed"),
            }));
          })
          .finally(() => loadingKeysRef.current.delete(key));
        return;
      }

      api
        .activityHistory(taskId, key, 0, PAGE)
        .then((r) => {
          if (reqTokenRef.current[key] !== token) return; // superseded
          if (r.snapshotCursor > snapshotRef.current) snapshotRef.current = r.snapshotCursor;
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items); // keep any live frames arrived meanwhile
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              error: undefined,
            };
          });
        })
        .catch((e) => {
          if (reqTokenRef.current[key] !== token) return;
          patchStore(key, (s) => ({ ...s, loading: false, error: (e as Error).message || t("toast.loadFailed") }));
        })
        .finally(() => loadingKeysRef.current.delete(key));
    },
    [taskId, patchStore, t],
  );

  // Load one older page (scroll-up) for a session, preserving scroll position.
  const loadEarlier = React.useCallback(
    (key: string, viewport: () => HTMLElement | null) => {
      const st = store[key];
      if (!st || st.loadingMore || !st.hasMore || !st.earliestSeq) return;
      const vp = viewport();
      const prevH = vp?.scrollHeight ?? 0;
      const prevTop = vp?.scrollTop ?? 0;
      patchStore(key, (s) => ({ ...s, loadingMore: true }));
      api
        .activityHistory(taskId, key, st.earliestSeq, PAGE)
        .then((r) => {
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items);
            return {
              ...s,
              items,
              loadingMore: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : s.earliestSeq,
            };
          });
          requestAnimationFrame(() => {
            const v = viewport();
            if (v) v.scrollTop = prevTop + (v.scrollHeight - prevH);
          });
        })
        .catch(() => {
          patchStore(key, (s) => ({ ...s, loadingMore: false }));
        });
    },
    [taskId, store, patchStore],
  );

  // ── task token total (whole task, all agents) ───────────────────────────────────
  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .tokenStats(taskId)
        .then((r) => {
          if (!alive) return;
          setTaskTokens(r.total);
          setSessionTokens(Object.fromEntries(r.sessions.map((item) => [item.session, item])));
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    setMainChatRunning(null);
    const load = () => {
      const request = ++chatStatusRequestRef.current;
      return api
        .chatStatus(taskId)
        .then(({ running }) => {
          if (alive && chatStatusRequestRef.current === request) setMainChatRunning(running);
        })
        .catch(() => {
          // Keep the last authoritative value. Before the first success, recent
          // activity remains a conservative fallback.
        });
    };
    void load();
    const timer = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .taskLLMResolution(taskId)
        .then((value) => {
          if (alive) setLLMResolutions(value);
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const timer = setInterval(load, 10_000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .interceptTask(taskId)
        .then((rows) => {
          if (alive) setPendingIntercepts(rows.filter((r) => r.status === "pending"));
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId]);

  // Re-render on a timer so "streaming" liveness recomputes as activity goes stale.
  const [, setTick] = React.useState(0);
  React.useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 1500);
    return () => clearInterval(id);
  }, []);

  // ── first load + single task SSE ────────────────────────────────────────────────
  // On task open: load Main's latest page, take the task-level snapshot cursor from
  // it, THEN open ONE task SSE at since=snapshot_cursor. History covers id≤cursor and
  // the SSE covers id>cursor with no gap. The SSE tails ALL agents; frames are routed
  // by session_key. EventSource auto-reconnects and (via our `id:` lines → Last-Event-
  // ID) resumes from the DB, so a dropped realtime link self-heals; seq-merge dedups.
  React.useEffect(() => {
    setStore({});
    doneSince.current.clear();
    setAutoFollow(initialAutoFollow);
    setLiveSlots(initialAutoFollow ? createAutoLiveSlots(PLANNER_ID) : createLiveSlots());
    setLiveCount(3);
    setGraphOpen(false);
    nextPane.current = 6;
    setLiveLayout("3");
    setCustomDock(null);
    setActivePane(null);
    liveScrollMemory.clear();
    liveReadingRef.current.clear();
    setIntents([]);
    setIntentAssets([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    setHasLoadedOlderIntentsPage(false);
    setTaskTokens(null);
    setSessionTokens({});
    setLLMResolutions(null);
    setSseLive(false);
    setActiveId(MAIN_ID); // a stale worker id from the previous task must not leak in
    setMainSegs([{ seq: 0, created_at: "" }]);
    setCurrentSeg(0);
    snapshotRef.current = 0;
    llmToastSeqRef.current = new Set();
    reqTokenRef.current = {};
    // Reserve the initial main key so the active-session effect (which fires for main on
    // mount) won't double-load it and pre-empt the SSE opened here.
    loadingKeysRef.current = new Set([mainSessionKey(0)]);
    let alive = true;

    // MOCK demo: no SSE backend — pull one activity snapshot and bucket by session.
    if (MOCK) {
      api
        .activity(taskId)
        .then((r) => {
          if (!alive) return;
          const buckets: SessionStore = {};
          for (const a of r.items) {
            const k = sessionKeyOf(a);
            if (!buckets[k]) buckets[k] = emptyState();
            buckets[k].items.push(a);
          }
          for (const k of Object.keys(buckets)) {
            const st = buckets[k];
            st.items.sort((p, q) => p.seq - q.seq);
            st.loaded = true;
            st.hasMore = false;
            st.earliestSeq = st.items.length ? st.items[0].seq : 0;
            st.lastTs = st.items.length ? st.items[st.items.length - 1].ts : "";
          }
          buckets[mainSessionKey(0)] ??= { ...emptyState(), loaded: true };
          buckets.system ??= { ...emptyState(), loaded: true };
          setStore(buckets);
        })
        .catch(() =>
          setStore({
            [mainSessionKey(0)]: { ...emptyState(), loaded: true },
            system: { ...emptyState(), loaded: true },
          }),
        );
      return () => {
        alive = false;
      };
    }

    const token = (reqTokenRef.current.mainboot ?? 0) + 1;
    reqTokenRef.current.mainboot = token;
    let bootKey = mainSessionKey(0);
    // Resolve the main-agent segments first, then load the CURRENT segment's history and
    // open the SSE from its snapshot cursor. The SSE tails all segments and routes each
    // frame by session_key (main:<seg>), so switching segments needs no new stream.
    api
      .mainSessions(taskId)
      .then((ms) => {
        if (!alive || reqTokenRef.current.mainboot !== token) throw new Error("superseded");
        const segs = ms.sessions.length ? ms.sessions : [{ seq: 0, created_at: "" }];
        setMainSegs(segs);
        setCurrentSeg(ms.current);
        bootKey = mainSessionKey(ms.current);
        if (bootKey !== mainSessionKey(0)) loadingKeysRef.current.delete(mainSessionKey(0));
        loadingKeysRef.current.add(bootKey);
        setActiveId(mainSessionId(ms.current));
        patchStore(bootKey, (s) => ({ ...s, loading: true }));
        return api.activityHistory(taskId, bootKey, 0, PAGE);
      })
      .then((r) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        snapshotRef.current = r.snapshotCursor;
        patchStore(bootKey, (s) => {
          const items = mergeBySeq(r.items, s.items);
          return {
            ...s,
            items,
            loaded: true,
            loading: false,
            hasMore: r.hasMore,
            earliestSeq: items.length ? items[0].seq : 0,
            unread: 0,
          };
        });
        // Open the single task SSE from the snapshot cursor.
        const es = new EventSource(
          sseUrl(`/api/exploration/activity/stream?task=${encodeURIComponent(taskId)}&since=${snapshotRef.current}`),
        );
        esRef.current = es;
        es.onopen = () => setSseLive(true);
        es.onerror = () => setSseLive(false); // EventSource auto-reconnects; DB compensates the gap
        es.onmessage = (e) => {
          let a: Activity;
          try {
            a = JSON.parse(e.data) as Activity;
          } catch {
            return; // ignore malformed frame
          }
          if ((a.kind === "llm_switch" || a.kind === "llm_failover") && !llmToastSeqRef.current.has(a.seq)) {
            llmToastSeqRef.current.add(a.seq);
            const transition = a.metadata?.llm_transition;
            if (transition?.mode === "exhausted" || a.is_error) {
              toast.error(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            } else if (transition?.mode === "automatic") {
              toast.success(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            } else {
              toast.info(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            }
            void api
              .taskLLMResolution(taskId)
              .then((value) => {
                if (alive) setLLMResolutions(value);
              })
              .catch(() => {
                // The periodic resolver poll will retry if this event-triggered refresh fails.
              });
          }
          const k = sessionKeyOf(a);
          setStore((prev) => {
            const cur = prev[k] ?? emptyState();
            const activeK = activeKeyRef.current;
            // Merge into sessions that are loaded, actively loading, or the current
            // view (so returning is instant + a frame that lands mid-load isn't lost).
            // A cold, inactive session also keeps a tiny accounting tail so the
            // sidebar can add the latest unfinished usage without loading history.
            if (!cur.loaded && !cur.loading && k !== activeK) {
              const accountingItems =
                a.kind === "usage" || a.kind === "result" ? mergeBySeq(cur.items, [a]).slice(-4) : cur.items;
              return {
                ...prev,
                [k]: { ...cur, items: accountingItems, lastTs: a.ts, unread: cur.unread + 1 },
              };
            }
            let items = mergeBySeq(cur.items, [a]);
            // Memory bound: trim oldest when over cap (older re-fetched on scroll-up),
            // but never while the user is reading this session's history (scrolled up).
            let hasMore = cur.hasMore;
            let earliestSeq = cur.earliestSeq;
            const readingInLivePane =
              liveModeRef.current &&
              [...liveReadingRef.current.values()].some((pane) => pane.key === k && !pane.following);
            const trimmable = !readingInLivePane && (k !== activeK || atBottomRef.current);
            if (trimmable && items.length > MAX_KEEP) {
              items = items.slice(items.length - MAX_KEEP);
              hasMore = true;
              earliestSeq = items[0].seq;
            }
            const unread = (liveModeRef.current ? visibleKeysRef.current.includes(k) : k === activeK)
              ? 0
              : cur.unread + 1;
            return { ...prev, [k]: { ...cur, items, lastTs: a.ts, unread, hasMore, earliestSeq } };
          });
        };
      })
      .catch((err) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        if ((err as Error).message === "superseded") return;
        patchStore(bootKey, (s) => ({ ...s, loading: false, error: (err as Error).message || t("toast.loadFailed") }));
      })
      .finally(() => loadingKeysRef.current.delete(bootKey));

    return () => {
      alive = false;
      esRef.current?.close();
      esRef.current = null;
    };
  }, [taskId, patchStore, t, liveScrollMemory]);

  // Reconcile visible transcripts with persisted history even when SSE is open.
  // onopen confirms the connection, not delivery: a proxy can buffer frames and
  // a lossy broadcast can miss individual events without closing the stream.
  // SSE still supplies immediate updates; history repairs gaps every 5s (2s when
  // disconnected). Merge the entire page, not just IDs above a high-water mark:
  // a missing message can precede a newer event already received over SSE.
  React.useEffect(() => {
    if (MOCK) return;
    let alive = true;
    let timer: ReturnType<typeof setTimeout>;
    const interval = sseLive ? 5000 : 2000;
    const poll = async () => {
      const keys = liveModeRef.current ? visibleKeysRef.current : [activeKeyRef.current];
      await Promise.all(
        [...new Set(keys)].map(async (key) => {
          try {
            const r = await api.activityHistory(taskId, key, 0, PAGE);
            if (!alive) return;
            patchStore(key, (s) => {
              const items = mergeBySeq(s.items, r.items);
              return {
                ...s,
                items,
                loaded: true,
                loading: false,
                error: undefined,
                hasMore: s.loaded ? s.hasMore : r.hasMore,
                earliestSeq: items[0]?.seq ?? 0,
                lastTs: items.at(-1)?.ts ?? s.lastTs,
              };
            });
          } catch {
            // Independent retries: one failed pane must not interrupt the others.
          }
        }),
      );
      if (alive) timer = setTimeout(() => void poll(), interval);
    };
    timer = setTimeout(() => void poll(), interval);
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [sseLive, taskId, patchStore]);

  React.useEffect(() => {
    let active = true;
    const load = () =>
      api
        .taskIntentAssets(taskId)
        .then((assets) => {
          if (active) setIntentAssets(assets);
        })
        .catch(() => {
          // The next poll retries; Worker controls and transcripts remain available.
        });
    void load();
    const timer = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [taskId]);

  // ── worker (intent) session list — paged, poll first page lightly ───────────────
  React.useEffect(() => {
    let active = true;
    firstIntentsRef.current = [];
    setIntents([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    const load = () =>
      api
        .intentsPage(taskId, 0, 300)
        .then((r) => {
          if (!active) return;
          const freshIds = new Set(r.items.map((item) => item.id));
          const oldestFreshId = r.items.reduce((minimum, item) => Math.min(minimum, Number(item.id)), Infinity);
          const displaced = r.hasMore
            ? firstIntentsRef.current.filter((item) => !freshIds.has(item.id) && Number(item.id) < oldestFreshId)
            : [];
          if (displaced.length > 0) {
            setOlderIntents((previous) => {
              const byId = new Map(previous.map((item) => [item.id, item]));
              for (const item of displaced) byId.set(item.id, item);
              return [...byId.values()];
            });
          }
          firstIntentsRef.current = r.items;
          setIntents(r.items);
          setFirstIntentsHasMore(r.hasMore);
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [taskId]);

  const loadOlderIntents = React.useCallback(() => {
    if (loadingOlderIntents) return;
    const all = [...intents, ...olderIntents];
    const minId = all.reduce((m, n) => Math.min(m, Number(n.id)), Infinity);
    if (!Number.isFinite(minId)) return;
    setLoadingOlderIntents(true);
    api
      .intentsPage(taskId, minId, 300)
      .then((r) => {
        setOlderIntents((prev) => {
          const seen = new Set([...intents, ...prev].map((n) => n.id));
          return [...prev, ...r.items.filter((n) => !seen.has(n.id))];
        });
        setOlderIntentsHasMore(r.hasMore);
        setHasLoadedOlderIntentsPage(true);
      })
      .catch(() => {
        // A later manual retry can fetch this page again.
      })
      .finally(() => setLoadingOlderIntents(false));
  }, [taskId, intents, olderIntents, loadingOlderIntents]);

  // Combined, de-duplicated worker list (newest first page + older loaded pages).
  const allIntents = React.useMemo(() => {
    const byId = new Map<string, TaskNode>();
    for (const n of olderIntents) byId.set(n.id, n);
    for (const n of intents) byId.set(n.id, n); // fresh poll wins over older snapshot
    return [...byId.values()].sort((a, b) => Number(b.id) - Number(a.id));
  }, [intents, olderIntents]);
  const intentAssetsByID = React.useMemo(() => {
    const grouped = new Map<string, IntentAsset[]>();
    for (const asset of intentAssets) {
      const key = String(asset.intent_id);
      const current = grouped.get(key);
      if (current) current.push(asset);
      else grouped.set(key, [asset]);
    }
    return grouped;
  }, [intentAssets]);

  const workerSessions = React.useMemo(() => allIntents.map(intentToSession), [allIntents]);
  const intentsHasMore = hasLoadedOlderIntentsPage ? olderIntentsHasMore : firstIntentsHasMore;

  // For each worker session (intent), derive the display title from the intent
  // payload summary. Also store the full TaskNode for the hover-JSON tooltip.
  const sessionMeta = React.useMemo(() => {
    const map = new Map<string, { title: string; json: unknown; deleted: boolean; deleteReason: string }>();
    for (const node of allIntents) {
      let title = `Intent ${node.id}`;
      let parsedPayload: unknown = node.payload;
      // 소프트 삭제: 의도 state='deleted', 사유는 별도 delete_reason 필드.
      const deleted = node.state === "deleted";
      const deleteReason = node.delete_reason ?? "";
      if (node.payload) {
        try {
          const p = JSON.parse(node.payload);
          parsedPayload = p;
          if (p?.summary) title = String(p.summary);
        } catch {
          title = node.payload.trim() || title;
        }
      }
      const json = { ...node, payload: parsedPayload };
      map.set(node.id, { title, json, deleted, deleteReason });
    }
    return map;
  }, [allIntents]);

  // Returns true if any pending intercept belongs to this session.
  // Worker agent_name format: "work#N · #intentID". Main/planner match by role key.
  const hasPendingForSession = React.useCallback(
    (s: Session): boolean => {
      if (s.inherited) return false;
      if (!pendingIntercepts.length) return false;
      if (s.role === "mainagent") return pendingIntercepts.some((r) => r.agent_name === "mainagent");
      if (s.role === "planner") return pendingIntercepts.some((r) => r.agent_name === "planner");
      // Worker: extract intent ID from "work#N · #<intentID>"
      return pendingIntercepts.some((r) => {
        const m = r.agent_name.match(/·\s*#(\d+)$/);
        return m ? m[1] === s.intent_id : false;
      });
    },
    [pendingIntercepts],
  );

  // Liveness from each session's last-seen activity time (kept fresh by the 1.5s tick).
  const recentLive = React.useCallback(
    (key: string) => {
      const ts = store[key]?.lastTs;
      if (!ts) return false;
      const t = Date.parse(ts);
      return t > 0 && Date.now() - t < STREAM_WINDOW_MS;
    },
    [store],
  );
  const currentMainKey = mainSessionKey(currentSeg);
  const plannerLive = recentLive("plan");

  // Which main segment is streaming RIGHT NOW. A main turn is serialized per task, so at
  // most one segment is live. Gate on the real running flag (sending / mainChatRunning)
  // rather than "recent activity", and drop it the moment the turn's terminal record
  // (kind='result', or an error) lands — otherwise the badge lingers for STREAM_WINDOW_MS
  // after the agent already finished. null = nothing running.
  const liveMainSeg = React.useMemo<number | null>(() => {
    if (!(sending || mainChatRunning)) return null;
    // the streaming segment is the one with the freshest activity (incl. the just-sent turn)
    let seg = currentSeg;
    let bestTs = -1;
    for (const m of mainSegs) {
      const raw = store[mainSessionKey(m.seq)]?.lastTs;
      const ts = raw ? Date.parse(raw) : -1;
      if (ts > bestTs) {
        bestTs = ts;
        seg = m.seq;
      }
    }
    const items = store[mainSessionKey(seg)]?.items ?? [];
    const last = items[items.length - 1];
    if (last && (last.kind === "result" || (last.kind === "text" && last.is_error))) return null;
    return seg;
  }, [sending, mainChatRunning, mainSegs, store, currentSeg]);

  // Every main-agent segment is an independent, interactive session (like the top-level
  // chat conversations) — you can talk in any of them, newest-first.
  const mainSessions = React.useMemo<Session[]>(
    () =>
      mainSegs.map((m) => ({
        id: mainSessionId(m.seq),
        role: "mainagent",
        title: t("title.main", { n: m.seq + 1 }),
        status: "running",
        live: m.seq === liveMainSeg,
        last_activity: m.created_at,
        seg: m.seq,
      })),
    [mainSegs, liveMainSeg, t],
  );

  const sessions = React.useMemo(() => {
    const items = [
      ...mainSessions,
      { ...PLANNER_SESSION, title: t("title.planner"), live: plannerLive },
      ...workerSessions,
      { ...SYSTEM_SESSION, title: t("title.system") },
    ];
    const located = focusSession ?? retainedSession;
    if (located && !items.some((s) => s.id === located.id)) items.push(located);
    return items;
  }, [mainSessions, workerSessions, plannerLive, focusSession, retainedSession, t]);

  const liveMainReady = !!store[mainSessionKey(currentSeg)]?.loaded;
  const workerSnapshot = JSON.stringify(workerSessions.map(({ id, status }) => ({ id, status })).reverse());
  React.useEffect(() => {
    // A changed task first clears the old task's snapshot in the bootstrap effect.
    if (autoTask.current !== taskId) {
      autoTask.current = taskId;
      return;
    }
    if (!liveMode || !liveMainReady) return;
    const workers: { id: string; status: SessionStatus }[] = JSON.parse(workerSnapshot);
    const now = Date.now();
    const completed = new Set(workers.filter((worker) => worker.status === "done").map((worker) => worker.id));
    for (const id of doneSince.current.keys()) if (!completed.has(id)) doneSince.current.delete(id);
    for (const id of completed) if (!doneSince.current.has(id)) doneSince.current.set(id, now);
    const held = new Set<string>();
    let nextRelease = Infinity;
    for (const [id, since] of doneSince.current) {
      const remaining = since + AUTO_DONE_HOLD_MS - now;
      if (remaining > 0) {
        held.add(id);
        nextRelease = Math.min(nextRelease, remaining);
      }
    }
    setLiveSlots((slots) =>
      autoFollow
        ? advanceAutoLiveSlots(slots, workers, held)
        : fillLiveSlots(slots, mainSessionId(currentSeg), PLANNER_ID, workers.map((worker) => worker.id).reverse()),
    );
    // Let completion glow finish before reusing a pane, even without another poll.
    if (autoFollow && Number.isFinite(nextRelease)) {
      const timer = setTimeout(() => setCompletionEpoch((value) => value + 1), nextRelease + 1);
      return () => clearTimeout(timer);
    }
  }, [liveMode, liveMainReady, currentSeg, workerSnapshot, autoFollow, taskId, completionEpoch]);
  const visibleKeys = liveMode
    ? visibleSlots
        .flatMap((slot) => {
          const session = sessions.find((item) => item.id === slot.sessionId);
          return session ? [keyForSession(session)] : [];
        })
        .join("|")
    : "";
  React.useEffect(() => {
    const keys = visibleKeys ? [...new Set(visibleKeys.split("|"))] : [];
    visibleKeysRef.current = keys;
    for (const [id, pane] of liveReadingRef.current) {
      if (!keys.includes(pane.key)) liveReadingRef.current.delete(id);
    }
    for (const key of keys) {
      // loadSession deduplicates in-flight loads, including the SSE bootstrap.
      if (!store[key]?.loaded && !store[key]?.loading && !store[key]?.error) loadSession(key);
      if (store[key]?.unread) patchStore(key, (value) => ({ ...value, unread: 0 }));
    }
  }, [visibleKeys, store, loadSession, patchStore]);
  React.useEffect(() => {
    if (approvalFocus.state && liveMode) onLiveModeChange?.(false);
  }, [approvalFocus.state, liveMode, onLiveModeChange]);

  function switchToManualLayout() {
    if (!autoFollow) return;
    setAutoFollow(false);
    setLiveCount(visibleSlots.length);
  }
  function toggleAutoFollow() {
    if (autoFollow) {
      switchToManualLayout();
      return;
    }
    setLiveSlots(advanceAutoLiveSlots(createAutoLiveSlots(PLANNER_ID), [...workerSessions].reverse()));
    setAutoFollow(true);
    setLiveLayout("3");
    setCustomDock(null);
    setActivePane(null);
  }
  function chooseLiveSession(id: string, index = activePane) {
    if (index === null) {
      const existing = visibleSlots.find((slot) => slot.sessionId === id);
      if (existing) setActivePane(liveSlots.findIndex((slot) => slot.id === existing.id));
      else toast.info(t("live.selectTarget"));
      return;
    }
    if (liveSlots[index]?.pinned) {
      toast.info(t("live.pinnedHint"));
      return;
    }
    switchToManualLayout();
    setLiveSlots((slots) => replaceLiveSlot(slots, index, id));
  }
  function addLiveSession(id: string, requestedTarget?: string, requestedZone: DockZone = "right") {
    let target = requestedTarget;
    let zone = requestedZone;
    if (!sessions.some((session) => session.id === id)) return;
    switchToManualLayout();
    const existing = visibleSlots.findIndex((slot) => slot.sessionId === id);
    if (existing >= 0) {
      setActivePane(existing);
      if (liveTree && target) setCustomDock(dockMove(liveTree, visibleSlots[existing].id, target, zone));
      return;
    }
    if (!target && liveTree) {
      // A click has no drop position: split the largest pane along its longer
      // screen dimension, instead of repeatedly halving the last narrow column.
      const geometry = dockGeometry(liveTree).panes;
      target = Object.keys(geometry).sort(
        (a, b) => geometry[b].width * geometry[b].height - geometry[a].width * geometry[a].height,
      )[0];
      const rect = geometry[target];
      const box = workspaceRef.current?.querySelector("[data-dock-workspace]")?.getBoundingClientRect();
      zone = rect.width * (box?.width || 900) >= rect.height * (box?.height || 500) ? "right" : "bottom";
    }
    const slot = { id: `pane-${++nextPane.current}`, sessionId: id, pinned: false };
    const nextTree =
      liveTree && target ? dockInsert(liveTree, slot.id, target, zone) : { kind: "pane" as const, id: slot.id };
    setLiveSlots([...visibleSlots, slot]);
    setLiveCount(visibleSlots.length + 1);
    setCustomDock(nextTree);
    setActivePane(visibleSlots.length);
  }
  function toggleGraph() {
    switchToManualLayout();
    if (graphOpen) {
      setCustomDock(liveTree ? dockRemove(liveTree, LIVE_GRAPH_ID) : null);
      setGraphOpen(false);
    } else {
      setCustomDock(
        liveTree
          ? {
              kind: "split",
              id: `graph-row-${++nextPane.current}`,
              axis: "y",
              ratio: 0.5,
              first: { kind: "pane", id: LIVE_GRAPH_ID },
              second: liveTree,
            }
          : { kind: "pane", id: LIVE_GRAPH_ID },
      );
      setGraphOpen(true);
    }
    setActivePane(null);
  }
  function closeLivePane(id: string) {
    const index = visibleSlots.findIndex((slot) => slot.id === id);
    if (index < 0 || !liveTree) return;
    switchToManualLayout();
    setLiveSlots(visibleSlots.filter((slot) => slot.id !== id));
    setLiveCount(visibleSlots.length - 1);
    setCustomDock(dockRemove(liveTree, id));
    setActivePane((active) => (active === null || active === index ? null : active > index ? active - 1 : active));
    liveReadingRef.current.delete(id);
  }
  function changeLiveLayout(id: LiveLayout) {
    const target = LIVE_LAYOUTS.find((item) => item.id === id);
    if (!target) return;
    const count = target.columns * target.rows;
    if (!canUseLiveLayout(liveSlots, count)) {
      toast.info(t("live.pinnedLayout"));
      return;
    }
    switchToManualLayout();
    setLiveSlots((slots) => {
      const next = [...slots];
      while (next.length < count) next.push({ id: `pane-${++nextPane.current}`, sessionId: null, pinned: false });
      return fillLiveSlots(
        next,
        mainSessionId(currentSeg),
        PLANNER_ID,
        workerSessions.map((s) => s.id),
      );
    });
    setLiveCount(count);
    setLiveLayout(id);
    setCustomDock(null);
    setActivePane((index) => (index === null ? null : Math.min(index, count - 1)));
  }
  const choices = sessions.map((session) => ({ session, title: sessionMeta.get(session.id)?.title ?? session.title }));
  const selectedLiveId = activePane === null ? undefined : liveSlots[activePane]?.sessionId;

  const grouped = {
    mainagent: sessions.filter((s) => s.role === "mainagent"),
    planner: sessions.filter((s) => s.role === "planner"),
    worker: sessions.filter((s) => s.role === "worker"),
    system: sessions.filter((s) => s.role === "system"),
  };

  const active = sessions.find((s) => s.id === activeId) ?? { ...MAIN_SESSION, title: t("title.main", { n: 1 }) };
  const mainSide = useSideQuestions(`/api/tasks/${taskId}/chat`);
  const workerSide = useSideQuestions(
    active.role === "worker" && !active.inherited && active.intent_id
      ? `/api/tasks/${taskId}/intents/${active.intent_id}`
      : null,
  );
  const side = active.role === "mainagent" ? mainSide : workerSide;
  const isMain = active.role === "mainagent";
  const isPlanner = active.role === "planner";
  const isSystem = active.role === "system";
  const activeKey = keyForSession(active);
  const activeState = store[activeKey];
  // The backend serializes main turns per task, including different visible segments.
  const mainBusy = sending || (mainChatRunning ?? mainSessions.some((session) => recentLive(keyForSession(session))));
  // 모바일 접힘 제목은 목록 전체를 대신하므로 현재 대화 이름 + 다른 대화의 읽지 않은 수 합계를 표시한다.
  // 그래야 접힌 상태에서도 현재 대화와 다른 곳의 새 메시지를 알 수 있다.
  const activeDisplayTitle = (active.role === "worker" ? sessionMeta.get(active.id)?.title : "") || active.title;
  const hiddenUnread = React.useMemo(
    () => Object.entries(store).reduce((sum, [key, s]) => (key === activeKey ? sum : sum + s.unread), 0),
    [store, activeKey],
  );

  // Keep the SSE dispatcher's notion of the active session current, and lazily load
  // + clear unread whenever the active session changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: cache updates must not reactivate the current session.
  React.useEffect(() => {
    activeKeyRef.current = activeKey;
    const st = store[activeKey];
    if (!st || (!st.loaded && !st.loading)) {
      loadSession(activeKey);
    } else if (st.unread) {
      patchStore(activeKey, (s) => ({ ...s, unread: 0 }));
    }
  }, [activeKey]);

  const focusKey = approvalFocus.state?.source?.session;
  const loadFocusPage = React.useCallback(
    (before: number) => api.activityHistory(taskId, focusKey ?? "main", before, PAGE),
    [taskId, focusKey],
  );
  const mergeFocusPage = React.useCallback(
    (page: { items: Activity[]; hasMore: boolean }) => {
      if (!focusKey) return;
      patchStore(focusKey, (s) => {
        const items = mergeBySeq(page.items, s.items);
        return { ...s, items, hasMore: page.hasMore, earliestSeq: items[0]?.seq ?? s.earliestSeq };
      });
    },
    [focusKey, patchStore],
  );
  const focusHistory = useApprovalHistory(
    approvalFocus.state?.source,
    !!focusKey && !!store[focusKey]?.loaded,
    focusKey ? (store[focusKey]?.items ?? []) : [],
    loadFocusPage,
    mergeFocusPage,
  );
  React.useEffect(() => {
    if (approvalFocus.state) atBottomRef.current = false;
  }, [approvalFocus.state]);

  // Main agent is the human↔orchestrator CONSOLE: only the conversation (user msgs +
  // the main agent's own replies/steps). Planner session shows planner steps; a
  // worker session shows only its intent's activity, led by the intent objective.
  const activity = React.useMemo(() => {
    const items = activeState?.items ?? [];
    // The main-agent console renders purely from server data: the human turn is
    // persisted+broadcast by the backend BEFORE it returns, so it arrives over the
    // same SSE stream (worker="mainagent") as every agent step — no client-side
    // optimistic echo, hence no fabricated seq that could collide with real DB ids.
    if (isMain) return items;
    if (isPlanner || isSystem) return items;
    // Worker session: the intent leads the transcript as a right-aligned "user"-style
    // message (the task handed to this worker), followed by its execution steps.
    const intentTitle = sessionMeta.get(active.id)?.title ?? active.title;
    const intentMsg: Activity = {
      seq: -1, // sorts/leads before any real step (real seq ≥ 0)
      worker: items[0]?.worker ?? active.id, // reuse the lane so no worker chips appear
      ts: active.last_activity || "",
      kind: "intent", // LLM-generated objective — rendered as a distinct (non-human) bubble
      summary: intentTitle,
      source_task_id: active.source_task_id,
      inherited: active.inherited,
    };
    return [intentMsg, ...items];
  }, [
    isMain,
    isPlanner,
    isSystem,
    activeState,
    active.id,
    active.title,
    active.last_activity,
    active.source_task_id,
    active.inherited,
    sessionMeta,
  ]);

  // seq of this session's most-recent TodoWrite call — for the Todo popover.
  const latestTodoSeq = React.useMemo(() => {
    for (let i = activity.length - 1; i >= 0; i--) {
      const a = activity[i];
      if (a.kind === "tool_use" && a.tool === "TodoWrite") return a.seq;
    }
    return null;
  }, [activity]);

  // Persisted result totals come from the backend's full activity history. Only
  // the latest unfinished usage frame is added client-side, so partial history
  // pages cannot undercount completed runs or double-count the active one.
  const tokenForSession = React.useCallback(
    (key: string): TokenTotal => {
      const saved = sessionTokens[key];
      const total: TokenTotal = {
        input_tokens: saved?.input_tokens ?? 0,
        output_tokens: saved?.output_tokens ?? 0,
        cache_read_tokens: saved?.cache_read_tokens ?? 0,
        cache_write_tokens: saved?.cache_write_tokens ?? 0,
      };
      const items = store[key]?.items ?? [];
      for (let index = items.length - 1; index >= 0; index--) {
        const item = items[index];
        if (item.kind === "result") break;
        if (item.kind !== "usage") continue;
        total.input_tokens += item.input_tokens ?? 0;
        total.output_tokens += item.output_tokens ?? 0;
        total.cache_read_tokens += item.cache_read_tokens ?? 0;
        total.cache_write_tokens += item.cache_write_tokens ?? 0;
        break;
      }
      return total;
    },
    [sessionTokens, store],
  );

  const activeTokens = tokenForSession(activeKey);
  const tokenTotal = {
    i: activeTokens.input_tokens,
    o: activeTokens.output_tokens,
    cr: activeTokens.cache_read_tokens,
    any:
      activeTokens.input_tokens +
        activeTokens.output_tokens +
        activeTokens.cache_read_tokens +
        activeTokens.cache_write_tokens >
      0,
  };

  // Run duration = span from this session's first step to its last (for a live
  // session, "now" so it ticks up — the 1.5s setTick above re-renders it).
  const runDuration = React.useMemo(() => {
    let min = Infinity,
      max = 0;
    for (const a of activity) {
      const t = Date.parse(a.ts);
      if (!Number.isFinite(t)) continue;
      if (t < min) min = t;
      if (t > max) max = t;
    }
    if (!Number.isFinite(min) || max === 0) return null;
    const end = active.live ? Date.now() : max;
    return Math.max(0, end - min);
  }, [activity, active.live]);

  // ---- transcript auto-scroll (open → bottom; stick to bottom unless scrolled up) ----
  const contentRef = React.useRef<HTMLDivElement | null>(null);
  const viewport = React.useCallback(
    () => (contentRef.current?.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null) ?? null,
    [],
  );
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId intentionally rebinds the scroll listener.
  React.useEffect(() => {
    const vp = viewport();
    if (!vp || liveMode) return;
    const onScroll = () => {
      if (approvalFocus.state && !focusHistory.ready) return;
      atBottomRef.current = vp.scrollTop + vp.clientHeight >= vp.scrollHeight - 60;
      if (vp.scrollTop <= 80) loadEarlier(activeKeyRef.current, viewport); // near top → older page
    };
    vp.addEventListener("scroll", onScroll, { passive: true });
    return () => vp.removeEventListener("scroll", onScroll);
  }, [viewport, activeId, loadEarlier, approvalFocus.state, focusHistory.ready, liveMode]);
  // open/switch a session → jump to the latest (bottom)
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId intentionally scrolls a newly selected session.
  React.useLayoutEffect(() => {
    const vp = viewport();
    if (vp && !approvalFocus.state && !liveMode) {
      vp.scrollTop = vp.scrollHeight;
      atBottomRef.current = true;
    }
  }, [activeId, viewport, approvalFocus.state, liveMode]);
  // new activity → stick to bottom only if the user is already pinned there
  // biome-ignore lint/correctness/useExhaustiveDependencies: activity growth intentionally drives live-edge scrolling.
  React.useLayoutEffect(() => {
    if (liveMode || approvalFocus.state || !atBottomRef.current) return;
    const vp = viewport();
    if (vp) vp.scrollTop = vp.scrollHeight;
  }, [activity, viewport, approvalFocus.state, liveMode]);
  // Lazy detail loads (AnswerBlock / ToolBlock / Markdown) grow the content AFTER the
  // activity array settles, WITHOUT changing its reference — so the layout effects
  // above never re-fire and a freshly opened session would leave its last message
  // scrolled partly off-screen (the final answer expands from a one-line summary to
  // full markdown below the fold). A ResizeObserver re-pins to the bottom on any
  // height change while the user is still at the bottom, so opening the main agent
  // lands on the last message fully shown. contentRef's div is always mounted, so the
  // observer catches the transcript mounting + each detail expanding.
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId intentionally rebinds the observer to the new session's content.
  React.useEffect(() => {
    const el = contentRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      if (liveMode || approvalFocus.state || !atBottomRef.current) return;
      const vp = viewport();
      if (vp) vp.scrollTop = vp.scrollHeight;
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [activeId, viewport, approvalFocus.state, liveMode]);

  function stop() {
    if (stopping) return;
    setStopping(true);
    void api
      .stopChat(taskId)
      .catch((e: Error) => toast.error(e.message))
      .finally(() => setStopping(false));
  }

  function sendMain(seg: number) {
    const draft = mainDrafts[seg] ?? EMPTY_MAIN_DRAFT;
    const text = draft.text.trim();
    const atts = draft.attachments;
    if (mainSide.handleCommand(text, () => updateMainDraft(seg, (d) => ({ ...d, text: "" })))) return;
    if ((!text && !atts.length) || mainSendingRef.current || mainBusy || draft.uploading) return;
    mainSendingRef.current = true;
    updateMainDraft(seg, (d) => ({ ...d, text: "", attachments: [] }));
    setSending(true);
    chatStatusRequestRef.current++;
    api
      .chat(text, taskId, atts.length ? atts : undefined, seg)
      .then(({ mode }) => {
        chatStatusRequestRef.current++;
        setMainChatRunning(mode === "llm");
      })
      .catch((e: Error) => {
        updateMainDraft(seg, (d) => ({
          ...d,
          text: d.text ? `${text}\n${d.text}` : text,
          attachments: [...atts, ...d.attachments],
        }));
        toast.error(t("toast.sendFailed", { msg: e.message || t("toast.retryLater") }));
      })
      .finally(() => {
        mainSendingRef.current = false;
        setSending(false);
      });
  }

  function mainComposer(seg: number, live = false) {
    const draft = mainDrafts[seg] ?? EMPTY_MAIN_DRAFT;
    return (
      <MainChatComposer
        input={draft.text}
        attachments={draft.attachments}
        uploading={draft.uploading}
        mainBusy={mainBusy}
        sending={sending}
        stopping={stopping}
        onChange={(text) => updateMainDraft(seg, (d) => ({ ...d, text }))}
        onRemove={(path) =>
          updateMainDraft(seg, (d) => ({ ...d, attachments: d.attachments.filter((a) => a.path !== path) }))
        }
        onUpload={(files) => pickFiles(seg, files)}
        onSend={() => sendMain(seg)}
        onStop={stop}
        sideButton={live ? <SideQuestionButton side={mainSide} /> : undefined}
      />
    );
  }

  function sendWorkerChat() {
    const intentId = active.intent_id;
    const message = workerMessage.trim();
    if (side.handleCommand(message, () => setWorkerMessage(""))) return;
    if (!intentId || active.inherited || active.status !== "paused" || workerMessageSending || !message) return;
    if (workerMessageCharCount(message) > MAX_WORKER_MESSAGE_CHARS) {
      toast.error(t("toast.messageTooLong", { max: MAX_WORKER_MESSAGE_CHARS }));
      return;
    }
    const requestId = workerMessageRequestId || newWorkerMessageRequestID();
    if (!workerMessageRequestId) setWorkerMessageRequestId(requestId);
    setWorkerMessageSending(true);
    api
      .sendWorkerMessage(taskId, intentId, message, requestId)
      .then((result) => {
        // The server records the user turn and streams the run over SSE, so there is
        // no optimistic insert: the message and the continuation arrive live.
        patchIntentState(intentId, result.state);
        setWorkerMessage("");
        setWorkerMessageRequestId("");
        toast.success(t("toast.workerMessageSent", { id: intentId }));
      })
      .catch((error) => {
        toast.error(t("toast.sendFailed", { msg: (error as Error).message || t("toast.retryLater") }));
      })
      .finally(() => setWorkerMessageSending(false));
  }

  const mainLoaded = !!store[currentMainKey]?.loaded;
  // 전송 키는 시스템 설정(localStorage)으로 결정하며 기본은 Enter다.
  const sendMode = useChatSendMode();
  // What the transcript pane should show for the active session.
  const showLoader = !activeState || (activeState.loading && !activeState.loaded);
  const resolutionForSession = (session: Session): TaskLLMResolution | undefined => {
    if (!llmResolutions || session.inherited || session.role === "system") return undefined;
    if (session.role === "mainagent") return llmResolutions.mainagent;
    if (session.role === "planner") return llmResolutions.planner;
    return llmResolutions.worker;
  };
  const activeResolution = resolutionForSession(active);
  const activeAssets =
    active.role === "worker" && active.intent_id ? intentAssetsByID.get(active.intent_id) : undefined;

  const workspaceRef = React.useRef<HTMLDivElement>(null);
  const [dockToolbar, setDockToolbar] = React.useState<HTMLDivElement | null>(null);
  React.useLayoutEffect(() => {
    if (!liveMode) return;
    const el = workspaceRef.current;
    if (!el) return;
    let frame = 0;
    const measure = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        el.style.height = `${Math.max(260, (window.visualViewport?.height ?? window.innerHeight) - el.getBoundingClientRect().top - 6)}px`;
      });
    };
    measure();
    const observer = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    const header = document.querySelector("[data-task-header]");
    if (header) observer?.observe(header);
    window.addEventListener("resize", measure);
    window.visualViewport?.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      cancelAnimationFrame(frame);
      window.removeEventListener("resize", measure);
      window.visualViewport?.removeEventListener("resize", measure);
      el.style.height = "";
    };
  }, [liveMode]);

  return (
    <TooltipProvider delayDuration={300}>
      {/* 높이 예약: 페이지 머리(제목 + 목표 + Tabs ≈ 7.5rem) + 안쪽 여백. 모바일 p-4,
        데스크톱 lg:p-6이며 데스크톱은 스크롤 여유도 필요해 각각 10rem/13rem을 예약한다.
        모바일에도 13rem을 쓰면 기록 높이 3rem을 불필요하게 잃는다. */}
      <div
        ref={workspaceRef}
        style={
          liveMode
            ? { gridTemplateColumns: liveListHidden ? "minmax(0,1fr)" : `${liveListWidth}px 8px minmax(0,1fr)` }
            : undefined
        }
        className={cn(
          "grid h-[calc(100svh-10rem)] min-h-0 grid-cols-1 gap-4",
          listOpen ? "grid-rows-[minmax(0,40svh)_minmax(0,1fr)]" : "grid-rows-[auto_minmax(0,1fr)]",
          "lg:h-[calc(100svh-13rem)] lg:grid-cols-[18rem_1fr] lg:grid-rows-[minmax(0,1fr)]",
          liveMode &&
            "grid-rows-[auto_minmax(0,1fr)] lg:grid-rows-[auto_minmax(0,1fr)] gap-0 transition-[height] duration-300 motion-reduce:transition-none",
        )}
      >
        {liveMode && (
          <div data-live-toolbar className="col-span-full mb-1 flex min-h-7 min-w-0 items-center gap-2">
            <Button
              size="icon-sm"
              className="size-7"
              variant="ghost"
              onClick={() => setLiveListHidden((hidden) => !hidden)}
              aria-label={t(liveListHidden ? "live.showList" : "live.hideList")}
              title={t(liveListHidden ? "live.showList" : "live.hideList")}
            >
              {liveListHidden ? <PanelLeftOpenIcon /> : <PanelLeftCloseIcon />}
            </Button>
            <span className="hidden truncate text-xs text-muted-foreground sm:inline">
              {activePane === null ? t("live.selectTarget") : t("live.target", { n: activePane + 1 })}
            </span>
            <span className="ml-auto text-xs text-muted-foreground">
              {t("live.workers", {
                running: workerSessions.filter((item) => item.status === "running").length,
                done: workerSessions.filter((item) => item.status === "done").length,
              })}
            </span>
            <Button
              size="sm"
              variant={graphOpen ? "secondary" : "ghost"}
              className="h-7 px-2"
              onClick={toggleGraph}
              aria-pressed={graphOpen}
              aria-label={t(graphOpen ? "live.graphClose" : "live.graphAdd")}
              title={t(graphOpen ? "live.graphClose" : "live.graphAdd")}
            >
              <NetworkIcon className="size-3.5" />
              <span className="text-xs">{t("live.graph")}</span>
            </Button>
            <Button
              size="sm"
              variant={autoFollow ? "secondary" : "ghost"}
              className="h-7 px-2 text-xs"
              aria-pressed={autoFollow}
              aria-label={t("live.autoFollow")}
              title={t("live.autoFollowHint")}
              onClick={toggleAutoFollow}
            >
              {t("live.autoFollow")}
            </Button>
            <div ref={setDockToolbar} />
            <Popover>
              <PopoverTrigger asChild>
                <Button
                  size="sm"
                  variant="ghost"
                  className="h-7 px-2"
                  aria-label={t("live.layout")}
                  title={t("live.layout")}
                >
                  <PanelsTopLeftIcon className="size-3.5" />
                  {!customDock && <span className="text-xs">{liveLayout.replace("x", "×")}</span>}
                </Button>
              </PopoverTrigger>
              <PopoverContent align="end" className="w-auto p-2">
                <fieldset className="flex flex-wrap items-center gap-1" aria-label={t("live.layout")}>
                  {LIVE_LAYOUTS.map((item) => (
                    <Button
                      key={item.id}
                      variant={!customDock && item.id === liveLayout ? "secondary" : "ghost"}
                      size="sm"
                      className="h-7 px-2"
                      aria-pressed={!customDock && item.id === liveLayout}
                      aria-label={t("live.layoutOption", { columns: item.columns, rows: item.rows })}
                      onClick={() => changeLiveLayout(item.id)}
                    >
                      {item.id.replace("x", "×")}
                    </Button>
                  ))}
                </fieldset>
                <Button
                  size="sm"
                  variant="ghost"
                  className="mt-2 w-full"
                  disabled={!liveTree}
                  onClick={() => {
                    if (!liveTree) return;
                    switchToManualLayout();
                    setCustomDock(
                      dockGrid(
                        [...visibleSlots.map((slot) => slot.id), ...(graphOpen ? [LIVE_GRAPH_ID] : [])],
                        Math.max(1, Math.ceil(Math.sqrt(visibleSlots.length + (graphOpen ? 1 : 0)))),
                      ),
                    );
                  }}
                >
                  {t("live.balancePanes")}
                </Button>
              </PopoverContent>
            </Popover>
          </div>
        )}
        {/* Left: session list */}
        <div
          data-session-list
          className={cn(
            "flex min-h-0 flex-col overflow-hidden rounded-lg border bg-card",
            liveMode && "my-1",
            liveMode && liveListHidden && "hidden",
          )}
        >
          <div className="border-b px-3 py-2">
            <div className="flex items-center justify-between gap-2">
              <button
                type="button"
                onClick={() => setListOpen((open) => !open)}
                aria-expanded={listOpen}
                aria-controls="session-list-panel"
                className="flex min-w-0 flex-1 items-center gap-1 text-left text-xs font-medium text-muted-foreground lg:pointer-events-none"
              >
                <ChevronDownIcon
                  className={cn("size-3.5 shrink-0 transition-transform lg:hidden", !listOpen && "-rotate-90")}
                />
                <span className="shrink-0">{t("list.title")}</span>
                {!listOpen && (
                  <>
                    <span className="min-w-0 truncate text-foreground lg:hidden" title={activeDisplayTitle}>
                      · {activeDisplayTitle}
                    </span>
                    {hiddenUnread > 0 && (
                      <span className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 lg:hidden dark:text-blue-400">
                        {hiddenUnread > 99 ? "99+" : hiddenUnread}
                      </span>
                    )}
                  </>
                )}
              </button>
              {!MOCK && (
                <span
                  className={cn(
                    "inline-flex items-center gap-1 text-[10px]",
                    sseLive ? "text-emerald-500" : "text-amber-500",
                  )}
                  title={sseLive ? t("sse.titleLive") : t("sse.titleDown")}
                >
                  {sseLive ? (
                    <>
                      <span className="size-1 animate-pulse rounded-full bg-emerald-500" />
                      {t("sse.live")}
                    </>
                  ) : (
                    <>
                      <WifiOffIcon className="size-3" />
                      {t("sse.reconnecting")}
                    </>
                  )}
                </span>
              )}
            </div>
            {taskTokens && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <div className="mt-2 flex min-w-0 flex-wrap items-center gap-1 text-xs text-muted-foreground">
                    <span>{t("taskTotal")}</span>
                    <span>·</span>
                    <TokenMetrics
                      input={taskTokens.input_tokens}
                      cache={taskTokens.cache_read_tokens}
                      output={taskTokens.output_tokens}
                    />
                  </div>
                </TooltipTrigger>
                <TooltipContent>
                  {t("tokens.full", {
                    input: taskTokens.input_tokens.toLocaleString(),
                    output: taskTokens.output_tokens.toLocaleString(),
                    cacheRead: taskTokens.cache_read_tokens.toLocaleString(),
                    cacheWrite: taskTokens.cache_write_tokens.toLocaleString(),
                  })}
                </TooltipContent>
              </Tooltip>
            )}
          </div>
          <ScrollArea
            id="session-list-panel"
            type="auto"
            className={cn(
              "min-h-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!",
              !listOpen && !liveMode && "max-lg:hidden",
            )}
          >
            <div className="flex w-full flex-col gap-3 p-2">
              {(["mainagent", "planner", "system", "worker"] as const).map((role) => {
                const items = grouped[role];
                if (!items.length) return null;
                const Meta = roleMeta[role];
                return (
                  <div key={role} className="flex flex-col gap-0.5">
                    <div className="flex items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted-foreground">
                      <Meta.icon className="size-3.5" />
                      {t(`role.${role}`)}
                      {role === "mainagent" && (
                        <button
                          type="button"
                          onClick={() => setConfirmNewMain(true)}
                          disabled={creatingMain}
                          title={t("newMain.title")}
                          aria-label={t("newMain.aria")}
                          className="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:bg-accent/60 hover:text-foreground disabled:opacity-50"
                        >
                          {creatingMain ? (
                            <Loader2Icon className="size-3.5 animate-spin" />
                          ) : (
                            <PlusIcon className="size-3.5" />
                          )}
                          {t("newMain.label")}
                        </button>
                      )}
                    </div>
                    {items.map((s) => {
                      const meta = s.role === "worker" ? sessionMeta.get(s.id) : undefined;
                      return (
                        <SessionItem
                          key={s.id}
                          onAdd={liveMode ? () => addLiveSession(s.id) : undefined}
                          s={s}
                          active={s.id === (liveMode ? selectedLiveId : activeId)}
                          displayTitle={meta?.title ?? s.title}
                          hasPending={hasPendingForSession(s)}
                          unread={store[keyForSession(s)]?.unread}
                          onClick={() => {
                            if (liveMode) {
                              chooseLiveSession(s.id);
                              return;
                            }
                            approvalFocus.close();
                            setActiveId(s.id);
                            setListOpen(false); // 모바일에서 선택 후 접어 기록에 높이를 돌려준다.
                            setWorkerMessage("");
                            setWorkerMessageRequestId("");
                          }}
                          controlling={controllingIntent === s.intent_id}
                          deleted={meta?.deleted}
                          onCancel={() => {
                            setCancelIntent(s);
                          }}
                        />
                      );
                    })}
                    {role === "worker" && intentsHasMore && (
                      <button
                        type="button"
                        onClick={loadOlderIntents}
                        disabled={loadingOlderIntents}
                        className="mt-0.5 flex items-center justify-center gap-1 rounded-md px-2 py-1 text-xs text-muted-foreground hover:bg-accent/50"
                      >
                        {loadingOlderIntents ? (
                          <Loader2Icon className="size-3.5 animate-spin" />
                        ) : (
                          <RotateCwIcon className="size-3.5" />
                        )}
                        {t("worker.loadOlder")}
                      </button>
                    )}
                  </div>
                );
              })}
              {mainLoaded && !workerSessions.length && (
                <div className="px-2 py-1 text-xs text-muted-foreground">{t("worker.noRunning")}</div>
              )}
            </div>
          </ScrollArea>
        </div>

        {liveMode && !liveListHidden && (
          <hr
            aria-label={t("live.resizeList")}
            aria-orientation="vertical"
            aria-valuemin={240}
            aria-valuemax={480}
            aria-valuenow={liveListWidth}
            tabIndex={0}
            className="h-full w-2 cursor-col-resize touch-none border-0 rounded hover:bg-accent focus-visible:bg-accent"
            onKeyDown={(e) => {
              if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
              e.preventDefault();
              setLiveListWidth((width) =>
                e.key === "Home"
                  ? 240
                  : e.key === "End"
                    ? 480
                    : Math.max(240, Math.min(480, width + (e.key === "ArrowRight" ? 20 : -20))),
              );
            }}
            onPointerDown={(e) => {
              if (e.button !== 0) return;
              e.preventDefault();
              const el = e.currentTarget,
                start = e.clientX,
                width = liveListWidth;
              el.setPointerCapture(e.pointerId);
              el.onpointermove = (event) =>
                setLiveListWidth(Math.max(240, Math.min(480, width + event.clientX - start)));
              const end = () => {
                el.onpointermove = null;
              };
              el.onpointerup = end;
              el.onpointercancel = end;
              el.onlostpointercapture = end;
            }}
          />
        )}
        {liveMode && (
          <div className="flex min-h-0 min-w-0 flex-col" data-live-workspace>
            <SideQuestionWorkspace side={mainSide} label={t("role.mainagent")}>
              {!liveTree ? (
                <button
                  type="button"
                  onClick={() => setLiveListHidden(false)}
                  className="m-1 flex flex-1 items-center justify-center rounded-lg border border-dashed p-8 text-sm text-muted-foreground"
                  data-empty-dock
                  onDragOver={(e) => {
                    if (e.dataTransfer.types.includes(SESSION_DRAG_TYPE)) {
                      e.preventDefault();
                      e.dataTransfer.dropEffect = "copy";
                    }
                  }}
                  onDrop={(e) => {
                    if (e.dataTransfer.types.includes(SESSION_DRAG_TYPE)) {
                      e.preventDefault();
                      addLiveSession(e.dataTransfer.getData(SESSION_DRAG_TYPE));
                    }
                  }}
                >
                  {t("live.emptyWorkspace")}
                </button>
              ) : (
                <SessionDockWorkspace
                  externalDrop={{ type: SESSION_DRAG_TYPE, label: t("live.addPane"), onDrop: addLiveSession }}
                  toolbarContainer={dockToolbar}
                  tree={liveTree}
                  onChange={(tree) => {
                    switchToManualLayout();
                    setCustomDock(tree);
                  }}
                  panes={[
                    ...(graphOpen
                      ? [
                          {
                            id: LIVE_GRAPH_ID,
                            label: t("live.graph"),
                            minimumSize: { width: 320, height: 240 },
                            render: (grip: DockGripProps) => (
                              <LiveGraphPane key={taskId} taskId={taskId} grip={grip} onClose={toggleGraph} />
                            ),
                          },
                        ]
                      : []),
                    ...visibleSlots.map((slot, index) => {
                      const session = sessions.find((item) => item.id === slot.sessionId);
                      const key = session ? keyForSession(session) : "";
                      const title = session ? (sessionMeta.get(session.id)?.title ?? session.title) : "";
                      return {
                        id: slot.id,
                        label: `${index + 1}. ${title}`,
                        render: (dockGrip: DockGripProps) => (
                          <LiveSessionPane
                            composer={session?.role === "mainagent" ? mainComposer(session.seg ?? 0, true) : undefined}
                            dockGrip={{
                              ...dockGrip,
                              onToggleExpand: () => {
                                switchToManualLayout();
                                dockGrip.onToggleExpand?.();
                              },
                            }}
                            key={`${slot.id}:${slot.sessionId}`}
                            paneId={slot.id}
                            number={index + 1}
                            taskId={taskId}
                            session={session}
                            title={title}
                            history={store[key]}
                            choices={choices}
                            active={activePane === index}
                            pinned={slot.pinned}
                            connected={sseLive || MOCK}
                            pendingApproval={!!session && hasPendingForSession(session)}
                            memory={liveScrollMemory}
                            onActivate={() => setActivePane(index)}
                            onClose={() => closeLivePane(slot.id)}
                            onSelect={(id) => chooseLiveSession(id, index)}
                            onPin={() => {
                              switchToManualLayout();
                              setLiveSlots((slots) =>
                                slots.map((item) => (item.id === slot.id ? { ...item, pinned: !item.pinned } : item)),
                              );
                            }}
                            onOpen={() => {
                              if (!session) return;
                              setActiveId(session.id);
                              approvalFocus.close();
                              onLiveModeChange?.(false);
                            }}
                            onRetry={() => loadSession(key)}
                            onLoadEarlier={(viewport) => loadEarlier(key, viewport)}
                            onFollowing={(following) => liveReadingRef.current.set(slot.id, { key, following })}
                          />
                        ),
                      };
                    }),
                  ]}
                />
              )}
            </SideQuestionWorkspace>
          </div>
        )}
        {/* Right: original interactive session, kept mounted to preserve drafts. */}
        <div className={cn("flex min-h-0 min-w-0", liveMode && "hidden")}>
          <SideQuestionWorkspace
            side={{ ...side, open: !liveMode && side.open }}
            label={
              active.role === "worker" ? `Worker #${active.intent_id} · ${activeDisplayTitle}` : activeDisplayTitle
            }
          >
            <div className="flex min-h-0 flex-1 min-w-0 flex-col overflow-hidden rounded-lg border bg-card">
              <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 border-b px-3 py-2 sm:px-4 sm:py-2.5">
                {(() => {
                  const isWorker = active.role === "worker";
                  const meta = isWorker ? sessionMeta.get(active.id) : undefined;
                  // Worker: the intent moved into the transcript as a message, so the
                  // header shows a stable generic label (intent JSON stays on hover).
                  const title = isWorker ? t("worker.execSession") : active.title;
                  const titleEl = <span className="min-w-0 truncate text-sm font-medium">{title}</span>;
                  return meta?.json ? (
                    <Tooltip>
                      <TooltipTrigger asChild>{titleEl}</TooltipTrigger>
                      <TooltipContent side="bottom" align="start" className="max-h-80 max-w-sm overflow-auto p-0">
                        <pre className="p-2 text-[10px] leading-relaxed">{JSON.stringify(meta.json, null, 2)}</pre>
                      </TooltipContent>
                    </Tooltip>
                  ) : (
                    titleEl
                  );
                })()}
                <SideQuestionButton side={side} />
                {activeResolution && (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Badge
                        variant="outline"
                        className="max-w-28 shrink-0 font-normal"
                        aria-label={
                          activeResolution.available
                            ? t("resolution.current", {
                                label: resolutionLabel(activeResolution, t("resolution.unnamed")),
                              })
                            : t("resolution.unavailable")
                        }
                      >
                        <span className="truncate">
                          {activeResolution.available
                            ? resolutionLabel(activeResolution, t("resolution.unnamed"))
                            : t("resolution.unavailable")}
                        </span>
                      </Badge>
                    </TooltipTrigger>
                    <TooltipContent side="bottom" className="max-w-xs [overflow-wrap:anywhere]">
                      {activeResolution.available
                        ? [resolutionLabel(activeResolution, t("resolution.unnamed")), activeResolution.model]
                            .filter(Boolean)
                            .join(" / ")
                        : activeResolution.reason || t("resolution.noConfig")}
                    </TooltipContent>
                  </Tooltip>
                )}
                {activeAssets && activeAssets.length > 0 && <WorkerAssetBadge assets={activeAssets} />}
                {active.inherited && active.source_task_id && (
                  <Badge variant="outline">{t("header.sourceTask", { id: active.source_task_id })}</Badge>
                )}
                {active.live && (
                  <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
                    <span className="size-1 animate-pulse rounded-full bg-blue-500" />
                    {t("sse.live")}
                  </span>
                )}
                {activeState?.hasMore && (
                  <span className="text-[10px] text-muted-foreground" title={t("header.olderHistoryTitle")}>
                    {t("header.olderHistoryLabel")}
                  </span>
                )}
                <div className="ml-auto flex min-w-0 max-w-full items-center justify-end gap-x-3 gap-y-1 text-xs text-muted-foreground max-sm:w-full max-sm:flex-wrap">
                  {tokenTotal.any && (
                    <Tooltip>
                      {/* 모바일은 짧은 라벨(입력/캐시/출력)을 써 줄이 두 줄로 늘어나 기록 영역이 줄지 않게 한다. */}
                      <TooltipTrigger asChild>
                        <span className="inline-flex min-w-0 items-center">
                          <TokenMetrics
                            input={tokenTotal.i}
                            cache={tokenTotal.cr}
                            output={tokenTotal.o}
                            labels="short"
                            className="sm:hidden"
                          />
                          <TokenMetrics
                            input={tokenTotal.i}
                            cache={tokenTotal.cr}
                            output={tokenTotal.o}
                            labels="long"
                            className="max-sm:hidden"
                          />
                        </span>
                      </TooltipTrigger>
                      <TooltipContent>
                        {t("tokens.full", {
                          input: activeTokens.input_tokens.toLocaleString(),
                          output: activeTokens.output_tokens.toLocaleString(),
                          cacheRead: activeTokens.cache_read_tokens.toLocaleString(),
                          cacheWrite: activeTokens.cache_write_tokens.toLocaleString(),
                        })}
                      </TooltipContent>
                    </Tooltip>
                  )}
                  {runDuration != null && (
                    <span className="inline-flex items-center gap-1" title={t("header.runDurationTitle")}>
                      <ClockIcon className="size-3" />
                      {fmtDuration(runDuration)}
                    </span>
                  )}
                  {isMain && <span>{t("header.interactive")}</span>}
                </div>
              </div>
              {(() => {
                const dm = active.role === "worker" ? sessionMeta.get(active.id) : undefined;
                if (!dm?.deleted) return null;
                return (
                  <div className="flex items-start gap-2 border-b border-destructive/30 bg-destructive/5 px-4 py-2.5 text-xs">
                    <Trash2Icon className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                    <div className="min-w-0">
                      <span className="font-medium text-destructive">{t("deletedNotice.heading")}</span>
                      <span className="text-muted-foreground">{t("deletedNotice.detail")}</span>
                      {dm.deleteReason && (
                        <p className="mt-1 break-words text-foreground">
                          <span className="text-muted-foreground">{t("deletedNotice.reason")}</span>
                          {dm.deleteReason}
                        </p>
                      )}
                    </div>
                  </div>
                );
              })()}
              <ApprovalExecutionFocus
                focus={{
                  ...approvalFocus,
                  close: () => {
                    setActiveId(activeId);
                    approvalFocus.close();
                  },
                }}
                history={focusHistory}
              />
              {/* Force Radix's internal viewport wrapper (display:table, sizes to content)
            to block so wide/unbreakable steps (long commands, code, URLs) can't blow
            out the width and defeat the truncation below — the transcript wraps to
            the panel instead of overflowing horizontally. */}
              <ScrollArea
                type="auto"
                className="min-h-0 min-w-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!"
              >
                <div className="min-w-0 max-w-full p-4" ref={contentRef}>
                  {activeState?.loadingMore && (
                    <div className="flex items-center justify-center gap-2 pb-2 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      {t("transcript.loadingMore")}
                    </div>
                  )}
                  {showLoader ? (
                    <div className="flex items-center gap-2 pl-9 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      {t("transcript.loadingActivity")}
                    </div>
                  ) : activeState?.error ? (
                    <div className="flex items-center gap-2 pl-9 text-xs text-red-500">
                      <CircleXIcon className="size-3.5" />
                      {t("transcript.loadError", { error: activeState.error })}
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-6 px-2 text-xs"
                        onClick={() => loadSession(activeKey)}
                      >
                        {t("transcript.retry")}
                      </Button>
                    </div>
                  ) : activity.length ? (
                    <Transcript
                      activity={activity}
                      live={active.live}
                      taskId={taskId}
                      chat={isMain}
                      focusedSeq={focusHistory.ready ? approvalFocus.state?.source?.seq : undefined}
                    />
                  ) : (
                    <div className="pl-9 text-xs text-muted-foreground">
                      {isMain ? t("transcript.emptyMain") : t("transcript.emptyWorker")}
                    </div>
                  )}
                </div>
              </ScrollArea>
              {isMain ? (
                mainComposer(active.seg ?? 0)
              ) : active.role === "worker" &&
                !active.inherited &&
                (active.status === "running" || active.status === "paused") ? (
                <div className="border-t p-3">
                  <InputGroup className="min-h-9 has-disabled:opacity-100">
                    <MentionTextarea
                      inputGroup
                      rows={1}
                      aria-label={t("composer.workerAria", { id: active.intent_id ?? "" })}
                      placeholder={t("composer.workerPlaceholder", { id: active.intent_id ?? "" })}
                      value={workerMessage}
                      onValueChange={(value) => {
                        setWorkerMessage(value);
                        setWorkerMessageRequestId("");
                      }}
                      onKeyDown={(event) => {
                        if (!shouldSubmitOnKey(event, sendMode)) return;
                        event.preventDefault();
                        sendWorkerChat();
                      }}
                      disabled={workerMessageSending}
                      aria-invalid={workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS}
                      className="max-h-36 min-h-9 overflow-y-auto"
                    />
                    <InputGroupAddon align="block-end">
                      <span
                        className={cn(
                          "px-1 text-[10px] tabular-nums text-muted-foreground",
                          workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS && "text-destructive",
                        )}
                      >
                        {workerMessageCharCount(workerMessage)}/{MAX_WORKER_MESSAGE_CHARS}
                      </span>
                      {active.status === "running" && isBtwCommand(workerMessage) && (
                        <InputGroupButton
                          size="icon-xs"
                          onClick={sendWorkerChat}
                          aria-label={t("composer.sideQuestion")}
                        >
                          <ArrowUpIcon />
                        </InputGroupButton>
                      )}
                      {active.status === "running" ? (
                        <InputGroupButton
                          className="ml-auto"
                          size="icon-xs"
                          variant="destructive"
                          onClick={() => void controlWorker(active, "pause")}
                          disabled={controllingIntent === active.intent_id}
                          title={t("composer.pauseTitle")}
                          aria-label={t("composer.pauseTitle")}
                        >
                          {controllingIntent === active.intent_id ? (
                            <Loader2Icon className="animate-spin" />
                          ) : (
                            <SquareIcon />
                          )}
                        </InputGroupButton>
                      ) : (
                        <>
                          <InputGroupButton
                            className="ml-auto"
                            size="xs"
                            variant="ghost"
                            onClick={() => void controlWorker(active, "resume")}
                            disabled={controllingIntent === active.intent_id || workerMessageSending}
                            title={t("composer.resumeTitle")}
                            aria-label={t("composer.resumeAria")}
                          >
                            {controllingIntent === active.intent_id ? (
                              <Loader2Icon className="animate-spin" />
                            ) : (
                              t("composer.resumeLabel")
                            )}
                          </InputGroupButton>
                          <InputGroupButton
                            size="icon-xs"
                            variant="default"
                            onClick={sendWorkerChat}
                            disabled={
                              workerMessageSending ||
                              !workerMessage.trim() ||
                              workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS
                            }
                            title={t("composer.sendTitle")}
                            aria-label={t("composer.sendTitle")}
                          >
                            {workerMessageSending ? <Spinner /> : <ArrowUpIcon />}
                          </InputGroupButton>
                        </>
                      )}
                    </InputGroupAddon>
                  </InputGroup>
                </div>
              ) : (
                <div className="flex items-center border-t px-4 py-2">
                  <TodoPopover
                    seq={latestTodoSeq}
                    fetchDetail={(seq) => api.activityDetail(seq, taskId).then((r) => r.detail ?? "")}
                  />
                </div>
              )}
            </div>
          </SideQuestionWorkspace>
        </div>
        <AlertDialog
          open={cancelIntent !== null}
          onOpenChange={(open) => {
            if (!open) {
              setCancelIntent(null);
              setCancelReason("");
              setDeleteMode("soft");
            }
          }}
        >
          <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
            <AlertDialogHeader>
              <AlertDialogTitle>{t("deleteDialog.title", { id: cancelIntent?.intent_id ?? "" })}</AlertDialogTitle>
              <AlertDialogDescription className="break-words whitespace-normal">
                {deleteMode === "hard"
                  ? t.rich("deleteDialog.hardDesc", { s: (c) => <strong>{c}</strong> })
                  : t.rich("deleteDialog.softDesc", { s: (c) => <strong>{c}</strong> })}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="grid gap-3 py-1">
              <div className="grid grid-cols-2 gap-2">
                <button
                  type="button"
                  onClick={() => setDeleteMode("soft")}
                  className={cn(
                    "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                    deleteMode === "soft" ? "border-primary bg-primary/5" : "hover:bg-accent",
                  )}
                >
                  <div className="font-medium">{t("deleteDialog.softLabel")}</div>
                  <div className="text-xs text-muted-foreground">{t("deleteDialog.softHint")}</div>
                </button>
                <button
                  type="button"
                  onClick={() => setDeleteMode("hard")}
                  className={cn(
                    "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                    deleteMode === "hard" ? "border-destructive bg-destructive/5" : "hover:bg-accent",
                  )}
                >
                  <div className="font-medium">{t("deleteDialog.hardLabel")}</div>
                  <div className="text-xs text-muted-foreground">{t("deleteDialog.hardHint")}</div>
                </button>
              </div>
              <div className="grid gap-2">
                <label htmlFor="cancel-reason" className="text-sm font-medium">
                  {t("deleteDialog.reasonLabel")}
                </label>
                <Textarea
                  id="cancel-reason"
                  value={cancelReason}
                  onChange={(e) => setCancelReason(e.target.value)}
                  placeholder={t("deleteDialog.reasonPlaceholder")}
                  rows={3}
                  autoFocus
                />
              </div>
            </div>
            <AlertDialogFooter>
              <AlertDialogCancel>{t("deleteDialog.back")}</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                disabled={!cancelIntent || controllingIntent !== null || !cancelReason.trim()}
                onClick={() => cancelIntent && void controlWorker(cancelIntent, "cancel", cancelReason, deleteMode)}
              >
                {controllingIntent ? <Loader2Icon className="animate-spin" /> : <Trash2Icon />}
                {deleteMode === "hard" ? t("deleteDialog.confirmHard") : t("deleteDialog.confirmSoft")}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>

        <AlertDialog open={confirmNewMain} onOpenChange={(open) => !open && setConfirmNewMain(false)}>
          <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
            <AlertDialogHeader>
              <AlertDialogTitle>{t("newSession.title")}</AlertDialogTitle>
              <AlertDialogDescription className="break-words whitespace-normal">
                {t("newSession.desc")}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={creatingMain}>{t("newSession.cancel")}</AlertDialogCancel>
              <AlertDialogAction disabled={creatingMain} onClick={() => void createMainSession()}>
                {creatingMain ? t("newSession.starting") : t("newSession.start")}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </div>
    </TooltipProvider>
  );
}
