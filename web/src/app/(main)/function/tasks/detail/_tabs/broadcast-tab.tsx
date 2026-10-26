"use client";

import * as React from "react";

import {
  ArrowDownIcon,
  ArrowUpIcon,
  ArrowUpToLineIcon,
  BugIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  CompassIcon,
  FlagIcon,
  FlaskConicalIcon,
  LayersIcon,
  LightbulbIcon,
  type LucideIcon,
  PauseIcon,
  PlayIcon,
  SearchIcon,
  TargetIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter } from "@/components/ui/card";
import { HoverCard, HoverCardContent, HoverCardTrigger } from "@/components/ui/hover-card";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { getDateLocale } from "@/i18n/datetime";
import { api } from "@/lib/api";
import { type Tone, toneClasses, toneDot } from "@/lib/status";
import type { Edge, ExploreKind, FindingAsset, TaskNode } from "@/lib/types";
import { cn } from "@/lib/utils";

const PAGE_SIZES = [20, 50, 100];
const POLL_MS = 8000;

type KindMeta = { icon: LucideIcon; dot: string; chip: string };

// 방송판 전용 표시 메타데이터. 탐색 경로 그래프는 노드 카드와 연결선의 위상 관점인 반면
// 방송은 시간축 행의 흐름 관점이라 정보 밀도와 색상 요구가 달라 독립적으로 관리한다.
// 문구는 i18n(taskDetail.broadcastTab.kind)을 사용하며 여기에는 아이콘과 색상만 둔다.
const KIND_META: Record<string, KindMeta> = {
  begin: {
    icon: FlagIcon,
    dot: "bg-slate-500",
    chip: "bg-slate-500/15 text-slate-600 dark:text-slate-300",
  },
  task: {
    icon: FlagIcon,
    dot: "bg-slate-500",
    chip: "bg-slate-500/15 text-slate-600 dark:text-slate-300",
  },
  goal: {
    icon: TargetIcon,
    dot: "bg-emerald-500",
    chip: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  },
  intent: {
    icon: CompassIcon,
    dot: "bg-blue-500",
    chip: "bg-blue-500/15 text-blue-600 dark:text-blue-400",
  },
  fact: {
    icon: FlaskConicalIcon,
    dot: "bg-amber-500",
    chip: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
  },
  finding: {
    icon: BugIcon,
    dot: "bg-rose-500",
    chip: "bg-rose-500/15 text-rose-600 dark:text-rose-400",
  },
  hint: {
    icon: LightbulbIcon,
    dot: "bg-violet-500",
    chip: "bg-violet-500/15 text-violet-600 dark:text-violet-400",
  },
  digest: {
    icon: LayersIcon,
    dot: "bg-teal-500",
    chip: "bg-teal-500/15 text-teal-600 dark:text-teal-400",
  },
};

function kindKeyOf(kind: string): string {
  return kind in KIND_META ? kind : "fact";
}

// 필터 가능한 유형. 시작점(fact/state=origin)은 따로 나누지 않고 사실과 함께 필터링한다.
const FILTER_KINDS: ExploreKind[] = ["goal", "intent", "fact", "finding", "hint", "digest"];

// goal/intent의 상태 의미는 전역 status 표(StatusBadge)가 제공한다. 나머지는 그래프와
// 방송에만 있어 여기서 보충한다(문구는 i18n, 여기에는 색상만).
const STATE_TONE: Record<string, Record<string, Tone>> = {
  fact: {
    origin: "slate",
    confirmed: "green",
    dismissed: "slate",
  },
  finding: {
    confirmed: "red",
    dismissed: "slate",
  },
  hint: {
    active: "violet",
    consumed: "slate",
  },
  digest: {
    active: "green",
    superseded: "slate",
  },
};

// 작업 루트는 state=origin인 fact이며 방송에서는 시작점으로 표시한다.
function viewKind(n: TaskNode): string {
  return n.type === "fact" && n.state === "origin" ? "begin" : n.type;
}

const SUMMARY_FIELDS: Record<string, string[]> = {
  begin: ["summary", "description"],
  task: ["summary", "description"],
  goal: ["text"],
  intent: ["summary"],
  fact: ["summary"],
  finding: ["name", "summary"],
  hint: ["text", "summary"],
  digest: ["body", "summary"],
};

