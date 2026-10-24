"use client";

import * as React from "react";

import {
  ArrowDownIcon,
  ArrowUpRightIcon,
  ChevronDownIcon,
  GripVerticalIcon,
  Loader2Icon,
  Maximize2Icon,
  Minimize2Icon,
  PinIcon,
  PinOffIcon,
  XIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";

import type { DockGripProps } from "@/components/session-dock-workspace";
import { StatusBadge } from "@/components/status-badge";
import { Transcript } from "@/components/transcript";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import type { Activity, Session } from "@/lib/types";
import { cn } from "@/lib/utils";

export type LiveSessionHistory = {
  items: Activity[];
  loaded: boolean;
  loading: boolean;
  loadingMore: boolean;
  hasMore: boolean;
  error?: string;
};
export type LiveScrollPosition = { top: number; following: boolean };
export type LiveSessionChoice = { session: Session; title: string };

// Keep consuming the task stream in the parent, but defer expensive transcript
// rendering while the user is resizing. The latest activity appears on release.
const ResizeAwareTranscript = React.memo(
  function ResizeAwareTranscript({
    activity,
    live,
    taskId,
    chat,
  }: {
    activity: Activity[];
    live?: boolean;
    taskId: string;
    chat: boolean;
    resizing: boolean;
  }) {
    return <Transcript activity={activity} live={live} taskId={taskId} chat={chat} />;
  },
  (previous, next) =>
    next.resizing ||
    (previous.activity === next.activity &&
      previous.live === next.live &&
      previous.taskId === next.taskId &&
      previous.chat === next.chat),
);

type Props = {
  dockGrip?: DockGripProps;
  composer?: React.ReactNode;
  taskId: string;
  paneId: string;
  number: number;
  session?: Session;
  title: string;
  history?: LiveSessionHistory;
  choices: LiveSessionChoice[];
  active: boolean;
  pinned: boolean;
  connected: boolean;
  pendingApproval: boolean;
  memory: Map<string, LiveScrollPosition>;
  onActivate: () => void;
  onSelect: (id: string) => void;
  onPin: () => void;
  onOpen: () => void;
  onClose?: () => void;
  onRetry: () => void;
  onLoadEarlier: (viewport: () => HTMLElement | null) => void;
  onFollowing: (following: boolean) => void;
};

export function LiveSessionPane({
  dockGrip,
  composer,
  taskId,
  paneId,
  number,
  session,
  title,
  history,
  choices,
  active,
  pinned,
  connected,
  pendingApproval,
  memory,
  onActivate,
  onSelect,
  onPin,
  onOpen,
  onClose,
  onRetry,
  onLoadEarlier,
  onFollowing,
}: Props) {
  const t = useTranslations("sessions");
  const resizing = dockGrip?.resizing ?? false;
  const resizingRef = React.useRef(resizing);
  const [pickerOpen, setPickerOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const viewportRef = React.useRef<HTMLDivElement>(null);
  const contentRef = React.useRef<HTMLDivElement>(null);
  const memoryKey = `${paneId}:${session?.id ?? "empty"}`;
  const [following, setFollowing] = React.useState(() => memory.get(memoryKey)?.following ?? true);
  const followingRef = React.useRef(following);
  const programmatic = React.useRef(false);
  const scrollTimer = React.useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const onFollowingRef = React.useRef(onFollowing);
  React.useLayoutEffect(() => {
    onFollowingRef.current = onFollowing;
  });

  const setFollow = React.useCallback((next: boolean) => {
    followingRef.current = next;
    setFollowing(next);
    onFollowingRef.current(next);
  }, []);
  const scrollToLatest = React.useCallback((smooth: boolean) => {
    const el = viewportRef.current;
    if (!el || resizingRef.current) return;
    clearTimeout(scrollTimer.current);
    programmatic.current = true;
    const reduceMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    el.scrollTo({ top: el.scrollHeight, behavior: smooth && !reduceMotion ? "smooth" : "instant" });
    scrollTimer.current = setTimeout(
      () => {
        programmatic.current = false;
      },
      smooth ? 450 : 0,
    );
  }, []);

  React.useLayoutEffect(() => {
    const wasResizing = resizingRef.current;
    resizingRef.current = resizing;
    if (resizing) {
      // Abort an in-flight smooth scroll before moving the viewport boundaries.
      clearTimeout(scrollTimer.current);
      programmatic.current = false;
      const el = viewportRef.current;
      if (el) el.scrollTo({ top: el.scrollTop, behavior: "instant" });
    } else if (wasResizing && followingRef.current) {
      scrollToLatest(false);
    }
  }, [resizing, scrollToLatest]);

  React.useLayoutEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const saved = memory.get(memoryKey);
    setFollow(saved?.following ?? true);
    if (saved && !saved.following) el.scrollTop = saved.top;
    else scrollToLatest(false);
    return () => {
      memory.set(memoryKey, { top: el.scrollTop, following: followingRef.current });
      clearTimeout(scrollTimer.current);
    };
  }, [memory, memoryKey, setFollow, scrollToLatest]);

  // Streaming text and lazily expanded tool details can grow after an SSE frame.
  // Each pane follows independently, and scrolling up never moves another pane.
  const firstLoad = React.useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: each new activity frame may grow the transcript.
  React.useLayoutEffect(() => {
    if (followingRef.current) scrollToLatest(!firstLoad.current);
    if (history?.loaded) firstLoad.current = false;
  }, [history?.items, history?.loaded, scrollToLatest]);
  React.useEffect(() => {
    const el = contentRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    let frame = 0;
    const observer = new ResizeObserver(() => {
      cancelAnimationFrame(frame);
      if (resizingRef.current) return;
      frame = requestAnimationFrame(() => {
        if (followingRef.current && !resizingRef.current) scrollToLatest(false);
      });
    });
    observer.observe(el);
    return () => {
      observer.disconnect();
      cancelAnimationFrame(frame);
    };
  }, [scrollToLatest]);

  const activity = React.useMemo(() => {
    const items = history?.items ?? [];
    if (session?.role !== "worker") return items;
    const intent: Activity = {
      seq: -1,
      worker: items[0]?.worker ?? session.id,
      ts: session.last_activity || "",
      kind: "intent",
      summary: title,
      source_task_id: session.source_task_id,
      inherited: session.inherited,
    };
    return [intent, ...items];
  }, [history?.items, session, title]);
  const cancelAutoScroll = () => {
    programmatic.current = false;
    clearTimeout(scrollTimer.current);
    const el = viewportRef.current;
    if (el) el.scrollTo({ top: el.scrollTop, behavior: "instant" });
  };

  const runState =
    session?.status === "done"
      ? "done"
      : session?.status === "running" && (session.role === "worker" || session.live)
        ? "running"
        : "idle";

  return (
    <section
      aria-label={t("live.pane", { n: number })}
      data-live-pane={paneId}
      data-run-state={runState}
      data-selected={active || undefined}
      onPointerDownCapture={onActivate}
      onFocusCapture={onActivate}
      className="live-session-shell relative flex min-h-0 min-w-0 flex-col overflow-hidden rounded-lg border bg-card text-card-foreground transition-colors"
    >
      {/* Header background and title expand; controls keep their own gestures. */}
      <div
        data-live-pane-header
        className="flex min-w-0 items-center gap-1 border-b px-2 py-1"
        onDoubleClick={(event) => {
          if ((event.target as HTMLElement).closest("button, input, [role=button]")) return;
          dockGrip?.onToggleExpand?.();
        }}
      >
        {dockGrip && (
          <button
            type="button"
            onPointerDown={dockGrip.onPointerDown}
            title={dockGrip.title}
            aria-label={dockGrip.title}
            className="shrink-0 touch-none cursor-grab rounded p-1 text-muted-foreground hover:bg-accent active:cursor-grabbing focus-visible:outline-2 focus-visible:outline-ring"
          >
            <GripVerticalIcon className="size-4" />
          </button>
        )}
        <button
          type="button"
          className="shrink-0 rounded px-1.5 py-1 text-xs text-muted-foreground hover:bg-accent"
          onClick={onActivate}
          aria-label={t("live.selectPane", { n: number })}
          aria-pressed={active}
        >
          {number}
        </button>
        <span className="min-w-0 flex-1 truncate text-sm font-medium select-none" title={title || t("live.choose")}>
          {title || t("live.choose")}
        </span>
        <Popover
          open={pickerOpen}
          onOpenChange={(open) => {
            setPickerOpen(open);
            if (!open) setQuery("");
          }}
        >
          <PopoverTrigger asChild>
            <Button
              variant="ghost"
              size="sm"
              disabled={pinned}
              className="size-7 shrink-0 px-1"
              aria-label={`${t("live.choose")}: ${title}`}
              title={t("live.choose")}
            >
              <ChevronDownIcon className="shrink-0" />
            </Button>
          </PopoverTrigger>
          <PopoverContent
            data-live-pane-controls
            align="start"
            sideOffset={5}
            className="w-80 max-w-[calc(100vw-2rem)]"
          >
            <Input
              aria-label={t("live.search")}
              placeholder={t("live.search")}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            <div className="max-h-80 overflow-y-auto">
              {(["mainagent", "planner", "system", "worker"] as const).map((role) => {
                const matches = choices.filter(
                  (choice) =>
                    choice.session.role === role &&
                    `${choice.title} ${choice.session.id}`.toLowerCase().includes(query.toLowerCase()),
                );
                if (!matches.length) return null;
                return (
                  <div key={role} className="mb-2">
                    <div className="px-2 py-1 text-xs text-muted-foreground">{t(`role.${role}`)}</div>
                    {matches.map((choice) => (
                      <button
                        key={choice.session.id}
                        type="button"
                        aria-pressed={choice.session.id === session?.id}
                        className={cn(
                          "flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm hover:bg-accent",
                          choice.session.id === session?.id && "bg-accent",
                        )}
                        onClick={() => {
                          onSelect(choice.session.id);
                          setPickerOpen(false);
                          setQuery("");
                        }}
                      >
                        {choice.session.intent_id && (
                          <span className="rounded bg-muted px-1 font-mono text-[10px] text-muted-foreground">
                            #{choice.session.intent_id}
                          </span>
                        )}
                        <span className="min-w-0 flex-1 line-clamp-2">{choice.title}</span>
                        {choice.session.live && <span className="size-1.5 shrink-0 rounded-full bg-blue-500" />}
                      </button>
                    ))}
                  </div>
                );
              })}
              {!choices.some((choice) =>
                `${choice.title} ${choice.session.id}`.toLowerCase().includes(query.toLowerCase()),
              ) && <p className="p-3 text-xs text-muted-foreground">{t("live.noMatches")}</p>}
            </div>
          </PopoverContent>
        </Popover>
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={onPin}
          aria-pressed={pinned}
          aria-label={t(pinned ? "live.unpin" : "live.pin")}
          title={t(pinned ? "live.unpin" : "live.pin")}
        >
          {pinned ? <PinIcon /> : <PinOffIcon />}
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          disabled={!session}
          onClick={onOpen}
          aria-label={t("live.openSession")}
          title={t("live.openSession")}
        >
          <ArrowUpRightIcon />
        </Button>
        {dockGrip?.onToggleExpand && (
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="size-7 shrink-0"
            onClick={dockGrip.onToggleExpand}
            aria-label={t(dockGrip.expanded ? "live.restorePane" : "live.expandPane")}
            title={t(dockGrip.expanded ? "live.restorePane" : "live.expandPane")}
            aria-expanded={dockGrip.expanded}
          >
            {dockGrip.expanded ? <Minimize2Icon /> : <Maximize2Icon />}
          </Button>
        )}
        {onClose && (
          <Button
            size="icon-sm"
            variant="ghost"
            className="size-7 shrink-0"
            onClick={(e) => {
              e.stopPropagation();
              onClose();
            }}
            title={t("live.closePane")}
            aria-label={t("live.closePane")}
          >
            <XIcon />
          </Button>
        )}
      </div>
      <div className="flex min-h-6 flex-wrap items-center gap-2 border-b px-3 py-1 text-[11px] text-muted-foreground">
        {session && (
          <span>
            {t(`role.${session.role}`)}
            {session.intent_id ? ` #${session.intent_id}` : ""}
          </span>
        )}
        {session?.live && (
          <span className="inline-flex items-center gap-1 text-blue-600 dark:text-blue-400">
            <span className="size-1 rounded-full bg-blue-500" />
            {t("sse.live")}
          </span>
        )}
        {session?.role === "worker" && (
          <StatusBadge domain="intent" value={session.status === "pending" ? "open" : session.status} />
        )}
        {pendingApproval && (
          <Badge variant="outline" className="text-amber-600">
            {t("live.approval")}
          </Badge>
        )}
        {!connected && <span className="ml-auto text-amber-600">{t("sse.reconnecting")}</span>}
      </div>
      <div className="relative flex min-h-0 flex-1 flex-col">
        <div
          ref={viewportRef}
          className="min-h-0 min-w-0 flex-1 overflow-x-hidden overflow-y-auto overscroll-contain"
          style={{ overflowAnchor: resizing ? "none" : undefined }}
          onWheel={(e) => {
            cancelAutoScroll();
            if (e.deltaY < 0) setFollow(false);
          }}
          onTouchStart={() => {
            cancelAutoScroll();
            setFollow(false);
          }}
          onPointerDown={cancelAutoScroll}
          onScroll={() => {
            const el = viewportRef.current;
            if (!el || programmatic.current || resizingRef.current) return;
            const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 60;
            setFollow(atBottom);
            memory.set(memoryKey, { top: el.scrollTop, following: atBottom });
          }}
        >
          <div ref={contentRef} data-dock-resize-content className="min-w-0 p-3 xl:p-4">
            {history?.hasMore && (
              <Button
                variant="ghost"
                size="sm"
                className="mb-3 w-full"
                disabled={history.loadingMore}
                onClick={() => {
                  setFollow(false);
                  onLoadEarlier(() => viewportRef.current);
                }}
              >
                {history.loadingMore && <Loader2Icon className="animate-spin" />}
                {t("live.older")}
              </Button>
            )}
            {!session ? (
              <p className="p-4 text-sm text-muted-foreground">{t("live.empty")}</p>
            ) : history?.error ? (
              <div className="space-y-2 text-sm text-destructive">
                <p>{t("transcript.loadError", { error: history.error })}</p>
                <Button size="sm" variant="outline" onClick={onRetry}>
                  {t("transcript.retry")}
                </Button>
              </div>
            ) : !history?.loaded ? (
              <div className="flex items-center gap-2 text-xs text-muted-foreground">
                <Loader2Icon className="size-4 animate-spin" />
                {t("transcript.loadingActivity")}
              </div>
            ) : activity.length ? (
              <ResizeAwareTranscript
                activity={activity}
                live={session.live}
                taskId={taskId}
                chat={session.role === "mainagent"}
                resizing={resizing}
              />
            ) : (
              <p className="text-sm text-muted-foreground">
                {t(session.role === "mainagent" ? "transcript.emptyMain" : "transcript.emptyWorker")}
              </p>
            )}
          </div>
        </div>
        {!following && session && (
          <div className="absolute inset-x-0 bottom-3 flex justify-center pointer-events-none">
            <Button
              size="sm"
              variant="secondary"
              className="pointer-events-auto shadow-sm"
              onClick={() => {
                setFollow(true);
                scrollToLatest(true);
              }}
            >
              <ArrowDownIcon />
              {t("live.latest")}
            </Button>
          </div>
        )}
      </div>
      {composer}
    </section>
  );
}