function summaryOf(n: TaskNode): string {
  const raw = n.payload ?? "";
  if (!raw.trim()) return "";
  for (const field of SUMMARY_FIELDS[viewKind(n)] ?? []) {
    try {
      const obj: unknown = JSON.parse(raw);
      if (obj && typeof obj === "object") {
        const v = (obj as Record<string, unknown>)[field];
        if (typeof v === "string" && v.trim()) return v;
      }
    } catch {
      return raw; // JSON이 아닌 payload는 그대로 방송한다.
    }
  }
  return raw;
}

function prettyPayload(raw: string | undefined, emptyLabel: string): string {
  if (!raw?.trim()) return emptyLabel;
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

const dayFmt = new Intl.DateTimeFormat(getDateLocale(), { month: "long", day: "numeric", weekday: "short" });
const clockFmt = new Intl.DateTimeFormat(getDateLocale(), {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

function NodeStateBadge({ node }: { node: TaskNode }) {
  const t = useTranslations("taskDetail.broadcastTab");
  if (node.type === "goal") return <StatusBadge domain="goal" value={node.state} dot />;
  if (node.type === "intent") return <StatusBadge domain="intent" value={node.state} dot />;
  const tone = STATE_TONE[node.type]?.[node.state];
  if (!tone) return null;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium whitespace-nowrap",
        toneClasses[tone],
      )}
    >
      <span className={cn("size-1.5 rounded-full", toneDot[tone])} />
      {t(`state.${node.type}.${node.state}`)}
    </span>
  );
}

function KindChip({ kind }: { kind: string }) {
  const t = useTranslations("taskDetail.broadcastTab");
  const key = kindKeyOf(kind);
  const meta = KIND_META[key];
  return (
    <span className={cn("rounded px-1.5 py-0.5 text-xs font-medium whitespace-nowrap", meta.chip)}>
      {t(`kind.${key}`)}
    </span>
  );
}

// 노드 연결 자산: 유형 라벨 + 식별 텍스트. 방송 페이지와 함께 제공(node id → 자산)되어
// 펼칠 때 추가 요청 없이 표시한다. 유형은 i18n(assetType)을 사용하며 미지원 유형은 원본 문자열로 표시한다.
function AssetList({ assets, dense = false }: { assets: FindingAsset[]; dense?: boolean }) {
  const t = useTranslations("taskDetail.broadcastTab");
  if (assets.length === 0) return null;
  return (
    <div>
      <div className="mb-1.5 text-xs font-medium text-muted-foreground">{t("assets", { count: assets.length })}</div>
      <ul className="flex flex-wrap gap-1.5">
        {assets.map((a) => {
          const typeLabel = t.has(`assetType.${a.type}`) ? t(`assetType.${a.type}`) : a.type;
          return (
            <li
              key={a.id}
              className={cn(
                "inline-flex max-w-full items-center gap-1.5 rounded-md border bg-background px-2 py-0.5",
                dense && "text-xs",
              )}
              title={`${typeLabel} · ${a.label}`}
            >
              <span className="shrink-0 rounded bg-muted px-1 text-[10px] text-muted-foreground">{typeLabel}</span>
              <code className="truncate font-mono text-xs">{a.label}</code>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// 상위/하위 항목에 마우스를 올리면 표시할 노드 카드: 유형/상태/출처/시간 + 요약 + payload 일부 + 관련 자산.
// 방송 API가 이웃 노드 전체를 반환하므로 현재 페이지 refs를 사용하고 추가 요청하지 않는다.
function RelatedNodeCard({ node, assets }: { node: TaskNode; assets: FindingAsset[] }) {
  const t = useTranslations("taskDetail.broadcastTab");
  const kind = viewKind(node);
  const ts = Date.parse(node.ts);
  const summary = summaryOf(node);
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1.5">
        <KindChip kind={kind} />
        <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">#{node.id}</code>
        <NodeStateBadge node={node} />
        {node.priority > 0 && (kind === "goal" || kind === "intent") && (
          <span className="rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">P{node.priority}</span>
        )}
      </div>
      <div className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
        <span>{t("fieldType", { label: t(`kind.${kindKeyOf(kind)}`) })}</span>
        <span>{t("fieldSource", { origin: node.origin || "system" })}</span>
        <span>{Number.isNaN(ts) ? node.ts : new Date(ts).toLocaleString(getDateLocale())}</span>
      </div>
      <p className="line-clamp-4 text-xs break-words">{summary || t("noSummary")}</p>
      <AssetList assets={assets} dense />
      <pre className="max-h-40 overflow-auto rounded border bg-muted/40 p-2 font-mono text-[11px] whitespace-pre-wrap">
        {prettyPayload(node.payload, t("noPayload"))}
      </pre>
    </div>
  );
}

// 방송의 상위/하위 관계: 상위=이 노드로 들어오는 간선, 하위=이 노드에서 나가는 간선.
function RelatedList({
  title,
  rows,
  refs,
  assets,
}: {
  title: string;
  rows: Array<{ rel: string; id: string }>;
  refs: Record<string, TaskNode>;
  assets: Record<string, FindingAsset[]>;
}) {
  const t = useTranslations("taskDetail.broadcastTab");
  if (rows.length === 0) return null;
  return (
    <div className="min-w-0 flex-1">
      <div className="mb-1.5 text-xs font-medium text-muted-foreground">{title}</div>
      <ul className="flex flex-col gap-1.5">
        {rows.map((row) => {
          const node = refs[row.id];
          return (
            <li key={`${row.rel}-${row.id}`} className="flex min-w-0 items-center gap-2 text-xs">
              <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-muted-foreground">
                {t.has(`rel.${row.rel}`) ? t(`rel.${row.rel}`) : row.rel}
              </span>
              {node ? (
                <HoverCard openDelay={150} closeDelay={100}>
                  <HoverCardTrigger asChild>
                    <button
                      type="button"
                      className="flex min-w-0 cursor-help items-center gap-2 text-left hover:underline"
                    >
                      <KindChip kind={viewKind(node)} />
                      <span className="truncate">{summaryOf(node) || t("nodeRef", { id: node.id })}</span>
                    </button>
                  </HoverCardTrigger>
                  <HoverCardContent align="start" className="w-96">
                    <RelatedNodeCard node={node} assets={assets[node.id] ?? []} />
                  </HoverCardContent>
                </HoverCard>
              ) : (
                <span className="text-muted-foreground">{t("nodeRef", { id: row.id })}</span>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function BroadcastRow({
  node,
  edges,
  refs,
  assets,
  now,
  fresh,
}: {
  node: TaskNode;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  assets: Record<string, FindingAsset[]>;
  now: number;
  fresh: boolean;
}) {
  const t = useTranslations("taskDetail.broadcastTab");
  const [open, setOpen] = React.useState(false);
  const kind = viewKind(node);
  const meta = KIND_META[kindKeyOf(kind)];
  const Icon = meta.icon;
  const ts = Date.parse(node.ts);
  const summary = summaryOf(node);
  const upstream = edges.filter((e) => e.dst === node.id).map((e) => ({ rel: e.rel, id: e.src }));
  const downstream = edges.filter((e) => e.src === node.id).map((e) => ({ rel: e.rel, id: e.dst }));

  // 상대 시간은 임계값별 구간을 사용하고 문구/단위는 i18n으로 처리한다. ts 해석 실패(NaN)면 빈 값으로 숨긴다.
  const relTime = (tsMs: number, nowMs: number): string => {
    if (!nowMs || !tsMs) return "";
    const sec = Math.max(0, (nowMs - tsMs) / 1000);
    if (sec < 60) return t("relTime.now");
    if (sec < 3600) return t("relTime.minutes", { count: Math.floor(sec / 60) });
    if (sec < 86400) return t("relTime.hours", { count: Math.floor(sec / 3600) });
    return t("relTime.days", { count: Math.floor(sec / 86400) });
  };

  return (
    <div className={cn("relative grid grid-cols-[4.5rem_1.75rem_1fr] gap-x-2", fresh && "bg-primary/5")}>
      {/* 시간 열 */}
      <div className="py-3 text-right text-xs text-muted-foreground tabular-nums">
        <div>{Number.isNaN(ts) ? "--:--:--" : clockFmt.format(ts)}</div>
        <div className="text-[11px] opacity-70">{relTime(ts, now)}</div>
      </div>

      {/* 시간축: 수직선 + 유형 점 */}
      <div className="relative flex justify-center">
        <span className="absolute inset-y-0 w-px bg-border" />
        <span
          className={cn(
            "relative mt-3.5 flex size-6 items-center justify-center rounded-full text-white ring-4 ring-background",
            meta.dot,
          )}
        >
          <Icon className="size-3.5" />
        </span>
      </div>

      {/* 내용 열 */}
      <div className="min-w-0 border-b py-3 pr-1 last:border-b-0">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full min-w-0 items-start gap-2 text-left"
        >
          <ChevronRightIcon
            className={cn("mt-0.5 size-3.5 shrink-0 text-muted-foreground transition-transform", open && "rotate-90")}
          />
          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-1.5">
              <KindChip kind={kind} />
              <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">#{node.id}</code>
              <NodeStateBadge node={node} />
              {node.priority > 0 && (kind === "goal" || kind === "intent") && (
                <span className="rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">P{node.priority}</span>
              )}
              {fresh && (
                <span className="rounded bg-primary px-1.5 py-0.5 text-[10px] font-semibold text-primary-foreground">
                  {t("fresh")}
                </span>
              )}
              <span className="ml-auto shrink-0 text-xs text-muted-foreground">{node.origin || "system"}</span>
            </div>
            <p className={cn("mt-1 text-sm", !open && "line-clamp-2")}>{summary || t("nodeRef", { id: node.id })}</p>
          </div>
        </button>

        {open && (
          <div className="mt-2 ml-5 flex flex-col gap-3 rounded-md border bg-muted/30 p-3">
            <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
              <span>
                {t("node")} <code className="font-mono">#{node.id}</code>
              </span>
              <span>{t("fieldType", { label: t(`kind.${kindKeyOf(kind)}`) })}</span>
              <span>{t("fieldSource", { origin: node.origin || "system" })}</span>
              <span>{Number.isNaN(ts) ? node.ts : new Date(ts).toLocaleString(getDateLocale())}</span>
            </div>
            {node.state === "deleted" && node.delete_reason && (
              <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs">
                <span className="font-medium text-destructive">{t("deleteReason")}</span>
                <span className="ml-2 break-words text-muted-foreground">{node.delete_reason}</span>
              </div>
            )}
            <AssetList assets={assets[node.id] ?? []} />
            {(upstream.length > 0 || downstream.length > 0) && (
              <div className="flex flex-col gap-3 sm:flex-row">
                <RelatedList title={t("upstream")} rows={upstream} refs={refs} assets={assets} />
                <RelatedList title={t("downstream")} rows={downstream} refs={refs} assets={assets} />
              </div>
            )}
            <div>
              <div className="mb-1.5 text-xs font-medium text-muted-foreground">payload</div>
              <pre className="max-h-64 overflow-auto rounded-md border bg-background p-3 font-mono text-xs whitespace-pre-wrap">
                {prettyPayload(node.payload, t("noPayload"))}
              </pre>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

export function BroadcastTab({ taskId }: { taskId: string }) {
  const t = useTranslations("taskDetail.broadcastTab");
  const tp = useTranslations("pagination");
  const [kinds, setKinds] = React.useState<ExploreKind[]>([]);
  const [queryInput, setQueryInput] = React.useState("");
  const [query, setQuery] = React.useState("");
  const [order, setOrder] = React.useState<"asc" | "desc">("desc");
  const [page, setPage] = React.useState(1);
  const [size, setSize] = React.useState(20);
  const [live, setLive] = React.useState(true);

  const [items, setItems] = React.useState<TaskNode[]>([]);
  const [edges, setEdges] = React.useState<Edge[]>([]);
  const [refs, setRefs] = React.useState<Record<string, TaskNode>>({});
  const [assets, setAssets] = React.useState<Record<string, FindingAsset[]>>({});
  const [total, setTotal] = React.useState(0);
  const [loaded, setLoaded] = React.useState(false);
  const [freshIDs, setFreshIDs] = React.useState<Set<string>>(new Set());
  const [pending, setPending] = React.useState(0);
  const [now, setNow] = React.useState(0);

  const seenRef = React.useRef<Set<string>>(new Set());
  const baselineRef = React.useRef<number | null>(null);
  const streamRef = React.useRef("");
  // 최신순 첫 페이지만 실제 실시간 위치다. 다른 위치에서는 폴링으로 읽지 않은 수만 갱신하고
  // 목록은 그대로 둬 페이지 전환/확장 중 내용이 바뀌지 않게 한다.
  const atLive = page === 1 && order === "desc";

  React.useEffect(() => {
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), 30000);
    return () => clearInterval(t);
  }, []);

  // 입력 디바운스: 300ms 입력이 멈추면 실제 조회하고 첫 페이지로 돌아간다.
  React.useEffect(() => {
    const t = setTimeout(() => {
      setQuery(queryInput);
      setPage(1);
    }, 300);
    return () => clearTimeout(t);
  }, [queryInput]);

  React.useEffect(() => {
    let alive = true;
    let rendered = false; // 이번 조회에서 내용을 이미 렌더링했는지 여부.
    // 작업/필터/정렬 변경은 방송 스트림 변경이므로 새 항목 표시와 읽지 않은 기준을 초기화한다. 페이지 전환은
    // 스트림 변경이 아니며 초기화하면 최신 위치로 돌아왔을 때 읽지 않은 수를 계산할 수 없다.
    const stream = `${taskId}|${kinds.join(",")}|${query}|${order}`;
    if (streamRef.current !== stream) {
      streamRef.current = stream;
      seenRef.current = new Set();
      baselineRef.current = null;
      setPending(0);
    }
    const load = () =>
      api
        .explorationNodes(taskId, { page, size, kinds, q: query, order })
        .then((r) => {
          if (!alive) return;
          // 실시간 위치는 매번 갱신한다. 다른 위치는 첫 번만 렌더링하고 이후에는 읽지 않은 수만 갱신한다.
          if (atLive || !rendered) {
            rendered = true;
            setItems(r.items);
            setEdges(r.edges);
            setRefs(r.refs);
            setAssets(r.assets);
          }
          setTotal(r.total);
          if (atLive) {
            const seen = seenRef.current;
            setFreshIDs(seen.size === 0 ? new Set() : new Set(r.items.filter((n) => !seen.has(n.id)).map((n) => n.id)));
            seenRef.current = new Set(r.items.map((n) => n.id));
            baselineRef.current = r.total;
            setPending(0);
          } else {
            const base = baselineRef.current;
            setPending(base === null ? 0 : Math.max(0, r.total - base));
          }
          setLoaded(true);
        })
        .catch(() => {
          // 폴링은 가능한 범위에서 처리한다. 이전 성공 내용을 유지하고 다음 회차에 자동 재시도한다.
        });
    void load();
    if (!live) {
      return () => {
        alive = false;
      };
    }
    const timer = setInterval(load, POLL_MS);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId, page, size, kinds, query, order, live, atLive]);

  const toggleKind = (kind: ExploreKind) => {
    setKinds((cur) => (cur.includes(kind) ? cur.filter((k) => k !== kind) : [...cur, kind]));
    setPage(1);
  };

  const backToLive = () => {
    setPage(1);
    setOrder("desc");
    setPending(0);
  };

  // 서버 refs는 현재 페이지 밖의 이웃만 보충하므로 같은 페이지 노드 간 참조는 items로 보완해야 한다.
  // 그렇지 않으면 인접 방송이 서로 참조할 때 단순한 노드 #id로만 표시된다.
  const nodeIndex = React.useMemo(() => {
    const idx: Record<string, TaskNode> = { ...refs };
    for (const n of items) idx[n.id] = n;
    return idx;
  }, [refs, items]);

  const pageCount = Math.max(1, Math.ceil(total / size));
  const start = total === 0 ? 0 : (page - 1) * size + 1;
  const end = (page - 1) * size + items.length;

  // 작업/필터 변경으로 수가 줄면 범위를 넘은 페이지 번호를 보정한다.
  React.useEffect(() => {
    if (page > pageCount) setPage(pageCount);
  }, [page, pageCount]);

  // 일별 그룹: 날짜별로 방송을 나눠 긴 작업에서 페이지를 넘겨도 어느 날의 일인지 알 수 있게 한다.
  const groups: Array<{ day: string; rows: TaskNode[] }> = [];
  for (const node of items) {
    const ts = Date.parse(node.ts);
    const day = Number.isNaN(ts) ? t("unknownDay") : dayFmt.format(ts);
    const last = groups[groups.length - 1];
    if (last && last.day === day) last.rows.push(node);
    else groups.push({ day, rows: [node] });
  }

  return (
    <Card className="overflow-hidden py-0">
      {/* 도구 모음 */}
      <div className="flex flex-wrap items-center gap-2 border-b px-4 py-2.5">
        <div className="relative w-full sm:w-64">
          <SearchIcon className="absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={queryInput}
            onChange={(e) => setQueryInput(e.target.value)}
            placeholder={t("toolbar.searchPlaceholder")}
            className="h-8 pl-8"
            aria-label={t("toolbar.searchAria")}
          />
        </div>
        <div className="flex flex-wrap items-center gap-1">
          {FILTER_KINDS.map((kind) => {
            const meta = KIND_META[kind];
            const active = kinds.includes(kind);
            return (
              <button
                key={kind}
                type="button"
                onClick={() => toggleKind(kind)}
                aria-pressed={active}
                className={cn(
                  "rounded-md border px-2 py-0.5 text-xs font-medium transition-colors",
                  active ? meta.chip : "border-transparent text-muted-foreground hover:bg-accent",
                )}
              >
                {t(`kind.${kind}`)}
              </button>
            );
          })}
          {kinds.length > 0 && (
            <Button
              variant="ghost"
              size="sm"
              className="h-7 px-2 text-xs"
              onClick={() => {
                setKinds([]);
                setPage(1);
              }}
            >
              {t("toolbar.clear")}
            </Button>
          )}
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            className="h-8"
            onClick={() => {
              setOrder((o) => (o === "desc" ? "asc" : "desc"));
              setPage(1);
            }}
            aria-label={order === "desc" ? t("toolbar.orderDescAria") : t("toolbar.orderAscAria")}
          >
            {order === "desc" ? <ArrowDownIcon /> : <ArrowUpIcon />}
            {order === "desc" ? t("toolbar.newest") : t("toolbar.oldest")}
          </Button>
          <Button
            variant={live ? "outline" : "secondary"}
            size="sm"
            className="h-8"
            onClick={() => setLive((v) => !v)}
            aria-label={live ? t("toolbar.pauseAria") : t("toolbar.resumeAria")}
          >
            {live ? <PauseIcon /> : <PlayIcon />}
            {live ? t("toolbar.live") : t("toolbar.paused")}
          </Button>
        </div>
      </div>

      {/* 실시간 위치 밖에서 읽지 않은 알림 */}
      {!atLive && pending > 0 && (
        <button
          type="button"
          onClick={backToLive}
          className="flex w-full items-center justify-center gap-1.5 border-b bg-primary/10 py-1.5 text-xs font-medium text-primary hover:bg-primary/15"
        >
          <ArrowUpToLineIcon className="size-3.5" />
          {t("pending", { count: pending > 99 ? "99+" : pending })}
        </button>
      )}

      <CardContent className="px-4 py-0">
        {!loaded ? (
          <div className="flex flex-col gap-3 py-4">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-12 w-full" />
            ))}
          </div>
        ) : items.length === 0 ? (
          <p className="py-10 text-center text-sm text-muted-foreground">
            {query || kinds.length > 0 ? t("emptyFiltered") : t("emptyNoNodes")}
          </p>
        ) : (
          groups.map((group) => (
            <div key={group.day}>
              <div className="py-2 pl-[6.25rem] text-xs font-medium text-muted-foreground">{group.day}</div>
              {group.rows.map((node) => (
                <BroadcastRow
                  key={node.id}
                  node={node}
                  edges={edges}
                  refs={nodeIndex}
                  assets={assets}
                  now={now}
                  fresh={freshIDs.has(node.id)}
                />
              ))}
            </div>
          ))
        )}
      </CardContent>

      <CardFooter className="flex flex-wrap items-center gap-2 border-t px-4 py-2.5 text-xs text-muted-foreground">
        <Select
          value={String(size)}
          onValueChange={(v) => {
            setSize(Number(v));
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="h-7 w-24">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {PAGE_SIZES.map((n) => (
                <SelectItem key={n} value={String(n)}>
                  {n}
                  {tp("perPage")}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <span className="tabular-nums">{tp("range", { from: start, to: end, total })}</span>
        <div className="ml-auto flex items-center gap-2">
          <Button
            variant="outline"
            size="icon-sm"
            disabled={page <= 1}
            onClick={() => setPage((p) => Math.max(1, p - 1))}
            aria-label={tp("prev")}
          >
            <ChevronLeftIcon />
          </Button>
          <span className="tabular-nums">
            {page} / {pageCount}
          </span>
          <Button
            variant="outline"
            size="icon-sm"
            disabled={page >= pageCount}
            onClick={() => setPage((p) => Math.min(pageCount, p + 1))}
            aria-label={tp("next")}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </CardFooter>
    </Card>
  );
}
